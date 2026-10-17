package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/oshinop/chinadapter/internal/provider"
	domain "github.com/oshinop/chinadapter/internal/state"
)

// newAccessStore returns an open store whose installation has an access token,
// so a stored catalog derives its selectors when loaded.
func newAccessStore(t *testing.T) (*SQLiteStore, domain.PersistentState, time.Time) {
	t.Helper()
	store, now := newClockedStore(t)
	base, err := store.Update(t.Context(), func(candidate *domain.PersistentState) error {
		return candidate.SetAccessToken("size limit fixture access token")
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, base, now
}

// largeCatalogState adds count nodes whose name, group, and model each use
// fieldBytes. Every field being within its own limit does not imply the
// database stays within its read limit.
func largeCatalogState(t *testing.T, base domain.PersistentState, now time.Time, count, fieldBytes int) domain.PersistentState {
	t.Helper()
	snapshot := provider.Snapshot{
		Provider: "quickfox", Account: provider.Account{UserID: "size-limit-user", Display: "s•••@example.test", Tier: "standard"},
		ExpiresAt: now.Add(time.Hour), RefreshState: []byte("refresh-state"),
		Authorities: map[string]provider.Authority{"default": {Protocol: "quickfox-test", Data: []byte("tunnel-authority")}},
	}
	session := &domain.RuntimeSnapshot{Generation: 1, CreatedAt: now, ExpiresAt: snapshot.ExpiresAt}
	for index := range count {
		route := provider.Route{Provider: "quickfox", Protocol: "quickfox-test", Host: "192.0.2.1", Port: uint16(index + 1)}
		id, err := provider.NodeID(route)
		if err != nil {
			t.Fatal(err)
		}
		name, group, model := strings.Repeat("n", fieldBytes), strings.Repeat("g", fieldBytes), strings.Repeat("m", fieldBytes)
		snapshot.Nodes = append(snapshot.Nodes, provider.Node{ID: id, AuthorityID: "default", Protocol: route.Protocol, Host: route.Host, Port: route.Port, Name: name, Group: group, Model: model, Eligible: true})
		session.Nodes = append(session.Nodes, domain.Node{ID: id, Provider: route.Provider, Protocol: route.Protocol, AuthorityID: "default", Host: route.Host, Port: route.Port, Name: name, Group: group, Model: model, Eligible: true, Health: domain.NodeHealthUnknown})
	}
	session.Providers = map[provider.ID]provider.Snapshot{snapshot.Provider: snapshot}
	result := base.Clone()
	result.ActiveSession = session
	if err := domain.ValidatePersistentState(result); err != nil {
		t.Fatalf("invalid catalog fixture: %v", err)
	}
	return result
}

func TestSQLiteSizeLimitRollsBackOversizedCatalog(t *testing.T) {
	for _, write := range []string{"Save", "Update"} {
		t.Run(write, func(t *testing.T) {
			store, base, now := newAccessStore(t)
			// A sizeable, legal catalog still fits and becomes the recovery baseline.
			if err := store.Save(t.Context(), largeCatalogState(t, base, now, 512, 4096)); err != nil {
				t.Fatalf("catalog below limit: %v", err)
			}
			original, err := store.Load(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			oversized := largeCatalogState(t, base, now, 4096, 4096)
			switch write {
			case "Save":
				err = store.Save(t.Context(), oversized)
			case "Update":
				_, err = store.Update(t.Context(), func(candidate *domain.PersistentState) error { *candidate = oversized.Clone(); return nil })
			}
			if !errors.Is(err, domain.ErrStateFull) || errors.Is(err, domain.ErrCorruptState) {
				t.Fatalf("oversized write = %v, want SQLite growth rejection", err)
			}
			info, err := os.Stat(store.Path())
			if err != nil {
				t.Fatal(err)
			}
			if info.Size() > maxSQLiteStateBytes {
				t.Fatalf("database grew beyond readable size: %d", info.Size())
			}
			loaded, err := store.Load(t.Context())
			if err != nil {
				t.Fatalf("load after rejection: %v", err)
			}
			if !reflect.DeepEqual(loaded, original) {
				t.Fatal("failed write changed previous state")
			}
			if _, err := store.Update(t.Context(), func(candidate *domain.PersistentState) error { candidate.ActiveSession = nil; return nil }); err != nil {
				t.Fatalf("write after rejection: %v", err)
			}
			if err := store.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			reopened, err := NewSQLiteStore(filepath.Dir(store.Path()))
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close(t.Context())
			if _, err := reopened.Load(t.Context()); err != nil {
				t.Fatalf("restart after rejection: %v", err)
			}
		})
	}
}

func TestSQLiteSizeLimitCleansUpOversizedInitialization(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := NewSQLiteStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close(t.Context())
	base, err := domain.NewPersistentState()
	if err != nil {
		t.Fatal(err)
	}
	oversized := largeCatalogState(t, base, time.Now().UTC(), 4096, 4096)
	store.initializeState = func(ctx context.Context, db *sql.DB, _ domain.PersistentState) error {
		return initializeSQLiteSchemaAndState(ctx, db, oversized)
	}
	if _, err := store.LoadOrCreate(t.Context()); !errors.Is(err, domain.ErrStateFull) {
		t.Fatalf("oversized initialization = %v", err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("incomplete database left behind: %v", entries)
	}
	store.initializeState = initializeSQLiteSchemaAndState
	if _, err := store.LoadOrCreate(t.Context()); err != nil {
		t.Fatalf("retry initialization: %v", err)
	}
}

func TestSQLiteSizeLimitFollowsPageSizeAndReplacementConnections(t *testing.T) {
	for _, pageSize := range []int64{512, 4096, 65536} {
		t.Run(fmt.Sprint(pageSize), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), sqliteStateFileName)
			// Existing SQLite databases need not use the default 4096-byte pages.
			raw, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := raw.ExecContext(t.Context(), fmt.Sprintf("PRAGMA page_size = %d", pageSize)); err != nil {
				t.Fatal(err)
			}
			if _, err := raw.ExecContext(t.Context(), "CREATE TABLE payload (value BLOB)"); err != nil {
				t.Fatal(err)
			}
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}
			db, err := openSQLite(t.Context(), path, false)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			maximum := int64(maxSQLiteStateBytes) / pageSize
			check := func() {
				var got int64
				if err := db.QueryRowContext(t.Context(), "PRAGMA max_page_count").Scan(&got); err != nil {
					t.Fatal(err)
				}
				if got != maximum {
					t.Fatalf("page limit = %d, want %d", got, maximum)
				}
			}
			check()
			// Release all idle connections, then force database/sql to open another.
			db.SetMaxIdleConns(0)
			check()
			db.SetMaxIdleConns(1)
			// A single large blob fills all pages exactly, including the root pages.
			size := (maximum - 2) * (pageSize - 4)
			if _, err := db.ExecContext(t.Context(), "INSERT INTO payload VALUES (zeroblob(?))", size); err != nil {
				t.Fatalf("write at limit: %v", err)
			}
			var pages int64
			if err := db.QueryRowContext(t.Context(), "PRAGMA page_count").Scan(&pages); err != nil {
				t.Fatal(err)
			}
			if pages != maximum {
				t.Fatalf("boundary fixture uses %d pages, want %d", pages, maximum)
			}
			tx, err := db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := tx.ExecContext(t.Context(), "INSERT INTO payload VALUES (zeroblob(?))", pageSize); err == nil {
				t.Fatal("growth beyond boundary succeeded")
			}
			_ = tx.Rollback()
			var count int
			if err := db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM payload").Scan(&count); err != nil || count != 1 {
				t.Fatalf("rows after rejected transaction: %d, %v", count, err)
			}
			if _, err := db.ExecContext(t.Context(), "DELETE FROM payload"); err != nil {
				t.Fatalf("recovery write: %v", err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Size() != maxSQLiteStateBytes {
				t.Fatalf("boundary file size = %d", info.Size())
			}
		})
	}
}

// fillSQLiteStateToLimit runs grow, an UPDATE taking one value's byte length,
// until the database uses every page the size limit permits. The raw
// connection is fixture-only; application writers always use the bounded
// connector.
func fillSQLiteStateToLimit(t *testing.T, path, grow string) int64 {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var pageSize, pages, free int64
	if err := db.QueryRowContext(t.Context(), "PRAGMA page_size").Scan(&pageSize); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(t.Context(), "PRAGMA page_count").Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(t.Context(), "PRAGMA freelist_count").Scan(&free); err != nil {
		t.Fatal(err)
	}
	maximum := int64(maxSQLiteStateBytes) / pageSize
	size := (maximum - pages + free) * (pageSize - 4)
	for range 4 {
		tx, err := db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(t.Context(), grow, size); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.QueryRowContext(t.Context(), "PRAGMA page_count").Scan(&pages); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.QueryRowContext(t.Context(), "PRAGMA freelist_count").Scan(&free); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if pages == maximum && free == 0 {
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			return size
		}
		_ = tx.Rollback()
		size += (maximum - pages + free) * (pageSize - 4)
	}
	t.Fatalf("could not fill database to page limit: pages=%d free=%d maximum=%d", pages, free, maximum)
	return 0
}

// fullSQLiteState stores maximum-size refresh state for (limit in MiB - 1)
// providers plus one provider that the raw fixture grows to fill the remaining
// pages, so the full database is still a valid aggregate.
func fullSQLiteState(t *testing.T) (string, time.Time) {
	t.Helper()
	const maxRefreshStateBytes = 1 << 20 // provider.Snapshot's refresh state limit
	store, base, now := newAccessStore(t)
	session := &domain.RuntimeSnapshot{Generation: 1, CreatedAt: now, ExpiresAt: now.Add(time.Hour), Providers: make(map[provider.ID]provider.Snapshot)}
	fixed := maxSQLiteStateBytes/maxRefreshStateBytes - 1
	for index := range fixed + 1 {
		refreshState := make([]byte, maxRefreshStateBytes)
		if index == fixed {
			refreshState = []byte{1}
		}
		id := provider.ID(fmt.Sprintf("fill-%02d", index))
		session.Providers[id] = provider.Snapshot{
			Provider: id, Account: provider.Account{UserID: "fill-user", Display: "fill", Tier: "standard"},
			ExpiresAt: session.ExpiresAt, RefreshState: refreshState,
			Authorities: map[string]provider.Authority{"default": {Protocol: "fill", Data: []byte{1}}},
		}
	}
	filled := base.Clone()
	filled.ActiveSession = session
	if err := store.Save(t.Context(), filled); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	fillSQLiteStateToLimit(t, store.Path(), fmt.Sprintf("UPDATE active_session_providers SET refresh_state = zeroblob(?) WHERE provider_id = 'fill-%02d'", fixed))
	return store.Path(), now
}

func TestSQLiteSizeLimitPreservesSessionsOnFullDatabase(t *testing.T) {
	path, now := fullSQLiteState(t)
	store, err := NewSQLiteStore(filepath.Dir(path), WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close(t.Context())
	if _, err := store.Load(t.Context()); err != nil {
		t.Fatalf("full database not readable: %v", err)
	}
	saved := make(map[string]bool)
	var first [32]byte
	for index := range maxBrowserSessions {
		token := base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, "%032d", index))
		if err := store.SaveBrowserSession(t.Context(), token, now.Add(time.Hour), maxBrowserSessions); err != nil {
			if !errors.Is(err, domain.ErrStateFull) {
				t.Fatal(err)
			}
			break
		}
		hash := BrowserSessionTokenHash(token)
		if index == 0 {
			first = hash
		}
		saved[string(hash[:])] = true
	}
	if len(saved) == 0 || len(saved) == maxBrowserSessions {
		t.Fatalf("expected some sessions then page limit rejection; stored %d", len(saved))
	}
	restored := make(map[string]bool)
	if err := store.RestoreBrowserSessions(t.Context(), now, maxBrowserSessions, func(hash []byte, _ time.Time) error {
		restored[string(hash)] = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored, saved) {
		t.Fatalf("failed insertion changed sessions: got %d, want %d", len(restored), len(saved))
	}
	if err := store.DeleteBrowserSession(t.Context(), first[:]); err != nil {
		t.Fatalf("delete after full insertion: %v", err)
	}
	if _, err := store.Load(t.Context()); err != nil {
		t.Fatalf("state after session rejection: %v", err)
	}
}

func TestSQLiteSizeLimitRollsBackMigration(t *testing.T) {
	copyFixture := func(name string) func(*testing.T) string {
		return func(t *testing.T) string {
			directory := t.TempDir()
			if err := os.Chmod(directory, 0o700); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join("testdata", name))
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(directory, sqliteStateFileName)
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			return path
		}
	}
	for _, fixture := range []struct {
		name    string
		version int
		prepare func(*testing.T) string
	}{
		{"v8", 8, copyFixture("v8.db")},
		{"fork-v10", 10, copyFixture("kfadapter-v10.db")},
		{"upstream-v9", 9, func(t *testing.T) string {
			store, base, now := newAccessStore(t)
			if err := store.Save(t.Context(), largeCatalogState(t, base, now, 1, 16)); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			raw, err := sql.Open("sqlite", store.Path())
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			for _, statement := range []string{"DROP TABLE inbound_preferences", "DROP TABLE smart_proxy_preferences", "UPDATE schema_version SET version = 9 WHERE id = 1"} {
				if _, err := raw.ExecContext(t.Context(), statement); err != nil {
					t.Fatal(err)
				}
			}
			return store.Path()
		}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			path := fixture.prepare(t)
			const firstNode = "position = (SELECT MIN(position) FROM active_session_nodes)"
			raw, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			var name string
			if err := raw.QueryRowContext(t.Context(), "SELECT name FROM active_session_nodes WHERE "+firstNode).Scan(&name); err != nil {
				t.Fatal(err)
			}
			length := fillSQLiteStateToLimit(t, path, "UPDATE active_session_nodes SET name = printf('%*s', ?, 'x') WHERE "+firstNode)
			store, err := NewSQLiteStore(filepath.Dir(path))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close(t.Context())
			// Rebuilding the node table needs pages the full database cannot grow.
			if _, err := store.Load(t.Context()); !errors.Is(err, domain.ErrStateFull) {
				t.Fatalf("full v%d migration = %v", fixture.version, err)
			}
			var version int
			var actualLength int64
			if err := raw.QueryRowContext(t.Context(), "SELECT version FROM schema_version").Scan(&version); err != nil {
				t.Fatal(err)
			}
			if version != fixture.version {
				t.Fatalf("failed migration changed schema to %d", version)
			}
			if err := raw.QueryRowContext(t.Context(), "SELECT length(name) FROM active_session_nodes WHERE "+firstNode).Scan(&actualLength); err != nil {
				t.Fatal(err)
			}
			if actualLength != length {
				t.Fatal("failed migration changed node metadata")
			}
			// Restore validation migrates a copy and reports the same failure.
			if err := ValidateSQLiteFile(t.Context(), path); !errors.Is(err, domain.ErrStateFull) {
				t.Fatalf("validation of full v%d database = %v", fixture.version, err)
			}
			// Freeing pages makes migration retryable without replacing the database.
			if _, err := raw.ExecContext(t.Context(), "UPDATE active_session_nodes SET name = ? WHERE "+firstNode, name); err != nil {
				t.Fatal(err)
			}
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}
			loaded, err := store.Load(t.Context())
			if err != nil {
				t.Fatalf("migration retry: %v", err)
			}
			if loaded.ActiveSession == nil || len(loaded.ActiveSession.Nodes) == 0 || loaded.ActiveSession.Nodes[0].Name != name {
				t.Fatal("migration retry lost node metadata")
			}
			if err := ValidateSQLiteFile(t.Context(), path); err != nil {
				t.Fatalf("migrated database: %v", err)
			}
		})
	}
}
