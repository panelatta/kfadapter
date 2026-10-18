package socks

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cenkalti/backoff/v5"

	credentialspkg "github.com/oshinop/chinadapter/internal/credentials"
	"github.com/oshinop/chinadapter/internal/provider"
	"github.com/oshinop/chinadapter/internal/shadowsocks"
	"github.com/oshinop/chinadapter/internal/state"
)

const (
	defaultHandshakeTimeout = 10 * time.Second
	defaultMaxConnections   = 1024
	maximumMaxConnections   = 65536
	// maxHandshakesPerClient bounds unauthenticated connections from one IP,
	// so a single LAN host cannot hold every slot through the setup timeout.
	maxHandshakesPerClient = 32
	// defaultFlowRevalidation is how often an established flow re-checks that
	// its credentials and provider account are still current.
	defaultFlowRevalidation = 30 * time.Second
)

// SnapshotSource resolves compact tunnel pins without exposing a runtime
// snapshot or selector map to the SOCKS hot path. state.Manager is the
// production implementation.
type SnapshotSource interface {
	CompactPin(selector string, now time.Time) (state.TunnelPin, error)
	SessionCurrentPin(pin state.TunnelPin, now time.Time) bool
}

// SmartSelector resolves the automatic route once per connection. Established
// flows retain that route while later connections can select another node.
type SmartSelector interface {
	Resolve() (string, error)
	Enabled() bool
}

// Config defines the SOCKS listener's dependency boundaries.
type Config struct {
	Smart           SmartSelector
	Snapshots       SnapshotSource
	Identity        *credentialspkg.Identity
	InitialSnapshot *state.RuntimeSnapshot
	Providers       *provider.Registry

	DialContext      provider.DialContextFunc
	HandshakeTimeout time.Duration
	MaxConnections   int
}

// Server hosts the proxy frontends: SOCKS5/RFC1929, SOCKS5 over TLS, Snell v6
// and Shadowsocks 2022. Each immutable credential registry is built from the
// current snapshot; established flows retain only a compact tunnel pin.
type Server struct {
	smart            SmartSelector
	snapshots        SnapshotSource
	providers        *provider.Registry
	dial             provider.DialContextFunc
	handshakeTimeout time.Duration
	slots            chan struct{}
	credentials      atomic.Pointer[credentialRegistry]
	revalidateEvery  time.Duration
	// ssUDPIdle ends a Shadowsocks UDP session without traffic for this long.
	ssUDPIdle time.Duration

	handshakeMu sync.Mutex
	handshakes  map[netip.Addr]int

	snellRecent recentKeys
	snellTrials chan struct{}
	salts       *shadowsocks.SaltFilter

	lifecycleMu  sync.Mutex
	listeners    map[io.Closer]struct{} // served net.Listeners and net.PacketConns
	shuttingDown bool
	active       map[net.Conn]activeHandler
	drained      chan struct{}
}

type activeHandler struct {
	cancel   context.CancelFunc
	listener io.Closer
}

// New constructs a server. Listener selection belongs to the caller.
func New(config Config) (*Server, error) {
	if config.Snapshots == nil {
		return nil, errors.New("socks: snapshot source is required")
	}
	if config.Identity == nil {
		return nil, errors.New("socks: installation identity is required")
	}
	if config.Providers == nil {
		return nil, errors.New("socks: provider registry is required")
	}
	timeout := config.HandshakeTimeout
	if timeout == 0 {
		timeout = defaultHandshakeTimeout
	}
	if timeout < 0 {
		return nil, errors.New("socks: handshake timeout must be positive")
	}
	maxConnections := config.MaxConnections
	if maxConnections == 0 {
		maxConnections = defaultMaxConnections
	}
	if maxConnections < 1 || maxConnections > maximumMaxConnections {
		return nil, fmt.Errorf("socks: max connections must be in 1..%d", maximumMaxConnections)
	}
	dial := config.DialContext
	if dial == nil {
		dialer := &net.Dialer{}
		dial = dialer.DialContext
	}
	drained := make(chan struct{})
	close(drained)
	server := &Server{
		smart: config.Smart, snapshots: config.Snapshots, providers: config.Providers, dial: dial, handshakeTimeout: timeout,
		slots: make(chan struct{}, maxConnections), listeners: make(map[io.Closer]struct{}), active: make(map[net.Conn]activeHandler),
		drained: drained, handshakes: make(map[netip.Addr]int), revalidateEvery: defaultFlowRevalidation, ssUDPIdle: ssUDPIdleTimeout,
		snellTrials: make(chan struct{}, max(2, runtime.NumCPU())), salts: shadowsocks.NewSaltFilter(),
	}
	if err := server.SetCredentials(config.Identity, config.InitialSnapshot); err != nil {
		return nil, err
	}
	return server, nil
}

