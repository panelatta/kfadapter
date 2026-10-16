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
	"testing"
	"time"
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
		connection, _ := listener.Accept()
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

func randomKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return key
}

// testClient is an independent SIP022/SIP023 client used to drive Accept.
type testClient struct {
	raw     net.Conn
	userKey []byte
	salt    []byte
	sender  *sealer
	in      *bufio.Reader
	opener  *sealer
}

func requestOpening(t *testing.T, serverKey, userKey []byte, timestamp time.Time, variable []byte) ([]byte, []byte, *sealer) {
	t.Helper()
	salt := randomKey(t)
	block, err := aes.NewCipher(deriveKey(identitySubkeyContext, serverKey, salt))
	if err != nil {
		t.Fatal(err)
	}
	hash := keyHash(userKey)
	identity := make([]byte, aesBlockSize)
	block.Encrypt(identity, hash[:])
	sender, err := newSealer(userKey, salt)
	if err != nil {
		t.Fatal(err)
	}
	fixed := []byte{headerTypeClient}
	fixed = binary.BigEndian.AppendUint64(fixed, uint64(timestamp.Unix()))
	fixed = binary.BigEndian.AppendUint16(fixed, uint16(len(variable)))
	out := append(append([]byte(nil), salt...), identity...)
	out = sender.seal(out, fixed)
	out = sender.seal(out, variable)
	return out, salt, sender
}

func variableHeader(target []byte, port uint16, padding int, payload []byte) []byte {
	header := append([]byte(nil), target...)
	header = binary.BigEndian.AppendUint16(header, port)
	header = binary.BigEndian.AppendUint16(header, uint16(padding))
	header = append(header, make([]byte, padding)...)
	return append(header, payload...)
}

func dialTest(t *testing.T, raw net.Conn, serverKey, userKey []byte, variable []byte) *testClient {
	t.Helper()
	opening, salt, sender := requestOpening(t, serverKey, userKey, time.Now(), variable)
	if _, err := raw.Write(opening); err != nil {
		t.Fatal(err)
	}
	return &testClient{raw: raw, userKey: userKey, salt: salt, sender: sender, in: bufio.NewReader(raw)}
}

func (client *testClient) write(t *testing.T, payload []byte) {
	t.Helper()
	var length [2]byte
	binary.BigEndian.PutUint16(length[:], uint16(len(payload)))
	out := client.sender.seal(nil, length[:])
	out = client.sender.seal(out, payload)
	if _, err := client.raw.Write(out); err != nil {
		t.Fatal(err)
	}
}

func (client *testClient) readAll(t *testing.T) []byte {
	t.Helper()
	var data []byte
	for {
		chunk, err := client.readChunk()
		if errors.Is(err, io.EOF) {
			return data
		}
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, chunk...)
	}
}

func (client *testClient) readChunk() ([]byte, error) {
	var length int
	if client.opener == nil {
		salt := make([]byte, saltSize)
		if _, err := io.ReadFull(client.in, salt); err != nil {
			return nil, err
		}
		opener, err := newSealer(client.userKey, salt)
		if err != nil {
			return nil, err
		}
		client.opener = opener
		sealed := make([]byte, 1+8+saltSize+2+tagSize)
		if _, err := io.ReadFull(client.in, sealed); err != nil {
			return nil, err
		}
		header, err := opener.open(nil, sealed)
		if err != nil {
			return nil, err
		}
		if header[0] != headerTypeServer || !bytes.Equal(header[9:9+saltSize], client.salt) || !withinSkew(header[1:9], time.Now()) {
			return nil, errors.New("bad response header")
		}
		length = int(binary.BigEndian.Uint16(header[9+saltSize:]))
	} else {
		sealed := make([]byte, 2+tagSize)
		if _, err := io.ReadFull(client.in, sealed); err != nil {
			return nil, err
		}
		plain, err := client.opener.open(nil, sealed)
		if err != nil {
			return nil, err
		}
		length = int(binary.BigEndian.Uint16(plain))
	}
	if length > maxChunkPayload {
		return nil, errors.New("oversized chunk")
	}
	sealed := make([]byte, length+tagSize)
	if _, err := io.ReadFull(client.in, sealed); err != nil {
		return nil, err
	}
	return client.opener.open(nil, sealed)
}

