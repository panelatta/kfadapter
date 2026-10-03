package state

import (
	"database/sql"
	"errors"
	"time"
)

var sqliteSchemaV9Tables = func() map[string]int {
	tables := make(map[string]int)
	for name, index := range sqliteSchemaTables {
		if name != "smart_proxy_preferences" {
			tables[name] = index
		}
	}
	return tables
}()
var sqliteSchemaV9Statements = sqliteSchemaStatements[:15]

var sqliteSchemaV8Tables = map[string]int{
	"schema_version": 0, "state_metadata": 1, "access_token_verifier": 2,
	"subscription_authority": 3, "preferences": 4, "excluded_node_ids": 5,
	"last_good": 6, "last_good_nodes": 7, "active_session": 8,
	"active_session_providers": 9, "active_session_authorities": 10,
	"active_session_nodes": 11, "active_session_selectors": 12, "browser_sessions": 13,
}

var sqliteSchemaV5Tables = map[string]int{
	"schema_version": 0, "state_metadata": 1, "access_token_verifier": 2,
	"subscription_generation": 3, "preferences": 4, "excluded_node_ids": 5,
	"last_good": 6, "last_good_nodes": 7, "active_session": 8,
	"active_session_nodes": 9, "active_session_selectors": 10, "browser_sessions": 11,
	"active_session_windows": 12, "active_session_node_profiles": 13,
}

var sqliteSchemaV6Tables = map[string]int{
	"schema_version": 0, "state_metadata": 1, "access_token_verifier": 2,
	"subscription_generation": 3, "preferences": 4, "excluded_node_ids": 5,
	"last_good": 6, "last_good_nodes": 7, "active_session": 8,
	"active_session_providers": 9, "active_session_authorities": 10,
	"active_session_nodes": 11, "active_session_selectors": 12, "browser_sessions": 13,
}

var sqliteSchemaV4Tables = map[string]int{
	"schema_version": 0, "state_metadata": 1, "access_token_verifier": 2,
	"subscription_generation": 3, "preferences": 4, "excluded_node_ids": 5,
	"last_good": 6, "last_good_nodes": 7, "active_session": 8,
	"active_session_nodes": 9, "active_session_selectors": 10, "browser_sessions": 11,
}

// sqliteSchemaV8Statements is the v8 definition, which lacked the account roster.
var sqliteSchemaV8Statements = sqliteSchemaStatements[:len(sqliteSchemaV8Tables)]

// sqliteSchemaV7Statements is retained solely to validate and atomically
// migrate historical databases. New installations use the current definition.
var sqliteSchemaV7Statements = func() []string {
	statements := append([]string(nil), sqliteSchemaV8Statements...)
	statements[3] = `CREATE TABLE subscription_generation (id INTEGER PRIMARY KEY CHECK (id = 1) REFERENCES state_metadata(id) ON DELETE CASCADE, generation INTEGER NOT NULL, selector_key BLOB NOT NULL, proxy_auth_key BLOB NOT NULL, account_binding BLOB, activated_at_ns INTEGER NOT NULL)`
	statements[6] = `CREATE TABLE last_good (id INTEGER PRIMARY KEY CHECK (id = 1) REFERENCES state_metadata(id) ON DELETE CASCADE, generation INTEGER NOT NULL, created_at_ns INTEGER, rendered_subscription TEXT NOT NULL, fetched_generation INTEGER NOT NULL, fetched_at_ns INTEGER, fetched_body_hash BLOB)`
	statements[12] = `CREATE TABLE active_session_selectors (selector TEXT PRIMARY KEY, session_id INTEGER NOT NULL REFERENCES active_session(id) ON DELETE CASCADE, node_id TEXT NOT NULL, generation INTEGER NOT NULL, tombstoned INTEGER NOT NULL, tombstone_until_ns INTEGER)`
	return statements
}()

var sqliteSchemaV6Statements = func() []string {
	statements := append([]string(nil), sqliteSchemaV7Statements...)
	statements[9] = `CREATE TABLE active_session_providers (provider_id TEXT PRIMARY KEY, session_id INTEGER NOT NULL REFERENCES active_session(id) ON DELETE CASCADE, expires_at_ns INTEGER NOT NULL, user_id TEXT NOT NULL, account_display TEXT NOT NULL, account_is_vip INTEGER NOT NULL, account_vip_ends_at_ns INTEGER, refresh_state BLOB NOT NULL)`
	return statements
}()

