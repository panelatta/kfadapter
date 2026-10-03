package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kfadapter/kfadapter/internal/state"
)

func TestRuntimeRefreshScheduleAfterProviderRemoval(t *testing.T) {
	for _, removal := range []string{"expire", "logout"} {
		for _, exhaustRetryWindow := range []bool{false, true} {
			name := removal + "/scheduled"
			if exhaustRetryWindow {
				name = removal + "/exhausted_retry_window"
			}
			t.Run(name, func(t *testing.T) {
				h := newRuntimeHarness(t)
				h.setup()
				h.login("alpha")
				h.drivers["beta"].lifetime = 4 * time.Minute
				h.login("beta")
				lastRefresh := h.runtime.lastRefreshAt
				if exhaustRetryWindow {
					h.drivers["beta"].refreshErr = errors.New("provider unavailable")
					if err := h.runtime.Heartbeat(context.Background(), true); err == nil {
						t.Fatal("expected failed scheduled refresh")
					}
					if !h.runtime.nextRefreshAt.IsZero() {
						t.Fatal("expected no retry inside the attempt budget before expiry")
					}
				}
				current := h.manager.Current()
				wantNext := boundedRefreshAt(lastRefresh, current.Providers["alpha"].ExpiresAt, h.runtime.RefreshEvery())
				if removal == "expire" {
					now := current.Providers["beta"].ExpiresAt.Add(time.Second)
					h.runtime.now = func() time.Time { return now }
					if err := h.runtime.Heartbeat(context.Background(), false); err != nil {
						t.Fatal(err)
					}
				} else if err := h.runtime.Logout(context.Background(), "beta"); err != nil {
					t.Fatal(err)
				}
				current = h.manager.Current()
				if len(current.Providers) != 1 || current.Providers["alpha"].Provider != "alpha" {
					t.Fatalf("remaining providers = %v", current.Providers)
				}
				if !h.runtime.nextRefreshAt.Equal(wantNext) {
					t.Fatalf("next refresh = %v, want %v", h.runtime.nextRefreshAt, wantNext)
				}
				if !h.runtime.lastRefreshAt.Equal(lastRefresh) {
					t.Fatal("removing a provider incorrectly recorded a successful refresh")
				}
				// Verify the restored schedule actually refreshes the remaining
				// account when the periodic worker observes it as due.
				h.runtime.now = func() time.Time { return wantNext }
				if !h.runtime.RefreshDue(wantNext) {
					t.Fatal("remaining provider is never due for refresh")
				}
				if err := h.runtime.Heartbeat(context.Background(), h.runtime.RefreshDue(wantNext)); err != nil {
					t.Fatal(err)
				}
				if h.manager.Current().Generation <= current.Generation || !h.runtime.nextRefreshAt.After(wantNext) {
					t.Fatal("scheduled refresh did not commit and schedule its successor")
				}
			})
		}
	}
}

func TestRuntimeRefreshScheduleClearedAfterFinalProviderRemoval(t *testing.T) {
	for _, removal := range []string{"expire", "logout"} {
		t.Run(removal, func(t *testing.T) {
			h := newRuntimeHarness(t)
			h.setup()
			h.login("alpha")
			lastRefresh := h.runtime.lastRefreshAt
			if h.runtime.nextRefreshAt.IsZero() {
				t.Fatal("login did not schedule a refresh")
			}
			if removal == "expire" {
				now := h.manager.Current().Providers["alpha"].ExpiresAt.Add(time.Second)
				h.runtime.now = func() time.Time { return now }
				if err := h.runtime.Heartbeat(context.Background(), false); err != nil {
					t.Fatal(err)
				}
				if h.manager.State() != state.StateExpired {
					t.Fatalf("state after expiry = %v", h.manager.State())
				}
			} else if err := h.runtime.Logout(context.Background(), "alpha"); err != nil {
				t.Fatal(err)
			}
			if !h.runtime.nextRefreshAt.IsZero() || h.runtime.RefreshDue(h.runtime.now().Add(24*time.Hour)) {
				t.Fatal("refresh remains scheduled without a provider")
			}
			if !h.runtime.lastRefreshAt.Equal(lastRefresh) {
				t.Fatal("removal changed historical refresh time")
			}
			// A failed refresh may release the mutation lock just before this
			// removal. Its subsequent retry calculation must not revive work.
			h.runtime.scheduleRefreshRetry()
			if !h.runtime.nextRefreshAt.IsZero() {
				t.Fatal("late failed-refresh retry recreated a signed-out schedule")
			}
		})
	}
}

func TestRuntimeProviderRemovalKeepsOverdueRefreshDue(t *testing.T) {
	h := newRuntimeHarness(t)
	h.setup()
	h.login("alpha")
	h.drivers["beta"].lifetime = 4 * time.Minute
	h.login("beta")
	wantNext := h.runtime.lastRefreshAt.Add(h.runtime.RefreshEvery())
	now := wantNext.Add(time.Minute)
	h.runtime.now = func() time.Time { return now }
	if err := h.runtime.Heartbeat(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if !h.runtime.nextRefreshAt.Equal(wantNext) || !h.runtime.RefreshDue(now) {
		t.Fatalf("provider removal postponed an overdue refresh to %v", h.runtime.nextRefreshAt)
	}
}
