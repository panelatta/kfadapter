package socks

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kfadapter/kfadapter/internal/provider"
	"github.com/kfadapter/kfadapter/internal/selector"
	"github.com/kfadapter/kfadapter/internal/state"
)

const testProtocol provider.Protocol = "test-native"

func TestCanonicalNodeComparisonRejectsReplacement(t *testing.T) {
	left := state.Node{ID: "quickfox_a", Provider: "quickfox", Host: "127.0.0.1", Port: 80, AuthorityID: "one"}
	if !sameCanonicalNode(left, left) {
		t.Fatal("identical node rejected")
	}
	replaced := left
	replaced.ID = "quickfox_b"
	if sameCanonicalNode(left, replaced) {
		t.Fatal("replacement node accepted")
	}
}

func TestPinAuthorityComparisonRejectsAuthorityChange(t *testing.T) {
	left := state.TunnelPin{Authority: provider.Authority{Data: []byte("old")}}
	right := state.TunnelPin{Authority: provider.Authority{Data: []byte("new")}}
	if samePinAuthority(left, right) {
		t.Fatal("different pin accepted")
	}
}

// fakeSnapshots resolves exactly one selector to one node.
type fakeSnapshots struct {
	mu       sync.Mutex
	selector string
	node     state.Node
	revoked  bool
}

func (f *fakeSnapshots) CompactPin(selectorName string, now time.Time) (state.TunnelPin, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.revoked || selectorName != f.selector {
		return state.TunnelPin{}, state.ErrSelectorUnknown
	}
	return state.TunnelPin{Authority: provider.Authority{Protocol: testProtocol, Data: []byte("authority")}, ExpiresAt: now.Truncate(time.Hour).Add(2 * time.Hour), Node: f.node}, nil
}

func (f *fakeSnapshots) SessionCurrentPin(state.TunnelPin, time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.revoked
}

// fakeTransport echoes stream bytes after recording the requested target.
type fakeTransport struct {
	mu       sync.Mutex
	targets  []provider.Target
	relayErr error
	hold     chan struct{}
}

func (*fakeTransport) Protocol() provider.Protocol { return testProtocol }

func (transport *fakeTransport) RelayStream(ctx context.Context, request provider.RelayRequest) error {
	transport.mu.Lock()
	transport.targets = append(transport.targets, request.Target)
	relayErr, hold := transport.relayErr, transport.hold
	transport.mu.Unlock()
	if relayErr != nil {
		return relayErr
	}
	if !request.Admit() {
		return provider.ErrNoSession
	}
	if err := request.Ready(&net.TCPAddr{IP: net.IPv4(192, 0, 2, 7), Port: 4321}); err != nil {
		return err
	}
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
		}
		return nil
	}
	_, err := io.Copy(request.Client, request.Client)
	return err
}

func (transport *fakeTransport) RelayDatagrams(ctx context.Context, request provider.DatagramRequest) error {
	if err := request.Ready(request.Packets.LocalAddr()); err != nil {
		return err
	}
	<-ctx.Done()
	return nil
}

func (transport *fakeTransport) lastTarget() provider.Target {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if len(transport.targets) == 0 {
		return provider.Target{}
	}
	return transport.targets[len(transport.targets)-1]
}

type testServer struct {
	server     *Server
	snapshots  *fakeSnapshots
	transport  *fakeTransport
	credential selector.Credentials
}

