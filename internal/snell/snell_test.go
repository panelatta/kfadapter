package snell

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"
)

const testTimeout = 5 * time.Second

func pipe(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			accepted <- nil
			return
		}
		accepted <- connection
	}()
	client, err := net.Dial("tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server := <-accepted
	if server == nil {
		t.Fatal("accept failed")
	}
	for _, connection := range []net.Conn{client, server} {
		_ = connection.SetDeadline(time.Now().Add(testTimeout))
		t.Cleanup(func() { _ = connection.Close() })
	}
	return client, server
}

func readResponse(t *testing.T, conn *Conn) []byte {
	t.Helper()
	var data []byte
	buffer := make([]byte, 4096)
	for {
		n, err := conn.Read(buffer)
		data = append(data, buffer[:n]...)
		if errors.Is(err, ErrZeroChunk) {
			return data
		}
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
	}
}

func TestAcceptIdentifiesPSKAndReusesConnection(t *testing.T) {
	clientRaw, serverRaw := pipe(t)
	psk := []byte("node-psk")
	client, err := Client(clientRaw, psk)
	if err != nil {
		t.Fatal(err)
	}
	upload := bytes.Repeat([]byte("upload-"), 5000)
	go func() {
		request := AppendConnectRequest(nil, "www.bilibili.com", 443, true)
		_ = client.WriteStream(request, upload)
		_ = client.WriteZeroChunk()
	}()
	server, index, err := Accept(serverRaw, [][]byte{[]byte("other"), []byte("another"), psk})
	if err != nil || index != 2 {
		t.Fatalf("Accept = %d, %v", index, err)
	}
	for round := range 3 {
		request, err := ReadRequest(server)
		if err != nil {
			t.Fatal(err)
		}
		if request != (Request{Command: CommandConnectV2, Host: "www.bilibili.com", Port: 443}) {
			t.Fatalf("request %d = %+v", round, request)
		}
		session := NewSession(server)
		got, err := io.ReadAll(session)
		if err != nil || !bytes.Equal(got, upload) {
			t.Fatalf("round %d upload = %d bytes, %v", round, len(got), err)
		}
		download := bytes.Repeat([]byte{byte(round)}, 40000)
		if _, err := session.Write(download); err != nil {
			t.Fatal(err)
		}
		if err := session.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		if !session.Finished() {
			t.Fatal("session did not finish in band")
		}
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
		response := readResponse(t, client)
		if len(response) != 1+len(download) || response[0] != ResponseTunnel || !bytes.Equal(response[1:], download) {
			t.Fatalf("round %d response = %d bytes", round, len(response))
		}
		if round < 2 {
			go func() {
				_ = client.WriteStream(AppendConnectRequest(nil, "www.bilibili.com", 443, true), upload)
				_ = client.WriteZeroChunk()
			}()
		}
	}
}

func TestAcceptRejectsUnknownPSK(t *testing.T) {
	clientRaw, serverRaw := pipe(t)
	client, err := Client(clientRaw, []byte("right"))
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = client.WriteStream(AppendConnectRequest(nil, "example.com", 80, false), nil) }()
	if _, _, err := Accept(serverRaw, [][]byte{[]byte("wrong")}); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("Accept error = %v", err)
	}
}