var sqliteSchemaV5Statements = []string{
	`CREATE TABLE schema_version (id INTEGER PRIMARY KEY CHECK (id = 1), version INTEGER NOT NULL)`,
	`CREATE TABLE state_metadata (id INTEGER PRIMARY KEY CHECK (id = 1), installation_id TEXT NOT NULL)`,
	`CREATE TABLE access_token_verifier (id INTEGER PRIMARY KEY CHECK (id = 1) REFERENCES state_metadata(id) ON DELETE CASCADE, memory_kib INTEGER NOT NULL, iterations INTEGER NOT NULL, parallelism INTEGER NOT NULL, salt BLOB NOT NULL, hash BLOB NOT NULL)`,
	`CREATE TABLE subscription_generation (id INTEGER PRIMARY KEY CHECK (id = 1) REFERENCES state_metadata(id) ON DELETE CASCADE, generation INTEGER NOT NULL, selector_key BLOB NOT NULL, proxy_auth_key BLOB NOT NULL, account_binding BLOB, activated_at_ns INTEGER NOT NULL)`,
	`CREATE TABLE preferences (id INTEGER PRIMARY KEY CHECK (id = 1) REFERENCES state_metadata(id) ON DELETE CASCADE, reveal_endpoints INTEGER NOT NULL, refresh_policy TEXT NOT NULL)`,
	`CREATE TABLE excluded_node_ids (node_id TEXT PRIMARY KEY, preference_id INTEGER NOT NULL REFERENCES preferences(id) ON DELETE CASCADE)`,
	`CREATE TABLE last_good (id INTEGER PRIMARY KEY CHECK (id = 1) REFERENCES state_metadata(id) ON DELETE CASCADE, generation INTEGER NOT NULL, created_at_ns INTEGER, rendered_subscription TEXT NOT NULL, fetched_generation INTEGER NOT NULL, fetched_at_ns INTEGER, fetched_body_hash BLOB)`,
	`CREATE TABLE last_good_nodes (position INTEGER PRIMARY KEY, last_good_id INTEGER NOT NULL REFERENCES last_good(id) ON DELETE CASCADE, node_id TEXT NOT NULL, selector TEXT NOT NULL, provider TEXT NOT NULL, host TEXT NOT NULL, port INTEGER NOT NULL, name TEXT NOT NULL, group_name TEXT NOT NULL, eligible INTEGER NOT NULL, excluded INTEGER NOT NULL)`,
	`CREATE TABLE active_session (id INTEGER PRIMARY KEY CHECK (id = 1) REFERENCES state_metadata(id) ON DELETE CASCADE, generation INTEGER NOT NULL, created_at_ns INTEGER NOT NULL, expires_at_ns INTEGER NOT NULL, account_display TEXT NOT NULL, account_is_vip INTEGER NOT NULL, account_vip_ends_at_ns INTEGER, session_user_id TEXT NOT NULL, session_login_token TEXT NOT NULL, session_provider_token TEXT NOT NULL, session_tunnel_password TEXT NOT NULL, session_tunnel_method TEXT NOT NULL, session_provider_extension TEXT NOT NULL)`,
	`CREATE TABLE active_session_nodes (position INTEGER PRIMARY KEY, session_id INTEGER NOT NULL REFERENCES active_session(id) ON DELETE CASCADE, node_id TEXT NOT NULL, selector TEXT NOT NULL, provider TEXT NOT NULL, host TEXT NOT NULL, port INTEGER NOT NULL, name TEXT NOT NULL, group_name TEXT NOT NULL, model TEXT NOT NULL, weight INTEGER NOT NULL, auto INTEGER NOT NULL, eligible INTEGER NOT NULL, excluded INTEGER NOT NULL, health TEXT NOT NULL, udp_health TEXT NOT NULL, tcp_rtt_ns INTEGER NOT NULL, probed_at_ns INTEGER)`,
	`CREATE TABLE active_session_selectors (selector TEXT PRIMARY KEY, session_id INTEGER NOT NULL REFERENCES active_session(id) ON DELETE CASCADE, node_id TEXT NOT NULL, generation INTEGER NOT NULL, tombstoned INTEGER NOT NULL, tombstone_until_ns INTEGER)`,
	`CREATE TABLE browser_sessions (token TEXT PRIMARY KEY, csrf TEXT NOT NULL, expires_at_ns INTEGER NOT NULL)`,
	`CREATE TABLE active_session_windows (id INTEGER PRIMARY KEY CHECK (id = 1) REFERENCES active_session(id) ON DELETE CASCADE, session_user_id TEXT NOT NULL, session_login_token TEXT NOT NULL, session_provider_token TEXT NOT NULL, session_tunnel_password TEXT NOT NULL, session_tunnel_method TEXT NOT NULL, session_provider_extension TEXT NOT NULL)`,
	`CREATE TABLE active_session_node_profiles (position INTEGER PRIMARY KEY REFERENCES active_session_nodes(position) ON DELETE CASCADE, client_profile TEXT NOT NULL)`,
}

