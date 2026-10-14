package store

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	domain "github.com/oshinop/chinadapter/internal/state"
)

// restoreImage writes a snapshot the way a restore lays it out and reopens it
// as a store.
func restoreImage(t *testing.T, image []byte) *SQLiteStore {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, sqliteStateFileName)
	if err := os.WriteFile(path, image, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSQLiteFile(t.Context(), path); err != nil {
		t.Fatalf("restored snapshot is invalid: %v", err)
	}
	restored, err := NewSQLiteStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restored.Close(t.Context()) })
	return restored
}

// boundTestStore holds an access token and a three-node catalog.
func boundTestStore(t *testing.T) (*SQLiteStore, domain.PersistentState) {
	t.Helper()
	store, base, now := newAccessStore(t)
	if err := store.Save(t.Context(), largeCatalogState(t, base, now, 3, 8)); err != nil {
		t.Fatal(err)
	}
	saved, err := store.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return store, saved
}

// copyStateFixture copies a testdata database into a fresh state directory.
func copyStateFixture(t *testing.T, name string) string {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, sqliteStateFileName)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSnapshotRestoresTheStoredAggregate(t *testing.T) {
	store, saved := boundTestStore(t)
	image, err := SnapshotSQLiteFile(t.Context(), store.Path())
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := restoreImage(t, image).Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded, saved) {
		t.Fatal("restored snapshot differs from the stored aggregate")
	}
	// The source stays usable: the snapshot took no lasting lock.
	if _, err := store.Update(t.Context(), func(candidate *domain.PersistentState) error {
		candidate.Preferences.RevealEndpoints = true
		return nil
	}); err != nil {
		t.Fatalf("store unusable after snapshot: %v", err)
	}
}

