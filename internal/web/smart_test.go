package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/oshinop/chinadapter/internal/state"
)

type fakeSmartProxy struct {
	mu        sync.Mutex
	enabled   bool
	interval  int
	running   bool
	probes    int
	configErr error
}

func (f *fakeSmartProxy) Status() any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return map[string]any{"enabled": f.enabled, "intervalMinutes": f.interval}
}

func (f *fakeSmartProxy) Configure(_ context.Context, enabled bool, interval int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.configErr != nil {
		return f.configErr
	}
	f.enabled, f.interval = enabled, interval
	return nil
}

func (f *fakeSmartProxy) RequestProbe() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.enabled || f.running {
		return false
	}
	f.probes++
	return true
}

func (f *fakeSmartProxy) Credentials() (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.enabled {
		return "", "", state.ErrSelectorUnknown
	}
	return "a_smart-user", "smart-password", nil
}

func TestSmartProxyAPIRequiresSessionAndOriginProtection(t *testing.T) {
	smart := &fakeSmartProxy{interval: 30}
	api, err := NewAPI(testConfig(), Dependencies{Backend: &fakeBackend{}, Subscriptions: newFakeSubscriptions(), Smart: smart})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ method, path string }{{http.MethodGet, "/api/v1/smart-proxy"}, {http.MethodGet, "/api/v1/smart-proxy/details"}, {http.MethodPut, "/api/v1/smart-proxy/config"}, {http.MethodPost, "/api/v1/smart-proxy/probe"}} {
		if response := request(api, test.method, test.path, "{}", nil, testOrigin); response.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s without session = %d", test.method, test.path, response.Code)
		}
	}
	cookie := establish(t, api)
	policy := `{"enabled":true,"intervalMinutes":60}`
	if response := rawRequest(api, http.MethodPut, "/api/v1/smart-proxy/config", policy, cookie, testOrigin, "cross-site"); response.Code != http.StatusForbidden {
		t.Fatalf("cross-site PUT = %d", response.Code)
	}
	plainRequest := httptest.NewRequest(http.MethodPut, "http://"+testHost+"/api/v1/smart-proxy/config", strings.NewReader(policy))
	plainRequest.Host = testHost
	plainRequest.Header.Set("Content-Type", "text/plain")
	plainRequest.Header.Set("Origin", testOrigin)
	plainRequest.AddCookie(cookie)
	plain := httptest.NewRecorder()
	api.ServeHTTP(plain, plainRequest)
	if plain.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("non-JSON PUT = %d", plain.Code)
	}
	if response := request(api, http.MethodPut, "/api/v1/smart-proxy/config", policy, cookie, testOrigin); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"intervalMinutes":60`) {
		t.Fatalf("configure = %d: %s", response.Code, response.Body.String())
	}
	for _, body := range []string{`{"enabled":true,"intervalMinutes":1}`, `{"intervalMinutes":30}`, `{"enabled":true,"intervalMinutes":30,"url":"http://private/"}`} {
		if response := request(api, http.MethodPut, "/api/v1/smart-proxy/config", body, cookie, testOrigin); response.Code != http.StatusBadRequest {
			t.Fatalf("invalid policy %s = %d", body, response.Code)
		}
	}
	if response := request(api, http.MethodGet, "/api/v1/smart-proxy", "", cookie, ""); response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" || strings.Contains(response.Body.String(), "smart-password") {
		t.Fatalf("status = %d %q", response.Code, response.Body.String())
	}
	if response := request(api, http.MethodPost, "/api/v1/smart-proxy/probe", "{}", cookie, testOrigin); response.Code != http.StatusAccepted {
		t.Fatalf("probe = %d", response.Code)
	}
	details := request(api, http.MethodGet, "/api/v1/smart-proxy/details", "", cookie, "")
	if details.Code != http.StatusOK || !strings.Contains(details.Body.String(), `"url":"socks5://a_smart-user:smart-password@127.0.0.1:10808#Smart%20domestic%20route"`) {
		t.Fatalf("details = %d %s", details.Code, details.Body.String())
	}
	if response := request(api, http.MethodPut, "/api/v1/smart-proxy/config", `{"enabled":false,"intervalMinutes":30}`, cookie, testOrigin); response.Code != http.StatusOK {
		t.Fatalf("disable = %d", response.Code)
	}
	if response := request(api, http.MethodPost, "/api/v1/smart-proxy/probe", "{}", cookie, testOrigin); response.Code != http.StatusConflict {
		t.Fatalf("disabled probe = %d", response.Code)
	}
	if response := request(api, http.MethodGet, "/api/v1/smart-proxy/details", "", cookie, ""); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled details = %d", response.Code)
	}
	if response := request(api, http.MethodDelete, "/api/v1/smart-proxy/config", "", cookie, testOrigin); response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != http.MethodPut {
		t.Fatalf("wrong method = %d %q", response.Code, response.Header().Get("Allow"))
	}
}

func TestSmartProxyAPIWithoutServiceIsUnavailable(t *testing.T) {
	api := newTestAPI(t, &fakeBackend{}, newFakeSubscriptions())
	cookie := establish(t, api)
	if response := request(api, http.MethodGet, "/api/v1/smart-proxy", "", cookie, ""); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status without service = %d", response.Code)
	}
}
