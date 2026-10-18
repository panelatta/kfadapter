package socks

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oshinop/chinadapter/internal/provider"
	"github.com/oshinop/chinadapter/internal/shadowsocks"
)

const (
	// ssUDPIdleTimeout ends a Shadowsocks UDP session after this long without
	// a packet in either direction.
	ssUDPIdleTimeout = 3 * time.Minute
	// ssUDPRetiredWindow keeps an ended session's replay window: SIP022
	// requires remembering sessions for at least 60 seconds, so packets of a
	// session that ended early cannot be replayed into a new one while their
	// timestamps are still accepted.
	ssUDPRetiredWindow = 60 * time.Second
	ssUDPSweepInterval = 10 * time.Second
	// maxShadowsocksUDPSessions bounds the sessions of one packet listener;
	// each also takes one of the server's connection slots.
	maxShadowsocksUDPSessions = 1024
	// ssUDPQueueLength bounds the client datagrams waiting for a session's
	// upstream; further datagrams are dropped, as on any UDP path.
	ssUDPQueueLength = 128
	// ssUDPSocketBuffer is the requested socket buffer size; the kernel caps
	// it (net.core.rmem_max and wmem_max on Linux).
	ssUDPSocketBuffer = 4 << 20
	maxUDPDatagram    = 1<<16 - 1
)

type ssUDPKey struct {
	userKey string
	session uint64
}

type ssUDPRetired struct {
	crypto *shadowsocks.UDPSession
	at     time.Time
}

// ssUDPFrontend demultiplexes one Shadowsocks UDP socket into sessions by
// user and client session ID; every session is one SOCKS-style UDP
// association dispatched through the route its user key names.
type ssUDPFrontend struct {
	server *Server
	conn   *net.UDPConn

	mu       sync.Mutex
	sessions map[ssUDPKey]*ssUDPSession
	retired  map[ssUDPKey]ssUDPRetired
}

// ServePacketFrontend serves Shadowsocks 2022 UDP sessions on conn until ctx
// is canceled or CloseListener closes conn. Sessions share the connection
// slots and route dispatch of the stream frontends.
func (s *Server) ServePacketFrontend(ctx context.Context, conn net.PacketConn, frontend Frontend) error {
	if s == nil || ctx == nil || conn == nil {
		return errors.New("socks: server, context, and packet connection are required")
	}
	if frontend != FrontendShadowsocks {
		return fmt.Errorf("socks: frontend %d has no packet protocol", frontend)
	}
	udp, ok := conn.(*net.UDPConn)
	if !ok {
		return errors.New("socks: Shadowsocks UDP requires a UDP socket")
	}
	if err := s.beginServe(conn); err != nil {
		return err
	}
	defer s.endServe(conn)
	_ = udp.SetReadBuffer(ssUDPSocketBuffer)
	_ = udp.SetWriteBuffer(ssUDPSocketBuffer)
	front := &ssUDPFrontend{server: s, conn: udp, sessions: make(map[ssUDPKey]*ssUDPSession), retired: make(map[ssUDPKey]ssUDPRetired)}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		ticker := time.NewTicker(min(ssUDPSweepInterval, max(s.ssUDPIdle/2, time.Millisecond)))
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				_ = conn.Close()
				return
			case <-stop:
				return
			case now := <-ticker.C:
				front.sweep(now)
			}
		}
	}()
	defer front.closeAll()

	buffer := make([]byte, maxUDPDatagram)
	for {
		n, source, err := udp.ReadFromUDPAddrPort(buffer)
		if err != nil {
			if ctx.Err() != nil || !s.accepting(conn) {
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			// Errors such as ICMP-reported unreachable peers end no session.
			continue
		}
		front.handle(ctx, buffer[:n], source)
	}
}

// handle authenticates one client packet and hands it to its session,
// starting the session when this is its first packet. Packets that fail any
// check are dropped without a reply. Like accepted connections, sessions do
// not inherit ctx's cancellation; they end with their socket instead, since
// their replies need it.
func (front *ssUDPFrontend) handle(ctx context.Context, packet []byte, source netip.AddrPort) {
	s := front.server
	registry := s.credentials.Load()
	if registry == nil || registry.shadowsocks == nil {
		return
	}
	identity, found := registry.shadowsocks.IdentifyPacket(packet)
	if !found {
		return
	}
	userKey := registry.shadowsocksKeys[identity.User]
	key := ssUDPKey{userKey: string(userKey), session: identity.SessionID}
	front.mu.Lock()
	session := front.sessions[key]
	front.mu.Unlock()
	if session != nil {
		payload, err := session.crypto.Open(identity, packet)
		if err != nil {
			return
		}
		session.received(source, payload)
		return
	}

	crypto, err := shadowsocks.NewUDPSession(registry.shadowsocks, identity, time.Now)
	if err != nil {
		return
	}
	front.mu.Lock()
	if retired, ok := front.retired[key]; ok {
		crypto.Continue(retired.crypto)
	}
	full := len(front.sessions) >= maxShadowsocksUDPSessions
	front.mu.Unlock()
	if full {
		return
	}
	payload, err := crypto.Open(identity, packet)
	if err != nil {
		return
	}
	principal := registry.shadowsocksRoute[identity.User]
	routeUsername, err := s.resolvePrincipal(registry, principal)
	if err != nil {
		return
	}
	select {
	case s.slots <- struct{}{}:
	default:
		return
	}
	session, err = newSSUDPSession(key, crypto, front.conn)
	if err != nil {
		<-s.slots
		return
	}
	handlerCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	if !s.addActiveHandler(session.control, front.conn, cancel) {
		cancel()
		_ = session.Close()
		_ = session.control.Close()
		<-s.slots
		return
	}
	front.mu.Lock()
	front.sessions[key] = session
	delete(front.retired, key)
	front.mu.Unlock()
	session.received(source, payload)
	go func() {
		defer func() { <-s.slots }()
		defer s.removeActiveHandler(session.control)
		defer cancel()
		defer front.retire(session)
		defer func() { _ = session.Close() }()
		_ = s.dispatch(handlerCtx, flow{
			command: commandUDP, client: session.control, routeUsername: routeUsername,
			authorized: func(current *credentialRegistry) bool {
				route, found := current.shadowsocksRouteFor(userKey)
				return s.principalAuthorized(current, principal, route, found)
			},
			openPackets: func() (provider.PacketIO, error) { return session, nil },
			// Shadowsocks UDP has no reply to a failed association; the
			// session simply ends.
			ready: func(net.Addr) error { return nil },
			fail:  func(byte) {},
		})
	}()
}

