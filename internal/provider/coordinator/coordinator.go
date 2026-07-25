// Package coordinator serializes provider account lifecycle operations and
// publishes one atomic provider-neutral runtime aggregate.
package coordinator

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/kfadapter/kfadapter/internal/provider"
	"github.com/kfadapter/kfadapter/internal/state"
)

// Config wires provider drivers to generic state publication.
type Config struct {
	Providers       *provider.Registry
	Manager         *state.Manager
	SelectorBuilder state.SelectorBuilder
	CommitSnapshot  func(*state.RuntimeSnapshot) error
	Clock           func() time.Time
}

// Coordinator owns one independent account per registered provider.
type Coordinator struct {
	providers *provider.Registry
	manager   *state.Manager
	builder   state.SelectorBuilder
	commit    func(*state.RuntimeSnapshot) error
	clock     func() time.Time
}

func New(config Config) (*Coordinator, error) {
	if config.Providers == nil || config.Manager == nil || config.SelectorBuilder == nil || config.CommitSnapshot == nil {
		return nil, errors.New("provider coordinator: incomplete configuration")
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}
	return &Coordinator{providers: config.Providers, manager: config.Manager, builder: config.SelectorBuilder, commit: config.CommitSnapshot, clock: config.Clock}, nil
}

func (coordinator *Coordinator) IDs() []provider.ID {
	if coordinator == nil {
		return nil
	}
	return coordinator.providers.IDs()
}

func (coordinator *Coordinator) Login(ctx context.Context, id provider.ID, credentials provider.Credentials) (provider.Account, error) {
	driver, err := coordinator.providers.Driver(id)
	if err != nil {
		return provider.Account{}, err
	}
	complete, err := coordinator.manager.Begin(state.OperationLogin)
	if err != nil {
		return provider.Account{}, err
	}
	outcome := state.OutcomeFailed
	defer func() { complete(outcome) }()
	current := coordinator.manager.Current()
	if current != nil {
		if _, exists := current.Providers[id]; exists {
			return provider.Account{}, provider.ErrAccountExists
		}
	}
	providerSnapshot, err := driver.Login(ctx, credentials)
	credentials.Password = ""
	if err != nil {
		return provider.Account{}, err
	}
	if err := provider.ValidateSnapshot(providerSnapshot, id, coordinator.now()); err != nil {
		return provider.Account{}, err
	}
	providers := coordinator.currentProviders()
	providers[id] = providerSnapshot.Clone()
	if err := coordinator.publish(providers); err != nil {
		return provider.Account{}, err
	}
	outcome = state.OutcomeSucceeded
	return providerSnapshot.Account, nil
}

// Refresh refreshes one provider or all active providers when id is empty. All
// selected results commit atomically; one failure retains the prior aggregate.
func (coordinator *Coordinator) Refresh(ctx context.Context, id provider.ID) error {
	current := coordinator.manager.Current()
	if current == nil || len(current.Providers) == 0 {
		return provider.ErrNoSession
	}
	ids, err := refreshIDs(current.Providers, id)
	if err != nil {
		return err
	}
	complete, err := coordinator.manager.Begin(state.OperationRefresh)
	if err != nil {
		return err
	}
	outcome := state.OutcomeFailed
	defer func() { complete(outcome) }()
	type result struct {
		id       provider.ID
		snapshot provider.Snapshot
		err      error
	}
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan result, len(ids))
	for _, providerID := range ids {
		driver, driverErr := coordinator.providers.Driver(providerID)
		if driverErr != nil {
			return driverErr
		}
		previous := current.Providers[providerID].Clone()
		go func() {
			next, refreshErr := driver.Refresh(child, previous)
			if refreshErr != nil {
				cancel()
			}
			results <- result{id: providerID, snapshot: next, err: refreshErr}
		}()
	}
	providers := cloneProviders(current.Providers)
	for range ids {
		result := <-results
		if result.err != nil {
			return result.err
		}
		if err := provider.ValidateSnapshot(result.snapshot, result.id, coordinator.now()); err != nil {
			return err
		}
		providers[result.id] = result.snapshot.Clone()
	}
	if err := coordinator.publish(providers); err != nil {
		return err
	}
	outcome = state.OutcomeSucceeded
	return nil
}

// Logout removes only the selected provider account. Other providers and their
// nodes remain active and are rebound atomically.
func (coordinator *Coordinator) Logout(_ context.Context, id provider.ID) error {
	if !id.Valid() {
		return provider.ErrUnknownProvider
	}
	current := coordinator.manager.Current()
	if current == nil {
		return provider.ErrNoSession
	}
	if _, available := current.Providers[id]; !available {
		return provider.ErrNoSession
	}
	complete, err := coordinator.manager.Begin(state.OperationRefresh)
	if err != nil {
		return err
	}
	outcome := state.OutcomeFailed
	defer func() { complete(outcome) }()
	providers := cloneProviders(current.Providers)
	delete(providers, id)
	pruneUnusableProviders(providers, coordinator.now())
	if len(providers) == 0 {
		if err := coordinator.commit(nil); err != nil {
			return err
		}
		coordinator.manager.SignOut()
		outcome = state.OutcomeSucceeded
		return nil
	}
	if err := coordinator.publish(providers); err != nil {
		return err
	}
	outcome = state.OutcomeSucceeded
	return nil
}

