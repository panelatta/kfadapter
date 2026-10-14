package app

import (
	"context"

	"github.com/oshinop/chinadapter/internal/smart"
	"github.com/oshinop/chinadapter/internal/state"
	"github.com/oshinop/chinadapter/internal/web"
)

// NewSmartProxyAdapter exposes the automatic route service to web.
func NewSmartProxyAdapter(service *smart.Service) web.SmartProxy {
	if service == nil {
		return nil
	}
	return smartProxyAdapter{service: service}
}

type smartProxyAdapter struct{ service *smart.Service }

func (a smartProxyAdapter) Status() any { return a.service.Status() }

func (a smartProxyAdapter) Configure(ctx context.Context, enabled bool, intervalMinutes int) error {
	return a.service.Configure(ctx, state.SmartProxyPreferences{Enabled: enabled, IntervalMinutes: intervalMinutes})
}

func (a smartProxyAdapter) RequestProbe() bool { return a.service.RequestProbe() }

func (a smartProxyAdapter) Credentials() (string, string, error) {
	credentials, err := a.service.Credentials()
	return credentials.Username, credentials.Password, err
}
