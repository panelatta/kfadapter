package quickfox

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"strconv"
	"testing"
	"testing/synctest"
	"time"
)

// nativeRelay is a loopback stand-in for a QuickFox relay in native datagram
// mode: a control TCP connection and the UDP port its acknowledgement names.
type nativeRelay struct {
	control *net.TCPConn
	udp     *net.UDPConn
	dials   chan string
}

func newNativeRelay(t *testing.T) (*nativeRelay, func(context.Context, string, string) (net.Conn, error)) {
	t.Helper()
	relay, relayAdapter := loopbackTCPPair(t)
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = udp.Close() })
	_ = udp.SetDeadline(time.Now().Add(5 * time.Second))
	native := &nativeRelay{control: relay, udp: udp, dials: make(chan string, 2)}
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		native.dials <- network + " " + address
		if network == "tcp" {
			return relayAdapter, nil
		}
		var dialer net.Dialer
		return dialer.DialContext(ctx, network, address)
	}
	return native, dial
}

func (relay *nativeRelay) port() uint16 { return uint16(relay.udp.LocalAddr().(*net.UDPAddr).Port) }

func (relay *nativeRelay) acknowledgement(port uint16) []byte {
	ack := observedControlACK
	binary.BigEndian.PutUint16(ack[8:10], port)
	return ack[:]
}

func (relay *nativeRelay) readDatagram(t *testing.T) ([]byte, *net.UDPAddr) {
	t.Helper()
	buffer := make([]byte, 4096)
	n, source, err := relay.udp.ReadFromUDP(buffer)
	if err != nil {
		t.Fatal(err)
	}
	return buffer[:n], source
}

func nativeTransport(t *testing.T) *Transport {
	t.Helper()
	transport := NewTransport()
	transport.EnableNativeUDP()
	transport.lookup = func(_ context.Context, network, host string) ([]netip.Addr, error) {
		if network != "ip4" || host != "dns.example" {
			return nil, errors.New("unexpected lookup")
		}
		return []netip.Addr{netip.MustParseAddr("192.0.2.9")}, nil
	}
	return transport
}