func TestAcceptIdentifiesUserAndRelays(t *testing.T) {
	clientRaw, serverRaw := pipe(t)
	serverKey, first, second := randomKey(t), randomKey(t), randomKey(t)
	users, err := NewUsers(serverKey, [][]byte{first, second})
	if err != nil {
		t.Fatal(err)
	}
	domain := append([]byte{addressDomain, 16}, "www.bilibili.com"...)
	client := dialTest(t, clientRaw, serverKey, second, variableHeader(domain, 443, 0, []byte("GET / HTTP/1.1\r\n")))
	client.write(t, []byte("rest of request"))
	conn, index, target, err := Accept(serverRaw, users, NewSaltFilter(), time.Now)
	if err != nil || index != 1 || target != (Target{Host: "www.bilibili.com", Port: 443}) {
		t.Fatalf("Accept = %d %+v %v", index, target, err)
	}
	if err := clientRaw.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	upload, err := io.ReadAll(conn)
	if err != nil || string(upload) != "GET / HTTP/1.1\r\nrest of request" {
		t.Fatalf("upload = %q, %v", upload, err)
	}
	download := bytes.Repeat([]byte("video"), 30000)
	if _, err := conn.Write(download); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("tail")); err != nil {
		t.Fatal(err)
	}
	if err := conn.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if got := client.readAll(t); !bytes.Equal(got, append(download, "tail"...)) {
		t.Fatalf("download = %d bytes", len(got))
	}
}

