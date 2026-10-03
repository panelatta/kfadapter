package state

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/kfadapter/kfadapter/internal/provider"
)

// restoreImage writes a snapshot the way restore-state.sh lays it out and
// reopens it as a store.
func restoreImage(t *testing.T, image []byte) *SQLiteStore {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, sqliteStateFileName)
	if err := os.WriteFile(path, image, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSQLiteFile(path); err != nil {
		t.Fatalf("restored snapshot is invalid: %v", err)
	}
	restored, err := NewSQLiteStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restored.Close() })
	return restored
}

func boundTestStore(t *testing.T) (*SQLiteStore, PersistentState) {
	t.Helper()
	store := newTestStore(t)
	base := time.Unix(1_900_000_000, 0).UTC()
	saved, err := store.Update(func(candidate *PersistentState) error {
		if err := candidate.SetAccessToken("correct horse battery token"); err != nil {
			return err
		}
		bindSession(t, candidate, testSession(1, base, map[provider.ID][]string{"kuaifan": {"k1", "k2"}, "quickfox": {"q1"}}))
		candidate.LastGood = testLastGood(base, "k1", "k2")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, saved
}

func TestSnapshotRestoresTheStoredAggregate(t *testing.T) {
	store, saved := boundTestStore(t)
	image, err := SnapshotSQLiteFile(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := restoreImage(t, image).Load()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(persistentStateRows(&loaded), persistentStateRows(&saved)) {
		t.Fatal("restored snapshot differs from the stored aggregate")
	}
	// The source stays usable: the snapshot took no lasting lock.
	if _, err := store.Update(func(candidate *PersistentState) error {
		candidate.Preferences.RevealEndpoints = true
		return nil
	}); err != nil {
		t.Fatalf("store unusable after snapshot: %v", err)
	}
}

func TestSnapshotExcludesUncommittedWrites(t *testing.T) {
	store, _ := boundTestStore(t)
	tx, err := store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("UPDATE preferences SET refresh_policy = '30m' WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	image, err := SnapshotSQLiteFile(store.Path())
	if err != nil {
		t.Fatalf("snapshot beside an open write transaction: %v", err)
	}
	loaded, err := restoreImage(t, image).Load()
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
			if _, err := store.Update(func(candidate *PersistentState) error {
				// Each commit changes rows in several tables at once; a torn
				// copy would pair a policy with the wrong node health.
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
		image, err := SnapshotSQLiteFile(store.Path())
		if err != nil {
			t.Fatalf("snapshot %d: %v", snapshots, err)
		}
		loaded, err := restoreImage(t, image).Load()
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
	if _, err := SnapshotSQLiteFile(store.Path()); !errors.Is(err, ErrInsecureStatePath) {
		t.Fatalf("snapshot of a world-readable file = %v", err)
	}
	if err := os.Chmod(store.Path(), 0o600); err != nil {
		t.Fatal(err)
	}
	image, err := SnapshotSQLiteFile(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	wal := append([]byte(nil), image...)
	wal[18], wal[19] = 2, 2
	for name, candidate := range map[string][]byte{
		"empty":      nil,
		"not sqlite": []byte("not a SQLite database, just text padding it past the header length...................................."),
		"wal":        wal,
		"truncated":  image[:len(image)-512],
	} {
		if err := validateSQLiteImage(candidate); !errors.Is(err, ErrCorruptState) {
			t.Fatalf("%s image = %v", name, err)
		}
	}
	rejected := errors.New("semantic rejection")
	if _, err := SnapshotSQLiteFile(store.Path(), func(PersistentState) error { return rejected }); !errors.Is(err, rejected) {
		t.Fatalf("semantic validator not applied: %v", err)
	}
	if _, err := store.db.Exec("UPDATE preferences SET refresh_policy = 'bogus' WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	if _, err := SnapshotSQLiteFile(store.Path()); !errors.Is(err, ErrCorruptState) {
		t.Fatalf("snapshot of an invalid aggregate = %v", err)
	}
}
