package state

import (
	"database/sql"
	"encoding/base64"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kfadapter/kfadapter/internal/provider"
)

func newTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := NewSQLiteStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.LoadOrCreate(); err != nil {
		t.Fatal(err)
	}
	return store
}

// testSession builds a structurally valid snapshot. Each provider gets one
// authority, and every node carries its provider's current generation tag so
// a later session can repoint the same positions at different rows.
func testSession(generation uint64, base time.Time, nodes map[provider.ID][]string) *RuntimeSnapshot {
	snapshot := &RuntimeSnapshot{
		Generation: generation, CreatedAt: base, ExpiresAt: base.Add(time.Hour),
		Providers: make(map[provider.ID]provider.Snapshot), Selectors: make(map[string]NodeRef),
	}
	for _, id := range sortedKeys(nodes) {
		protocol := provider.Protocol(string(id) + "-tunnel")
		providerSnapshot := provider.Snapshot{
			Provider: id, ExpiresAt: base.Add(time.Hour), RefreshState: []byte(fmt.Sprintf("refresh-%s-%d", id, generation)),
			Account:     provider.Account{UserID: "user-" + string(id), Display: "d***@example.com", Tier: "standard"},
			Authorities: map[string]provider.Authority{"primary": {Protocol: protocol, Data: []byte(fmt.Sprintf("authority-%s-%d", id, generation))}},
		}
		for index, nodeID := range nodes[id] {
			source := provider.Node{ID: nodeID, AuthorityID: "primary", Protocol: protocol, Host: fmt.Sprintf("10.0.0.%d", index+1), Port: 443, Name: "node " + nodeID, Group: "g", Eligible: true}
			providerSnapshot.Nodes = append(providerSnapshot.Nodes, source)
			snapshot.Nodes = append(snapshot.Nodes, Node{
				ID: source.ID, Selector: "sel-" + source.ID, Provider: id, Protocol: protocol, AuthorityID: source.AuthorityID,
				Host: source.Host, Port: source.Port, Name: source.Name, Group: source.Group, Eligible: true,
				Health: NodeHealthUnknown, UDPHealth: UDPHealthUnknown,
			})
			snapshot.Selectors["sel-"+source.ID] = NodeRef{NodeID: source.ID}
		}
		snapshot.Providers[id] = providerSnapshot
	}
	return snapshot
}

func testLastGood(base time.Time, nodeIDs ...string) LastGoodState {
	lastGood := LastGoodState{CreatedAt: base}
	var links strings.Builder
	for index, nodeID := range nodeIDs {
		lastGood.Nodes = append(lastGood.Nodes, PersistedNode{ID: nodeID, Selector: "sel-" + nodeID, Provider: "kuaifan", Host: "192.0.2.1", Port: uint16(1080 + index), Name: nodeID, Group: "g", Eligible: true})
		fmt.Fprintf(&links, "socks5://user:pass@192.0.2.1:%d\n", 1080+index)
	}
	lastGood.RenderedSubscription = base64.StdEncoding.EncodeToString([]byte(links.String()))
	return lastGood
}

func bindSession(t *testing.T, candidate *PersistentState, session *RuntimeSnapshot) {
	t.Helper()
	if _, err := EnsureSubscriptionAccountBinding(candidate, session.AccountBindingID(), session.CreatedAt); err != nil {
		t.Fatal(err)
	}
	candidate.ActiveSession = session
}

