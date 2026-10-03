// Package smart measures domestic HTTPS destinations through provider tunnels
// and supplies a stable, authenticated automatic SOCKS route.
package smart

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/kfadapter/kfadapter/internal/provider"
	"github.com/kfadapter/kfadapter/internal/selector"
	"github.com/kfadapter/kfadapter/internal/state"
)

type Target struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

var defaultTargets = []Target{
	{"Bilibili", "https://www.bilibili.com/"},
	{"Xiaohongshu", "https://www.xiaohongshu.com/"},
	{"WeChat", "https://weixin.qq.com/"},
	{"QQ", "https://www.qq.com/"},
}

type Measurement struct {
	Target    string `json:"target"`
	LatencyMS int64  `json:"latencyMs,omitempty"`
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
}
type Result struct {
	NodeID       string        `json:"nodeId"`
	Name         string        `json:"name"`
	Provider     string        `json:"provider"`
	Successes    int           `json:"successes"`
	LatencyMS    int64         `json:"latencyMs,omitempty"`
	Measurements []Measurement `json:"measurements"`
	node         state.Node
	epoch        selector.Credentials
}
type Status struct {
	Enabled         bool      `json:"enabled"`
	IntervalMinutes int       `json:"intervalMinutes"`
	Running         bool      `json:"running"`
	CandidateCount  int       `json:"candidateCount"`
	Incomplete      bool      `json:"incomplete"`
	LastRunAt       time.Time `json:"lastRunAt,omitzero"`
	NextRunAt       time.Time `json:"nextRunAt,omitzero"`
	SelectedNodeID  string    `json:"selectedNodeId,omitempty"`
	SelectedName    string    `json:"selectedName,omitempty"`
	Targets         []Target  `json:"targets"`
	Results         []Result  `json:"results"`
}
type Config struct {
	Manager    *state.Manager
	Store      *state.SQLiteStore
	Registry   func() *selector.Registry
	Providers  *provider.Registry
	Dial       provider.DialContextFunc
	MutationMu *sync.Mutex
	// Probe is injectable for deterministic tests; production uses HTTPS tunnels.
	Probe func(context.Context, state.TunnelPin, Target) Measurement
}
type Service struct {
	manager        *state.Manager
	store          *state.SQLiteStore
	registry       func() *selector.Registry
	mutations      *sync.Mutex
	probe          func(context.Context, state.TunnelPin, Target) Measurement
	mu             sync.Mutex
	policy         state.SmartProxyPreferences
	running        bool
	candidateCount int
	incomplete     bool
	cursor         int
	roundTimeout   time.Duration
	revision       uint64
	last, next     time.Time
	results        []Result
	cancel         context.CancelFunc
	wake           chan struct{}
}

func New(c Config) (*Service, error) {
	if c.Manager == nil || c.Store == nil || c.Registry == nil || c.MutationMu == nil {
		return nil, errors.New("smart: missing dependencies")
	}
	persistent, err := c.Store.Load()
	if err != nil {
		return nil, err
	}
	if c.Probe == nil {
		if c.Providers == nil {
			return nil, errors.New("smart: missing transports")
		}
		c.Probe = tunnelProbe(c.Manager, c.Providers, c.Dial)
	}
	policy := persistent.Preferences.SmartProxy
	if policy.IntervalMinutes == 0 {
		policy.IntervalMinutes = 30
	}
	return &Service{manager: c.Manager, store: c.Store, registry: c.Registry, mutations: c.MutationMu, probe: c.Probe, policy: policy, roundTimeout: 10 * time.Minute, wake: make(chan struct{}, 1)}, nil
}

