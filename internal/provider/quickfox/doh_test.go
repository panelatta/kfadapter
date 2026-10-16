package quickfox

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/oshinop/chinadapter/internal/provider"
)

type dohTestQuery struct {
	name   string
	subnet netip.Prefix
}

type dohTestServer struct {
	*httptest.Server
	requests atomic.Int32
	mu       sync.Mutex
	queries  []dohTestQuery
}

// newDoHTestServer validates every RFC 8484 query and lets respond write the
// answer for its question name and ECS subnet.
func newDoHTestServer(t *testing.T, respond func(http.ResponseWriter, *http.Request, dohTestQuery)) *dohTestServer {
	t.Helper()
	server := &dohTestServer{}
	server.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		server.requests.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil || r.Method != http.MethodPost || r.URL.Path != "/dns-query" ||
			r.Header.Get("Content-Type") != dohMediaType || r.Header.Get("Accept") != dohMediaType {
			t.Errorf("DoH request %s %s %q, body error %v", r.Method, r.URL.Path, r.Header.Get("Content-Type"), err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var message dnsmessage.Message
		if err := message.Unpack(body); err != nil || message.ID != 0 || message.Response || !message.RecursionDesired ||
			len(message.Questions) != 1 || message.Questions[0].Type != dnsmessage.TypeA || message.Questions[0].Class != dnsmessage.ClassINET {
			t.Errorf("DoH query %+v, unpack error %v", message, err)
			http.Error(w, "bad query", http.StatusBadRequest)
			return
		}
		query := dohTestQuery{name: message.Questions[0].Name.String()}
		for _, additional := range message.Additionals {
			options, ok := additional.Body.(*dnsmessage.OPTResource)
			if !ok {
				continue
			}
			for _, option := range options.Options {
				if option.Code != ednsClientSubnet {
					continue
				}
				// Family IPv4, scope zero, and only the prefix's leading octets.
				if len(option.Data) != 7 || option.Data[0] != 0 || option.Data[1] != 1 || option.Data[3] != 0 {
					t.Errorf("ECS option % x", option.Data)
					continue
				}
				query.subnet = netip.PrefixFrom(netip.AddrFrom4([4]byte{option.Data[4], option.Data[5], option.Data[6], 0}), int(option.Data[2]))
			}
		}
		server.mu.Lock()
		server.queries = append(server.queries, query)
		server.mu.Unlock()
		respond(w, r, query)
	}))
	t.Cleanup(server.Close)
	return server
}

func (server *dohTestServer) lastQuery() dohTestQuery {
	server.mu.Lock()
	defer server.mu.Unlock()
	if len(server.queries) == 0 {
		return dohTestQuery{}
	}
	return server.queries[len(server.queries)-1]
}

type dohTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (clock *dohTestClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *dohTestClock) Advance(duration time.Duration) {
	clock.mu.Lock()
	clock.now = clock.now.Add(duration)
	clock.mu.Unlock()
}

// dohTestTransport resolves through server and reports any system resolver
// use through fallbacks.
func dohTestTransport(t *testing.T, server *dohTestServer) (*Transport, *dohTestClock, *atomic.Int32) {
	t.Helper()
	resolver, err := newDoHResolver(server.URL+"/dns-query", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	clock := &dohTestClock{now: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}
	resolver.clock = clock.Now
	transport := NewTransport()
	transport.doh = resolver
	fallbacks := &atomic.Int32{}
	transport.lookup = func(_ context.Context, network, _ string) ([]netip.Addr, error) {
		fallbacks.Add(1)
		if network != "ip4" {
			t.Errorf("system lookup network %q", network)
		}
		return []netip.Addr{netip.MustParseAddr("203.0.113.80")}, nil
	}
	return transport, clock, fallbacks
}

func writeDoHAnswer(t *testing.T, w http.ResponseWriter, query dohTestQuery, rcode dnsmessage.RCode, answers ...dnsmessage.Resource) {
	t.Helper()
	message := dnsmessage.Message{
		Header:    dnsmessage.Header{Response: true, RecursionDesired: true, RecursionAvailable: true, RCode: rcode},
		Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName(query.name), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}},
		Answers:   answers,
	}
	packed, err := message.Pack()
	if err != nil {
		t.Error(err)
		return
	}
	w.Header().Set("Content-Type", dohMediaType)
	_, _ = w.Write(packed)
}