// dumpPersistentTables renders every PersistentState row in key order.
func dumpPersistentTables(t *testing.T, db *sql.DB) map[string][]string {
	t.Helper()
	dump := make(map[string][]string, len(persistentStateTables))
	for _, table := range persistentStateTables {
		rows, err := db.Query("SELECT * FROM " + table.name + " ORDER BY " + strings.Join(table.keys, ", "))
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for index := range values {
				pointers[index] = &values[index]
			}
			if err := rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			dump[table.name] = append(dump[table.name], fmt.Sprintf("%#v", values))
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return dump
}

// fullRewriteDump writes state into a fresh database through the full
// replacement path, which is the reference an incremental write must match.
func fullRewriteDump(t *testing.T, state PersistentState) map[string][]string {
	t.Helper()
	reference := newTestStore(t)
	tx, err := reference.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := writePersistentStateTx(tx, nil, state); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return dumpPersistentTables(t, reference.db)
}

func totalChanges(t *testing.T, store *SQLiteStore) int64 {
	t.Helper()
	var changes int64
	if err := store.db.QueryRow("SELECT total_changes()").Scan(&changes); err != nil {
		t.Fatal(err)
	}
	return changes
}

func TestIncrementalWritesMatchFullRewrite(t *testing.T) {
	store := newTestStore(t)
	base := time.Unix(1_900_000_000, 0).UTC()
	steps := []struct {
		name   string
		change func(*testing.T, *PersistentState)
	}{
		{"bind two providers", func(t *testing.T, candidate *PersistentState) {
			if err := candidate.SetAccessToken("correct horse battery token"); err != nil {
				t.Fatal(err)
			}
			// quickfox takes positions 0 and 1 so a later step can drop it while
			// those positions survive with kuaifan nodes.
			session := testSession(1, base, map[provider.ID][]string{"kuaifan": {"k1", "k2", "k3"}, "quickfox": {"q1", "q2"}})
			session.Nodes = append(append([]Node(nil), session.Nodes[3:]...), session.Nodes[:3]...)
			bindSession(t, candidate, session)
			candidate.LastGood = testLastGood(base, "k1", "k2", "k3")
			candidate.Preferences.ExcludedNodeIDs = map[string]bool{"k2": true, "q1": true}
		}},
		{"probe results only", func(t *testing.T, candidate *PersistentState) {
			candidate.ActiveSession.Nodes[3].Health = NodeHealthHealthy
			candidate.ActiveSession.Nodes[3].TCPRTT = 42 * time.Millisecond
			candidate.ActiveSession.Nodes[3].ProbedAt = base.Add(time.Minute)
		}},
		{"drop a provider and repoint positions", func(t *testing.T, candidate *PersistentState) {
			// Positions 0 and 1 move from quickfox to kuaifan while quickfox is
			// deleted; a cascade from that deletion must not take them along.
			bindSession(t, candidate, testSession(2, base.Add(time.Hour), map[provider.ID][]string{"kuaifan": {"k3", "k1"}}))
			candidate.LastGood = testLastGood(base.Add(time.Hour), "k3", "k1")
			candidate.Preferences.ExcludedNodeIDs = map[string]bool{"k1": true}
			candidate.Preferences.RevealEndpoints = true
		}},
		{"re-add a provider on reused positions", func(t *testing.T, candidate *PersistentState) {
			// Positions 0 and 1 now belong to quickfox, which is inserted before
			// they are repointed; kuaifan keeps later positions.
			session := testSession(3, base.Add(2*time.Hour), map[provider.ID][]string{"kuaifan": {"k4", "k5"}, "quickfox": {"q9"}})
			session.Nodes = []Node{session.Nodes[2], session.Nodes[0], session.Nodes[1]}
			bindSession(t, candidate, session)
			candidate.Preferences.RefreshPolicy = "30m"
		}},
		{"log out every provider", func(t *testing.T, candidate *PersistentState) {
			candidate.ActiveSession = nil
			candidate.Preferences.ExcludedNodeIDs = nil
		}},
		{"new session after logout", func(t *testing.T, candidate *PersistentState) {
			bindSession(t, candidate, testSession(4, base.Add(3*time.Hour), map[provider.ID][]string{"quickfox": {"q1"}}))
		}},
	}
	for _, step := range steps {
		saved, err := store.Update(func(candidate *PersistentState) error {
			step.change(t, candidate)
			return nil
		})
		if err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if got, want := dumpPersistentTables(t, store.db), fullRewriteDump(t, saved); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: incremental tables differ from a full rewrite\n got: %v\nwant: %v", step.name, got, want)
		}
		loaded, err := store.Load()
		if err != nil {
			t.Fatalf("%s: reload: %v", step.name, err)
		}
		if !reflect.DeepEqual(persistentStateRows(&loaded), persistentStateRows(&saved)) {
			t.Fatalf("%s: reloaded aggregate differs from the saved aggregate", step.name)
		}
	}
	if err := ValidateSQLiteFile(store.Path()); err != nil {
		t.Fatalf("database invalid after incremental writes: %v", err)
	}
}

