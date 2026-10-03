package web

import (
	"context"
	"net"
	"net/http"
	"time"

	"golang.org/x/net/netutil"
)

// NewHTTPServer constructs the production HTTP server around a validated API.
// Management operations and their response writes have separate time budgets.
// Authenticated SSE handlers retain their own per-write deadlines.
func NewHTTPServer(config Config, dependencies Dependencies) (*http.Server, *API, error) {
	api, err := NewAPI(config, dependencies)
	if err != nil {
		return nil, nil, err
	}
	return &http.Server{
		Addr:              api.config.Listen,
		Handler:           api.httpHandler(),
		BaseContext:       func(net.Listener) context.Context { return api.requests },
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       api.config.ReadTimeout,
		WriteTimeout:      api.config.OperationTimeout + api.config.WriteTimeout,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}, api, nil
}

// Listen binds the configured management address. Cmd owns serving and
// shutdown orchestration.
func (a *API) Listen() (net.Listener, error) {
	if a == nil {
		return nil, net.ErrClosed
	}
	listener, err := net.Listen("tcp", a.config.Listen)
	if err != nil {
		return nil, err
	}
	return netutil.LimitListener(listener, a.config.MaxConnections), nil
}

// CancelRequests cancels the context of every in-flight request. Callers use it
// when shutting down so long backend calls do not outlive the drain deadline.
func (a *API) CancelRequests() {
	if a != nil && a.cancelRequests != nil {
		a.cancelRequests()
	}
}
