package smart

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kfadapter/kfadapter/internal/state"
)

func enableSmart(t *testing.T, service *Service) {
	t.Helper()
	if err := service.Configure(state.SmartProxyPreferences{Enabled: true, IntervalMinutes: 30}); err != nil {
		t.Fatal(err)
	}
}

func TestMeasuredResultsSurviveSameAccountAuthorityRefresh(t *testing.T) {
	for _, change := range []string{"authority", "expiry"} {
		t.Run(change, func(t *testing.T) {
			service, _ := harness(t, rankedProbe)
			enableSmart(t, service)
			service.roundIfDue(context.Background())
			previousSelector, err := service.Resolve()
			if err != nil {
				t.Fatal("initial measured route unavailable", err)
			}
			previousRun := service.Status().LastRunAt

			next := service.manager.Current()
			snapshot := next.Providers["test"]
			if change == "authority" {
				authority := snapshot.Authorities["default"]
				authority.Data = []byte("replacement-authority")
				snapshot.Authorities["default"] = authority
			} else {
				snapshot.ExpiresAt = snapshot.ExpiresAt.Add(time.Minute)
			}
			next.Providers["test"] = snapshot
			next.ExpiresAt = state.ProviderExpiresAt(next.Providers)
			next.Generation++
			if err := service.manager.Commit(next); err != nil {
				t.Fatal(err)
			}

			if selected, err := service.Resolve(); err != nil || selected != previousSelector {
				t.Fatalf("normal provider %s refresh discarded a measured current route: %v", change, err)
			}
			if got := service.Status(); len(got.Results) != 4 || got.SelectedNodeID != "fast" {
				t.Fatalf("normal provider %s refresh cleared completed rankings", change)
			}
			// Resolve returns a selector; SOCKS admission will use the current
			// authority. Refreshing credentials need not force an extra round.
			service.roundIfDue(context.Background())
			if !service.Status().LastRunAt.Equal(previousRun) {
				t.Fatal("normal provider refresh unnecessarily restarted probing")
			}
		})
	}
}

func TestRoundBudgetCancellationOnFinalTargetIsIncomplete(t *testing.T) {
	service, _ := harness(t, func(ctx context.Context, _ state.TunnelPin, target Target) Measurement {
		if target.Name == defaultTargets[len(defaultTargets)-1].Name {
			<-ctx.Done()
			return Measurement{Error: "timeout"}
		}
		return Measurement{OK: true, LatencyMS: 1}
	})
	service.roundTimeout = 30 * time.Millisecond
	enableSmart(t, service)
	service.roundIfDue(context.Background())
	status := service.Status()
	if !status.Incomplete || status.CandidateCount != 4 || len(status.Results) != 0 {
		t.Fatalf("round-budget cancellation was counted as a completed node measurement: %+v", status)
	}
	if _, err := service.Resolve(); err == nil {
		t.Fatal("selected an incompletely measured route")
	}
}

func TestNoAccountHasNoProbeCandidatesAndRecovers(t *testing.T) {
	service, _ := harness(t, rankedProbe)
	saved := service.manager.Current()
	service.manager.SignOut()
	enableSmart(t, service)
	service.roundIfDue(context.Background())
	status := service.Status()
	if status.CandidateCount != 0 || status.Incomplete || len(status.Results) != 0 {
		t.Fatalf("signed-out node metadata counted as active candidates: %+v", status)
	}
	if _, err := service.Resolve(); err == nil {
		t.Fatal("signed-out route resolved")
	}
	if remaining := time.Until(status.NextRunAt); remaining > time.Minute || remaining < 50*time.Second {
		t.Fatalf("no-account retry is not bounded to one minute: %s", remaining)
	}
	finish, err := service.manager.Begin(state.OperationLogin)
	if err != nil {
		t.Fatal(err)
	}
	saved.Generation++
	if err := service.manager.Commit(saved); err != nil {
		t.Fatal(err)
	}
	finish(state.OutcomeSucceeded)
	service.mu.Lock()
	service.next = time.Now().Add(-time.Second)
	service.mu.Unlock()
	service.roundIfDue(context.Background())
	if got := service.Status(); got.SelectedNodeID != "fast" || got.Incomplete {
		t.Fatalf("scheduler did not recover on its next retry: %+v", got)
	}
}

