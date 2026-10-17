package store

import (
	"context"
	"database/sql"
	"strings"

	domain "github.com/oshinop/chinadapter/internal/state"
)

var sqliteSchemaTables = map[string]int{
	"schema_version": 0, "state_metadata": 1, "access_token_verifier": 2,
	"preferences": 3, "excluded_node_ids": 4, "active_session": 5,
	"active_session_providers": 6, "active_session_authorities": 7,
	"active_session_nodes": 8, "browser_sessions": 9, "smart_proxy_preferences": 10,
	"inbound_preferences": 11,
}

var sqliteSchemaStatements = func() []string {
	statements := make([]string, 0, len(sqliteSchemaTables))
	for _, index := range []int{0, 1, 2, 4, 5, 8, 9, 10, 11, 13} {
		statements = append(statements, sqliteSchemaV8Statements[index])
	}
	statements[8] = strings.Replace(statements[8], "selector TEXT NOT NULL, ", "", 1)
	statements[9] = `CREATE TABLE browser_sessions (token_hash BLOB PRIMARY KEY CHECK (length(token_hash) = 32), csrf TEXT NOT NULL, expires_at_ns INTEGER NOT NULL)`
	// The kfadapter fork's v10 definition, kept byte-identical so its
	// databases migrate without rebuilding this table.
	statements = append(statements, `CREATE TABLE smart_proxy_preferences (id INTEGER PRIMARY KEY CHECK (id = 1) REFERENCES preferences(id) ON DELETE CASCADE, enabled INTEGER NOT NULL, interval_minutes INTEGER NOT NULL)`)
	// Fork only: which inbound protocols are served. Schema v12 added the
	// table; v13 appended socks5, and this is byte-identical to the
	// definition SQLite stores after that ALTER TABLE ADD COLUMN.
	statements = append(statements, `CREATE TABLE inbound_preferences (id INTEGER PRIMARY KEY CHECK (id = 1) REFERENCES preferences(id) ON DELETE CASCADE, snell INTEGER NOT NULL, shadowsocks INTEGER NOT NULL, socks_tls INTEGER NOT NULL, socks5 INTEGER NOT NULL DEFAULT 1)`)
	return statements
}()

func initializeSQLiteSchemaAndState(ctx context.Context, db *sql.DB, initial domain.PersistentState) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return corruptDatabase(err)
	}
	defer tx.Rollback()
	for _, statement := range sqliteSchemaStatements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return corruptDatabase(err)
		}
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO schema_version (id, version) VALUES (1, ?)", sqliteSchemaVersion); err != nil {
		return corruptDatabase(err)
	}
	if err := replacePersistentStateTx(ctx, tx, initial); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return corruptDatabase(err)
	}
	return nil
}
