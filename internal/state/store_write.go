package state

import (
	"bytes"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// sqliteTable names the columns of one normalized state table. Identifiers are
// compile-time constants; only row values are ever bound as parameters.
type sqliteTable struct {
	name    string
	keys    []string
	columns []string
}

// sqliteRow is one row of a normalized state table: its primary-key values and
// every remaining column, in the order declared by its sqliteTable.
type sqliteRow struct {
	key    []any
	values []any
}

// persistentStateTables lists every table PersistentState owns, parents before
// children. browser_sessions is deliberately absent: it has its own writers.
var persistentStateTables = []sqliteTable{
	{name: "state_metadata", keys: []string{"id"}, columns: []string{"installation_id"}},
	{name: "access_token_verifier", keys: []string{"id"}, columns: []string{"memory_kib", "iterations", "parallelism", "salt", "hash"}},
	{name: "subscription_authority", keys: []string{"id"}, columns: []string{"selector_key", "proxy_auth_key", "account_binding", "activated_at_ns"}},
	{name: "subscription_account_roster", keys: []string{"provider_id"}, columns: []string{"authority_id", "account_digest"}},
	{name: "preferences", keys: []string{"id"}, columns: []string{"reveal_endpoints", "refresh_policy"}},
	{name: "excluded_node_ids", keys: []string{"node_id"}, columns: []string{"preference_id"}},
	{name: "last_good", keys: []string{"id"}, columns: []string{"created_at_ns", "rendered_subscription"}},
	{name: "last_good_nodes", keys: []string{"position"}, columns: []string{"last_good_id", "node_id", "selector", "provider", "host", "port", "name", "group_name", "eligible", "excluded"}},
	{name: "active_session", keys: []string{"id"}, columns: []string{"generation", "created_at_ns", "expires_at_ns"}},
	{name: "active_session_providers", keys: []string{"provider_id"}, columns: []string{"session_id", "expires_at_ns", "user_id", "account_display", "account_tier", "subscription_active", "subscription_ends_at_ns", "refresh_state"}},
	{name: "active_session_authorities", keys: []string{"provider_id", "authority_id"}, columns: []string{"protocol", "authority"}},
	{name: "active_session_nodes", keys: []string{"position"}, columns: []string{"session_id", "node_id", "selector", "provider_id", "protocol", "authority_id", "host", "port", "name", "group_name", "model", "weight", "auto", "eligible", "excluded", "health", "udp_health", "tcp_rtt_ns", "probed_at_ns"}},
	{name: "active_session_selectors", keys: []string{"selector"}, columns: []string{"session_id", "node_id"}},
}

// writePersistentStateTx makes the normalized tables equal to next. With a
// previous aggregate read in the same transaction, only rows that differ are
// inserted, updated, or deleted. Without one, every table is cleared through
// the state_metadata cascade and rewritten in full.
//
// Upserts run parents first and deletions children first. A retained child row
// is therefore repointed at its new parent before a stale parent is deleted, so
// no ON DELETE CASCADE ever removes a row that next still contains.
func writePersistentStateTx(tx *sql.Tx, previous *PersistentState, next PersistentState) error {
	if err := ValidatePersistentState(next); err != nil {
		return fmt.Errorf("refusing invalid persistent state: %w", err)
	}
	if previous == nil {
		if _, err := tx.Exec("DELETE FROM state_metadata WHERE id = 1"); err != nil {
			return corruptDatabase(err)
		}
	}
	before := persistentStateRows(previous)
	after := persistentStateRows(&next)
	for _, table := range persistentStateTables {
		if err := upsertRowsTx(tx, table, before[table.name], after[table.name]); err != nil {
			return err
		}
	}
	for index := len(persistentStateTables) - 1; index >= 0; index-- {
		table := persistentStateTables[index]
		if err := deleteRowsTx(tx, table, before[table.name], after[table.name]); err != nil {
			return err
		}
	}
	return nil
}

// persistentStateRows renders an aggregate as the exact column values it is
// stored with, so two aggregates differ in a row exactly when that row's
// persisted representation differs. A nil aggregate has no rows.
func persistentStateRows(state *PersistentState) map[string][]sqliteRow {
	rows := make(map[string][]sqliteRow, len(persistentStateTables))
	if state == nil {
		return rows
	}
	add := func(table string, key []any, values ...any) {
		rows[table] = append(rows[table], sqliteRow{key: key, values: values})
	}
	one := []any{1}

	add("state_metadata", one, state.InstallationID)
	if verifier := state.AccessTokenVerifier; verifier != nil {
		add("access_token_verifier", one, verifier.Parameters.MemoryKiB, verifier.Parameters.Iterations, verifier.Parameters.Parallelism, verifier.Salt, verifier.Hash)
	}
	subscription := state.Subscription
	add("subscription_authority", one, subscription.SelectorKey, subscription.ProxyAuthKey, nullBytes(subscription.AccountBinding), nanos(subscription.ActivatedAt))
	for _, id := range sortedKeys(subscription.AccountRoster) {
		add("subscription_account_roster", []any{string(id)}, 1, subscription.AccountRoster[id])
	}
	add("preferences", one, boolInt(state.Preferences.RevealEndpoints), state.Preferences.RefreshPolicy)
	for _, nodeID := range sortedKeys(state.Preferences.ExcludedNodeIDs) {
		add("excluded_node_ids", []any{nodeID}, 1)
	}
	lastGood := state.LastGood
	add("last_good", one, nullableNanos(lastGood.CreatedAt), lastGood.RenderedSubscription)
	for index, node := range lastGood.Nodes {
		add("last_good_nodes", []any{index}, 1, node.ID, node.Selector, node.Provider, node.Host, node.Port, node.Name, node.Group, boolInt(node.Eligible), boolInt(node.Excluded))
	}

	snapshot := state.ActiveSession
	if snapshot == nil {
		return rows
	}
	add("active_session", one, snapshot.Generation, nanos(snapshot.CreatedAt), nanos(snapshot.ExpiresAt))
	for _, id := range sortedKeys(snapshot.Providers) {
		providerSnapshot := snapshot.Providers[id]
		account := providerSnapshot.Account
		add("active_session_providers", []any{string(id)}, 1, nanos(providerSnapshot.ExpiresAt), account.UserID, account.Display, account.Tier, boolInt(account.SubscriptionActive), nullableNanos(account.SubscriptionEndsAt), providerSnapshot.RefreshState)
		for _, authorityID := range sortedKeys(providerSnapshot.Authorities) {
			authority := providerSnapshot.Authorities[authorityID]
			add("active_session_authorities", []any{string(id), authorityID}, string(authority.Protocol), authority.Data)
		}
	}
	for index, node := range snapshot.Nodes {
		add("active_session_nodes", []any{index}, 1, node.ID, node.Selector, string(node.Provider), string(node.Protocol), node.AuthorityID, node.Host, node.Port, node.Name, node.Group, node.Model, node.Weight, boolInt(node.Auto), boolInt(node.Eligible), boolInt(node.Excluded), string(node.Health), string(node.UDPHealth), int64(node.TCPRTT), nullableNanos(node.ProbedAt))
	}
	for _, selector := range sortedKeys(snapshot.Selectors) {
		add("active_session_selectors", []any{selector}, 1, snapshot.Selectors[selector].NodeID)
	}
	return rows
}

// upsertRowsTx inserts rows that are new in next and updates rows whose stored
// values changed. Unchanged rows are not touched.
func upsertRowsTx(tx *sql.Tx, table sqliteTable, previous, next []sqliteRow) error {
	existing := indexRows(previous)
	for _, row := range next {
		old, found := existing[row.identity()]
		switch {
		case !found:
			if _, err := tx.Exec(table.insertSQL(), append(append([]any(nil), row.key...), row.values...)...); err != nil {
				return corruptDatabase(err)
			}
		case !sameSQLiteValues(old.values, row.values):
			if err := execOneRowTx(tx, table.updateSQL(), append(append([]any(nil), row.values...), row.key...)...); err != nil {
				return err
			}
		}
	}
	return nil
}

// deleteRowsTx deletes rows present in previous but absent from next.
func deleteRowsTx(tx *sql.Tx, table sqliteTable, previous, next []sqliteRow) error {
	retained := indexRows(next)
	for _, row := range previous {
		if _, found := retained[row.identity()]; found {
			continue
		}
		if err := execOneRowTx(tx, table.deleteSQL(), row.key...); err != nil {
			return err
		}
	}
	return nil
}

// execOneRowTx requires a keyed UPDATE or DELETE to touch exactly the one row
// the previous aggregate said exists; anything else means the tables drifted.
func execOneRowTx(tx *sql.Tx, statement string, arguments ...any) error {
	result, err := tx.Exec(statement, arguments...)
	if err != nil {
		return corruptDatabase(err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return corruptDatabase(err)
	}
	if affected != 1 {
		return corruptDatabase(fmt.Errorf("incremental state write affected %d rows", affected))
	}
	return nil
}

func (t sqliteTable) insertSQL() string {
	columns := append(append([]string(nil), t.keys...), t.columns...)
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(columns)), ", ")
	return "INSERT INTO " + t.name + " (" + strings.Join(columns, ", ") + ") VALUES (" + placeholders + ")"
}

