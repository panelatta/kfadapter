package quickfox

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// The relay preface carries only an IPv4 destination, so domain targets are
// resolved on the adapter host. Its own resolver answers for the host's
// network, and CDN GSLBs then pick edges for that network (overseas or
// another carrier) instead of the selected node's. The optional DoH resolver
// sends the node's /24 as EDNS Client Subnet (RFC 7871) so answers match the
// network the relay connects from.
const (
	dohMediaType       = "application/dns-message"
	dohSubnetBits      = 24
	dohUDPPayload      = 1232
	ednsClientSubnet   = 8
	maxDoHMessage      = 65535
	maxDoHCacheEntries = 1024
	maxCNAMEHops       = 8
	minDoHTTL          = 30 * time.Second
	maxDoHTTL          = 5 * time.Minute
	negativeDoHTTL     = 30 * time.Second
	// maxDoHBudget bounds one lookup so a stalled resolver leaves the
	// handshake time for the system resolver fallback.
	maxDoHBudget = 3 * time.Second
	// dohBackoff skips the resolver after it fails so an outage costs at
	// most one budget per backoff window instead of one per connection.
	dohBackoff = 30 * time.Second
	// dohNameRetry keeps one name on the system resolver after its lookup
	// failed upstream, so a slow authoritative server costs one budget per
	// window for that name without pausing DoH for other names.
	dohNameRetry = 30 * time.Second
	// dohNameFailureLimit distinct names failing in a row, with no answer in
	// between, mean the resolver itself is struggling.
	dohNameFailureLimit = 3
)

var (
	// errDoHNoAddress reports an authoritative NXDOMAIN or an answer without
	// IPv4 records for the name.
	errDoHNoAddress = errors.New("quickfox: DoH answer has no IPv4 address")
	// errDoHUnavailable marks resolver failures that fall back to the
	// system resolver and start the backoff window.
	errDoHUnavailable = errors.New("quickfox: DoH resolver unavailable")
	// errDoHNameFailed marks a timeout after the connection was ready or a
	// SERVFAIL: the name's upstream failed, not the resolver.
	errDoHNameFailed = errors.New("quickfox: DoH lookup failed for this name")
)

type dohResolver struct {
	endpoint string
	client   *http.Client
	clock    func() time.Time

	mu           sync.Mutex
	cache        map[dohCacheKey]dohCacheEntry
	backoffUntil time.Time
	// nameFailures counts distinct names that failed since the last answer.
	nameFailures int
}

type dohCacheKey struct {
	host   string
	subnet netip.Prefix
}

// dohCacheEntry holds unfiltered A records; nil addresses cache a negative
// answer, and failed sends the name to the system resolver until it expires.
type dohCacheEntry struct {
	addresses []netip.Addr
	failed    bool
	expires   time.Time
}

// NewTransportWithDoH returns a transport that resolves domain targets with
// the RFC 8484 resolver at endpoint, scoped to the selected node's /24, and
// falls back to the system resolver while that resolver is unavailable.
func NewTransportWithDoH(endpoint string) (*Transport, error) {
	resolver, err := newDoHResolver(endpoint, nil)
	if err != nil {
		return nil, err
	}
	transport := NewTransport()
	transport.doh = resolver
	return transport, nil
}

func newDoHResolver(endpoint string, client *http.Client) (*dohResolver, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.Opaque != "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || strings.Contains(endpoint, "#") {
		return nil, errors.New("quickfox: DoH resolver must be an HTTPS URL without credentials, query, or fragment")
	}
	if client == nil {
		dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
		client = &http.Client{Transport: &http.Transport{
			DialContext: dialer.DialContext, ForceAttemptHTTP2: true, MaxIdleConns: 4,
			IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second,
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		}}
	}
	if transport, ok := client.Transport.(*http.Transport); ok && transport.TLSClientConfig != nil && transport.TLSClientConfig.InsecureSkipVerify {
		return nil, errors.New("quickfox: insecure TLS transport")
	}
	clone := *client
	clone.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &dohResolver{endpoint: endpoint, client: &clone, clock: time.Now, cache: make(map[dohCacheKey]dohCacheEntry)}, nil
}

