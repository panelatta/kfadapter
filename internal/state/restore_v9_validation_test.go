package state_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/kfadapter/kfadapter/internal/provider"
	"github.com/kfadapter/kfadapter/internal/selector"
	"github.com/kfadapter/kfadapter/internal/state"
	"github.com/kfadapter/kfadapter/internal/subscription"
)

func legacyV9RestoreFixture(t *testing.T) (string, state.PersistentState, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := state.NewSQLiteStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.LoadOrCreate(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(func(p *state.PersistentState) error {
		p.Preferences.RevealEndpoints = true
		p.Preferences.RefreshPolicy = "30m"
		return p.SetAccessToken("v9-restore-regression-token")
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	p := provider.Snapshot{
		Provider: "quickfox", Account: provider.Account{UserID: "restore-user", Display: "r***@example.test", Tier: "standard"},
		ExpiresAt: now.Add(time.Hour), RefreshState: []byte("refresh-state"),
		Authorities: map[string]provider.Authority{"primary": {Protocol: "quickfox-test", Data: []byte("tunnel-authority")}},
		Nodes:       []provider.Node{{ID: "restore-node", AuthorityID: "primary", Protocol: "quickfox-test", Host: "192.0.2.1", Port: 443, Name: "Restorable node", Group: "Restore group", Eligible: true}},
	}
	service, err := subscription.NewService(subscription.ServiceConfig{Store: store, SocksAddress: "127.0.0.1:10808", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &state.RuntimeSnapshot{Generation: 1, CreatedAt: now, ExpiresAt: p.ExpiresAt, Providers: map[provider.ID]provider.Snapshot{p.Provider: p}}
	plan, err := service.PrepareRuntimeCommit(context.Background(), snapshot.AccountBindingID())
	if err != nil {
		t.Fatal(err)
	}
	registry, err := selector.NewRegistry(plan.Authority)
	if err != nil {
		t.Fatal(err)
	}
	n := p.Nodes[0]
	built, err := registry.Build([]state.Node{{ID: n.ID, Provider: p.Provider, Protocol: n.Protocol, AuthorityID: n.AuthorityID, Host: n.Host, Port: n.Port, Name: n.Name, Group: n.Group, Eligible: true}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Nodes, snapshot.Selectors = built.Nodes, built.Selectors
	if _, err := service.CommitRuntimeSnapshot(context.Background(), plan, snapshot); err != nil {
		t.Fatal(err)
	}
	token := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	csrf := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32))
	if err := store.SaveBrowserSession(token, csrf, now.Add(time.Hour), 16); err != nil {
		t.Fatal(err)
	}
	before, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := subscription.ValidatePersistentState(before); err != nil {
		t.Fatalf("fixture semantic validation: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	path := store.Path()
	editV9Fixture(t, path, "DROP TABLE smart_proxy_preferences", "UPDATE schema_version SET version = 9 WHERE id = 1")
	return path, before, token
}

func editV9Fixture(t *testing.T, path string, statements ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateV9RestoreWithoutChangingArchive(t *testing.T) {
	path, original, token := legacyV9RestoreFixture(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	validateErr := state.ValidateSQLiteFile(path, subscription.ValidatePersistentState)
	t.Logf("read-only validate-state result: %v", validateErr)
	if validateErr != nil {
		t.Errorf("valid v9 backup rejected: %v", validateErr)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || !info.ModTime().Equal(afterInfo.ModTime()) {
		t.Fatal("validation modified the archived database")
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "state.db" {
		t.Fatal("validation created sidecar files")
	}
	// Snapshot production remains strict: it must never export a legacy schema.
	if _, err := state.SnapshotSQLiteFile(path, subscription.ValidatePersistentState); !errors.Is(err, state.ErrCorruptState) {
		t.Fatalf("legacy snapshot = %v", err)
	}
	restoredDir := t.TempDir()
	if err := os.Chmod(restoredDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(restoredDir, "state.db"), before, 0600); err != nil {
		t.Fatal(err)
	}
	restored, err := state.NewSQLiteStore(restoredDir)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	migrated, err := restored.LoadOrCreate(subscription.ValidatePersistentState)
	t.Logf("restored LoadOrCreate migration result: %v", err)
	if err != nil {
		t.Fatal(err)
	}
	if migrated.Preferences.SmartProxy != (state.SmartProxyPreferences{}) {
		t.Fatalf("migration enabled or configured smart proxy: %+v", migrated.Preferences.SmartProxy)
	}
	if !reflect.DeepEqual(migrated, original) {
		t.Fatal("migration changed existing state")
	}
	var sessions []string
	if err := restored.RestoreBrowserSessions(time.Now().UTC(), 16, func(token, _ string, _ time.Time) error { sessions = append(sessions, token); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0] != token {
		t.Fatalf("browser sessions changed: %v", sessions)
	}
	if _, err := state.SnapshotSQLiteFile(restored.Path(), subscription.ValidatePersistentState); err != nil {
		t.Fatalf("migrated snapshot: %v", err)
	}
}

func TestValidateV9RestoreRejectsSchemaAndDataCorruption(t *testing.T) {
	for _, test := range []struct{ name, change string }{
		{"unknown version", "UPDATE schema_version SET version = 11 WHERE id = 1"},
		{"older version outside restore support", "UPDATE schema_version SET version = 8 WHERE id = 1"},
		{"missing current table is not legacy", "UPDATE schema_version SET version = 10 WHERE id = 1"},
		{"additional table", "CREATE TABLE unrecognized (value TEXT)"},
		{"changed historical definition", "ALTER TABLE preferences ADD COLUMN unrecognized TEXT"},
		{"missing historical table", "DROP TABLE subscription_account_roster"},
		{"invalid installation", "UPDATE state_metadata SET installation_id = 'invalid'"},
		{"invalid preferences", "UPDATE preferences SET reveal_endpoints = 2"},
		{"invalid provider authority", "UPDATE active_session_authorities SET authority = X''"},
		{"mismatched account roster", "UPDATE subscription_account_roster SET account_digest = zeroblob(32)"},
		{"invalid browser session", "UPDATE browser_sessions SET csrf = 'invalid'"},
		// Relational/state validation alone permits this string. The subscription
		// validator must still reject its mismatch with the authenticated rendering.
		{"invalid subscription semantics", "UPDATE last_good_nodes SET name = 'tampered'"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path, _, _ := legacyV9RestoreFixture(t)
			editV9Fixture(t, path, test.change)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := state.ValidateSQLiteFile(path, subscription.ValidatePersistentState); err == nil {
				t.Fatal("invalid legacy backup was accepted")
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("validation changed rejected backup")
			}
		})
	}
}

func TestValidateV9RestoreRunsSemanticValidatorsWithMigrationDefaults(t *testing.T) {
	path, original, _ := legacyV9RestoreFixture(t)
	marker := errors.New("semantic rejection")
	called := false
	err := state.ValidateSQLiteFile(path, subscription.ValidatePersistentState, func(p state.PersistentState) error {
		called = true
		if p.Preferences.SmartProxy != (state.SmartProxyPreferences{}) {
			t.Errorf("legacy smart defaults: %+v", p.Preferences.SmartProxy)
		}
		if !reflect.DeepEqual(p, original) {
			t.Error("legacy reconstruction differs from original state")
		}
		return marker
	})
	if !called || !errors.Is(err, marker) {
		t.Fatalf("semantic validation = %v, called=%v", err, called)
	}
}
