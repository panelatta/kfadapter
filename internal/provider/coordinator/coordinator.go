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

// Refresh refreshes one provider or all active providers when id is empty.
// Providers refresh independently: every successful result is published even
// when another provider fails, and one provider's failure never cancels or
// discards another's refresh. Failures are returned joined, each wrapped in a
// RefreshError naming its provider.
func (coordinator *Coordinator) Refresh(ctx context.Context, id provider.ID) error {
	if current := coordinator.manager.Current(); current == nil || len(current.Providers) == 0 {
		return provider.ErrNoSession
	}
	complete, err := coordinator.manager.Begin(state.OperationRefresh)
	if err != nil {
		return err
	}
	outcome := state.OutcomeFailed
	defer func() { complete(outcome) }()
	current := coordinator.manager.Current()
	if current == nil || len(current.Providers) == 0 {
		return provider.ErrNoSession
	}
	ids, err := refreshIDs(current.Providers, id)
	if err != nil {
		return err
	}
	drivers := make(map[provider.ID]provider.Driver, len(ids))
	for _, providerID := range ids {
		driver, driverErr := coordinator.providers.Driver(providerID)
		if driverErr != nil {
			return driverErr
		}
		drivers[providerID] = driver
	}
	type result struct {
		id       provider.ID
		snapshot provider.Snapshot
		err      error
	}
	results := make(chan result, len(ids))
	for _, providerID := range ids {
		driver := drivers[providerID]
		previous := current.Providers[providerID].Clone()
		go func() {
			next, refreshErr := driver.Refresh(ctx, previous)
			results <- result{id: providerID, snapshot: next, err: refreshErr}
		}()
	}
	providers := cloneProviders(current.Providers)
	var failures []error
	refreshed := 0
	for range ids {
		result := <-results
		if result.err == nil {
			result.err = provider.ValidateSnapshot(result.snapshot, result.id, coordinator.now())
		}
		if result.err != nil {
			failures = append(failures, &RefreshError{Provider: result.id, Err: result.err})
			continue
		}
		providers[result.id] = result.snapshot.Clone()
		refreshed++
	}
	if refreshed > 0 {
		if err := coordinator.publish(providers); err != nil {
			return errors.Join(append(failures, err)...)
		}
	}
	if len(failures) != 0 {
		return errors.Join(failures...)
	}
	outcome = state.OutcomeSucceeded
	return nil
}

// RefreshError reports one provider's refresh failure.
type RefreshError struct {
	Provider provider.ID
	Err      error
}

func (e *RefreshError) Error() string {
	return fmt.Sprintf("provider %s refresh: %v", e.Provider, e.Err)
}

func (e *RefreshError) Unwrap() error { return e.Err }

// Logout removes only the selected provider account. Other providers and their
// nodes remain active and are rebound atomically.
func (coordinator *Coordinator) Logout(_ context.Context, id provider.ID) error {
	if !id.Valid() {
		return provider.ErrUnknownProvider
	}
	if current := coordinator.manager.Current(); current == nil {
		return provider.ErrNoSession
	} else if _, available := current.Providers[id]; !available {
		return provider.ErrNoSession
	}
	complete, err := coordinator.manager.Begin(state.OperationRefresh)
	if err != nil {
		return err
	}
	outcome := state.OutcomeFailed
	defer func() { complete(outcome) }()
	current := coordinator.manager.Current()
	if current == nil {
		return provider.ErrNoSession
	}
	if _, available := current.Providers[id]; !available {
		return provider.ErrNoSession
	}
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
		// Expiry of the final provider is a terminal transition valid from any
		// active state, so it does not take a refresh lease. Callers serialize
		// Expire with every other account mutation.
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
	// Re-read under the lease so a concurrent mutation cannot be overwritten.
	if current = coordinator.manager.Current(); current == nil || len(current.Providers) == 0 {
		return false, nil
	}
	providers = cloneProviders(current.Providers)
	pruneUnusableProviders(providers, now)
	if len(providers) == 0 || len(providers) == len(current.Providers) {
		return false, nil
	}
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
	generation := uint64(1)
	if current := coordinator.manager.Current(); current != nil {
		generation = current.Generation + 1
	}
	nodes, err := runtimeNodes(providers)
	if err != nil {
		return err
	}
	builtNodes, selectors, err := coordinator.buildSelectors(nodes)
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

func (coordinator *Coordinator) buildSelectors(nodes []state.Node) ([]state.Node, map[string]state.NodeRef, error) {
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