func newTestServer(t *testing.T, maxConnections int) *testServer {
	t.Helper()
	registry, err := selector.NewRegistry(state.SubscriptionAuthority{SelectorKey: bytes.Repeat([]byte{1}, 32), ProxyAuthKey: bytes.Repeat([]byte{2}, 32), ActivatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	node := state.Node{ID: "node-a", Provider: "test", Protocol: testProtocol, AuthorityID: "default", Host: "192.0.2.1", Port: 443, Eligible: true}
	credential, ok := registry.Credentials(selector.NodeIdentity{NodeID: node.ID})
	if !ok {
		t.Fatal("no credential")
	}
	node.Selector = credential.Selector
	transport := &fakeTransport{}
	providers, err := provider.NewRegistry(nil, []provider.Transport{transport})
	if err != nil {
		t.Fatal(err)
	}
	snapshots := &fakeSnapshots{selector: credential.Selector, node: node}
	server, err := New(Config{Snapshots: snapshots, Selectors: registry, Providers: providers, HandshakeTimeout: 2 * time.Second, MaxConnections: maxConnections})
	if err != nil {
		t.Fatal(err)
	}
	return &testServer{server: server, snapshots: snapshots, transport: transport, credential: credential}
}

// handle runs HandleConn over an in-memory pipe and returns the client side.
func (ts *testServer) handle(t *testing.T) (net.Conn, <-chan error) {
	t.Helper()
	client, server := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- ts.server.HandleConn(context.Background(), server) }()
	t.Cleanup(func() { _ = client.Close() })
	_ = client.SetDeadline(time.Now().Add(5 * time.Second))
	return client, done
}

func mustWrite(t *testing.T, conn net.Conn, data []byte) {
	t.Helper()
	if _, err := conn.Write(data); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func mustRead(t *testing.T, conn net.Conn, n int) []byte {
	t.Helper()
	buffer := make([]byte, n)
	if _, err := io.ReadFull(conn, buffer); err != nil {
		t.Fatalf("read %d bytes: %v", n, err)
	}
	return buffer
}

func authenticate(t *testing.T, conn net.Conn, username, password string) byte {
	t.Helper()
	mustWrite(t, conn, []byte{version5, 1, methodUserPassword})
	if reply := mustRead(t, conn, 2); !bytes.Equal(reply, []byte{version5, methodUserPassword}) {
		t.Fatalf("method reply = %v", reply)
	}
	message := []byte{version1929, byte(len(username))}
	message = append(message, username...)
	message = append(message, byte(len(password)))
	message = append(message, password...)
	mustWrite(t, conn, message)
	status := mustRead(t, conn, 2)
	if status[0] != version1929 {
		t.Fatalf("auth version = %d", status[0])
	}
	return status[1]
}

func request(command, atyp byte, address []byte, port uint16) []byte {
	message := []byte{version5, command, 0, atyp}
	message = append(message, address...)
	return binary.BigEndian.AppendUint16(message, port)
}

func readReply(t *testing.T, conn net.Conn) (byte, []byte) {
	t.Helper()
	header := mustRead(t, conn, 4)
	var addressLength int
	switch header[3] {
	case addressIPv4:
		addressLength = 4
	case addressIPv6:
		addressLength = 16
	case addressDomain:
		addressLength = int(mustRead(t, conn, 1)[0])
	default:
		t.Fatalf("reply address type %d", header[3])
	}
	bound := mustRead(t, conn, addressLength+2)
	return header[1], bound
}

func TestNegotiationRequiresUserPassword(t *testing.T) {
	ts := newTestServer(t, 0)
	conn, done := ts.handle(t)
	mustWrite(t, conn, []byte{version5, 1, methodNoAuthentication})
	if reply := mustRead(t, conn, 2); !bytes.Equal(reply, []byte{version5, methodNoAcceptable}) {
		t.Fatalf("reply = %v", reply)
	}
	if err := <-done; !errors.Is(err, errBadGreeting) {
		t.Fatalf("handler error = %v", err)
	}
}

func TestNegotiationRejectsWrongVersion(t *testing.T) {
	ts := newTestServer(t, 0)
	conn, done := ts.handle(t)
	// The server rejects after the 2-byte header, so the pipe write may fail.
	_, _ = conn.Write([]byte{0x04, 1, methodUserPassword})
	if err := <-done; !errors.Is(err, errBadGreeting) {
		t.Fatalf("handler error = %v", err)
	}
}

func TestAuthenticationRejectsWrongPassword(t *testing.T) {
	ts := newTestServer(t, 0)
	conn, done := ts.handle(t)
	if status := authenticate(t, conn, ts.credential.Selector, ts.credential.Password[:len(ts.credential.Password)-1]+"A"); status == 0 {
		t.Fatal("wrong password accepted")
	}
	if err := <-done; err == nil {
		t.Fatal("handler accepted wrong password")
	}
}

func TestAuthenticationRejectsRevokedSession(t *testing.T) {
	ts := newTestServer(t, 0)
	ts.snapshots.revoked = true
	conn, done := ts.handle(t)
	if status := authenticate(t, conn, ts.credential.Selector, ts.credential.Password); status == 0 {
		t.Fatal("revoked session accepted")
	}
	<-done
}

func TestConnectRelaysEveryAddressType(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		atyp    byte
		address []byte
		want    string
	}{
		{"ipv4", addressIPv4, []byte{198, 51, 100, 9}, "198.51.100.9"},
		{"ipv6", addressIPv6, net.ParseIP("2001:db8::1").To16(), "2001:db8::1"},
		{"domain", addressDomain, append([]byte{11}, "example.com"...), "example.com"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ts := newTestServer(t, 0)
			conn, done := ts.handle(t)
			if status := authenticate(t, conn, ts.credential.Selector, ts.credential.Password); status != 0 {
				t.Fatalf("auth status = %d", status)
			}
			mustWrite(t, conn, request(commandConnect, testCase.atyp, testCase.address, 8443))
			code, bound := readReply(t, conn)
			if code != replySucceeded || !bytes.Equal(bound, []byte{192, 0, 2, 7, 0x10, 0xe1}) {
				t.Fatalf("reply = %d %v", code, bound)
			}
			if got := ts.transport.lastTarget(); got.Host != testCase.want || got.Port != 8443 {
				t.Fatalf("target = %#v", got)
			}
			mustWrite(t, conn, []byte("ping"))
			if echoed := mustRead(t, conn, 4); string(echoed) != "ping" {
				t.Fatalf("echo = %q", echoed)
			}
			_ = conn.Close()
			<-done
		})
	}
}