func (s *Service) Configure(policy state.SmartProxyPreferences) error {
	if policy.IntervalMinutes != 30 && policy.IntervalMinutes != 60 {
		return errors.New("smart: interval must be 30 or 60 minutes")
	}
	s.mutations.Lock()
	defer s.mutations.Unlock()
	if _, err := s.store.Update(func(p *state.PersistentState) error { p.Preferences.SmartProxy = policy; return nil }); err != nil {
		return err
	}
	s.mu.Lock()
	if s.policy.Enabled != policy.Enabled {
		s.results = nil
		s.candidateCount, s.incomplete = 0, false
	}
	s.policy = policy
	s.revision++
	if s.cancel != nil {
		s.cancel()
	}
	s.next = time.Time{}
	s.mu.Unlock()
	s.signal()
	return nil
}
func (s *Service) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
func (s *Service) Enabled() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.policy.Enabled }

// RequestProbe coalesces manual requests and never launches overlapping rounds.
func (s *Service) RequestProbe() bool {
	s.mu.Lock()
	if !s.policy.Enabled || s.running {
		s.mu.Unlock()
		return false
	}
	s.next = time.Time{}
	s.mu.Unlock()
	s.signal()
	return true
}

// Completed observations describe route performance. CompactPin checks the
// current authority, so normal same-account credential refreshes retain rankings.
func (s *Service) current(result Result) bool {
	registry := s.registry()
	if registry == nil || registry.SmartCredentials() != result.epoch {
		return false
	}
	pin, err := s.manager.CompactPin(result.node.Selector, time.Now())
	return err == nil && pin.Node.TunnelEligible() && pin.Node.ID == result.NodeID && pin.Node.Host == result.node.Host && pin.Node.Port == result.node.Port && pin.Node.Protocol == result.node.Protocol
}

// Resolve picks the best measured route still present in the current account.
// It fails closed when disabled or no measured route remains usable.
func (s *Service) Resolve() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.policy.Enabled {
		for _, result := range s.results {
			if result.Successes > 0 && s.current(result) {
				return result.node.Selector, nil
			}
		}
	}
	return "", state.ErrSelectorUnknown
}
func (s *Service) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := Status{Enabled: s.policy.Enabled, IntervalMinutes: s.policy.IntervalMinutes, Running: s.running, CandidateCount: s.candidateCount, Incomplete: s.incomplete, LastRunAt: s.last, NextRunAt: s.next, Targets: append([]Target(nil), defaultTargets...), Results: make([]Result, 0, len(s.results))}
	for _, result := range s.results {
		if !s.current(result) {
			continue
		}
		copyResult := result
		copyResult.Measurements = append([]Measurement(nil), result.Measurements...)
		status.Results = append(status.Results, copyResult)
		if status.Enabled && status.SelectedNodeID == "" && result.Successes > 0 {
			status.SelectedNodeID, status.SelectedName = result.NodeID, result.Name
		}
	}
	return status
}