func TestAcceptRejectsWrongKeysReplayAndSkew(t *testing.T) {
	serverKey, userKey := randomKey(t), randomKey(t)
	users, err := NewUsers(serverKey, [][]byte{userKey})
	if err != nil {
		t.Fatal(err)
	}
	ipv4 := []byte{addressIPv4, 1, 2, 3, 4}
	variable := variableHeader(ipv4, 80, 16, nil)
	salts := NewSaltFilter()
	accept := func(opening []byte) error {
		clientRaw, serverRaw := pipe(t)
		if _, err := clientRaw.Write(opening); err != nil {
			t.Fatal(err)
		}
		_, _, _, err := Accept(serverRaw, users, salts, time.Now)
		return err
	}
	valid, _, _ := requestOpening(t, serverKey, userKey, time.Now(), variable)
	if err := accept(valid); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	if err := accept(valid); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("replayed salt: %v", err)
	}
	for name, opening := range map[string][]byte{
		"wrong server key": first(requestOpening(t, randomKey(t), userKey, time.Now(), variable)),
		"unknown user":     first(requestOpening(t, serverKey, randomKey(t), time.Now(), variable)),
		"stale timestamp":  first(requestOpening(t, serverKey, userKey, time.Now().Add(-time.Minute), variable)),
		"future timestamp": first(requestOpening(t, serverKey, userKey, time.Now().Add(time.Minute), variable)),
		"no padding":       first(requestOpening(t, serverKey, userKey, time.Now(), variableHeader(ipv4, 80, 0, nil))),
		"huge padding":     first(requestOpening(t, serverKey, userKey, time.Now(), variableHeader(ipv4, 80, 901, nil))),
		"zero port":        first(requestOpening(t, serverKey, userKey, time.Now(), variableHeader(ipv4, 0, 1, nil))),
		"bad atyp":         first(requestOpening(t, serverKey, userKey, time.Now(), variableHeader([]byte{9, 1, 2, 3, 4}, 80, 1, nil))),
	} {
		if err := accept(opening); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func first(opening, _ []byte, _ *sealer) []byte { return opening }

func TestVariableHeaderTargets(t *testing.T) {
	ipv6 := append([]byte{addressIPv6}, net.ParseIP("2001:db8::1").To16()...)
	mapped := append([]byte{addressIPv6}, net.ParseIP("::ffff:1.2.3.4").To16()...)
	for _, test := range []struct {
		header []byte
		want   Target
	}{
		{variableHeader(ipv6, 443, 0, []byte("x")), Target{Host: "2001:db8::1", Port: 443}},
		{variableHeader(mapped, 80, 3, nil), Target{Host: "1.2.3.4", Port: 80}},
	} {
		target, _, err := parseVariableHeader(test.header)
		if err != nil || target != test.want {
			t.Errorf("parseVariableHeader = %+v, %v; want %+v", target, err, test.want)
		}
	}
	for _, bad := range [][]byte{
		variableHeader([]byte{addressDomain, 0}, 80, 1, nil),
		variableHeader(append([]byte{addressDomain, 3}, "a\nb"...), 80, 1, nil),
		{addressIPv4, 1, 2},
	} {
		if _, _, err := parseVariableHeader(bad); err == nil {
			t.Errorf("header %x accepted", bad)
		}
	}
}

func TestSaltFilterRetainsSaltsForWindow(t *testing.T) {
	filter := NewSaltFilter()
	start := time.Unix(1_700_000_000, 0)
	salt := bytes.Repeat([]byte{1}, saltSize)
	if !filter.Add(salt, start) {
		t.Fatal("fresh salt rejected")
	}
	for _, elapsed := range []time.Duration{time.Second, 59 * time.Second, 61 * time.Second, 119 * time.Second} {
		if filter.Add(salt, start.Add(elapsed)) {
			t.Fatalf("salt accepted again after %s", elapsed)
		}
	}
	if !filter.Add(salt, start.Add(240*time.Second)) {
		t.Fatal("salt still remembered after two windows")
	}
}

func TestNewUsersValidatesKeys(t *testing.T) {
	key := randomKey(t)
	if _, err := NewUsers(key[:8], nil); err == nil {
		t.Fatal("short server key accepted")
	}
	if _, err := NewUsers(key, [][]byte{key[:15]}); err == nil {
		t.Fatal("short user key accepted")
	}
	if _, err := NewUsers(key, [][]byte{key, key}); err == nil {
		t.Fatal("duplicate user keys accepted")
	}
}

func TestClientConnRoundTrip(t *testing.T) {
	for _, test := range []struct {
		name   string
		target Target
		upload []byte
	}{
		{name: "domain with data", target: Target{Host: "www.bilibili.com", Port: 443}, upload: bytes.Repeat([]byte("u"), 100000)},
		{name: "ipv6 padding only", target: Target{Host: "2001:db8::1", Port: 80}},
	} {
		t.Run(test.name, func(t *testing.T) {
			clientRaw, serverRaw := pipe(t)
			serverKey, userKey := randomKey(t), randomKey(t)
			users, err := NewUsers(serverKey, [][]byte{userKey})
			if err != nil {
				t.Fatal(err)
			}
			client, err := NewClient(clientRaw, serverKey, userKey, test.target)
			if err != nil {
				t.Fatal(err)
			}
			go func() {
				if len(test.upload) > 0 {
					_, _ = client.Write(test.upload)
				}
				_ = client.CloseWrite()
			}()
			conn, _, target, err := Accept(serverRaw, users, NewSaltFilter(), time.Now)
			if err != nil || target != test.target {
				t.Fatalf("Accept = %+v, %v", target, err)
			}
			upload, err := io.ReadAll(conn)
			if err != nil || !bytes.Equal(upload, test.upload) {
				t.Fatalf("upload = %d bytes, %v", len(upload), err)
			}
			if _, err := conn.Write([]byte("reply")); err != nil {
				t.Fatal(err)
			}
			_ = conn.CloseWrite()
			if reply, err := io.ReadAll(client); err != nil || string(reply) != "reply" {
				t.Fatalf("reply = %q, %v", reply, err)
			}
		})
	}
}

func BenchmarkResponseThroughput(b *testing.B) {
	serverRaw, clientRaw := net.Pipe()
	defer serverRaw.Close()
	go func() { _, _ = io.Copy(io.Discard, clientRaw) }()
	conn := &Conn{raw: serverRaw, userKey: make([]byte, KeySize), requestSalt: make([]byte, saltSize), now: time.Now}
	payload := make([]byte, 32<<10)
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for range b.N {
		if _, err := conn.Write(payload); err != nil {
			b.Fatal(err)
		}
	}
}