func TestIndividualTargetFailureStillCompletesNodeMeasurement(t *testing.T) {
	service, _ := harness(t, func(_ context.Context, _ state.TunnelPin, target Target) Measurement {
		if target.Name == defaultTargets[len(defaultTargets)-1].Name {
			return Measurement{Error: "timeout"}
		}
		return Measurement{OK: true, LatencyMS: 1}
	})
	enableSmart(t, service)
	service.roundIfDue(context.Background())
	status := service.Status()
	if status.Incomplete || len(status.Results) != 4 || status.SelectedNodeID == "" {
		t.Fatal("individual target failure was confused with cancelling the entire round")
	}
	for _, result := range status.Results {
		if result.Successes != 3 || len(result.Measurements) != 4 {
			t.Fatal("completed node did not retain its four target outcomes")
		}
	}
}

func TestCappedRoundsEventuallyProbeTheEntireInventory(t *testing.T) {
	var seenMu sync.Mutex
	seen := make(map[string]bool)
	var active, peak atomic.Int32
	service, _ := harness(t, func(ctx context.Context, pin state.TunnelPin, _ Target) Measurement {
		count := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); count > old && !peak.CompareAndSwap(old, count); old = peak.Load() {
		}
		seenMu.Lock()
		seen[pin.Node.ID] = true
		seenMu.Unlock()
		<-ctx.Done()
		return Measurement{Error: "timeout"}
	})
	next := service.manager.Current()
	snapshot := next.Providers["test"]
	for index := range 4 {
		id := fmt.Sprintf("extra-%d", index)
		node := snapshot.Nodes[0]
		node.ID, node.Name, node.Host = id, id, id+".example"
		snapshot.Nodes = append(snapshot.Nodes, node)
		copyNode := next.Nodes[0]
		copyNode.ID, copyNode.Name, copyNode.Host = id, id, node.Host
		next.Nodes = append(next.Nodes, copyNode)
	}
	next.Providers["test"] = snapshot
	built, err := service.registry().Build(next.Nodes)
	if err != nil {
		t.Fatal(err)
	}
	next.Nodes, next.Selectors = built.Nodes, built.Selectors
	next.Generation++
	if err := service.manager.Commit(next); err != nil {
		t.Fatal(err)
	}
	service.roundTimeout = 20 * time.Millisecond
	enableSmart(t, service)
	for range len(next.Nodes) {
		service.RequestProbe()
		service.roundIfDue(context.Background())
		if active.Load() != 0 {
			t.Fatal("capped round returned with probe workers still active")
		}
		status := service.Status()
		if !status.Incomplete || status.CandidateCount != len(next.Nodes) || len(status.Results) != 0 {
			t.Fatal("capped round reported invalid coverage")
		}
		seenMu.Lock()
		covered := len(seen)
		seenMu.Unlock()
		if covered == len(next.Nodes) {
			break
		}
	}
	seenMu.Lock()
	defer seenMu.Unlock()
	if len(seen) != len(next.Nodes) || peak.Load() > 4 {
		t.Fatalf("rotation starved candidates or exceeded worker limit: measured %d/%d, peak %d", len(seen), len(next.Nodes), peak.Load())
	}
}

func TestAuthorityRefreshDuringRoundRejectsInFlightMeasurements(t *testing.T) {
	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	service, _ := harness(t, func(ctx context.Context, _ state.TunnelPin, target Target) Measurement {
		if target.Name == defaultTargets[0].Name {
			entered <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
				return Measurement{Error: "timeout"}
			}
		}
		return Measurement{OK: true, LatencyMS: 1}
	})
	enableSmart(t, service)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		service.roundIfDue(ctx)
	}()
	for range 4 {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("initial probes did not start")
		}
	}
	next := service.manager.Current()
	snapshot := next.Providers["test"]
	authority := snapshot.Authorities["default"]
	authority.Data = []byte("replacement-authority")
	snapshot.Authorities["default"] = authority
	next.Providers["test"] = snapshot
	next.Generation++
	if err := service.manager.Commit(next); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("probing did not drain")
	}
	if status := service.Status(); len(status.Results) != 0 || !status.Incomplete || status.SelectedNodeID != "" {
		t.Fatal("round published measurements collected across an authority change")
	}
}