// TestRecordWireFormat pins the unshaped layout independently of Conn: the
// salt, then AES-128-GCM(Argon2id key) over a 7-byte header and the payload
// with a little-endian counter nonce and no associated data.
func TestRecordWireFormat(t *testing.T) {
	psk, salt := []byte("v6-wire"), bytes.Repeat([]byte{0xa0}, SaltSize)
	sender, err := newSealer(psk, salt)
	if err != nil {
		t.Fatal(err)
	}
	var wire bytes.Buffer
	conn := &Conn{raw: writerConn{&wire}, sender: sender, salt: salt}
	payload := []byte("hello snell")
	if err := conn.WriteRecord(payload); err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteZeroChunk(); err != nil {
		t.Fatal(err)
	}

	block, _ := aes.NewCipher(argon2.IDKey(psk, salt, 3, 8, 1, 32)[:16])
	aead, _ := cipher.NewGCM(block)
	nonce := func(counter uint64) []byte {
		value := make([]byte, 12)
		binary.LittleEndian.PutUint64(value, counter)
		return value
	}
	want := append([]byte(nil), salt...)
	want = aead.Seal(want, nonce(0), []byte{4, 0, 0, 0, 0, 0, byte(len(payload))}, nil)
	want = aead.Seal(want, nonce(1), payload, nil)
	want = aead.Seal(want, nonce(2), []byte{4, 0, 0, 0, 0, 0, 0}, nil)
	if !bytes.Equal(wire.Bytes(), want) {
		t.Fatalf("wire mismatch\n got %x\nwant %x", wire.Bytes(), want)
	}
}

type writerConn struct{ io.Writer }