// SetCredentials swaps one coherent generation of every frontend's
// credentials: SOCKS usernames and instance password, Snell PSKs, Shadowsocks
// user keys and the SOCKS5-TLS certificate.
func (s *Server) SetCredentials(authority *credentialspkg.Identity, snapshot *state.RuntimeSnapshot) error {
	if s == nil || authority == nil {
		return errors.New("socks: installation identity is required")
	}
	next, err := buildCredentialRegistry(authority, snapshot)
	if err != nil {
		return err
	}
	s.credentials.Store(next)
	return nil
}

// Shutdown stops accepting new connections on every listener and waits for
// active handlers to drain. When ctx expires, it cancels and closes every
// remaining handler so relays and in-flight setup cannot outlive the shutdown
// deadline.
func (s *Server) Shutdown(ctx context.Context) error {
	if s == nil || ctx == nil {
		return errors.New("socks: server and shutdown context are required")
	}
	s.lifecycleMu.Lock()
	s.shuttingDown = true
	listeners := make([]io.Closer, 0, len(s.listeners))
	for listener := range s.listeners {
		listeners = append(listeners, listener)
	}
	clear(s.listeners)
	drained := s.drained
	s.lifecycleMu.Unlock()
	for _, listener := range listeners {
		_ = listener.Close()
	}
	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		s.forceActiveHandlers(nil)
		return ctx.Err()
	}
}

// CloseListener stops accepting on one listener or packet connection served
// by this server and immediately ends the flows it accepted. Other listeners
// are unaffected.
func (s *Server) CloseListener(listener io.Closer) {
	if s == nil || listener == nil {
		return
	}
	s.lifecycleMu.Lock()
	delete(s.listeners, listener)
	s.lifecycleMu.Unlock()
	_ = listener.Close()
	s.forceActiveHandlers(listener)
}

func (s *Server) beginServe(listener io.Closer) error {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.shuttingDown {
		return errors.New("socks: server is shutting down")
	}
	if _, serving := s.listeners[listener]; serving {
		return errors.New("socks: listener is already served")
	}
	s.listeners[listener] = struct{}{}
	return nil
}

func (s *Server) endServe(listener io.Closer) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	delete(s.listeners, listener)
}

func (s *Server) accepting(listener io.Closer) bool {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	_, serving := s.listeners[listener]
	return !s.shuttingDown && serving
}

func (s *Server) addActiveHandler(connection net.Conn, listener io.Closer, cancel context.CancelFunc) bool {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if _, serving := s.listeners[listener]; s.shuttingDown || !serving {
		return false
	}
	if len(s.active) == 0 {
		s.drained = make(chan struct{})
	}
	s.active[connection] = activeHandler{cancel: cancel, listener: listener}
	return true
}

func (s *Server) removeActiveHandler(connection net.Conn) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	delete(s.active, connection)
	if len(s.active) == 0 {
		close(s.drained)
	}
}

// forceActiveHandlers cancels and closes the handlers accepted on listener,
// or every handler when listener is nil.
func (s *Server) forceActiveHandlers(listener io.Closer) {
	s.lifecycleMu.Lock()
	connections := make([]net.Conn, 0, len(s.active))
	cancels := make([]context.CancelFunc, 0, len(s.active))
	for connection, handler := range s.active {
		if listener == nil || handler.listener == listener {
			connections = append(connections, connection)
			cancels = append(cancels, handler.cancel)
		}
	}
	s.lifecycleMu.Unlock()
	for index, connection := range connections {
		cancels[index]()
		_ = connection.Close()
	}
}