func TestIncrementalUpdateTouchesOnlyChangedRows(t *testing.T) {
	store := newTestStore(t)
	base := time.Unix(1_900_000_000, 0).UTC()
	if _, err := store.Update(func(candidate *PersistentState) error {
		if err := candidate.SetAccessToken("correct horse battery token"); err != nil {
			return err
		}
		bindSession(t, candidate, testSession(1, base, map[provider.ID][]string{"kuaifan": {"k1", "k2", "k3"}, "quickfox": {"q1"}}))
		candidate.LastGood = testLastGood(base, "k1", "k2", "k3")
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	before := totalChanges(t, store)
	if _, err := store.Update(func(*PersistentState) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if changed := totalChanges(t, store) - before; changed != 0 {
		t.Fatalf("unchanged update wrote %d rows", changed)
	}

	before = totalChanges(t, store)
	if _, err := store.Update(func(candidate *PersistentState) error {
		candidate.ActiveSession.Nodes[2].Health = NodeHealthUnhealthy
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if changed := totalChanges(t, store) - before; changed != 1 {
		t.Fatalf("one node change wrote %d rows", changed)
	}

	before = totalChanges(t, store)
	saved, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(saved); err != nil {
		t.Fatal(err)
	}
	if changed := totalChanges(t, store) - before; changed != 0 {
		t.Fatalf("saving the stored aggregate wrote %d rows", changed)
	}
}

func TestSaveReplacesAggregateThatNoLongerLoads(t *testing.T) {
	store := newTestStore(t)
	saved, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	// A preference row the validator rejects makes the stored aggregate
	// unloadable; Save must still restore the caller's aggregate in full.
	if _, err := store.db.Exec("UPDATE preferences SET refresh_policy = 'bogus' WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); err == nil {
		t.Fatal("corrupted preference loaded")
	}
	if err := store.Save(saved); err != nil {
		t.Fatal(err)
	}
	if got, want := dumpPersistentTables(t, store.db), fullRewriteDump(t, saved); !reflect.DeepEqual(got, want) {
		t.Fatalf("save did not restore the aggregate\n got: %v\nwant: %v", got, want)
	}
}

func TestFailedIncrementalUpdateLeavesStateUnchanged(t *testing.T) {
	store := newTestStore(t)
	base := time.Unix(1_900_000_000, 0).UTC()
	saved, err := store.Update(func(candidate *PersistentState) error {
		if err := candidate.SetAccessToken("correct horse battery token"); err != nil {
			return err
		}
		bindSession(t, candidate, testSession(1, base, map[provider.ID][]string{"kuaifan": {"k1"}}))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := dumpPersistentTables(t, store.db)
	// The dangling selector makes the candidate invalid, so the node change in
	// the same callback must not reach the database either.
	if _, err := store.Update(func(candidate *PersistentState) error {
		candidate.ActiveSession.Nodes[0].Health = NodeHealthHealthy
		candidate.ActiveSession.Selectors["dangling"] = NodeRef{NodeID: "missing"}
		return nil
	}); err == nil {
		t.Fatal("accepted a dangling selector")
	}
	if got := dumpPersistentTables(t, store.db); !reflect.DeepEqual(got, want) {
		t.Fatal("failed update changed stored rows")
	}
	if loaded, err := store.Load(); err != nil || !reflect.DeepEqual(persistentStateRows(&loaded), persistentStateRows(&saved)) {
		t.Fatalf("failed update changed the aggregate: %v", err)
	}
}
