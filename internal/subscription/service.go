package subscription

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/kfadapter/kfadapter/internal/provider"
	"github.com/kfadapter/kfadapter/internal/selector"
	"github.com/kfadapter/kfadapter/internal/state"
)

var errRenderedSubscriptionMismatch = errors.New("rendered subscription does not match durable node authority")

const (
	subscriptionResponseConcurrency = 8
	subscriptionResponseChunkBytes  = 32 << 10
)

// ServiceConfig wires the single, account-bound subscription authority. The
// mutation locker is shared with the runtime coordinator; when present its
// lock is always acquired before Service.mu by fetch-proof persistence.
type ServiceConfig struct {
	Store          *state.SQLiteStore
	SocksAddress   string
	Now            func() time.Time
	MutationLocker sync.Locker
}

// Service stores and serves exactly one current subscription authority. Its
// URL token is the verifier-derived account binding.
type Service struct {
	store          *state.SQLiteStore
	socksAddress   string
	now            func() time.Time
	mutationLocker sync.Locker

	mu            sync.Mutex
	cache         cachedSubscription
	responseSlots chan struct{}
	hooks         serviceHooks
}

// RuntimeCommitPlan is a non-durable candidate subscription authority. It
// lets runtime install matching selector authority before the final aggregate
// transaction writes the binding, rendered subscription, and active session.
type RuntimeCommitPlan struct {
	Authority state.SubscriptionAuthority
	// Rotated reports that a provider account changed and the plan carries
	// fresh selector and proxy keys.
	Rotated  bool
	userID   string
	previous state.SubscriptionAuthority
}

type serviceHooks struct {
	onLoad   func()
	onRender func()
}

type cachedLink struct {
	provider provider.ID
	link     Link
}

type subscriptionFilter struct {
	provider provider.ID
	group    string
	name     string
}

// cachedSubscription is the only state consulted for unauthenticated path
// admission after syntax validation. Invalid probes therefore never load disk
// state or derive SOCKS credentials.
type cachedSubscription struct {
	binding   [sha256.Size]byte
	body      string
	nodeCount int
	links     []cachedLink
}

func (s *Service) noteStateLoad() {
	if s.hooks.onLoad != nil {
		s.hooks.onLoad()
	}
}

func (s *Service) loadPersisted() (state.PersistentState, error) {
	s.noteStateLoad()
	return s.store.Load()
}

