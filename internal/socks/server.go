package socks

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cenkalti/backoff/v5"

	credentialspkg "github.com/oshinop/chinadapter/internal/credentials"
	"github.com/oshinop/chinadapter/internal/provider"
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

// Server is a SOCKS5/RFC1929 frontend. Each immutable auth map is built from
// the current snapshot; established flows retain only a compact tunnel pin.
type Server struct {
	smart            SmartSelector
	snapshots        SnapshotSource
	providers        *provider.Registry
	dial             provider.DialContextFunc
	handshakeTimeout time.Duration
	slots            chan struct{}
	credentials      atomic.Pointer[credentialRegistry]
	revalidateEvery  time.Duration

	handshakeMu sync.Mutex
	handshakes  map[netip.Addr]int

	lifecycleMu  sync.Mutex
	listener     net.Listener
	shuttingDown bool
	active       map[net.Conn]context.CancelFunc
	drained      chan struct{}
}

type credentialRegistry struct {
	identity *credentialspkg.Identity
	nodes    map[string]struct{}
	smart    string
}

func (registry *credentialRegistry) authenticate(username, password string) bool {
	if registry == nil {
		return false
	}
	_, available := registry.nodes[username]
	return available && registry.identity.AuthenticatePassword(password)
}

// authenticateSmart accepts the automatic-route username, which lives in its
// own namespace and never appears in the node map.
func (registry *credentialRegistry) authenticateSmart(username, password string) bool {
	return registry != nil && registry.smart != "" && subtle.ConstantTimeCompare([]byte(username), []byte(registry.smart)) == 1 &&
		registry.identity.AuthenticatePassword(password)
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
	server := &Server{smart: config.Smart, snapshots: config.Snapshots, providers: config.Providers, dial: dial, handshakeTimeout: timeout, slots: make(chan struct{}, maxConnections), active: make(map[net.Conn]context.CancelFunc), drained: drained, handshakes: make(map[netip.Addr]int), revalidateEvery: defaultFlowRevalidation}
	if err := server.SetCredentials(config.Identity, config.InitialSnapshot); err != nil {
		return nil, err
	}
	return server, nil
}

// SetCredentials swaps one coherent live username map and instance password.
func (s *Server) SetCredentials(authority *credentialspkg.Identity, snapshot *state.RuntimeSnapshot) error {
	if s == nil || authority == nil {
		return errors.New("socks: installation identity is required")
	}
	nodeCount := 0
	if snapshot != nil {
		nodeCount = len(snapshot.Nodes)
	}
	next := &credentialRegistry{identity: authority, nodes: make(map[string]struct{}, nodeCount)}
	if smart, available := authority.SmartCredentials(); available {
		next.smart = smart.Username
	}
	if snapshot != nil {
		for _, node := range snapshot.Nodes {
			if !node.TunnelEligible() {
				continue
			}
			if _, available := snapshot.Providers[node.Provider]; !available {
				continue
			}
			credentials, available := authority.Credentials(node.Route())
			if !available {
				return credentialspkg.ErrInvalidKey
			}
			if _, duplicate := next.nodes[credentials.Username]; duplicate {
				return fmt.Errorf("%w: duplicate node route", state.ErrInvalidSnapshot)
			}
			next.nodes[credentials.Username] = struct{}{}
		}
	}
	s.credentials.Store(next)
	return nil
}

// Shutdown stops accepting new SOCKS connections and waits for active
// handlers to drain. When ctx expires, it cancels and closes every remaining
// handler so relays and in-flight setup cannot outlive the shutdown deadline.
func (s *Server) Shutdown(ctx context.Context) error {
	if s == nil || ctx == nil {
		return errors.New("socks: server and shutdown context are required")
	}
	s.lifecycleMu.Lock()
	s.shuttingDown = true
	listener := s.listener
	s.listener = nil
	drained := s.drained
	s.lifecycleMu.Unlock()
	if listener != nil {
		_ = listener.Close()
	}
	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		s.forceActiveHandlers()
		return ctx.Err()
	}
}

func (s *Server) beginServe(listener net.Listener) error {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.shuttingDown {
		return errors.New("socks: server is shutting down")
	}
	if s.listener != nil {
		return errors.New("socks: server is already serving")
	}
	s.listener = listener
	return nil
}

func (s *Server) endServe(listener net.Listener) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.listener == listener {
		s.listener = nil
	}
}

func (s *Server) accepting(listener net.Listener) bool {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	return !s.shuttingDown && s.listener == listener
}

