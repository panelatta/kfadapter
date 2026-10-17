package snell

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestOfficialServerInterop drives the client side of this record layer
// against an official snell-server v6 in unshaped mode, which pins the wire
// format the daemon's server side shares. It runs only when
// CHINADAPTER_SNELL_SERVER (host:port) and CHINADAPTER_SNELL_PSK are set;
// CHINADAPTER_SNELL_TARGET_HOST names this machine as the server sees it.
func TestOfficialServerInterop(t *testing.T) {
	serverAddress, psk := os.Getenv("CHINADAPTER_SNELL_SERVER"), os.Getenv("CHINADAPTER_SNELL_PSK")
	if serverAddress == "" || psk == "" {
		t.Skip("CHINADAPTER_SNELL_SERVER and CHINADAPTER_SNELL_PSK are not set")
	}
	targetHost := os.Getenv("CHINADAPTER_SNELL_TARGET_HOST")
	if targetHost == "" {
		targetHost = "127.0.0.1"
	}
	body := strings.Repeat("snell-interop ", 30000)
	httpListener, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer httpListener.Close()
	go func() {
		_ = http.Serve(httpListener, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			upload, _ := io.ReadAll(r.Body)
			w.Header().Set("X-Upload", strconv.Itoa(len(upload)))
			_, _ = io.WriteString(w, body)
		}))
	}()
	httpPort := uint16(httpListener.Addr().(*net.TCPAddr).Port)

	raw, err := net.DialTimeout("tcp", serverAddress, testTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(30 * time.Second))
	client, err := Client(raw, []byte(psk))
	if err != nil {
		t.Fatal(err)
	}
	upload := bytes.Repeat([]byte("u"), 70000)
	for round := range 3 {
		request := "POST /" + strconv.Itoa(round) + " HTTP/1.1\r\nHost: interop\r\nConnection: close\r\nContent-Length: " + strconv.Itoa(len(upload)) + "\r\n\r\n"
		if err := client.WriteStream(AppendConnectRequest(nil, targetHost, httpPort, true), append([]byte(request), upload...)); err != nil {
			t.Fatal(err)
		}
		response := readResponse(t, client)
		if len(response) == 0 || response[0] != ResponseTunnel {
			t.Fatalf("round %d: response starts with %x", round, response[:min(len(response), 8)])
		}
		parsed, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(response[1:])), nil)
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		got, _ := io.ReadAll(parsed.Body)
		if string(got) != body || parsed.Header.Get("X-Upload") != strconv.Itoa(len(upload)) {
			t.Fatalf("round %d: body %d bytes, upload header %q", round, len(got), parsed.Header.Get("X-Upload"))
		}
		// The official server closed the target after the response; finish
		// this request in band so the connection serves the next one.
		if err := client.WriteZeroChunk(); err != nil {
			t.Fatal(err)
		}
	}

	echo, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		buffer := make([]byte, 65536)
		for {
			n, source, err := echo.ReadFromUDPAddrPort(buffer)
			if err != nil {
				return
			}
			_, _ = echo.WriteToUDPAddrPort(append([]byte("echo:"), buffer[:n]...), source)
		}
	}()
	udpRaw, err := net.DialTimeout("tcp", serverAddress, testTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer udpRaw.Close()
	_ = udpRaw.SetDeadline(time.Now().Add(30 * time.Second))
	udpClient, err := Client(udpRaw, []byte(psk))
	if err != nil {
		t.Fatal(err)
	}
	if err := udpClient.WriteRecord([]byte{Version, CommandUDP, 0}); err != nil {
		t.Fatal(err)
	}
	reply, err := udpClient.ReadRecord()
	if err != nil || !bytes.Equal(reply, []byte{ResponseTunnel}) {
		t.Fatalf("UDP reply = %x, %v", reply, err)
	}
	echoPort, prefix := uint16(echo.LocalAddr().(*net.UDPAddr).Port), []byte("echo:")
	echoHost := targetHost
	// CHINADAPTER_SNELL_UDP_ECHO names a plain echo service the server can
	// reach when this machine's UDP is not reachable from it.
	if remote := os.Getenv("CHINADAPTER_SNELL_UDP_ECHO"); remote != "" {
		address, err := netip.ParseAddrPort(remote)
		if err != nil {
			t.Fatal(err)
		}
		echoHost, echoPort, prefix = address.Addr().String(), address.Port(), nil
	}
	for _, size := range []int{21, 1400, 4000} {
		payload := bytes.Repeat([]byte{byte(size)}, size)
		// A domain frame lets the server resolve a host name only it knows.
		frame := append([]byte{udpForward, byte(len(echoHost))}, echoHost...)
		frame = append(append(frame, byte(echoPort>>8), byte(echoPort)), payload...)
		if address, err := netip.ParseAddr(echoHost); err == nil {
			frame = AppendDatagram(nil, netip.AddrPortFrom(address, echoPort), payload)
		}
		if err := udpClient.WriteRecord(frame); err != nil {
			t.Fatal(err)
		}
		frame, err := udpClient.ReadRecord()
		if err != nil {
			t.Fatal(err)
		}
		source, data, err := parseDatagramResponse(frame)
		if err != nil || source.Port() != echoPort || !bytes.Equal(data, append(prefix, payload...)) {
			t.Fatalf("UDP %d: source %v, %d bytes, %v", size, source, len(data), err)
		}
	}
}

func parseDatagramResponse(frame []byte) (netip.AddrPort, []byte, error) {
	size := 0
	switch {
	case len(frame) > 0 && frame[0] == 0x04:
		size = 4
	case len(frame) > 0 && frame[0] == 0x06:
		size = 16
	default:
		return netip.AddrPort{}, nil, errors.New("unknown family")
	}
	if len(frame) < 1+size+2 {
		return netip.AddrPort{}, nil, errors.New("short frame")
	}
	address, _ := netip.AddrFromSlice(frame[1 : 1+size])
	port := uint16(frame[1+size])<<8 | uint16(frame[2+size])
	return netip.AddrPortFrom(address.Unmap(), port), frame[3+size:], nil
}
