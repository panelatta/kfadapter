package web

import (
	"context"
	"net/http"
	"net/url"
)

// SmartProxy is the browser-safe facade over the automatic route. Status is
// already browser-safe; Credentials returns only locally derived SOCKS
// credentials and is called only on explicit demand.
type SmartProxy interface {
	Status() any
	Configure(ctx context.Context, enabled bool, intervalMinutes int) error
	RequestProbe() bool
	Credentials() (username, password string, err error)
}

func (a *API) smartProxyStatus(w http.ResponseWriter, _ *http.Request) {
	if a.smart == nil {
		a.backendUnavailable(w)
		return
	}
	a.writeJSON(w, http.StatusOK, a.smart.Status())
}

func (a *API) smartProxyConfigure(w http.ResponseWriter, r *http.Request) {
	if a.smart == nil {
		a.backendUnavailable(w)
		return
	}
	var body struct {
		Enabled         *bool `json:"enabled"`
		IntervalMinutes int   `json:"intervalMinutes"`
	}
	if err := readJSONBody(w, r, a.config.JSONBodyLimit, &body); err != nil {
		a.writeBodyError(w, err)
		return
	}
	if body.Enabled == nil || (body.IntervalMinutes != 30 && body.IntervalMinutes != 60) {
		a.writeProblem(w, http.StatusBadRequest, "invalid_smart_policy", "Use a 30 or 60 minute interval")
		return
	}
	if err := a.smart.Configure(r.Context(), *body.Enabled, body.IntervalMinutes); err != nil {
		a.writeBackendError(w, err, "smart_config_failed", http.StatusServiceUnavailable)
		return
	}
	a.writeJSON(w, http.StatusOK, a.smart.Status())
}

func (a *API) smartProxyProbe(w http.ResponseWriter, r *http.Request) {
	if a.smart == nil {
		a.backendUnavailable(w)
		return
	}
	if err := a.requireEmptyJSON(w, r); err != nil {
		a.writeBodyError(w, err)
		return
	}
	if !a.smart.RequestProbe() {
		a.writeProblem(w, http.StatusConflict, "smart_probe_unavailable", "Enable smart proxy and wait for the current probe to finish")
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (a *API) smartProxyDetails(w http.ResponseWriter, r *http.Request) {
	if a.smart == nil {
		a.backendUnavailable(w)
		return
	}
	username, password, err := a.smart.Credentials()
	if err != nil {
		a.writeBackendError(w, err, "smart_proxy_unavailable", http.StatusServiceUnavailable)
		return
	}
	address := a.socksAddress(r)
	link := (&url.URL{Scheme: "socks5", Host: address, User: url.UserPassword(username, password), Fragment: "Smart domestic route"}).String()
	var profiles []ProxyProfile
	if a.inbounds != nil {
		profiles, _ = a.inbounds.SmartProfiles(a.advertisedHost(r))
	}
	a.writeJSON(w, http.StatusOK, struct {
		Address  string         `json:"socksAddress"`
		Username string         `json:"socksUsername"`
		Password string         `json:"socksPassword"`
		URL      string         `json:"url"`
		Profiles []ProxyProfile `json:"profiles,omitempty"`
	}{address, username, password, link, profiles})
}
