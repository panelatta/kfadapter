package shadowsocks

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"io"
	"net"
	"os"
	"testing"
	"time"
)

// TestClientInterop lets an independent Shadowsocks 2022 client (for example
// sing-box with password "serverKey:userKey") reach this server through its
// SOCKS5 inbound. It runs only when CHINADAPTER_SS_SOCKS (the client's SOCKS5
// address), CHINADAPTER_SS_LISTEN (where this server listens for it) and the
// base64 keys CHINADAPTER_SS_SERVER_KEY and CHINADAPTER_SS_USER_KEY are set.
func TestClientInterop(t *testing.T) {
	socksAddress, listen := os.Getenv("CHINADAPTER_SS_SOCKS"), os.Getenv("CHINADAPTER_SS_LISTEN")
	serverKey, serverErr := base64.StdEncoding.DecodeString(os.Getenv("CHINADAPTER_SS_SERVER_KEY"))
	userKey, userErr := base64.StdEncoding.DecodeString(os.Getenv("CHINADAPTER_SS_USER_KEY"))
	if socksAddress == "" || listen == "" || serverErr != nil || userErr != nil || len(serverKey) == 0 || len(userKey) == 0 {
		t.Skip("Shadowsocks interop environment is not set")
	}
	users, err := NewUsers(serverKey, [][]byte{randomKey(t), userKey})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	download := bytes.Repeat([]byte("response "), 40000)
	upload := bytes.Repeat([]byte("request "), 30000)
	served := make(chan error, 1)
	go func() {
		raw, err := listener.Accept()
		if err != nil {
			served <- err
			return
		}
		defer raw.Close()
		_ = raw.SetDeadline(time.Now().Add(30 * time.Second))
		conn, index, target, err := Accept(raw, users, NewSaltFilter(), time.Now)
		if err != nil {
			served <- err
			return
		}
		if index != 1 || target != (Target{Host: "interop.example", Port: 8443}) {
			t.Errorf("Accept = %d %+v", index, target)
		}
		got := make([]byte, len(upload))
		if _, err := io.ReadFull(conn, got); err != nil || !bytes.Equal(got, upload) {
			t.Errorf("upload mismatch: %v", err)
		}
		if _, err := conn.Write(download); err != nil {
			served <- err
			return
		}
		served <- conn.CloseWrite()
	}()

	client, err := net.DialTimeout("tcp", socksAddress, testTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(30 * time.Second))
	request := []byte{5, 1, 0, 5, 1, 0, 3, byte(len("interop.example"))}
	request = binary.BigEndian.AppendUint16(append(request, "interop.example"...), 8443)
	if _, err := client.Write(request); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 2+10)
	if _, err := io.ReadFull(client, reply); err != nil || reply[0] != 5 || reply[1] != 0 || reply[3] != 0 {
		t.Fatalf("SOCKS reply %x, %v", reply, err)
	}
	if _, err := client.Write(upload); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(client)
	if err != nil || !bytes.Equal(got, download) {
		t.Fatalf("download = %d bytes, %v", len(got), err)
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
}