func migrateSQLiteSchema(db *sql.DB) error {
	var version int
	if err := db.QueryRow("SELECT version FROM schema_version WHERE id = 1").Scan(&version); err != nil {
		return corruptDatabase(err)
	}
	if version == sqliteSchemaVersion {
		return nil
	}
	if version == 4 {
		if err := validateSQLiteSchemaDefinition(db, 4, sqliteSchemaV4Tables, sqliteSchemaV5Statements[:len(sqliteSchemaV4Tables)]); err != nil {
			return err
		}
		if err := migrateSQLiteV4ToV5(db); err != nil {
			return err
		}
		version = 5
	}
	if version == 5 {
		if err := validateSQLiteSchemaDefinition(db, 5, sqliteSchemaV5Tables, sqliteSchemaV5Statements); err != nil {
			return err
		}
		if err := migrateSQLiteV5ToV6(db); err != nil {
			return err
		}
		version = 6
	}
	if version == 6 {
		if err := validateSQLiteSchemaDefinition(db, 6, sqliteSchemaV6Tables, sqliteSchemaV6Statements); err != nil {
			return err
		}
		if err := migrateSQLiteV6ToV7(db); err != nil {
			return err
		}
		version = 7
	}
	if version == 7 {
		if err := validateSQLiteSchemaDefinition(db, 7, sqliteSchemaV6Tables, sqliteSchemaV7Statements); err != nil {
			return err
		}
		if err := migrateSQLiteV7ToV8(db); err != nil {
			return err
		}
		version = 8
	}
	if version == 8 {
		if err := validateSQLiteSchemaDefinition(db, 8, sqliteSchemaV8Tables, sqliteSchemaV8Statements); err != nil {
			return err
		}
		if err := migrateSQLiteV8ToV9(db); err != nil {
			return err
		}
		version = 9
	}
	if version != 9 {
		return corruptDatabase(errors.New("unsupported SQLite schema version"))
	}
	if err := validateSQLiteSchemaDefinition(db, 9, sqliteSchemaV9Tables, sqliteSchemaV9Statements); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return corruptDatabase(err)
	}
	defer tx.Rollback()
	for _, statement := range []string{
		sqliteSchemaStatements[15],
		"INSERT INTO smart_proxy_preferences (id, enabled, interval_minutes) VALUES (1, 0, 0)",
		"UPDATE schema_version SET version = 10 WHERE id = 1",
	} {
		if _, err := tx.Exec(statement); err != nil {
			return corruptDatabase(err)
		}
	}
	return tx.Commit()
}

