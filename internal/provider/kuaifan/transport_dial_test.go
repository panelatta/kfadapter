package kuaifan

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/oshinop/chinadapter/internal/provider"
)

type closeRecordingConn struct {
	net.Conn
	closed bool
}

func (c *closeRecordingConn) Close() error {
	c.closed = true
	return nil
}

// A dial that succeeds just as the caller cancels must not leak the socket.
func TestOpenWIFIINClosesUpstreamWhenCancelledAfterDial(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	upstream := &closeRecordingConn{}
	dial := func(context.Context, string, string) (net.Conn, error) {
		cancel()
		return upstream, nil
	}
	_, _, _, _, err := openWIFIIN(ctx, dial, "node.example", 8080,
		provider.Target{Host: "target.example", Port: 443}, tunnelAuthority{Password: "password", Extension: "|token|cc.fancast.major|order|7|MAC|1.0.46"}, time.Second, false)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("openWIFIIN error = %v, want context.Canceled", err)
	}
	if !upstream.closed {
		t.Fatal("upstream connection leaked after cancellation")
	}
}

func TestOpenWIFIINAbortsHandshakeOnCancellation(t *testing.T) {
	var relaySide net.Conn
	dial := func(context.Context, string, string) (net.Conn, error) {
		adapterSide, remote := net.Pipe()
		relaySide = remote // never read: the handshake write blocks
		return adapterSide, nil
	}
	defer func() {
		if relaySide != nil {
			_ = relaySide.Close()
		}
	}()
	authority := tunnelAuthority{Password: "password", Method: "aes-256-cfb", Extension: "|provider-token|cc.fancast.major|order|user|MAC|1.0.46"}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	started := time.Now()
	_, _, _, _, err := openWIFIIN(ctx, dial, "192.0.2.1", 443, provider.Target{Host: "example.com", Port: 443}, authority, 10*time.Second, false)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("handshake error = %v, want cancellation", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("cancelled handshake took %s", elapsed)
	}
}
