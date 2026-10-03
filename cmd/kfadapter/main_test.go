package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kfadapter/kfadapter/internal/config"
	"github.com/kfadapter/kfadapter/internal/logging"
	"github.com/kfadapter/kfadapter/internal/state"
)

func runCommand(t *testing.T, arguments ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(arguments, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestVersionAndUnknownCommand(t *testing.T) {
	if code, stdout, _ := runCommand(t, "version"); code != 0 || strings.TrimSpace(stdout) != version {
		t.Fatalf("version = %d %q", code, stdout)
	}
	if code, _, stderr := runCommand(t, "bogus"); code != 2 || !strings.Contains(stderr, "unknown command") {
		t.Fatalf("unknown command = %d %q", code, stderr)
	}
}

func TestValidateConfigReportsReasonWithoutCreatingFiles(t *testing.T) {
	t.Chdir(t.TempDir())
	code, _, stderr := runCommand(t, "validate-config")
	if code != 1 || !strings.Contains(stderr, "invalid configuration: open configuration") {
		t.Fatalf("missing config = %d %q", code, stderr)
	}
	if _, err := os.Stat(configPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("validate-config created a configuration file")
	}
	if err := os.WriteFile(configPath, []byte("listenAddr: 127.0.0.1\nproxy:\n  port: 70000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runCommand(t, "validate-config"); code != 1 || !strings.Contains(stderr, "port must be a decimal integer") {
		t.Fatalf("invalid port = %d %q", code, stderr)
	}
	if err := os.WriteFile(configPath, []byte("listenAddr: 127.0.0.1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, stdout, stderr := runCommand(t, "validate-config"); code != 0 || !strings.Contains(stdout, "configuration valid") {
		t.Fatalf("valid config = %d %q %q", code, stdout, stderr)
	}
	if code, _, _ := runCommand(t, "validate-config", "extra"); code != 1 {
		t.Fatal("validate-config accepted arguments")
	}
}

func TestValidateStateRejectsBadArguments(t *testing.T) {
	for _, arguments := range [][]string{{"validate-state"}, {"validate-state", "--file", ""}, {"validate-state", "--file", "a", "b"}} {
		if code, _, stderr := runCommand(t, arguments...); code != 1 || !strings.Contains(stderr, "invalid state") {
			t.Fatalf("%v = %d %q", arguments, code, stderr)
		}
	}
}

func TestBackupWritesRestorableSnapshot(t *testing.T) {
	t.Chdir(t.TempDir())
	if code, _, stderr := runCommand(t, "backup"); code != 1 || !strings.Contains(stderr, "backup failed") {
		t.Fatalf("backup without state = %d %q", code, stderr)
	}
	if err := os.Mkdir(stateDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := state.NewSQLiteStore(stateDirectory)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	persistent, err := store.LoadOrCreate()
	if err != nil {
		t.Fatal(err)
	}
	// The service keeps its connection open while the snapshot is taken.
	code, stdout, stderr := runCommand(t, "backup")
	if code != 0 || stderr != "" {
		t.Fatalf("backup = %d %q", code, stderr)
	}
	restoreDir := filepath.Join(t.TempDir(), "restore")
	if err := os.Mkdir(restoreDir, 0o700); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(restoreDir, "state.db")
	if err := os.WriteFile(restored, []byte(stdout), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runCommand(t, "validate-state", "--file", restored); code != 0 {
		t.Fatalf("restored snapshot invalid: %q", stderr)
	}
	restoredStore, err := state.NewSQLiteStore(restoreDir)
	if err != nil {
		t.Fatal(err)
	}
	defer restoredStore.Close()
	loaded, err := restoredStore.Load()
	if err != nil || loaded.InstallationID != persistent.InstallationID {
		t.Fatalf("restored snapshot = %v", err)
	}
	if code, _, _ := runCommand(t, "backup", "extra"); code != 1 {
		t.Fatal("backup accepted arguments")
	}
}

func TestHealthcheckFailsWithoutService(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile(configPath, []byte("listenAddr: 127.0.0.1\nmanagement:\n  port: "+strconv.Itoa(freePort(t))+"\nproxy:\n  port: "+strconv.Itoa(freePort(t))+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runCommand(t, "healthcheck"); code != 1 || !strings.Contains(stderr, "healthcheck failed:") {
		t.Fatalf("healthcheck = %d %q", code, stderr)
	}
}

func TestListenInterfaceValidation(t *testing.T) {
	addresses := []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("192.0.2.5")}
	for _, listen := range []string{"0.0.0.0", "127.0.0.1", "192.0.2.5"} {
		if err := validateListenInterfaceAddresses(listen, addresses); err != nil {
			t.Fatalf("%s: %v", listen, err)
		}
	}
	for _, listen := range []string{"192.0.2.6", "::", "not-an-ip"} {
		if err := validateListenInterfaceAddresses(listen, addresses); err == nil {
			t.Fatalf("%s accepted", listen)
		}
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func TestAdapterStartsServesHealthAndStopsCleanly(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.ListenAddr = "127.0.0.1"
	cfg.Management.Port = config.Port(freePort(t))
	cfg.Proxy.Port = config.Port(freePort(t))
	var logs bytes.Buffer
	adapter, err := newAdapterAtStateDirectory(cfg, stateDir, logging.New(&logs))
	if err != nil {
		t.Fatalf("start adapter: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- adapter.runContext(ctx) }()

	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(5 * time.Second)
	for {
		response, err := client.Get("http://" + cfg.ManagementAddress() + "/healthz")
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("management endpoint never became healthy: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	proxy, err := net.DialTimeout("tcp", cfg.ProxyAddress(), 2*time.Second)
	if err != nil {
		t.Fatalf("proxy listener unavailable: %v", err)
	}
	_ = proxy.Close()
	if !strings.Contains(logs.String(), "msg=ready") {
		t.Fatalf("ready was not logged: %q", logs.String())
	}
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("adapter stopped with %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("adapter did not stop")
	}
}
