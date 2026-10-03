package smart

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kfadapter/kfadapter/internal/provider"
)

type bridgeTransport struct{ active atomic.Int32 }

func (*bridgeTransport) Protocol() provider.Protocol { return "test-native" }
func (b *bridgeTransport) RelayDatagrams(context.Context, provider.DatagramRequest) error {
	return provider.ErrCommandUnsupported
}
func (b *bridgeTransport) RelayStream(ctx context.Context, r provider.RelayRequest) error {
	b.active.Add(1)
	defer b.active.Add(-1)
	conn, err := r.Dial(ctx, "tcp", net.JoinHostPort(r.NodeHost, strconv.Itoa(int(r.NodePort))))
	if err != nil {
		return err
	}
	defer conn.Close()
	if !r.Admit() {
		return provider.ErrNoSession
	}
	if err := r.Ready(conn.LocalAddr()); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close(); _ = r.Client.Close() })
	defer stop()
	done := make(chan struct{})
	go func() { defer close(done); _, _ = io.Copy(conn, r.Client); _ = conn.Close() }()
	_, err = io.Copy(r.Client, conn)
	_ = r.Client.Close()
	<-done
	return err
}
func TestHTTPSProbeUsesTunnelVerifiesTLSAndDoesNotFollowRedirects(t *testing.T) {
	service, _ := harness(t, rankedProbe)
	node := service.manager.Current().Nodes[0]
	pin, err := service.manager.CompactPin(node.Selector, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var status atomic.Int32
	status.Store(http.StatusOK)
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if status.Load() == 0 {
			<-r.Context().Done()
			return
		}
		w.Header().Set("Location", "https://never-follow.example/")
		w.WriteHeader(int(status.Load()))
	}))
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	config := &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	transport := &bridgeTransport{}
	providers, err := provider.NewRegistry(nil, []provider.Transport{transport})
	if err != nil {
		t.Fatal(err)
	}
	var dialMu sync.Mutex
	var dialed []string
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		dialMu.Lock()
		dialed = append(dialed, address)
		dialMu.Unlock()
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	target := Target{Name: "test", URL: "https://127.0.0.1/"}
	for _, code := range []int{200, 302, 403, 429, 503} {
		status.Store(int32(code))
		before := requests.Load()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		result := measureHTTPS(ctx, pin, target, service.manager, providers, dial, config)
		cancel()
		if result.OK != (code < 400) || (code >= 400 && result.Error != "http_rejected") {
			t.Fatalf("HTTP %d: %+v", code, result)
		}
		if requests.Load() != before+1 {
			t.Fatal("redirect followed or request not tunneled")
		}
		if transport.active.Load() != 0 {
			t.Fatal("probe returned before relay drained")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	untrusted := measureHTTPS(ctx, pin, target, service.manager, providers, dial, nil)
	cancel()
	if untrusted.OK {
		t.Fatal("untrusted TLS accepted")
	}
	status.Store(0)
	ctx, cancel = context.WithTimeout(context.Background(), 50*time.Millisecond)
	timeout := measureHTTPS(ctx, pin, target, service.manager, providers, dial, config)
	cancel()
	if timeout.Error != "timeout" || transport.active.Load() != 0 {
		t.Fatal("timeout did not drain", timeout)
	}
	dialMu.Lock()
	defer dialMu.Unlock()
	for _, address := range dialed {
		if address != net.JoinHostPort(pin.Node.Host, "443") {
			t.Fatal("direct target dial", address)
		}
	}
}

func TestRevokedProbeAuthorityCannotMeasureDestination(t *testing.T) {
	service, _ := harness(t, rankedProbe)
	pin, err := service.manager.CompactPin(service.manager.Current().Nodes[0].Selector, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	service.manager.SignOut()
	transport := &bridgeTransport{}
	providers, err := provider.NewRegistry(nil, []provider.Transport{transport})
	if err != nil {
		t.Fatal(err)
	}
	dial := func(context.Context, string, string) (net.Conn, error) {
		a, b := net.Pipe()
		_ = b.Close()
		return a, nil
	}
	result := measureHTTPS(context.Background(), pin, Target{Name: "test", URL: "https://127.0.0.1/"}, service.manager, providers, dial, nil)
	if result.OK {
		t.Fatal("revoked session accepted")
	}
}
