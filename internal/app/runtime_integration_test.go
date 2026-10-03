package app

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kfadapter/kfadapter/internal/logging"
	"github.com/kfadapter/kfadapter/internal/provider"
	"github.com/kfadapter/kfadapter/internal/provider/coordinator"
	"github.com/kfadapter/kfadapter/internal/selector"
	"github.com/kfadapter/kfadapter/internal/state"
	"github.com/kfadapter/kfadapter/internal/subscription"
	"github.com/kfadapter/kfadapter/internal/web"
)

const testProtocol provider.Protocol = "test-native"

const testAccessToken = "correct horse battery token"

type fakeDriver struct {
	id provider.ID

	mu         sync.Mutex
	userID     string
	refreshErr error
	lifetime   time.Duration
}

func (driver *fakeDriver) ID() provider.ID { return driver.id }

func (driver *fakeDriver) snapshot() provider.Snapshot {
	driver.mu.Lock()
	defer driver.mu.Unlock()
	return provider.Snapshot{
		Provider:     driver.id,
		Account:      provider.Account{UserID: driver.userID, Display: "a•••@example.com", Tier: "standard"},
		ExpiresAt:    time.Now().Add(driver.lifetime),
		RefreshState: []byte("refresh"),
		Authorities:  map[string]provider.Authority{"default": {Protocol: testProtocol, Data: []byte("authority")}},
		Nodes: []provider.Node{{
			ID: string(driver.id) + "_node", AuthorityID: "default", Protocol: testProtocol,
			Host: "192.0.2.1", Port: 443, Name: string(driver.id) + " node", Group: "Group", Eligible: true,
		}},
	}
}

func (driver *fakeDriver) Login(context.Context, provider.Credentials) (provider.Snapshot, error) {
	return driver.snapshot(), nil
}

func (driver *fakeDriver) Refresh(context.Context, provider.Snapshot) (provider.Snapshot, error) {
	driver.mu.Lock()
	err := driver.refreshErr
	driver.mu.Unlock()
	if err != nil {
		return provider.Snapshot{}, err
	}
	return driver.snapshot(), nil
}

type nopApplier struct{}

func (nopApplier) SetSelectors(*selector.Registry) error { return nil }

type runtimeHarness struct {
	t       *testing.T
	runtime *Runtime
	store   *state.SQLiteStore
	manager *state.Manager
	drivers map[provider.ID]*fakeDriver
	logs    *syncBuffer
}

type syncBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