func TestRequestReplyCodes(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		message  []byte
		relayErr error
		want     byte
	}{
		{"bind is unsupported", request(commandBind, addressIPv4, []byte{1, 2, 3, 4}, 80), nil, replyCommandUnsupported},
		{"unknown address type", []byte{version5, commandConnect, 0, 0x05}, nil, replyAddressUnsupported},
		{"zero port", request(commandConnect, addressIPv4, []byte{1, 2, 3, 4}, 0), nil, replyGeneralFailure},
		{"refused", request(commandConnect, addressIPv4, []byte{1, 2, 3, 4}, 80), syscall.ECONNREFUSED, replyConnectionRefused},
		{"unreachable", request(commandConnect, addressIPv4, []byte{1, 2, 3, 4}, 80), syscall.EHOSTUNREACH, replyHostUnreachable},
		{"timeout", request(commandConnect, addressIPv4, []byte{1, 2, 3, 4}, 80), context.DeadlineExceeded, replyTTLExpired},
		{"transport lacks command", request(commandConnect, addressIPv4, []byte{1, 2, 3, 4}, 80), fmt.Errorf("x: %w", provider.ErrCommandUnsupported), replyCommandUnsupported},
		{"transport lacks address type", request(commandConnect, addressIPv4, []byte{1, 2, 3, 4}, 80), fmt.Errorf("x: %w", provider.ErrAddressUnsupported), replyAddressUnsupported},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ts := newTestServer(t, 0)
			ts.transport.relayErr = testCase.relayErr
			conn, done := ts.handle(t)
			if status := authenticate(t, conn, ts.credential.Selector, ts.credential.Password); status != 0 {
				t.Fatalf("auth status = %d", status)
			}
			mustWrite(t, conn, testCase.message)
			if code, _ := readReply(t, conn); code != testCase.want {
				t.Fatalf("reply = %#x, want %#x", code, testCase.want)
			}
			_ = conn.Close()
			<-done
		})
	}
}