// NewService validates and primes serving material before it accepts requests.
// A valid body rendered for a prior configured listener is migrated by exact
// re-rendering; tampered bodies remain fail-closed.
func NewService(config ServiceConfig) (*Service, error) {
	if config.Store == nil {
		return nil, errors.New("subscription state store is required")
	}
	if err := validateAddress(config.SocksAddress); err != nil {
		return nil, err
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	service := &Service{
		store:        config.Store,
		socksAddress: config.SocksAddress, now: config.Now, mutationLocker: config.MutationLocker,
		responseSlots: make(chan struct{}, subscriptionResponseConcurrency),
	}
	if err := service.reconcileAndPrime(); err != nil {
		return nil, err
	}
	return service, nil
}

func (s *Service) reconcileAndPrime() error {
	if s.mutationLocker != nil {
		s.mutationLocker.Lock()
		defer s.mutationLocker.Unlock()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	persistent, err := s.loadPersisted()
	if err != nil {
		return err
	}
	if address, ok := concreteRenderedAddress(s.socksAddress, persistent.LastGood.RenderedSubscription); ok {
		s.socksAddress = address
	}
	updated, changed, err := s.reconcileListenerAddress(persistent)
	if err != nil {
		return err
	}
	if changed {
		updated, err = s.store.Update(func(candidate *state.PersistentState) error {
			repaired, changed, err := s.reconcileListenerAddress(*candidate)
			if err != nil || !changed {
				return err
			}
			*candidate = repaired
			return nil
		})
		if err != nil {
			return err
		}
	}
	if err := s.validateRenderedState(updated); err != nil {
		return err
	}
	s.primeCacheLocked(updated)
	return nil
}

func concreteRenderedAddress(listenAddress, body string) (string, bool) {
	listenHost, listenPort, err := parseAddress(listenAddress)
	if err != nil {
		return "", false
	}
	listenIP, _ := netip.ParseAddr(listenHost)
	if !listenIP.IsUnspecified() {
		return "", false
	}
	address, ok := renderedAddress(body)
	if !ok {
		return "", false
	}
	host, port, err := parseAddress(address)
	if err != nil || port != listenPort {
		return "", false
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return address, true
	}
	return address, !ip.IsUnspecified() && ip.Is4() == listenIP.Is4()
}

func (s *Service) reconcileListenerAddress(persistent state.PersistentState) (state.PersistentState, bool, error) {
	candidate := persistent.Clone()
	lastGood := candidate.LastGood
	if lastGood.RenderedSubscription == "" {
		if !emptyLastGood(lastGood) {
			return state.PersistentState{}, false, errRenderedSubscriptionMismatch
		}
		return candidate, false, nil
	}
	if len(candidate.Subscription.AccountBinding) != sha256.Size {
		return state.PersistentState{}, false, errRenderedSubscriptionMismatch
	}
	rendered, _, _, err := s.renderPersistedNodes(lastGood.Nodes, candidate.Subscription)
	if err != nil {
		return state.PersistentState{}, false, errRenderedSubscriptionMismatch
	}
	if rendered == lastGood.RenderedSubscription {
		return candidate, false, nil
	}
	if !s.matchesRenderedAtAddress(lastGood.RenderedSubscription, lastGood.Nodes, candidate.Subscription) {
		return state.PersistentState{}, false, errRenderedSubscriptionMismatch
	}
	candidate.LastGood.RenderedSubscription = rendered
	return candidate, true, nil
}

// matchesRenderedAtAddress accepts a body the current renderer produced for a
// previously configured listener address, so startup can re-render it.
func (s *Service) matchesRenderedAtAddress(body string, nodes []state.PersistedNode, authority state.SubscriptionAuthority) bool {
	address, ok := renderedAddress(body)
	if !ok {
		return false
	}
	rendered, _, _, err := s.renderPersistedNodesAt(nodes, authority, address)
	return err == nil && rendered == body
}

func renderedAddress(body string) (string, bool) {
	decoded, err := base64.StdEncoding.DecodeString(body)
	if err != nil || len(decoded) == 0 {
		return "", false
	}
	lines := strings.Split(strings.TrimSuffix(string(decoded), "\n"), "\n")
	if len(lines) == 0 || lines[0] == "" {
		return "", false
	}
	address := ""
	for _, line := range lines {
		link, err := url.Parse(line)
		if err != nil || link.Scheme != "socks5" || link.Host == "" || link.User == nil || link.RawQuery != "" {
			return "", false
		}
		if address == "" {
			if validateAddress(link.Host) != nil {
				return "", false
			}
			address = link.Host
		} else if link.Host != address {
			return "", false
		}
	}
	return address, true
}

func emptyLastGood(lastGood state.LastGoodState) bool {
	return lastGood.CreatedAt.IsZero() && len(lastGood.Nodes) == 0 && lastGood.RenderedSubscription == ""
}

func (s *Service) primeCacheLocked(persistent state.PersistentState) {
	cache := cachedSubscription{}
	if len(persistent.Subscription.AccountBinding) == sha256.Size && persistent.LastGood.RenderedSubscription != "" {
		copy(cache.binding[:], persistent.Subscription.AccountBinding)
		cache.body = persistent.LastGood.RenderedSubscription
		cache.nodeCount = eligiblePersistedCount(persistent.LastGood.Nodes)
		cache.links = make([]cachedLink, 0, cache.nodeCount)
		for _, node := range persistent.LastGood.Nodes {
			if !node.Eligible || !provider.ID(node.Provider).Valid() || node.Host == "" || node.Port == 0 {
				continue
			}
			credential, err := selector.Derive(selector.NodeIdentity{NodeID: node.ID, Provider: node.Provider, Host: node.Host, Port: int(node.Port)}, persistent.Subscription.SelectorKey, persistent.Subscription.ProxyAuthKey)
			if err != nil {
				cache = cachedSubscription{}
				break
			}
			cache.links = append(cache.links, cachedLink{
				provider: provider.ID(node.Provider),
				link:     Link{Selector: credential.Selector, Password: credential.Password, Name: node.Name, Group: node.Group, Eligible: true},
			})
		}
		if len(cache.links) != cache.nodeCount {
			cache = cachedSubscription{}
		}
	}
	s.cache = cache
}

func cloneSubscriptionAuthority(authority state.SubscriptionAuthority) state.SubscriptionAuthority {
	return authority.Clone()
}

// PrepareRuntimeCommit derives, but does not persist, the account-bound
// authority needed to install matching selector state. CommitRuntimeSnapshot
// verifies the plan against current durable state before writing it.
func (s *Service) PrepareRuntimeCommit(ctx context.Context, userID string) (RuntimeCommitPlan, error) {
	if err := ctx.Err(); err != nil {
		return RuntimeCommitPlan{}, err
	}
	if s == nil || strings.TrimSpace(userID) == "" {
		return RuntimeCommitPlan{}, state.ErrAccountChanged
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	persistent, err := s.loadPersisted()
	if err != nil {
		return RuntimeCommitPlan{}, err
	}
	candidate := persistent.Clone()
	rotated, err := state.EnsureSubscriptionAccountBinding(&candidate, userID, s.now().UTC())
	if err != nil {
		return RuntimeCommitPlan{}, err
	}
	return RuntimeCommitPlan{
		Authority: cloneSubscriptionAuthority(candidate.Subscription),
		Rotated:   rotated,
		userID:    userID,
		previous:  cloneSubscriptionAuthority(persistent.Subscription),
	}, nil
}

// CommitRuntimeSnapshot atomically persists one coherent provider authority
// aggregate: the planned account binding, rendered LastGood subscription, and
// complete ActiveSession. The returned rollback restores the exact prior
// aggregate if the in-memory manager later rejects publication.
func (s *Service) CommitRuntimeSnapshot(ctx context.Context, plan RuntimeCommitPlan, snapshot *state.RuntimeSnapshot) (func() error, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s == nil || snapshot == nil || !state.SessionUsable(snapshot, s.now()) || snapshot.AccountBindingID() != plan.userID {
		return nil, state.ErrAccountChanged
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var before state.PersistentState
	after, err := s.store.Update(func(candidate *state.PersistentState) error {
		before = candidate.Clone()
		if !sameSubscriptionAuthority(candidate.Subscription, plan.previous) ||
			!state.SameAccountRoster(candidate.Subscription.AccountRoster, plan.previous.AccountRoster) {
			return state.ErrAccountChanged
		}
		if !candidate.AuthorityAdmits(plan.Authority, plan.userID) {
			return state.ErrAccountChanged
		}
		changedAccount := !sameSubscriptionAuthority(candidate.Subscription, plan.Authority)
		candidate.Subscription = cloneSubscriptionAuthority(plan.Authority)
		if changedAccount {
			candidate.LastGood = state.LastGoodState{}
			candidate.ActiveSession = nil
		}
		if err := s.updateLastGood(candidate, snapshot); err != nil {
			return err
		}
		candidate.ActiveSession = snapshot.Clone()
		return s.validateRenderedState(*candidate)
	})
	if err != nil {
		return nil, err
	}
	s.primeCacheLocked(after)
	var once sync.Once
	var rollbackErr error
	rollback := func() error {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			rollbackErr = s.store.Save(before)
			if rollbackErr == nil {
				s.primeCacheLocked(before)
			}
		})
		return rollbackErr
	}
	return rollback, nil
}

func sameSubscriptionAuthority(left, right state.SubscriptionAuthority) bool {
	return subtle.ConstantTimeCompare(left.SelectorKey, right.SelectorKey) == 1 &&
		subtle.ConstantTimeCompare(left.ProxyAuthKey, right.ProxyAuthKey) == 1 &&
		subtle.ConstantTimeCompare(left.AccountBinding, right.AccountBinding) == 1
}

func (s *Service) updateLastGood(candidate *state.PersistentState, snapshot *state.RuntimeSnapshot) error {
	if candidate == nil || len(candidate.Subscription.AccountBinding) != sha256.Size {
		return ErrSubscriptionUnavailable
	}
	body, _, nodes, err := s.renderNodes(snapshot.Nodes, candidate.Subscription)
	if err != nil {
		return err
	}
	oldBody := candidate.LastGood.RenderedSubscription
	bodyChanged := oldBody != body
	candidate.LastGood.Nodes = nodes
	candidate.LastGood.RenderedSubscription = body
	if bodyChanged {
		candidate.LastGood.CreatedAt = s.now().UTC()
	}
	return nil
}

// Metadata returns redacted state and never exposes the subscription URL,
// account binding, SOCKS credentials, or provider material.
func (s *Service) Metadata() (Metadata, error) {
	if s == nil {
		return Metadata{}, ErrSubscriptionUnavailable
	}
	s.mu.Lock()
	current := s.cache
	s.mu.Unlock()
	return Metadata{Active: current.body != "", NodeCount: current.nodeCount}, nil
}

// SubscriptionURL returns the reusable path token for the active binding.
func (s *Service) SubscriptionURL(baseURL string) (string, error) {
	if s == nil {
		return "", ErrSubscriptionUnavailable
	}
	s.mu.Lock()
	current := s.cache
	s.mu.Unlock()
	if current.body == "" {
		return "", ErrSubscriptionUnavailable
	}
	return subscriptionURL(baseURL, base64.RawURLEncoding.EncodeToString(current.binding[:])), nil
}

// ServeHTTP handles the full canonical subscription path. It is useful for
// direct tests; production routing extracts the token and calls ServeSubscription.
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.acquireResponseSlot(w) {
		return
	}
	defer s.releaseResponseSlot()
	binding, ok := subscriptionRequestPath(r.URL.Path)
	if !ok {
		writeNotFound(w)
		return
	}
	s.serveSubscription(w, r, binding, "")
}

// ServeSubscription serves one router-extracted account-binding path segment.
// Every malformed, stale, unknown, or method-invalid request gets the same
// empty no-store 404 response.
func (s *Service) ServeSubscription(w http.ResponseWriter, r *http.Request, binding string) {
	if !s.acquireResponseSlot(w) {
		return
	}
	defer s.releaseResponseSlot()
	s.serveSubscription(w, r, binding, "")
}

// ServeSubscriptionAt serves a valid binding after adapting a wildcard SOCKS
// listener to the concrete host used for this request.
func (s *Service) ServeSubscriptionAt(w http.ResponseWriter, r *http.Request, binding, socksAddress string) {
	if !s.acquireResponseSlot(w) {
		return
	}
	defer s.releaseResponseSlot()
	s.serveSubscription(w, r, binding, socksAddress)
}

func (s *Service) acquireResponseSlot(w http.ResponseWriter) bool {
	select {
	case s.responseSlots <- struct{}{}:
		return true
	default:
		writeResponseBusy(w)
		return false
	}
}

func (s *Service) releaseResponseSlot() { <-s.responseSlots }

func writeResponseBusy(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", "0")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Retry-After", "1")
	w.WriteHeader(http.StatusServiceUnavailable)
}