func aRecord(name string, ttl uint32, address string) dnsmessage.Resource {
	return dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName(name), Class: dnsmessage.ClassINET, TTL: ttl},
		Body:   &dnsmessage.AResource{A: netip.MustParseAddr(address).As4()},
	}
}

func cnameRecord(name string, ttl uint32, target string) dnsmessage.Resource {
	return dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName(name), Class: dnsmessage.ClassINET, TTL: ttl},
		Body:   &dnsmessage.CNAMEResource{CNAME: dnsmessage.MustNewName(target)},
	}
}

func TestDoHResolvesTargetsForTheNodeSubnet(t *testing.T) {
	t.Parallel()
	server := newDoHTestServer(t, func(w http.ResponseWriter, _ *http.Request, query dohTestQuery) {
		// Records off the CNAME chain are ignored, and private answers on
		// it are still filtered.
		writeDoHAnswer(t, w, query, dnsmessage.RCodeSuccess,
			aRecord("unrelated.example.", 60, "198.51.100.1"),
			cnameRecord(query.name, 60, "edge.cdn.example."),
			aRecord("EDGE.cdn.example.", 60, "10.1.2.3"),
			aRecord("edge.cdn.example.", 60, "34.160.111.145"),
		)
	})
	transport, _, fallbacks := dohTestTransport(t, server)
	address, err := transport.resolveTarget(context.Background(), "Video.Example.", "198.51.100.77", time.Second)
	if err != nil || address != netip.MustParseAddr("34.160.111.145") {
		t.Fatalf("resolved %v, %v", address, err)
	}
	if query := server.lastQuery(); query.name != "video.example." || query.subnet != netip.MustParsePrefix("198.51.100.0/24") {
		t.Fatalf("DoH query = %+v", query)
	}
	if fallbacks.Load() != 0 {
		t.Fatalf("system resolver used %d times", fallbacks.Load())
	}
}

func TestDoHCachesAnswersPerNodeSubnet(t *testing.T) {
	t.Parallel()
	var ttl atomic.Uint32
	ttl.Store(5)
	server := newDoHTestServer(t, func(w http.ResponseWriter, _ *http.Request, query dohTestQuery) {
		writeDoHAnswer(t, w, query, dnsmessage.RCodeSuccess, aRecord(query.name, ttl.Load(), "34.160.111.145"))
	})
	transport, clock, _ := dohTestTransport(t, server)
	resolve := func(node string) {
		t.Helper()
		if _, err := transport.resolveTarget(context.Background(), "video.example", node, time.Second); err != nil {
			t.Fatal(err)
		}
	}
	resolve("198.51.100.77")
	resolve("198.51.100.78")
	if got := server.requests.Load(); got != 1 {
		t.Fatalf("requests within one /24 = %d, want 1", got)
	}
	resolve("192.0.2.10")
	if got := server.requests.Load(); got != 2 {
		t.Fatalf("requests for a second /24 = %d, want 2", got)
	}
	// A 5-second TTL is held for the 30-second minimum.
	clock.Advance(minDoHTTL - time.Second)
	resolve("198.51.100.77")
	if got := server.requests.Load(); got != 2 {
		t.Fatalf("requests before minimum TTL = %d, want 2", got)
	}
	ttl.Store(3600)
	clock.Advance(2 * time.Second)
	resolve("198.51.100.77")
	if got := server.requests.Load(); got != 3 {
		t.Fatalf("requests after minimum TTL = %d, want 3", got)
	}
	// A one-hour TTL is cut to the five-minute maximum.
	clock.Advance(maxDoHTTL + time.Second)
	resolve("198.51.100.77")
	if got := server.requests.Load(); got != 4 {
		t.Fatalf("requests after maximum TTL = %d, want 4", got)
	}
}

