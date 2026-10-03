package socks

import (
	"bytes"
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/kfadapter/kfadapter/internal/selector"
	"github.com/kfadapter/kfadapter/internal/state"
)

func TestRevokedFlowCancelsTransportAndReleasesSlot(t *testing.T) {
	for _, change := range []string{"logout", "credential rotation", "smart logout", "smart credential rotation", "smart disabled"} {
		for _, command := range []byte{commandConnect, commandUDP} {
			name := "TCP"
			if command == commandUDP {
				name = "UDP"
			}
			t.Run(change+"/"+name, func(t *testing.T) {
				ts := newTestServer(t, 1)
				ts.server.revalidateEvery = 5 * time.Millisecond
				credentials := ts.credential
				var smartRoute *automaticRoute
				if strings.HasPrefix(change, "smart ") {
					smartRoute = &automaticRoute{enabled: true, route: ts.credential.Selector}
					ts.server.smart = smartRoute
					credentials = ts.server.selectors.Load().SmartCredentials()
				}
				// This transport stays active until its context is canceled,
				// even if the SOCKS socket is closed. It models an idle peer.
				ts.transport.hold = make(chan struct{})
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				served := make(chan error, 1)
				go func() { served <- ts.server.Serve(ctx, listener) }()
				t.Cleanup(func() {
					cancel()
					_ = listener.Close()
					shutdown, stop := context.WithTimeout(context.Background(), time.Second)
					defer stop()
					_ = ts.server.Shutdown(shutdown)
					select {
					case <-served:
					case <-time.After(2 * time.Second):
						t.Error("SOCKS listener failed to stop")
					}
				})
				connect := func(credentials selector.Credentials) net.Conn {
					t.Helper()
					conn, err := net.Dial("tcp", listener.Addr().String())
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = conn.Close() })
					if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
						t.Fatal(err)
					}
					if status := authenticate(t, conn, credentials.Selector, credentials.Password); status != 0 {
						t.Fatalf("auth status = %d", status)
					}
					return conn
				}
				first := connect(credentials)
				destination := request(command, addressIPv4, []byte{1, 2, 3, 4}, 80)
				if command == commandUDP {
					destination = request(command, addressIPv4, []byte{0, 0, 0, 0}, 0)
				}
				mustWrite(t, first, destination)
				if code, _ := readReply(t, first); code != replySucceeded {
					t.Fatalf("reply = %d", code)
				}
				nextCredentials := credentials
				switch strings.TrimPrefix(change, "smart ") {
				case "logout":
					ts.snapshots.mu.Lock()
					ts.snapshots.revoked = true
					ts.snapshots.mu.Unlock()
				case "credential rotation":
					registry, err := selector.NewRegistry(state.SubscriptionAuthority{
						SelectorKey:  bytes.Repeat([]byte{1}, 32),
						ProxyAuthKey: bytes.Repeat([]byte{3}, 32),
						ActivatedAt:  time.Now().UTC(),
					})
					if err != nil {
						t.Fatal(err)
					}
					if err := ts.server.SetSelectors(registry); err != nil {
						t.Fatal(err)
					}
					var ok bool
					nextCredentials, ok = registry.Credentials(selector.NodeIdentity{NodeID: ts.snapshots.node.ID})
					if !ok {
						t.Fatal("rotated registry has no credentials")
					}
					if smartRoute != nil {
						nextCredentials = registry.SmartCredentials()
					}
				case "disabled":
					smartRoute.set(false, ts.credential.Selector)
				}
				if _, err := io.ReadFull(first, make([]byte, 1)); err == nil {
					t.Fatal("unauthorized client remained connected")
				}
				deadline := time.Now().Add(2 * time.Second)
				for len(ts.server.slots) != 0 && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
				if len(ts.server.slots) != 0 {
					t.Fatal("revoked transport retained the only connection slot")
				}
				ts.snapshots.mu.Lock()
				ts.snapshots.revoked = false
				ts.snapshots.mu.Unlock()
				if smartRoute != nil {
					smartRoute.set(true, ts.credential.Selector)
				}
				// The slot must be reusable without restarting the listener.
				second := connect(nextCredentials)
				_ = second.Close()
			})
		}
	}
}