func writeResponseUnavailable(w http.ResponseWriter, retry bool) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", "0")
	w.Header().Set("Cache-Control", "no-store")
	if retry {
		w.Header().Set("Retry-After", "1")
	}
	w.WriteHeader(http.StatusServiceUnavailable)
}

func (s *Service) serveSubscription(w http.ResponseWriter, r *http.Request, binding, socksAddress string) {
	filter, validFilter := parseSubscriptionFilter(r.URL.RawQuery)
	if r.Method != http.MethodGet || !validBinding(binding) || !validFilter {
		writeNotFound(w)
		return
	}
	var token [sha256.Size]byte
	_, _ = base64.RawURLEncoding.Decode(token[:], []byte(binding))
	s.mu.Lock()
	current := s.cache
	renderedAddress := s.socksAddress
	matched := current.body != "" && subtle.ConstantTimeCompare(token[:], current.binding[:]) == 1
	s.mu.Unlock()
	if !matched {
		writeNotFound(w)
		return
	}
	// A wildcard listener is reached through a concrete host address. The body
	// is rendered for that address per request; shared state is never mutated
	// by a read, so concurrent clients on different addresses stay isolated.
	address := renderedAddress
	if socksAddress != "" && validateAddress(socksAddress) == nil {
		address = socksAddress
	}
	body := current.body
	if !filter.empty() || address != renderedAddress {
		var err error
		body, err = renderFilteredLinks(current.links, filter, address)
		if errors.Is(err, ErrNoEligibleLinks) {
			writeNotFound(w)
			return
		}
		if err != nil {
			writeResponseUnavailable(w, false)
			return
		}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	written, err := writeSubscriptionBody(w, body)
	if err != nil || written != len(body) {
		return
	}
}

func (filter subscriptionFilter) empty() bool {
	return filter.provider == "" && filter.group == "" && filter.name == ""
}

func parseSubscriptionFilter(rawQuery string) (subscriptionFilter, bool) {
	if rawQuery == "" {
		return subscriptionFilter{}, true
	}
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return subscriptionFilter{}, false
	}
	for key := range values {
		if key != "provider" && key != "group" && key != "name" {
			return subscriptionFilter{}, false
		}
	}
	providerValue, ok := singleFilterValue(values, "provider", 32)
	if !ok {
		return subscriptionFilter{}, false
	}
	group, ok := singleFilterValue(values, "group", provider.MaxNodeGroupBytes)
	if !ok {
		return subscriptionFilter{}, false
	}
	name, ok := singleFilterValue(values, "name", maxNameFilterBytes)
	if !ok {
		return subscriptionFilter{}, false
	}
	filter := subscriptionFilter{provider: provider.ID(providerValue), group: group, name: name}
	if providerValue != "" && !filter.provider.Valid() {
		return subscriptionFilter{}, false
	}
	return filter, true
}

