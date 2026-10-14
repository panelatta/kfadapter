package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	domain "github.com/oshinop/chinadapter/internal/state"
)

// ValidateStateDir requires a real, current-user-owned 0700 directory.
func ValidateStateDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("state directory %q: %w", dir, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return fmt.Errorf("state directory %q: %w", dir, domain.ErrInsecureStatePath)
	}
	if err := ownedByCurrentUser(info); err != nil {
		return fmt.Errorf("state directory %q: %w", dir, err)
	}
	return nil
}

func ownedByCurrentUser(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return domain.ErrInsecureStatePath
	}
	return nil
}

func hasSingleLink(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink == 1
}

func (s *SQLiteStore) validate() error {
	if s == nil {
		return domain.ErrInsecureStatePath
	}
	return ValidateStateDir(s.dir)
}

// ValidateSQLiteFile validates an existing state.db without writing it, as a
// restore would accept it. A current database is validated in place. An older
// supported schema is copied into a private temporary directory and opened by
// the normal store, which checks and migrates it exactly as daemon startup
// would; the copy is then validated as current state and removed. Optional
// semantic validators inspect the aggregate after relational validation.
func ValidateSQLiteFile(ctx context.Context, path string, semantic ...func(domain.PersistentState) error) error {
	var image []byte
	defer func() { clear(image) }()
	err := readSQLiteFile(ctx, path, func(conn *sql.Conn, tx *sql.Tx) error {
		var version int
		if err := tx.QueryRowContext(ctx, "SELECT version FROM schema_version WHERE id = 1").Scan(&version); err != nil {
			return corruptDatabase(err)
		}
		if version == sqliteSchemaVersion {
			return validateCurrentSQLiteStateTx(ctx, tx, semantic)
		}
		var err error
		image, err = serializeSQLite(conn)
		return err
	})
	if err != nil || image == nil {
		return err
	}
	return validateMigratedSQLiteImage(ctx, image, semantic)
}

// readSQLiteFile opens an existing state.db read-only and runs read inside one
// read transaction on a dedicated connection, after the journal mode and
// integrity checks. The file is never written.
func readSQLiteFile(ctx context.Context, path string, read func(*sql.Conn, *sql.Tx) error) error {
	if path == "" {
		return domain.ErrInsecureStatePath
	}
	clean := filepath.Clean(path)
	if err := ValidateStateDir(filepath.Dir(clean)); err != nil {
		return err
	}
	if err := validateSecureStateFile(clean, maxSQLiteStateBytes); err != nil {
		return err
	}
	db, err := openSQLite(ctx, clean, true)
	if err != nil {
		return corruptDatabase(err)
	}
	defer func() { _ = db.Close() }()
	if err := configureSQLiteReadOnly(ctx, db); err != nil {
		return corruptDatabase(err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return corruptDatabase(err)
	}
	defer func() { _ = conn.Close() }()
	// With _txlock=immediate only a read-only transaction issues a plain BEGIN,
	// so this reader never asks for the write lock the daemon's writers need.
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return corruptDatabase(err)
	}
	defer tx.Rollback()
	// The first read takes the shared lock that pins this point in time.
	if err := validateSQLiteJournalMode(ctx, tx); err != nil {
		return err
	}
	if err := checkSQLiteIntegrity(ctx, tx); err != nil {
		return err
	}
	if err := read(conn, tx); err != nil {
		return err
	}
	return corruptDatabase(tx.Commit())
}

// validateCurrentSQLiteStateTx checks the exact current schema and the
// aggregate it stores, without pruning browser sessions.
func validateCurrentSQLiteStateTx(ctx context.Context, tx *sql.Tx, semantic []func(domain.PersistentState) error) error {
	if err := validateSQLiteSchema(ctx, tx); err != nil {
		return err
	}
	state, err := loadPersistentStateTx(ctx, tx)
	if err != nil {
		return err
	}
	if err := validateBrowserSessionsTx(ctx, tx, time.Now().UTC(), maxBrowserSessions, false); err != nil {
		return err
	}
	return validatePersistentStateSemantics(state, semantic)
}

// validateMigratedSQLiteImage migrates a private copy of an older database the
// way daemon startup would, then validates the result as current state.
func validateMigratedSQLiteImage(ctx context.Context, image []byte, semantic []func(domain.PersistentState) error) (err error) {
	dir, err := os.MkdirTemp("", "chinadapter-state-")
	if err != nil {
		return fmt.Errorf("create state validation directory: %w", err)
	}
	defer func() {
		if removeErr := os.RemoveAll(dir); removeErr != nil {
			err = errors.Join(err, fmt.Errorf("remove state validation copy: %w", removeErr))
		}
	}()
	// MkdirTemp honors the umask; the store requires exactly 0700.
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("chmod state validation directory: %w", err)
	}
	path := filepath.Join(dir, sqliteStateFileName)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("create state validation copy: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return fmt.Errorf("chmod state validation copy: %w", err)
	}
	if _, err := file.Write(image); err != nil {
		_ = file.Close()
		return fmt.Errorf("write state validation copy: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close state validation copy: %w", err)
	}
	store, err := NewSQLiteStore(dir)
	if err != nil {
		return err
	}
	_, err = store.Load(ctx)
	if closeErr := store.Close(ctx); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return readSQLiteFile(ctx, path, func(_ *sql.Conn, tx *sql.Tx) error {
		return validateCurrentSQLiteStateTx(ctx, tx, semantic)
	})
}