// Frontend selects the protocol a listener speaks.
type Frontend int

const (
	FrontendSOCKS Frontend = iota
	FrontendSOCKSTLS
	FrontendSnell
	FrontendShadowsocks
)

// Serve accepts SOCKS5 connections until ctx is canceled. Accepted handlers
// retain independent contexts and must be drained with Shutdown.
func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	return s.ServeFrontend(ctx, listener, FrontendSOCKS)
}

// ServeFrontend accepts connections of one frontend protocol until ctx is
// canceled or CloseListener closes listener. Every frontend shares the
// connection slots, per-client setup limit and route dispatch.
func (s *Server) ServeFrontend(ctx context.Context, listener net.Listener, frontend Frontend) error {
	if s == nil || ctx == nil || listener == nil {
		return errors.New("socks: server, context, and listener are required")
	}
	accept, handle := listener, s.handleConn
	switch frontend {
	case FrontendSOCKS:
	case FrontendSOCKSTLS:
		accept = tls.NewListener(listener, s.tlsConfig())
	case FrontendSnell:
		handle = s.handleSnell
	case FrontendShadowsocks:
		handle = s.handleShadowsocks
	default:
		return fmt.Errorf("socks: unknown frontend %d", frontend)
	}
	if err := s.beginServe(listener); err != nil {
		return err
	}
	defer s.endServe(listener)
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = listener.Close()
		case <-stop:
		}
	}()
	defer close(stop)

	retry := newAcceptBackOff()
	for {
		connection, err := accept.Accept()
		if err != nil {
			if ctx.Err() != nil || !s.accepting(listener) {
				return nil
			}
			if !temporaryAcceptError(err) {
				return err
			}
			timer := time.NewTimer(retry.NextBackOff())
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil
			case <-timer.C:
			}
			continue
		}
		retry.Reset()
		releaseHandshake, admitted := s.beginHandshake(connection.RemoteAddr())
		if !admitted {
			_ = connection.Close()
			continue
		}
		select {
		case s.slots <- struct{}{}:
			handlerCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
			if !s.addActiveHandler(connection, listener, cancel) {
				cancel()
				releaseHandshake()
				_ = connection.Close()
				<-s.slots
				continue
			}
			keepAlive(connection)
			go func(connection net.Conn, handlerCtx context.Context, cancel context.CancelFunc) {
				defer func() { <-s.slots }()
				defer s.removeActiveHandler(connection)
				defer cancel()
				defer releaseHandshake()
				_ = handle(handlerCtx, connection, releaseHandshake)
			}(connection, handlerCtx, cancel)
		default:
			releaseHandshake()
			_ = connection.Close()
		}
	}
}

// clientKeepAlive detects clients that vanished without closing, such as a
// phone that left its network, well before an idle relay would notice.
var clientKeepAlive = net.KeepAliveConfig{Enable: true, Idle: 30 * time.Second, Interval: 10 * time.Second, Count: 3}

func keepAlive(connection net.Conn) {
	if inner, ok := connection.(interface{ NetConn() net.Conn }); ok {
		connection = inner.NetConn()
	}
	if tcp, ok := connection.(*net.TCPConn); ok {
		_ = tcp.SetKeepAliveConfig(clientKeepAlive)
	}
}

// temporaryAcceptError reports accept failures caused by transient resource
// pressure, such as descriptor exhaustion, rather than a closed listener.
func temporaryAcceptError(err error) bool {
	if errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE) ||
		errors.Is(err, syscall.ENOBUFS) || errors.Is(err, syscall.ENOMEM) ||
		errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EINTR) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func newAcceptBackOff() backoff.ExponentialBackOff {
	return backoff.ExponentialBackOff{
		InitialInterval: 5 * time.Millisecond, RandomizationFactor: 0,
		Multiplier: 2, MaxInterval: time.Second,
	}
}

// HandleConn processes one SOCKS connection. It never logs credentials,
// tunnel material, provider extensions, or requested destinations.
func (s *Server) HandleConn(ctx context.Context, client net.Conn) error {
	return s.handleConn(ctx, client, nil)
}

