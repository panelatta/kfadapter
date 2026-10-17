package store

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

var sqliteSchemaV8Statements = []string{
	`CREATE TABLE schema_version (id INTEGER PRIMARY KEY CHECK (id = 1), version INTEGER NOT NULL)`,
	`CREATE TABLE state_metadata (id INTEGER PRIMARY KEY CHECK (id = 1), installation_id TEXT NOT NULL)`,
	`CREATE TABLE access_token_verifier (id INTEGER PRIMARY KEY CHECK (id = 1) REFERENCES state_metadata(id) ON DELETE CASCADE, memory_kib INTEGER NOT NULL, iterations INTEGER NOT NULL, parallelism INTEGER NOT NULL, salt BLOB NOT NULL, hash BLOB NOT NULL)`,
	`CREATE TABLE subscription_authority (id INTEGER PRIMARY KEY CHECK (id = 1) REFERENCES state_metadata(id) ON DELETE CASCADE, selector_key BLOB NOT NULL, proxy_auth_key BLOB NOT NULL, account_binding BLOB, activated_at_ns INTEGER NOT NULL)`,
	`CREATE TABLE preferences (id INTEGER PRIMARY KEY CHECK (id = 1) REFERENCES state_metadata(id) ON DELETE CASCADE, reveal_endpoints INTEGER NOT NULL, refresh_policy TEXT NOT NULL)`,
	`CREATE TABLE excluded_node_ids (node_id TEXT PRIMARY KEY, preference_id INTEGER NOT NULL REFERENCES preferences(id) ON DELETE CASCADE)`,
	`CREATE TABLE last_good (id INTEGER PRIMARY KEY CHECK (id = 1) REFERENCES state_metadata(id) ON DELETE CASCADE, created_at_ns INTEGER, rendered_subscription TEXT NOT NULL)`,
	`CREATE TABLE last_good_nodes (position INTEGER PRIMARY KEY, last_good_id INTEGER NOT NULL REFERENCES last_good(id) ON DELETE CASCADE, node_id TEXT NOT NULL, selector TEXT NOT NULL, provider TEXT NOT NULL, host TEXT NOT NULL, port INTEGER NOT NULL, name TEXT NOT NULL, group_name TEXT NOT NULL, eligible INTEGER NOT NULL, excluded INTEGER NOT NULL)`,
	`CREATE TABLE active_session (id INTEGER PRIMARY KEY CHECK (id = 1) REFERENCES state_metadata(id) ON DELETE CASCADE, generation INTEGER NOT NULL, created_at_ns INTEGER NOT NULL, expires_at_ns INTEGER NOT NULL)`,
	`CREATE TABLE active_session_providers (provider_id TEXT PRIMARY KEY, session_id INTEGER NOT NULL REFERENCES active_session(id) ON DELETE CASCADE, expires_at_ns INTEGER NOT NULL, user_id TEXT NOT NULL, account_display TEXT NOT NULL, account_tier TEXT NOT NULL, subscription_active INTEGER NOT NULL, subscription_ends_at_ns INTEGER, refresh_state BLOB NOT NULL)`,
	`CREATE TABLE active_session_authorities (provider_id TEXT NOT NULL REFERENCES active_session_providers(provider_id) ON DELETE CASCADE, authority_id TEXT NOT NULL, protocol TEXT NOT NULL, authority BLOB NOT NULL, PRIMARY KEY (provider_id, authority_id))`,
	`CREATE TABLE active_session_nodes (position INTEGER PRIMARY KEY, session_id INTEGER NOT NULL REFERENCES active_session(id) ON DELETE CASCADE, node_id TEXT NOT NULL, selector TEXT NOT NULL, provider_id TEXT NOT NULL REFERENCES active_session_providers(provider_id) ON DELETE CASCADE, protocol TEXT NOT NULL, authority_id TEXT NOT NULL, host TEXT NOT NULL, port INTEGER NOT NULL, name TEXT NOT NULL, group_name TEXT NOT NULL, model TEXT NOT NULL, weight INTEGER NOT NULL, auto INTEGER NOT NULL, eligible INTEGER NOT NULL, excluded INTEGER NOT NULL, health TEXT NOT NULL, udp_health TEXT NOT NULL, tcp_rtt_ns INTEGER NOT NULL, probed_at_ns INTEGER)`,
	`CREATE TABLE active_session_selectors (selector TEXT PRIMARY KEY, session_id INTEGER NOT NULL REFERENCES active_session(id) ON DELETE CASCADE, node_id TEXT NOT NULL)`,
	`CREATE TABLE browser_sessions (token TEXT PRIMARY KEY, csrf TEXT NOT NULL, expires_at_ns INTEGER NOT NULL)`,
}

