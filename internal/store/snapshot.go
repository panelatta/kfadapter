package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"

	domain "github.com/oshinop/chinadapter/internal/state"
)

// sqliteHeader prefixes every SQLite database file.
var sqliteHeader = []byte("SQLite format 3\x00")

// SnapshotSQLiteFile returns a validated, point-in-time image of the state.db
// at path while the running daemon may keep writing it. Validation and the
// page copy share one read transaction, so the image is exactly the committed
// state that passed the same checks as ValidateSQLiteFile; a concurrent writer
// waits for the shared lock rather than tearing the copy. The file is opened
// read-only and never modified.
func SnapshotSQLiteFile(ctx context.Context, path string, semantic ...func(domain.PersistentState) error) ([]byte, error) {
	var image []byte
	err := readSQLiteFile(ctx, path, func(conn *sql.Conn, tx *sql.Tx) error {
		if err := validateCurrentSQLiteStateTx(ctx, tx, semantic); err != nil {
			return err
		}
		var err error
		image, err = serializeSQLite(conn)
		return err
	})
	if err != nil {
		clear(image)
		return nil, err
	}
	return image, nil
}

// serializeSQLite copies the main database through conn. Inside a read
// transaction, the image holds exactly the pages that transaction validated.
func serializeSQLite(conn *sql.Conn) ([]byte, error) {
	var image []byte
	err := conn.Raw(func(driverConn any) error {
		serializer, ok := driverConn.(interface{ Serialize() ([]byte, error) })
		if !ok {
			return errors.New("SQLite driver cannot serialize a database")
		}
		var err error
		image, err = serializer.Serialize()
		return err
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
// rollback-journal database, the only layout the store reopens.
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