// lookup returns the A records for host as seen from node's /24. Parent
// cancellation is returned as is. A resolver failure wraps errDoHUnavailable
// and starts the backoff window; a name failure wraps errDoHNameFailed and
// sends only that name to the system resolver for a while; a name that cannot
// be encoded changes neither.
func (resolver *dohResolver) lookup(ctx context.Context, host string, node netip.Addr, budget time.Duration) ([]netip.Addr, error) {
	if !node.Is4() {
		return nil, errors.New("quickfox: DoH subnet requires an IPv4 node")
	}
	subnet, err := node.Prefix(dohSubnetBits)
	if err != nil {
		return nil, err
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	name, err := dnsmessage.NewName(host + ".")
	if err != nil {
		return nil, fmt.Errorf("quickfox: encode DoH query: %w", err)
	}
	query, err := encodeDoHQuery(name, subnet)
	if err != nil {
		return nil, fmt.Errorf("quickfox: encode DoH query: %w", err)
	}
	key := dohCacheKey{host: host, subnet: subnet}
	resolver.mu.Lock()
	now := resolver.clock()
	if entry, ok := resolver.cache[key]; ok && now.Before(entry.expires) {
		resolver.mu.Unlock()
		switch {
		case entry.failed:
			return nil, fmt.Errorf("%w: retrying later", errDoHNameFailed)
		case entry.addresses == nil:
			return nil, errDoHNoAddress
		}
		return entry.addresses, nil
	}
	if now.Before(resolver.backoffUntil) {
		resolver.mu.Unlock()
		return nil, fmt.Errorf("%w: backing off", errDoHUnavailable)
	}
	resolver.mu.Unlock()

	exchangeContext, cancel := context.WithTimeout(ctx, budget)
	addresses, ttl, err := resolver.exchange(exchangeContext, query, name)
	cancel()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	resolver.record(key, addresses, ttl, err)
	if err != nil {
		return nil, err
	}
	return addresses, nil
}

// record caches one exchange's outcome and updates the backoff state.
func (resolver *dohResolver) record(key dohCacheKey, addresses []netip.Addr, ttl time.Duration, err error) {
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	now := resolver.clock()
	switch {
	case err == nil || errors.Is(err, errDoHNoAddress):
		resolver.nameFailures = 0
		resolver.storeLocked(key, dohCacheEntry{addresses: addresses, expires: now.Add(ttl)})
	case errors.Is(err, errDoHNameFailed):
		// Concurrent connections to one slow name count once.
		if previous, ok := resolver.cache[key]; ok && previous.failed && now.Before(previous.expires) {
			return
		}
		resolver.storeLocked(key, dohCacheEntry{failed: true, expires: now.Add(dohNameRetry)})
		resolver.nameFailures++
		if resolver.nameFailures >= dohNameFailureLimit {
			resolver.nameFailures = 0
			resolver.backoffUntil = now.Add(dohBackoff)
		}
	default:
		resolver.nameFailures = 0
		resolver.backoffUntil = now.Add(dohBackoff)
	}
}

func (resolver *dohResolver) storeLocked(key dohCacheKey, entry dohCacheEntry) {
	if _, ok := resolver.cache[key]; !ok && len(resolver.cache) >= maxDoHCacheEntries {
		now := resolver.clock()
		for cached, value := range resolver.cache {
			if !now.Before(value.expires) {
				delete(resolver.cache, cached)
			}
		}
		for cached := range resolver.cache {
			if len(resolver.cache) < maxDoHCacheEntries {
				break
			}
			delete(resolver.cache, cached)
		}
	}
	resolver.cache[key] = entry
}

func (resolver *dohResolver) exchange(ctx context.Context, query []byte, name dnsmessage.Name) ([]netip.Addr, time.Duration, error) {
	var connected atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { connected.Store(true) }})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, resolver.endpoint, bytes.NewReader(query))
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %w", errDoHUnavailable, err)
	}
	request.Header.Set("Content-Type", dohMediaType)
	request.Header.Set("Accept", dohMediaType)
	response, err := resolver.client.Do(request)
	if err != nil {
		// Waiting past the budget on a ready connection is a slow upstream
		// for this name; failing to connect is the resolver's own failure.
		if connected.Load() && errors.Is(err, context.DeadlineExceeded) {
			return nil, 0, fmt.Errorf("%w: %w", errDoHNameFailed, err)
		}
		return nil, 0, fmt.Errorf("%w: %w", errDoHUnavailable, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("%w: HTTP status %d", errDoHUnavailable, response.StatusCode)
	}
	if mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type")); err != nil || mediaType != dohMediaType {
		return nil, 0, fmt.Errorf("%w: unexpected content type", errDoHUnavailable)
	}
	message, err := io.ReadAll(io.LimitReader(response.Body, maxDoHMessage+1))
	if err != nil {
		return nil, 0, fmt.Errorf("%w: read answer: %w", errDoHUnavailable, err)
	}
	if len(message) > maxDoHMessage {
		return nil, 0, fmt.Errorf("%w: oversized answer", errDoHUnavailable)
	}
	return decodeDoHAnswer(message, name)
}