// handleConn calls releaseHandshake, when set, once authentication succeeds,
// so an authenticated flow no longer counts against its client's setup limit.
func (s *Server) handleConn(ctx context.Context, client net.Conn, releaseHandshake func()) error {
	if s == nil || client == nil {
		return errors.New("socks: server and client are required")
	}
	defer client.Close()
	defer closeOnCancel(ctx, client)()
	if err := client.SetDeadline(time.Now().Add(s.handshakeTimeout)); err != nil {
		return fmt.Errorf("socks: set client setup deadline: %w", err)
	}
	if err := negotiate(client, client); err != nil {
		return err
	}
	username, password, err := readUserPassword(client)
	if err != nil {
		_ = writeAuthStatus(client, false)
		return err
	}

	// Look up the username in the immutable current map before comparing the
	// shared instance password. The flow retains only its compact tunnel pin.
	now := time.Now()
	registry := s.credentials.Load()
	if registry == nil {
		_ = writeAuthStatus(client, false)
		return state.ErrSelectorUnknown
	}
	nodeCredentials := registry.authenticate(username, password)
	isSmart := !nodeCredentials && registry.authenticateSmart(username, password)
	if !nodeCredentials && !isSmart {
		_ = writeAuthStatus(client, false)
		return state.ErrSelectorUnknown
	}
	routeUsername := username
	if isSmart {
		if s.smart == nil || !s.smart.Enabled() {
			_ = writeAuthStatus(client, false)
			return state.ErrSelectorUnknown
		}
		if routeUsername, err = s.smart.Resolve(); err != nil {
			_ = writeAuthStatus(client, false)
			return err
		}
	}
	pin, err := s.snapshots.CompactPin(routeUsername, now)
	if err != nil || !pin.Node.TunnelEligible() {
		_ = writeAuthStatus(client, false)
		if err != nil {
			return err
		}
		return state.ErrSelectorUnknown
	}
	if err := writeAuthStatus(client, true); err != nil {
		return err
	}
	if releaseHandshake != nil {
		releaseHandshake()
	}

	command, destination, err := readRequest(client)
	if err != nil {
		code := replyGeneralFailure
		if errors.Is(err, errAddressTypeUnsupported) {
			code = replyAddressUnsupported
		}
		_ = writeReply(client, code, target{})
		return err
	}
	if command != commandConnect && command != commandUDP {
		_ = writeReply(client, replyCommandUnsupported, target{})
		return nil
	}
	if command == commandConnect && destination.Port == 0 {
		_ = writeReply(client, replyGeneralFailure, target{})
		return errBadAddress
	}
	return s.dispatch(ctx, flow{
		command: command, destination: destination, client: client, routeUsername: routeUsername, pin: &pin,
		authorized: func(registry *credentialRegistry) bool {
			return s.credentialsCurrent(registry, username, password, isSmart)
		},
		openPackets: func() (provider.PacketIO, error) {
			association, err := newUDPAssociation(client, destination)
			if err != nil {
				return nil, err
			}
			return association, nil
		},
		ready: func(bound net.Addr) error { return writeReply(client, replySucceeded, targetFromAddr(bound)) },
		fail:  func(code byte) { _ = writeReply(client, code, target{}) },
	})
}

// closeOnCancel closes client when ctx ends before the returned stop runs.
func closeOnCancel(ctx context.Context, client net.Conn) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = client.Close()
		case <-done:
		}
	}()
	return func() { close(done) }
}

// flow is one authenticated request from any frontend.
type flow struct {
	command     byte
	destination target
	// client carries the relayed stream of a CONNECT and owns the lifetime of
	// a UDP association; revocation closes it.
	client        net.Conn
	routeUsername string
	// pin, when the frontend resolved one during authentication, must still
	// name the same node and authority at dispatch.
	pin *state.TunnelPin
	// authorized re-authenticates the frontend credentials against a
	// registry generation, including the automatic route's enabled state.
	authorized  func(*credentialRegistry) bool
	openPackets func() (provider.PacketIO, error)
	ready       func(net.Addr) error
	fail        func(code byte)
}

