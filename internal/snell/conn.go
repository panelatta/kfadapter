package snell

import (
	"bufio"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

const (
	readBufferSize = 32 << 10
	// streamRecordSize caps stream records like TLS does, so a lossy path
	// never holds more than 16 KiB of payload behind one missing segment.
	streamRecordSize = 16 << 10
	maxRetainedWrite = 256 << 10
)

// Conn is one Snell record stream. Reads and writes may run concurrently;
// each direction is serialized internally.
type Conn struct {
	raw net.Conn
	psk []byte

	readMu      sync.Mutex
	in          *bufio.Reader
	opener      *sealer
	firstHeader []byte
	sealedHead  [sealedHeaderSize]byte
	headBuffer  [headerSize]byte
	buffer      []byte
	record      []byte
	offset      int

	writeMu sync.Mutex
	sender  *sealer
	salt    []byte
	out     []byte
}

// Handshake is a client's opening bytes: its salt and the sealed header of
// its first record.
type Handshake struct {
	raw     net.Conn
	in      *bufio.Reader
	opening [SaltSize + sealedHeaderSize]byte
}

// ReadHandshake reads a client's opening bytes. The caller bounds the read
// with a deadline.
func ReadHandshake(raw net.Conn) (*Handshake, error) {
	handshake := &Handshake{raw: raw, in: bufio.NewReaderSize(raw, readBufferSize)}
	if _, err := io.ReadFull(handshake.in, handshake.opening[:]); err != nil {
		return nil, fmt.Errorf("snell: read handshake: %w", err)
	}
	return handshake, nil
}

// Accept identifies the client among candidate pre-shared keys by opening
// its first record header, and returns the stream and the index of the
// matching key. Argon2id runs once per candidate tried.
func (h *Handshake) Accept(candidates [][]byte) (*Conn, int, error) {
	salt, sealed := h.opening[:SaltSize], h.opening[SaltSize:]
	var scratch [headerSize]byte
	for index, psk := range candidates {
		opener, err := newSealer(psk, salt)
		if err != nil {
			return nil, -1, err
		}
		header, err := opener.open(scratch[:0], sealed)
		if err != nil {
			continue
		}
		conn, err := newConn(h.raw, h.in, psk)
		if err != nil {
			return nil, -1, err
		}
		conn.opener = opener
		conn.firstHeader = append(conn.headBuffer[:0], header...)
		return conn, index, nil
	}
	return nil, -1, ErrUnauthenticated
}

// Accept reads a handshake from raw and identifies its pre-shared key.
func Accept(raw net.Conn, candidates [][]byte) (*Conn, int, error) {
	handshake, err := ReadHandshake(raw)
	if err != nil {
		return nil, -1, err
	}
	return handshake.Accept(candidates)
}

// Client starts the client side of a record stream. It exists for tests and
// interoperability checks; the daemon only accepts Snell connections.
func Client(raw net.Conn, psk []byte) (*Conn, error) {
	return newConn(raw, bufio.NewReaderSize(raw, readBufferSize), psk)
}

func newConn(raw net.Conn, in *bufio.Reader, psk []byte) (*Conn, error) {
	salt := make([]byte, SaltSize)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	sender, err := newSealer(psk, salt)
	if err != nil {
		return nil, err
	}
	return &Conn{raw: raw, psk: append([]byte(nil), psk...), in: in, sender: sender, salt: salt}, nil
}

// Read returns record payload bytes as a stream. A zero chunk ends the
// current request with ErrZeroChunk; the next Read continues with the next
// request's records. A clean EOF between records is io.EOF.
func (c *Conn) Read(p []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	for c.offset >= len(c.record) {
		record, err := c.nextRecord()
		if err != nil {
			return 0, err
		}
		c.record, c.offset = record, 0
	}
	n := copy(p, c.record[c.offset:])
	c.offset += n
	return n, nil
}

// ReadRecord returns the unread rest of the current record or, when it is
// consumed, the whole next record. The slice is valid until the next read.
func (c *Conn) ReadRecord() ([]byte, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	if c.offset < len(c.record) {
		rest := c.record[c.offset:]
		c.record, c.offset = nil, 0
		return rest, nil
	}
	record, err := c.nextRecord()
	c.record, c.offset = nil, 0
	return record, err
}

func (c *Conn) nextRecord() ([]byte, error) {
	header := c.firstHeader
	c.firstHeader = nil
	if header == nil {
		if c.opener == nil {
			var salt [SaltSize]byte
			if _, err := io.ReadFull(c.in, salt[:]); err != nil {
				return nil, err
			}
			opener, err := newSealer(c.psk, salt[:])
			if err != nil {
				return nil, err
			}
			c.opener = opener
		}
		if _, err := io.ReadFull(c.in, c.sealedHead[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return nil, io.EOF
			}
			return nil, fmt.Errorf("snell: read record header: %w", err)
		}
		var err error
		if header, err = c.opener.open(c.headBuffer[:0], c.sealedHead[:]); err != nil {
			return nil, errBadRecord
		}
	}
	length, err := decodeHeader(header)
	if err != nil {
		return nil, err
	}
	if length == 0 {
		return nil, ErrZeroChunk
	}
	sealedLength := length + tagSize
	if cap(c.buffer) < sealedLength {
		c.buffer = make([]byte, sealedLength, max(sealedLength, streamRecordSize+tagSize))
	}
	sealed := c.buffer[:sealedLength]
	if _, err := io.ReadFull(c.in, sealed); err != nil {
		return nil, fmt.Errorf("snell: read record payload: %w", unexpectedEOF(err))
	}
	payload, err := c.opener.open(sealed[:0], sealed)
	if err != nil {
		return nil, errBadRecord
	}
	return payload, nil
}

func unexpectedEOF(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// Write sends p as stream records in a single socket write.
func (c *Conn) Write(p []byte) (int, error) {
	if err := c.WriteStream(nil, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// WriteStream sends prefix followed by p as stream records in one socket
// write. It lets a response byte share the first record with relayed data.
func (c *Conn) WriteStream(prefix, p []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	out := c.begin()
	if len(prefix) > 0 {
		head := min(len(p), streamRecordSize-len(prefix))
		record := append(append(make([]byte, 0, len(prefix)+head), prefix...), p[:head]...)
		out = c.appendRecord(out, record)
		p = p[head:]
	}
	for len(p) > 0 {
		size := min(len(p), streamRecordSize)
		out = c.appendRecord(out, p[:size])
		p = p[size:]
	}
	return c.flush(out)
}

// WriteRecord sends exactly one record whose payload is the concatenation of
// parts, keeping datagram boundaries. Empty parts send a zero chunk.
func (c *Conn) WriteRecord(parts ...[]byte) error {
	size := 0
	for _, part := range parts {
		size += len(part)
	}
	if size > MaxRecordPayload {
		return fmt.Errorf("snell: record payload of %d bytes exceeds %d", size, MaxRecordPayload)
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	out := c.begin()
	if len(parts) == 1 {
		out = c.appendRecord(out, parts[0])
	} else {
		payload := make([]byte, 0, size)
		for _, part := range parts {
			payload = append(payload, part...)
		}
		out = c.appendRecord(out, payload)
	}
	return c.flush(out)
}

// WriteZeroChunk half-closes the current request in band.
func (c *Conn) WriteZeroChunk() error { return c.WriteRecord() }

func (c *Conn) begin() []byte {
	out := c.out[:0]
	if c.salt != nil {
		out = append(out, c.salt...)
		c.salt = nil
	}
	return out
}

func (c *Conn) appendRecord(out, payload []byte) []byte {
	header := encodeHeader(len(payload))
	out = c.sender.seal(out, header[:])
	if len(payload) > 0 {
		out = c.sender.seal(out, payload)
	}
	return out
}

func (c *Conn) flush(out []byte) error {
	_, err := c.raw.Write(out)
	if cap(out) <= maxRetainedWrite {
		c.out = out[:0]
	} else {
		c.out = nil
	}
	return err
}

func (c *Conn) Close() error                       { return c.raw.Close() }
func (c *Conn) LocalAddr() net.Addr                { return c.raw.LocalAddr() }
func (c *Conn) RemoteAddr() net.Addr               { return c.raw.RemoteAddr() }
func (c *Conn) SetDeadline(t time.Time) error      { return c.raw.SetDeadline(t) }
func (c *Conn) SetReadDeadline(t time.Time) error  { return c.raw.SetReadDeadline(t) }
func (c *Conn) SetWriteDeadline(t time.Time) error { return c.raw.SetWriteDeadline(t) }
