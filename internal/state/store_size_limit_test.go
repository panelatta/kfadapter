package state

import (
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kfadapter/kfadapter/internal/provider"
)

// This uses the node count and metadata sizes accepted from QuickFox. SQLite
// stores both the active catalog and last-good output, so each field being
// within its own limit does not imply the database is within its read limit.
func largeCatalogState(t *testing.T, base PersistentState, count int) PersistentState {
	t.Helper()
	ids := make([]string, count)
	for i := range ids {
		ids[i] = fmt.Sprintf("quickfox_%016d", i)
	}
	now := time.Now().UTC()
	session := testSession(1, now, map[provider.ID][]string{"quickfox": ids})
	p := session.Providers["quickfox"]
	lastGood := LastGoodState{CreatedAt: now}
	var links strings.Builder
	for i := range p.Nodes {
		n := &p.Nodes[i]
		n.Host = "192.0.2.1"
		n.Name = strings.Repeat("n", 512)
		n.Group = strings.Repeat("g", 512) + " / " + strings.Repeat("r", 512)
		n.Model = strings.Repeat("g", 512)
		session.Nodes[i].Host = n.Host
		session.Nodes[i].Name = n.Name
		session.Nodes[i].Group = n.Group
		session.Nodes[i].Model = n.Model
		lastGood.Nodes = append(lastGood.Nodes, PersistedNode{ID: n.ID, Selector: session.Nodes[i].Selector, Provider: "quickfox", Host: n.Host, Port: n.Port, Name: n.Name, Group: n.Group, Eligible: true})
		link := url.URL{Scheme: "socks5", User: url.UserPassword("user", "pass"), Host: "127.0.0.1:10808", Fragment: n.Name}
		links.WriteString(link.String())
		links.WriteByte('\n')
	}
	session.Providers["quickfox"] = p
	result := base.Clone()
	bindSession(t, &result, session)
	lastGood.RenderedSubscription = base64.StdEncoding.EncodeToString([]byte(links.String()))
	result.LastGood = lastGood
	if err := ValidatePersistentState(result); err != nil {
		t.Fatalf("invalid catalog fixture: %v", err)
	}
	return result
}