// migrateSQLiteV8ToV9 adds the per-provider account roster. Existing epochs
// keep their composite binding and adopt a roster on the next matching login.
func migrateSQLiteV8ToV9(db *sql.DB) error {
	if err := configureSQLiteDurability(db); err != nil {
		return corruptDatabase(err)
	}
	tx, err := db.Begin()
	if err != nil {
		return corruptDatabase(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(sqliteSchemaStatements[sqliteSchemaTables["subscription_account_roster"]]); err != nil {
		return corruptDatabase(err)
	}
	if _, err := tx.Exec("UPDATE schema_version SET version = 9 WHERE id = 1"); err != nil {
		return corruptDatabase(err)
	}
	if err := tx.Commit(); err != nil {
		return corruptDatabase(err)
	}
	return nil
}

func migrateSQLiteV4ToV5(db *sql.DB) error {
	if err := configureSQLiteDurability(db); err != nil {
		return corruptDatabase(err)
	}
	tx, err := db.Begin()
	if err != nil {
		return corruptDatabase(err)
	}
	defer tx.Rollback()
	for _, statement := range sqliteSchemaV5Statements[len(sqliteSchemaV4Tables):] {
		if _, err := tx.Exec(statement); err != nil {
			return corruptDatabase(err)
		}
	}
	if _, err := tx.Exec("INSERT INTO active_session_node_profiles (position, client_profile) SELECT position, 'ios' FROM active_session_nodes"); err != nil {
		return corruptDatabase(err)
	}
	if _, err := tx.Exec("UPDATE schema_version SET version = 5 WHERE id = 1"); err != nil {
		return corruptDatabase(err)
	}
	if err := tx.Commit(); err != nil {
		return corruptDatabase(err)
	}
	return nil
}

func migrateSQLiteV5ToV6(db *sql.DB) error {
	if err := configureSQLiteDurability(db); err != nil {
		return corruptDatabase(err)
	}
	tx, err := db.Begin()
	if err != nil {
		return corruptDatabase(err)
	}
	defer tx.Rollback()
	// The v5 authority schema was inseparable from one provider account. A v6
	// migration securely discards only that renewable authority while retaining
	// access control, preferences, browser sessions, and last-good metadata.
	for _, table := range []string{"active_session_node_profiles", "active_session_windows", "active_session_selectors", "active_session_nodes", "active_session"} {
		if _, err := tx.Exec("DROP TABLE " + table); err != nil {
			return corruptDatabase(err)
		}
	}
	for _, statement := range sqliteSchemaV6Statements[8:13] {
		if _, err := tx.Exec(statement); err != nil {
			return corruptDatabase(err)
		}
	}
	if _, err := tx.Exec("UPDATE schema_version SET version = 6 WHERE id = 1"); err != nil {
		return corruptDatabase(err)
	}
	if err := tx.Commit(); err != nil {
		return corruptDatabase(err)
	}
	return nil
}

func migrateSQLiteV6ToV7(db *sql.DB) error {
	if err := configureSQLiteDurability(db); err != nil {
		return corruptDatabase(err)
	}
	tx, err := db.Begin()
	if err != nil {
		return corruptDatabase(err)
	}
	defer tx.Rollback()
	// v6 stored one provider-neutral VIP bit that could not distinguish
	// QuickFox's Standard, VIP, and SVIP tiers. Discard renewable provider
	// authority and account-bound output rather than granting stale eligibility.
	for _, table := range []string{"active_session_selectors", "active_session_nodes", "active_session_authorities", "active_session_providers", "active_session"} {
		if _, err := tx.Exec("DROP TABLE " + table); err != nil {
			return corruptDatabase(err)
		}
	}
	if _, err := tx.Exec("DELETE FROM last_good_nodes"); err != nil {
		return corruptDatabase(err)
	}
	if _, err := tx.Exec("UPDATE last_good SET generation = 0, created_at_ns = NULL, rendered_subscription = '', fetched_generation = 0, fetched_at_ns = NULL, fetched_body_hash = NULL WHERE id = 1"); err != nil {
		return corruptDatabase(err)
	}
	if _, err := tx.Exec("UPDATE subscription_generation SET account_binding = NULL WHERE id = 1"); err != nil {
		return corruptDatabase(err)
	}
	for _, statement := range sqliteSchemaV7Statements[8:13] {
		if _, err := tx.Exec(statement); err != nil {
			return corruptDatabase(err)
		}
	}
	if _, err := tx.Exec("UPDATE schema_version SET version = 7 WHERE id = 1"); err != nil {
		return corruptDatabase(err)
	}
	if err := tx.Commit(); err != nil {
		return corruptDatabase(err)
	}
	return nil
}

func migrateSQLiteV7ToV8(db *sql.DB) error {
	if err := configureSQLiteDurability(db); err != nil {
		return corruptDatabase(err)
	}
	selectorKey, err := randomBytes(32)
	if err != nil {
		return err
	}
	defer wipeBytes(selectorKey)
	proxyKey, err := randomBytes(32)
	if err != nil {
		return err
	}
	defer wipeBytes(proxyKey)
	tx, err := db.Begin()
	if err != nil {
		return corruptDatabase(err)
	}
	defer tx.Rollback()
	// Historical provider IDs and selector derivations cannot be safely converted.
	for _, table := range []string{"active_session_selectors", "active_session_nodes", "active_session_authorities", "active_session_providers", "active_session", "last_good_nodes", "last_good", "subscription_generation"} {
		if _, err := tx.Exec("DROP TABLE " + table); err != nil {
			return corruptDatabase(err)
		}
	}
	for _, index := range []int{3, 6, 7, 8, 9, 10, 11, 12} {
		if _, err := tx.Exec(sqliteSchemaStatements[index]); err != nil {
			return corruptDatabase(err)
		}
	}
	if _, err := tx.Exec("INSERT INTO subscription_authority (id, selector_key, proxy_auth_key, account_binding, activated_at_ns) VALUES (1, ?, ?, NULL, ?)", selectorKey, proxyKey, nanos(time.Now().UTC())); err != nil {
		return corruptDatabase(err)
	}
	if _, err := tx.Exec("INSERT INTO last_good (id, created_at_ns, rendered_subscription) VALUES (1, NULL, '')"); err != nil {
		return corruptDatabase(err)
	}
	if _, err := tx.Exec("UPDATE schema_version SET version = 8 WHERE id = 1"); err != nil {
		return corruptDatabase(err)
	}
	if err := tx.Commit(); err != nil {
		return corruptDatabase(err)
	}
	return nil
}