func (writerConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (writerConn) Close() error                     { return nil }
func (writerConn) LocalAddr() net.Addr              { return nil }
func (writerConn) RemoteAddr() net.Addr             { return nil }
func (writerConn) SetDeadline(time.Time) error      { return nil }
func (writerConn) SetReadDeadline(time.Time) error  { return nil }
func (writerConn) SetWriteDeadline(time.Time) error { return nil }

func TestHeaderRejectsPaddingAndReservedBytes(t *testing.T) {
	for _, header := range [][]byte{
		{4, 0, 0, 0, 1, 0, 1}, {4, 1, 0, 0, 0, 0, 1}, {5, 0, 0, 0, 0, 0, 1}, {4, 0, 0},
	} {
		if _, err := decodeHeader(header); err == nil {
			t.Fatalf("header %x accepted", header)
		}
	}
}

func TestSessionAbortsUnlessFinishedInBand(t *testing.T) {
	clientRaw, serverRaw := pipe(t)
	psk := []byte("k")
	client, _ := Client(clientRaw, psk)
	go func() { _ = client.WriteStream(AppendConnectRequest(nil, "example.com", 80, true), []byte("x")) }()
	server, _, err := Accept(serverRaw, [][]byte{psk})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRequest(server); err != nil {
		t.Fatal(err)
	}
	session := NewSession(server)
	if err := session.Fail(1, "upstream refused"); err != nil {
		t.Fatal(err)
	}
	if session.Finished() {
		t.Fatal("failed session reported in-band completion")
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	response, err := client.ReadRecord()
	if err != nil || !bytes.Equal(response, ErrorResponse(1, "upstream refused")) {
		t.Fatalf("error response = %x, %v", response, err)
	}
	if _, err := client.ReadRecord(); err == nil {
		t.Fatal("aborted connection stayed open")
	}
}

func TestReadRequestValidation(t *testing.T) {
	for _, test := range []struct {
		name  string
		input []byte
		want  Request
		fails bool
	}{
		{name: "ping", input: []byte{1, 0}, want: Request{Command: CommandPing}},
		{name: "udp", input: []byte{1, 6, 0}, want: Request{Command: CommandUDP}},
		{name: "client id", input: append([]byte{1, 1, 2, 'i', 'd', 7}, append([]byte("1.2.3.4"), 0, 80)...), want: Request{Command: CommandConnect, Host: "1.2.3.4", Port: 80}},
		{name: "bracketed ipv6", input: append([]byte{1, 5, 0, 5}, append([]byte("[::1]"), 1, 187)...), want: Request{Command: CommandConnectV2, Host: "::1", Port: 443}},
		{name: "bad version", input: []byte{2, 1, 0}, fails: true},
		{name: "unknown command", input: []byte{1, 9, 0}, fails: true},
		{name: "empty host", input: []byte{1, 1, 0, 0, 0, 80}, fails: true},
		{name: "zero port", input: append([]byte{1, 1, 0, 1, 'a'}, 0, 0), fails: true},
		{name: "control byte", input: append([]byte{1, 1, 0, 2, 'a', '\n'}, 0, 80), fails: true},
	} {
		request, err := ReadRequest(bytes.NewReader(test.input))
		if test.fails != (err != nil) || !test.fails && request != test.want {
			t.Errorf("%s: ReadRequest = %+v, %v", test.name, request, err)
		}
	}
}

func TestDatagramFraming(t *testing.T) {
	destination := netip.MustParseAddrPort("8.8.8.8:53")
	frame := AppendDatagram(nil, destination, []byte("query"))
	datagram, err := ParseDatagram(frame)
	if err != nil || datagram.Addr != destination.Addr() || datagram.Port != 53 || string(datagram.Payload) != "query" {
		t.Fatalf("ParseDatagram = %+v, %v", datagram, err)
	}
	domain := append([]byte{1, 11}, append([]byte("example.com"), 0, 53, 'q')...)
	if datagram, err := ParseDatagram(domain); err != nil || datagram.Host != "example.com" || datagram.Port != 53 || string(datagram.Payload) != "q" {
		t.Fatalf("domain datagram = %+v, %v", datagram, err)
	}
	mapped := append([]byte{1, 0, 6}, netip.MustParseAddr("::ffff:1.2.3.4").AsSlice()...)
	if datagram, err := ParseDatagram(append(mapped, 0, 53)); err != nil || datagram.Addr != netip.MustParseAddr("1.2.3.4") {
		t.Fatalf("mapped datagram = %+v, %v", datagram, err)
	}
	for _, bad := range [][]byte{{1}, {2, 0, 4}, {1, 0, 5, 1, 2}, {1, 0, 4, 1, 2, 3}, {1, 3, 'a', 'b'}} {
		if _, err := ParseDatagram(bad); err == nil {
			t.Fatalf("frame %x accepted", bad)
		}
	}
	response := AppendDatagramResponse(nil, netip.MustParseAddrPort("[::ffff:1.2.3.4]:53"), []byte("answer"))
	if !bytes.Equal(response, append([]byte{4, 1, 2, 3, 4, 0, 53}, "answer"...)) {
		t.Fatalf("response = %x", response)
	}
}

func TestLargeWritesSplitIntoBoundedRecords(t *testing.T) {
	clientRaw, serverRaw := pipe(t)
	psk := []byte("k")
	client, _ := Client(clientRaw, psk)
	data := bytes.Repeat([]byte("0123456789"), 10000)
	go func() {
		_ = client.WriteStream(AppendConnectRequest(nil, "example.com", 80, false), data)
	}()
	server, _, err := Accept(serverRaw, [][]byte{psk})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRequest(server); err != nil {
		t.Fatal(err)
	}
	var got []byte
	for len(got) < len(data) {
		record, err := server.ReadRecord()
		if err != nil {
			t.Fatal(err)
		}
		if len(record) > streamRecordSize {
			t.Fatalf("record of %d bytes exceeds %d", len(record), streamRecordSize)
		}
		got = append(got, record...)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("payload mismatch")
	}
}

func BenchmarkStreamThroughput(b *testing.B) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	defer listener.Close()
	psk := []byte("benchmark-psk")
	go func() {
		raw, err := listener.Accept()
		if err != nil {
			return
		}
		server, _, err := Accept(raw, [][]byte{psk})
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, server)
	}()
	raw, err := net.Dial("tcp4", listener.Addr().String())
	if err != nil {
		b.Fatal(err)
	}
	defer raw.Close()
	client, err := Client(raw, psk)
	if err != nil {
		b.Fatal(err)
	}
	payload := make([]byte, 32<<10)
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for range b.N {
		if _, err := client.Write(payload); err != nil {
			b.Fatal(err)
		}
	}
}