func TestUDPAssociateRepliesWithBoundAddress(t *testing.T) {
	ts := newTestServer(t, 0)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = ts.server.Serve(ctx, listener) }()
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if status := authenticate(t, conn, ts.credential.Selector, ts.credential.Password); status != 0 {
		t.Fatalf("auth status = %d", status)
	}
	mustWrite(t, conn, request(commandUDP, addressIPv4, []byte{0, 0, 0, 0}, 0))
	code, bound := readReply(t, conn)
	if code != replySucceeded || binary.BigEndian.Uint16(bound[len(bound)-2:]) == 0 {
		t.Fatalf("UDP reply = %d %v", code, bound)
	}
}

func TestUDPAssociationAcceptsOnlyTheClientSource(t *testing.T) {
	association := &udpAssociation{}
	association.clientIP = mustAddr(t, "192.0.2.10")
	if association.acceptSource(&net.UDPAddr{IP: net.ParseIP("192.0.2.11"), Port: 5000}) {
		t.Fatal("foreign client IP accepted")
	}
	if !association.acceptSource(&net.UDPAddr{IP: net.ParseIP("192.0.2.10"), Port: 5000}) {
		t.Fatal("client datagram rejected")
	}
	if association.acceptSource(&net.UDPAddr{IP: net.ParseIP("192.0.2.10"), Port: 5001}) {
		t.Fatal("second source port accepted after the association was claimed")
	}
	if !association.acceptSource(&net.UDPAddr{IP: net.ParseIP("192.0.2.10"), Port: 5000}) {
		t.Fatal("claimed source port rejected")
	}
}

func TestConnectionSlotsBoundConcurrentHandlers(t *testing.T) {
	ts := newTestServer(t, 1)
	ts.transport.hold = make(chan struct{})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- ts.server.Serve(ctx, listener) }()
	first, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	_ = first.SetDeadline(time.Now().Add(5 * time.Second))
	if status := authenticate(t, first, ts.credential.Selector, ts.credential.Password); status != 0 {
		t.Fatalf("auth status = %d", status)
	}
	mustWrite(t, first, request(commandConnect, addressIPv4, []byte{1, 2, 3, 4}, 80))
	if code, _ := readReply(t, first); code != replySucceeded {
		t.Fatalf("reply = %d", code)
	}
	second, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	_ = second.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(second, make([]byte, 1)); err == nil {
		t.Fatal("connection beyond the slot limit was served")
	}
	cancel()
	if err := <-served; err != nil {
		t.Fatalf("Serve returned %v after cancellation", err)
	}
	// Graceful shutdown waits for the established flow to finish.
	close(ts.transport.hold)
	shutdown, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := ts.server.Shutdown(shutdown); err != nil {
		t.Fatalf("Shutdown did not drain: %v", err)
	}
}

func TestShutdownForcesHandlersAfterDeadline(t *testing.T) {
	ts := newTestServer(t, 0)
	ts.transport.hold = make(chan struct{})
	defer close(ts.transport.hold)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = ts.server.Serve(context.Background(), listener) }()
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if status := authenticate(t, conn, ts.credential.Selector, ts.credential.Password); status != 0 {
		t.Fatalf("auth status = %d", status)
	}
	mustWrite(t, conn, request(commandConnect, addressIPv4, []byte{1, 2, 3, 4}, 80))
	if code, _ := readReply(t, conn); code != replySucceeded {
		t.Fatalf("reply = %d", code)
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := ts.server.Shutdown(shutdown); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("forced shutdown = %v", err)
	}
	if _, err := io.ReadFull(conn, make([]byte, 1)); err == nil {
		t.Fatal("forced shutdown left the client connection open")
	}
}

