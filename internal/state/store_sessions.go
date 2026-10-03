package state

import (
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// RestoreBrowserSessions prunes expired or excess rows and restores current
// opaque cookie/CSRF pairs through add while holding one database transaction.
func (s *SQLiteStore) RestoreBrowserSessions(now time.Time, max int, add func(token, csrf string, expiresAt time.Time) error) error {
	if s == nil || add == nil || !validBrowserSessionLimit(max) {
		return ErrInsecureStatePath
	}
	now = now.UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.ensureOpenLocked(false, nil); err != nil {
		return err
	}
	if err := validateSQLiteSchemaQuick(s.db); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return corruptDatabase(err)
	}
	defer tx.Rollback()
	if err := pruneBrowserSessionsTx(tx, now, max); err != nil {
		return err
	}
	rows, err := tx.Query("SELECT token, csrf, expires_at_ns FROM browser_sessions ORDER BY expires_at_ns, token")
	if err != nil {
		return corruptDatabase(err)
	}
	for rows.Next() {
		var token, csrf string
		var expires int64
		if err := rows.Scan(&token, &csrf, &expires); err != nil {
			rows.Close()
			return corruptDatabase(err)
		}
		expiresAt := time.Unix(0, expires).UTC()
		if !validBrowserSession(token, csrf, expiresAt, now) {
			rows.Close()
			return corruptDatabase(errors.New("malformed browser session"))
		}
		if err := add(token, csrf, expiresAt); err != nil {
			rows.Close()
			return err
		}
	}
	if err := rows.Close(); err != nil {
		return corruptDatabase(err)
	}
	if err := rows.Err(); err != nil {
		return corruptDatabase(err)
	}
	return tx.Commit()
}

// SaveBrowserSession persists one unexpired opaque cookie/CSRF pair, pruning
// expired sessions and respecting the caller's configured maximum.
func (s *SQLiteStore) SaveBrowserSession(token, csrf string, expiresAt time.Time, max int) error {
	if s == nil || !validBrowserSessionLimit(max) {
		return ErrInsecureStatePath
	}
	now := time.Now().UTC()
	expiresAt = expiresAt.UTC()
	if !validBrowserSession(token, csrf, expiresAt, now) {
		return ErrCorruptState
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.ensureOpenLocked(false, nil); err != nil {
		return err
	}
	if err := validateSQLiteSchemaQuick(s.db); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return corruptDatabase(err)
	}
	defer tx.Rollback()
	if err := pruneBrowserSessionsTx(tx, now, max); err != nil {
		return err
	}
	var exists int
	if err := tx.QueryRow("SELECT COUNT(*) FROM browser_sessions WHERE token = ?", token).Scan(&exists); err != nil {
		return corruptDatabase(err)
	}
	if exists == 0 {
		var count int
		if err := tx.QueryRow("SELECT COUNT(*) FROM browser_sessions").Scan(&count); err != nil {
			return corruptDatabase(err)
		}
		if count >= max {
			return fmt.Errorf("browser session limit reached")
		}
	}
	if _, err := tx.Exec("INSERT INTO browser_sessions (token, csrf, expires_at_ns) VALUES (?, ?, ?) ON CONFLICT(token) DO UPDATE SET csrf = excluded.csrf, expires_at_ns = excluded.expires_at_ns", token, csrf, nanos(expiresAt)); err != nil {
		return corruptDatabase(err)
	}
	return tx.Commit()
}

// DeleteBrowserSession durably revokes an opaque browser cookie token.
func (s *SQLiteStore) DeleteBrowserSession(token string) error {
	if s == nil || !validOpaqueBrowserToken(token) {
		return ErrCorruptState
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.ensureOpenLocked(false, nil); err != nil {
		return err
	}
	if err := validateSQLiteSchemaQuick(s.db); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return corruptDatabase(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM browser_sessions WHERE token = ?", token); err != nil {
		return corruptDatabase(err)
	}
	return tx.Commit()
}

func validateBrowserSessionsTx(tx *sql.Tx, now time.Time, max int, prune bool) error {
	if prune {
		if err := pruneBrowserSessionsTx(tx, now, max); err != nil {
			return err
		}
	}
	rows, err := tx.Query("SELECT token, csrf, expires_at_ns FROM browser_sessions")
	if err != nil {
		return corruptDatabase(err)
	}
	count := 0
	for rows.Next() {
		var token, csrf string
		var expires int64
		if err := rows.Scan(&token, &csrf, &expires); err != nil {
			rows.Close()
			return corruptDatabase(err)
		}
		expiresAt := time.Unix(0, expires).UTC()
		if !expiresAt.After(now) || expiresAt.After(now.Add(maxSessionLifetime)) {
			if prune {
				rows.Close()
				return corruptDatabase(errors.New("out-of-window browser session remained after prune"))
			}
			continue
		}
		if !validBrowserSession(token, csrf, expiresAt, now) {
			rows.Close()
			return corruptDatabase(errors.New("malformed browser session"))
		}
		count++
	}
	if err := rows.Close(); err != nil {
		return corruptDatabase(err)
	}
	if err := rows.Err(); err != nil {
		return corruptDatabase(err)
	}
	if count > max {
		return corruptDatabase(errors.New("browser session limit exceeded"))
	}
	return nil
}

func pruneBrowserSessionsTx(tx *sql.Tx, now time.Time, max int) error {
	if !validBrowserSessionLimit(max) {
		return ErrCorruptState
	}
	// Sessions expiring beyond the maximum lifetime can only come from a wall
	// clock that has since stepped backwards (for example a router without an
	// RTC before NTP sync). They are revoked rather than treated as corruption.
	if _, err := tx.Exec("DELETE FROM browser_sessions WHERE expires_at_ns <= ? OR expires_at_ns > ?", nanos(now), nanos(now.Add(maxSessionLifetime))); err != nil {
		return corruptDatabase(err)
	}
	rows, err := tx.Query("SELECT token FROM browser_sessions ORDER BY expires_at_ns, token")
	if err != nil {
		return corruptDatabase(err)
	}
	var excess []string
	count := 0
	for rows.Next() {
		var token string
		if err := rows.Scan(&token); err != nil {
			rows.Close()
			return corruptDatabase(err)
		}
		count++
		if count > max {
			excess = append(excess, token)
		}
	}
	if err := rows.Close(); err != nil {
		return corruptDatabase(err)
	}
	if err := rows.Err(); err != nil {
		return corruptDatabase(err)
	}
	for _, token := range excess {
		if _, err := tx.Exec("DELETE FROM browser_sessions WHERE token = ?", token); err != nil {
			return corruptDatabase(err)
		}
	}
	return nil
}

func validBrowserSessionLimit(max int) bool { return max > 0 && max <= maxBrowserSessions }

func validBrowserSession(token, csrf string, expiresAt, now time.Time) bool {
	return validOpaqueBrowserToken(token) && validOpaqueBrowserToken(csrf) && !expiresAt.IsZero() && expiresAt.After(now) && !expiresAt.After(now.Add(maxSessionLifetime))
}

func validOpaqueBrowserToken(value string) bool {
	if len(value) != 43 {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(decoded) == 32 && base64.RawURLEncoding.EncodeToString(decoded) == value
}
