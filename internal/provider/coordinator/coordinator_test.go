package coordinator

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/kfadapter/kfadapter/internal/provider"
	"github.com/kfadapter/kfadapter/internal/state"
)

const testProtocol provider.Protocol = "test-native"

type fakeDriver struct {
	id provider.ID

	mu         sync.Mutex
	userID     string
	refreshErr error
	block      bool
	refreshes  int
	lifetime   time.Duration
	now        func() time.Time
}

func (driver *fakeDriver) ID() provider.ID { return driver.id }

func (driver *fakeDriver) snapshot() provider.Snapshot {
	return provider.Snapshot{
		Provider:     driver.id,
		Account:      provider.Account{UserID: driver.userID, Display: "a•••@example.com", Tier: "standard"},
		ExpiresAt:    driver.now().Add(driver.lifetime),
		RefreshState: []byte("refresh"),
		Authorities:  map[string]provider.Authority{"default": {Protocol: testProtocol, Data: []byte("authority")}},
		Nodes: []provider.Node{{
			ID: string(driver.id) + "_node", AuthorityID: "default", Protocol: testProtocol,
			Host: "192.0.2.1", Port: 443, Name: "node", Eligible: true,
		}},
	}
}

func (driver *fakeDriver) Login(context.Context, provider.Credentials) (provider.Snapshot, error) {
	driver.mu.Lock()
	defer driver.mu.Unlock()
	return driver.snapshot(), nil
}

func (driver *fakeDriver) Refresh(ctx context.Context, _ provider.Snapshot) (provider.Snapshot, error) {
	driver.mu.Lock()
	driver.refreshes++
	block, err := driver.block, driver.refreshErr
	driver.mu.Unlock()
	if block {
		<-ctx.Done()
		return provider.Snapshot{}, ctx.Err()
	}
	if err != nil {
		return provider.Snapshot{}, err
	}
	driver.mu.Lock()
	defer driver.mu.Unlock()
	return driver.snapshot(), nil
}

type selectorBuilder struct{}

func (selectorBuilder) Build(nodes []state.Node) (map[string]state.NodeRef, error) {
	refs := make(map[string]state.NodeRef, len(nodes))
	for _, node := range nodes {
		refs["s_"+node.ID] = state.NodeRef{NodeID: node.ID}
	}
	return refs, nil
}

type harness struct {
	t           *testing.T
	clock       time.Time
	persistent  state.PersistentState
	manager     *state.Manager
	coordinator *Coordinator
	drivers     map[provider.ID]*fakeDriver
	rotations   int
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, clock: time.Now().UTC(), drivers: make(map[provider.ID]*fakeDriver)}
	persistent, err := state.NewPersistentState()
	if err != nil {
		t.Fatal(err)
	}
	if err := persistent.SetAccessToken("correct horse battery token"); err != nil {
		t.Fatal(err)
	}
	h.persistent = persistent
	manager, err := state.NewManagerWithSubscription(nil, persistent.Subscription, persistent.AccessTokenVerifier.BindingKey())
	if err != nil {
		t.Fatal(err)
	}
	h.manager = manager
	var drivers []provider.Driver
	for _, id := range []provider.ID{"alpha", "beta"} {
		driver := &fakeDriver{id: id, userID: string(id) + "-user", lifetime: 20 * time.Hour, now: h.now}
		h.drivers[id] = driver
		drivers = append(drivers, driver)
	}
	registry, err := provider.NewRegistry(drivers, nil)
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := New(Config{Providers: registry, Manager: manager, SelectorBuilder: selectorBuilder{}, CommitSnapshot: h.commit, Clock: h.now})
	if err != nil {
		t.Fatal(err)
	}
	h.coordinator = coordinator
	return h
}

func (h *harness) now() time.Time { return h.clock }

// commit mirrors app.Runtime.CommitControlSnapshotLocked without persistence.
func (h *harness) commit(snapshot *state.RuntimeSnapshot) error {
	if snapshot == nil {
		return nil
	}
	bindingID := snapshot.AccountBindingID()
	candidate := h.persistent.Clone()
	rotated, err := state.EnsureSubscriptionAccountBinding(&candidate, bindingID, h.now())
	if err != nil {
		return err
	}
	rollback, err := h.manager.InstallEpoch(candidate.Subscription, bindingID)
	if err != nil {
		return err
	}
	if err := h.manager.Commit(snapshot); err != nil {
		rollback()
		return err
	}
	if rotated {
		h.rotations++
	}
	h.persistent = candidate
	return nil
}

func (h *harness) login(id provider.ID) {
	h.t.Helper()
	credentials := provider.Credentials{Account: "a@example.com", Password: "secret", InstallationID: "installation"}
	if _, err := h.coordinator.Login(context.Background(), id, credentials); err != nil {
		h.t.Fatalf("login %s: %v", id, err)
	}
}

func (h *harness) providerExpiry(id provider.ID) time.Time {
	h.t.Helper()
	current := h.manager.Current()
	if current == nil {
		h.t.Fatalf("no snapshot")
	}
	snapshot, ok := current.Providers[id]
	if !ok {
		h.t.Fatalf("provider %s missing", id)
	}
	return snapshot.ExpiresAt
}

