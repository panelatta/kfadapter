package state

import (
	"bytes"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAccountBindingKeepsKeysForSameAccountAndRotatesOnCutover(t *testing.T) {
	persistent, err := NewPersistentState()
	if err != nil {
		t.Fatal(err)
	}
	if err := persistent.SetAccessToken("correct horse battery token"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if rotated, err := EnsureSubscriptionAccountBinding(&persistent, "account-a", now); err != nil || rotated {
		t.Fatalf("first bind = %v, %v", rotated, err)
	}
	selectorKey := append([]byte(nil), persistent.Subscription.SelectorKey...)
	proxyKey := append([]byte(nil), persistent.Subscription.ProxyAuthKey...)
	if rotated, err := EnsureSubscriptionAccountBinding(&persistent, "account-a", now.Add(time.Minute)); err != nil || rotated || !bytes.Equal(selectorKey, persistent.Subscription.SelectorKey) || !bytes.Equal(proxyKey, persistent.Subscription.ProxyAuthKey) {
		t.Fatalf("same account changed keys: %v, %v", rotated, err)
	}
	if rotated, err := EnsureSubscriptionAccountBinding(&persistent, "account-b", now.Add(2*time.Minute)); err != nil || !rotated || bytes.Equal(selectorKey, persistent.Subscription.SelectorKey) || bytes.Equal(proxyKey, persistent.Subscription.ProxyAuthKey) {
		t.Fatalf("cutover did not rotate both keys: %v, %v", rotated, err)
	}
}

func TestV8StoreRoundTripHasLiveOnlySelectors(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := NewSQLiteStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	persistent, err := store.LoadOrCreate()
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidatePersistentState(persistent); err != nil {
		t.Fatal(err)
	}
	if len(persistent.Subscription.SelectorKey) != 32 || len(persistent.Subscription.ProxyAuthKey) != 32 {
		t.Fatalf("invalid fresh keys")
	}
}

func TestRuntimeGenerationRemainsMonotonic(t *testing.T) {
	now := time.Now().UTC()
	snapshot := &RuntimeSnapshot{Generation: 1, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	store, err := NewRuntimeStore(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Publish(&RuntimeSnapshot{Generation: 1, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); err == nil {
		t.Fatal("accepted non-monotonic runtime generation")
	}
}

func TestV7ToV8MigrationClearsRenewableAuthority(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, sqliteStateFileName)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range sqliteSchemaV7Statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	oldSelector, oldProxy := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	verifierState, err := NewPersistentState()
	if err != nil {
		t.Fatal(err)
	}
	if err := verifierState.SetAccessToken("correct horse battery token"); err != nil {
		t.Fatal(err)
	}
	verifier := verifierState.AccessTokenVerifier
	for _, query := range []string{
		"INSERT INTO schema_version (id, version) VALUES (1, 7)",
		"INSERT INTO state_metadata (id, installation_id) VALUES (1, '00112233445566778899aabbccddeeff')",
		"INSERT INTO preferences (id, reveal_endpoints, refresh_policy) VALUES (1, 1, '1h')",
		"INSERT INTO excluded_node_ids (node_id, preference_id) VALUES ('legacy-node', 1)",
		"INSERT INTO browser_sessions (token, csrf, expires_at_ns) VALUES ('AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE', 'AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI', 1)",
		"INSERT INTO last_good (id, generation, created_at_ns, rendered_subscription, fetched_generation) VALUES (1, 1, 1, 'legacy', 0)",
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec("INSERT INTO subscription_generation (id, generation, selector_key, proxy_auth_key, account_binding, activated_at_ns) VALUES (1, 1, ?, ?, ?, 1)", oldSelector, oldProxy, bytes.Repeat([]byte{3}, 32)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO access_token_verifier (id, memory_kib, iterations, parallelism, salt, hash) VALUES (1, ?, ?, ?, ?, ?)", verifier.Parameters.MemoryKiB, verifier.Parameters.Iterations, verifier.Parameters.Parallelism, verifier.Salt, verifier.Hash); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO last_good_nodes (position, last_good_id, node_id, selector, provider, host, port, name, group_name, eligible, excluded) VALUES (0, 1, 'legacy-node', 'n_legacy', 'quickfox', '127.0.0.1', 80, 'legacy', 'legacy', 1, 0)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO active_session (id, generation, created_at_ns, expires_at_ns) VALUES (1, 1, 1, 2)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO active_session_providers (provider_id, session_id, expires_at_ns, user_id, account_display, account_tier, subscription_active, refresh_state) VALUES ('quickfox', 1, 2, 'user', 'display', 'VIP', 1, X'00')"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO active_session_authorities (provider_id, authority_id, protocol, authority) VALUES ('quickfox', 'default', 'quickfox-native', X'00')"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO active_session_nodes (position, session_id, node_id, selector, provider_id, protocol, authority_id, host, port, name, group_name, model, weight, auto, eligible, excluded, health, udp_health, tcp_rtt_ns) VALUES (0, 1, 'legacy-node', 'n_legacy', 'quickfox', 'quickfox-native', 'default', '127.0.0.1', 80, 'legacy', 'legacy', '', 0, 0, 1, 0, 'unknown', 'unknown', 0)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO active_session_selectors (selector, session_id, node_id, generation, tombstoned) VALUES ('n_legacy', 1, 'legacy-node', 1, 0)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE browser_sessions SET expires_at_ns = ?", nanos(time.Now().Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewSQLiteStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ActiveSession != nil || len(loaded.Subscription.AccountBinding) != 0 || loaded.LastGood.RenderedSubscription != "" {
		t.Fatalf("renewable state survived migration: %#v", loaded)
	}
	if bytes.Equal(loaded.Subscription.SelectorKey, oldSelector) || bytes.Equal(loaded.Subscription.ProxyAuthKey, oldProxy) {
		t.Fatal("migration did not rotate keys")
	}
	if loaded.AccessTokenVerifier == nil || !bytes.Equal(loaded.AccessTokenVerifier.Salt, verifier.Salt) || !bytes.Equal(loaded.AccessTokenVerifier.Hash, verifier.Hash) || loaded.AccessTokenVerifier.Parameters != verifier.Parameters {
		t.Fatal("access verifier did not survive migration")
	}
	if !loaded.Preferences.RevealEndpoints || !loaded.Preferences.ExcludedNodeIDs["legacy-node"] {
		t.Fatalf("preferences not preserved: %#v", loaded.Preferences)
	}
	if err := validateSQLiteSchema(store.db); err != nil {
		t.Fatal(err)
	}
	var sessions int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM browser_sessions").Scan(&sessions); err != nil || sessions != 1 {
		t.Fatalf("browser sessions = %d, %v", sessions, err)
	}
	var renewableRows int
	if err := store.db.QueryRow("SELECT (SELECT COUNT(*) FROM active_session) + (SELECT COUNT(*) FROM active_session_providers) + (SELECT COUNT(*) FROM active_session_authorities) + (SELECT COUNT(*) FROM active_session_nodes) + (SELECT COUNT(*) FROM active_session_selectors) + (SELECT COUNT(*) FROM last_good_nodes)").Scan(&renewableRows); err != nil || renewableRows != 0 {
		t.Fatalf("renewable rows = %d, %v", renewableRows, err)
	}
}

func TestHistoricalSchemasMigrateToExactV8(t *testing.T) {
	fixtures := []struct {
		version    int
		statements []string
	}{
		{4, sqliteSchemaV5Statements[:len(sqliteSchemaV4Tables)]},
		{5, sqliteSchemaV5Statements},
		{6, sqliteSchemaV6Statements},
		{7, sqliteSchemaV7Statements},
	}
	for _, fixture := range fixtures {
		t.Run(fmt.Sprintf("v%d", fixture.version), func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, sqliteStateFileName)
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			for _, statement := range fixture.statements {
				if _, err := db.Exec(statement); err != nil {
					t.Fatal(err)
				}
			}
			for _, query := range []string{
				fmt.Sprintf("INSERT INTO schema_version (id, version) VALUES (1, %d)", fixture.version),
				"INSERT INTO state_metadata (id, installation_id) VALUES (1, '00112233445566778899aabbccddeeff')",
				"INSERT INTO subscription_generation (id, generation, selector_key, proxy_auth_key, account_binding, activated_at_ns) VALUES (1, 1, zeroblob(32), zeroblob(32), NULL, 1)",
				"INSERT INTO preferences (id, reveal_endpoints, refresh_policy) VALUES (1, 0, '')",
				"INSERT INTO last_good (id, generation, created_at_ns, rendered_subscription, fetched_generation) VALUES (1, 0, NULL, '', 0)",
			} {
				if _, err := db.Exec(query); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0o600); err != nil {
				t.Fatal(err)
			}
			store, err := NewSQLiteStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if _, err := store.Load(); err != nil {
				t.Fatal(err)
			}
			if err := validateSQLiteSchema(store.db); err != nil {
				t.Fatal(err)
			}
		})
	}
}
