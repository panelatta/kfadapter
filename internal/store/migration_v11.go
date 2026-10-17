package store

import (
	"context"
	"database/sql"
	"encoding/base64"
)

// migrateSQLiteV9OrForkToV11 finishes databases at version 9 or 10. Upstream
// chinadapter v9 only lacks smart-proxy preferences. The kfadapter fork used
// versions 9 and 10 for the v8 layout plus an account roster and smart-proxy
// preferences; it gets the upstream v8-to-v9 conversion in one transaction.
func migrateSQLiteV9OrForkToV11(ctx context.Context, db *sql.DB, version int) error {
	if version == 9 {
		if err := validateSQLiteSchemaDefinition(ctx, db, 9, sqliteSchemaV9Tables, sqliteSchemaV9Statements); err == nil {
			return migrateSQLiteV9ToV11(ctx, db)
		}
		if err := validateSQLiteSchemaDefinition(ctx, db, 9, sqliteSchemaForkV9Tables, sqliteSchemaForkV9Statements); err != nil {
			return err
		}
		return migrateSQLiteForkToV11(ctx, db, false)
	}
	if err := validateSQLiteSchemaDefinition(ctx, db, 10, sqliteSchemaForkV10Tables, sqliteSchemaForkV10Statements); err != nil {
		return err
	}
	return migrateSQLiteForkToV11(ctx, db, true)
}

// migrateSQLiteV9ToV11 adds disabled smart-proxy preferences.
func migrateSQLiteV9ToV11(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return corruptDatabase(err)
	}
	defer tx.Rollback()
	for _, query := range []string{
		sqliteSchemaStatements[sqliteSchemaTables["smart_proxy_preferences"]],
		"INSERT INTO smart_proxy_preferences (id, enabled, interval_minutes) VALUES (1, 0, 0)",
		"UPDATE schema_version SET version = 11 WHERE id = 1",
	} {
		if _, err := tx.ExecContext(ctx, query); err != nil {
			return corruptDatabase(err)
		}
	}
	return corruptDatabase(tx.Commit())
}

// migrateSQLiteForkToV11 drops the fork's account roster, whose only purpose
// was rotating credentials on an account switch; derived credentials never
// rotate for provider changes. Smart-proxy preferences are kept as stored.
func migrateSQLiteForkToV11(ctx context.Context, db *sql.DB, hasSmartPreferences bool) error {
	prepare := []string{"DROP TABLE subscription_account_roster"}
	if !hasSmartPreferences {
		prepare = append(prepare,
			sqliteSchemaStatements[sqliteSchemaTables["smart_proxy_preferences"]],
			"INSERT INTO smart_proxy_preferences (id, enabled, interval_minutes) VALUES (1, 0, 0)",
		)
	}
	return migrateSQLiteV8Layout(ctx, db, v8LayoutMigration{prepare: prepare, sessionHash: decodeForkBrowserSessionHash, version: 11})
}

// decodeForkBrowserSessionHash reads the fork's browser_sessions.token, which
// already holds base64url(SHA-256(cookie token)) rather than the raw token.
func decodeForkBrowserSessionHash(stored string) ([]byte, bool) {
	if !validOpaqueBrowserToken(stored) {
		return nil, false
	}
	hash, err := base64.RawURLEncoding.DecodeString(stored)
	return hash, err == nil
}

// migrateSQLiteV11ToV12 adds inbound preferences with every optional inbound
// protocol disabled, so an upgrade opens no new listener by itself.
func migrateSQLiteV11ToV12(ctx context.Context, db *sql.DB) error {
	if err := validateSQLiteSchemaDefinition(ctx, db, 11, sqliteSchemaV11Tables, sqliteSchemaV11Statements); err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return corruptDatabase(err)
	}
	defer tx.Rollback()
	for _, query := range []string{
		sqliteSchemaStatements[sqliteSchemaTables["inbound_preferences"]],
		"INSERT INTO inbound_preferences (id, snell, shadowsocks, socks_tls) VALUES (1, 0, 0, 0)",
		"UPDATE schema_version SET version = 12 WHERE id = 1",
	} {
		if _, err := tx.ExecContext(ctx, query); err != nil {
			return corruptDatabase(err)
		}
	}
	return corruptDatabase(tx.Commit())
}