func (t sqliteTable) updateSQL() string {
	return "UPDATE " + t.name + " SET " + strings.Join(t.columns, " = ?, ") + " = ? WHERE " + t.keyPredicate()
}

func (t sqliteTable) deleteSQL() string {
	return "DELETE FROM " + t.name + " WHERE " + t.keyPredicate()
}

func (t sqliteTable) keyPredicate() string {
	return strings.Join(t.keys, " = ? AND ") + " = ?"
}

func indexRows(rows []sqliteRow) map[string]sqliteRow {
	index := make(map[string]sqliteRow, len(rows))
	for _, row := range rows {
		index[row.identity()] = row
	}
	return index
}

// identity encodes a primary key unambiguously: %#v quotes strings and keeps
// integers bare, so no two distinct keys share an encoding.
func (r sqliteRow) identity() string {
	var builder strings.Builder
	for _, value := range r.key {
		fmt.Fprintf(&builder, "%#v,", value)
	}
	return builder.String()
}

// sameSQLiteValues compares two rendered rows. Byte slices compare by content
// because the loader reads nil and empty blobs back as the same nil slice.
func sameSQLiteValues(first, second []any) bool {
	if len(first) != len(second) {
		return false
	}
	for index := range first {
		firstBytes, firstIsBytes := first[index].([]byte)
		secondBytes, secondIsBytes := second[index].([]byte)
		if firstIsBytes || secondIsBytes {
			if !firstIsBytes || !secondIsBytes || !bytes.Equal(firstBytes, secondBytes) {
				return false
			}
			continue
		}
		if first[index] != second[index] {
			return false
		}
	}
	return true
}

func sortedKeys[K ~string, V any](values map[K]V) []K {
	keys := make([]K, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}