func TestRelayNativeDatagramsUsesRelayUDPPort(t *testing.T) {
	t.Parallel()
	controlClient, control := loopbackTCPPair(t)
	relay, dial := newNativeRelay(t)
	packets := newTestPacketIO()
	ready := make(chan net.Addr, 1)
	done := make(chan error, 1)
	go func() {
		request := nativeDatagramRequest(t, control, packets, dial)
		request.Ready = func(address net.Addr) error { ready <- address; return nil }
		done <- nativeTransport(t).RelayDatagrams(context.Background(), request)
	}()

	var preface [62]byte
	if _, err := io.ReadFull(relay.control, preface[:]); err != nil {
		t.Fatal(err)
	}
	if command := preface[5+int(preface[4])+9]; command&0xf0 != nativeControlMode {
		t.Fatalf("control command = %#x, want stream base %#x", command, nativeControlMode)
	}
	if err := writeAll(relay.control, relay.acknowledgement(relay.port())); err != nil {
		t.Fatal(err)
	}
	if got, want := <-relay.dials, "tcp 127.0.0.1:443"; got != want {
		t.Fatalf("control dial = %q, want %q", got, want)
	}
	if got, want := <-relay.dials, "udp 127.0.0.1:"+strconv.Itoa(int(relay.port())); got != want {
		t.Fatalf("datagram dial = %q, want %q", got, want)
	}
	var client *net.UDPAddr

	select {
	case address := <-ready:
		if address.String() != packets.LocalAddr().String() {
			t.Fatalf("ready address = %s", address)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("datagram relay did not become ready")
	}

	packets.requests <- append([]byte{0, 0, 0, 4}, append(netip.MustParseAddr("2001:db8::1").AsSlice(), 0, 53, 'x')...)
	packets.requests <- append(socksIPv4Request("192.0.2.1:53"), bytes.Repeat([]byte{'x'}, maxDatagramPayload+1)...)
	packets.requests <- append(socksIPv4Request("192.0.2.1:53"), "first"...)
	packets.requests <- append([]byte{0, 0, 0, 3, byte(len("dns.example"))}, append([]byte("dns.example"), 0x01, 0xbb, 's', 'e', 'c', 'o', 'n', 'd')...)
	for sequence, want := range []struct {
		target  netip.AddrPort
		payload string
	}{{netip.MustParseAddrPort("192.0.2.1:53"), "first"}, {netip.MustParseAddrPort("192.0.2.9:443"), "second"}} {
		datagram, source := relay.readDatagram(t)
		client = source
		if len(datagram) != datagramHeaderSize+len(want.payload) {
			t.Fatalf("datagram %d length %d carries a length prefix or extra bytes", sequence, len(datagram))
		}
		header, payload := datagram[:datagramHeaderSize], datagram[datagramHeaderSize:]
		target := netip.AddrPortFrom(netip.AddrFrom4([4]byte(header[4:8])), binary.BigEndian.Uint16(header[8:10]))
		if binary.BigEndian.Uint16(header[1:3]) != testFlowID || target != want.target || string(payload) != want.payload {
			t.Fatalf("datagram %d = flow %d %s %q, want %s %q", sequence, binary.BigEndian.Uint16(header[1:3]), target, payload, want.target, want.payload)
		}
		// Relays drop sequence 0 as already seen, so numbering starts at 1.
		if got := binary.LittleEndian.Uint16(header[10:12]); got != uint16(sequence+1) || header[12] != 0 || header[13] != 0 {
			t.Fatalf("datagram %d sequence/trailer = % x", sequence, header[10:14])
		}
	}

	for _, reply := range []struct {
		flow    uint16
		payload string
	}{{testFlowID + 1, "other flow"}, {testFlowID, "reply"}} {
		datagram := make([]byte, datagramHeaderSize+len(reply.payload))
		binary.BigEndian.PutUint16(datagram[1:3], reply.flow)
		copy(datagram[4:8], []byte{198, 51, 100, 7})
		binary.BigEndian.PutUint16(datagram[8:10], 3478)
		copy(datagram[datagramHeaderSize:], reply.payload)
		if _, err := relay.udp.WriteToUDP(datagram, client); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case reply := <-packets.responses:
		if want := append(socksIPv4Request("198.51.100.7:3478"), "reply"...); !bytes.Equal(reply, want) {
			t.Fatalf("SOCKS reply = % x, want % x", reply, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("relay reply was not delivered")
	}

	_ = controlClient.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("relay after SOCKS close = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("relay did not stop after the SOCKS control connection closed")
	}
	var buffer [1]byte
	if count, err := relay.control.Read(buffer[:]); count != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("relay control after stop: read %d bytes, error %v", count, err)
	}
}

func TestRelayNativeDatagramsEndsWhenRelayClosesControl(t *testing.T) {
	t.Parallel()
	_, control := loopbackTCPPair(t)
	relay, dial := newNativeRelay(t)
	packets := newTestPacketIO()
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		request := nativeDatagramRequest(t, control, packets, dial)
		request.Ready = func(net.Addr) error { close(ready); return nil }
		done <- nativeTransport(t).RelayDatagrams(context.Background(), request)
	}()
	if err := readNativePreface(relay.control); err != nil {
		t.Fatal(err)
	}
	if err := writeAll(relay.control, relay.acknowledgement(relay.port())); err != nil {
		t.Fatal(err)
	}
	<-ready
	_ = relay.control.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("relay after relay control close = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("association outlived the relay control connection")
	}
	select {
	case <-packets.closed:
	default:
		t.Fatal("UDP association was not closed")
	}
}

func TestRelayNativeDatagramsRejectsAcknowledgementWithoutPort(t *testing.T) {
	t.Parallel()
	_, control := loopbackTCPPair(t)
	relay, dial := newNativeRelay(t)
	packets := newTestPacketIO()
	done := make(chan error, 1)
	go func() {
		done <- nativeTransport(t).RelayDatagrams(context.Background(), nativeDatagramRequest(t, control, packets, dial))
	}()
	if err := readNativePreface(relay.control); err != nil {
		t.Fatal(err)
	}
	if err := writeAll(relay.control, relay.acknowledgement(0)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrHandshake) {
			t.Fatalf("relay error = %v, want %v", err, ErrHandshake)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("relay accepted an acknowledgement without a UDP port")
	}
	if got := <-relay.dials; got != "tcp 127.0.0.1:443" {
		t.Fatalf("control dial = %q", got)
	}
	select {
	case extra := <-relay.dials:
		t.Fatalf("dialed %q without a UDP port", extra)
	default:
	}
}

func TestNativeDatagramKeepaliveRepeatsLastSequenceWhenQuiet(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		local, remote := net.Pipe()
		defer local.Close()
		defer remote.Close()
		writer := &datagramWriter{upstream: local, native: true}
		writer.lastSent.Store(time.Now().UnixNano())
		writer.lastSequence.Store(41)
		done := make(chan struct{})
		result := make(chan error, 1)
		go func() { result <- writer.keepAlive(done, testFlowID) }()
		received := make(chan time.Duration, 4)
		start := time.Now()
		go func() {
			buffer := make([]byte, 64)
			for {
				n, err := remote.Read(buffer)
				if err != nil {
					return
				}
				keepalive := buffer[:n]
				// Header only, for the flow, addressed to 0.0.0.0:0, and
				// numbered like the last datagram so the relay discards it.
				if n != datagramHeaderSize || binary.BigEndian.Uint16(keepalive[1:3]) != testFlowID ||
					!bytes.Equal(keepalive[4:10], make([]byte, 6)) || binary.LittleEndian.Uint16(keepalive[10:12]) != 41 ||
					keepalive[12] != 0 || keepalive[13] != 0 {
					t.Errorf("keepalive = % x", keepalive)
				}
				received <- time.Since(start)
			}
		}()

		// A fresh association and recent traffic both postpone the keepalive.
		time.Sleep(nativeKeepalive / 2)
		synctest.Wait()
		if len(received) != 0 {
			t.Fatal("keepalive sent right after the association started")
		}
		time.Sleep(nativeKeepalive/2 - time.Second)
		writer.lastSent.Store(time.Now().UnixNano())
		synctest.Wait()
		if len(received) != 0 {
			t.Fatal("keepalive sent while the association was active")
		}
		time.Sleep(nativeKeepalive + nativeKeepalive/5)
		synctest.Wait()
		if len(received) != 1 {
			t.Fatalf("keepalives after one quiet interval = %d, want 1", len(received))
		}
		if at := <-received; at < 2*nativeKeepalive-time.Second {
			t.Fatalf("keepalive at %s, before the association was quiet for %s", at, nativeKeepalive)
		}
		close(done)
		if err := <-result; err != nil {
			t.Fatalf("keepalive result = %v", err)
		}
	})
}
