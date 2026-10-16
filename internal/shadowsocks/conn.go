package shadowsocks

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"
)

const (
	readBufferSize   = 32 << 10
	maxRetainedWrite = 256 << 10
	// The fixed request header is type, timestamp and the length of the
	// variable header chunk.
	requestFixedSize = 1 + 8 + 2
	// ATYP, the shortest address, port and padding length.
	minVariableSize = 1 + 1 + 2 + 2

	addressIPv4   = 1
	addressDomain = 3
	addressIPv6   = 4
)

// Target is a requested destination: a canonical IP literal or a domain.
type Target struct {
	Host string
	Port uint16
}

// Conn is an authenticated request stream and its response stream.
type Conn struct {
	raw         net.Conn
	userKey     []byte
	requestSalt []byte
	now         func() time.Time

	readMu      sync.Mutex
	in          *bufio.Reader
	opener      *sealer
	pending     []byte
	lengthChunk [2 + tagSize]byte
	lengthPlain [2]byte
	buffer      []byte

	writeMu sync.Mutex
	sender  *sealer
	out     []byte
}

// Accept authenticates a request stream: salt, identity header, fixed and
// variable header chunks. It returns the stream, the matched user index and
// the target. Every authentication failure is ErrUnauthenticated, after
// which the caller should Reject raw. The caller bounds reads with a deadline.
func Accept(raw net.Conn, users *Users, salts *SaltFilter, now func() time.Time) (*Conn, int, Target, error) {
	in := bufio.NewReaderSize(raw, readBufferSize)
	var opening [saltSize + aesBlockSize + requestFixedSize + tagSize]byte
	if _, err := io.ReadFull(in, opening[:]); err != nil {
		return nil, -1, Target{}, fmt.Errorf("%w: %w", ErrUnauthenticated, err)
	}
	salt := opening[:saltSize]
	index, found := users.identify(salt, opening[saltSize:saltSize+aesBlockSize])
	if !found {
		return nil, -1, Target{}, ErrUnauthenticated
	}
	userKey := users.keys[index]
	opener, err := newSealer(userKey, salt)
	if err != nil {
		return nil, -1, Target{}, err
	}
	fixed, err := opener.open(nil, opening[saltSize+aesBlockSize:])
	if err != nil || fixed[0] != headerTypeClient || !withinSkew(fixed[1:9], now()) {
		return nil, -1, Target{}, ErrUnauthenticated
	}
	length := int(binary.BigEndian.Uint16(fixed[9:]))
	if length < minVariableSize {
		return nil, -1, Target{}, ErrUnauthenticated
	}
	if !salts.Add(salt, now()) {
		return nil, -1, Target{}, ErrUnauthenticated
	}
	sealed := make([]byte, length+tagSize)
	if _, err := io.ReadFull(in, sealed); err != nil {
		return nil, -1, Target{}, fmt.Errorf("%w: %w", ErrUnauthenticated, err)
	}
	variable, err := opener.open(sealed[:0], sealed)
	if err != nil {
		return nil, -1, Target{}, ErrUnauthenticated
	}
	target, payload, err := parseVariableHeader(variable)
	if err != nil {
		return nil, -1, Target{}, fmt.Errorf("%w: %w", ErrUnauthenticated, err)
	}
	conn := &Conn{
		raw: raw, userKey: append([]byte(nil), userKey...), requestSalt: append([]byte(nil), salt...), now: now,
		in: in, opener: opener, pending: payload,
	}
	return conn, index, target, nil
}

const aesBlockSize = 16

func withinSkew(encoded []byte, now time.Time) bool {
	timestamp := time.Unix(int64(binary.BigEndian.Uint64(encoded)), 0)
	skew := now.Sub(timestamp)
	return skew <= maxTimeSkew && skew >= -maxTimeSkew
}

func parseVariableHeader(header []byte) (Target, []byte, error) {
	if len(header) < minVariableSize {
		return Target{}, nil, errBadHeader
	}
	var target Target
	rest := header[1:]
	switch header[0] {
	case addressIPv4, addressIPv6:
		size := 4
		if header[0] == addressIPv6 {
			size = 16
		}
		if len(rest) < size {
			return Target{}, nil, errBadHeader
		}
		address, _ := netip.AddrFromSlice(rest[:size])
		target.Host, rest = address.Unmap().String(), rest[size:]
	case addressDomain:
		size := int(rest[0])
		if size == 0 || len(rest) < 1+size {
			return Target{}, nil, errBadHeader
		}
		for _, character := range rest[1 : 1+size] {
			if character < 0x21 || character > 0x7e {
				return Target{}, nil, errBadHeader
			}
		}
		target.Host, rest = string(rest[1:1+size]), rest[1+size:]
	default:
		return Target{}, nil, errBadHeader
	}
	if len(rest) < 4 {
		return Target{}, nil, errBadHeader
	}
	target.Port = binary.BigEndian.Uint16(rest)
	padding := int(binary.BigEndian.Uint16(rest[2:]))
	rest = rest[4:]
	if target.Port == 0 || padding > maxPaddingLength || len(rest) < padding {
		return Target{}, nil, errBadHeader
	}
	payload := rest[padding:]
	// A request header carries initial payload or nonzero padding.
	if len(payload) == 0 && padding == 0 {
		return Target{}, nil, errBadHeader
	}
	return target, payload, nil
}

