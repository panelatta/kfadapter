package quickfox

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/kfadapter/kfadapter/internal/provider"
)

// observedHandshakeConn lets a test cancel exactly at a blocked I/O operation
// or after successful I/O, before RelayStream can advance to the next phase.
type observedHandshakeConn struct {
	net.Conn
	beforeRead, afterRead   func()
	beforeWrite, afterWrite func()
	closed                  chan struct{}
	closeOnce               sync.Once
}

func (connection *observedHandshakeConn) Read(buffer []byte) (int, error) {
	if connection.beforeRead != nil {
		connection.beforeRead()
	}
	count, err := connection.Conn.Read(buffer)
	if err == nil && connection.afterRead != nil {
		connection.afterRead()
	}
	return count, err
}

func (connection *observedHandshakeConn) Write(buffer []byte) (int, error) {
	if connection.beforeWrite != nil {
		connection.beforeWrite()
	}
	count, err := connection.Conn.Write(buffer)
	if err == nil && connection.afterWrite != nil {
		connection.afterWrite()
	}
	return count, err
}

func (connection *observedHandshakeConn) Close() error {
	err := connection.Conn.Close()
	connection.closeOnce.Do(func() { close(connection.closed) })
	return err
}

func TestTransportCancellationInterruptsEveryHandshakePhase(t *testing.T) {
	for _, phase := range []string{
		"control preface blocked", "control ACK blocked", "data preface blocked",
		"control ACK completed", "data preface completed", "ready",
	} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			control, controlPeer := net.Pipe()
			data, dataPeer := net.Pipe()
			client, app := net.Pipe()
			for _, conn := range []net.Conn{control, controlPeer, data, dataPeer, client, app} {
				t.Cleanup(func() { _ = conn.Close() })
			}
			controlConn := &observedHandshakeConn{Conn: control, closed: make(chan struct{})}
			dataConn := &observedHandshakeConn{Conn: data, closed: make(chan struct{})}
			reached := make(chan struct{})
			var reachedOnce sync.Once
			notify := func() { reachedOnce.Do(func() { close(reached) }) }
			cancelAfterIO := func() {
				cancel()
				notify()
			}
			switch phase {
			case "control preface blocked":
				controlConn.beforeWrite = notify
			case "control ACK blocked":
				controlConn.beforeRead = notify
			case "data preface blocked":
				dataConn.beforeWrite = notify
			case "control ACK completed":
				controlConn.afterRead = cancelAfterIO
			case "data preface completed":
				dataConn.afterWrite = cancelAfterIO
			}
			authority := tunnelAuthority{Token: "0123456789abcdef0123456789abcdef", UserID: 15904241}
			encodedAuthority, err := json.Marshal(authority)
			if err != nil {
				t.Fatal(err)
			}
			packet, err := encodeTunnelAuth(authority, 0, 0x20, netip.Addr{}, 0)
			if err != nil {
				t.Fatal(err)
			}
			serverDone := make(chan struct{})
			go func() {
				defer close(serverDone)
				if phase == "control preface blocked" {
					return
				}
				if _, err := io.ReadFull(controlPeer, make([]byte, len(packet))); err != nil {
					return
				}
				if phase == "control ACK blocked" {
					return
				}
				if _, err := controlPeer.Write(make([]byte, controlACKSize)); err != nil {
					return
				}
				if phase == "control ACK completed" || phase == "data preface blocked" {
					return
				}
				_, _ = io.ReadFull(dataPeer, make([]byte, len(packet)))
			}()

			var dialed []*observedHandshakeConn
			ready := false
			done := make(chan error, 1)
			go func() {
				done <- NewTransport().RelayStream(ctx, provider.RelayRequest{
					Dial: func(context.Context, string, string) (net.Conn, error) {
						connection := controlConn
						if len(dialed) != 0 {
							connection = dataConn
						}
						dialed = append(dialed, connection)
						return connection, nil
					},
					Client: client, NodeHost: "relay.example", NodePort: 443,
					Target: provider.Target{Host: "8.8.8.8", Port: 443}, Authority: encodedAuthority,
					HandshakeTimeout: 5 * time.Second, Admit: func() bool { return true },
					Ready: func(net.Addr) error {
						ready = true
						if phase == "ready" {
							cancelAfterIO()
						}
						return nil
					},
				})
			}()
			select {
			case <-reached:
			case <-time.After(2 * time.Second):
				t.Fatal("handshake did not reach the test phase")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("handshake returned %v, want context.Canceled", err)
				}
			case <-time.After(time.Second):
				t.Fatal("cancellation waited for the handshake deadline")
			}
			if ready != (phase == "ready") {
				t.Fatalf("Ready called = %v during %s", ready, phase)
			}
			for _, connection := range dialed {
				select {
				case <-connection.closed:
				default:
					t.Fatal("canceled handshake retained an upstream connection")
				}
			}
			select {
			case <-serverDone:
			case <-time.After(time.Second):
				t.Fatal("cancellation left the handshake peer blocked")
			}
		})
	}
}