func TestSnapshotExcludesUncommittedWrites(t *testing.T) {
	store, _ := boundTestStore(t)
	// The store's writers begin IMMEDIATE transactions and hold the reserved lock.
	tx, err := store.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(t.Context(), "UPDATE preferences SET refresh_policy = '30m' WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	image, err := SnapshotSQLiteFile(t.Context(), store.Path())
	if err != nil {
		t.Fatalf("snapshot beside an open write transaction: %v", err)
	}
	loaded, err := restoreImage(t, image).Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Preferences.RefreshPolicy != "" {
		t.Fatalf("snapshot captured an uncommitted write: %q", loaded.Preferences.RefreshPolicy)
	}
}

func TestSnapshotIsConsistentDuringConcurrentWrites(t *testing.T) {
	store, _ := boundTestStore(t)
	policies := []string{"15m", "30m", "1h", "2h"}
	var writer sync.WaitGroup
	writerErr := make(chan error, 1)
	writer.Add(1)
	go func() {
		defer writer.Done()
		for index := range 40 {
			if _, err := store.Update(t.Context(), func(candidate *domain.PersistentState) error {
				// Each commit changes rows in several tables at once; a torn
				// copy would pair a policy with the wrong node RTT.
				candidate.Preferences.RefreshPolicy = policies[index%len(policies)]
				for position := range candidate.ActiveSession.Nodes {
					candidate.ActiveSession.Nodes[position].TCPRTT = time.Duration(index) * time.Millisecond
				}
				return nil
			}); err != nil {
				writerErr <- err
				return
			}
		}
	}()
	done := make(chan struct{})
	go func() { writer.Wait(); close(done) }()

	snapshots := 0
	for finished := false; !finished; {
		select {
		case <-done:
			finished = true
		default:
		}
		image, err := SnapshotSQLiteFile(t.Context(), store.Path())
		if err != nil {
			t.Fatalf("snapshot %d: %v", snapshots, err)
		}
		loaded, err := restoreImage(t, image).Load(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		rtt := loaded.ActiveSession.Nodes[0].TCPRTT
		for _, node := range loaded.ActiveSession.Nodes {
			if node.TCPRTT != rtt {
				t.Fatalf("snapshot %d mixes two commits", snapshots)
			}
		}
		if rtt != 0 || loaded.Preferences.RefreshPolicy != "" {
			index := int(rtt / time.Millisecond)
			if loaded.Preferences.RefreshPolicy != policies[index%len(policies)] {
				t.Fatalf("snapshot %d pairs policy %q with commit %d", snapshots, loaded.Preferences.RefreshPolicy, index)
			}
		}
		snapshots++
	}
	select {
	case err := <-writerErr:
		t.Fatalf("writer failed beside snapshots: %v", err)
	default:
	}
}

func TestSnapshotRejectsUnsafeOrInvalidInput(t *testing.T) {
	store, _ := boundTestStore(t)
	if err := os.Chmod(store.Path(), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := SnapshotSQLiteFile(t.Context(), store.Path()); !errors.Is(err, domain.ErrInsecureStatePath) {
		t.Fatalf("snapshot of a world-readable file = %v", err)
	}
	if err := os.Chmod(store.Path(), 0o600); err != nil {
		t.Fatal(err)
	}
	image, err := SnapshotSQLiteFile(t.Context(), store.Path())
	if err != nil {
		t.Fatal(err)
	}
	wal := append([]byte(nil), image...)
	wal[18], wal[19] = 2, 2
	for name, candidate := range map[string][]byte{
		"empty":      nil,
		"not sqlite": bytes.Repeat([]byte("not a SQLite database "), 8),
		"wal":        wal,
		"truncated":  image[:len(image)-512],
	} {
		if err := validateSQLiteImage(candidate); !errors.Is(err, domain.ErrCorruptState) {
			t.Fatalf("%s image = %v", name, err)
		}
	}
	rejected := errors.New("semantic rejection")
	if _, err := SnapshotSQLiteFile(t.Context(), store.Path(), func(domain.PersistentState) error { return rejected }); !errors.Is(err, rejected) {
		t.Fatalf("semantic validator not applied: %v", err)
	}
	if _, err := store.db.ExecContext(t.Context(), "UPDATE preferences SET refresh_policy = 'bogus' WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	if _, err := SnapshotSQLiteFile(t.Context(), store.Path()); !errors.Is(err, domain.ErrCorruptState) {
		t.Fatalf("snapshot of an invalid aggregate = %v", err)
	}
	if err := ValidateSQLiteFile(t.Context(), store.Path()); !errors.Is(err, domain.ErrCorruptState) {
		t.Fatalf("validation of an invalid aggregate = %v", err)
	}
}

func TestSnapshotRequiresTheCurrentSchema(t *testing.T) {
	path := copyStateFixture(t, "v8.db")
	if _, err := SnapshotSQLiteFile(t.Context(), path); !errors.Is(err, domain.ErrCorruptState) {
		t.Fatalf("snapshot of an unmigrated v8 database = %v", err)
	}
}

func TestValidateSQLiteFileMigratesOnlyAPrivateCopy(t *testing.T) {
	for _, fixture := range []struct {
		name    string
		version int
	}{{"v8.db", 8}, {"kfadapter-v10.db", 10}} {
		t.Run(fixture.name, func(t *testing.T) {
			path := copyStateFixture(t, fixture.name)
			temporary := t.TempDir()
			t.Setenv("TMPDIR", temporary)
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := ValidateSQLiteFile(t.Context(), path); err != nil {
				t.Fatalf("supported v%d database rejected: %v", fixture.version, err)
			}
			rejected := errors.New("semantic rejection")
			if err := ValidateSQLiteFile(t.Context(), path, func(domain.PersistentState) error { return rejected }); !errors.Is(err, rejected) {
				t.Fatalf("semantic validator not applied after migration: %v", err)
			}
			if current, err := os.ReadFile(path); err != nil || !bytes.Equal(current, original) {
				t.Fatal("validation modified the source database")
			}
			if entries, err := os.ReadDir(filepath.Dir(path)); err != nil || len(entries) != 1 {
				t.Fatalf("validation left files beside the source: %v %v", entries, err)
			}
			if entries, err := os.ReadDir(temporary); err != nil || len(entries) != 0 {
				t.Fatalf("validation left its migrated copy behind: %v %v", entries, err)
			}
		})
	}
}

func TestValidateSQLiteFileRejectsUnsupportedSchemaVersions(t *testing.T) {
	store, _ := newClockedStore(t)
	for _, version := range []int{3, sqliteSchemaVersion + 1} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			if _, err := store.db.ExecContext(t.Context(), "UPDATE schema_version SET version = ? WHERE id = 1", version); err != nil {
				t.Fatal(err)
			}
			if err := ValidateSQLiteFile(t.Context(), store.Path()); !errors.Is(err, domain.ErrCorruptState) {
				t.Fatalf("schema v%d = %v", version, err)
			}
		})
	}
	if err := ValidateSQLiteFile(t.Context(), ""); !errors.Is(err, domain.ErrInsecureStatePath) {
		t.Fatalf("empty path = %v", err)
	}
	if err := ValidateSQLiteFile(t.Context(), filepath.Join(t.TempDir(), sqliteStateFileName)); err == nil {
		t.Fatal("validation accepted a missing file")
	}
}
