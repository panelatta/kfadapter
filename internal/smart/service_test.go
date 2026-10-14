package smart

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oshinop/chinadapter/internal/credentials"
	"github.com/oshinop/chinadapter/internal/provider"
	"github.com/oshinop/chinadapter/internal/state"
	"github.com/oshinop/chinadapter/internal/store"
)

func harness(t *testing.T, probe func(context.Context, state.TunnelPin, Target) Measurement) (*Service, *store.SQLiteStore) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	persistence, err := store.NewSQLiteStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = persistence.Close(context.Background()) })
	if _, err := persistence.LoadOrCreate(context.Background()); err != nil {
		t.Fatal(err)
	}
	persistent, err := persistence.Update(context.Background(), func(p *state.PersistentState) error {
		return p.SetAccessToken("smart-proxy-test-token")
	})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := persistent.Identity()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(-time.Second).UTC()
	snapshot := &state.RuntimeSnapshot{Generation: 1, CreatedAt: now, ExpiresAt: now.Add(time.Hour), Providers: map[provider.ID]provider.Snapshot{}}
	ps := provider.Snapshot{Provider: "test", Account: provider.Account{UserID: "user", Display: "user", Tier: "standard"}, ExpiresAt: snapshot.ExpiresAt, RefreshState: []byte("refresh"), Authorities: map[string]provider.Authority{"default": {Protocol: "test-native", Data: []byte("secret-authority")}}}
	for _, id := range []string{"fast", "slow", "partial", "failed"} {
		pn := provider.Node{ID: id, AuthorityID: "default", Protocol: "test-native", Host: id + ".example", Port: 443, Name: id, Eligible: true}
		ps.Nodes = append(ps.Nodes, pn)
		snapshot.Nodes = append(snapshot.Nodes, state.Node{ID: id, Provider: "test", Protocol: pn.Protocol, AuthorityID: pn.AuthorityID, Host: pn.Host, Port: pn.Port, Name: id, Eligible: true})
	}
	snapshot.Providers["test"] = ps
	if err := assignSelectors(snapshot, identity); err != nil {
		t.Fatal(err)
	}
	if err := state.ValidateRuntimeSnapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	manager, err := state.NewManager(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	service, err := New(Config{Manager: manager, Store: persistence, Policy: persistent.Preferences.SmartProxy, Identity: func() *credentials.Identity { return identity }, MutationMu: &sync.Mutex{}, Probe: probe})
	if err != nil {
		t.Fatal(err)
	}
	return service, persistence
}

// assignSelectors mirrors the credential coordinator: node usernames derive
// from the installation identity and each node's route.
func assignSelectors(snapshot *state.RuntimeSnapshot, identity *credentials.Identity) error {
	selectors, err := state.BuildSelectors(snapshot.Nodes, identity)
	if err != nil {
		return err
	}
	snapshot.Selectors = selectors
	for index := range snapshot.Nodes {
		credentials, _ := identity.Credentials(snapshot.Nodes[index].Route())
		snapshot.Nodes[index].Selector = credentials.Username
	}
	return nil
}

func rebuildSelectors(t *testing.T, service *Service, snapshot *state.RuntimeSnapshot) {
	t.Helper()
	if err := assignSelectors(snapshot, service.identity()); err != nil {
		t.Fatal(err)
	}
}

func smartCredentials(t *testing.T, service *Service) credentials.Credentials {
	t.Helper()
	smart, ok := service.identity().SmartCredentials()
	if !ok {
		t.Fatal("identity has no smart credentials")
	}
	return smart
}