func TestSQLiteSizeLimitRollsBackOversizedCatalog(t *testing.T) {
	for _, write := range []string{"Save", "Update"} {
		t.Run(write, func(t *testing.T) {
			store := newTestStore(t)
			original, err := store.Update(func(p *PersistentState) error { return p.SetAccessToken("correct horse battery token") })
			if err != nil {
				t.Fatal(err)
			}
			// A sizeable, legal catalog still fits and becomes our recovery baseline.
			original = largeCatalogState(t, original, 2048)
			if err := store.Save(original); err != nil {
				t.Fatalf("catalog below limit: %v", err)
			}
			oversized := largeCatalogState(t, original, 4096)
			switch write {
			case "Save":
				err = store.Save(oversized)
			case "Update":
				_, err = store.Update(func(p *PersistentState) error { *p = oversized; return nil })
			}
			if err == nil || !strings.Contains(err.Error(), "database or disk is full") {
				t.Fatalf("oversized write = %v, want SQLite growth rejection", err)
			}
			info, err := os.Stat(store.Path())
			if err != nil {
				t.Fatal(err)
			}
			if info.Size() > maxSQLiteStateBytes {
				t.Fatalf("database grew beyond readable size: %d", info.Size())
			}
			loaded, err := store.Load()
			if err != nil {
				t.Fatalf("load after rejection: %v", err)
			}
			if !reflect.DeepEqual(persistentStateRows(&loaded), persistentStateRows(&original)) {
				t.Fatal("failed write changed previous state")
			}
			if _, err := store.Update(func(p *PersistentState) error { p.ActiveSession = nil; p.LastGood = LastGoodState{}; return nil }); err != nil {
				t.Fatalf("write after rejection: %v", err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := NewSQLiteStore(filepath.Dir(store.Path()))
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if _, err := reopened.Load(); err != nil {
				t.Fatalf("restart after rejection: %v", err)
			}
		})
	}
}

func TestSQLiteSizeLimitCleansUpOversizedInitialization(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := NewSQLiteStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	oversized := largeCatalogState(t, boundPersistentState(t), 4096)
	store.initializeState = func(db *sql.DB, _ PersistentState) error { return initializeSQLiteSchemaAndState(db, oversized) }
	if _, err := store.LoadOrCreate(); err == nil || !strings.Contains(err.Error(), "database or disk is full") {
		t.Fatalf("oversized initialization = %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("incomplete database left behind: %v", entries)
	}
	store.initializeState = initializeSQLiteSchemaAndState
	if _, err := store.LoadOrCreate(); err != nil {
		t.Fatalf("retry initialization: %v", err)
	}
}

func TestSQLiteSizeLimitFollowsPageSizeAndReplacementConnections(t *testing.T) {
	for _, pageSize := range []int64{512, 4096, 65536} {
		t.Run(fmt.Sprint(pageSize), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			// Existing SQLite databases need not use the default 4096-byte pages.
			raw, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := raw.Exec(fmt.Sprintf("PRAGMA page_size = %d", pageSize)); err != nil {
				t.Fatal(err)
			}
			if _, err := raw.Exec("CREATE TABLE payload (value BLOB)"); err != nil {
				t.Fatal(err)
			}
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}
			db, err := openSQLite(path, false)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			maximum := int64(maxSQLiteStateBytes) / pageSize
			check := func() {
				var got int64
				if err := db.QueryRow("PRAGMA max_page_count").Scan(&got); err != nil {
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
			bytes := (maximum - 2) * (pageSize - 4)
			if _, err := db.Exec("INSERT INTO payload VALUES (zeroblob(?))", bytes); err != nil {
				t.Fatalf("write at limit: %v", err)
			}
			var pages int64
			if err := db.QueryRow("PRAGMA page_count").Scan(&pages); err != nil {
				t.Fatal(err)
			}
			if pages != maximum {
				t.Fatalf("boundary fixture uses %d pages, want %d", pages, maximum)
			}
			tx, err := db.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := tx.Exec("INSERT INTO payload VALUES (zeroblob(?))", pageSize); err == nil {
				t.Fatal("growth beyond boundary succeeded")
			}
			_ = tx.Rollback()
			var count int
			if err := db.QueryRow("SELECT COUNT(*) FROM payload").Scan(&count); err != nil || count != 1 {
				t.Fatalf("rows after rejected transaction: %d, %v", count, err)
			}
			if _, err := db.Exec("DELETE FROM payload"); err != nil {
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

// fillSQLiteStateToLimit creates a semantically valid database whose last-good
// metadata consumes every permitted page. The raw connection is fixture-only;
// application writers always use the bounded connector.
func fillSQLiteStateToLimit(t *testing.T, path string, downgrade bool) int64 {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if downgrade {
		for _, statement := range []string{"DROP TABLE subscription_account_roster", "UPDATE schema_version SET version = 8 WHERE id = 1"} {
			if _, err := db.Exec(statement); err != nil {
				t.Fatal(err)
			}
		}
	}
	var pageSize, pages, free int64
	if err := db.QueryRow("PRAGMA page_size").Scan(&pageSize); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("PRAGMA page_count").Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("PRAGMA freelist_count").Scan(&free); err != nil {
		t.Fatal(err)
	}
	maximum := int64(maxSQLiteStateBytes) / pageSize
	size := (maximum - pages + free) * (pageSize - 4)
	for attempt := 0; attempt < 4; attempt++ {
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec("UPDATE last_good_nodes SET name = printf('%*s', ?, 'x') WHERE position = 0", size); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.QueryRow("PRAGMA page_count").Scan(&pages); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.QueryRow("PRAGMA freelist_count").Scan(&free); err != nil {
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

func fullSQLiteStatePath(t *testing.T, downgrade bool) (string, int64) {
	t.Helper()
	store := newTestStore(t)
	if _, err := store.Update(func(p *PersistentState) error {
		if err := p.SetAccessToken("correct horse battery token"); err != nil {
			return err
		}
		now := time.Now().UTC()
		bindSession(t, p, testSession(1, now, map[provider.ID][]string{"quickfox": {"q1"}}))
		p.LastGood = testLastGood(now, "q1")
		if downgrade {
			binding, err := accountBindingFor(p.AccessTokenVerifier.Hash, p.ActiveSession.AccountBindingID())
			p.Subscription.AccountBinding = binding
			p.Subscription.AccountRoster = nil
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return store.Path(), fillSQLiteStateToLimit(t, store.Path(), downgrade)
}

func TestSQLiteSizeLimitPreservesSessionsOnFullDatabase(t *testing.T) {
	path, _ := fullSQLiteStatePath(t, false)
	store, err := NewSQLiteStore(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Load(); err != nil {
		t.Fatalf("full database not readable: %v", err)
	}
	expiry := time.Now().UTC().Add(time.Hour)
	var saved []string
	for i := 0; i < maxBrowserSessions; i++ {
		token := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%032d", i)))
		if err := store.SaveBrowserSession(token, token, expiry, maxBrowserSessions); err != nil {
			if !strings.Contains(err.Error(), "database or disk is full") {
				t.Fatal(err)
			}
			break
		}
		saved = append(saved, token)
	}
	if len(saved) == 0 || len(saved) == maxBrowserSessions {
		t.Fatalf("expected some sessions then page limit rejection; stored %d", len(saved))
	}
	var restored []string
	if err := store.RestoreBrowserSessions(time.Now().UTC(), maxBrowserSessions, func(token, _ string, _ time.Time) error { restored = append(restored, token); return nil }); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored, saved) {
		t.Fatalf("failed insertion changed sessions: got %d, want %d", len(restored), len(saved))
	}
	if err := store.DeleteBrowserSession(saved[0]); err != nil {
		t.Fatalf("delete after full insertion: %v", err)
	}
	if _, err := store.Load(); err != nil {
		t.Fatalf("state after session rejection: %v", err)
	}
}

func TestSQLiteSizeLimitRollsBackMigration(t *testing.T) {
	path, length := fullSQLiteStatePath(t, true)
	store, err := NewSQLiteStore(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Load(); err == nil || !strings.Contains(err.Error(), "database or disk is full") {
		t.Fatalf("full v8 migration = %v", err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var version int
	var actualLength int64
	if err := raw.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 8 {
		t.Fatalf("failed migration changed schema to %d", version)
	}
	if err := raw.QueryRow("SELECT length(name) FROM last_good_nodes WHERE position = 0").Scan(&actualLength); err != nil {
		t.Fatal(err)
	}
	if actualLength != length {
		t.Fatal("failed migration changed last-good metadata")
	}
	// Freeing pages makes migration retryable without replacing the database.
	if _, err := raw.Exec("UPDATE last_good_nodes SET name = 'q1' WHERE position = 0"); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); err != nil {
		t.Fatalf("migration retry: %v", err)
	}
	if err := ValidateSQLiteFile(path); err != nil {
		t.Fatalf("migrated database: %v", err)
	}
}
