package store

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oshinop/chinadapter/internal/provider"
	domain "github.com/oshinop/chinadapter/internal/state"
)

// testdata/kfadapter-v10.db was produced by the kfadapter fork's store at
// f6ded86: it migrated testdata/v8.db through fork v9 and v10, enabled the
// smart proxy at 60 minutes, recorded both provider accounts in the roster,
// and saved one browser session in the fork's hashed token format.
func TestForkV10FixtureMigrationPreservesProvidersSessionsAndSmartProxy(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("testdata/kfadapter-v10.db")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, sqliteStateFileName)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 10, 14, 18, 0, 0, 0, time.UTC)
	store, err := NewSQLiteStore(directory, WithClock(func() time.Time { return clock }))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close(t.Context())
	loaded, err := store.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if loaded.InstallationID != "00112233445566778899aabbccddeeff" || !loaded.VerifyAccessToken("v8 fixture access token") || loaded.Preferences.RefreshPolicy != "1h" || !loaded.Preferences.RevealEndpoints {
		t.Fatal("durable local authority or preferences lost")
	}
	if loaded.Preferences.SmartProxy != (domain.SmartProxyPreferences{Enabled: true, IntervalMinutes: 60}) {
		t.Fatalf("smart proxy preferences lost: %+v", loaded.Preferences.SmartProxy)
	}
	if loaded.ActiveSession == nil || len(loaded.ActiveSession.Providers) != 2 || len(loaded.ActiveSession.Nodes) != 2 {
		t.Fatal("migration requires provider re-login")
	}
	for _, id := range []provider.ID{"kuaifan", "quickfox"} {
		snapshot, exists := loaded.ActiveSession.Providers[id]
		if !exists || !snapshot.Valid() || snapshot.Account.UserID != string(id)+"-fixture-user" || len(snapshot.RefreshState) == 0 {
			t.Fatalf("provider %s not preserved", id)
		}
		if authority, exists := snapshot.Authorities["default"]; !exists || len(authority.Data) == 0 {
			t.Fatal("tunnel authority lost")
		}
		expected, err := provider.NodeID(provider.Route{Provider: id, Protocol: snapshot.Nodes[0].Protocol, Host: snapshot.Nodes[0].Host, Port: snapshot.Nodes[0].Port})
		if err != nil || snapshot.Nodes[0].ID != expected {
			t.Fatal("node ID not remapped from persisted route")
		}
		if id == "kuaifan" && !loaded.Preferences.ExcludedNodeIDs[expected] {
			t.Fatal("excluded ID not remapped")
		}
	}
	if len(loaded.Preferences.ExcludedNodeIDs) != 1 || len(loaded.ActiveSession.Selectors) != 2 {
		t.Fatal("exclusions or live credential map not rebuilt")
	}
	var version int
	if err := store.db.QueryRowContext(t.Context(), "SELECT version FROM schema_version WHERE id = 1").Scan(&version); err != nil || version != sqliteSchemaVersion {
		t.Fatalf("schema version %d: %v", version, err)
	}
	if err := validateSQLiteSchema(t.Context(), store.db); err != nil {
		t.Fatal(err)
	}
	expectedHash := sha256.Sum256([]byte("AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI"))
	restored := false
	if err := store.RestoreBrowserSessions(t.Context(), clock, 4096, func(hash []byte, _ time.Time) error {
		restored = bytes.Equal(hash, expectedHash[:])
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !restored {
		t.Fatal("fork browser session hash not carried over")
	}
}

func TestForkV9AndUpstreamV9MigrateToCurrentSchema(t *testing.T) {
	for _, fixture := range []struct {
		name       string
		version    int
		statements []string
		rows       []string
	}{
		{"upstream-v9", 9, sqliteSchemaV9Statements, nil},
		{"fork-v9", 9, sqliteSchemaForkV9Statements, []string{
			"INSERT INTO subscription_authority (id,selector_key,proxy_auth_key,account_binding,activated_at_ns) VALUES (1,zeroblob(32),zeroblob(32),zeroblob(32),1)",
			"INSERT INTO last_good (id,created_at_ns,rendered_subscription) VALUES (1,NULL,'')",
			"INSERT INTO subscription_account_roster (provider_id,authority_id,account_digest) VALUES ('kuaifan',1,zeroblob(32))",
		}},
		{"fork-v10", 10, sqliteSchemaForkV10Statements, []string{
			"INSERT INTO subscription_authority (id,selector_key,proxy_auth_key,account_binding,activated_at_ns) VALUES (1,zeroblob(32),zeroblob(32),NULL,1)",
			"INSERT INTO last_good (id,created_at_ns,rendered_subscription) VALUES (1,NULL,'')",
			"INSERT INTO smart_proxy_preferences (id,enabled,interval_minutes) VALUES (1,1,30)",
		}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			directory := t.TempDir()
			if err := os.Chmod(directory, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(directory, sqliteStateFileName)
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			for _, statement := range fixture.statements {
				if _, err := db.ExecContext(t.Context(), statement); err != nil {
					t.Fatal(err)
				}
			}
			rows := append([]string{
				fmt.Sprintf("INSERT INTO schema_version (id,version) VALUES (1,%d)", fixture.version),
				"INSERT INTO state_metadata (id,installation_id) VALUES (1,'00112233445566778899aabbccddeeff')",
				"INSERT INTO preferences (id,reveal_endpoints,refresh_policy) VALUES (1,1,'1h')",
			}, fixture.rows...)
			for _, query := range rows {
				if _, err := db.ExecContext(t.Context(), query); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0600); err != nil {
				t.Fatal(err)
			}
			store, err := NewSQLiteStore(directory)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close(t.Context())
			loaded, err := store.Load(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			want := domain.SmartProxyPreferences{}
			if fixture.version == 10 {
				want = domain.SmartProxyPreferences{Enabled: true, IntervalMinutes: 30}
			}
			if !loaded.Preferences.RevealEndpoints || loaded.Preferences.SmartProxy != want {
				t.Fatalf("migration changed durable preferences: %+v", loaded.Preferences)
			}
			if err := validateSQLiteSchema(t.Context(), store.db); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSmartProxyPreferencesRoundTrip(t *testing.T) {
	store, _ := newClockedStore(t)
	if _, err := store.Update(t.Context(), func(state *domain.PersistentState) error {
		state.Preferences.SmartProxy = domain.SmartProxyPreferences{Enabled: true, IntervalMinutes: 30}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(t.Context(), func(state *domain.PersistentState) error {
		state.Preferences.SmartProxy = domain.SmartProxyPreferences{Enabled: true, IntervalMinutes: 45}
		return nil
	}); err == nil {
		t.Fatal("invalid smart proxy interval persisted")
	}
	loaded, err := store.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Preferences.SmartProxy != (domain.SmartProxyPreferences{Enabled: true, IntervalMinutes: 30}) {
		t.Fatalf("smart proxy preferences not persisted: %+v", loaded.Preferences.SmartProxy)
	}
}
