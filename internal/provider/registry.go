package provider

import (
	"fmt"
	"sort"
)

// Registry is an immutable dispatch table for provider lifecycle drivers and
// data-plane transports.
type Registry struct {
	drivers    map[ID]Driver
	transports map[Protocol]Transport
}

func NewRegistry(drivers []Driver, transports []Transport) (*Registry, error) {
	registry := &Registry{
		drivers: make(map[ID]Driver, len(drivers)), transports: make(map[Protocol]Transport, len(transports)),
	}
	for _, driver := range drivers {
		if driver == nil || !driver.ID().Valid() {
			return nil, fmt.Errorf("provider: invalid driver")
		}
		if _, duplicate := registry.drivers[driver.ID()]; duplicate {
			return nil, fmt.Errorf("provider: duplicate driver %s", driver.ID())
		}
		registry.drivers[driver.ID()] = driver
	}
	for _, transport := range transports {
		if transport == nil || !transport.Protocol().Valid() {
			return nil, fmt.Errorf("provider: invalid transport")
		}
		if _, duplicate := registry.transports[transport.Protocol()]; duplicate {
			return nil, fmt.Errorf("provider: duplicate transport %s", transport.Protocol())
		}
		registry.transports[transport.Protocol()] = transport
	}
	if len(registry.drivers) == 0 && len(registry.transports) == 0 {
		return nil, fmt.Errorf("provider: at least one driver or transport is required")
	}
	return registry, nil
}

func (registry *Registry) Driver(id ID) (Driver, error) {
	if registry == nil {
		return nil, ErrUnknownProvider
	}
	driver, available := registry.drivers[id]
	if !available {
		return nil, ErrUnknownProvider
	}
	return driver, nil
}

func (registry *Registry) Transport(protocol Protocol) (Transport, error) {
	if registry == nil {
		return nil, ErrUnknownTransport
	}
	transport, available := registry.transports[protocol]
	if !available {
		return nil, ErrUnknownTransport
	}
	return transport, nil
}

func (registry *Registry) IDs() []ID {
	if registry == nil {
		return nil
	}
	ids := make([]ID, 0, len(registry.drivers))
	for id := range registry.drivers {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(left, right int) bool { return ids[left] < ids[right] })
	return ids
}
