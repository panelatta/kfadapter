package socks

import (
	"sync"
	"testing"
	"time"

	"github.com/kfadapter/kfadapter/internal/state"
)

type automaticRoute struct {
	mu      sync.Mutex
	enabled bool
	route   string
}

func (a *automaticRoute) Enabled() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.enabled }
func (a *automaticRoute) Resolve() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.enabled {
		return "", state.ErrSelectorUnknown
	}
	return a.route, nil
}
func (a *automaticRoute) set(enabled bool, route string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.enabled, a.route = enabled, route
}

func TestSmartRoutePinsConnectionAndDisableRevokesIt(t *testing.T) {
	ts := newTestServer(t, 8)
	route := &automaticRoute{enabled: true, route: ts.credential.Selector}
	ts.server.smart = route
	ts.server.revalidateEvery = 10 * time.Millisecond
	credentials := ts.server.selectors.Load().SmartCredentials()
	client, done := ts.handle(t)
	if authenticate(t, client, credentials.Selector, credentials.Password) != 0 {
		t.Fatal("smart auth rejected")
	}
	// The route can change between AUTH and CONNECT without moving this flow.
	route.set(true, "another-route")
	mustWrite(t, client, request(commandConnect, addressIPv4, []byte{1, 1, 1, 1}, 443))
	if code, _ := readReply(t, client); code != replySucceeded {
		t.Fatal("pinned CONNECT failed", code)
	}
	mustWrite(t, client, []byte("hello"))
	if got := string(mustRead(t, client, 5)); got != "hello" {
		t.Fatal(got)
	}
	other, _ := ts.handle(t)
	if authenticate(t, other, credentials.Selector, credentials.Password) == 0 {
		t.Fatal("new flow reused old route")
	}
	// A new winner must not make watchFlow drop the already established stream.
	if !ts.server.flowAuthorized(credentials.Selector, credentials.Password, ts.credential.Selector, true, ts.snapshots.node) {
		t.Fatal("route switch revoked established flow")
	}
	route.set(false, ts.credential.Selector)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("disable did not revoke smart stream")
	}
	regular, _ := ts.handle(t)
	if authenticate(t, regular, ts.credential.Selector, ts.credential.Password) != 0 {
		t.Fatal("disable revoked regular per-node credentials")
	}
	disabled, _ := ts.handle(t)
	if authenticate(t, disabled, credentials.Selector, credentials.Password) == 0 {
		t.Fatal("disabled smart credential accepted")
	}
}

func TestSmartRouteStillRequiresMatchingPassword(t *testing.T) {
	ts := newTestServer(t, 8)
	ts.server.smart = &automaticRoute{enabled: true, route: ts.credential.Selector}
	credentials := ts.server.selectors.Load().SmartCredentials()
	client, _ := ts.handle(t)
	if authenticate(t, client, credentials.Selector, ts.credential.Password) == 0 {
		t.Fatal("per-node password accepted for smart selector")
	}
}
