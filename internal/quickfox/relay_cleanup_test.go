package quickfox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func cleanupTCPPair(t *testing.T) (*net.TCPConn, *net.TCPConn) {
	t.Helper()
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := listener.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	app, err := net.DialTCP("tcp", nil, listener.Addr().(*net.TCPAddr))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	connection, err := listener.AcceptTCP()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	for _, conn := range []*net.TCPConn{app, connection} {
		if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	return app, connection
}

func cleanupWire(data string) []byte {
	result := []byte(data)
	for index := range result {
		result[index] ^= relayXORByte
	}
	return result
}

func TestRelayXORLocalCloseReleasesIdlePeer(t *testing.T) {
	for _, action := range []string{"client", "upstream", "cancel"} {
		t.Run(action, func(t *testing.T) {
			app, client := cleanupTCPPair(t)
			upstream, peer := cleanupTCPPair(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- relayXOR(ctx, client, upstream) }()
			// Complete a transfer so both pumps are active, then leave both
			// remote endpoints open and idle while the local flow is revoked.
			if _, err := app.Write([]byte("ping")); err != nil {
				t.Fatal(err)
			}
			request := make([]byte, 4)
			if _, err := io.ReadFull(peer, request); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(request, cleanupWire("ping")) {
				t.Fatalf("request = %x", request)
			}
			switch action {
			case "client":
				if err := client.Close(); err != nil {
					t.Fatal(err)
				}
			case "upstream":
				if err := upstream.Close(); err != nil {
					t.Fatal(err)
				}
			case "cancel":
				cancel()
			}
			select {
			case err := <-done:
				if action == "cancel" && !errors.Is(err, context.Canceled) {
					t.Fatalf("canceled relay returned %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("local abort left the reverse copy blocked on an idle peer")
			}
			for _, conn := range []*net.TCPConn{app, peer} {
				var data [1]byte
				if _, err := conn.Read(data[:]); !errors.Is(err, io.EOF) {
					t.Fatalf("peer remained open after relay exit: %v", err)
				}
			}
		})
	}
}

func TestRelayXORTCPHalfCloseDrainsResponse(t *testing.T) {
	app, client := cleanupTCPPair(t)
	upstream, peer := cleanupTCPPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- relayXOR(ctx, client, upstream) }()
	if _, err := app.Write([]byte("request")); err != nil {
		t.Fatal(err)
	}
	if err := app.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	request, err := io.ReadAll(peer)
	if err != nil || !bytes.Equal(request, cleanupWire("request")) {
		t.Fatalf("request after half-close = %x, %v", request, err)
	}
	// The upstream may produce its response only after seeing the request EOF.
	if _, err := peer.Write(cleanupWire("response")); err != nil {
		t.Fatal(err)
	}
	if err := peer.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	response, err := io.ReadAll(app)
	if err != nil || string(response) != "response" {
		t.Fatalf("response after half-close = %q, %v", response, err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("relay returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("relay did not finish after both directions reached EOF")
	}
}