func newRuntimeHarness(t *testing.T) *runtimeHarness {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := state.NewSQLiteStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	persistent, err := store.LoadOrCreate(subscription.ValidatePersistentState)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := state.NewManagerWithSubscription(nil, persistent.Subscription, nil)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := selector.NewRegistry(persistent.Subscription)
	if err != nil {
		t.Fatal(err)
	}
	selectors, err := NewSelectorCoordinator(nopApplier{}, registry)
	if err != nil {
		t.Fatal(err)
	}
	mutations := &sync.Mutex{}
	subscriptions, err := subscription.NewService(subscription.ServiceConfig{Store: store, SocksAddress: "127.0.0.1:10808", MutationLocker: mutations})
	if err != nil {
		t.Fatal(err)
	}
	h := &runtimeHarness{t: t, store: store, manager: manager, drivers: make(map[provider.ID]*fakeDriver), logs: &syncBuffer{}}
	var drivers []provider.Driver
	for _, id := range []provider.ID{"alpha", "beta"} {
		driver := &fakeDriver{id: id, userID: string(id) + "-user", lifetime: 20 * time.Hour}
		h.drivers[id] = driver
		drivers = append(drivers, driver)
	}
	providers, err := provider.NewRegistry(drivers, nil)
	if err != nil {
		t.Fatal(err)
	}
	var runtime *Runtime
	controller, err := coordinator.New(coordinator.Config{
		Providers: providers, Manager: manager, SelectorBuilder: selectors,
		CommitSnapshot: func(snapshot *state.RuntimeSnapshot) error { return runtime.CommitControlSnapshotLocked(snapshot) },
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err = NewRuntime(RuntimeConfig{
		Manager: manager, Store: store, Providers: controller, Subscriptions: subscriptions, Selectors: selectors,
		MutationMu: mutations, SocksAddress: "127.0.0.1:10808", HTTPAddress: "127.0.0.1:10809", Version: "test",
		RefreshEvery: time.Hour, Logger: logging.New(h.logs),
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			client, server := net.Pipe()
			_ = server.Close()
			return client, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.runtime = runtime
	t.Cleanup(runtime.Stop)
	return h
}

func (h *runtimeHarness) setup() {
	h.t.Helper()
	if err := h.runtime.AccessSetup(context.Background(), testAccessToken); err != nil {
		h.t.Fatal(err)
	}
}

func (h *runtimeHarness) login(id provider.ID) {
	h.t.Helper()
	if _, err := h.runtime.Login(context.Background(), web.LoginInput{Provider: string(id), Account: "a@example.com", Password: "secret"}); err != nil {
		h.t.Fatalf("login %s: %v", id, err)
	}
}

func (h *runtimeHarness) credentials(nodeID string) (string, string) {
	h.t.Helper()
	details, err := h.runtime.NodeDetails(context.Background(), nodeID)
	if err != nil {
		h.t.Fatalf("node details %s: %v", nodeID, err)
	}
	return details.SocksUsername, details.SocksPassword
}

func TestRuntimeAccessLifecycle(t *testing.T) {
	h := newRuntimeHarness(t)
	status, err := h.runtime.AccessStatus(context.Background())
	if err != nil || status.Initialized {
		t.Fatalf("fresh access status = %#v, %v", status, err)
	}
	h.setup()
	if status, err := h.runtime.AccessStatus(context.Background()); err != nil || !status.Initialized {
		t.Fatalf("access status after setup = %#v, %v", status, err)
	}
	if err := h.runtime.AccessSetup(context.Background(), testAccessToken); err == nil {
		t.Fatal("second setup succeeded")
	}
	if err := h.runtime.AccessLogin(context.Background(), testAccessToken); err != nil {
		t.Fatalf("login with the right token: %v", err)
	}
	if err := h.runtime.AccessLogin(context.Background(), "wrong horse battery token"); err == nil {
		t.Fatal("login with a wrong token succeeded")
	}
}

func TestRuntimeLoginPublishesNodesAndStatus(t *testing.T) {
	h := newRuntimeHarness(t)
	h.setup()
	h.login("alpha")
	status, err := h.runtime.Status(context.Background())
	if err != nil || status.State != string(state.StateReady) || len(status.Accounts) != 1 {
		t.Fatalf("status = %#v, %v", status, err)
	}
	nodes, err := h.runtime.Nodes(context.Background())
	if err != nil || len(nodes) != 1 || nodes[0].ID != "alpha_node" {
		t.Fatalf("nodes = %#v, %v", nodes, err)
	}
	username, password := h.credentials("alpha_node")
	if username == "" || password == "" {
		t.Fatal("node details carry no SOCKS credentials")
	}
	if _, err := h.runtime.Login(context.Background(), web.LoginInput{Provider: "alpha", Account: "a@example.com", Password: "secret"}); err == nil {
		t.Fatal("a second login for the same provider succeeded")
	}
}

func TestRuntimeKeepsCredentialsAcrossProviderSetChanges(t *testing.T) {
	h := newRuntimeHarness(t)
	h.setup()
	h.login("alpha")
	username, password := h.credentials("alpha_node")
	h.login("beta")
	if u, p := h.credentials("alpha_node"); u != username || p != password {
		t.Fatal("logging in a second provider rotated the first provider's credentials")
	}
	if err := h.runtime.Logout(context.Background(), "beta"); err != nil {
		t.Fatal(err)
	}
	if u, p := h.credentials("alpha_node"); u != username || p != password {
		t.Fatal("logging out a provider rotated the remaining credentials")
	}
	h.drivers["beta"].userID = "another-user"
	h.login("beta")
	if u, p := h.credentials("alpha_node"); u == username && p == password {
		t.Fatal("switching a provider account kept the old credentials")
	}
	if !strings.Contains(h.logs.String(), "subscription credentials rotated") {
		t.Fatalf("credential rotation was not logged: %s", h.logs.String())
	}
}

func TestRuntimeRefreshReportsPartialFailure(t *testing.T) {
	h := newRuntimeHarness(t)
	h.setup()
	h.login("alpha")
	h.login("beta")
	if err := h.runtime.Refresh(context.Background(), ""); err != nil {
		t.Fatalf("healthy refresh: %v", err)
	}
	if !strings.Contains(h.logs.String(), "provider refresh succeeded") {
		t.Fatalf("successful refresh was not logged: %s", h.logs.String())
	}
	h.drivers["beta"].refreshErr = errors.New("token revoked")
	err := h.runtime.Refresh(context.Background(), "")
	if err == nil {
		t.Fatal("refresh with a failing provider succeeded")
	}
	if got := h.manager.State(); got != state.StateDegraded {
		t.Fatalf("state after partial refresh = %s", got)
	}
	if !strings.Contains(h.logs.String(), "provider refresh failed") || strings.Contains(h.logs.String(), "secret") {
		t.Fatalf("unexpected refresh log: %s", h.logs.String())
	}
	if _, err := h.runtime.NodeDetails(context.Background(), "alpha_node"); err != nil {
		t.Fatalf("healthy provider unusable after a partial failure: %v", err)
	}
}

func TestRuntimeProbeAndDiagnostics(t *testing.T) {
	h := newRuntimeHarness(t)
	h.setup()
	h.login("alpha")
	result, err := h.runtime.Probe(context.Background(), "alpha_node")
	if err != nil || result.Health != string(state.NodeHealthHealthy) || result.ProbedAt.IsZero() {
		t.Fatalf("probe = %#v, %v", result, err)
	}
	if _, err := h.runtime.Probe(context.Background(), "missing"); err == nil {
		t.Fatal("probe of an unknown node succeeded")
	}
	diagnostics, err := h.runtime.Diagnostics(context.Background())
	if err != nil || diagnostics == nil {
		t.Fatalf("diagnostics = %#v, %v", diagnostics, err)
	}
}

func TestRuntimeStopRejectsFurtherWork(t *testing.T) {
	h := newRuntimeHarness(t)
	h.setup()
	h.runtime.Stop()
	if h.runtime.Healthy() {
		t.Fatal("stopped runtime reports healthy")
	}
	if _, err := h.runtime.Status(context.Background()); err == nil {
		t.Fatal("status succeeded after Stop")
	}
	if err := h.runtime.Refresh(context.Background(), ""); err == nil {
		t.Fatal("refresh succeeded after Stop")
	}
}
