package app

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/oshinop/chinadapter/internal/provider"
	"github.com/oshinop/chinadapter/internal/state"
	"github.com/oshinop/chinadapter/internal/web"
)

// Probe measures only one bounded direct TCP connection to the selected
// upstream node. It never starts SOCKS authentication or sends a destination.
func (r *Runtime) Probe(ctx context.Context, nodeID string) (web.ProbeResult, error) {
	if r == nil {
		return web.ProbeResult{}, errors.New("app: runtime unavailable")
	}
	if !r.alive.Load() {
		return web.ProbeResult{}, errRuntimeStopped
	}
	current := r.manager.Current()
	if !state.SessionUsable(current, r.now()) {
		return web.ProbeResult{}, &problemError{cause: provider.ErrNoSession, code: "node_snapshot_unavailable", status: http.StatusConflict}
	}
	node, found := current.NodeByID(nodeID)
	if !found {
		return web.ProbeResult{}, &problemError{cause: errors.New("node not found"), code: "node_not_found", status: http.StatusNotFound}
	}
	if !node.TunnelEligible() {
		return web.ProbeResult{}, &problemError{cause: errors.New("node ineligible"), code: "node_ineligible", status: http.StatusConflict}
	}
	target := &probeTarget{node: node}
	if providerSnapshot, available := current.Providers[node.Provider]; available {
		target.accountID = providerSnapshot.Account.UserID
	}
	address, err := r.probeAddress(current, node)
	if err != nil {
		return web.ProbeResult{}, &problemError{cause: err, code: "probe_unavailable", status: http.StatusConflict}
	}
	select {
	case r.probeSlots <- struct{}{}:
		defer func() { <-r.probeSlots }()
	default:
		return web.ProbeResult{}, &problemError{cause: ErrProbeBusy, code: "probe_busy", status: http.StatusTooManyRequests}
	}
	// Only the newest probe of a node may publish: an older, slower dial must
	// not overwrite a newer result.
	r.mutations.Lock()
	if r.probes == nil {
		r.probes = make(map[string]*probeTarget)
	}
	r.probes[nodeID] = target
	r.mutations.Unlock()
	defer func() {
		r.mutations.Lock()
		if r.probes[nodeID] == target {
			delete(r.probes, nodeID)
		}
		r.mutations.Unlock()
	}()

	probeCtx, cancel := context.WithTimeout(ctx, r.probeTimeout)
	defer cancel()
	started := r.now()
	connection, dialErr := r.dial(probeCtx, "tcp", address)
	// Subtract before UTC(), which strips the monotonic clock reading.
	observed := r.now()
	latency := observed.Sub(started)
	finished := observed.UTC()
	if latency < 0 {
		latency = 0
	}
	if connection != nil {
		_ = connection.Close()
	}
	health := state.NodeHealthHealthy
	if dialErr != nil {
		health = state.NodeHealthUnhealthy
	}
	result := web.ProbeResult{NodeID: nodeID, Health: string(health), ProbedAt: finished}
	if latency > 0 {
		result.TCPLatencyMS = int(latency.Round(time.Millisecond) / time.Millisecond)
	}

	r.mutations.Lock()
	if !r.alive.Load() {
		r.mutations.Unlock()
		return web.ProbeResult{}, errRuntimeStopped
	}
	latest := r.manager.Current()
	if r.probes[nodeID] != target || !target.matches(latest) || !state.SessionUsable(latest, r.now()) {
		r.mutations.Unlock()
		return result, &problemError{cause: ErrStaleProbe, code: "stale_probe", status: http.StatusConflict}
	}
	updated, updateErr := r.updateProbe(latest, nodeID, health, latency, finished)
	r.mutations.Unlock()
	if updateErr != nil {
		return result, updateErr
	}
	if updated != nil {
		r.publish("probe", probeEvent{NodeID: nodeID, Health: string(health), TCPLatencyMS: result.TCPLatencyMS, ProbedAt: finished})
		r.publishState()
	}
	if dialErr != nil {
		return result, tcpProbeProblem(dialErr, probeCtx.Err())
	}
	return result, nil
}

// probeAddress is the node's route address unless its transport implements
// provider.ProbeAddresser, whose stable route is an identity, not an endpoint.
func (r *Runtime) probeAddress(current *state.RuntimeSnapshot, node state.Node) (string, error) {
	route := net.JoinHostPort(node.Host, strconv.Itoa(int(node.Port)))
	// Without a registered ProbeAddresser the route itself is dialable.
	var addresser provider.ProbeAddresser
	if r.transports != nil {
		if transport, err := r.transports.Transport(node.Protocol); err == nil {
			addresser, _ = transport.(provider.ProbeAddresser)
		}
	}
	if addresser == nil {
		return route, nil
	}
	authority, ok := current.Authority(node)
	if !ok {
		return "", errors.New("app: node authority unavailable")
	}
	return addresser.ProbeAddress(authority.Data, node.Host, node.Port)
}

// probeTarget pins the probed route and the account that offered it. Tunnel
// authority bytes rotate on every provider refresh and are not pinned.
type probeTarget struct {
	node      state.Node
	accountID string
}

func (target *probeTarget) matches(snapshot *state.RuntimeSnapshot) bool {
	if snapshot == nil {
		return false
	}
	node, found := snapshot.NodeByID(target.node.ID)
	if !found || !node.TunnelEligible() || node.Selector != target.node.Selector || node.Provider != target.node.Provider ||
		node.Protocol != target.node.Protocol || node.AuthorityID != target.node.AuthorityID ||
		node.Host != target.node.Host || node.Port != target.node.Port || snapshot.Selectors[node.Selector].NodeID != node.ID {
		return false
	}
	providerSnapshot, available := snapshot.Providers[node.Provider]
	return available && providerSnapshot.Account.UserID == target.accountID
}

func (r *Runtime) updateProbe(current *state.RuntimeSnapshot, nodeID string, health state.NodeHealth, latency time.Duration, observed time.Time) (*state.RuntimeSnapshot, error) {
	if !state.SessionUsable(current, observed) {
		return nil, errors.New("app: no active node snapshot")
	}
	next := current.Clone()
	next.Generation++
	for index := range next.Nodes {
		if next.Nodes[index].ID == nodeID {
			next.Nodes[index].Health = health
			next.Nodes[index].TCPRTT = latency
			next.Nodes[index].ProbedAt = observed
			break
		}
	}
	if err := r.manager.Commit(next); err != nil {
		return nil, err
	}
	return next, nil
}