// Expire removes expired providers while preserving every still-usable account.
func (coordinator *Coordinator) Expire(now time.Time) (bool, error) {
	current := coordinator.manager.Current()
	if current == nil || len(current.Providers) == 0 {
		return false, nil
	}
	providers := cloneProviders(current.Providers)
	changed := false
	for id, snapshot := range providers {
		if !snapshot.UsableAt(now) {
			delete(providers, id)
			changed = true
		}
	}
	if !changed {
		return false, nil
	}
	if len(providers) == 0 {
		if err := coordinator.commit(nil); err != nil {
			return false, err
		}
		return true, coordinator.manager.MarkExpired(now)
	}
	complete, err := coordinator.manager.Begin(state.OperationRefresh)
	if err != nil {
		return false, err
	}
	outcome := state.OutcomeFailed
	defer func() { complete(outcome) }()
	if err := coordinator.publish(providers); err != nil {
		return false, err
	}
	outcome = state.OutcomeSucceeded
	return true, nil
}

func refreshIDs(providers map[provider.ID]provider.Snapshot, requested provider.ID) ([]provider.ID, error) {
	if requested.Valid() {
		if _, available := providers[requested]; !available {
			return nil, provider.ErrNoSession
		}
		return []provider.ID{requested}, nil
	}
	if requested != "" {
		return nil, provider.ErrUnknownProvider
	}
	ids := make([]provider.ID, 0, len(providers))
	for id := range providers {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(left, right int) bool { return ids[left] < ids[right] })
	return ids, nil
}

func (coordinator *Coordinator) publish(providers map[provider.ID]provider.Snapshot) error {
	now := coordinator.now().UTC()
	pruneUnusableProviders(providers, now)
	if len(providers) == 0 {
		return provider.ErrNoSession
	}
	current := coordinator.manager.Current()
	generation := uint64(1)
	var previous map[string]state.NodeRef
	if current != nil {
		generation = current.Generation + 1
		previous = current.Selectors
	}
	nodes, err := runtimeNodes(providers)
	if err != nil {
		return err
	}
	builtNodes, selectors, err := coordinator.buildSelectors(generation, nodes, previous)
	if err != nil {
		return fmt.Errorf("provider coordinator: build selectors: %w", err)
	}
	snapshot := &state.RuntimeSnapshot{
		Generation: generation, CreatedAt: now, ExpiresAt: state.ProviderExpiresAt(providers),
		Providers: cloneProviders(providers), Nodes: builtNodes, Selectors: selectors,
	}
	return coordinator.commit(snapshot)
}

func runtimeNodes(providers map[provider.ID]provider.Snapshot) ([]state.Node, error) {
	ids := make([]provider.ID, 0, len(providers))
	for id := range providers {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(left, right int) bool { return ids[left] < ids[right] })
	total := 0
	for _, id := range ids {
		total += len(providers[id].Nodes)
	}
	nodes := make([]state.Node, 0, total)
	seen := make(map[string]struct{}, total)
	for _, id := range ids {
		snapshot := providers[id]
		if snapshot.Provider != id || !snapshot.Valid() {
			return nil, provider.ErrInvalidSnapshot
		}
		for _, source := range snapshot.Nodes {
			if !source.Eligible {
				continue
			}
			if _, duplicate := seen[source.ID]; duplicate {
				return nil, fmt.Errorf("provider coordinator: duplicate node id %s", source.ID)
			}
			seen[source.ID] = struct{}{}
			nodes = append(nodes, state.Node{
				ID: source.ID, Provider: id, Protocol: source.Protocol, AuthorityID: source.AuthorityID,
				Host: source.Host, Port: source.Port, Name: source.Name, Group: source.Group, Model: source.Model,
				Weight: source.Weight, Auto: source.Auto, Eligible: source.Eligible,
				Health: state.NodeHealthUnknown, UDPHealth: state.UDPHealthUnknown,
			})
		}
	}
	return nodes, nil
}

func cloneProviders(source map[provider.ID]provider.Snapshot) map[provider.ID]provider.Snapshot {
	clone := make(map[provider.ID]provider.Snapshot, len(source))
	for id, snapshot := range source {
		clone[id] = snapshot.Clone()
	}
	return clone
}

func pruneUnusableProviders(providers map[provider.ID]provider.Snapshot, now time.Time) {
	for id, snapshot := range providers {
		if !snapshot.UsableAt(now) {
			delete(providers, id)
		}
	}
}

func (coordinator *Coordinator) currentProviders() map[provider.ID]provider.Snapshot {
	current := coordinator.manager.Current()
	if current == nil {
		return make(map[provider.ID]provider.Snapshot)
	}
	return cloneProviders(current.Providers)
}

func (coordinator *Coordinator) buildSelectors(_ uint64, nodes []state.Node, _ map[string]state.NodeRef) ([]state.Node, map[string]state.NodeRef, error) {
	selectors, err := coordinator.builder.Build(nodes)
	if err != nil {
		return nil, nil, err
	}
	byID := make(map[string]int, len(nodes))
	for index := range nodes {
		byID[nodes[index].ID] = index
	}
	for name, reference := range selectors {
		index, exists := byID[reference.NodeID]
		if !exists {
			return nil, nil, provider.ErrInvalidSnapshot
		}
		nodes[index].Selector = name
	}
	return nodes, selectors, nil
}

func (coordinator *Coordinator) now() time.Time { return coordinator.clock() }
