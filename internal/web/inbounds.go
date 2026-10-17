package web

import (
	"context"
	"net"
	"net/http"
)

// Inbounds is the browser-safe facade over the inbound protocols. Profiles
// carry locally derived credentials and are returned only on explicit demand
// by node and smart-route details.
type Inbounds interface {
	Status() InboundsStatus
	Configure(context.Context, InboundChoices) error
	NodeProfiles(nodeID, host string) ([]ProxyProfile, error)
	SmartProfiles(host string) ([]ProxyProfile, error)
}

// InboundChoices chooses the served inbound protocols.
type InboundChoices struct {
	SOCKS5      bool
	Snell       bool
	Shadowsocks bool
	SocksTLS    bool
}

// InboundsStatus lists every inbound protocol. TLSFingerprint is the pinned
// SHA-256 of the SOCKS5-TLS certificate, a public value.
type InboundsStatus struct {
	Inbounds       []InboundStatus `json:"inbounds"`
	TLSFingerprint string          `json:"tlsFingerprint,omitempty"`
}

// InboundStatus reports one protocol. Problem is "listen_failed" when an
// enabled protocol could not bind its port.
type InboundStatus struct {
	Protocol  string `json:"protocol"`
	Port      int    `json:"port"`
	Enabled   bool   `json:"enabled"`
	Listening bool   `json:"listening"`
	Problem   string `json:"problem,omitempty"`
}

// ProxyProfile is one route reached through one enabled protocol: a Surge
// policy line and, for SOCKS5 and Shadowsocks, a URI.
type ProxyProfile struct {
	Protocol string `json:"protocol"`
	Port     int    `json:"port"`
	Surge    string `json:"surge"`
	URI      string `json:"uri,omitempty"`
}

func (a *API) inboundsStatus(w http.ResponseWriter, _ *http.Request) {
	if a.inbounds == nil {
		a.backendUnavailable(w)
		return
	}
	a.writeJSON(w, http.StatusOK, a.inbounds.Status())
}

func (a *API) inboundsConfigure(w http.ResponseWriter, r *http.Request) {
	if a.inbounds == nil {
		a.backendUnavailable(w)
		return
	}
	var body struct {
		SOCKS5      *bool `json:"socks5"`
		Snell       *bool `json:"snell"`
		Shadowsocks *bool `json:"shadowsocks"`
		SocksTLS    *bool `json:"socksTLS"`
	}
	if err := readJSONBody(w, r, a.config.JSONBodyLimit, &body); err != nil {
		a.writeBodyError(w, err)
		return
	}
	if body.Snell == nil || body.Shadowsocks == nil || body.SocksTLS == nil {
		a.writeProblem(w, http.StatusBadRequest, "invalid_inbounds", "Set snell, shadowsocks and socksTLS")
		return
	}
	// socks5 is optional, so a console loaded before SOCKS5 could be turned
	// off keeps it as it is.
	socks5 := a.socks5Enabled()
	if body.SOCKS5 != nil {
		socks5 = *body.SOCKS5
	}
	if err := a.inbounds.Configure(r.Context(), InboundChoices{SOCKS5: socks5, Snell: *body.Snell, Shadowsocks: *body.Shadowsocks, SocksTLS: *body.SocksTLS}); err != nil {
		a.writeBackendError(w, err, "inbounds_config_failed", http.StatusServiceUnavailable)
		return
	}
	a.writeJSON(w, http.StatusOK, a.inbounds.Status())
}

// socks5Enabled reports the current SOCKS5 choice; without inbound status
// SOCKS5 is served.
func (a *API) socks5Enabled() bool {
	if a.inbounds == nil {
		return true
	}
	for _, inbound := range a.inbounds.Status().Inbounds {
		if inbound.Protocol == "socks5" {
			return inbound.Enabled
		}
	}
	return true
}

// advertisedHost is the host clients use for every inbound listener: the
// host part of the request's SOCKS advertisement.
func (a *API) advertisedHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(a.socksAddress(r))
	if err != nil {
		return ""
	}
	return host
}