func TestDoHNegativeAnswersAreFinalAndCached(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		rcode   dnsmessage.RCode
		answers []dnsmessage.Resource
	}{
		{name: "NXDOMAIN", rcode: dnsmessage.RCodeNameError},
		{name: "no data", rcode: dnsmessage.RCodeSuccess},
		{name: "dangling CNAME", rcode: dnsmessage.RCodeSuccess, answers: []dnsmessage.Resource{
			cnameRecord("missing.example.", 60, "edge.cdn.example."), aRecord("other.cdn.example.", 60, "34.160.111.145"),
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := newDoHTestServer(t, func(w http.ResponseWriter, _ *http.Request, query dohTestQuery) {
				writeDoHAnswer(t, w, query, test.rcode, test.answers...)
			})
			transport, _, fallbacks := dohTestTransport(t, server)
			for range 2 {
				address, err := transport.resolveTarget(context.Background(), "missing.example", "198.51.100.77", time.Second)
				if address.IsValid() || !errors.Is(err, provider.ErrAddressUnsupported) {
					t.Fatalf("negative answer resolved to %v, %v", address, err)
				}
			}
			if server.requests.Load() != 1 || fallbacks.Load() != 0 {
				t.Fatalf("requests %d, system lookups %d", server.requests.Load(), fallbacks.Load())
			}
		})
	}
}

func TestDoHFailuresFallBackAndBackOff(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		respond func(*testing.T, http.ResponseWriter, *http.Request, dohTestQuery)
	}{
		{name: "HTTP error", respond: func(_ *testing.T, w http.ResponseWriter, _ *http.Request, _ dohTestQuery) {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}},
		{name: "redirect", respond: func(_ *testing.T, w http.ResponseWriter, r *http.Request, _ dohTestQuery) {
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		}},
		{name: "SERVFAIL", respond: func(t *testing.T, w http.ResponseWriter, _ *http.Request, query dohTestQuery) {
			writeDoHAnswer(t, w, query, dnsmessage.RCodeServerFailure)
		}},
		{name: "content type", respond: func(t *testing.T, w http.ResponseWriter, _ *http.Request, query dohTestQuery) {
			w.Header().Set("Content-Type", "application/dns-json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("{}"))
		}},
		{name: "mismatched question", respond: func(t *testing.T, w http.ResponseWriter, _ *http.Request, _ dohTestQuery) {
			writeDoHAnswer(t, w, dohTestQuery{name: "other.example."}, dnsmessage.RCodeSuccess, aRecord("other.example.", 60, "34.160.111.145"))
		}},
		{name: "oversized", respond: func(_ *testing.T, w http.ResponseWriter, _ *http.Request, _ dohTestQuery) {
			w.Header().Set("Content-Type", dohMediaType)
			_, _ = w.Write(bytes.Repeat([]byte{0}, maxDoHMessage+1))
		}},
		{name: "malformed", respond: func(_ *testing.T, w http.ResponseWriter, _ *http.Request, _ dohTestQuery) {
			w.Header().Set("Content-Type", dohMediaType)
			_, _ = w.Write([]byte{0, 0, 0x81})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := newDoHTestServer(t, func(w http.ResponseWriter, r *http.Request, query dohTestQuery) { test.respond(t, w, r, query) })
			transport, clock, fallbacks := dohTestTransport(t, server)
			resolve := func(host string) {
				t.Helper()
				address, err := transport.resolveTarget(context.Background(), host, "198.51.100.77", time.Second)
				if err != nil || address != netip.MustParseAddr("203.0.113.80") {
					t.Fatalf("fallback resolved %v, %v", address, err)
				}
			}
			resolve("video.example")
			resolve("image.example")
			if server.requests.Load() != 1 || fallbacks.Load() != 2 {
				t.Fatalf("within backoff: requests %d, system lookups %d", server.requests.Load(), fallbacks.Load())
			}
			clock.Advance(dohBackoff)
			resolve("video.example")
			if server.requests.Load() != 2 || fallbacks.Load() != 3 {
				t.Fatalf("after backoff: requests %d, system lookups %d", server.requests.Load(), fallbacks.Load())
			}
		})
	}
}

