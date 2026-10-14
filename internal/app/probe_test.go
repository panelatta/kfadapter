package app

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/oshinop/chinadapter/internal/state"
)

func TestNewestProbeForSameNodeWins(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		entered, release := make(chan struct{}), make(chan struct{})
		fixture := newRuntimeFixture(t, func(ctx context.Context, network, address string) (net.Conn, error) {
			if calls.Add(1) > 1 {
				return pipeDial(ctx, network, address)
			}
			close(entered)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-release:
				return nil, errors.New("older probe failed late")
			}
		})
		older := make(chan error, 1)
		go func() { _, err := fixture.runtime.Probe(t.Context(), "alpha-node"); older <- err }()
		synctest.Wait()
		select {
		case <-entered:
		default:
			t.Fatal("older probe did not start")
		}
		if _, err := fixture.runtime.Probe(t.Context(), "alpha-node"); err != nil {
			t.Fatalf("newer probe: %v", err)
		}
		close(release)
		var problem *problemError
		if err := <-older; !errors.As(err, &problem) || problem.code != "stale_probe" {
			t.Fatalf("older probe = %v, want stale_probe", err)
		}
		node, found := fixture.manager.Current().NodeByID("alpha-node")
		if !found || node.Health != state.NodeHealthHealthy {
			t.Fatalf("older probe overwrote the newer result: %+v", node)
		}
	})
}
