package snell

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	errBadRequest = errors.New("snell: malformed request")
	errBadPacket  = errors.New("snell: malformed UDP frame")
)

// Request is one decoded request header. Host and Port are empty for ping and
// UDP requests.
type Request struct {
	Command byte
	Host    string
	Port    uint16
}

// ReadRequest decodes a request header from the record stream. Ping carries
// no client ID; every other command carries a length-prefixed client ID that
// is ignored.
func ReadRequest(r io.Reader) (Request, error) {
	var head [2]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return Request{}, err
	}
	if head[0] != Version {
		return Request{}, fmt.Errorf("%w: version %d", errBadRequest, head[0])
	}
	request := Request{Command: head[1]}
	if request.Command == CommandPing {
		return request, nil
	}
	if _, err := readShortString(r); err != nil {
		return Request{}, err
	}
	switch request.Command {
	case CommandUDP:
		return request, nil
	case CommandConnect, CommandConnectV2:
	default:
		return Request{}, fmt.Errorf("%w: command %d", errBadRequest, request.Command)
	}
	host, err := readShortString(r)
	if err != nil {
		return Request{}, err
	}
	var port [2]byte
	if _, err := io.ReadFull(r, port[:]); err != nil {
		return Request{}, unexpectedEOF(err)
	}
	request.Host, request.Port = canonicalHost(host), binary.BigEndian.Uint16(port[:])
	if !validHost(request.Host) || request.Port == 0 {
		return Request{}, errBadRequest
	}
	return request, nil
}

func readShortString(r io.Reader) (string, error) {
	var length [1]byte
	if _, err := io.ReadFull(r, length[:]); err != nil {
		return "", unexpectedEOF(err)
	}
	value := make([]byte, int(length[0]))
	if _, err := io.ReadFull(r, value); err != nil {
		return "", unexpectedEOF(err)
	}
	return string(value), nil
}

func canonicalHost(host string) string {
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		if address, err := netip.ParseAddr(host[1 : len(host)-1]); err == nil && address.Is6() {
			return address.String()
		}
	}
	if address, err := netip.ParseAddr(host); err == nil && address.Zone() == "" {
		return address.Unmap().String()
	}
	return host
}

func validHost(host string) bool {
	if host == "" {
		return false
	}
	for index := range len(host) {
		if host[index] < 0x21 || host[index] > 0x7e {
			return false
		}
	}
	return true
}

// AppendConnectRequest encodes a client CONNECT request; it exists for tests
// and interoperability checks.
func AppendConnectRequest(dst []byte, host string, port uint16, reuse bool) []byte {
	command := CommandConnect
	if reuse {
		command = CommandConnectV2
	}
	dst = append(dst, Version, command, 0, byte(len(host)))
	dst = append(dst, host...)
	return binary.BigEndian.AppendUint16(dst, port)
}

// ErrorResponse encodes a server error response. Messages are truncated to
// the 255-byte length field.
func ErrorResponse(code byte, message string) []byte {
	if len(message) > 250 {
		message = message[:250]
	}
	return append([]byte{ResponseError, code, byte(len(message))}, message...)
}

// Datagram is one decoded UDP-over-TCP request frame. Host is set for a
// domain destination, Addr otherwise.
type Datagram struct {
	Host    string
	Addr    netip.Addr
	Port    uint16
	Payload []byte
}

// ParseDatagram decodes `[0x01][host-length][host | family ip][port][payload]`.
// A zero host length is followed by family 0x04 or 0x06 and a raw address.
func ParseDatagram(frame []byte) (Datagram, error) {
	if len(frame) < 2 || frame[0] != udpForward {
		return Datagram{}, errBadPacket
	}
	if hostLength := int(frame[1]); hostLength != 0 {
		end := 2 + hostLength
		if len(frame) < end+2 {
			return Datagram{}, errBadPacket
		}
		host := canonicalHost(string(frame[2:end]))
		if !validHost(host) {
			return Datagram{}, errBadPacket
		}
		datagram := Datagram{Port: binary.BigEndian.Uint16(frame[end:]), Payload: frame[end+2:]}
		if address, err := netip.ParseAddr(host); err == nil {
			datagram.Addr = address
		} else {
			datagram.Host = host
		}
		return datagram, nil
	}
	if len(frame) < 3 {
		return Datagram{}, errBadPacket
	}
	size := 0
	switch frame[2] {
	case 0x04:
		size = 4
	case 0x06:
		size = 16
	default:
		return Datagram{}, errBadPacket
	}
	if len(frame) < 3+size+2 {
		return Datagram{}, errBadPacket
	}
	address, _ := netip.AddrFromSlice(frame[3 : 3+size])
	return Datagram{Addr: address.Unmap(), Port: binary.BigEndian.Uint16(frame[3+size:]), Payload: frame[3+size+2:]}, nil
}

