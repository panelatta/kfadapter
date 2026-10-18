package quickfox

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/netip"
	"strconv"
	"time"

	"github.com/oshinop/chinadapter/internal/provider"
)

// Native datagram mode, measured against live relays from the Singapore
// production VPS in 2026-10. The control connection authenticates with the
// stream command base 0x20; acknowledgement bytes 8..9 are then a big-endian
// UDP port on the relay, allocated per control connection, which accepts and
// answers the udp2tcp header and raw payload as one UDP datagram each, without
// the length prefix. The association lives while the control connection stays
// open. Under base 0x30 that port is silent, and read little-endian it is
// closed. Mainland-bound relays drop a datagram whose sequence number they
// have already seen and start every association as if 0 had been seen; they
// accept numbers out of order, and 0 again after the counter wraps. Datagrams
// are therefore numbered from 1. Relays kept associations that were quiet for
// four minutes, so the keepalive below only refreshes NAT and firewall state
// between this host and the relay, which home routers expire sooner, so relay
// datagrams after a quiet period still arrive.
const (
	nativeControlMode = 0x20
	// nativeKeepalive is the quiet interval after which a keepalive goes to
	// the relay.
	nativeKeepalive = 25 * time.Second
)

// EnableNativeUDP makes UDP associations use the relay's native datagram port
// instead of the TCP-carried udp2tcp mode. Call it before the transport is
// registered.
func (transport *Transport) EnableNativeUDP() { transport.nativeUDP = true }

func (transport *Transport) relayNativeDatagrams(ctx context.Context, request provider.DatagramRequest, authority tunnelAuthority) error {
	control, err := dialRelay(ctx, request.Dial, request.NodeHost, request.NodePort, request.HandshakeTimeout)
	if err != nil {
		return err
	}
	defer control.Close()
	stopControlCancel := context.AfterFunc(ctx, func() { _ = control.Close() })
	defer stopControlCancel()
	if err := control.SetDeadline(time.Now().Add(request.HandshakeTimeout)); err != nil {
		return fmt.Errorf("quickfox: set datagram control deadline: %w", err)
	}
	preface, err := encodeTunnelAuth(authority, nativeNonce(transport.clock()), nativeControlMode, netip.Addr{}, 0)
	if err != nil {
		return err
	}
	if err := writeAll(control, preface); err != nil {
		return fmt.Errorf("quickfox: write datagram control preface: %w", err)
	}
	var acknowledgement [controlACKSize]byte
	if _, err := io.ReadFull(control, acknowledgement[:]); err != nil {
		return fmt.Errorf("%w: read datagram control acknowledgement: %w", ErrHandshake, err)
	}
	if err := control.SetDeadline(time.Time{}); err != nil {
		return fmt.Errorf("quickfox: clear datagram control deadline: %w", err)
	}
	port := binary.BigEndian.Uint16(acknowledgement[8:10])
	if port == 0 {
		return fmt.Errorf("%w: acknowledgement carries no UDP port", ErrHandshake)
	}
	// The UDP port belongs to the relay that accepted this control
	// connection, so address the same IP even when NodeHost is a name.
	relayHost := request.NodeHost
	if remote, ok := control.RemoteAddr().(*net.TCPAddr); ok && remote != nil && remote.IP != nil {
		relayHost = remote.IP.String()
	}
	dialCtx, cancel := context.WithTimeout(ctx, request.HandshakeTimeout)
	upstream, err := request.Dial(dialCtx, "udp", net.JoinHostPort(relayHost, strconv.Itoa(int(port))))
	cancel()
	if ctx.Err() != nil {
		if err == nil {
			_ = upstream.Close()
		}
		return ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("quickfox: connect relay UDP port: %w", err)
	}
	defer upstream.Close()
	if !request.Admit() {
		return provider.ErrNoSession
	}
	if err := request.Ready(request.Packets.LocalAddr()); err != nil {
		return err
	}
	results := make(chan error, 5)
	done := make(chan struct{})
	closeAll := func() {
		_ = request.Packets.Close()
		_ = upstream.Close()
		_ = control.Close()
		_ = request.Control.Close()
	}
	stopCancellation := context.AfterFunc(ctx, closeAll)
	defer stopCancellation()
	writer := &datagramWriter{
		transport: transport, ctx: ctx, upstream: upstream, node: request.NodeHost, timeout: request.HandshakeTimeout,
		sequence: 1, native: true,
	}
	writer.lastSent.Store(time.Now().UnixNano())
	go func() { results <- watchControl(request.Control) }()
	// The relay ends the association by closing its control connection.
	go func() { results <- watchControl(control) }()
	go func() { results <- writer.pump(request.Packets) }()
	go func() { results <- readNativeDatagrams(upstream, request.Packets) }()
	go func() { results <- writer.keepAlive(done, request.Packets.FlowID()) }()
	err = <-results
	close(done)
	closeAll()
	for range 4 {
		<-results
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err == nil || errors.Is(err, io.EOF) || benignRelayClose(err) {
		return nil
	}
	return err
}

// keepAlive sends a header-only datagram whenever no datagram went to the
// relay for nativeKeepalive. It repeats the last sequence number used, so the
// relay discards it as a duplicate, and addresses 0.0.0.0:0. It returns nil
// when done closes.
func (writer *datagramWriter) keepAlive(done <-chan struct{}, flowID uint16) error {
	ticker := time.NewTicker(nativeKeepalive / 5)
	defer ticker.Stop()
	keepalive := make([]byte, datagramHeaderSize)
	for {
		select {
		case <-done:
			return nil
		case now := <-ticker.C:
			if now.Sub(time.Unix(0, writer.lastSent.Load())) < nativeKeepalive {
				continue
			}
			writer.lastSent.Store(now.UnixNano())
			keepalive[0] = byte(rand.N(254))
			binary.BigEndian.PutUint16(keepalive[1:3], flowID)
			keepalive[3] = byte(rand.N(254))
			binary.LittleEndian.PutUint16(keepalive[10:12], uint16(writer.lastSequence.Load()))
			if _, err := writer.upstream.Write(keepalive); err != nil {
				return fmt.Errorf("quickfox: write datagram keepalive: %w", err)
			}
		}
	}
}

// readNativeDatagrams converts relay datagrams for this association's flow
// into SOCKS UDP replies.
func readNativeDatagrams(upstream net.Conn, packets provider.PacketIO) error {
	buffer := make([]byte, maxReplyFrame)
	flowID := packets.FlowID()
	for {
		n, err := upstream.Read(buffer)
		if err != nil {
			return err
		}
		if n < datagramHeaderSize {
			continue
		}
		if err := deliverDatagram(buffer[:n], flowID, packets); err != nil {
			return err
		}
	}
}
