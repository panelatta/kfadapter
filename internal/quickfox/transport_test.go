package quickfox

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/kfadapter/kfadapter/internal/provider"
)

func TestEncodeTunnelAuthGolden(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 21, 21, 53, 20, 0, time.UTC)
	packet, err := encodeTunnelAuth(
		tunnelAuthority{Token: "0123456789abcdef0123456789abcdef", UserID: 15904241},
		nativeNonce(now), 0x10, netip.MustParseAddr("34.160.111.145"), 80,
	)
	if err != nil {
		t.Fatal(err)
	}
	const want = "47490408234330313233343536373839616263646566303132333435363738396162636465662c30f1adf200664400000b14000422a06f910050ffffffff"
	if got := hex.EncodeToString(packet); got != want {
		t.Fatalf("tunnel packet = %s, want %s", got, want)
	}
}

func TestTransportRelayStream(t *testing.T) {
	t.Parallel()
	transport := NewTransport()
	transport.clock = func() time.Time { return time.Date(2026, 7, 21, 21, 53, 20, 0, time.UTC) }
	transport.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("34.160.111.145")}, nil
	}
	authority, err := json.Marshal(tunnelAuthority{Token: "0123456789abcdef0123456789abcdef", UserID: 15904241})
	if err != nil {
		t.Fatal(err)
	}
	client, adapter := net.Pipe()
	defer client.Close()
	connections := make(chan net.Conn, 2)
	dial := func(context.Context, string, string) (net.Conn, error) {
		adapterSide, relaySide := net.Pipe()
		connections <- relaySide
		return adapterSide, nil
	}
	serverErr := make(chan error, 1)
	go func() {
		control := <-connections
		defer control.Close()
		controlPacket := make([]byte, 62)
		if _, err := io.ReadFull(control, controlPacket); err != nil {
			serverErr <- err
			return
		}
		if controlPacket[len(controlPacket)-13] != 0x24 {
			serverErr <- errors.New("unexpected control mode")
			return
		}
		if _, err := control.Write(make([]byte, controlACKSize)); err != nil {
			serverErr <- err
			return
		}
		data := <-connections
		defer data.Close()
		dataPacket := make([]byte, 62)
		if _, err := io.ReadFull(data, dataPacket); err != nil {
			serverErr <- err
			return
		}
		if dataPacket[len(dataPacket)-13] != 0x14 {
			serverErr <- errors.New("unexpected data mode")
			return
		}
		encoded := make([]byte, 4)
		if _, err := io.ReadFull(data, encoded); err != nil {
			serverErr <- err
			return
		}
		for index := range encoded {
			encoded[index] ^= relayXORByte
		}
		if string(encoded) != "ping" {
			serverErr <- errors.New("unexpected relayed request")
			return
		}
		response := []byte("pong")
		for index := range response {
			response[index] ^= relayXORByte
		}
		_, err := data.Write(response)
		serverErr <- err
	}()

	ready := make(chan struct{})
	relayErr := make(chan error, 1)
	go func() {
		relayErr <- transport.RelayStream(context.Background(), provider.RelayRequest{
			NodeHost: "relay.example", NodePort: 443, Target: provider.Target{Host: "target.example", Port: 80},
			Authority: authority, Client: adapter, Dial: dial, HandshakeTimeout: time.Second,
			Admit: func() bool { return true }, Ready: func(net.Addr) error { close(ready); return nil },
		})
	}()
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("relay did not become ready")
	}
	if _, err := client.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, 4)
	if _, err := io.ReadFull(client, response); err != nil {
		t.Fatal(err)
	}
	if string(response) != "pong" {
		t.Fatalf("relay response = %q", response)
	}
	_ = client.Close()
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
	if err := <-relayErr; err != nil {
		t.Fatal(err)
	}
}

func TestTransportRejectsDatagrams(t *testing.T) {
	t.Parallel()
	if err := NewTransport().RelayDatagrams(context.Background(), provider.DatagramRequest{}); err == nil {
		t.Fatal("RelayDatagrams unexpectedly succeeded")
	}
}

func TestResolveTargetSkipsNonPublicAnswers(t *testing.T) {
	transport := NewTransport()
	answers := []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("10.0.0.8"), netip.MustParseAddr("100.64.1.1"), netip.MustParseAddr("34.160.111.145")}
	transport.lookup = func(context.Context, string, string) ([]netip.Addr, error) { return answers, nil }
	address, err := transport.resolveTarget(context.Background(), "example.com", time.Second)
	if err != nil || address.String() != "34.160.111.145" {
		t.Fatalf("resolved %v, %v", address, err)
	}
	answers = answers[:3]
	if _, err := transport.resolveTarget(context.Background(), "localhost", time.Second); err == nil {
		t.Fatal("private-only answers were accepted")
	}
	if _, err := transport.resolveTarget(context.Background(), "2001:db8::1", time.Second); !errors.Is(err, provider.ErrAddressUnsupported) {
		t.Fatalf("IPv6 literal error = %v", err)
	}
	if address, err := transport.resolveTarget(context.Background(), "192.168.1.1", time.Second); err != nil || address.String() != "192.168.1.1" {
		t.Fatalf("explicit literal target = %v, %v", address, err)
	}
}

func TestRelayXORTransformsBothDirectionsAndHalfCloses(t *testing.T) {
	clientApp, clientSide := net.Pipe()
	upstreamSide, relay := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- relayXOR(context.Background(), clientSide, upstreamSide) }()

	go func() { _, _ = clientApp.Write([]byte("ping")) }()
	encoded := make([]byte, 4)
	if _, err := io.ReadFull(relay, encoded); err != nil {
		t.Fatal(err)
	}
	for index, value := range []byte("ping") {
		if encoded[index] != value^relayXORByte {
			t.Fatalf("upstream bytes = %x", encoded)
		}
	}
	reply := []byte("pong")
	for index := range reply {
		reply[index] ^= relayXORByte
	}
	go func() { _, _ = relay.Write(reply) }()
	decoded := make([]byte, 4)
	if _, err := io.ReadFull(clientApp, decoded); err != nil || string(decoded) != "pong" {
		t.Fatalf("client bytes = %q, %v", decoded, err)
	}
	_ = relay.Close()
	_ = clientApp.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("relay did not finish after both sides closed")
	}
}

func TestRelayXORStopsOnCancellation(t *testing.T) {
	clientApp, clientSide := net.Pipe()
	upstreamSide, relay := net.Pipe()
	defer clientApp.Close()
	defer relay.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- relayXOR(ctx, clientSide, upstreamSide) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("relay ignored cancellation")
	}
}
