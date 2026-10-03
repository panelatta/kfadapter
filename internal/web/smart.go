package web

import (
	"net/http"
	"net/url"

	"github.com/kfadapter/kfadapter/internal/state"
)

func (a *API) smartProxy(w http.ResponseWriter, r *http.Request) {
	if a.smart == nil {
		a.backendUnavailable(w)
		return
	}
	switch r.URL.Path {
	case "/api/v1/smart-proxy":
		a.writeJSON(w, http.StatusOK, a.smart.Status())
	case "/api/v1/smart-proxy/config":
		var body struct {
			Enabled         *bool `json:"enabled"`
			IntervalMinutes int   `json:"intervalMinutes"`
		}
		if err := readJSONBody(w, r, a.config.JSONBodyLimit, &body); err != nil {
			a.writeBodyError(w, err)
			return
		}
		if body.Enabled == nil || (body.IntervalMinutes != 30 && body.IntervalMinutes != 60) {
			a.writeProblem(w, http.StatusBadRequest, "invalid_smart_policy", "Use a 30 or 60 minute interval", "")
			return
		}
		if err := a.smart.Configure(state.SmartProxyPreferences{Enabled: *body.Enabled, IntervalMinutes: body.IntervalMinutes}); err != nil {
			a.writeBackendError(w, err, "smart_config_failed", http.StatusServiceUnavailable)
			return
		}
		a.writeJSON(w, http.StatusOK, a.smart.Status())
	case "/api/v1/smart-proxy/probe":
		if err := a.requireEmptyJSON(w, r); err != nil {
			a.writeBodyError(w, err)
			return
		}
		if !a.smart.RequestProbe() {
			a.writeProblem(w, http.StatusConflict, "smart_probe_unavailable", "Enable smart proxy and wait for the current probe to finish", "")
			return
		}
		w.WriteHeader(http.StatusAccepted)
	case "/api/v1/smart-proxy/details":
		credentials, err := a.smart.Credentials()
		if err != nil {
			a.writeBackendError(w, err, "smart_proxy_unavailable", http.StatusServiceUnavailable)
			return
		}
		address := a.socksAddress(r)
		link := (&url.URL{Scheme: "socks5", Host: address, User: url.UserPassword(credentials.Selector, credentials.Password), Fragment: "Smart domestic route"}).String()
		a.writeJSON(w, http.StatusOK, struct {
			Address  string `json:"socksAddress"`
			Username string `json:"socksUsername"`
			Password string `json:"socksPassword"`
			URL      string `json:"url"`
		}{address, credentials.Selector, credentials.Password, link})
	}
}