// Reject ends a connection that failed authentication without revealing how
// many bytes were consumed: it half-closes and drains until the peer closes
// or the caller's read deadline passes.
func Reject(raw net.Conn) {
	if closer, ok := raw.(interface{ CloseWrite() error }); ok {
		_ = closer.CloseWrite()
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(raw, 1<<20))
	_ = raw.Close()
}

func (c *Conn) Read(p []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	for len(c.pending) == 0 {
		chunk, err := c.nextChunk()
		if err != nil {
			return 0, err
		}
		c.pending = chunk
	}
	n := copy(p, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}

func (c *Conn) nextChunk() ([]byte, error) {
	if _, err := io.ReadFull(c.in, c.lengthChunk[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.EOF
		}
		return nil, fmt.Errorf("shadowsocks: read length chunk: %w", err)
	}
	plain, err := c.opener.open(c.lengthPlain[:0], c.lengthChunk[:])
	if err != nil {
		return nil, errors.New("shadowsocks: length chunk authentication failed")
	}
	sealedLength := int(binary.BigEndian.Uint16(plain)) + tagSize
	if cap(c.buffer) < sealedLength {
		c.buffer = make([]byte, sealedLength, max(sealedLength, 16<<10+tagSize))
	}
	sealed := c.buffer[:sealedLength]
	if _, err := io.ReadFull(c.in, sealed); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, fmt.Errorf("shadowsocks: read payload chunk: %w", err)
	}
	payload, err := c.opener.open(sealed[:0], sealed)
	if err != nil {
		return nil, errors.New("shadowsocks: payload chunk authentication failed")
	}
	return payload, nil
}

// Write sends p as payload chunks in one socket write. The first write opens
// the response stream with its salt and fixed header.
func (c *Conn) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	out := c.out[:0]
	rest := p
	if c.sender == nil {
		salt := make([]byte, saltSize)
		if _, err := rand.Read(salt); err != nil {
			return 0, err
		}
		sender, err := newSealer(c.userKey, salt)
		if err != nil {
			return 0, err
		}
		c.sender = sender
		first := min(len(rest), maxChunkPayload)
		header := make([]byte, 0, 1+8+saltSize+2)
		header = append(header, headerTypeServer)
		header = binary.BigEndian.AppendUint64(header, uint64(c.now().Unix()))
		header = append(header, c.requestSalt...)
		header = binary.BigEndian.AppendUint16(header, uint16(first))
		out = append(out, salt...)
		out = c.sender.seal(out, header)
		out = c.sender.seal(out, rest[:first])
		rest = rest[first:]
	}
	for len(rest) > 0 {
		size := min(len(rest), maxChunkPayload)
		var length [2]byte
		binary.BigEndian.PutUint16(length[:], uint16(size))
		out = c.sender.seal(out, length[:])
		out = c.sender.seal(out, rest[:size])
		rest = rest[size:]
	}
	_, err := c.raw.Write(out)
	if cap(out) <= maxRetainedWrite {
		c.out = out[:0]
	} else {
		c.out = nil
	}
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

// CloseWrite half-closes the underlying connection; Shadowsocks has no
// in-band end of stream.
func (c *Conn) CloseWrite() error {
	if closer, ok := c.raw.(interface{ CloseWrite() error }); ok {
		return closer.CloseWrite()
	}
	return c.raw.Close()
}

func (c *Conn) Close() error                       { return c.raw.Close() }
func (c *Conn) LocalAddr() net.Addr                { return c.raw.LocalAddr() }
func (c *Conn) RemoteAddr() net.Addr               { return c.raw.RemoteAddr() }
func (c *Conn) SetDeadline(t time.Time) error      { return c.raw.SetDeadline(t) }
func (c *Conn) SetReadDeadline(t time.Time) error  { return c.raw.SetReadDeadline(t) }
func (c *Conn) SetWriteDeadline(t time.Time) error { return c.raw.SetWriteDeadline(t) }