// dispatch revalidates an authenticated request against the current
// credentials and snapshot, then relays it through the pinned node's
// transport. An admitted flow keeps its compact pin and is never remapped.
func (s *Server) dispatch(ctx context.Context, request flow) error {
	// Authentication may have completed before a client supplies its request.
	// Do not let a stalled socket create a tunnel after logout, authority
	// refresh, selector rotation, preference exclusion, or node replacement.
	// Re-resolving returns a compact current pin and never clones a snapshot.
	now := time.Now()
	registry := s.credentials.Load()
	if registry == nil || !request.authorized(registry) {
		request.fail(replyGeneralFailure)
		return state.ErrSelectorUnknown
	}
	pin, resolveErr := s.snapshots.CompactPin(request.routeUsername, now)
	node := pin.Node
	if resolveErr != nil || s.credentials.Load() != registry || !node.TunnelEligible() ||
		request.pin != nil && (!samePinAuthority(*request.pin, pin) || !sameCanonicalNode(request.pin.Node, node)) {
		request.fail(replyGeneralFailure)
		if resolveErr != nil {
			return resolveErr
		}
		return state.ErrSelectorUnknown
	}
	if err := request.client.SetDeadline(time.Time{}); err != nil {
		request.fail(replyGeneralFailure)
		return fmt.Errorf("socks: clear client setup deadline: %w", err)
	}

	transport, err := s.providers.Transport(node.Protocol)
	if err != nil {
		request.fail(replyGeneralFailure)
		return err
	}
	var packets provider.PacketIO
	var datagrams provider.DatagramTransport
	if request.command == commandUDP {
		capable, ok := transport.(provider.DatagramTransport)
		if !ok {
			request.fail(replyCommandUnsupported)
			return nil
		}
		datagrams = capable
		packets, err = request.openPackets()
		if err != nil {
			code := replyGeneralFailure
			if errors.Is(err, errAddressTypeUnsupported) {
				code = replyAddressUnsupported
			}
			request.fail(code)
			return err
		}
		defer func() { _ = packets.Close() }()
	}
	admit := func() bool {
		now := time.Now()
		if !s.snapshots.SessionCurrentPin(pin, now) {
			return false
		}
		currentRegistry := s.credentials.Load()
		if !request.authorized(currentRegistry) {
			return false
		}
		finalPin, resolveErr := s.snapshots.CompactPin(request.routeUsername, now)
		finalNode := finalPin.Node
		if resolveErr != nil || s.credentials.Load() != currentRegistry || !samePinAuthority(pin, finalPin) || !finalNode.TunnelEligible() || !sameCanonicalNode(node, finalNode) {
			return false
		}
		pin, node = finalPin, finalNode
		return true
	}
	ready := false
	readyRelay := func(bound net.Addr) error {
		if err := request.ready(bound); err != nil {
			return err
		}
		ready = true
		return nil
	}
	// Established flows end when their credentials are rotated or their
	// provider account is logged out, not only at disconnect.
	relayCtx, cancelRelay := context.WithCancel(ctx)
	defer cancelRelay()
	stopWatch := s.watchFlow(request.client, cancelRelay, request.routeUsername, request.authorized, node)
	defer stopWatch()
	if packets != nil {
		err = datagrams.RelayDatagrams(relayCtx, provider.DatagramRequest{
			Dial: s.dial, Control: request.client, Packets: packets, NodeHost: node.Host, NodePort: node.Port,
			Authority: pin.Authority.Data, HandshakeTimeout: s.handshakeTimeout, Admit: admit, Ready: readyRelay,
		})
	} else {
		err = transport.RelayStream(relayCtx, provider.RelayRequest{
			Dial: s.dial, Client: request.client, NodeHost: node.Host, NodePort: node.Port,
			Target: provider.Target{Host: request.destination.Host, Port: request.destination.Port}, Authority: pin.Authority.Data,
			HandshakeTimeout: s.handshakeTimeout, Admit: admit, Ready: readyRelay,
		})
	}
	if err != nil && !ready {
		request.fail(dialReply(err))
	}
	return err
}

func samePinAuthority(left, right state.TunnelPin) bool {
	return left.Node.Provider == right.Node.Provider && left.Node.AuthorityID == right.Node.AuthorityID && left.Authority.Protocol == right.Authority.Protocol &&
		subtle.ConstantTimeCompare(left.Authority.Data, right.Authority.Data) == 1
}

