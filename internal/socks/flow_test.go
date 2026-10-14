package socks

import (
	"bytes"
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/oshinop/chinadapter/internal/credentials"
	"github.com/oshinop/chinadapter/internal/provider"
	"github.com/oshinop/chinadapter/internal/state"
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

// holdingTransport reports success and then models an idle peer: the relay
// only ends when its context is canceled, even if the SOCKS socket closes.
func holdingTransport(canceled chan<- struct{}) *frontendStreamTransport {
	return &frontendStreamTransport{relay: func(ctx context.Context, request provider.RelayRequest) error {
		if !request.Admit() {
			return provider.ErrNoSession
		}
		if err := request.Ready(&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}); err != nil {
			return err
		}
		<-ctx.Done()
		canceled <- struct{}{}
		return ctx.Err()
	}}
}

func connectFrontend(t *testing.T, fixture *frontendFixture, username, password string) net.Conn {
	t.Helper()
	connection := fixture.connect(t)
	if status := authenticateFrontend(t, connection, username, password); status != 0 {
		t.Fatalf("authentication status = %x, want 00", status)
	}
	sendFrontendRequest(t, connection, commandConnect, target{Host: "1.1.1.1", Port: 443})
	readFrontendReply(t, connection, replySucceeded)
	return connection
}

func expectRelayCanceled(t *testing.T, canceled <-chan struct{}, connection net.Conn) {
	t.Helper()
	select {
	case <-canceled:
	case <-time.After(frontendTestTimeout):
		t.Fatal("revoked flow kept its transport")
	}
	expectFrontendClosed(t, connection)
}

func smartCredentials(t *testing.T, fixture *frontendFixture) credentials.Credentials {
	t.Helper()
	smart, ok := fixture.identity.SmartCredentials()
	if !ok {
		t.Fatal("fixture identity has no smart credentials")
	}
	return smart
}

func TestEstablishedFlowClosesWhenSessionIsRevoked(t *testing.T) {
	canceled := make(chan struct{}, 1)
	fixture := newConfiguredFrontendFixture(t, holdingTransport(canceled), 4, nil, func(server *Server) {
		server.revalidateEvery = 5 * time.Millisecond
	})
	connection := connectFrontend(t, fixture, fixture.credentials.Username, fixture.credentials.Password)
	// A provider refresh rotates authority bytes; established flows survive it.
	fixture.snapshots.replaceAuthority()
	select {
	case <-canceled:
		t.Fatal("authority refresh revoked an established flow")
	case <-time.After(50 * time.Millisecond):
	}
	fixture.snapshots.signOut()
	expectRelayCanceled(t, canceled, connection)
}

func TestEstablishedFlowClosesWhenCredentialsRotate(t *testing.T) {
	canceled := make(chan struct{}, 1)
	fixture := newConfiguredFrontendFixture(t, holdingTransport(canceled), 4, nil, func(server *Server) {
		server.revalidateEvery = 5 * time.Millisecond
	})
	connection := connectFrontend(t, fixture, fixture.credentials.Username, fixture.credentials.Password)
	rotated, err := credentials.New(bytes.Repeat([]byte{0x43}, 32), "00112233445566778899aabbccddeeff")
	if err != nil {
		t.Fatal(err)
	}
	node := fixture.snapshots.pin.Node
	if err := fixture.server.SetCredentials(rotated, &state.RuntimeSnapshot{Nodes: []state.Node{node}, Providers: map[provider.ID]provider.Snapshot{node.Provider: {}}}); err != nil {
		t.Fatal(err)
	}
	expectRelayCanceled(t, canceled, connection)
}

func TestSmartRoutePinsConnectionAndDisableRevokesIt(t *testing.T) {
	canceled := make(chan struct{}, 1)
	route := &automaticRoute{enabled: true}
	fixture := newConfiguredFrontendFixture(t, holdingTransport(canceled), 8, nil, func(server *Server) {
		server.smart = route
		server.revalidateEvery = 5 * time.Millisecond
	})
	route.set(true, fixture.credentials.Username)
	smart := smartCredentials(t, fixture)
	if smart.Username == fixture.credentials.Username {
		t.Fatal("smart and node usernames collide")
	}
	connection := connectFrontend(t, fixture, smart.Username, smart.Password)
	// A new winner must not move or revoke the established flow.
	route.set(true, "another-route")
	if !fixture.server.flowAuthorized(smart.Username, smart.Password, fixture.credentials.Username, true, fixture.snapshots.pin.Node) {
		t.Fatal("route switch revoked established flow")
	}
	route.set(false, fixture.credentials.Username)
	expectRelayCanceled(t, canceled, connection)
	if status := authenticateFrontend(t, fixture.connect(t), smart.Username, smart.Password); status == 0 {
		t.Fatal("disabled smart credential accepted")
	}
	if status := authenticateFrontend(t, fixture.connect(t), fixture.credentials.Username, fixture.credentials.Password); status != 0 {
		t.Fatal("disabling smart proxy revoked per-node credentials")
	}
}

func TestSmartRouteRequiresInstancePasswordAndEnabledService(t *testing.T) {
	route := &automaticRoute{enabled: true}
	fixture := newConfiguredFrontendFixture(t, &frontendStreamTransport{}, 8, nil, func(server *Server) { server.smart = route })
	route.set(true, fixture.credentials.Username)
	smart := smartCredentials(t, fixture)
	if status := authenticateFrontend(t, fixture.connect(t), smart.Username, "incorrect"); status == 0 {
		t.Fatal("smart username accepted a wrong password")
	}
	unconfigured := newFrontendFixture(t, &frontendStreamTransport{}, 8, nil)
	if status := authenticateFrontend(t, unconfigured.connect(t), smart.Username, smart.Password); status == 0 {
		t.Fatal("smart credential accepted without a smart service")
	}
}

func TestHandshakeAdmissionIsBoundedPerClientIP(t *testing.T) {
	fixture := newFrontendFixture(t, &frontendStreamTransport{}, maxHandshakesPerClient+8, nil)
	pending := make([]net.Conn, 0, maxHandshakesPerClient)
	for range maxHandshakesPerClient {
		connection := fixture.connect(t)
		// Complete method negotiation so the server has accepted the client.
		if err := writeAll(connection, []byte{version5, 1, methodUserPassword}); err != nil {
			t.Fatal(err)
		}
		var method [2]byte
		if _, err := connection.Read(method[:]); err != nil {
			t.Fatal(err)
		}
		pending = append(pending, connection)
	}
	expectFrontendClosed(t, fixture.connect(t))
	// Authentication releases the client's setup slot.
	auth := append([]byte{version1929, byte(len(fixture.credentials.Username))}, fixture.credentials.Username...)
	auth = append(auth, byte(len(fixture.credentials.Password)))
	auth = append(auth, fixture.credentials.Password...)
	if err := writeAll(pending[0], auth); err != nil {
		t.Fatal(err)
	}
	var status [2]byte
	if _, err := pending[0].Read(status[:]); err != nil || status[1] != 0 {
		t.Fatalf("authentication status = %x: %v", status, err)
	}
	fixture.authenticated(t)
}