func validatePersistentStateSemantics(state domain.PersistentState, validators []func(domain.PersistentState) error) error {
	for _, validator := range validators {
		if validator == nil {
			continue
		}
		if err := validator(state.Clone()); err != nil {
			return err
		}
	}
	return nil
}

func validateEmptyStateDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read state directory: %w", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, strconv.Quote(entry.Name()))
		}
		return fmt.Errorf("state directory must be empty before first run but contains %s: %w", strings.Join(names, ", "), domain.ErrInsecureStatePath)
	}
	return nil
}

func stateFileExists(path string, maximum int64) (bool, error) {
	err := validateSecureStateFile(path, maximum)
	if errors.Is(err, domain.ErrStateNotFound) {
		return false, nil
	}
	return err == nil, err
}

func validateSecureStateFile(path string, maximum int64) error {
	file, err := openSecureStateFile(path, maximum)
	if err != nil {
		return err
	}
	return file.Close()
}

func openSecureStateFile(path string, maximum int64) (*os.File, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, domain.ErrStateNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("stat state file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || !hasSingleLink(info) {
		return nil, domain.ErrInsecureStatePath
	}
	if err := ownedByCurrentUser(info); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, domain.ErrStateNotFound
	}
	if errors.Is(err, syscall.ELOOP) {
		return nil, domain.ErrInsecureStatePath
	}
	if err != nil {
		return nil, fmt.Errorf("open state file: %w", err)
	}
	opened, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("stat opened state file: %w", err)
	}
	if !opened.Mode().IsRegular() || opened.Mode().Perm() != 0o600 || opened.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || !hasSingleLink(opened) {
		_ = file.Close()
		return nil, domain.ErrInsecureStatePath
	}
	if err := ownedByCurrentUser(opened); err != nil {
		_ = file.Close()
		return nil, err
	}
	if opened.Size() < 0 || opened.Size() > maximum {
		_ = file.Close()
		return nil, fmt.Errorf("%w: state file exceeds maximum size", domain.ErrCorruptState)
	}
	return file, nil
}

// sqliteQuerier is satisfied by *sql.DB, *sql.Conn, and *sql.Tx, so validation
// can also run inside a caller's read transaction.
type sqliteQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func validateSQLiteJournalMode(ctx context.Context, db sqliteQuerier) error {
	var mode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		return corruptDatabase(err)
	}
	if !strings.EqualFold(mode, "delete") {
		return corruptDatabase(fmt.Errorf("unexpected SQLite journal mode %q", mode))
	}
	return nil
}

func validateSQLiteSchema(ctx context.Context, db sqliteQuerier) error {
	return validateSQLiteSchemaDefinition(ctx, db, sqliteSchemaVersion, sqliteSchemaTables, sqliteSchemaStatements)
}

func validateSQLiteSchemaDefinition(ctx context.Context, db sqliteQuerier, expectedVersion int, expectedTables map[string]int, expectedStatements []string) error {
	if err := validateSQLiteJournalMode(ctx, db); err != nil {
		return err
	}
	rows, err := db.QueryContext(ctx, "SELECT type, name, tbl_name, sql FROM sqlite_master WHERE name NOT LIKE 'sqlite_%'")
	if err != nil {
		return corruptDatabase(err)
	}
	defer rows.Close()
	tables := make(map[string]struct{}, len(expectedTables))
	for rows.Next() {
		var objectType, name, tableName string
		var definition sql.NullString
		if err := rows.Scan(&objectType, &name, &tableName, &definition); err != nil {
			return corruptDatabase(err)
		}
		statementIndex, expected := expectedTables[name]
		if !expected || objectType != "table" || tableName != name || !definition.Valid || statementIndex >= len(expectedStatements) || definition.String != expectedStatements[statementIndex] {
			return corruptDatabase(fmt.Errorf("unexpected SQLite schema object %q", name))
		}
		if _, duplicate := tables[name]; duplicate {
			return corruptDatabase(fmt.Errorf("duplicate SQLite table %q", name))
		}
		tables[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return corruptDatabase(err)
	}
	if len(tables) != len(expectedTables) {
		return corruptDatabase(errors.New("missing SQLite schema tables"))
	}
	versions, err := db.QueryContext(ctx, "SELECT id, version FROM schema_version")
	if err != nil {
		return corruptDatabase(err)
	}
	defer versions.Close()
	count := 0
	for versions.Next() {
		var id, version int
		if err := versions.Scan(&id, &version); err != nil {
			return corruptDatabase(err)
		}
		if id != 1 || version != expectedVersion {
			return corruptDatabase(errors.New("unsupported SQLite schema version"))
		}
		count++
	}
	if err := versions.Err(); err != nil || count != 1 {
		if err != nil {
			return corruptDatabase(err)
		}
		return corruptDatabase(errors.New("missing SQLite schema version"))
	}
	foreign, err := db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return corruptDatabase(err)
	}
	defer foreign.Close()
	if foreign.Next() {
		return corruptDatabase(errors.New("foreign key violation"))
	}
	if err := foreign.Err(); err != nil {
		return corruptDatabase(err)
	}
	return nil
}

func checkSQLiteIntegrity(ctx context.Context, db sqliteQuerier) error {
	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check(1)").Scan(&integrity); err != nil {
		return corruptDatabase(err)
	}
	if integrity != "ok" {
		return corruptDatabase(errors.New("SQLite integrity check failed"))
	}
	return nil
}