// Historical definitions are retained only for exact schema validation.
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

// sqliteSchemaV12Statements is schema v12: the current tables, with inbound
// preferences that lack the SOCKS5 switch.
var sqliteSchemaV12Statements = func() []string {
	statements := append([]string(nil), sqliteSchemaStatements...)
	statements[sqliteSchemaTables["inbound_preferences"]] = `CREATE TABLE inbound_preferences (id INTEGER PRIMARY KEY CHECK (id = 1) REFERENCES preferences(id) ON DELETE CASCADE, snell INTEGER NOT NULL, shadowsocks INTEGER NOT NULL, socks_tls INTEGER NOT NULL)`
	return statements
}()

// sqliteSchemaV11Tables is the merged schema v11: schema v12 without inbound
// preferences.
var sqliteSchemaV11Tables = func() map[string]int {
	tables := make(map[string]int, len(sqliteSchemaTables)-1)
	for name, index := range sqliteSchemaTables {
		if name != "inbound_preferences" {
			tables[name] = index
		}
	}
	return tables
}()

var sqliteSchemaV11Statements = sqliteSchemaStatements[:len(sqliteSchemaV11Tables)]

// sqliteSchemaV9Tables is upstream chinadapter v9: schema v11 without
// smart-proxy preferences.
var sqliteSchemaV9Tables = func() map[string]int {
	tables := make(map[string]int, len(sqliteSchemaV11Tables)-1)
	for name, index := range sqliteSchemaV11Tables {
		if name != "smart_proxy_preferences" {
			tables[name] = index
		}
	}
	return tables
}()

var sqliteSchemaV9Statements = sqliteSchemaStatements[:len(sqliteSchemaV9Tables)]

// The kfadapter fork reused versions 9 and 10 for a different lineage: v8 plus
// a per-provider account roster (fork v9), then smart-proxy preferences (fork
// v10). Exact definitions tell fork v9 apart from upstream v9.
var sqliteSchemaForkV10Tables = func() map[string]int {
	tables := make(map[string]int, len(sqliteSchemaV8Tables)+2)
	for name, index := range sqliteSchemaV8Tables {
		tables[name] = index
	}
	tables["subscription_account_roster"] = len(sqliteSchemaV8Tables)
	tables["smart_proxy_preferences"] = len(sqliteSchemaV8Tables) + 1
	return tables
}()

var sqliteSchemaForkV9Tables = func() map[string]int {
	tables := make(map[string]int, len(sqliteSchemaV8Tables)+1)
	for name, index := range sqliteSchemaForkV10Tables {
		if name != "smart_proxy_preferences" {
			tables[name] = index
		}
	}
	return tables
}()

var sqliteSchemaForkV10Statements = append(append([]string(nil), sqliteSchemaV8Statements...),
	`CREATE TABLE subscription_account_roster (provider_id TEXT PRIMARY KEY, authority_id INTEGER NOT NULL REFERENCES subscription_authority(id) ON DELETE CASCADE, account_digest BLOB NOT NULL)`,
	sqliteSchemaStatements[sqliteSchemaTables["smart_proxy_preferences"]],
)

var sqliteSchemaForkV9Statements = sqliteSchemaForkV10Statements[:len(sqliteSchemaForkV9Tables)]
