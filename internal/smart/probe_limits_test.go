package smart

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kfadapter/kfadapter/internal/provider"
)

func TestProbeCancellationBeforeTransportReadyDrainsRelay(t *testing.T) {
	service, _ := harness(t, rankedProbe)
	pin, err := service.manager.CompactPin(service.manager.Current().Nodes[0].Selector, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	transport := &bridgeTransport{}
	providers, err := provider.NewRegistry(nil, []provider.Transport{transport})
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan Measurement, 1)
	go func() {
		result <- measureHTTPS(ctx, pin, Target{Name: "test", URL: "https://127.0.0.1/"}, service.manager, providers, dial, nil)
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("transport dial did not begin")
	}
	cancel()
	select {
	case measurement := <-result:
		if measurement.OK || transport.active.Load() != 0 {
			t.Fatal("cancelled probe succeeded or returned with an active relay")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not drain pending transport setup")
	}
}

func TestProbeRejectsOversizedHTTPSHeadersAndDrainsRelay(t *testing.T) {
	service, _ := harness(t, rankedProbe)
	pin, err := service.manager.CompactPin(service.manager.Current().Nodes[0].Selector, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Oversized", strings.Repeat("a", 40<<10))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	transport := &bridgeTransport{}
	providers, err := provider.NewRegistry(nil, []provider.Transport{transport})
	if err != nil {
		t.Fatal(err)
	}
	dial := func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	measurement := measureHTTPS(ctx, pin, Target{Name: "test", URL: "https://127.0.0.1/"}, service.manager, providers, dial, &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots})
	if measurement.OK || measurement.Error != "request_failed" || transport.active.Load() != 0 {
		t.Fatalf("oversized headers accepted or relay left running: ok=%v, error=%s, active=%d", measurement.OK, measurement.Error, transport.active.Load())
	}
}