// encodeDoHQuery builds a recursive A query with ID 0 (RFC 8484 §4.1) and an
// ECS option carrying only the subnet's leading octets (RFC 7871 §6).
func encodeDoHQuery(name dnsmessage.Name, subnet netip.Prefix) ([]byte, error) {
	builder := dnsmessage.NewBuilder(make([]byte, 0, 128), dnsmessage.Header{RecursionDesired: true})
	builder.EnableCompression()
	if err := builder.StartQuestions(); err != nil {
		return nil, err
	}
	if err := builder.Question(dnsmessage.Question{Name: name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}); err != nil {
		return nil, err
	}
	if err := builder.StartAdditionals(); err != nil {
		return nil, err
	}
	var header dnsmessage.ResourceHeader
	if err := header.SetEDNS0(dohUDPPayload, dnsmessage.RCodeSuccess, false); err != nil {
		return nil, err
	}
	octets := subnet.Masked().Addr().As4()
	option := []byte{0, 1, byte(subnet.Bits()), 0}
	option = append(option, octets[:(subnet.Bits()+7)/8]...)
	if err := builder.OPTResource(header, dnsmessage.OPTResource{Options: []dnsmessage.Option{{Code: ednsClientSubnet, Data: option}}}); err != nil {
		return nil, err
	}
	return builder.Finish()
}

// decodeDoHAnswer follows the CNAME chain from name and returns its A
// records with the smallest TTL on the chain, clamped to the cache bounds.
func decodeDoHAnswer(message []byte, name dnsmessage.Name) ([]netip.Addr, time.Duration, error) {
	var parser dnsmessage.Parser
	header, err := parser.Start(message)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %w", errDoHUnavailable, err)
	}
	if !header.Response || header.ID != 0 || header.Truncated {
		return nil, 0, fmt.Errorf("%w: invalid answer header", errDoHUnavailable)
	}
	questions, err := parser.AllQuestions()
	if err != nil || len(questions) != 1 || questions[0].Type != dnsmessage.TypeA || questions[0].Class != dnsmessage.ClassINET ||
		!strings.EqualFold(questions[0].Name.String(), name.String()) {
		return nil, 0, fmt.Errorf("%w: answer does not match the query", errDoHUnavailable)
	}
	switch header.RCode {
	case dnsmessage.RCodeSuccess:
	case dnsmessage.RCodeNameError:
		return nil, negativeDoHTTL, errDoHNoAddress
	case dnsmessage.RCodeServerFailure:
		return nil, 0, fmt.Errorf("%w: response code %s", errDoHNameFailed, header.RCode)
	default:
		return nil, 0, fmt.Errorf("%w: response code %s", errDoHUnavailable, header.RCode)
	}
	answers, err := parser.AllAnswers()
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %w", errDoHUnavailable, err)
	}
	owner := name.String()
	ttl := maxDoHTTL
	var addresses []netip.Addr
	for range maxCNAMEHops + 1 {
		next := ""
		for _, answer := range answers {
			if answer.Header.Class != dnsmessage.ClassINET || !strings.EqualFold(answer.Header.Name.String(), owner) {
				continue
			}
			switch body := answer.Body.(type) {
			case *dnsmessage.AResource:
				addresses = append(addresses, netip.AddrFrom4(body.A))
				ttl = min(ttl, time.Duration(answer.Header.TTL)*time.Second)
			case *dnsmessage.CNAMEResource:
				if next == "" {
					next = body.CNAME.String()
					ttl = min(ttl, time.Duration(answer.Header.TTL)*time.Second)
				}
			}
		}
		if len(addresses) != 0 || next == "" {
			break
		}
		owner = next
	}
	if len(addresses) == 0 {
		return nil, negativeDoHTTL, errDoHNoAddress
	}
	return addresses, max(ttl, minDoHTTL), nil
}