func (s *Server) addActiveHandler(connection net.Conn, cancel context.CancelFunc) bool {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.shuttingDown {
		return false
	}
	if len(s.active) == 0 {
		s.drained = make(chan struct{})
	}
	s.active[connection] = cancel
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

func (s *Server) forceActiveHandlers() {
	s.lifecycleMu.Lock()
	handlers := make([]struct {
		connection net.Conn
		cancel     context.CancelFunc
	}, 0, len(s.active))
	for connection, cancel := range s.active {
		handlers = append(handlers, struct {
			connection net.Conn
			cancel     context.CancelFunc
		}{connection: connection, cancel: cancel})
	}
	s.lifecycleMu.Unlock()
	for _, handler := range handlers {
		handler.cancel()
		_ = handler.connection.Close()
	}
}

// Serve accepts connections until ctx is canceled. Accepted handlers retain
// independent contexts and must be drained with Shutdown.
func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	if s == nil || ctx == nil || listener == nil {
		return errors.New("socks: server, context, and listener are required")
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
		connection, err := listener.Accept()
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
			if !s.addActiveHandler(connection, cancel) {
				cancel()
				releaseHandshake()
				_ = connection.Close()
				<-s.slots
				continue
			}
			go func(connection net.Conn, handlerCtx context.Context, cancel context.CancelFunc) {
				defer func() { <-s.slots }()
				defer s.removeActiveHandler(connection)
				defer cancel()
				defer releaseHandshake()
				_ = s.handleConn(handlerCtx, connection, releaseHandshake)
			}(connection, handlerCtx, cancel)
		default:
			releaseHandshake()
			_ = connection.Close()
		}
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
	handlerDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = client.Close()
		case <-handlerDone:
		}
	}()
	defer close(handlerDone)
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
	node := pin.Node
	if err != nil || !node.TunnelEligible() {
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
	// Authentication may have completed before a client supplies CONNECT. Do
	// not let a stalled socket create a tunnel after logout, authority
	// refresh, selector rotation, preference exclusion, or node replacement.
	// Re-resolving returns a compact current pin and never clones a snapshot.
	now = time.Now()
	registry = s.credentials.Load()
	if registry == nil {
		_ = writeReply(client, replyGeneralFailure, target{})
		return state.ErrSelectorUnknown
	}
	if !s.credentialsCurrent(registry, username, password, isSmart) {
		_ = writeReply(client, replyGeneralFailure, target{})
		return state.ErrSelectorUnknown
	}
	revalidatedPin, resolveErr := s.snapshots.CompactPin(routeUsername, now)
	revalidatedNode := revalidatedPin.Node
	if resolveErr != nil || s.credentials.Load() != registry || !samePinAuthority(pin, revalidatedPin) || !revalidatedNode.TunnelEligible() || !sameCanonicalNode(node, revalidatedNode) {
		_ = writeReply(client, replyGeneralFailure, target{})
		if resolveErr != nil {
			return resolveErr
		}
		return state.ErrSelectorUnknown
	}
	pin, node = revalidatedPin, revalidatedNode
	if err := client.SetDeadline(time.Time{}); err != nil {
		_ = writeReply(client, replyGeneralFailure, target{})
		return fmt.Errorf("socks: clear client setup deadline: %w", err)
	}

	transport, err := s.providers.Transport(node.Protocol)
	if err != nil {
		_ = writeReply(client, replyGeneralFailure, target{})
		return err
	}
	var association *udpAssociation
	var datagrams provider.DatagramTransport
	if command == commandUDP {
		capable, ok := transport.(provider.DatagramTransport)
		if !ok {
			_ = writeReply(client, replyCommandUnsupported, target{})
			return nil
		}
		datagrams = capable
		association, err = newUDPAssociation(client, destination)
		if err != nil {
			code := replyGeneralFailure
			if errors.Is(err, errAddressTypeUnsupported) {
				code = replyAddressUnsupported
			}
			_ = writeReply(client, code, target{})
			return err
		}
		defer func() { _ = association.Close() }()
	}
	admit := func() bool {
		now := time.Now()
		if !s.snapshots.SessionCurrentPin(pin, now) {
			return false
		}
		currentRegistry := s.credentials.Load()
		if !s.credentialsCurrent(currentRegistry, username, password, isSmart) {
			return false
		}
		finalPin, resolveErr := s.snapshots.CompactPin(routeUsername, now)
		finalNode := finalPin.Node
		if resolveErr != nil || s.credentials.Load() != currentRegistry || !samePinAuthority(pin, finalPin) || !finalNode.TunnelEligible() || !sameCanonicalNode(node, finalNode) {
			return false
		}
		pin, node = finalPin, finalNode
		return true
	}
	ready := false
	readyRelay := func(bound net.Addr) error {
		if err := writeReply(client, replySucceeded, targetFromAddr(bound)); err != nil {
			return err
		}
		ready = true
		return nil
	}
	// Established flows end when their credentials are rotated or their
	// provider account is logged out, not only at disconnect.
	relayCtx, cancelRelay := context.WithCancel(ctx)
	defer cancelRelay()
	stopWatch := s.watchFlow(client, cancelRelay, username, password, routeUsername, isSmart, node)
	defer stopWatch()
	if association != nil {
		err = datagrams.RelayDatagrams(relayCtx, provider.DatagramRequest{
			Dial: s.dial, Control: client, Packets: association, NodeHost: node.Host, NodePort: node.Port,
			Authority: pin.Authority.Data, HandshakeTimeout: s.handshakeTimeout, Admit: admit, Ready: readyRelay,
		})
	} else {
		err = transport.RelayStream(relayCtx, provider.RelayRequest{
			Dial: s.dial, Client: client, NodeHost: node.Host, NodePort: node.Port,
			Target: provider.Target{Host: destination.Host, Port: destination.Port}, Authority: pin.Authority.Data,
			HandshakeTimeout: s.handshakeTimeout, Admit: admit, Ready: readyRelay,
		})
	}
	if err != nil && !ready {
		_ = writeReply(client, dialReply(err), target{})
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
func (s *Server) watchFlow(client net.Conn, cancel context.CancelFunc, username, password, routeUsername string, isSmart bool, node state.Node) func() {
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
				if !s.flowAuthorized(username, password, routeUsername, isSmart, node) {
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
	if !s.credentialsCurrent(s.credentials.Load(), username, password, isSmart) {
		return false
	}
	pin, err := s.snapshots.CompactPin(routeUsername, time.Now())
	return err == nil && pin.Node.TunnelEligible() && sameCanonicalNode(node, pin.Node)
}
