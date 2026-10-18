package shadowsocks

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
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

// TestUDPClientInterop lets an independent Shadowsocks 2022 client reach this
// server's UDP relay through the client's SOCKS5 UDP ASSOCIATE. It runs only
// when CHINADAPTER_SS_SOCKS (the client's SOCKS5 address), the base64 keys,
// and CHINADAPTER_SS_UDP_LISTEN (where this server listens for UDP) are set.
// The server answers each datagram with "echo:" and its payload.
func TestUDPClientInterop(t *testing.T) {
	socksAddress, listen := os.Getenv("CHINADAPTER_SS_SOCKS"), os.Getenv("CHINADAPTER_SS_UDP_LISTEN")
	serverKey, serverErr := base64.StdEncoding.DecodeString(os.Getenv("CHINADAPTER_SS_SERVER_KEY"))
	userKey, userErr := base64.StdEncoding.DecodeString(os.Getenv("CHINADAPTER_SS_USER_KEY"))
	if socksAddress == "" || listen == "" || serverErr != nil || userErr != nil || len(serverKey) == 0 || len(userKey) == 0 {
		t.Skip("Shadowsocks UDP interop environment is not set")
	}
	users, err := NewUsers(serverKey, [][]byte{randomKey(t), userKey})
	if err != nil {
		t.Fatal(err)
	}
	server, err := net.ListenPacket("udp", listen)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	sessions := make(map[uint64]*UDPSession)
	go func() {
		buffer := make([]byte, 65535)
		for {
			n, source, err := server.ReadFrom(buffer)
			if err != nil {
				return
			}
			identity, found := users.IdentifyPacket(buffer[:n])
			if !found || identity.User != 1 {
				t.Errorf("unidentified packet from %s", source)
				continue
			}
			session := sessions[identity.SessionID]
			if session == nil {
				if session, err = NewUDPSession(users, identity, time.Now); err != nil {
					t.Error(err)
					continue
				}
				sessions[identity.SessionID] = session
			}
			opened, err := session.Open(identity, buffer[:n])
			if err != nil {
				t.Errorf("open: %v", err)
				continue
			}
			address := opened[:len(opened)-len(socksPayload(opened))]
			reply := append(append([]byte(nil), address...), append([]byte("echo:"), socksPayload(opened)...)...)
			if _, err := server.WriteTo(session.Seal(nil, reply), source); err != nil {
				t.Error(err)
			}
		}
	}()

	for association := range 2 {
		control, err := net.DialTimeout("tcp", socksAddress, testTimeout)
		if err != nil {
			t.Fatal(err)
		}
		_ = control.SetDeadline(time.Now().Add(30 * time.Second))
		if _, err := control.Write([]byte{5, 1, 0, 5, 3, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
			t.Fatal(err)
		}
		// Method selection, then VER REP RSV ATYP BND.ADDR BND.PORT.
		reply := make([]byte, 2+10)
		if _, err := io.ReadFull(control, reply); err != nil || reply[1] != 0 || reply[3] != 0 || reply[5] != addressIPv4 {
			t.Fatalf("UDP ASSOCIATE reply %x, %v", reply, err)
		}
		host := net.IP(reply[6:10]).String()
		if net.IP(reply[6:10]).IsUnspecified() {
			host, _, _ = net.SplitHostPort(socksAddress)
		}
		relay, err := net.Dial("udp", net.JoinHostPort(host, fmt.Sprint(binary.BigEndian.Uint16(reply[10:12]))))
		if err != nil {
			t.Fatal(err)
		}
		_ = relay.SetDeadline(time.Now().Add(30 * time.Second))
		for sequence := range 20 {
			payload := fmt.Sprintf("association %d datagram %d", association, sequence)
			request := append([]byte{0, 0, 0, addressDomain, byte(len("interop.example"))}, "interop.example"...)
			request = binary.BigEndian.AppendUint16(request, 53)
			if _, err := relay.Write(append(request, payload...)); err != nil {
				t.Fatal(err)
			}
			response := make([]byte, 2048)
			n, err := relay.Read(response)
			if err != nil {
				t.Fatalf("association %d datagram %d: %v", association, sequence, err)
			}
			if got := string(socksPayload(response[3:n])); got != "echo:"+payload {
				t.Fatalf("association %d datagram %d reply = %q", association, sequence, got)
			}
		}
		_ = relay.Close()
		_ = control.Close()
	}
	if len(sessions) != 2 {
		t.Fatalf("sessions = %d, want one per association", len(sessions))
	}
}

// socksPayload returns the payload after the SOCKS address at packet's start.
func socksPayload(packet []byte) []byte {
	switch packet[0] {
	case addressIPv4:
		return packet[1+4+2:]
	case addressIPv6:
		return packet[1+16+2:]
	default:
		return packet[2+int(packet[1])+2:]
	}
}