// AppendDatagram encodes a client UDP request frame for an IP destination;
// it exists for tests and interoperability checks.
func AppendDatagram(dst []byte, destination netip.AddrPort, payload []byte) []byte {
	dst = append(dst, udpForward, 0)
	if address := destination.Addr().Unmap(); address.Is4() {
		raw := address.As4()
		dst = append(append(dst, 0x04), raw[:]...)
	} else {
		raw := address.As16()
		dst = append(append(dst, 0x06), raw[:]...)
	}
	dst = binary.BigEndian.AppendUint16(dst, destination.Port())
	return append(dst, payload...)
}

// AppendDatagramResponse encodes a server UDP response frame
// `[0x04|0x06][ip][port][payload]`.
func AppendDatagramResponse(dst []byte, source netip.AddrPort, payload []byte) []byte {
	if address := source.Addr().Unmap(); address.Is4() {
		raw := address.As4()
		dst = append(append(dst, 0x04), raw[:]...)
	} else {
		raw := address.As16()
		dst = append(append(dst, 0x06), raw[:]...)
	}
	dst = binary.BigEndian.AppendUint16(dst, source.Port())
	return append(dst, payload...)
}

// Session relays one CONNECT request over a Conn. Its first write carries the
// tunnel response, CloseWrite sends the zero chunk, and Close keeps the
// connection open only when both directions ended in band, so a reusable
// connection can serve the next request.
type Session struct {
	conn *Conn

	mu        sync.Mutex
	responded bool
	writeDone bool
	failed    bool
	readDone  atomic.Bool
	closed    atomic.Bool
}

// NewSession starts a request on conn.
func NewSession(conn *Conn) *Session { return &Session{conn: conn} }

func (s *Session) Read(p []byte) (int, error) {
	if s.readDone.Load() {
		return 0, io.EOF
	}
	n, err := s.conn.Read(p)
	if errors.Is(err, ErrZeroChunk) {
		s.readDone.Store(true)
		return n, io.EOF
	}
	return n, err
}

func (s *Session) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writeDone {
		return 0, net.ErrClosed
	}
	if len(p) == 0 {
		return 0, nil
	}
	var prefix []byte
	if !s.responded {
		prefix = []byte{ResponseTunnel}
	}
	if err := s.conn.WriteStream(prefix, p); err != nil {
		return 0, err
	}
	s.responded = true
	return len(p), nil
}

// CloseWrite ends the response with a zero chunk, sending the tunnel response
// first when no data preceded it.
func (s *Session) CloseWrite() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writeDone {
		return nil
	}
	s.writeDone = true
	if !s.responded {
		s.responded = true
		if err := s.conn.WriteRecord([]byte{ResponseTunnel}); err != nil {
			return err
		}
	}
	return s.conn.WriteZeroChunk()
}

// Fail sends an error response unless the tunnel response already went out.
func (s *Session) Fail(code byte, message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.responded {
		return nil
	}
	s.responded, s.writeDone, s.failed = true, true, true
	return s.conn.WriteRecord(ErrorResponse(code, message))
}

// Finished reports whether both directions ended in band.
func (s *Session) Finished() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readDone.Load() && s.writeDone && !s.failed
}

// Close aborts the connection unless the request finished in band.
func (s *Session) Close() error {
	if s.closed.Swap(true) || s.Finished() {
		return nil
	}
	return s.conn.Close()
}

func (s *Session) LocalAddr() net.Addr                { return s.conn.LocalAddr() }
func (s *Session) RemoteAddr() net.Addr               { return s.conn.RemoteAddr() }
func (s *Session) SetDeadline(t time.Time) error      { return s.conn.SetDeadline(t) }
func (s *Session) SetReadDeadline(t time.Time) error  { return s.conn.SetReadDeadline(t) }
func (s *Session) SetWriteDeadline(t time.Time) error { return s.conn.SetWriteDeadline(t) }