func TestRefreshCommitsHealthyProvidersWhenAnotherFails(t *testing.T) {
	h := newHarness(t)
	h.login("alpha")
	h.login("beta")
	alphaBefore := h.providerExpiry("alpha")
	betaBefore := h.providerExpiry("beta")
	generation := h.manager.Current().Generation

	// Manager lifecycle transitions use the wall clock, so extend the refreshed
	// lifetime instead of advancing the harness clock.
	h.drivers["alpha"].lifetime = 23 * time.Hour
	revoked := errors.New("token revoked")
	h.drivers["beta"].refreshErr = revoked
	err := h.coordinator.Refresh(context.Background(), "")
	if !errors.Is(err, revoked) {
		t.Fatalf("refresh error = %v, want revoked", err)
	}
	var refreshErr *RefreshError
	if !errors.As(err, &refreshErr) || refreshErr.Provider != "beta" {
		t.Fatalf("refresh error does not name the failed provider: %v", err)
	}
	if !h.providerExpiry("alpha").After(alphaBefore) {
		t.Fatal("healthy provider refresh was discarded")
	}
	if !h.providerExpiry("beta").Equal(betaBefore) {
		t.Fatal("failed provider snapshot changed")
	}
	if h.manager.Current().Generation <= generation {
		t.Fatal("partial refresh did not publish a new generation")
	}
	if got := h.manager.State(); got != state.StateDegraded {
		t.Fatalf("state after partial refresh = %s, want degraded", got)
	}
}

func TestRefreshFailureDoesNotCancelOtherProviders(t *testing.T) {
	h := newHarness(t)
	h.login("alpha")
	h.login("beta")
	h.drivers["alpha"].refreshErr = errors.New("fails fast")
	// beta blocks until its context is cancelled; a cancelling coordinator would
	// turn its refresh into a failure.
	h.drivers["beta"].block = true
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := h.coordinator.Refresh(ctx, "")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("beta finished before the caller deadline: %v", err)
	}
}

func TestRefreshAllFailedKeepsSnapshot(t *testing.T) {
	h := newHarness(t)
	h.login("alpha")
	generation := h.manager.Current().Generation
	h.drivers["alpha"].refreshErr = errors.New("down")
	if err := h.coordinator.Refresh(context.Background(), ""); err == nil {
		t.Fatal("refresh succeeded")
	}
	if h.manager.Current().Generation != generation {
		t.Fatal("failed refresh published a generation")
	}
}

func TestRefreshSingleProvider(t *testing.T) {
	h := newHarness(t)
	h.login("alpha")
	h.login("beta")
	if err := h.coordinator.Refresh(context.Background(), "alpha"); err != nil {
		t.Fatal(err)
	}
	if h.drivers["alpha"].refreshes != 1 || h.drivers["beta"].refreshes != 0 {
		t.Fatalf("refresh counts alpha=%d beta=%d", h.drivers["alpha"].refreshes, h.drivers["beta"].refreshes)
	}
	if got := h.manager.State(); got != state.StateReady {
		t.Fatalf("state = %s", got)
	}
	if err := h.coordinator.Refresh(context.Background(), "gamma"); !errors.Is(err, provider.ErrNoSession) {
		t.Fatalf("unknown provider refresh = %v", err)
	}
}

func TestProviderSetChangesKeepCredentials(t *testing.T) {
	h := newHarness(t)
	h.login("alpha")
	h.login("beta")
	selectorKey := append([]byte(nil), h.persistent.Subscription.SelectorKey...)
	if err := h.coordinator.Logout(context.Background(), "beta"); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.manager.Current().Providers["beta"]; ok {
		t.Fatal("logout kept provider")
	}
	h.login("beta")
	if h.rotations != 0 || string(selectorKey) != string(h.persistent.Subscription.SelectorKey) {
		t.Fatal("provider set changes rotated credentials")
	}
	if err := h.coordinator.Logout(context.Background(), "beta"); err != nil {
		t.Fatal(err)
	}
	h.drivers["beta"].userID = "someone-else"
	h.login("beta")
	if h.rotations != 1 {
		t.Fatalf("account switch rotations = %d, want 1", h.rotations)
	}
}

func TestExpireRemovesOnlyExpiredProviders(t *testing.T) {
	h := newHarness(t)
	h.drivers["alpha"].lifetime = 2 * time.Hour
	h.login("alpha")
	h.login("beta")
	h.clock = h.clock.Add(3 * time.Hour)
	changed, err := h.coordinator.Expire(h.now())
	if err != nil || !changed {
		t.Fatalf("expire = %v, %v", changed, err)
	}
	current := h.manager.Current()
	if _, ok := current.Providers["alpha"]; ok {
		t.Fatal("expired provider retained")
	}
	if _, ok := current.Providers["beta"]; !ok {
		t.Fatal("usable provider removed")
	}
	h.clock = h.clock.Add(20 * time.Hour)
	changed, err = h.coordinator.Expire(h.now())
	if err != nil || !changed {
		t.Fatalf("final expire = %v, %v", changed, err)
	}
	if got := h.manager.State(); got != state.StateExpired {
		t.Fatalf("state = %s, want expired", got)
	}
}

func TestLogoutLastProviderSignsOut(t *testing.T) {
	h := newHarness(t)
	h.login("alpha")
	if err := h.coordinator.Logout(context.Background(), "alpha"); err != nil {
		t.Fatal(err)
	}
	if got := h.manager.State(); got != state.StateSignedOut {
		t.Fatalf("state = %s", got)
	}
	if err := h.coordinator.Logout(context.Background(), "alpha"); !errors.Is(err, provider.ErrNoSession) {
		t.Fatalf("second logout = %v", err)
	}
}
