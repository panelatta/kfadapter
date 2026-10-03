package app

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kfadapter/kfadapter/internal/state"
)

func waitProbeResult(t *testing.T, results <-chan error) error {
	t.Helper()
	select {
	case err := <-results:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("probe did not finish")
		return nil
	}
}

func waitProbeDial(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("probe did not dial")
	}
}

func beginBlockedProbe(t *testing.T, h *runtimeHarness) (chan struct{}, <-chan error) {
	t.Helper()
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	h.runtime.dial = func(ctx context.Context, _, _ string) (net.Conn, error) {
		entered <- struct{}{}
		select {
		case <-release:
			client, server := net.Pipe()
			_ = server.Close()
			return client, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	results := make(chan error, 1)
	go func() {
		_, err := h.runtime.Probe(context.Background(), "alpha_node")
		results <- err
	}()
	waitProbeDial(t, entered)
	return release, results
}

func TestRuntimeParallelProbesForIndependentNodes(t *testing.T) {
	h := newRuntimeHarness(t)
	h.setup()
	h.login("alpha")
	h.login("beta")
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	h.runtime.dial = func(ctx context.Context, _, _ string) (net.Conn, error) {
		entered <- struct{}{}
		select {
		case <-release:
			client, server := net.Pipe()
			_ = server.Close()
			return client, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	results := make(chan error, 2)
	for _, id := range []string{"alpha_node", "beta_node"} {
		go func() {
			_, err := h.runtime.Probe(context.Background(), id)
			results <- err
		}()
	}
	waitProbeDial(t, entered)
	waitProbeDial(t, entered)
	close(release)
	for range 2 {
		if err := waitProbeResult(t, results); err != nil {
			t.Fatalf("independent successful TCP probe was rejected: %v", err)
		}
	}
	for _, node := range h.manager.Current().Nodes {
		if node.Health != state.NodeHealthHealthy || node.ProbedAt.IsZero() {
			t.Errorf("node %s lost its successful observation", node.ID)
		}
	}
	if len(h.runtime.probes) != 0 {
		t.Fatal("completed probes retained request records")
	}
}

func TestRuntimeNewestProbeForSameNodeWins(t *testing.T) {
	for _, newestFirst := range []bool{false, true} {
		name := "old_finishes_first"
		if newestFirst {
			name = "new_finishes_first"
		}
		t.Run(name, func(t *testing.T) {
			h := newRuntimeHarness(t)
			h.setup()
			h.login("alpha")
			entered := make(chan struct{}, 2)
			releases := []chan struct{}{make(chan struct{}), make(chan struct{})}
			var sequence atomic.Int32
			h.runtime.dial = func(ctx context.Context, _, _ string) (net.Conn, error) {
				index := int(sequence.Add(1)) - 1
				entered <- struct{}{}
				select {
				case <-releases[index]:
					if index == 0 {
						return nil, errors.New("old unsuccessful observation")
					}
					client, server := net.Pipe()
					_ = server.Close()
					return client, nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			results := []chan error{make(chan error, 1), make(chan error, 1)}
			for i := range 2 {
				go func() {
					_, err := h.runtime.Probe(context.Background(), "alpha_node")
					results[i] <- err
				}()
				waitProbeDial(t, entered)
			}
			order := []int{0, 1}
			if newestFirst {
				order = []int{1, 0}
			}
			for _, index := range order {
				close(releases[index])
				err := waitProbeResult(t, results[index])
				if index == 0 && !errors.Is(err, ErrStaleProbe) {
					t.Fatalf("old probe returned %v, want stale_probe", err)
				}
				if index == 1 && err != nil {
					t.Fatalf("latest probe returned %v", err)
				}
			}
			node, _ := h.manager.Current().NodeByID("alpha_node")
			if node.Health != state.NodeHealthHealthy {
				t.Fatalf("old result overwrote latest health: %s", node.Health)
			}
			if len(h.runtime.probes) != 0 {
				t.Fatal("completed probes retained request records")
			}
		})
	}
}

func TestRuntimeProbeRejectsChangedTarget(t *testing.T) {
	for _, change := range []string{"route", "selector", "authority", "expiry", "ineligible", "logout", "credential_rotation", "provider_expired"} {
		t.Run(change, func(t *testing.T) {
			h := newRuntimeHarness(t)
			h.setup()
			h.login("alpha")
			h.login("beta")
			release, results := beginBlockedProbe(t, h)
			switch change {
			case "logout":
				if err := h.runtime.Logout(context.Background(), "alpha"); err != nil {
					t.Fatal(err)
				}
			case "credential_rotation":
				if err := h.runtime.Logout(context.Background(), "beta"); err != nil {
					t.Fatal(err)
				}
				h.drivers["beta"].userID = "replacement-account"
				h.login("beta")
			case "provider_expired":
				now := h.manager.Current().Providers["alpha"].ExpiresAt
				h.runtime.now = func() time.Time { return now }
			default:
				h.runtime.mutations.Lock()
				next := h.manager.Current()
				snapshot := next.Providers["alpha"]
				for i := range next.Nodes {
					if next.Nodes[i].ID != "alpha_node" {
						continue
					}
					switch change {
					case "route":
						next.Nodes[i].Host = "192.0.2.2"
						snapshot.Nodes[0].Host = "192.0.2.2"
					case "selector":
						delete(next.Selectors, next.Nodes[i].Selector)
						next.Nodes[i].Selector = "changed-selector"
						next.Selectors["changed-selector"] = state.NodeRef{NodeID: "alpha_node"}
					case "authority":
						authority := snapshot.Authorities["default"]
						authority.Data = []byte("rotated-authority")
						snapshot.Authorities["default"] = authority
					case "expiry":
						snapshot.ExpiresAt = snapshot.ExpiresAt.Add(-time.Minute)
					case "ineligible":
						next.Nodes[i].Eligible = false
						snapshot.Nodes[0].Eligible = false
					}
				}
				next.Providers["alpha"] = snapshot
				next.ExpiresAt = state.ProviderExpiresAt(next.Providers)
				next.Generation++
				err := h.manager.Commit(next)
				h.runtime.mutations.Unlock()
				if err != nil {
					t.Fatal(err)
				}
			}
			close(release)
			if err := waitProbeResult(t, results); !errors.Is(err, ErrStaleProbe) {
				t.Fatalf("changed %s accepted the old probe: %v", change, err)
			}
			if node, exists := h.manager.Current().NodeByID("alpha_node"); exists && !node.ProbedAt.IsZero() {
				t.Fatal("stale observation was applied")
			}
		})
	}
}

func TestRuntimeProbeSurvivesUnrelatedProviderChanges(t *testing.T) {
	for _, change := range []string{"refresh", "logout"} {
		t.Run(change, func(t *testing.T) {
			h := newRuntimeHarness(t)
			h.setup()
			h.login("alpha")
			h.login("beta")
			release, results := beginBlockedProbe(t, h)
			var err error
			if change == "refresh" {
				err = h.runtime.Refresh(context.Background(), "beta")
			} else {
				err = h.runtime.Logout(context.Background(), "beta")
			}
			if err != nil {
				t.Fatal(err)
			}
			close(release)
			if err := waitProbeResult(t, results); err != nil {
				t.Fatalf("unrelated provider %s invalidated alpha's probe: %v", change, err)
			}
		})
	}
}
