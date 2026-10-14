package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oshinop/chinadapter/internal/store"
)

func TestHealthcheckUsesConfiguredWildcardListeners(t *testing.T) {
	management, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer management.Close()
	proxy, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	})}
	go func() { _ = server.Serve(management) }()
	defer server.Shutdown(context.Background())
	accepted := make(chan struct{}, 1)
	go func() {
		c, err := proxy.Accept()
		if err == nil {
			close(accepted)
			_ = c.Close()
		}
	}()
	writeWorkingConfig(t, management.Addr().String(), proxy.Addr().String())
	var stdout, stderr bytes.Buffer
	if code := run([]string{"healthcheck"}, &stdout, &stderr); code != 0 {
		t.Fatalf("healthcheck exit=%d stderr=%q", code, stderr.String())
	}
	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("healthcheck did not connect to wildcard proxy")
	}
}

func TestServeRejectsBareProcessAtSeam(t *testing.T) {
	t.Chdir(t.TempDir())
	previous := requireContainer
	requireContainer = func() error { return errors.New("container required") }
	t.Cleanup(func() { requireContainer = previous })
	if err := os.WriteFile(configPath, []byte("listenAddr: 127.0.0.1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := serve(nil); err == nil || !strings.Contains(err.Error(), "container required") {
		t.Fatalf("serve error=%v", err)
	}
}

func TestRunRejectsUnknownAndExtraServeArguments(t *testing.T) {
	for _, args := range [][]string{{"unknown"}, {"-x"}, {"serve", "extra"}} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 2 || !strings.HasPrefix(stderr.String(), "usage: chinadapter ") {
			t.Fatalf("args=%v code=%d stderr=%q", args, code, stderr.String())
		}
	}
}

func runCommand(t *testing.T, arguments ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(arguments, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestHealthcheckReportsWhyItFailed(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile(configPath, []byte("bogus: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runCommand(t, "healthcheck")
	if code != 1 || !strings.Contains(stderr, "chinadapter: healthcheck failed") || !strings.Contains(stderr, "decode configuration") {
		t.Fatalf("healthcheck = %d %q", code, stderr)
	}
}

func TestValidateStateRejectsBadArguments(t *testing.T) {
	for _, arguments := range [][]string{{"validate-state"}, {"validate-state", "--file", ""}, {"validate-state", "--file", "a", "b"}, {"validate-state", "--bogus"}} {
		if code, stdout, stderr := runCommand(t, arguments...); code != 1 || stdout != "" || !strings.Contains(stderr, "chinadapter: invalid state") || !strings.Contains(stderr, "invalid validate-state arguments") {
			t.Fatalf("%v = %d %q %q", arguments, code, stdout, stderr)
		}
	}
}

func TestValidateStateReportsWhyStateIsInvalid(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "state.db")
	if err := os.WriteFile(path, bytes.Repeat([]byte("not a SQLite database "), 64), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runCommand(t, "validate-state", "--file", path); code != 1 || !strings.Contains(stderr, "persistent state is corrupt") {
		t.Fatalf("corrupt state = %d %q", code, stderr)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runCommand(t, "validate-state", "--file", path); code != 1 || !strings.Contains(stderr, "insecure ownership or mode") {
		t.Fatalf("world-readable state = %d %q", code, stderr)
	}
}

func TestValidateStateAcceptsAnOlderSupportedSchema(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "internal", "store", "testdata", "v8.db"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "state.db")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if code, stdout, stderr := runCommand(t, "validate-state", "--file", path); code != 0 || stdout != "state valid\n" {
		t.Fatalf("v8 state = %d %q %q", code, stdout, stderr)
	}
	if current, err := os.ReadFile(path); err != nil || !bytes.Equal(current, data) {
		t.Fatal("validate-state modified the file")
	}
}

func TestBackupWritesRestorableSnapshot(t *testing.T) {
	t.Chdir(t.TempDir())
	if code, stdout, stderr := runCommand(t, "backup"); code != 1 || stdout != "" || !strings.HasPrefix(stderr, "chinadapter: backup failed: ") {
		t.Fatalf("backup without state = %d %q", code, stderr)
	}
	if err := os.Mkdir(stateDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	source, err := store.NewSQLiteStore(stateDirectory)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close(context.Background())
	persistent, err := source.LoadOrCreate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// The daemon keeps its connection open while the snapshot is taken.
	code, stdout, stderr := runCommand(t, "backup")
	if code != 0 || stderr != "" || !strings.HasPrefix(stdout, "SQLite format 3\x00") {
		t.Fatalf("backup = %d %q", code, stderr)
	}
	restoreDirectory := filepath.Join(t.TempDir(), "restore")
	if err := os.Mkdir(restoreDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(restoreDirectory, "state.db")
	if err := os.WriteFile(restored, []byte(stdout), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, stdout, stderr := runCommand(t, "validate-state", "--file", restored); code != 0 || stdout != "state valid\n" {
		t.Fatalf("restored snapshot invalid: %d %q", code, stderr)
	}
	restoredStore, err := store.NewSQLiteStore(restoreDirectory)
	if err != nil {
		t.Fatal(err)
	}
	defer restoredStore.Close(context.Background())
	loaded, err := restoredStore.Load(t.Context())
	if err != nil || loaded.InstallationID != persistent.InstallationID {
		t.Fatalf("restored snapshot = %v", err)
	}
	if code, stdout, _ := runCommand(t, "backup", "extra"); code != 1 || stdout != "" {
		t.Fatal("backup accepted arguments")
	}
}

func writeWorkingConfig(t *testing.T, management, proxy string) {
	t.Helper()
	t.Chdir(t.TempDir())
	m := netip.MustParseAddrPort(management)
	p := netip.MustParseAddrPort(proxy)
	body := fmt.Sprintf("listenAddr: %q\nmanagement:\n  port: %d\nproxy:\n  port: %d\n", m.Addr().String(), m.Port(), p.Port())
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
