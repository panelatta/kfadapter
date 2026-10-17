package shadowsocks

import (
	"bufio"
	"bytes"
	"crypto/aes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"
)

// ClientConn is the client side of a request stream. It exists for tests and
// interoperability checks; the daemon only accepts Shadowsocks connections.
type ClientConn struct {
	raw     net.Conn
	userKey []byte
	salt    []byte

	writeMu sync.Mutex
	sender  *sealer
	header  []byte

	readMu  sync.Mutex
	in      *bufio.Reader
	opener  *sealer
	pending []byte
}

// NewClient prepares a request for target through the identity of
// serverKey:userKey. The request header leaves with the first Write, or
// with random padding on CloseWrite or the first Read.
func NewClient(raw net.Conn, serverKey, userKey []byte, target Target) (*ClientConn, error) {
	salt := make([]byte, saltSize)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(deriveKey(identitySubkeyContext, serverKey, salt))
	if err != nil {
		return nil, err
	}
	hash := keyHash(userKey)
	identity := make([]byte, aesBlockSize)
	block.Encrypt(identity, hash[:])
	sender, err := newSealer(userKey, salt)
	if err != nil {
		return nil, err
	}
	address, err := appendAddress(nil, target)
	if err != nil {
		return nil, err
	}
	return &ClientConn{
		raw: raw, userKey: append([]byte(nil), userKey...), salt: salt, sender: sender,
		header: append(append(append([]byte(nil), salt...), identity...), address...), in: bufio.NewReader(raw),
	}, nil
}

func appendAddress(dst []byte, target Target) ([]byte, error) {
	if address, err := netip.ParseAddr(target.Host); err == nil {
		if address.Is4() {
			raw := address.As4()
			dst = append(append(dst, addressIPv4), raw[:]...)
		} else {
			raw := address.As16()
			dst = append(append(dst, addressIPv6), raw[:]...)
		}
	} else {
		if target.Host == "" || len(target.Host) > 255 {
			return nil, errBadHeader
		}
		dst = append(append(dst, addressDomain, byte(len(target.Host))), target.Host...)
	}
	return binary.BigEndian.AppendUint16(dst, target.Port), nil
}

func (c *ClientConn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	rest := p
	var out []byte
	if c.header != nil {
		initial := rest[:min(len(rest), maxChunkPayload-len(c.header))]
		var err error
		if out, err = c.requestHeader(initial); err != nil {
			return 0, err
		}
		rest = rest[len(initial):]
	}
	for len(rest) > 0 {
		size := min(len(rest), maxChunkPayload)
		var length [2]byte
		binary.BigEndian.PutUint16(length[:], uint16(size))
		out = c.sender.seal(out, length[:])
		out = c.sender.seal(out, rest[:size])
		rest = rest[size:]
	}
	if _, err := c.raw.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}

// requestHeader returns the encoded request header carrying payload as its
// initial data, or nil once the header was sent.
func (c *ClientConn) requestHeader(payload []byte) ([]byte, error) {
	if c.header == nil {
		return nil, nil
	}
	if len(payload) > maxChunkPayload-len(c.header) {
		return nil, errors.New("shadowsocks: initial payload too large")
	}
	salt, identity, address := c.header[:saltSize], c.header[saltSize:saltSize+aesBlockSize], c.header[saltSize+aesBlockSize:]
	padding := 0
	if len(payload) == 0 {
		var random [1]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, err
		}
		padding = 1 + int(random[0])
	}
	variable := append([]byte(nil), address...)
	variable = binary.BigEndian.AppendUint16(variable, uint16(padding))
	variable = append(append(variable, make([]byte, padding)...), payload...)
	fixed := []byte{headerTypeClient}
	fixed = binary.BigEndian.AppendUint64(fixed, uint64(time.Now().Unix()))
	fixed = binary.BigEndian.AppendUint16(fixed, uint16(len(variable)))
	out := append(append([]byte(nil), salt...), identity...)
	out = c.sender.seal(out, fixed)
	out = c.sender.seal(out, variable)
	c.header = nil
	return out, nil
}

func (c *ClientConn) flushHeader() error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	out, err := c.requestHeader(nil)
	if err != nil || out == nil {
		return err
	}
	_, err = c.raw.Write(out)
	return err
}

// CloseWrite sends a pending request header and half-closes the connection.
func (c *ClientConn) CloseWrite() error {
	if err := c.flushHeader(); err != nil {
		return err
	}
	if closer, ok := c.raw.(interface{ CloseWrite() error }); ok {
		return closer.CloseWrite()
	}
	return nil
}

func (c *ClientConn) Read(p []byte) (int, error) {
	if err := c.flushHeader(); err != nil {
		return 0, err
	}
	c.readMu.Lock()
	defer c.readMu.Unlock()
	for len(c.pending) == 0 {
		chunk, err := c.readChunk()
		if err != nil {
			return 0, err
		}
		c.pending = chunk
	}
	n := copy(p, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}

func (c *ClientConn) readChunk() ([]byte, error) {
	var length int
	if c.opener == nil {
		salt := make([]byte, saltSize)
		if _, err := io.ReadFull(c.in, salt); err != nil {
			return nil, err
		}
		opener, err := newSealer(c.userKey, salt)
		if err != nil {
			return nil, err
		}
		sealed := make([]byte, 1+8+saltSize+2+tagSize)
		if _, err := io.ReadFull(c.in, sealed); err != nil {
			return nil, unexpected(err)
		}
		header, err := opener.open(nil, sealed)
		if err != nil || header[0] != headerTypeServer || !bytes.Equal(header[9:9+saltSize], c.salt) || !withinSkew(header[1:9], time.Now()) {
			return nil, ErrUnauthenticated
		}
		c.opener = opener
		length = int(binary.BigEndian.Uint16(header[9+saltSize:]))
	} else {
		sealed := make([]byte, 2+tagSize)
		if _, err := io.ReadFull(c.in, sealed); err != nil {
			return nil, err
		}
		plain, err := c.opener.open(nil, sealed)
		if err != nil {
			return nil, ErrUnauthenticated
		}
		length = int(binary.BigEndian.Uint16(plain))
	}
	sealed := make([]byte, length+tagSize)
	if _, err := io.ReadFull(c.in, sealed); err != nil {
		return nil, unexpected(err)
	}
	payload, err := c.opener.open(nil, sealed)
	if err != nil {
		return nil, ErrUnauthenticated
	}
	return payload, nil
}

func unexpected(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

func (c *ClientConn) Close() error                       { return c.raw.Close() }
func (c *ClientConn) LocalAddr() net.Addr                { return c.raw.LocalAddr() }
func (c *ClientConn) RemoteAddr() net.Addr               { return c.raw.RemoteAddr() }
func (c *ClientConn) SetDeadline(t time.Time) error      { return c.raw.SetDeadline(t) }
func (c *ClientConn) SetReadDeadline(t time.Time) error  { return c.raw.SetReadDeadline(t) }
func (c *ClientConn) SetWriteDeadline(t time.Time) error { return c.raw.SetWriteDeadline(t) }
