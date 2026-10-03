package web

import (
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"

	"context"
	"github.com/kfadapter/kfadapter/internal/selector"
	"github.com/kfadapter/kfadapter/internal/smart"
	"github.com/kfadapter/kfadapter/internal/state"
)

func TestSmartProxyAPIRequiresSessionAndCSRF(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := state.NewSQLiteStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	persistent, err := store.LoadOrCreate()
	if err != nil {
		t.Fatal(err)
	}
	registry, err := selector.NewRegistry(persistent.Subscription)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := state.NewManager(nil)
	if err != nil {
		t.Fatal(err)
	}
	service, err := smart.New(smart.Config{Manager: manager, Store: store, Registry: func() *selector.Registry { return registry }, MutationMu: &sync.Mutex{}, Probe: func(context.Context, state.TunnelPin, smart.Target) smart.Measurement { return smart.Measurement{} }})
	if err != nil {
		t.Fatal(err)
	}
	api, err := NewAPI(testConfig(), Dependencies{Backend: &fakeBackend{}, Smart: service})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ method, path string }{{"GET", "/api/v1/smart-proxy"}, {"GET", "/api/v1/smart-proxy/details"}, {"PUT", "/api/v1/smart-proxy/config"}, {"POST", "/api/v1/smart-proxy/probe"}} {
		if response := request(api, test.method, test.path, "{}", nil, testOrigin); response.Code != http.StatusUnauthorized {
			t.Fatal(test, response.Code)
		}
	}
	cookie, csrf := establish(t, api)
	policy := `{"enabled":true,"intervalMinutes":60}`
	if response := request(api, "PUT", "/api/v1/smart-proxy/config", policy, cookie, testOrigin); response.Code != http.StatusForbidden {
		t.Fatal("missing CSRF accepted")
	}
	if response := requestWithCSRF(api, "PUT", "/api/v1/smart-proxy/config", policy, cookie, csrf); response.Code != http.StatusOK {
		t.Fatal(response.Code, response.Body.String())
	}
	for _, body := range []string{`{"enabled":true,"intervalMinutes":1}`, `{"intervalMinutes":30}`, `{"enabled":true,"intervalMinutes":30,"url":"http://private/"}`} {
		if response := requestWithCSRF(api, "PUT", "/api/v1/smart-proxy/config", body, cookie, csrf); response.Code != http.StatusBadRequest {
			t.Fatal("invalid policy accepted", body, response.Code)
		}
	}
	response := request(api, "GET", "/api/v1/smart-proxy", "", cookie, "")
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" || strings.Contains(response.Body.String(), registry.SmartCredentials().Password) {
		t.Fatal("status boundary", response.Code, response.Body.String())
	}
	if response := requestWithCSRF(api, "POST", "/api/v1/smart-proxy/probe", "{}", cookie, csrf); response.Code != http.StatusAccepted {
		t.Fatal(response.Code)
	}
	policy = `{"enabled":false,"intervalMinutes":30}`
	if response := requestWithCSRF(api, "PUT", "/api/v1/smart-proxy/config", policy, cookie, csrf); response.Code != http.StatusOK {
		t.Fatal(response.Code)
	}
	if response := requestWithCSRF(api, "POST", "/api/v1/smart-proxy/probe", "{}", cookie, csrf); response.Code != http.StatusConflict {
		t.Fatal("disabled probe accepted")
	}
	if response := request(api, "GET", "/api/v1/smart-proxy/details", "", cookie, ""); response.Code != http.StatusServiceUnavailable {
		t.Fatal("disabled credentials exposed")
	}
}
