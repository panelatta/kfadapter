package socks

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/oshinop/chinadapter/internal/provider"
	"github.com/oshinop/chinadapter/internal/shadowsocks"
	"github.com/oshinop/chinadapter/internal/snell"
	"github.com/oshinop/chinadapter/internal/state"
)

// snellIdleTimeout bounds how long a reusable Snell connection waits for its
// next request; clients keep pooled connections for about a minute.
const snellIdleTimeout = 5 * time.Minute

func (s *Server) tlsConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			if registry := s.credentials.Load(); registry != nil && registry.certificate != nil {
				return registry.certificate, nil
			}
			return nil, errors.New("socks: TLS certificate unavailable")
		},
	}
}

func (s *Server) smartEnabled() bool { return s.smart != nil && s.smart.Enabled() }

// resolvePrincipal maps a frontend principal, a node's SOCKS username or the
// automatic route's username, to the route a request dispatches to. The
// automatic route resolves once per request.
func (s *Server) resolvePrincipal(registry *credentialRegistry, principal string) (string, error) {
	if registry == nil || principal != registry.smart {
		return principal, nil
	}
	if !s.smartEnabled() {
		return "", state.ErrSelectorUnknown
	}
	return s.smart.Resolve()
}

// principalAuthorized reports whether a frontend key still names principal in
// registry; the automatic route also requires it to be enabled.
func (s *Server) principalAuthorized(registry *credentialRegistry, principal, route string, found bool) bool {
	return found && route == principal && (registry == nil || route != registry.smart || s.smartEnabled())
}

