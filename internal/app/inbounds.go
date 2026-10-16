package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oshinop/chinadapter/internal/clientconfig"
	"github.com/oshinop/chinadapter/internal/socks"
	"github.com/oshinop/chinadapter/internal/state"
	"github.com/oshinop/chinadapter/internal/subscription"
	"github.com/oshinop/chinadapter/internal/web"
)

// InboundServer hosts the frontends of the optional inbound protocols;
// socks.Server implements it.
type InboundServer interface {
	ServeFrontend(context.Context, net.Listener, socks.Frontend) error
	CloseListener(net.Listener)
}

// InboundPreferenceStore persists the inbound choices.
type InboundPreferenceStore interface {
	Update(context.Context, func(*state.PersistentState) error) (state.PersistentState, error)
}

// InboundConfig wires the optional inbound listeners.
type InboundConfig struct {
	Server InboundServer
	Store  InboundPreferenceStore
	// Addresses are the listener addresses of the optional protocols.
	Addresses map[clientconfig.Protocol]string
	Ports     clientconfig.Ports
	Initial   state.InboundPreferences
	Listen    func(network, address string) (net.Listener, error)
}

type inboundListener struct {
	listener net.Listener
	done     chan struct{}
	stopped  atomic.Bool
}

// inboundStopWait bounds how long disabling a protocol waits for its accept
// loop, which ends as soon as its listener is closed.
const inboundStopWait = 5 * time.Second

// InboundManager owns the listeners of the optional inbound protocols. A
// protocol listens only while enabled; enabling binds its fixed port before
// the choice is persisted, so a port conflict is reported instead of saved.
type InboundManager struct {
	server    InboundServer
	store     InboundPreferenceStore
	addresses map[clientconfig.Protocol]string
	ports     clientconfig.Ports
	listen    func(network, address string) (net.Listener, error)
	current   atomic.Pointer[state.InboundPreferences]

	mu       sync.Mutex
	ctx      context.Context
	running  map[clientconfig.Protocol]*inboundListener
	failures map[clientconfig.Protocol]bool
}

var optionalInbounds = []struct {
	protocol clientconfig.Protocol
	frontend socks.Frontend
}{
	{clientconfig.Snell, socks.FrontendSnell},
	{clientconfig.Shadowsocks, socks.FrontendShadowsocks},
	{clientconfig.SOCKS5TLS, socks.FrontendSOCKSTLS},
}

func NewInboundManager(config InboundConfig) (*InboundManager, error) {
	if config.Server == nil || config.Store == nil {
		return nil, errors.New("app: inbound manager requires a server and a store")
	}
	for _, inbound := range optionalInbounds {
		if config.Addresses[inbound.protocol] == "" {
			return nil, fmt.Errorf("app: no listener address for %s", inbound.protocol)
		}
	}
	if config.Listen == nil {
		config.Listen = net.Listen
	}
	manager := &InboundManager{
		server: config.Server, store: config.Store, addresses: config.Addresses, ports: config.Ports, listen: config.Listen,
		running: make(map[clientconfig.Protocol]*inboundListener), failures: make(map[clientconfig.Protocol]bool),
	}
	initial := config.Initial
	manager.current.Store(&initial)
	return manager, nil
}

// Preferences returns the persisted choices without locking.
func (m *InboundManager) Preferences() state.InboundPreferences {
	if m == nil {
		return state.InboundPreferences{}
	}
	return *m.current.Load()
}

// Ports returns the inbound listener ports.
func (m *InboundManager) Ports() clientconfig.Ports { return m.ports }

// Run serves the enabled protocols until ctx ends. A port that cannot be
// bound is reported in Status; the daemon and the other protocols keep
// running.
func (m *InboundManager) Run(ctx context.Context) error {
	m.mu.Lock()
	m.ctx = ctx
	preferences := m.Preferences()
	for _, inbound := range optionalInbounds {
		if clientconfig.Enabled(inbound.protocol, preferences) {
			if err := m.startLocked(ctx, inbound.protocol, inbound.frontend); err != nil {
				slog.Warn("inbound listener unavailable", "protocol", inbound.protocol, "address", m.addresses[inbound.protocol], "error", err)
			}
		}
	}
	m.mu.Unlock()
	<-ctx.Done()
	m.mu.Lock()
	defer m.mu.Unlock()
	for protocol := range m.running {
		m.stopLocked(protocol)
	}
	m.ctx = nil
	return ctx.Err()
}

// startLocked binds and serves protocol. Its listener lives for runCtx, the
// manager's Run context, not for the request that enabled it.
func (m *InboundManager) startLocked(runCtx context.Context, protocol clientconfig.Protocol, frontend socks.Frontend) error {
	if running, ok := m.running[protocol]; ok && !closed(running.done) {
		return nil
	}
	listener, err := m.listen("tcp", m.addresses[protocol])
	if err != nil {
		m.failures[protocol] = true
		return err
	}
	delete(m.failures, protocol)
	slog.Info("inbound listening", "protocol", protocol, "address", listener.Addr().String())
	running := &inboundListener{listener: listener, done: make(chan struct{})}
	m.running[protocol] = running
	go func() {
		defer close(running.done)
		if err := m.server.ServeFrontend(runCtx, listener, frontend); err != nil && !running.stopped.Load() {
			slog.Warn("inbound listener stopped", "protocol", protocol, "error", err)
		}
	}()
	return nil
}