func TestDoHBudgetLeavesTimeForTheSystemResolver(t *testing.T) {
	t.Parallel()
	server := newDoHTestServer(t, func(_ http.ResponseWriter, r *http.Request, _ dohTestQuery) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	transport, _, fallbacks := dohTestTransport(t, server)
	const timeout = 400 * time.Millisecond
	started := time.Now()
	address, err := transport.resolveTarget(context.Background(), "video.example", "198.51.100.77", timeout)
	if err != nil || address != netip.MustParseAddr("203.0.113.80") || fallbacks.Load() != 1 {
		t.Fatalf("stalled resolver: %v, %v, %d system lookups", address, err, fallbacks.Load())
	}
	if elapsed := time.Since(started); elapsed >= timeout {
		t.Fatalf("fallback took %s, beyond the %s handshake timeout", elapsed, timeout)
	}
}

func TestDoHCallerCancellationDoesNotBackOff(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{}, 1)
	var stall atomic.Bool
	stall.Store(true)
	server := newDoHTestServer(t, func(w http.ResponseWriter, r *http.Request, query dohTestQuery) {
		if stall.Load() {
			entered <- struct{}{}
			<-r.Context().Done()
			return
		}
		writeDoHAnswer(t, w, query, dnsmessage.RCodeSuccess, aRecord(query.name, 60, "34.160.111.145"))
	})
	transport, _, fallbacks := dohTestTransport(t, server)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-entered
		cancel()
	}()
	if address, err := transport.resolveTarget(ctx, "video.example", "198.51.100.77", 5*time.Second); address.IsValid() || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled resolution = %v, %v", address, err)
	}
	stall.Store(false)
	address, err := transport.resolveTarget(context.Background(), "video.example", "198.51.100.77", time.Second)
	if err != nil || address != netip.MustParseAddr("34.160.111.145") || fallbacks.Load() != 0 {
		t.Fatalf("after cancellation: %v, %v, %d system lookups", address, err, fallbacks.Load())
	}
}

func TestDoHSkipsUnencodableNamesAndNonIPv4Nodes(t *testing.T) {
	t.Parallel()
	server := newDoHTestServer(t, func(w http.ResponseWriter, _ *http.Request, query dohTestQuery) {
		writeDoHAnswer(t, w, query, dnsmessage.RCodeSuccess, aRecord(query.name, 60, "34.160.111.145"))
	})
	transport, _, fallbacks := dohTestTransport(t, server)
	for _, test := range []struct{ host, node string }{
		{host: "empty..label.example", node: "198.51.100.77"},
		{host: strings.Repeat("a", 64) + ".example", node: "198.51.100.77"},
		{host: "video.example", node: "relay.example"},
		{host: "video.example", node: "2001:db8::1"},
		{host: "video.example", node: ""},
	} {
		address, err := transport.resolveTarget(context.Background(), test.host, test.node, time.Second)
		if err != nil || address != netip.MustParseAddr("203.0.113.80") {
			t.Fatalf("%q via %q resolved %v, %v", test.host, test.node, address, err)
		}
	}
	if server.requests.Load() != 0 || fallbacks.Load() != 5 {
		t.Fatalf("requests %d, system lookups %d", server.requests.Load(), fallbacks.Load())
	}
	// None of those started the backoff window.
	address, err := transport.resolveTarget(context.Background(), "video.example", "198.51.100.77", time.Second)
	if err != nil || address != netip.MustParseAddr("34.160.111.145") || server.requests.Load() != 1 {
		t.Fatalf("DoH after skipped lookups: %v, %v, %d requests", address, err, server.requests.Load())
	}
}

func TestDoHCacheIsBounded(t *testing.T) {
	t.Parallel()
	resolver, err := newDoHResolver("https://dns.example/dns-query", nil)
	if err != nil {
		t.Fatal(err)
	}
	clock := &dohTestClock{now: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}
	resolver.clock = clock.Now
	subnet := netip.MustParsePrefix("198.51.100.0/24")
	expired := dohCacheKey{host: "expired.example", subnet: subnet}
	resolver.store(expired, dohCacheEntry{expires: clock.Now().Add(time.Second)})
	for index := range maxDoHCacheEntries + 10 {
		key := dohCacheKey{host: "host" + strconv.Itoa(index) + ".example", subnet: subnet}
		resolver.store(key, dohCacheEntry{expires: clock.Now().Add(time.Minute)})
		if index == 0 {
			clock.Advance(2 * time.Second)
		}
	}
	if len(resolver.cache) > maxDoHCacheEntries {
		t.Fatalf("cache holds %d entries, limit %d", len(resolver.cache), maxDoHCacheEntries)
	}
	if _, ok := resolver.cache[expired]; ok {
		t.Fatal("expired entry survived eviction")
	}
}

