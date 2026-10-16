package app

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/oshinop/chinadapter/internal/clientconfig"
	"github.com/oshinop/chinadapter/internal/socks"
	"github.com/oshinop/chinadapter/internal/state"
)

type fakeInboundServer struct {
	mu     sync.Mutex
	served map[net.Listener]socks.Frontend
	closed int
}

func (f *fakeInboundServer) ServeFrontend(_ context.Context, listener net.Listener, frontend socks.Frontend) error {
	f.mu.Lock()
	f.served[listener] = frontend
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		delete(f.served, listener)
		f.mu.Unlock()
	}()
	for {
		connection, err := listener.Accept()
		if err != nil {
			return nil
		}
		_ = connection.Close()
	}
}

func (f *fakeInboundServer) CloseListener(listener net.Listener) {
	f.mu.Lock()
	f.closed++
	f.mu.Unlock()
	_ = listener.Close()
}

func (f *fakeInboundServer) frontends() map[socks.Frontend]bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := make(map[socks.Frontend]bool)
	for _, frontend := range f.served {
		result[frontend] = true
	}
	return result
}

type fakePreferenceStore struct {
	mu      sync.Mutex
	saved   state.PersistentState
	updates int
	err     error
}

func (f *fakePreferenceStore) Update(_ context.Context, change func(*state.PersistentState) error) (state.PersistentState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return state.PersistentState{}, f.err
	}
	next := f.saved
	if err := change(&next); err != nil {
		return state.PersistentState{}, err
	}
	f.saved = next
	f.updates++
	return next, nil
}

func newTestInboundManager(t *testing.T, initial state.InboundPreferences, blocked map[string]bool) (*InboundManager, *fakeInboundServer, *fakePreferenceStore) {
	t.Helper()
	server := &fakeInboundServer{served: make(map[net.Listener]socks.Frontend)}
	store := &fakePreferenceStore{}
	manager, err := NewInboundManager(InboundConfig{
		Server: server, Store: store, Initial: initial,
		Ports: clientconfig.Ports{SOCKS5: 10808, Snell: 10810, Shadowsocks: 10811, SOCKS5TLS: 10812},
		Addresses: map[clientconfig.Protocol]string{
			clientconfig.Snell: "snell", clientconfig.Shadowsocks: "shadowsocks", clientconfig.SOCKS5TLS: "tls",
		},
		Listen: func(_, address string) (net.Listener, error) {
			if blocked[address] {
				return nil, errors.New("address already in use")
			}
			return net.Listen("tcp4", "127.0.0.1:0")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return manager, server, store
}

func runInbounds(t *testing.T, manager *InboundManager) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = manager.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		manager.mu.Lock()
		started := manager.ctx != nil
		manager.mu.Unlock()
		if started {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("inbound manager did not start")
		}
		time.Sleep(time.Millisecond)
	}
}

func waitForFrontends(t *testing.T, server *fakeInboundServer, want map[socks.Frontend]bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := server.frontends()
		if len(got) == len(want) {
			matched := true
			for frontend := range want {
				matched = matched && got[frontend]
			}
			if matched {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("served frontends = %v, want %v", got, want)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestInboundManagerStartsEnabledProtocolsAndReportsBindFailures(t *testing.T) {
	manager, server, _ := newTestInboundManager(t, state.InboundPreferences{Snell: true, SocksTLS: true}, map[string]bool{"tls": true})
	runInbounds(t, manager)
	waitForFrontends(t, server, map[socks.Frontend]bool{socks.FrontendSnell: true})
	statuses := manager.Status()
	if len(statuses) != 4 || statuses[0].Protocol != "socks5" || !statuses[0].Listening {
		t.Fatalf("statuses = %+v", statuses)
	}
	for _, status := range statuses[1:] {
		switch status.Protocol {
		case "snell":
			if !status.Enabled || !status.Listening || status.Problem != "" || status.Port != 10810 {
				t.Errorf("snell = %+v", status)
			}
		case "shadowsocks":
			if status.Enabled || status.Listening || status.Problem != "" {
				t.Errorf("shadowsocks = %+v", status)
			}
		case "socks5-tls":
			if !status.Enabled || status.Listening || status.Problem != "listen_failed" {
				t.Errorf("socks5-tls = %+v", status)
			}
		}
	}
}

func TestInboundManagerConfigureBindsBeforePersisting(t *testing.T) {
	manager, server, store := newTestInboundManager(t, state.InboundPreferences{}, map[string]bool{"shadowsocks": true})
	runInbounds(t, manager)
	if err := manager.Configure(t.Context(), state.InboundPreferences{Snell: true, SocksTLS: true}); err != nil {
		t.Fatal(err)
	}
	waitForFrontends(t, server, map[socks.Frontend]bool{socks.FrontendSnell: true, socks.FrontendSOCKSTLS: true})
	if store.saved.Preferences.Inbounds != (state.InboundPreferences{Snell: true, SocksTLS: true}) || manager.Preferences() != store.saved.Preferences.Inbounds {
		t.Fatalf("persisted = %+v, live = %+v", store.saved.Preferences.Inbounds, manager.Preferences())
	}

	// A port conflict leaves both the listeners and the stored choice alone.
	err := manager.Configure(t.Context(), state.InboundPreferences{Snell: true, Shadowsocks: true, SocksTLS: true})
	var problem *problemError
	if !errors.As(err, &problem) || problem.HTTPStatus() != http.StatusConflict || problem.Code() != "inbound_listen_failed" || !errors.Is(err, ErrInboundListen) {
		t.Fatalf("conflicting configure = %v", err)
	}
	if store.updates != 1 || manager.Preferences() != (state.InboundPreferences{Snell: true, SocksTLS: true}) {
		t.Fatalf("failed configure changed preferences: %+v", manager.Preferences())
	}

	// A failed write rolls back listeners the call opened.
	store.err = errors.New("disk full")
	server.mu.Lock()
	closedBefore := server.closed
	server.mu.Unlock()
	manager.addresses[clientconfig.Shadowsocks] = "available"
	if err := manager.Configure(t.Context(), state.InboundPreferences{Snell: true, Shadowsocks: true, SocksTLS: true}); err == nil {
		t.Fatal("configure succeeded without persisting")
	}
	server.mu.Lock()
	rolledBack := server.closed - closedBefore
	server.mu.Unlock()
	if rolledBack != 1 {
		t.Fatalf("rollback closed %d listeners", rolledBack)
	}
	store.err = nil

	if err := manager.Configure(t.Context(), state.InboundPreferences{}); err != nil {
		t.Fatal(err)
	}
	waitForFrontends(t, server, map[socks.Frontend]bool{})
	for _, status := range manager.Status()[1:] {
		if status.Enabled || status.Listening {
			t.Fatalf("disabled protocol still served: %+v", status)
		}
	}
}

func TestInboundManagerConfigureBeforeRunOnlyPersists(t *testing.T) {
	manager, server, store := newTestInboundManager(t, state.InboundPreferences{}, nil)
	if err := manager.Configure(t.Context(), state.InboundPreferences{Shadowsocks: true}); err != nil {
		t.Fatal(err)
	}
	if store.updates != 1 || len(server.frontends()) != 0 {
		t.Fatal("configure before Run bound a listener")
	}
	runInbounds(t, manager)
	waitForFrontends(t, server, map[socks.Frontend]bool{socks.FrontendShadowsocks: true})
}