const maxNameFilterBytes = 512

func singleFilterValue(values url.Values, key string, maxBytes int) (string, bool) {
	items, exists := values[key]
	if !exists {
		return "", true
	}
	if len(items) != 1 || items[0] == "" || len(items[0]) > maxBytes || !utf8.ValidString(items[0]) || strings.IndexByte(items[0], 0) >= 0 || strings.TrimSpace(items[0]) != items[0] {
		return "", false
	}
	return items[0], true
}

func renderFilteredLinks(cached []cachedLink, filter subscriptionFilter, socksAddress string) (string, error) {
	links := make([]Link, 0, len(cached))
	foldedName := strings.ToLower(filter.name)
	for _, candidate := range cached {
		if filter.provider != "" && candidate.provider != filter.provider || filter.group != "" && candidate.link.Group != filter.group || foldedName != "" && !strings.Contains(strings.ToLower(candidate.link.Name), foldedName) {
			continue
		}
		links = append(links, candidate.link)
	}
	body, _, err := Render(links, socksAddress)
	return body, err
}

func validBinding(binding string) bool {
	if len(binding) != base64.RawURLEncoding.EncodedLen(sha256.Size) {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(binding)
	return err == nil && len(decoded) == sha256.Size && base64.RawURLEncoding.EncodeToString(decoded) == binding
}

func writeSubscriptionBody(w io.Writer, body string) (int, error) {
	written := 0
	for len(body) != 0 {
		limit := subscriptionResponseChunkBytes
		if len(body) < limit {
			limit = len(body)
		}
		n, err := io.WriteString(w, body[:limit])
		written += n
		if err != nil {
			return written, err
		}
		if n != limit {
			return written, io.ErrShortWrite
		}
		body = body[n:]
	}
	return written, nil
}

// ValidatePersistentState re-derives durable local authority from the
// subscription authority. It is pure so offline validation can reject a
// database that NewService would reject.
func ValidatePersistentState(persistent state.PersistentState) error {
	if err := state.ValidatePersistentState(persistent); err != nil {
		return err
	}
	lastGood := persistent.LastGood
	if lastGood.RenderedSubscription == "" {
		if !emptyLastGood(lastGood) {
			return errRenderedSubscriptionMismatch
		}
	} else {
		if len(persistent.Subscription.AccountBinding) != sha256.Size {
			return errRenderedSubscriptionMismatch
		}
		address, err := renderedSubscriptionAddress(lastGood.RenderedSubscription)
		if err != nil {
			return errRenderedSubscriptionMismatch
		}
		links := make([]Link, 0, len(lastGood.Nodes))
		for _, node := range lastGood.Nodes {
			credential, err := selector.Derive(selector.NodeIdentity{NodeID: node.ID, Provider: node.Provider, Host: node.Host, Port: int(node.Port)}, persistent.Subscription.SelectorKey, persistent.Subscription.ProxyAuthKey)
			if err != nil || credential.Selector != node.Selector {
				return errRenderedSubscriptionMismatch
			}
			links = append(links, Link{
				Selector: credential.Selector, Password: credential.Password, Name: node.Name, Group: node.Group,
				Eligible: node.Eligible && provider.ID(node.Provider).Valid() && node.Host != "" && node.Port != 0,
			})
		}
		rendered, _, err := Render(links, address)
		if err != nil || rendered != lastGood.RenderedSubscription {
			return errRenderedSubscriptionMismatch
		}
	}
	if active := persistent.ActiveSession; active != nil {
		if lastGood.RenderedSubscription == "" || len(active.Nodes) != len(lastGood.Nodes) {
			return errRenderedSubscriptionMismatch
		}
		persistedNodes := make(map[string]state.PersistedNode, len(lastGood.Nodes))
		for _, node := range lastGood.Nodes {
			persistedNodes[node.ID] = node
		}
		liveSelectors := make(map[string]struct{}, len(active.Nodes))
		for _, node := range active.Nodes {
			persistedNode, found := persistedNodes[node.ID]
			if !found || node.Selector != persistedNode.Selector || string(node.Provider) != persistedNode.Provider || node.Host != persistedNode.Host || node.Port != persistedNode.Port || node.Name != persistedNode.Name || node.Group != persistedNode.Group || node.Eligible != persistedNode.Eligible || node.Excluded != persistedNode.Excluded {
				return errRenderedSubscriptionMismatch
			}
			credential, err := selector.Derive(selector.NodeIdentity{NodeID: node.ID}, persistent.Subscription.SelectorKey, persistent.Subscription.ProxyAuthKey)
			if err != nil || node.Selector != credential.Selector {
				return errRenderedSubscriptionMismatch
			}
			reference, found := active.Selectors[credential.Selector]
			if !found || reference.NodeID != node.ID {
				return errRenderedSubscriptionMismatch
			}
			liveSelectors[credential.Selector] = struct{}{}
		}
		for name := range active.Selectors {
			if _, found := liveSelectors[name]; !found {
				return errRenderedSubscriptionMismatch
			}
		}
	}
	return nil
}

func renderedSubscriptionAddress(rendered string) (string, error) {
	decoded, err := base64.StdEncoding.DecodeString(rendered)
	if err != nil || base64.StdEncoding.EncodeToString(decoded) != rendered {
		return "", errRenderedSubscriptionMismatch
	}
	text := string(decoded)
	if text == "" || !strings.HasSuffix(text, "\n") {
		return "", errRenderedSubscriptionMismatch
	}
	first := strings.Split(strings.TrimSuffix(text, "\n"), "\n")[0]
	parsed, err := url.Parse(first)
	if err != nil || parsed.Scheme != "socks5" || parsed.User == nil || parsed.Host == "" || parsed.RawQuery != "" {
		return "", errRenderedSubscriptionMismatch
	}
	if err := validateAddress(parsed.Host); err != nil {
		return "", errRenderedSubscriptionMismatch
	}
	return parsed.Host, nil
}

func (s *Service) validateRenderedState(persistent state.PersistentState) error {
	if err := ValidatePersistentState(persistent); err != nil {
		return err
	}
	lastGood := persistent.LastGood
	if lastGood.RenderedSubscription == "" {
		return nil
	}
	rendered, _, _, err := s.renderPersistedNodes(lastGood.Nodes, persistent.Subscription)
	if err != nil || rendered != lastGood.RenderedSubscription {
		return errRenderedSubscriptionMismatch
	}
	return nil
}

func (s *Service) renderNodes(nodes []state.Node, authority state.SubscriptionAuthority) (string, int, []state.PersistedNode, error) {
	if s.hooks.onRender != nil {
		s.hooks.onRender()
	}
	links := make([]Link, 0, len(nodes))
	persisted := make([]state.PersistedNode, 0, len(nodes))
	for _, node := range nodes {
		credential, err := selector.Derive(selector.NodeIdentity{NodeID: node.ID, Provider: string(node.Provider), Host: node.Host, Port: int(node.Port)}, authority.SelectorKey, authority.ProxyAuthKey)
		if err != nil {
			return "", 0, nil, err
		}
		links = append(links, Link{Selector: credential.Selector, Password: credential.Password, Name: node.Name, Group: node.Group, Eligible: node.TunnelEligible()})
		persisted = append(persisted, state.PersistedNode{ID: node.ID, Selector: credential.Selector, Provider: string(node.Provider), Host: node.Host, Port: node.Port, Name: node.Name, Group: node.Group, Eligible: node.TunnelEligible(), Excluded: node.Excluded})
	}
	body, count, err := Render(links, s.socksAddress)
	if err != nil {
		return "", 0, nil, err
	}
	return body, count, persisted, nil
}

func (s *Service) renderPersistedNodes(nodes []state.PersistedNode, authority state.SubscriptionAuthority) (string, int, []state.PersistedNode, error) {
	return s.renderPersistedNodesAt(nodes, authority, s.socksAddress)
}

func (s *Service) renderPersistedNodesAt(nodes []state.PersistedNode, authority state.SubscriptionAuthority, socksAddress string) (string, int, []state.PersistedNode, error) {
	if s.hooks.onRender != nil {
		s.hooks.onRender()
	}
	links := make([]Link, 0, len(nodes))
	for _, node := range nodes {
		credential, err := selector.Derive(selector.NodeIdentity{NodeID: node.ID, Provider: node.Provider, Host: node.Host, Port: int(node.Port)}, authority.SelectorKey, authority.ProxyAuthKey)
		if err != nil {
			return "", 0, nil, err
		}
		eligible := node.Eligible && provider.ID(node.Provider).Valid() && node.Host != "" && node.Port != 0
		links = append(links, Link{Selector: credential.Selector, Password: credential.Password, Name: node.Name, Group: node.Group, Eligible: eligible})
	}
	body, count, err := Render(links, socksAddress)
	return body, count, append([]state.PersistedNode(nil), nodes...), err
}

func eligiblePersistedCount(nodes []state.PersistedNode) int {
	count := 0
	for _, node := range nodes {
		if node.Eligible && provider.ID(node.Provider).Valid() && node.Host != "" && node.Port != 0 {
			count++
		}
	}
	return count
}

func subscriptionRequestPath(requestPath string) (string, bool) {
	parts := strings.Split(requestPath, "/")
	if len(parts) != 3 || parts[0] != "" || parts[1] != "sub" || parts[2] == "" {
		return "", false
	}
	return parts[2], true
}
