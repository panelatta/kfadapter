package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/oshinop/chinadapter/internal/store"
)

// backupState writes a validated, point-in-time copy of state.db to stdout
// while the daemon keeps running. The container filesystem is read-only, so
// the image leaves through the exec stream and is archived on the host.
func backupState(arguments []string, stdout io.Writer) error {
	if len(arguments) != 0 {
		return errors.New("backup arguments are not supported")
	}
	if file, ok := stdout.(*os.File); ok {
		if info, err := file.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
			return errors.New("refusing to write the state database to a terminal; redirect standard output")
		}
	}
	image, err := store.SnapshotSQLiteFile(context.Background(), filepath.Join(stateDirectory, "state.db"))
	if err != nil {
		return err
	}
	defer clear(image)
	_, err = stdout.Write(image)
	return err
}