func sameCanonicalNode(left, right state.Node) bool {
	leftKey, leftErr := provider.NodeKey(left.Route())
	rightKey, rightErr := provider.NodeKey(right.Route())
	return leftErr == nil && rightErr == nil && leftKey == rightKey && left.AuthorityID == right.AuthorityID
}

func targetFromAddr(address net.Addr) target {
	switch value := address.(type) {
	case *net.TCPAddr:
		if value != nil && value.Port > 0 && value.Port <= 65535 {
			return target{Host: value.IP.String(), Port: uint16(value.Port)}
		}
	case *net.UDPAddr:
		if value != nil && value.Port > 0 && value.Port <= 65535 {
			return target{Host: value.IP.String(), Port: uint16(value.Port)}
		}
	}
	return target{}
}

func dialReply(err error) byte {
	if errors.Is(err, provider.ErrAddressUnsupported) {
		return replyAddressUnsupported
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return replyTTLExpired
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) && networkErr.Timeout() {
		return replyTTLExpired
	}
	if errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM) {
		return replyConnectionForbidden
	}
	if errors.Is(err, syscall.ENETUNREACH) {
		return replyNetworkUnreachable
	}
	if errors.Is(err, syscall.EHOSTUNREACH) {
		return replyHostUnreachable
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return replyConnectionRefused
	}
	return replyGeneralFailure
}

// credentialsCurrent re-authenticates a flow's credentials against registry.
// The automatic route also stops authenticating once it is disabled.
func (s *Server) credentialsCurrent(registry *credentialRegistry, username, password string, isSmart bool) bool {
	if isSmart {
		return registry.authenticateSmart(username, password) && s.smart != nil && s.smart.Enabled()
	}
	return registry.authenticate(username, password)
}

// beginHandshake admits a new connection unless its client IP already has
// maxHandshakesPerClient connections that have not finished authentication.
// The returned release is idempotent.
func (s *Server) beginHandshake(remote net.Addr) (func(), bool) {
	address, ok := remoteIP(remote)
	if !ok {
		return func() {}, true
	}
	s.handshakeMu.Lock()
	if s.handshakes[address] >= maxHandshakesPerClient {
		s.handshakeMu.Unlock()
		return nil, false
	}
	s.handshakes[address]++
	s.handshakeMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.handshakeMu.Lock()
			defer s.handshakeMu.Unlock()
			if s.handshakes[address] <= 1 {
				delete(s.handshakes, address)
			} else {
				s.handshakes[address]--
			}
		})
	}, true
}

func remoteIP(remote net.Addr) (netip.Addr, bool) {
	tcp, ok := remote.(*net.TCPAddr)
	if !ok || tcp == nil {
		return netip.Addr{}, false
	}
	address, ok := netip.AddrFromSlice(tcp.IP)
	return address.Unmap(), ok
}

// watchFlow cancels the transport and closes client once the flow's
// credentials stop authenticating or its node leaves the provider snapshot.
// Authority bytes rotate on every provider refresh, so they are not compared.
func (s *Server) watchFlow(client net.Conn, cancel context.CancelFunc, routeUsername string, authorized func(*credentialRegistry) bool, node state.Node) func() {
	interval := s.revalidateEvery
	if interval <= 0 {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if !s.routeAuthorized(routeUsername, authorized, node) {
					cancel()
					_ = client.Close()
					return
				}
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}

func (s *Server) flowAuthorized(username, password, routeUsername string, isSmart bool, node state.Node) bool {
	return s.routeAuthorized(routeUsername, func(registry *credentialRegistry) bool {
		return s.credentialsCurrent(registry, username, password, isSmart)
	}, node)
}

func (s *Server) routeAuthorized(routeUsername string, authorized func(*credentialRegistry) bool, node state.Node) bool {
	if !authorized(s.credentials.Load()) {
		return false
	}
	pin, err := s.snapshots.CompactPin(routeUsername, time.Now())
	return err == nil && pin.Node.TunnelEligible() && sameCanonicalNode(node, pin.Node)
}
