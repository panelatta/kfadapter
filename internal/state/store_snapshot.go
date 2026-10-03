package state

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"path/filepath"
	"time"
)

// sqliteHeader prefixes every SQLite database file.
var sqliteHeader = []byte("SQLite format 3\x00")

// SnapshotSQLiteFile returns a validated, point-in-time image of the state.db
// at path while the running service may keep writing it. Validation and the
// page copy share one read transaction, so the image is exactly the committed
// state that passed the validate-state checks: a concurrent writer waits on the
// shared lock rather than tearing the copy. The file is opened read-only and
// never modified.
func SnapshotSQLiteFile(path string, semantic ...func(PersistentState) error) ([]byte, error) {
	if path == "" {
		return nil, ErrInsecureStatePath
	}
	clean := filepath.Clean(path)
	if err := ValidateStateDir(filepath.Dir(clean)); err != nil {
		return nil, err
	}
	if err := validateSecureStateFile(clean, maxSQLiteStateBytes); err != nil {
		return nil, err
	}
	db, err := openSQLite(clean, true)
	if err != nil {
		return nil, corruptDatabase(err)
	}
	defer db.Close()
	if err := configureSQLiteReadOnly(db); err != nil {
		return nil, corruptDatabase(err)
	}
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, corruptDatabase(err)
	}
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, corruptDatabase(err)
	}
	defer tx.Rollback()
	// The first read takes the shared lock that pins this snapshot.
	if err := validateSQLiteJournalMode(tx); err != nil {
		return nil, err
	}
	if err := validateSQLiteSchemaObjects(tx, sqliteSchemaVersion, sqliteSchemaTables, sqliteSchemaStatements, true); err != nil {
		return nil, err
	}
	state, err := loadPersistentStateTx(tx)
	if err != nil {
		return nil, err
	}
	if err := validateBrowserSessionsTx(tx, time.Now().UTC(), maxBrowserSessions, false); err != nil {
		return nil, err
	}
	if err := validatePersistentStateSemantics(state, semantic); err != nil {
		return nil, err
	}
	// The transaction is still open on this connection, so the serialized
	// pages are the ones just validated.
	var image []byte
	err = conn.Raw(func(driverConn any) error {
		serializer, ok := driverConn.(interface{ Serialize() ([]byte, error) })
		if !ok {
			return errors.New("SQLite driver cannot serialize a database")
		}
		var serializeErr error
		image, serializeErr = serializer.Serialize()
		return serializeErr
	})
	if err != nil {
		return nil, fmt.Errorf("serialize state database: %w", err)
	}
	if err := validateSQLiteImage(image); err != nil {
		clear(image)
		return nil, err
	}
	return image, nil
}

// validateSQLiteImage checks that a serialized image is a complete, bounded
// rollback-journal database, the only layout restore-state.sh accepts.
func validateSQLiteImage(image []byte) error {
	if len(image) < 100 || int64(len(image)) > maxSQLiteStateBytes || !bytes.HasPrefix(image, sqliteHeader) {
		return corruptDatabase(errors.New("snapshot is not a bounded SQLite database"))
	}
	// Bytes 18 and 19 are the file format write and read versions. Version 1
	// is a rollback journal, so a restored file reopens in journal_mode=DELETE.
	if image[18] != 1 || image[19] != 1 {
		return corruptDatabase(errors.New("snapshot does not use a rollback journal"))
	}
	pageSize := int(binary.BigEndian.Uint16(image[16:18]))
	if pageSize == 1 {
		pageSize = 65536
	}
	pages := int(binary.BigEndian.Uint32(image[28:32]))
	if pageSize < 512 || pageSize&(pageSize-1) != 0 || pages*pageSize != len(image) {
		return corruptDatabase(errors.New("snapshot size does not match its page count"))
	}
	return nil
}