// retire removes an ended session and keeps its replay window.
func (front *ssUDPFrontend) retire(session *ssUDPSession) {
	front.mu.Lock()
	defer front.mu.Unlock()
	if front.sessions[session.key] == session {
		delete(front.sessions, session.key)
	}
	front.retired[session.key] = ssUDPRetired{crypto: session.crypto, at: time.Now()}
}

// sweep ends idle sessions and forgets old replay windows.
func (front *ssUDPFrontend) sweep(now time.Time) {
	front.mu.Lock()
	var idle []*ssUDPSession
	for _, session := range front.sessions {
		if now.Sub(time.Unix(0, session.lastActive.Load())) >= front.server.ssUDPIdle {
			idle = append(idle, session)
		}
	}
	for key, retired := range front.retired {
		if now.Sub(retired.at) >= ssUDPRetiredWindow {
			delete(front.retired, key)
		}
	}
	front.mu.Unlock()
	for _, session := range idle {
		_ = session.Close()
	}
}

func (front *ssUDPFrontend) closeAll() {
	front.mu.Lock()
	sessions := make([]*ssUDPSession, 0, len(front.sessions))
	for _, session := range front.sessions {
		sessions = append(sessions, session)
	}
	front.mu.Unlock()
	for _, session := range sessions {
		_ = session.Close()
	}
}

// ssUDPSession is one client session as a provider.PacketIO. Its control is
// one end of a pipe that never carries data: closing the session closes the
// other end, which ends the association like a SOCKS client closing its TCP
// control connection.
type ssUDPSession struct {
	key        ssUDPKey
	crypto     *shadowsocks.UDPSession
	conn       *net.UDPConn
	flowID     uint16
	client     atomic.Pointer[netip.AddrPort]
	lastActive atomic.Int64
	queue      chan []byte
	closed     chan struct{}
	closeOnce  sync.Once
	control    net.Conn
	peer       net.Conn

	writeMu sync.Mutex
	out     []byte
}

func newSSUDPSession(key ssUDPKey, crypto *shadowsocks.UDPSession, conn *net.UDPConn) (*ssUDPSession, error) {
	var flowID [2]byte
	for flowID == [2]byte{} {
		if _, err := rand.Read(flowID[:]); err != nil {
			return nil, err
		}
	}
	control, peer := net.Pipe()
	session := &ssUDPSession{
		key: key, crypto: crypto, conn: conn, flowID: binary.BigEndian.Uint16(flowID[:]),
		queue: make(chan []byte, ssUDPQueueLength), closed: make(chan struct{}), control: control, peer: peer,
	}
	session.lastActive.Store(time.Now().UnixNano())
	return session, nil
}

// received records an authenticated client datagram: replies go to the
// address of the latest one, so the session follows a client that changes
// networks.
func (session *ssUDPSession) received(source netip.AddrPort, addressAndPayload []byte) {
	session.client.Store(&source)
	session.lastActive.Store(time.Now().UnixNano())
	datagram := make([]byte, 3+len(addressAndPayload))
	copy(datagram[3:], addressAndPayload)
	select {
	case session.queue <- datagram:
	default:
	}
}

func (session *ssUDPSession) FlowID() uint16      { return session.flowID }
func (session *ssUDPSession) LocalAddr() net.Addr { return session.conn.LocalAddr() }

// ReadPacket returns the next client datagram in SOCKS UDP request framing.
func (session *ssUDPSession) ReadPacket(packet []byte) (int, error) {
	for {
		select {
		case datagram := <-session.queue:
			if len(datagram) <= len(packet) {
				return copy(packet, datagram), nil
			}
		case <-session.closed:
			return 0, net.ErrClosed
		}
	}
}

// WritePacket sends one SOCKS UDP reply to the client as a server packet.
func (session *ssUDPSession) WritePacket(packet []byte) error {
	if len(packet) < 4 || packet[0] != 0 || packet[1] != 0 || packet[2] != 0 {
		return nil
	}
	client := session.client.Load()
	if client == nil {
		return nil
	}
	session.writeMu.Lock()
	defer session.writeMu.Unlock()
	session.out = session.crypto.Seal(session.out[:0], packet[3:])
	session.lastActive.Store(time.Now().UnixNano())
	if _, err := session.conn.WriteToUDPAddrPort(session.out, *client); errors.Is(err, net.ErrClosed) {
		return err
	}
	// Any other failed send loses this datagram only.
	return nil
}

func (session *ssUDPSession) Close() error {
	session.closeOnce.Do(func() {
		close(session.closed)
		_ = session.peer.Close()
	})
	return nil
}
