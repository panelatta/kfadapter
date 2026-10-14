package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/oshinop/chinadapter/internal/provider"
)

// migrateSQLiteV8ToV9 preserves every provider account and its opaque refresh
// and tunnel authority. Both transports route by host and port; v8 stores all
// nodeKey inputs, so neither provider needs clearing or another login.
func migrateSQLiteV8ToV9(ctx context.Context, db *sql.DB) error {
	return migrateSQLiteV8Layout(ctx, db, v8LayoutMigration{sessionHash: hashRawBrowserSessionToken, version: 9})
}

// v8LayoutMigration describes one lineage that still stores the v8 tables.
type v8LayoutMigration struct {
	// prepare runs first inside the migration transaction.
	prepare []string
	// sessionHash maps a stored browser_sessions.token to its token hash.
	sessionHash func(stored string) ([]byte, bool)
	version     int
}

func hashRawBrowserSessionToken(token string) ([]byte, bool) {
	if !validOpaqueBrowserToken(token) {
		return nil, false
	}
	hash := BrowserSessionTokenHash(token)
	return hash[:], true
}

func migrateSQLiteV8Layout(ctx context.Context, db *sql.DB, migration v8LayoutMigration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return corruptDatabase(err)
	}
	defer tx.Rollback()
	for _, query := range migration.prepare {
		if _, err := tx.ExecContext(ctx, query); err != nil {
			return corruptDatabase(err)
		}
	}
	type migratedNode struct {
		position        int
		oldID, id, host string
	}
	var nodes []migratedNode
	mapping := make(map[string]string)
	seen := make(map[string]bool)
	rows, err := tx.QueryContext(ctx, "SELECT position, node_id, provider_id, protocol, host, port FROM active_session_nodes ORDER BY position")
	if err != nil {
		return corruptDatabase(err)
	}
	defer rows.Close()
	for rows.Next() {
		var node migratedNode
		var rawProvider, rawProtocol, host string
		var port int
		if err := rows.Scan(&node.position, &node.oldID, &rawProvider, &rawProtocol, &host, &port); err != nil {
			return corruptDatabase(err)
		}
		if port < 1 || port > 65535 {
			return corruptDatabase(errors.New("invalid migration node port"))
		}
		route, err := provider.Canonicalize(provider.Route{Provider: provider.ID(rawProvider), Protocol: provider.Protocol(rawProtocol), Host: host, Port: uint16(port)})
		if err != nil {
			return corruptDatabase(fmt.Errorf("migrate node route: %w", err))
		}
		node.id, err = provider.NodeID(route)
		if err != nil {
			return corruptDatabase(err)
		}
		node.host = route.Host
		mapping[node.oldID] = node.id
		if !seen[node.id] {
			nodes = append(nodes, node)
			seen[node.id] = true
		}
	}
	if err := rows.Close(); err != nil {
		return corruptDatabase(err)
	}
	if err := rows.Err(); err != nil {
		return corruptDatabase(err)
	}
	excludedRows, err := tx.QueryContext(ctx, "SELECT node_id FROM excluded_node_ids")
	if err != nil {
		return corruptDatabase(err)
	}
	defer excludedRows.Close()
	excluded := make(map[string]bool)
	for excludedRows.Next() {
		var oldID string
		if err := excludedRows.Scan(&oldID); err != nil {
			return corruptDatabase(err)
		}
		if id, exists := mapping[oldID]; exists {
			excluded[id] = true
		}
	}
	if err := excludedRows.Close(); err != nil {
		return corruptDatabase(err)
	}
	if err := excludedRows.Err(); err != nil {
		return corruptDatabase(err)
	}
	type browserSession struct {
		hash    []byte
		csrf    string
		expires int64
	}
	var sessions []browserSession
	sessionRows, err := tx.QueryContext(ctx, "SELECT token, csrf, expires_at_ns FROM browser_sessions")
	if err != nil {
		return corruptDatabase(err)
	}
	defer sessionRows.Close()
	for sessionRows.Next() {
		var session browserSession
		var token string
		if err := sessionRows.Scan(&token, &session.csrf, &session.expires); err != nil {
			return corruptDatabase(err)
		}
		var valid bool
		session.hash, valid = migration.sessionHash(token)
		if !valid || !validOpaqueBrowserToken(session.csrf) {
			return corruptDatabase(errors.New("invalid migration browser session"))
		}
		sessions = append(sessions, session)
	}
	if err := sessionRows.Close(); err != nil {
		return corruptDatabase(err)
	}
	if err := sessionRows.Err(); err != nil {
		return corruptDatabase(err)
	}
	for _, query := range []string{
		"DROP TABLE active_session_selectors", "DROP TABLE last_good_nodes", "DROP TABLE last_good", "DROP TABLE subscription_authority",
		"ALTER TABLE active_session_nodes RENAME TO v8_nodes", sqliteSchemaStatements[8],
		"DROP TABLE browser_sessions", sqliteSchemaStatements[9], "DELETE FROM excluded_node_ids",
	} {
		if _, err := tx.ExecContext(ctx, query); err != nil {
			return corruptDatabase(err)
		}
	}
	nodeStatement, err := tx.PrepareContext(ctx, `INSERT INTO active_session_nodes (position, session_id, node_id, provider_id, protocol, authority_id, host, port, name, group_name, model, weight, auto, eligible, excluded, health, udp_health, tcp_rtt_ns, probed_at_ns) SELECT position, session_id, ?, provider_id, protocol, authority_id, ?, port, name, group_name, model, weight, auto, eligible, ?, health, udp_health, tcp_rtt_ns, probed_at_ns FROM v8_nodes WHERE position = ?`)
	if err != nil {
		return corruptDatabase(err)
	}
	defer func() { _ = nodeStatement.Close() }()
	for _, node := range nodes {
		if _, err := nodeStatement.ExecContext(ctx, node.id, node.host, boolInt(excluded[node.id]), node.position); err != nil {
			return corruptDatabase(err)
		}
	}
	excludedStatement, err := tx.PrepareContext(ctx, "INSERT INTO excluded_node_ids (node_id, preference_id) VALUES (?, 1)")
	if err != nil {
		return corruptDatabase(err)
	}
	defer func() { _ = excludedStatement.Close() }()
	for id := range excluded {
		if _, err := excludedStatement.ExecContext(ctx, id); err != nil {
			return corruptDatabase(err)
		}
	}
	sessionStatement, err := tx.PrepareContext(ctx, "INSERT INTO browser_sessions (token_hash, csrf, expires_at_ns) VALUES (?, ?, ?)")
	if err != nil {
		return corruptDatabase(err)
	}
	defer func() { _ = sessionStatement.Close() }()
	for _, session := range sessions {
		if _, err := sessionStatement.ExecContext(ctx, session.hash, session.csrf, session.expires); err != nil {
			return corruptDatabase(err)
		}
	}
	if _, err := tx.ExecContext(ctx, "DROP TABLE v8_nodes"); err != nil {
		return corruptDatabase(err)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE schema_version SET version = ? WHERE id = 1", migration.version); err != nil {
		return corruptDatabase(err)
	}
	return corruptDatabase(tx.Commit())
}