func rankedProbe(_ context.Context, pin state.TunnelPin, target Target) Measurement {
	if pin.Node.ID == "failed" || (pin.Node.ID == "partial" && target.Name == "QQ") {
		return Measurement{Error: "http_rejected"}
	}
	latency := int64(1)
	if pin.Node.ID == "fast" {
		latency = 30
	}
	if pin.Node.ID == "slow" {
		latency = 90
	}
	return Measurement{OK: true, LatencyMS: latency}
}
func TestRankingPersistenceAndRevocation(t *testing.T) {
	service, store := harness(t, rankedProbe)
	if _, err := service.Resolve(); err == nil {
		t.Fatal("disabled route accepted")
	}
	if err := service.Configure(context.Background(), state.SmartProxyPreferences{Enabled: true, IntervalMinutes: 60}); err != nil {
		t.Fatal(err)
	}
	service.roundIfDue(context.Background())
	status := service.Status()
	if status.SelectedNodeID != "fast" || len(status.Results) != 4 || status.Results[2].NodeID != "partial" {
		t.Fatalf("bad ranking: %+v", status)
	}
	if remaining := time.Until(status.NextRunAt); remaining < 59*time.Minute || remaining > time.Hour {
		t.Fatal(remaining)
	}
	current, err := service.Resolve()
	if err != nil || current != status.Results[0].node.Selector {
		t.Fatal("selected route mismatch", err)
	}
	encoded, _ := json.Marshal(status)
	for _, secret := range []string{"secret-authority", smartCredentials(t, service).Password, ".example", "selector"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatal("status contains secret or endpoint", secret)
		}
	}
	saved, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := New(Config{Manager: service.manager, Store: store, Policy: saved.Preferences.SmartProxy, Identity: service.identity, MutationMu: service.mutations, Probe: rankedProbe})
	if err != nil || !reloaded.Enabled() || reloaded.Status().IntervalMinutes != 60 {
		t.Fatal("policy did not survive reload", err)
	}
	if err := service.Configure(context.Background(), state.SmartProxyPreferences{Enabled: true, IntervalMinutes: 30}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Resolve(); err != nil {
		t.Fatal("changing cadence dropped current route")
	}
	service.manager.SignOut()
	if _, err := service.Resolve(); err == nil {
		t.Fatal("signed-out route accepted")
	}
	if service.Status().SelectedNodeID != "" {
		t.Fatal("stale winner displayed")
	}
	if err := service.Configure(context.Background(), state.SmartProxyPreferences{Enabled: false, IntervalMinutes: 30}); err != nil {
		t.Fatal(err)
	}
	if service.RequestProbe() {
		t.Fatal("disabled probe accepted")
	}
	if err := service.Configure(context.Background(), state.SmartProxyPreferences{Enabled: true, IntervalMinutes: 1}); err == nil {
		t.Fatal("invalid cadence accepted")
	}
	saved, err = store.Load(context.Background())
	if err != nil || saved.Preferences.SmartProxy.Enabled {
		t.Fatal("disabled preference not persisted", err)
	}
}
func TestSchedulerStartsImmediatelyCancelsAndBoundsConcurrency(t *testing.T) {
	var active, peak atomic.Int32
	started := make(chan struct{}, 4)
	service, _ := harness(t, func(ctx context.Context, _ state.TunnelPin, _ Target) Measurement {
		count := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); count > old && !peak.CompareAndSwap(old, count); old = peak.Load() {
		}
		started <- struct{}{}
		<-ctx.Done()
		return Measurement{Error: "timeout"}
	})
	if err := service.Configure(context.Background(), state.SmartProxyPreferences{Enabled: true, IntervalMinutes: 30}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- service.Run(ctx) }()
	for range 4 {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("initial round not started")
		}
	}
	if service.RequestProbe() {
		t.Fatal("overlapping manual round accepted")
	}
	if err := service.Configure(context.Background(), state.SmartProxyPreferences{Enabled: false, IntervalMinutes: 30}); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not drain workers")
	}
	if active.Load() != 0 || peak.Load() > 4 || len(service.Status().Results) != 0 {
		t.Fatal("workers leaked or stale results published")
	}
}
func TestNoSuccessfulRouteFailsClosedAndRetries(t *testing.T) {
	service, _ := harness(t, func(context.Context, state.TunnelPin, Target) Measurement { return Measurement{Error: "timeout"} })
	if err := service.Configure(context.Background(), state.SmartProxyPreferences{Enabled: true, IntervalMinutes: 30}); err != nil {
		t.Fatal(err)
	}
	service.roundIfDue(context.Background())
	if _, err := service.Resolve(); err == nil {
		t.Fatal("unmeasured or failed route selected")
	}
	if next := time.Until(service.Status().NextRunAt); next > time.Minute || next < 50*time.Second {
		t.Fatal("missing bounded retry")
	}
	if !service.RequestProbe() {
		t.Fatal("manual retry unavailable")
	}
}

func TestScheduledCadenceAndRemovedWinnerFallback(t *testing.T) {
	var calls atomic.Int32
	service, _ := harness(t, func(ctx context.Context, pin state.TunnelPin, target Target) Measurement {
		calls.Add(1)
		return rankedProbe(ctx, pin, target)
	})
	if err := service.Configure(context.Background(), state.SmartProxyPreferences{Enabled: true, IntervalMinutes: 30}); err != nil {
		t.Fatal(err)
	}
	service.roundIfDue(context.Background())
	if calls.Load() != 16 {
		t.Fatal("did not probe every node and target")
	}
	service.roundIfDue(context.Background())
	if calls.Load() != 16 {
		t.Fatal("ran before interval elapsed")
	}
	service.mu.Lock()
	service.next = time.Now().Add(-time.Second)
	service.mu.Unlock()
	service.roundIfDue(context.Background())
	if calls.Load() != 32 {
		t.Fatal("scheduled round not run")
	}
	snapshot := service.manager.Current()
	snapshot.Generation++
	node := snapshot.Nodes[0]
	snapshot.Nodes = snapshot.Nodes[1:]
	delete(snapshot.Selectors, node.Selector)
	ps := snapshot.Providers["test"]
	ps.Nodes = ps.Nodes[1:]
	snapshot.Providers["test"] = ps
	if err := service.manager.Commit(snapshot); err != nil {
		t.Fatal(err)
	}
	selected, err := service.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	pin, err := service.manager.CompactPin(selected, time.Now())
	if err != nil || pin.Node.ID != "slow" {
		t.Fatal("removed winner did not fall back to measured runner-up", err)
	}
}

func TestTimedOutRoundDisclosesPartialCoverageAndRotates(t *testing.T) {
	service, _ := harness(t, func(ctx context.Context, _ state.TunnelPin, _ Target) Measurement {
		<-ctx.Done()
		return Measurement{Error: "timeout"}
	})
	service.roundTimeout = 20 * time.Millisecond
	if err := service.Configure(context.Background(), state.SmartProxyPreferences{Enabled: true, IntervalMinutes: 30}); err != nil {
		t.Fatal(err)
	}
	service.roundIfDue(context.Background())
	status := service.Status()
	if !status.Incomplete || status.CandidateCount != 4 || len(status.Results) != 0 || service.cursor == 0 {
		t.Fatalf("incomplete coverage not recorded: %+v", status)
	}
}