// handleSnell serves one Snell v6 (unshaped) connection: requests repeat on
// it while the client uses CONNECT v2 and each request ends in band.
func (s *Server) handleSnell(ctx context.Context, client net.Conn, releaseHandshake func()) error {
	defer client.Close()
	defer closeOnCancel(ctx, client)()
	if err := client.SetDeadline(time.Now().Add(s.handshakeTimeout)); err != nil {
		return fmt.Errorf("socks: set client setup deadline: %w", err)
	}
	handshake, err := snell.ReadHandshake(client)
	if err != nil {
		return err
	}
	registry := s.credentials.Load()
	if registry == nil || len(registry.snellKeys) == 0 {
		return state.ErrSelectorUnknown
	}
	candidates := s.snellRecent.order(registry)
	// One Argon2id derivation runs per candidate tried; bound how many
	// handshakes derive keys at once so unknown clients cannot starve flows.
	select {
	case s.snellTrials <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	conn, index, err := handshake.Accept(candidates)
	<-s.snellTrials
	if err != nil {
		return err
	}
	psk := candidates[index]
	principal, found := registry.snellRouteFor(psk)
	if !found {
		return state.ErrSelectorUnknown
	}
	s.snellRecent.touch(psk)
	if releaseHandshake != nil {
		releaseHandshake()
	}
	authorized := func(current *credentialRegistry) bool {
		route, found := current.snellRouteFor(psk)
		return s.principalAuthorized(current, principal, route, found)
	}
	for served := 0; ; served++ {
		if served > 0 {
			if err := client.SetDeadline(time.Now().Add(snellIdleTimeout)); err != nil {
				return err
			}
		}
		request, err := snell.ReadRequest(conn)
		if err != nil {
			if served > 0 && errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if request.Command == snell.CommandPing {
			return conn.WriteRecord([]byte{snell.ResponsePong})
		}
		routeUsername, err := s.resolvePrincipal(s.credentials.Load(), principal)
		if err != nil {
			_ = conn.WriteRecord(snell.ErrorResponse(replyGeneralFailure, replyMessage(replyGeneralFailure)))
			return err
		}
		if request.Command == snell.CommandUDP {
			return s.dispatchSnellDatagrams(ctx, conn, routeUsername, authorized)
		}
		session := snell.NewSession(conn)
		err = s.dispatch(ctx, flow{
			command: commandConnect, destination: target{Host: request.Host, Port: request.Port}, client: session,
			routeUsername: routeUsername, authorized: authorized,
			ready: func(net.Addr) error { return nil },
			fail:  func(code byte) { _ = session.Fail(code, replyMessage(code)) },
		})
		if request.Command != snell.CommandConnectV2 || !session.Finished() || ctx.Err() != nil {
			return err
		}
	}
}

func (s *Server) dispatchSnellDatagrams(ctx context.Context, conn *snell.Conn, routeUsername string, authorized func(*credentialRegistry) bool) error {
	watched, peer := net.Pipe()
	defer peer.Close()
	control := datagramControl{Conn: watched, raw: conn}
	defer func() { _ = control.Close() }()
	packets, err := newSnellPackets(conn)
	if err != nil {
		return err
	}
	return s.dispatch(ctx, flow{
		command: commandUDP, client: control, routeUsername: routeUsername, authorized: authorized,
		openPackets: func() (provider.PacketIO, error) { return packets, nil },
		ready:       func(net.Addr) error { return conn.WriteRecord([]byte{snell.ResponseTunnel}) },
		fail: func(code byte) {
			_ = conn.WriteRecord(snell.ErrorResponse(code, replyMessage(code)))
		},
	})
}

// datagramControl stands in for the TCP control connection of a SOCKS UDP
// association. Transports read it only to learn that the client left, so it
// is a pipe that never carries data; closing it closes the Snell connection
// and its deadlines apply to that connection too.
type datagramControl struct {
	net.Conn
	raw *snell.Conn
}

func (control datagramControl) Close() error {
	_ = control.raw.Close()
	return control.Conn.Close()
}

func (control datagramControl) SetDeadline(t time.Time) error {
	if err := control.raw.SetDeadline(t); err != nil {
		return err
	}
	return control.Conn.SetDeadline(t)
}

// snellPackets carries SOCKS UDP request framing over Snell UDP-over-TCP
// records, so transports see the same datagrams as from a SOCKS association.
type snellPackets struct {
	conn   *snell.Conn
	flowID uint16
	mu     sync.Mutex
	out    []byte
}

func newSnellPackets(conn *snell.Conn) (*snellPackets, error) {
	var flowID [2]byte
	for flowID == [2]byte{} {
		if _, err := rand.Read(flowID[:]); err != nil {
			return nil, err
		}
	}
	return &snellPackets{conn: conn, flowID: binary.BigEndian.Uint16(flowID[:])}, nil
}

func (packets *snellPackets) FlowID() uint16      { return packets.flowID }
func (packets *snellPackets) LocalAddr() net.Addr { return &net.UDPAddr{IP: net.IPv4zero} }
func (packets *snellPackets) Close() error        { return packets.conn.Close() }

// ReadPacket returns the next client datagram in SOCKS UDP request framing.
// Malformed or oversized datagrams are discarded one at a time.
func (packets *snellPackets) ReadPacket(packet []byte) (int, error) {
	for {
		frame, err := packets.conn.ReadRecord()
		if errors.Is(err, snell.ErrZeroChunk) {
			return 0, io.EOF
		}
		if err != nil {
			return 0, err
		}
		datagram, err := snell.ParseDatagram(frame)
		if err != nil {
			continue
		}
		if n, ok := encodeUDPRequest(packet, datagram); ok {
			return n, nil
		}
	}
}

func encodeUDPRequest(packet []byte, datagram snell.Datagram) (int, bool) {
	header := make([]byte, 0, 4+1+255+2)
	header = append(header, 0, 0, 0)
	switch {
	case datagram.Host != "":
		if len(datagram.Host) > 255 {
			return 0, false
		}
		header = append(append(header, addressDomain, byte(len(datagram.Host))), datagram.Host...)
	case datagram.Addr.Is4():
		raw := datagram.Addr.As4()
		header = append(append(header, addressIPv4), raw[:]...)
	default:
		raw := datagram.Addr.As16()
		header = append(append(header, addressIPv6), raw[:]...)
	}
	header = binary.BigEndian.AppendUint16(header, datagram.Port)
	if len(header)+len(datagram.Payload) > len(packet) {
		return 0, false
	}
	n := copy(packet, header)
	return n + copy(packet[n:], datagram.Payload), true
}

// WritePacket relays one SOCKS UDP reply as a Snell UDP response record.
// Replies from domain sources cannot be represented and are dropped.
func (packets *snellPackets) WritePacket(packet []byte) error {
	if len(packet) < 4 || packet[0] != 0 || packet[1] != 0 || packet[2] != 0 {
		return nil
	}
	size := 0
	switch packet[3] {
	case addressIPv4:
		size = 4
	case addressIPv6:
		size = 16
	default:
		return nil
	}
	if len(packet) < 4+size+2 {
		return nil
	}
	address, _ := netip.AddrFromSlice(packet[4 : 4+size])
	source := netip.AddrPortFrom(address, binary.BigEndian.Uint16(packet[4+size:]))
	payload := packet[4+size+2:]
	packets.mu.Lock()
	defer packets.mu.Unlock()
	packets.out = snell.AppendDatagramResponse(packets.out[:0], source, payload)
	if len(packets.out) > snell.MaxRecordPayload {
		return nil
	}
	return packets.conn.WriteRecord(packets.out)
}

// handleShadowsocks serves one Shadowsocks 2022 connection. A request that
// fails authentication is drained rather than answered.
func (s *Server) handleShadowsocks(ctx context.Context, client net.Conn, releaseHandshake func()) error {
	defer client.Close()
	defer closeOnCancel(ctx, client)()
	if err := client.SetDeadline(time.Now().Add(s.handshakeTimeout)); err != nil {
		return fmt.Errorf("socks: set client setup deadline: %w", err)
	}
	registry := s.credentials.Load()
	if registry == nil || registry.shadowsocks == nil {
		shadowsocks.Reject(client)
		return state.ErrSelectorUnknown
	}
	conn, index, destination, err := shadowsocks.Accept(client, registry.shadowsocks, s.salts, time.Now)
	if err != nil {
		shadowsocks.Reject(client)
		return err
	}
	userKey, principal := registry.shadowsocksKeys[index], registry.shadowsocksRoute[index]
	if releaseHandshake != nil {
		releaseHandshake()
	}
	routeUsername, err := s.resolvePrincipal(registry, principal)
	if err != nil {
		return err
	}
	return s.dispatch(ctx, flow{
		command: commandConnect, destination: target{Host: destination.Host, Port: destination.Port}, client: conn,
		routeUsername: routeUsername,
		authorized: func(current *credentialRegistry) bool {
			route, found := current.shadowsocksRouteFor(userKey)
			return s.principalAuthorized(current, principal, route, found)
		},
		// Shadowsocks has no success or failure reply; a failed request
		// simply closes.
		ready: func(net.Addr) error { return nil },
		fail:  func(byte) {},
	})
}

// replyMessage is the Snell error text for a SOCKS reply code.
func replyMessage(code byte) string {
	switch code {
	case replyConnectionForbidden:
		return "connection not allowed"
	case replyNetworkUnreachable:
		return "network unreachable"
	case replyHostUnreachable:
		return "host unreachable"
	case replyConnectionRefused:
		return "connection refused"
	case replyTTLExpired:
		return "connection timed out"
	case replyCommandUnsupported:
		return "command not supported by this route"
	case replyAddressUnsupported:
		return "address type not supported by this route"
	default:
		return "route unavailable"
	}
}