type flakyListener struct {
	net.Listener
	mu       sync.Mutex
	failures int
}

func (listener *flakyListener) Accept() (net.Conn, error) {
	listener.mu.Lock()
	if listener.failures > 0 {
		listener.failures--
		listener.mu.Unlock()
		return nil, &net.OpError{Op: "accept", Net: "tcp", Err: syscall.EMFILE}
	}
	listener.mu.Unlock()
	return listener.Listener.Accept()
}

func TestServeBacksOffOnTemporaryAcceptErrors(t *testing.T) {
	ts := newTestServer(t, 0)
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := &flakyListener{Listener: inner, failures: 3}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- ts.server.Serve(ctx, listener) }()
	conn, err := net.Dial("tcp", inner.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if status := authenticate(t, conn, ts.credential.Selector, ts.credential.Password); status != 0 {
		t.Fatalf("auth status after EMFILE = %d", status)
	}
	select {
	case err := <-served:
		t.Fatalf("Serve stopped on a temporary error: %v", err)
	default:
	}
	cancel()
	if err := <-served; err != nil {
		t.Fatalf("Serve returned %v after cancellation", err)
	}
}

func TestTemporaryAcceptErrorClassification(t *testing.T) {
	if !temporaryAcceptError(&net.OpError{Op: "accept", Err: syscall.EMFILE}) {
		t.Fatal("EMFILE is not temporary")
	}
	if temporaryAcceptError(net.ErrClosed) {
		t.Fatal("closed listener is temporary")
	}
	if temporaryAcceptError(errors.New("permanent")) {
		t.Fatal("arbitrary error is temporary")
	}
}

func mustAddr(t *testing.T, value string) netip.Addr {
	t.Helper()
	address, err := netip.ParseAddr(value)
	if err != nil {
		t.Fatal(err)
	}
	return address
}

func TestHandshakeAdmissionIsBoundedPerClientIP(t *testing.T) {
	ts := newTestServer(t, 0)
	client := &net.TCPAddr{IP: net.ParseIP("192.0.2.50"), Port: 1000}
	var releases []func()
	for range maxHandshakesPerClient {
		release, ok := ts.server.beginHandshake(client)
		if !ok {
			t.Fatal("handshake below the per-client bound was refused")
		}
		releases = append(releases, release)
	}
	if _, ok := ts.server.beginHandshake(client); ok {
		t.Fatal("handshake beyond the per-client bound was admitted")
	}
	if _, ok := ts.server.beginHandshake(&net.TCPAddr{IP: net.ParseIP("192.0.2.51"), Port: 1000}); !ok {
		t.Fatal("another client was refused")
	}
	releases[0]()
	releases[0]() // idempotent
	if _, ok := ts.server.beginHandshake(client); !ok {
		t.Fatal("released handshake slot was not reusable")
	}
}

func TestEstablishedFlowClosesWhenSessionIsRevoked(t *testing.T) {
	ts := newTestServer(t, 0)
	ts.server.revalidateEvery = 20 * time.Millisecond
	ts.transport.hold = make(chan struct{})
	defer close(ts.transport.hold)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = ts.server.Serve(ctx, listener) }()
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if status := authenticate(t, conn, ts.credential.Selector, ts.credential.Password); status != 0 {
		t.Fatalf("auth status = %d", status)
	}
	mustWrite(t, conn, request(commandConnect, addressIPv4, []byte{1, 2, 3, 4}, 80))
	if code, _ := readReply(t, conn); code != replySucceeded {
		t.Fatalf("reply = %d", code)
	}
	ts.snapshots.mu.Lock()
	ts.snapshots.revoked = true
	ts.snapshots.mu.Unlock()
	if _, err := io.ReadFull(conn, make([]byte, 1)); err == nil {
		t.Fatal("flow survived provider logout")
	}
}