// Run owns the scheduler and all probe workers. Cancellation closes tunnels and
// waits for the bounded worker pool before returning to the process supervisor.
func (s *Service) Run(ctx context.Context) error {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	defer func() { s.mu.Lock(); s.running = false; s.mu.Unlock() }()
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.roundIfDue(ctx)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.wake:
		case <-ticker.C:
		}
	}
}
func (s *Service) roundIfDue(parent context.Context) {
	s.mu.Lock()
	if s.policy.Enabled && len(s.results) > 0 {
		current := false
		for _, result := range s.results {
			if s.current(result) {
				current = true
				break
			}
		}
		if !current {
			s.next = time.Time{}
		}
	}
	if !s.policy.Enabled || s.running || time.Now().Before(s.next) {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithTimeout(parent, s.roundTimeout)
	s.running, s.cancel = true, cancel
	revision, cursor := s.revision, s.cursor
	s.mu.Unlock()
	defer cancel()

	snapshot := s.manager.Current()
	registry := s.registry()
	var nodes []state.Node
	now := time.Now()
	if registry != nil && state.SessionUsable(snapshot, now) {
		for _, node := range snapshot.Nodes {
			account, available := snapshot.Providers[node.Provider]
			if node.TunnelEligible() && available && account.ExpiresAt.After(now) {
				nodes = append(nodes, node)
			}
		}
	}
	// Rotate the starting position after a capped round, so large inventories
	// do not permanently starve the nodes near the end of the provider catalog.
	start := 0
	if len(nodes) > 0 {
		start = cursor % len(nodes)
		nodes = append(nodes[start:], nodes[:start]...)
	}
	// Release the authority-bearing snapshot before any network I/O.
	snapshot = nil
	results := make([]Result, len(nodes))
	jobs := make(chan int)
	var workers sync.WaitGroup
	for range min(4, len(nodes)) {
		workers.Go(func() {
			for index := range jobs {
				if ctx.Err() != nil {
					return
				}
				node := nodes[index]
				pin, err := s.manager.CompactPin(node.Selector, time.Now())
				if err != nil {
					continue
				}
				node = pin.Node
				result := Result{NodeID: node.ID, Name: node.Name, Provider: string(node.Provider), node: node, epoch: registry.SmartCredentials()}
				var total int64
				for _, target := range defaultTargets {
					if ctx.Err() != nil {
						break
					}
					probeCtx, stop := context.WithTimeout(ctx, 8*time.Second)
					measurement := s.probe(probeCtx, pin, target)
					stop()
					measurement.Target = target.Name
					result.Measurements = append(result.Measurements, measurement)
					if measurement.OK {
						result.Successes++
						total += measurement.LatencyMS
					}
				}
				// A round deadline is not a target failure. In particular, a
				// cancelled final target must not turn partial work into a full result.
				if ctx.Err() != nil || len(result.Measurements) != len(defaultTargets) || !s.manager.SessionCurrentPin(pin, time.Now()) {
					continue
				}
				if result.Successes > 0 {
					result.LatencyMS = total / int64(result.Successes)
				}
				results[index] = result
			}
		})
	}
	dispatched := 0
dispatch:
	for index := range nodes {
		// Stop dispatching after cancellation even if a worker is also ready.
		// Otherwise select can keep advancing the cursor over unprobed nodes.
		if ctx.Err() != nil {
			break
		}
		select {
		case jobs <- index:
			dispatched++
		case <-ctx.Done():
			break dispatch
		}
	}
	close(jobs)
	workers.Wait()
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Successes != results[j].Successes {
			return results[i].Successes > results[j].Successes
		}
		if results[i].LatencyMS != results[j].LatencyMS {
			return results[i].LatencyMS < results[j].LatencyMS
		}
		return results[i].NodeID < results[j].NodeID
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running, s.cancel = false, nil
	if revision != s.revision || parent.Err() != nil {
		return
	}
	s.results = nil
	for _, result := range results {
		if result.NodeID != "" && s.current(result) {
			s.results = append(s.results, result)
		}
	}
	s.candidateCount = len(nodes)
	s.incomplete = len(s.results) < len(nodes)
	if s.incomplete && len(nodes) > 0 {
		s.cursor = (start + max(1, dispatched-4)) % len(nodes)
	} else {
		s.cursor = 0
	}
	s.last = time.Now().UTC()
	s.next = s.last.Add(time.Duration(s.policy.IntervalMinutes) * time.Minute)
	// Retry soon when no account is available or every measured route failed.
	if len(s.results) == 0 || s.results[0].Successes == 0 {
		s.next = s.last.Add(time.Minute)
	}
}

// Credentials returns only locally derived proxy credentials on explicit demand.
func (s *Service) Credentials() (selector.Credentials, error) {
	if !s.Enabled() {
		return selector.Credentials{}, state.ErrSelectorUnknown
	}
	registry := s.registry()
	if registry == nil || !state.SessionUsable(s.manager.Current(), time.Now()) {
		return selector.Credentials{}, state.ErrSelectorUnknown
	}
	return registry.SmartCredentials(), nil
}
