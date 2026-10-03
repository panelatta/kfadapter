package smart

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/kfadapter/kfadapter/internal/provider"
	"github.com/kfadapter/kfadapter/internal/state"
)

// tunnelProbe measures provider handshake, verified TLS, and HTTP headers.
// Destination traffic never falls back to a direct connection.
func tunnelProbe(manager *state.Manager, providers *provider.Registry, dial provider.DialContextFunc) func(context.Context, state.TunnelPin, Target) Measurement {
	return func(ctx context.Context, pin state.TunnelPin, target Target) Measurement {
		return measureHTTPS(ctx, pin, target, manager, providers, dial, nil)
	}
}
func measureHTTPS(ctx context.Context, pin state.TunnelPin, target Target, manager *state.Manager, providers *provider.Registry, dial provider.DialContextFunc, tlsConfig *tls.Config) Measurement {
	started := time.Now()
	result := Measurement{Target: target.Name}
	destination, err := url.Parse(target.URL)
	if err != nil || destination.Scheme != "https" || destination.Hostname() == "" {
		result.Error = "invalid_target"
		return result
	}
	transport, err := providers.Transport(pin.Node.Protocol)
	if err != nil {
		result.Error = "transport_unavailable"
		return result
	}
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	requestCtx, cancel := context.WithCancel(ctx)
	clientPipe, relay := net.Pipe()
	ready := make(chan error, 1)
	done := make(chan struct{})
	stopClose := context.AfterFunc(requestCtx, func() { _ = relay.Close(); _ = clientPipe.Close() })
	go func() {
		defer close(done)
		defer relay.Close()
		err := transport.RelayStream(requestCtx, provider.RelayRequest{
			Dial: dial, Client: relay, NodeHost: pin.Node.Host, NodePort: pin.Node.Port,
			Target: provider.Target{Host: destination.Hostname(), Port: 443}, Authority: pin.Authority.Data,
			HandshakeTimeout: 8 * time.Second,
			Admit:            func() bool { return manager.SessionCurrentPin(pin, time.Now()) },
			Ready:            func(net.Addr) error { ready <- nil; return nil },
		})
		if err == nil {
			err = net.ErrClosed
		}
		select {
		case ready <- err:
		default:
		}
	}()
	defer func() {
		cancel()
		_ = clientPipe.Close()
		_ = relay.Close()
		stopClose()
		// Do not let cancelled rounds accumulate background provider handshakes.
		<-done
	}()
	select {
	case err = <-ready:
	case <-ctx.Done():
		err = ctx.Err()
	}
	if err != nil {
		result.Error = probeError(ctx, err)
		return result
	}
	if tlsConfig == nil {
		tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	httpTransport := &http.Transport{
		Proxy: nil, DisableKeepAlives: true, DisableCompression: true,
		TLSClientConfig: tlsConfig, MaxResponseHeaderBytes: 32 << 10,
		DialContext: func(context.Context, string, string) (net.Conn, error) { return clientPipe, nil },
	}
	defer httpTransport.CloseIdleConnections()
	client := &http.Client{Transport: httpTransport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, target.URL, nil)
	if err != nil {
		result.Error = "invalid_target"
		return result
	}
	request.Header.Set("User-Agent", "Mozilla/5.0 (compatible; kfadapter-probe/1.0)")
	response, err := client.Do(request)
	if err != nil {
		result.Error = probeError(ctx, err)
		return result
	}
	latency := time.Since(started).Milliseconds()
	_ = response.Body.Close()
	// Explicit HTTP rejections must not win merely because they respond quickly.
	if response.StatusCode < 200 || response.StatusCode >= 400 {
		result.Error = "http_rejected"
		return result
	}
	result.OK, result.LatencyMS = true, max(1, latency)
	return result
}
func probeError(ctx context.Context, err error) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "request_failed"
}
