package socks

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/netip"
	"syscall"
	"testing"
	"time"

	"github.com/oshinop/chinadapter/internal/credentials"
	"github.com/oshinop/chinadapter/internal/provider"
	"github.com/oshinop/chinadapter/internal/shadowsocks"
	"github.com/oshinop/chinadapter/internal/snell"
)

// echoTransport answers each stream with "<target host>:<upload>" after the
// client half-closes, then half-closes itself.
func echoTransport() *frontendStreamTransport {
	return &frontendStreamTransport{relay: func(_ context.Context, request provider.RelayRequest) error {
		if !request.Admit() {
			return provider.ErrSessionRejected
		}
		if err := request.Ready(&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}); err != nil {
			return err
		}
		upload, err := io.ReadAll(request.Client)
		if err != nil {
			return err
		}
		if _, err := request.Client.Write(append([]byte(request.Target.Host+":"), upload...)); err != nil {
			return err
		}
		return request.Client.(interface{ CloseWrite() error }).CloseWrite()
	}}
}

func (fixture *frontendFixture) lastSelector() string {
	if selector := fixture.snapshots.selector.Load(); selector != nil {
		return *selector
	}
	return ""
}

func dialSnell(t *testing.T, fixture *frontendFixture, psk string) *snell.Conn {
	t.Helper()
	client, err := snell.Client(fixture.connect(t), []byte(psk))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func readSnellSession(t *testing.T, client *snell.Conn) []byte {
	t.Helper()
	var response []byte
	buffer := make([]byte, 4096)
	for {
		n, err := client.Read(buffer)
		response = append(response, buffer[:n]...)
		if errors.Is(err, snell.ErrZeroChunk) {
			return response
		}
		if err != nil {
			t.Fatalf("read Snell response: %v", err)
		}
	}
}

func nodePSK(t *testing.T, fixture *frontendFixture) string {
	t.Helper()
	psk, ok := fixture.identity.SnellPSK(fixture.snapshots.pin.Node.Route())
	if !ok {
		t.Fatal("fixture route has no Snell PSK")
	}
	return psk
}

func TestSnellFrontendReusesConnectionAcrossRequests(t *testing.T) {
	transport := echoTransport()
	fixture := newProtocolFixture(t, transport, 0, nil, nil, FrontendSnell)
	client := dialSnell(t, fixture, nodePSK(t, fixture))
	for round := range 3 {
		upload := bytes.Repeat([]byte{byte('a' + round)}, 20000*(round+1))
		if err := client.WriteStream(snell.AppendConnectRequest(nil, "www.bilibili.com", 443, true), upload); err != nil {
			t.Fatal(err)
		}
		if err := client.WriteZeroChunk(); err != nil {
			t.Fatal(err)
		}
		response := readSnellSession(t, client)
		want := append([]byte{snell.ResponseTunnel}, append([]byte("www.bilibili.com:"), upload...)...)
		if !bytes.Equal(response, want) {
			t.Fatalf("round %d response = %d bytes, want %d", round, len(response), len(want))
		}
	}
	if transport.calls.Load() != 3 || fixture.lastSelector() != fixture.credentials.Username {
		t.Fatalf("transport calls = %d, selector = %q", transport.calls.Load(), fixture.lastSelector())
	}
}

func TestSnellFrontendRejectsUnknownPSKWithoutDispatch(t *testing.T) {
	transport := echoTransport()
	fixture := newProtocolFixture(t, transport, 0, nil, nil, FrontendSnell)
	client := dialSnell(t, fixture, "not-a-route-psk")
	if err := client.WriteStream(snell.AppendConnectRequest(nil, "example.com", 80, true), []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ReadRecord(); err == nil {
		t.Fatal("unknown PSK received a response")
	}
	if transport.calls.Load() != 0 || fixture.snapshots.calls.Load() != 0 {
		t.Fatal("unknown PSK reached route resolution")
	}
}

func TestSnellFrontendReportsTransportFailure(t *testing.T) {
	transport := &frontendStreamTransport{relay: func(context.Context, provider.RelayRequest) error {
		return syscall.ECONNREFUSED
	}}
	fixture := newProtocolFixture(t, transport, 0, nil, nil, FrontendSnell)
	client := dialSnell(t, fixture, nodePSK(t, fixture))
	if err := client.WriteStream(snell.AppendConnectRequest(nil, "example.com", 80, true), []byte("x")); err != nil {
		t.Fatal(err)
	}
	response, err := client.ReadRecord()
	if err != nil || !bytes.Equal(response, snell.ErrorResponse(replyConnectionRefused, "connection refused")) {
		t.Fatalf("response = %q, %v", response, err)
	}
	if _, err := client.ReadRecord(); err == nil {
		t.Fatal("connection stayed open after a failed request")
	}
}

func TestSnellFrontendSmartRoute(t *testing.T) {
	route := &automaticRoute{}
	transport := echoTransport()
	fixture := newProtocolFixture(t, transport, 0, nil, func(server *Server) { server.smart = route }, FrontendSnell)
	psk, ok := fixture.identity.SmartSnellPSK()
	if !ok {
		t.Fatal("no smart PSK")
	}
	disabled := dialSnell(t, fixture, psk)
	if err := disabled.WriteStream(snell.AppendConnectRequest(nil, "example.com", 80, false), []byte("x")); err != nil {
		t.Fatal(err)
	}
	if response, err := disabled.ReadRecord(); err != nil || len(response) == 0 || response[0] != snell.ResponseError {
		t.Fatalf("disabled smart route response = %q, %v", response, err)
	}

	route.set(true, fixture.credentials.Username)
	client := dialSnell(t, fixture, psk)
	if err := client.WriteStream(snell.AppendConnectRequest(nil, "example.com", 80, false), []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := client.WriteZeroChunk(); err != nil {
		t.Fatal(err)
	}
	if response := readSnellSession(t, client); string(response) != "\x00example.com:x" {
		t.Fatalf("smart response = %q", response)
	}
	if fixture.lastSelector() != fixture.credentials.Username {
		t.Fatalf("smart route resolved %q", fixture.lastSelector())
	}
}

func TestSnellFrontendRelaysDatagramsOverTCP(t *testing.T) {
	packets := make(chan []byte, 8)
	fixture := newProtocolFixture(t, newFrontendDatagramEcho(packets), 0, nil, nil, FrontendSnell)
	client := dialSnell(t, fixture, nodePSK(t, fixture))
	if err := client.WriteRecord([]byte{snell.Version, snell.CommandUDP, 0}); err != nil {
		t.Fatal(err)
	}
	if reply, err := client.ReadRecord(); err != nil || !bytes.Equal(reply, []byte{snell.ResponseTunnel}) {
		t.Fatalf("UDP reply = %x, %v", reply, err)
	}
	destination := netip.MustParseAddrPort("1.2.3.4:53")
	if err := client.WriteRecord(snell.AppendDatagram(nil, destination, []byte("query"))); err != nil {
		t.Fatal(err)
	}
	receiveFrontendPacket(t, packets, append([]byte{0, 0, 0, addressIPv4, 1, 2, 3, 4, 0, 53}, "query"...))
	response, err := client.ReadRecord()
	if err != nil || !bytes.Equal(response, snell.AppendDatagramResponse(nil, destination, []byte("query"))) {
		t.Fatalf("UDP response = %x, %v", response, err)
	}
	// A domain datagram reaches the transport as a SOCKS domain request; its
	// echo has no IP source and is dropped instead of breaking the session.
	domain := append([]byte{1, 11}, append([]byte("example.com"), 0, 53, 'd')...)
	if err := client.WriteRecord(domain); err != nil {
		t.Fatal(err)
	}
	receiveFrontendPacket(t, packets, append(append([]byte{0, 0, 0, addressDomain, 11}, "example.com"...), 0, 53, 'd'))
	if err := client.WriteRecord(snell.AppendDatagram(nil, destination, []byte("again"))); err != nil {
		t.Fatal(err)
	}
	receiveFrontendPacket(t, packets, append([]byte{0, 0, 0, addressIPv4, 1, 2, 3, 4, 0, 53}, "again"...))
	if response, err := client.ReadRecord(); err != nil || !bytes.Equal(response, snell.AppendDatagramResponse(nil, destination, []byte("again"))) {
		t.Fatalf("UDP response after domain = %x, %v", response, err)
	}
}

func TestSnellFlowEndsWhenCredentialsRotate(t *testing.T) {
	canceled := make(chan struct{}, 1)
	transport := holdingTransport(canceled)
	fixture := newProtocolFixture(t, transport, 0, nil, func(server *Server) {
		server.revalidateEvery = 5 * time.Millisecond
	}, FrontendSnell)
	client := dialSnell(t, fixture, nodePSK(t, fixture))
	if err := client.WriteStream(snell.AppendConnectRequest(nil, "example.com", 80, true), []byte("x")); err != nil {
		t.Fatal(err)
	}
	waitForRelay(t, transport)
	rotated, err := credentials.New(bytes.Repeat([]byte{0x43}, 32), "00112233445566778899aabbccddeeff")
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.server.SetCredentials(rotated, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-canceled:
	case <-time.After(frontendTestTimeout):
		t.Fatal("rotated Snell flow kept its transport")
	}
}

func TestShadowsocksFrontendRelaysAndRejects(t *testing.T) {
	transport := echoTransport()
	fixture := newProtocolFixture(t, transport, 0, nil, nil, FrontendShadowsocks)
	serverKey, _ := fixture.identity.ShadowsocksServerKey()
	userKey, _ := fixture.identity.ShadowsocksUserKey(fixture.snapshots.pin.Node.Route())
	client, err := shadowsocks.NewClient(fixture.connect(t), serverKey, userKey, shadowsocks.Target{Host: "upos-sz-mirrorcos.bilivideo.com", Port: 443})
	if err != nil {
		t.Fatal(err)
	}
	upload := bytes.Repeat([]byte("segment"), 20000)
	if _, err := client.Write(upload); err != nil {
		t.Fatal(err)
	}
	if err := client.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	response, err := io.ReadAll(client)
	if err != nil || !bytes.Equal(response, append([]byte("upos-sz-mirrorcos.bilivideo.com:"), upload...)) {
		t.Fatalf("response = %d bytes, %v", len(response), err)
	}
	if fixture.lastSelector() != fixture.credentials.Username {
		t.Fatalf("selector = %q", fixture.lastSelector())
	}

	stranger, err := shadowsocks.NewClient(fixture.connect(t), serverKey, bytes.Repeat([]byte{9}, 16), shadowsocks.Target{Host: "example.com", Port: 80})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stranger.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := stranger.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if data, err := io.ReadAll(stranger); err != nil || len(data) != 0 {
		t.Fatalf("unknown user received %d bytes, %v", len(data), err)
	}
	if transport.calls.Load() != 1 {
		t.Fatalf("transport calls = %d", transport.calls.Load())
	}
}

func TestSOCKSTLSFrontendPinsDerivedCertificate(t *testing.T) {
	transport := echoTransport()
	fixture := newProtocolFixture(t, transport, 0, nil, nil, FrontendSOCKSTLS)
	fingerprint, ok := fixture.identity.TLSFingerprint()
	if !ok {
		t.Fatal("no TLS fingerprint")
	}
	connection := tls.Client(fixture.connect(t), &tls.Config{
		InsecureSkipVerify: true, // the client pins the fingerprint below, as Surge does
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			digest := sha256.Sum256(raw[0])
			if hex.EncodeToString(digest[:]) != fingerprint {
				return errors.New("fingerprint mismatch")
			}
			return nil
		},
	})
	if status := authenticateFrontend(t, connection, fixture.credentials.Username, fixture.credentials.Password); status != 0 {
		t.Fatalf("authentication status = %x", status)
	}
	sendFrontendRequest(t, connection, commandConnect, target{Host: "example.com", Port: 443})
	readFrontendReply(t, connection, replySucceeded)
	if _, err := connection.Write([]byte("over tls")); err != nil {
		t.Fatal(err)
	}
	if err := connection.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if response, err := io.ReadAll(connection); err != nil || string(response) != "example.com:over tls" {
		t.Fatalf("response = %q, %v", response, err)
	}
}

func TestCloseListenerEndsOnlyItsFlows(t *testing.T) {
	canceled := make(chan struct{}, 1)
	transport := holdingTransport(canceled)
	fixture := newProtocolFixture(t, transport, 0, nil, nil, FrontendSOCKS)
	snellListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- fixture.server.ServeFrontend(context.Background(), snellListener, FrontendSnell) }()
	socksFlow := connectFrontend(t, fixture, fixture.credentials.Username, fixture.credentials.Password)

	raw, err := net.DialTimeout("tcp4", snellListener.Addr().String(), frontendTestTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(frontendTestTimeout))
	client, _ := snell.Client(raw, []byte(nodePSK(t, fixture)))
	if err := client.WriteStream(snell.AppendConnectRequest(nil, "example.com", 80, true), []byte("x")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(frontendTestTimeout)
	for transport.calls.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("Snell request never reached the transport")
		}
		time.Sleep(time.Millisecond)
	}
	fixture.server.CloseListener(snellListener)
	select {
	case <-canceled:
	case <-time.After(frontendTestTimeout):
		t.Fatal("closing the Snell listener did not end its flow")
	}
	if err := <-served; err != nil {
		t.Fatalf("ServeFrontend: %v", err)
	}
	if _, err := client.ReadRecord(); err == nil {
		t.Fatal("Snell connection stayed open")
	}
	_ = socksFlow.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	var one [1]byte
	if _, err := socksFlow.Read(one[:]); !isTimeout(err) {
		t.Fatalf("SOCKS flow ended with the Snell listener: %v", err)
	}
	// The holding transport outlives a closed socket; end it for cleanup.
	fixture.server.CloseListener(fixture.listener)
	<-canceled
}

func waitForRelay(t *testing.T, transport *frontendStreamTransport) {
	t.Helper()
	deadline := time.Now().Add(frontendTestTimeout)
	for transport.calls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("request never reached the transport")
		}
		time.Sleep(time.Millisecond)
	}
}

func isTimeout(err error) bool {
	networkErr, ok := errors.AsType[net.Error](err)
	return ok && networkErr.Timeout()
}
