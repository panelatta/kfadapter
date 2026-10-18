package socks

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	"github.com/oshinop/chinadapter/internal/shadowsocks"
)

// shadowsocksUDPFixture serves the fixture's server on a UDP socket too.
type shadowsocksUDPFixture struct {
	*frontendFixture
	conn      *net.UDPConn
	served    chan error
	serverKey []byte
	userKey   []byte
}

func newShadowsocksUDPFixture(t *testing.T, packets chan []byte, maxConnections int, configure func(*Server)) *shadowsocksUDPFixture {
	t.Helper()
	fixture := newProtocolFixture(t, newFrontendDatagramEcho(packets), maxConnections, nil, configure, FrontendShadowsocks)
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- fixture.server.ServePacketFrontend(context.Background(), conn, FrontendShadowsocks) }()
	serverKey, _ := fixture.identity.ShadowsocksServerKey()
	userKey, _ := fixture.identity.ShadowsocksUserKey(fixture.snapshots.pin.Node.Route())
	return &shadowsocksUDPFixture{frontendFixture: fixture, conn: conn, served: served, serverKey: serverKey, userKey: userKey}
}

func (fixture *shadowsocksUDPFixture) client(t *testing.T) *shadowsocks.UDPClient {
	t.Helper()
	client, err := shadowsocks.NewUDPClient(fixture.serverKey, fixture.userKey, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func (fixture *shadowsocksUDPFixture) send(t *testing.T, from *net.UDPConn, packet []byte) {
	t.Helper()
	if _, err := from.WriteToUDP(packet, fixture.conn.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatal(err)
	}
}

func readShadowsocksReply(t *testing.T, socket *net.UDPConn, client *shadowsocks.UDPClient, want []byte) {
	t.Helper()
	buffer := make([]byte, 2048)
	_ = socket.SetReadDeadline(time.Now().Add(frontendTestTimeout))
	n, err := socket.Read(buffer)
	if err != nil {
		t.Fatalf("no reply: %v", err)
	}
	opened, err := client.Open(buffer[:n])
	if err != nil || !bytes.Equal(opened, want) {
		t.Fatalf("reply = %x, %v; want %x", opened, err, want)
	}
}

func expectNoTransportPacket(t *testing.T, packets <-chan []byte) {
	t.Helper()
	select {
	case packet := <-packets:
		t.Fatalf("transport received %x", packet)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestShadowsocksUDPRelaysSessionsAndFollowsClients(t *testing.T) {
	packets := make(chan []byte, 16)
	fixture := newShadowsocksUDPFixture(t, packets, 0, nil)
	query := append([]byte{addressIPv4, 1, 2, 3, 4, 0, 53}, "query"...)

	// Two clients of the same route get separate sessions on one port.
	first, second := fixture.client(t), fixture.client(t)
	firstSocket, secondSocket := listenFrontendUDP(t), listenFrontendUDP(t)
	opening := first.Seal(nil, query, 0)
	fixture.send(t, firstSocket, bytes.Clone(opening))
	receiveFrontendPacket(t, packets, append([]byte{0, 0, 0}, query...))
	readShadowsocksReply(t, firstSocket, first, query)
	fixture.send(t, secondSocket, second.Seal(nil, query, 16))
	receiveFrontendPacket(t, packets, append([]byte{0, 0, 0}, query...))
	readShadowsocksReply(t, secondSocket, second, query)

	// Replies follow the latest authenticated source of a session.
	moved := listenFrontendUDP(t)
	domain := append(append([]byte{addressDomain, 11}, "example.com"...), 1, 187, 'x')
	fixture.send(t, moved, first.Seal(nil, domain, 0))
	receiveFrontendPacket(t, packets, append([]byte{0, 0, 0}, domain...))
	readShadowsocksReply(t, moved, first, domain)

	// Replayed packets and packets for unknown users never reach the route.
	fixture.send(t, firstSocket, opening)
	stranger, _ := shadowsocks.NewUDPClient(fixture.serverKey, bytes.Repeat([]byte{7}, shadowsocks.KeySize), time.Now)
	fixture.send(t, firstSocket, stranger.Seal(nil, query, 0))
	expectNoTransportPacket(t, packets)
	if got := activeHandlers(fixture.server); got != 2 {
		t.Fatalf("active sessions = %d, want 2", got)
	}
}

func TestShadowsocksUDPSessionsShareConnectionSlots(t *testing.T) {
	packets := make(chan []byte, 16)
	fixture := newShadowsocksUDPFixture(t, packets, 1, nil)
	query := append([]byte{addressIPv4, 1, 2, 3, 4, 0, 53}, "query"...)
	first, second := fixture.client(t), fixture.client(t)
	socket := listenFrontendUDP(t)
	fixture.send(t, socket, first.Seal(nil, query, 0))
	receiveFrontendPacket(t, packets, append([]byte{0, 0, 0}, query...))
	fixture.send(t, socket, second.Seal(nil, query, 0))
	expectNoTransportPacket(t, packets)
}

func TestShadowsocksUDPIdleSessionsEndWithoutAllowingReplay(t *testing.T) {
	packets := make(chan []byte, 16)
	fixture := newShadowsocksUDPFixture(t, packets, 0, func(server *Server) { server.ssUDPIdle = 50 * time.Millisecond })
	query := append([]byte{addressIPv4, 1, 2, 3, 4, 0, 53}, "query"...)
	client := fixture.client(t)
	socket := listenFrontendUDP(t)
	opening := client.Seal(nil, query, 0)
	fixture.send(t, socket, bytes.Clone(opening))
	receiveFrontendPacket(t, packets, append([]byte{0, 0, 0}, query...))
	waitForActiveHandlers(t, fixture.server, 0)

	// The ended session's replay window carries over: its old packet stays
	// rejected, while the client's next packet starts a new session.
	fixture.send(t, socket, opening)
	expectNoTransportPacket(t, packets)
	fixture.send(t, socket, client.Seal(nil, query, 0))
	receiveFrontendPacket(t, packets, append([]byte{0, 0, 0}, query...))
}

func TestCloseListenerEndsShadowsocksUDPSessions(t *testing.T) {
	packets := make(chan []byte, 16)
	fixture := newShadowsocksUDPFixture(t, packets, 0, nil)
	query := append([]byte{addressIPv4, 1, 2, 3, 4, 0, 53}, "query"...)
	client := fixture.client(t)
	socket := listenFrontendUDP(t)
	fixture.send(t, socket, client.Seal(nil, query, 0))
	receiveFrontendPacket(t, packets, append([]byte{0, 0, 0}, query...))
	fixture.server.CloseListener(fixture.conn)
	select {
	case err := <-fixture.served:
		if err != nil {
			t.Fatalf("ServePacketFrontend: %v", err)
		}
	case <-time.After(frontendTestTimeout):
		t.Fatal("ServePacketFrontend did not stop")
	}
	waitForActiveHandlers(t, fixture.server, 0)
}

func activeHandlers(server *Server) int {
	server.lifecycleMu.Lock()
	defer server.lifecycleMu.Unlock()
	return len(server.active)
}

func waitForActiveHandlers(t *testing.T, server *Server, want int) {
	t.Helper()
	deadline := time.Now().Add(frontendTestTimeout)
	for activeHandlers(server) != want {
		if time.Now().After(deadline) {
			t.Fatalf("active handlers = %d, want %d", activeHandlers(server), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