func TestDoHStreamAndDatagramPathsUseTheSelectedNode(t *testing.T) {
	t.Parallel()
	server := newDoHTestServer(t, func(w http.ResponseWriter, _ *http.Request, query dohTestQuery) {
		writeDoHAnswer(t, w, query, dnsmessage.RCodeSuccess, aRecord(query.name, 60, "34.160.111.145"))
	})
	transport, _, _ := dohTestTransport(t, server)

	client, adapter := loopbackTCPPair(t)
	control, controlAdapter := loopbackTCPPair(t)
	upstream, upstreamAdapter := loopbackTCPPair(t)
	dataPreface := make(chan []byte, 1)
	go func() {
		if err := readNativePreface(control); err != nil {
			t.Error(err)
			return
		}
		if err := writeAll(control, observedControlACK[:]); err != nil {
			t.Error(err)
			return
		}
		preface := make([]byte, 62)
		if _, err := io.ReadFull(upstream, preface); err != nil {
			t.Error(err)
		}
		dataPreface <- preface
	}()
	dials := 0
	request := nativeRelayRequest(t, adapter, func(context.Context, string, string) (net.Conn, error) {
		dials++
		if dials == 1 {
			return controlAdapter, nil
		}
		return upstreamAdapter, nil
	})
	request.NodeHost = "127.0.0.9"
	request.Target.Host = "video.example"
	ready := make(chan struct{})
	request.Ready = func(net.Addr) error { close(ready); return nil }
	relayErr := make(chan error, 1)
	go func() { relayErr <- transport.RelayStream(context.Background(), request) }()
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("relay did not become ready")
	}
	select {
	case preface := <-dataPreface:
		if netip.AddrFrom4([4]byte(preface[52:56])) != netip.MustParseAddr("34.160.111.145") {
			t.Fatalf("data preface target % x", preface[52:56])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("relay did not send the data preface")
	}
	if query := server.lastQuery(); query.subnet != netip.MustParsePrefix("127.0.0.0/24") {
		t.Fatalf("stream DoH subnet = %s", query.subnet)
	}
	_ = client.Close()
	_ = upstream.Close()
	<-relayErr

	writer := &datagramWriter{transport: transport, ctx: context.Background(), node: "192.0.2.10", timeout: time.Second}
	if address, ok := writer.resolve("voice.example"); !ok || address != netip.MustParseAddr("34.160.111.145") {
		t.Fatalf("datagram resolution = %v, %v", address, ok)
	}
	if query := server.lastQuery(); query.name != "voice.example." || query.subnet != netip.MustParsePrefix("192.0.2.0/24") {
		t.Fatalf("datagram DoH query = %+v", query)
	}
}

func TestNewTransportWithDoHValidatesTheEndpoint(t *testing.T) {
	t.Parallel()
	for _, endpoint := range []string{"https://223.5.5.5/dns-query", "https://dns.alidns.com/dns-query", "https://dns.example:8443/resolve"} {
		if transport, err := NewTransportWithDoH(endpoint); err != nil || transport.doh == nil {
			t.Fatalf("NewTransportWithDoH(%q) = %v", endpoint, err)
		}
	}
	for _, endpoint := range []string{
		"", "dns.example", "http://dns.example/dns-query", "https:///dns-query", "https://user@dns.example/dns-query",
		"https://dns.example/dns-query?dns=x", "https://dns.example/dns-query?", "https://dns.example/dns-query#", "https:dns.example",
	} {
		if _, err := NewTransportWithDoH(endpoint); err == nil {
			t.Fatalf("accepted DoH endpoint %q", endpoint)
		}
	}
	insecure := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	if _, err := newDoHResolver("https://dns.example/dns-query", insecure); err == nil {
		t.Fatal("accepted an insecure TLS client")
	}
}