func (m *InboundManager) stopLocked(protocol clientconfig.Protocol) {
	running, ok := m.running[protocol]
	if !ok {
		return
	}
	delete(m.running, protocol)
	running.stopped.Store(true)
	m.server.CloseListener(running.listener)
	select {
	case <-running.done:
	case <-time.After(inboundStopWait):
	}
}

func closed(done chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

// ErrInboundListen means an enabled protocol's port could not be bound.
var ErrInboundListen = errors.New("app: inbound listener could not bind its port")

// Configure applies and persists next. Newly enabled protocols bind first;
// if any cannot, nothing changes. Disabled protocols stop accepting at once
// and their established flows end.
func (m *InboundManager) Configure(ctx context.Context, next state.InboundPreferences) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var started []clientconfig.Protocol
	rollback := func() {
		for _, protocol := range started {
			m.stopLocked(protocol)
		}
	}
	if m.ctx != nil {
		for _, inbound := range optionalInbounds {
			if _, running := m.running[inbound.protocol]; running || !clientconfig.Enabled(inbound.protocol, next) {
				continue
			}
			if err := m.startLocked(m.ctx, inbound.protocol, inbound.frontend); err != nil { //nolint:contextcheck // listeners outlive the request
				rollback()
				return &problemError{cause: fmt.Errorf("%w: %s: %w", ErrInboundListen, inbound.protocol, err), code: "inbound_listen_failed", status: http.StatusConflict}
			}
			started = append(started, inbound.protocol)
		}
	}
	if _, err := m.store.Update(ctx, func(persistent *state.PersistentState) error {
		persistent.Preferences.Inbounds = next
		return nil
	}); err != nil {
		rollback()
		return err
	}
	for _, inbound := range optionalInbounds {
		if !clientconfig.Enabled(inbound.protocol, next) {
			m.stopLocked(inbound.protocol)
			delete(m.failures, inbound.protocol)
		}
	}
	m.current.Store(&next)
	return nil
}

// Status reports every inbound protocol, SOCKS5 first.
func (m *InboundManager) Status() []web.InboundStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	preferences := m.Preferences()
	statuses := []web.InboundStatus{{Protocol: string(clientconfig.SOCKS5), Port: int(m.ports.SOCKS5), Enabled: true, Listening: true}}
	for _, inbound := range optionalInbounds {
		status := web.InboundStatus{Protocol: string(inbound.protocol), Port: int(m.ports.Port(inbound.protocol)), Enabled: clientconfig.Enabled(inbound.protocol, preferences)}
		if running, ok := m.running[inbound.protocol]; ok && !closed(running.done) {
			status.Listening = true
		} else if status.Enabled && (m.failures[inbound.protocol] || ok) {
			status.Problem = "listen_failed"
		}
		statuses = append(statuses, status)
	}
	return statuses
}

// NewInboundsAdapter exposes inbound status, configuration and per-route
// client profiles to web.
func NewInboundsAdapter(manager *InboundManager, credentials *CredentialCoordinator, snapshots *state.Manager) web.Inbounds {
	if manager == nil || credentials == nil || snapshots == nil {
		return nil
	}
	return inboundsAdapter{manager: manager, credentials: credentials, snapshots: snapshots}
}

type inboundsAdapter struct {
	manager     *InboundManager
	credentials *CredentialCoordinator
	snapshots   *state.Manager
}

func (a inboundsAdapter) Status() web.InboundsStatus {
	status := web.InboundsStatus{Inbounds: a.manager.Status()}
	if fingerprint, ok := a.credentials.Identity().TLSFingerprint(); ok {
		status.TLSFingerprint = fingerprint
	}
	return status
}

func (a inboundsAdapter) Configure(ctx context.Context, choices web.InboundChoices) error {
	return a.manager.Configure(ctx, state.InboundPreferences{Snell: choices.Snell, Shadowsocks: choices.Shadowsocks, SocksTLS: choices.SocksTLS})
}

func (a inboundsAdapter) NodeProfiles(nodeID, host string) ([]web.ProxyProfile, error) {
	current := a.snapshots.Current()
	if !state.SessionUsable(current, time.Now()) {
		return nil, errors.New("app: no usable node snapshot")
	}
	node, found := current.NodeByID(nodeID)
	if !found || !node.TunnelEligible() {
		return nil, errors.New("app: node not found")
	}
	identity := a.credentials.Identity()
	return a.profiles(func(protocol clientconfig.Protocol) (clientconfig.Profile, bool) {
		return clientconfig.RouteProfile(identity, node.Route(), protocol, node.Name, host, a.manager.Ports())
	}), nil
}

func (a inboundsAdapter) SmartProfiles(host string) ([]web.ProxyProfile, error) {
	identity := a.credentials.Identity()
	return a.profiles(func(protocol clientconfig.Protocol) (clientconfig.Profile, bool) {
		return clientconfig.SmartProfile(identity, protocol, subscription.SmartRouteName, host, a.manager.Ports())
	}), nil
}

func (a inboundsAdapter) profiles(render func(clientconfig.Protocol) (clientconfig.Profile, bool)) []web.ProxyProfile {
	preferences := a.manager.Preferences()
	var profiles []web.ProxyProfile
	for _, protocol := range clientconfig.Protocols {
		if !clientconfig.Enabled(protocol, preferences) {
			continue
		}
		profile, ok := render(protocol)
		if !ok {
			continue
		}
		uri, _ := clientconfig.URI(profile)
		profiles = append(profiles, web.ProxyProfile{Protocol: string(protocol), Port: int(profile.Port), Surge: clientconfig.Surge(profile), URI: uri})
	}
	return profiles
}
