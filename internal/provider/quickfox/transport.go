package quickfox

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/oshinop/chinadapter/internal/provider"
)

const (
	relayXORByte    = byte(0x32)
	relayBufferSize = 32 * 1024
	controlACKSize  = 14
)

// ErrHandshake reports a rejected or incomplete native relay handshake.
var ErrHandshake = errors.New("quickfox: relay handshake failed")

type lookupNetIPFunc func(context.Context, string, string) ([]netip.Addr, error)

// Transport implements the observed QuickFox native framed TCP relay for IPv4
// destinations, and its TCP-carried datagram relay (datagram.go). Streams read
// the 14-byte control acknowledgement before opening the data connection.
// Domain targets use the optional node-scoped DoH resolver (doh.go) when
// configured, and the system resolver otherwise.
type Transport struct {
	clock  func() time.Time
	lookup lookupNetIPFunc
	doh    *dohResolver
}

func NewTransport() *Transport {
	return &Transport{clock: time.Now, lookup: net.DefaultResolver.LookupNetIP}
}

func (*Transport) Protocol() provider.Protocol { return NativeProtocol }

func (transport *Transport) RelayStream(ctx context.Context, request provider.RelayRequest) (relayErr error) {
	if transport == nil || ctx == nil || request.Dial == nil || request.Client == nil || request.NodeHost == "" || request.NodePort == 0 ||
		request.Target.Host == "" || request.Target.Port == 0 || request.Admit == nil || request.Ready == nil || request.HandshakeTimeout <= 0 ||
		transport.clock == nil || transport.lookup == nil {
		return errors.New("quickfox: invalid stream relay request")
	}
	// Closing a socket to interrupt a handshake reports an I/O error. Preserve
	// the caller's cancellation reason even when that close wins the race.
	defer func() {
		if err := ctx.Err(); err != nil {
			relayErr = err
		}
	}()
	authority, err := decodeTunnelAuthority(request.Authority)
	if err != nil {
		return err
	}
	target, err := transport.resolveTarget(ctx, request.Target.Host, request.NodeHost, request.HandshakeTimeout)
	if err != nil {
		return err
	}
	control, err := dialRelay(ctx, request.Dial, request.NodeHost, request.NodePort, request.HandshakeTimeout)
	if err != nil {
		return err
	}
	defer control.Close()
	// Keep cancellation attached through both handshakes and the relay: the
	// control connection stays open while the data connection is in use.
	stopControlCancel := context.AfterFunc(ctx, func() { _ = control.Close() })
	defer stopControlCancel()
	if err := control.SetDeadline(time.Now().Add(request.HandshakeTimeout)); err != nil {
		return fmt.Errorf("quickfox: set control deadline: %w", err)
	}
	nonce := nativeNonce(transport.clock())
	controlPacket, err := encodeTunnelAuth(authority, nonce, 0x20, netip.Addr{}, 0)
	if err != nil {
		return err
	}
	if err := writeAll(control, controlPacket); err != nil {
		return fmt.Errorf("quickfox: write control preface: %w", err)
	}
	var acknowledgement [controlACKSize]byte
	if _, err := io.ReadFull(control, acknowledgement[:]); err != nil {
		return fmt.Errorf("%w: read control acknowledgement: %w", ErrHandshake, err)
	}
	if err := control.SetDeadline(time.Time{}); err != nil {
		return fmt.Errorf("quickfox: clear control deadline: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	upstream, err := dialRelay(ctx, request.Dial, request.NodeHost, request.NodePort, request.HandshakeTimeout)
	if err != nil {
		return err
	}
	defer upstream.Close()
	stopDataCancel := context.AfterFunc(ctx, func() { _ = upstream.Close() })
	defer stopDataCancel()
	if err := upstream.SetDeadline(time.Now().Add(request.HandshakeTimeout)); err != nil {
		return fmt.Errorf("quickfox: set data deadline: %w", err)
	}
	dataPacket, err := encodeTunnelAuth(authority, nonce, 0x10, target, request.Target.Port)
	if err != nil {
		return err
	}
	if err := writeAll(upstream, dataPacket); err != nil {
		return fmt.Errorf("quickfox: write data preface: %w", err)
	}
	if err := upstream.SetDeadline(time.Time{}); err != nil {
		return fmt.Errorf("quickfox: clear data deadline: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !request.Admit() {
		return provider.ErrNoSession
	}
	if err := request.Ready(upstream.LocalAddr()); err != nil {
		return err
	}
	return relayXOR(ctx, request.Client, upstream)
}

func (transport *Transport) resolveTarget(ctx context.Context, host, node string, timeout time.Duration) (netip.Addr, error) {
	if address, err := netip.ParseAddr(host); err == nil {
		if address.Is4() {
			return address, nil
		}
		return netip.Addr{}, fmt.Errorf("quickfox: IPv6 targets are unsupported: %w", provider.ErrAddressUnsupported)
	}
	// The relay preface carries only an IPv4 address, so domain targets are
	// resolved locally. Answers pointing at local or private space are
	// skipped: forwarding them to the provider relay can never be intended.
	resolveContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	addresses, err := transport.lookupTarget(resolveContext, host, node, timeout)
	if err != nil {
		var dnsErr *net.DNSError
		if errors.Is(err, errDoHNoAddress) || errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			return netip.Addr{}, fmt.Errorf("quickfox: target has no IPv4 address: %w: %w", provider.ErrAddressUnsupported, err)
		}
		return netip.Addr{}, fmt.Errorf("quickfox: resolve target: %w", err)
	}
	for _, address := range addresses {
		address = address.Unmap()
		if address.Is4() && publicUnicast(address) {
			return address, nil
		}
	}
	return netip.Addr{}, fmt.Errorf("quickfox: target has no public IPv4 address: %w", provider.ErrAddressUnsupported)
}

// lookupTarget prefers the DoH resolver scoped to the node's network. Its
// authoritative answers are final; when it is unavailable the system resolver
// answers instead, which only loses the node-scoped edge selection.
func (transport *Transport) lookupTarget(ctx context.Context, host, node string, timeout time.Duration) ([]netip.Addr, error) {
	if transport.doh != nil {
		if nodeAddress, err := netip.ParseAddr(node); err == nil && nodeAddress.Unmap().Is4() {
			addresses, err := transport.doh.lookup(ctx, host, nodeAddress.Unmap(), min(timeout/2, maxDoHBudget))
			if err == nil || errors.Is(err, errDoHNoAddress) {
				return addresses, err
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
		}
	}
	return transport.lookup(ctx, "ip4", host)
}

func publicUnicast(address netip.Addr) bool {
	return address.IsGlobalUnicast() && !address.IsPrivate() && !address.IsLoopback() &&
		!address.IsLinkLocalUnicast() && !address.IsUnspecified() && !sharedAddressSpace.Contains(address)
}

// sharedAddressSpace is RFC 6598 carrier-grade NAT space.
var sharedAddressSpace = netip.MustParsePrefix("100.64.0.0/10")

func dialRelay(ctx context.Context, dial provider.DialContextFunc, host string, port uint16, timeout time.Duration) (net.Conn, error) {
	dialContext, cancel := context.WithTimeout(ctx, timeout)
	connection, err := dial(dialContext, "tcp", net.JoinHostPort(host, strconv.Itoa(int(port))))
	cancel()
	if ctx.Err() != nil {
		if err == nil {
			_ = connection.Close()
		}
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, fmt.Errorf("quickfox: connect selected upstream: %w", err)
	}
	return connection, nil
}

func nativeNonce(now time.Time) byte {
	seed := uint32(now.Unix())
	state := seed*214013 + 2531011
	return byte(((state >> 16) & 0x7fff) % 16)
}

func encodeTunnelAuth(authority tunnelAuthority, nonce, mode byte, target netip.Addr, targetPort uint16) ([]byte, error) {
	if !validToken(authority.Token) || authority.UserID <= 0 || authority.UserID > int64(^uint32(0)) || nonce > 15 ||
		mode != 0x10 && mode != 0x20 && mode != datagramControlMode || targetPort != 0 && !target.Is4() {
		return nil, ErrSchema
	}
	identity := []byte("C" + authority.Token + "," + strconv.FormatUint(uint64(authority.AppID), 10))
	if len(identity) > 255 {
		return nil, ErrSchema
	}
	packet := make([]byte, 27+len(identity))
	packet[0] = nonce<<4 | 7
	packet[1] = 0x49
	packet[2] = nonce
	packet[3] = 8
	packet[4] = byte(len(identity))
	copy(packet[5:], identity)
	offset := 5 + len(identity)
	binary.LittleEndian.PutUint32(packet[offset:], uint32(authority.UserID))
	offset += 4
	var targetWord uint32
	if target.Is4() {
		octets := target.As4()
		targetWord = binary.LittleEndian.Uint32(octets[:])
	}
	portWord := (targetPort&0xff)<<8 | targetPort>>8
	checksum := uint32(byte(targetWord)^byte(portWord)^nonce) + uint32(nonce)*0x1110
	binary.LittleEndian.PutUint32(packet[offset:], checksum)
	offset += 4
	packet[offset] = 0x0b
	packet[offset+1] = mode + nonce
	binary.LittleEndian.PutUint16(packet[offset+2:], 0x0400)
	binary.LittleEndian.PutUint32(packet[offset+4:], targetWord)
	binary.LittleEndian.PutUint16(packet[offset+8:], portWord)
	binary.LittleEndian.PutUint32(packet[offset+10:], ^uint32(0))
	return packet, nil
}

func relayXOR(ctx context.Context, client, upstream net.Conn) error {
	results := make(chan error, 2)
	var closeOnce sync.Once
	closeBoth := func() {
		closeOnce.Do(func() {
			_ = client.Close()
			_ = upstream.Close()
		})
	}
	cancelled := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			closeBoth()
		case <-cancelled:
		}
	}()
	pump := func(destination, source net.Conn) {
		buffer := make([]byte, relayBufferSize)
		var result error
		for {
			count, readErr := source.Read(buffer)
			if count > 0 {
				for index := range count {
					buffer[index] ^= relayXORByte
				}
				if writeErr := writeAll(destination, buffer[:count]); writeErr != nil {
					result = writeErr
					break
				}
			}
			if readErr != nil {
				if errors.Is(readErr, io.EOF) {
					if closer, ok := destination.(interface{ CloseWrite() error }); ok {
						result = closer.CloseWrite()
					} else {
						result = destination.Close()
					}
				} else {
					result = readErr
				}
				break
			}
		}
		// A locally closed socket must unblock the other pump even when
		// its error is harmless to report. Only a clean EOF half-closes.
		if result != nil {
			closeBoth()
		}
		results <- result
	}
	go pump(upstream, client)
	go pump(client, upstream)
	first := <-results
	second := <-results
	close(cancelled)
	if first != nil && !benignRelayClose(first) {
		return first
	}
	if second != nil && !benignRelayClose(second) {
		return second
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func benignRelayClose(err error) bool {
	return errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe)
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) != 0 {
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		if written <= 0 || written > len(data) {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}
