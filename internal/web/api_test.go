package web

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testHost         = "127.0.0.1:10809"
	testOrigin       = "http://127.0.0.1:10809"
	validTestToken   = "access-token-12345"
	invalidTestToken = "invalid-token-123"
)

func testConfig() Config {
	return Config{Listen: testHost}
}

type publicError struct {
	status int
}

func (e publicError) Error() string   { return "public access error" }
func (e publicError) Code() string    { return "access_error" }
func (e publicError) HTTPStatus() int { return e.status }

type fakeBackend struct {
	mu                   sync.Mutex
	accessInitialized    bool
	accessToken          string
	accessSetupCalls     int
	accessLoginCalls     int
	accessSetupStarted   chan<- struct{}
	accessSetupRelease   <-chan struct{}
	nodes                []Node
	nodeDetails          NodeDetails
	nodeDetailsCalls     int
	nodeDetailsID        string
	accountLoginCalls    int
	accountLoginProvider string
	accountLoginStarted  chan<- struct{}
	accountLoginRelease  <-chan struct{}
	logoutCalls          int
	logoutProvider       string
	refreshProvider      string
	events               chan Event
	subscribed           chan struct{}
}

func (f *fakeBackend) AccessStatus(context.Context) (AccessStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return AccessStatus{Initialized: f.accessInitialized}, nil
}

func (f *fakeBackend) AccessSetup(_ context.Context, token string) error {
	f.mu.Lock()
	f.accessSetupCalls++
	started, release := f.accessSetupStarted, f.accessSetupRelease
	f.mu.Unlock()
	if started != nil {
		started <- struct{}{}
	}
	if release != nil {
		<-release
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.accessInitialized {
		return publicError{status: http.StatusConflict}
	}
	f.accessInitialized = true
	f.accessToken = token
	return nil
}

func (f *fakeBackend) AccessLogin(_ context.Context, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accessLoginCalls++
	if !f.accessInitialized || subtle.ConstantTimeCompare([]byte(f.accessToken), []byte(token)) != 1 {
		return publicError{status: http.StatusUnauthorized}
	}
	return nil
}

func (f *fakeBackend) Status(context.Context) (Status, error) {
	return Status{State: "ready", Providers: []string{"kuaifan"}, Accounts: map[string]Account{"kuaifan": {Provider: "kuaifan", Display: "person@example.com", Tier: "Standard"}}}, nil
}
func (f *fakeBackend) Nodes(context.Context) ([]Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Node(nil), f.nodes...), nil
}
func (f *fakeBackend) NodeDetails(_ context.Context, id string) (NodeDetails, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nodeDetailsCalls++
	f.nodeDetailsID = id
	return f.nodeDetails, nil
}
func (f *fakeBackend) Login(_ context.Context, input LoginInput) (Account, error) {
	f.mu.Lock()
	f.accountLoginCalls++
	f.accountLoginProvider = input.Provider
	started, release := f.accountLoginStarted, f.accountLoginRelease
	f.mu.Unlock()
	if started != nil {
		started <- struct{}{}
	}
	if release != nil {
		<-release
	}
	if input.Account == "fail@example.com" {
		return Account{}, errors.New("rejected")
	}
	return Account{Provider: input.Provider, Display: input.Account}, nil
}
func (f *fakeBackend) Logout(_ context.Context, provider string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logoutCalls++
	f.logoutProvider = provider
	return nil
}
func (f *fakeBackend) Refresh(_ context.Context, provider string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshProvider = provider
	return nil
}
func (f *fakeBackend) Probe(context.Context, string) (ProbeResult, error) {
	return ProbeResult{Health: "healthy", ProbedAt: time.Now().UTC()}, nil
}
func (f *fakeBackend) Diagnostics(context.Context) (any, error) {
	return map[string]any{"safe": "value"}, nil
}
func (f *fakeBackend) Subscribe(context.Context) (<-chan Event, func(), error) {
	f.mu.Lock()
	if f.events == nil {
		f.events = make(chan Event, 1)
	}
	events, subscribed := f.events, f.subscribed
	f.mu.Unlock()
	if subscribed != nil {
		subscribed <- struct{}{}
	}
	return events, func() {}, nil
}

type fakeSubscriptions struct {
	binding  string
	metadata SubscriptionMetadata
	mu       sync.Mutex
	urlCalls int
}

func newFakeSubscriptions() *fakeSubscriptions {
	return &fakeSubscriptions{
		binding:  base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)),
		metadata: SubscriptionMetadata{Active: true, NodeCount: 1},
	}
}

func (s *fakeSubscriptions) Metadata(context.Context) (SubscriptionMetadata, error) {
	return s.metadata, nil
}
func (s *fakeSubscriptions) SubscriptionURL(_ context.Context, baseURL, _ string) (SubscriptionURL, error) {
	s.mu.Lock()
	s.urlCalls++
	s.mu.Unlock()
	return SubscriptionURL{URL: strings.TrimRight(baseURL, "/") + "/sub/" + s.binding}, nil
}
func (s *fakeSubscriptions) ServeSubscription(w http.ResponseWriter, _ *http.Request, binding, _ string) {
	if binding != s.binding {
		writeEmptyNotFound(w)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("subscription"))
}

func newTestAPI(t *testing.T, backend *fakeBackend, subscriptions *fakeSubscriptions) *API {
	t.Helper()
	api, err := NewAPI(testConfig(), Dependencies{Backend: backend, Subscriptions: subscriptions})
	if err != nil {
		t.Fatal(err)
	}
	return api
}

func TestAPIWildcardAndHostnamePolicy(t *testing.T) {
	const wildcardHost = "0.0.0.0:10809"
	api, err := NewAPI(Config{Listen: wildcardHost, Hostname: "adapter.example.com"}, Dependencies{Backend: &fakeBackend{}})
	if err != nil {
		t.Fatal(err)
	}
	const remoteHost = "192.0.2.10:10809"
	if got, want := api.baseURL(remoteHost), "http://"+remoteHost; got != want {
		t.Fatalf("subscription base URL = %q, want %q", got, want)
	}

	for _, host := range []string{wildcardHost, testHost, remoteHost, "localhost:10809", "LOCALHOST:10809", "adapter.example.com:10809", "ADAPTER.EXAMPLE.COM:10809"} {
		request := httptest.NewRequest(http.MethodGet, "http://"+host+"/healthz", nil)
		request.Host = host
		response := httptest.NewRecorder()
		api.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("Host %q = %d: %s", host, response.Code, response.Body.String())
		}
	}
	for _, host := range []string{"192.0.2.10:10810", "[::1]:10809"} {
		request := httptest.NewRequest(http.MethodGet, "http://"+host+"/healthz", nil)
		request.Host = host
		response := httptest.NewRecorder()
		api.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid Host %q = %d", host, response.Code)
		}
	}

	if got := api.baseURL("LOCALHOST:10809"); got != "http://localhost:10809" {
		t.Fatalf("localhost base URL = %q", got)
	}
	localhostRequest := httptest.NewRequest(http.MethodGet, "http://localhost:10809/", nil)
	if got := api.socksAddress(localhostRequest); got != "localhost:10808" {
		t.Fatalf("localhost SOCKS address = %q", got)
	}
	loopback, err := NewAPI(Config{Listen: testHost}, Dependencies{})
	if err != nil {
		t.Fatal(err)
	}
	loopbackRequest := httptest.NewRequest(http.MethodGet, "http://localhost:10809/healthz", nil)
	loopbackResponse := httptest.NewRecorder()
	loopback.ServeHTTP(loopbackResponse, loopbackRequest)
	if loopbackResponse.Code != http.StatusOK {
		t.Fatalf("loopback listener rejected implicit localhost: %d", loopbackResponse.Code)
	}
	specific, err := NewAPI(Config{Listen: "192.0.2.10:10809"}, Dependencies{})
	if err != nil {
		t.Fatal(err)
	}
	specificRequest := httptest.NewRequest(http.MethodGet, "http://localhost:10809/healthz", nil)
	specificResponse := httptest.NewRecorder()
	specific.ServeHTTP(specificResponse, specificRequest)
	if specificResponse.Code != http.StatusBadRequest {
		t.Fatalf("non-loopback listener accepted implicit localhost: %d", specificResponse.Code)
	}

	setup := func(origin string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "http://"+remoteHost+"/api/v1/access/setup", strings.NewReader(`{"token":"`+validTestToken+`"}`))
		request.Host = remoteHost
		request.RemoteAddr = "127.0.0.1:40001"
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", origin)
		response := httptest.NewRecorder()
		api.ServeHTTP(response, request)
		return response
	}
	if malformedOrigin := setup("http://" + remoteHost + "/"); malformedOrigin.Code != http.StatusForbidden {
		t.Fatalf("malformed Origin = %d", malformedOrigin.Code)
	}
	if exactOrigin := setup("http://" + remoteHost); exactOrigin.Code != http.StatusCreated {
		t.Fatalf("exact Origin = %d: %s", exactOrigin.Code, exactOrigin.Body.String())
	}
}

func TestAPIEndpointConfiguration(t *testing.T) {
	for _, listen := range []string{"127.0.0.1:10809", "192.0.2.10:10809", "[2001:db8::1]:10809"} {
		if _, err := NewAPI(Config{Listen: listen}, Dependencies{}); err != nil {
			t.Fatalf("NewAPI(%q): %v", listen, err)
		}
	}
	for _, config := range []Config{
		{Listen: "localhost:10809"},
		{Listen: "127.0.0.1:010809"},
		{Listen: testHost, Hostname: "http://adapter.example.com"},
		{Listen: testHost, Hostname: "adapter.example.com:10809"},
	} {
		if _, err := NewAPI(config, Dependencies{}); err == nil {
			t.Fatalf("NewAPI(%#v) accepted invalid endpoint policy", config)
		}
	}
}

func TestAPIDerivesWildcardSocksAddressFromReachedHost(t *testing.T) {
	api, err := NewAPI(Config{Listen: "0.0.0.0:10809", SocksListen: "0.0.0.0:10808", Hostname: "adapter.example.com"}, Dependencies{})
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"192.0.2.10", "198.51.100.20"} {
		numeric := httptest.NewRequest(http.MethodGet, "http://"+host+":10809/", nil)
		if got := api.socksAddress(numeric); got != host+":10808" {
			t.Fatalf("numeric reached address = %q", got)
		}
	}
	hostname := httptest.NewRequest(http.MethodGet, "http://adapter.example.com:10809/", nil)
	if got := api.socksAddress(hostname); got != "adapter.example.com:10808" {
		t.Fatalf("hostname SOCKS address = %q", got)
	}
	if got := api.baseURL("ADAPTER.EXAMPLE.COM:10809"); got != "http://adapter.example.com:10809" {
		t.Fatalf("hostname base URL = %q", got)
	}
	numeric := httptest.NewRequest(http.MethodGet, "http://192.0.2.10:10809/", nil)
	specific, err := NewAPI(Config{Listen: "0.0.0.0:10809", SocksListen: "192.0.2.20:10808"}, Dependencies{})
	if err != nil {
		t.Fatal(err)
	}
	if got := specific.socksAddress(numeric); got != "192.0.2.20:10808" {
		t.Fatalf("specific SOCKS address = %q", got)
	}
}

func TestAPIAdvertisesHostnameUsedByRequest(t *testing.T) {
	subscriptions := newFakeSubscriptions()
	api, err := NewAPI(Config{Listen: "0.0.0.0:10809", SocksListen: "0.0.0.0:10808", Hostname: "adapter.example.com"}, Dependencies{Backend: &fakeBackend{}, Subscriptions: subscriptions})
	if err != nil {
		t.Fatal(err)
	}
	cookie, _ := establish(t, api)

	statusRequest := httptest.NewRequest(http.MethodGet, "http://adapter.example.com:10809/api/v1/status", nil)
	statusRequest.Host = "adapter.example.com:10809"
	statusRequest.AddCookie(cookie)
	statusResponse := httptest.NewRecorder()
	api.ServeHTTP(statusResponse, statusRequest)
	if statusResponse.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", statusResponse.Code, statusResponse.Body.String())
	}
	var status Status
	if err := json.Unmarshal(statusResponse.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.DataPlane.SocksAddress != "adapter.example.com:10808" {
		t.Fatalf("advertised SOCKS address = %q", status.DataPlane.SocksAddress)
	}

	urlRequest := httptest.NewRequest(http.MethodGet, "http://ADAPTER.EXAMPLE.COM:10809/api/v1/subscription/url", nil)
	urlRequest.Host = "ADAPTER.EXAMPLE.COM:10809"
	urlRequest.AddCookie(cookie)
	urlResponse := httptest.NewRecorder()
	api.ServeHTTP(urlResponse, urlRequest)
	if urlResponse.Code != http.StatusOK {
		t.Fatalf("subscription URL = %d: %s", urlResponse.Code, urlResponse.Body.String())
	}
	var subscriptionURL SubscriptionURL
	if err := json.Unmarshal(urlResponse.Body.Bytes(), &subscriptionURL); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(subscriptionURL.URL, "http://adapter.example.com:10809/sub/") {
		t.Fatalf("advertised subscription URL = %q", subscriptionURL.URL)
	}
}

func TestAPIAdvertisesImplicitLocalhost(t *testing.T) {
	subscriptions := newFakeSubscriptions()
	api, err := NewAPI(Config{Listen: "0.0.0.0:10809", SocksListen: "0.0.0.0:10808"}, Dependencies{Backend: &fakeBackend{}, Subscriptions: subscriptions})
	if err != nil {
		t.Fatal(err)
	}
	cookie, _ := establish(t, api)

	statusRequest := httptest.NewRequest(http.MethodGet, "http://LOCALHOST:10809/api/v1/status", nil)
	statusRequest.Host = "LOCALHOST:10809"
	statusRequest.AddCookie(cookie)
	statusResponse := httptest.NewRecorder()
	api.ServeHTTP(statusResponse, statusRequest)
	if statusResponse.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", statusResponse.Code, statusResponse.Body.String())
	}
	var status Status
	if err := json.Unmarshal(statusResponse.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.DataPlane.SocksAddress != "localhost:10808" {
		t.Fatalf("advertised SOCKS address = %q", status.DataPlane.SocksAddress)
	}

	urlRequest := httptest.NewRequest(http.MethodGet, "http://localhost:10809/api/v1/subscription/url", nil)
	urlRequest.Host = "localhost:10809"
	urlRequest.AddCookie(cookie)
	urlResponse := httptest.NewRecorder()
	api.ServeHTTP(urlResponse, urlRequest)
	if urlResponse.Code != http.StatusOK {
		t.Fatalf("subscription URL = %d: %s", urlResponse.Code, urlResponse.Body.String())
	}
	var subscriptionURL SubscriptionURL
	if err := json.Unmarshal(urlResponse.Body.Bytes(), &subscriptionURL); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(subscriptionURL.URL, "http://localhost:10809/sub/") {
		t.Fatalf("advertised subscription URL = %q", subscriptionURL.URL)
	}
}

func TestAccessSetupLoginStatusAndReload(t *testing.T) {
	backend := &fakeBackend{}
	api := newTestAPI(t, backend, newFakeSubscriptions())

	initial := request(api, http.MethodGet, "/api/v1/access/status", "", nil, "")
	assertAccessStatus(t, initial, false, false, false)
	if initial.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("access status cache control = %q", initial.Header().Get("Cache-Control"))
	}

	setup := request(api, http.MethodPost, "/api/v1/access/setup", `{"token":"  `+validTestToken+`  "}`, nil, testOrigin)
	if setup.Code != http.StatusCreated {
		t.Fatalf("setup = %d: %s", setup.Code, setup.Body.String())
	}
	cookie, csrf := sessionFromResponse(t, setup)
	assertAccessStatus(t, setup, true, true, true)
	backend.mu.Lock()
	storedToken := backend.accessToken
	backend.mu.Unlock()
	if storedToken != validTestToken {
		t.Fatalf("setup token was not trimmed: %q", storedToken)
	}

	reloaded := request(api, http.MethodGet, "/api/v1/access/status", "", cookie, "")
	assertAccessStatus(t, reloaded, true, true, true)
	var reloadBody struct {
		CSRFToken string `json:"csrfToken"`
	}
	if err := json.NewDecoder(reloaded.Body).Decode(&reloadBody); err != nil {
		t.Fatal(err)
	}
	if reloadBody.CSRFToken != csrf {
		t.Fatalf("reload csrf = %q, want original session csrf", reloadBody.CSRFToken)
	}

	conflict := request(api, http.MethodPost, "/api/v1/access/setup", `{"token":"`+validTestToken+`"}`, nil, testOrigin)
	if conflict.Code != http.StatusConflict || !strings.Contains(conflict.Body.String(), "access_initialized") {
		t.Fatalf("repeated setup = %d: %s", conflict.Code, conflict.Body.String())
	}
	invalid := request(api, http.MethodPost, "/api/v1/access/login", `{"token":"`+invalidTestToken+`"}`, nil, testOrigin)
	if invalid.Code != http.StatusUnauthorized || !strings.Contains(invalid.Body.String(), "invalid_access_token") || strings.Contains(invalid.Body.String(), invalidTestToken) {
		t.Fatalf("invalid login = %d: %s", invalid.Code, invalid.Body.String())
	}
	login := request(api, http.MethodPost, "/api/v1/access/login", `{"token":"`+validTestToken+`"}`, nil, testOrigin)
	if login.Code != http.StatusOK {
		t.Fatalf("access login = %d: %s", login.Code, login.Body.String())
	}
	_, _ = sessionFromResponse(t, login)
}

func TestAccessTokenValidationBounds(t *testing.T) {
	for _, test := range []struct {
		token string
		valid bool
	}{
		{token: strings.Repeat("a", 16), valid: true},
		{token: strings.Repeat("a", 128), valid: true},
		{token: strings.Repeat("a", 15), valid: false},
		{token: strings.Repeat("a", 129), valid: false},
		{token: string([]byte{0xff}) + strings.Repeat("a", 16), valid: false},
	} {
		if got := validAccessToken(test.token); got != test.valid {
			t.Fatalf("validAccessToken(%q) = %t, want %t", test.token, got, test.valid)
		}
	}
}

func TestAccessSetupRequiresLoopbackClient(t *testing.T) {
	backend := &fakeBackend{}
	api := newTestAPI(t, backend, newFakeSubscriptions())
	status := func(remote string) map[string]any {
		req := httptest.NewRequest(http.MethodGet, "http://"+testHost+"/api/v1/access/status", nil)
		req.Host = testHost
		req.RemoteAddr = remote
		response := httptest.NewRecorder()
		api.ServeHTTP(response, req)
		var body map[string]any
		if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	if body := status("192.0.2.1:10001"); body["setupAllowed"] != false {
		t.Fatalf("LAN client may set up: %v", body)
	}
	if body := status("[::1]:10001"); body["setupAllowed"] != true {
		t.Fatalf("loopback client may not set up: %v", body)
	}
	req := httptest.NewRequest(http.MethodPost, "http://"+testHost+"/api/v1/access/setup", strings.NewReader(`{"token":"`+validTestToken+`"}`))
	req.Host = testHost
	req.RemoteAddr = "192.0.2.1:10001"
	req.Header.Set("Origin", testOrigin)
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	api.ServeHTTP(response, req)
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "setup_requires_loopback") {
		t.Fatalf("LAN setup = %d %s", response.Code, response.Body.String())
	}
	backend.mu.Lock()
	calls := backend.accessSetupCalls
	backend.mu.Unlock()
	if calls != 0 {
		t.Fatal("LAN setup reached the backend")
	}
}

func TestAccessSetupFirstClaimIsAtomic(t *testing.T) {
	backend := &fakeBackend{}
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	backend.accessSetupStarted = started
	backend.accessSetupRelease = release
	api := newTestAPI(t, backend, newFakeSubscriptions())

	setup := func() *httptest.ResponseRecorder {
		return request(api, http.MethodPost, "/api/v1/access/setup", `{"token":"`+validTestToken+`"}`, nil, testOrigin)
	}
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- setup() }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("setup did not reach backend")
	}
	// Every loopback peer shares one admission slot, so a concurrent claim is
	// refused while the first is in flight.
	if concurrent := setup(); concurrent.Code != http.StatusTooManyRequests {
		t.Fatalf("concurrent setup = %d", concurrent.Code)
	}
	close(release)
	if response := <-first; response.Code != http.StatusCreated {
		t.Fatalf("first setup = %d", response.Code)
	}
	if again := setup(); again.Code != http.StatusConflict {
		t.Fatalf("second claim = %d", again.Code)
	}
	backend.mu.Lock()
	initialized := backend.accessInitialized
	backend.mu.Unlock()
	if !initialized {
		t.Fatal("first claim did not initialize")
	}
}

func TestAccessThrottleAndPreSessionChecks(t *testing.T) {
	backend := &fakeBackend{}
	api := newTestAPI(t, backend, newFakeSubscriptions())
	if response := request(api, http.MethodPost, "/api/v1/access/setup", `{"token":"`+validTestToken+`"}`, nil, "http://evil.test"); response.Code != http.StatusForbidden {
		t.Fatalf("cross-origin setup = %d", response.Code)
	}
	nonJSONRequest := httptest.NewRequest(http.MethodPost, "http://"+testHost+"/api/v1/access/login", strings.NewReader(`{"token":"`+validTestToken+`"}`))
	nonJSONRequest.Host = testHost
	nonJSONRequest.Header.Set("Origin", testOrigin)
	nonJSON := httptest.NewRecorder()
	api.ServeHTTP(nonJSON, nonJSONRequest)
	if nonJSON.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("non-JSON login = %d", nonJSON.Code)
	}
	_, _ = establish(t, api)
	for attempt := range 5 {
		response := request(api, http.MethodPost, "/api/v1/access/login", `{"token":"`+invalidTestToken+`"}`, nil, testOrigin)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("invalid access login %d = %d", attempt, response.Code)
		}
	}
	limited := request(api, http.MethodPost, "/api/v1/access/login", `{"token":"`+invalidTestToken+`"}`, nil, testOrigin)
	if limited.Code != http.StatusTooManyRequests || limited.Header().Get("Retry-After") == "" {
		t.Fatalf("access throttle = %d %#v", limited.Code, limited.Header())
	}
}

func TestAccountLogoutPreservesBrowserAccess(t *testing.T) {
	backend := &fakeBackend{}
	api := newTestAPI(t, backend, newFakeSubscriptions())
	cookie, csrf := establish(t, api)

	missingCSRF := request(api, http.MethodPost, "/api/v1/auth/logout", `{"provider":"kuaifan"}`, cookie, testOrigin)
	if missingCSRF.Code != http.StatusForbidden {
		t.Fatalf("account logout without csrf = %d", missingCSRF.Code)
	}
	backend.mu.Lock()
	logoutCalls := backend.logoutCalls
	backend.mu.Unlock()
	if logoutCalls != 0 {
		t.Fatalf("account logout backend calls without csrf = %d", logoutCalls)
	}

	logout := requestWithCSRF(api, http.MethodPost, "/api/v1/auth/logout", `{"provider":"kuaifan"}`, cookie, csrf)
	if logout.Code != http.StatusNoContent || logout.Header().Get("Cache-Control") != "no-store" || logout.Header().Get("Set-Cookie") != "" {
		t.Fatalf("account logout = %d %#v", logout.Code, logout.Header())
	}
	backend.mu.Lock()
	logoutCalls = backend.logoutCalls
	logoutProvider := backend.logoutProvider
	backend.mu.Unlock()
	if logoutCalls != 1 || logoutProvider != "kuaifan" {
		t.Fatalf("account logout backend = calls:%d provider:%q", logoutCalls, logoutProvider)
	}
	accessStatus := request(api, http.MethodGet, "/api/v1/access/status", "", cookie, "")
	assertAccessStatus(t, accessStatus, true, true, true)
	status := request(api, http.MethodGet, "/api/v1/status", "", cookie, "")
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), `"providers":["kuaifan"]`) || !strings.Contains(status.Body.String(), `"accounts":{"kuaifan"`) || !strings.Contains(status.Body.String(), `"tier":"Standard","subscriptionActive":false`) || strings.Contains(status.Body.String(), "person@example.com") || strings.Contains(status.Body.String(), "subscriptionEndsAt") {
		t.Fatalf("provider status after account logout = %d: %s", status.Code, status.Body.String())
	}
}

func TestAuthenticatedMutationsAccountLoginAndAccessLock(t *testing.T) {
	backend := &fakeBackend{}
	api := newTestAPI(t, backend, newFakeSubscriptions())
	cookie, csrf := establish(t, api)

	missingCSRF := request(api, http.MethodPost, "/api/v1/control/refresh", `{"provider":"kuaifan"}`, cookie, testOrigin)
	if missingCSRF.Code != http.StatusForbidden {
		t.Fatalf("mutation without csrf = %d", missingCSRF.Code)
	}
	refreshed := requestWithCSRF(api, http.MethodPost, "/api/v1/control/refresh", `{"provider":"kuaifan"}`, cookie, csrf)
	if refreshed.Code != http.StatusAccepted {
		t.Fatalf("refresh = %d: %s", refreshed.Code, refreshed.Body.String())
	}
	accountLogin := requestWithCSRF(api, http.MethodPost, "/api/v1/auth/login", `{"provider":"kuaifan","account":"person@example.com","password":"password"}`, cookie, csrf)
	if accountLogin.Code != http.StatusOK || strings.Contains(accountLogin.Body.String(), "person@example.com") {
		t.Fatalf("account login = %d: %s", accountLogin.Code, accountLogin.Body.String())
	}
	backend.mu.Lock()
	loginProvider, refreshProvider := backend.accountLoginProvider, backend.refreshProvider
	backend.mu.Unlock()
	if loginProvider != "kuaifan" || refreshProvider != "kuaifan" {
		t.Fatalf("provider-keyed mutations routed login=%q refresh=%q", loginProvider, refreshProvider)
	}
	otherLogin := request(api, http.MethodPost, "/api/v1/access/login", `{"token":"`+validTestToken+`"}`, nil, testOrigin)
	if otherLogin.Code != http.StatusOK {
		t.Fatalf("second access login = %d: %s", otherLogin.Code, otherLogin.Body.String())
	}
	otherCookie, _ := sessionFromResponse(t, otherLogin)
	lock := requestWithCSRF(api, http.MethodPost, "/api/v1/access/logout", `{}`, cookie, csrf)
	if lock.Code != http.StatusNoContent || lock.Header().Get("Cache-Control") != "no-store" || !strings.Contains(lock.Header().Get("Set-Cookie"), "Max-Age=0") {
		t.Fatalf("access lock = %d %#v", lock.Code, lock.Header())
	}
	backend.mu.Lock()
	logoutCalls := backend.logoutCalls
	backend.mu.Unlock()
	if logoutCalls != 0 {
		t.Fatalf("access lock invoked provider logout %d times", logoutCalls)
	}
	if response := request(api, http.MethodGet, "/api/v1/status", "", cookie, ""); response.Code != http.StatusUnauthorized {
		t.Fatalf("revoked status = %d", response.Code)
	}
	if response := request(api, http.MethodGet, "/api/v1/status", "", otherCookie, ""); response.Code != http.StatusOK {
		t.Fatalf("other session status = %d", response.Code)
	}
	legacy := request(api, http.MethodDelete, "/api/v1/auth/session", "", nil, "")
	if legacy.Code != http.StatusNotFound || legacy.Body.Len() != 0 || legacy.Header().Get("Content-Length") != "0" {
		t.Fatalf("removed legacy auth-session route = %d %#v %q", legacy.Code, legacy.Header(), legacy.Body.String())
	}
}

func TestAccessLockCompletesDuringProviderLogin(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	backend := &fakeBackend{accountLoginStarted: started, accountLoginRelease: release}
	api := newTestAPI(t, backend, newFakeSubscriptions())
	cookie, csrf := establish(t, api)

	loginResponses := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		loginResponses <- requestWithCSRF(api, http.MethodPost, "/api/v1/auth/login", `{"provider":"kuaifan","account":"person@example.com","password":"password"}`, cookie, csrf)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("provider login did not reach the backend")
	}

	logoutResponses := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		logoutResponses <- requestWithCSRF(api, http.MethodPost, "/api/v1/access/logout", `{}`, cookie, csrf)
	}()
	select {
	case logout := <-logoutResponses:
		if logout.Code != http.StatusNoContent || !strings.Contains(logout.Header().Get("Set-Cookie"), "Max-Age=0") {
			t.Fatalf("access lock = %d %#v", logout.Code, logout.Header())
		}
	case <-time.After(time.Second):
		t.Fatal("access lock waited for provider login")
	}
	if response := request(api, http.MethodGet, "/api/v1/status", "", cookie, ""); response.Code != http.StatusUnauthorized {
		t.Fatalf("revoked session status = %d", response.Code)
	}
	select {
	case response := <-loginResponses:
		t.Fatalf("provider login completed before release: %d", response.Code)
	default:
	}
	close(release)
	select {
	case response := <-loginResponses:
		if response.Code != http.StatusOK {
			t.Fatalf("provider login = %d: %s", response.Code, response.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("provider login did not complete after release")
	}
	backend.mu.Lock()
	calls := backend.accountLoginCalls
	backend.mu.Unlock()
	if calls != 1 {
		t.Fatalf("provider login calls = %d", calls)
	}
}

func TestNodeDetailsRequiresAuthenticationAndUsesExactRoute(t *testing.T) {
	const nodeID = "node-alpha"
	backend := &fakeBackend{nodeDetails: NodeDetails{ID: nodeID}}
	api := newTestAPI(t, backend, newFakeSubscriptions())

	unauthenticated := request(api, http.MethodGet, "/api/v1/nodes/"+nodeID+"/details", "", nil, "")
	if unauthenticated.Code != http.StatusUnauthorized || unauthenticated.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("unauthenticated node details = %d %#v", unauthenticated.Code, unauthenticated.Header())
	}
	backend.mu.Lock()
	detailCalls := backend.nodeDetailsCalls
	backend.mu.Unlock()
	if detailCalls != 0 {
		t.Fatalf("unauthenticated node details backend calls = %d", detailCalls)
	}

	cookie, csrf := establish(t, api)
	wrongMethod := requestWithCSRF(api, http.MethodPost, "/api/v1/nodes/"+nodeID+"/details", `{}`, cookie, csrf)
	if wrongMethod.Code != http.StatusMethodNotAllowed || wrongMethod.Header().Get("Allow") != http.MethodGet || wrongMethod.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("node details wrong method = %d %#v", wrongMethod.Code, wrongMethod.Header())
	}
	badPath := request(api, http.MethodGet, "/api/v1/nodes//details", "", cookie, "")
	if badPath.Code != http.StatusNotFound || badPath.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("node details bad path = %d %#v", badPath.Code, badPath.Header())
	}

	response := request(api, http.MethodGet, "/api/v1/nodes/"+nodeID+"/details", "", cookie, "")
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("node details = %d %#v", response.Code, response.Header())
	}
	backend.mu.Lock()
	detailCalls, detailID := backend.nodeDetailsCalls, backend.nodeDetailsID
	backend.mu.Unlock()
	if detailCalls != 1 || detailID != nodeID {
		t.Fatalf("node details backend dispatch calls=%d id=%q", detailCalls, detailID)
	}
}

func TestNodesOmitLegacyEndpointAndExcludedFields(t *testing.T) {
	expected := Node{
		ID: "node-alpha", Name: "Alpha", Group: "Core", Provider: "WIFIIN", Health: "healthy",
		TCPLatencyMS: 17, UDPHealth: "healthy", Eligible: true,
	}
	backend := &fakeBackend{nodes: []Node{expected}}
	api := newTestAPI(t, backend, newFakeSubscriptions())
	cookie, _ := establish(t, api)

	response := request(api, http.MethodGet, "/api/v1/nodes", "", cookie, "")
	if response.Code != http.StatusOK {
		t.Fatalf("nodes = %d: %s", response.Code, response.Body.String())
	}
	var payload struct {
		Nodes []Node `json:"nodes"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Nodes) != 1 || payload.Nodes[0] != expected {
		t.Fatalf("nodes response = %#v", payload.Nodes)
	}
	var raw struct {
		Nodes []map[string]json.RawMessage `json:"nodes"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw.Nodes) != 1 {
		t.Fatalf("raw nodes response = %#v", raw.Nodes)
	}
	for _, field := range []string{"endpoint", "excluded", "activeConnections"} {
		if _, present := raw.Nodes[0][field]; present {
			t.Fatalf("routine node exposed removed %s field: %s", field, response.Body.String())
		}
	}
}

func TestNodeDetailsReturnsOnlyLocalCredentials(t *testing.T) {
	expected := NodeDetails{
		ID: "node-alpha", Name: "Alpha", Group: "Core", Provider: "WIFIIN",
		UpstreamHost: "upstream.example.test", UpstreamPort: 443,
		SocksAddress: "127.0.0.1:10808", SocksUsername: "local-selector", SocksPassword: "local-password",
		Health: "healthy", TCPLatencyMS: 17,
	}
	backend := &fakeBackend{nodeDetails: expected}
	api := newTestAPI(t, backend, newFakeSubscriptions())
	cookie, _ := establish(t, api)

	response := request(api, http.MethodGet, "/api/v1/nodes/"+expected.ID+"/details", "", cookie, "")
	if response.Code != http.StatusOK {
		t.Fatalf("node details = %d: %s", response.Code, response.Body.String())
	}
	var details NodeDetails
	if err := json.Unmarshal(response.Body.Bytes(), &details); err != nil {
		t.Fatal(err)
	}
	if details != expected {
		t.Fatalf("node details response = %#v", details)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw) != 11 {
		t.Fatalf("node details response fields = %#v", raw)
	}
	for _, field := range []string{"loginToken", "providerToken", "tunnelPassword", "tunnelMethod", "providerExtension", "selector", "endpoint", "excluded", "activeConnections"} {
		if _, present := raw[field]; present {
			t.Fatalf("node details exposed provider field %s: %s", field, response.Body.String())
		}
	}
}

func TestStatusUsesReachedLANAddress(t *testing.T) {
	api, err := NewAPI(Config{Listen: "0.0.0.0:10809", SocksListen: "0.0.0.0:10808"}, Dependencies{Backend: &fakeBackend{}, Subscriptions: newFakeSubscriptions()})
	if err != nil {
		t.Fatal(err)
	}
	cookie, _ := establish(t, api)
	statusRequest := httptest.NewRequest(http.MethodGet, "http://192.0.2.10:10809/api/v1/status", nil)
	statusRequest.Host = "192.0.2.10:10809"
	statusRequest.AddCookie(cookie)
	response := httptest.NewRecorder()
	api.ServeHTTP(response, statusRequest)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	var raw struct {
		DataPlane map[string]json.RawMessage `json:"dataPlane"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	var socksAddress string
	if err := json.Unmarshal(raw.DataPlane["socksAddress"], &socksAddress); err != nil || socksAddress != "192.0.2.10:10808" {
		t.Fatalf("status SOCKS address = %q, %v", socksAddress, err)
	}
	for _, field := range []string{"activeTcp", "activeUdpAssociations"} {
		if _, present := raw.DataPlane[field]; present {
			t.Fatalf("status exposed removed %s field: %s", field, response.Body.String())
		}
	}
}

func TestStableSubscriptionURLAndRemovedRoutes(t *testing.T) {
	backend := &fakeBackend{}
	subscriptions := newFakeSubscriptions()
	api := newTestAPI(t, backend, subscriptions)
	cookie, _ := establish(t, api)

	first := request(api, http.MethodGet, "/api/v1/subscription/url", "", cookie, "")
	second := request(api, http.MethodGet, "/api/v1/subscription/url", "", cookie, "")
	if first.Code != http.StatusOK || second.Code != http.StatusOK || first.Body.String() != second.Body.String() {
		t.Fatalf("stable URL responses = %d/%d %q/%q", first.Code, second.Code, first.Body.String(), second.Body.String())
	}
	var result SubscriptionURL
	if err := json.NewDecoder(first.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	expected := testOrigin + "/sub/" + subscriptions.binding
	if result.URL != expected {
		t.Fatalf("stable URL = %#v", result)
	}
	status := request(api, http.MethodGet, "/api/v1/status", "", cookie, "")
	if strings.Contains(status.Body.String(), result.URL) {
		t.Fatalf("ordinary endpoint exposed subscription URL: status=%s", status.Body.String())
	}
	for _, legacy := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/v1/local-session"},
		{http.MethodPost, "/api/v1/subscription/reveal"},
		{http.MethodPost, "/api/v1/subscription/rotation"},
		{http.MethodPost, "/api/v1/subscription/rotation/old/commit"},
		{http.MethodGet, "/api/v1/settings"},
		{http.MethodPut, "/api/v1/settings"},
		{http.MethodPut, "/api/v1/nodes/node-alpha/preference"},
	} {
		response := request(api, legacy.method, legacy.path, `{}`, nil, testOrigin)
		if response.Code != http.StatusNotFound {
			t.Fatalf("legacy route %s %s = %d", legacy.method, legacy.path, response.Code)
		}
	}
	for _, route := range []string{
		"/sub",
		"/sub/short",
		"/sub/" + strings.Repeat("A", 42),
		"/sub/" + strings.Repeat("!", 43),
		"/sub/" + subscriptions.binding + "/subscription",
		"/sub/" + subscriptions.binding + "/",
		"/sub/" + subscriptions.binding + "/other",
	} {
		response := request(api, http.MethodGet, route, "", nil, "")
		if response.Code != http.StatusNotFound || response.Body.Len() != 0 || response.Header().Get("Content-Length") != "0" || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("invalid subscription route %s = %d %#v %q", route, response.Code, response.Header(), response.Body.String())
		}
	}
	served := request(api, http.MethodGet, "/sub/"+subscriptions.binding, "", nil, "")
	if served.Code != http.StatusOK || served.Body.String() != "subscription" {
		t.Fatalf("stable external subscription = %d %q", served.Code, served.Body.String())
	}
}

func TestSSESurvivesAccessMigration(t *testing.T) {
	backend := &fakeBackend{events: make(chan Event), subscribed: make(chan struct{}, 1)}
	api := newTestAPI(t, backend, newFakeSubscriptions())
	cookie, _ := establish(t, api)
	context, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "http://"+testHost+"/api/v1/events", nil).WithContext(context)
	req.Host = testHost
	req.Header.Set("Origin", testOrigin)
	req.AddCookie(cookie)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		api.ServeHTTP(response, req)
		close(done)
	}()
	select {
	case <-backend.subscribed:
	case <-time.After(time.Second):
		t.Fatal("SSE did not subscribe")
	}
	backend.events <- Event{Type: "state", Data: map[string]any{"state": "ready"}}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("SSE did not close after cancellation")
	}
	if !strings.Contains(response.Body.String(), "event: state") {
		t.Fatalf("SSE payload = %q", response.Body.String())
	}
}

func TestSSEAcceptsSameOriginBrowserRequestsWithoutOrigin(t *testing.T) {
	backend := &fakeBackend{events: make(chan Event), subscribed: make(chan struct{}, 1)}
	api := newTestAPI(t, backend, newFakeSubscriptions())
	cookie, _ := establish(t, api)
	open := func(headers map[string]string) (*httptest.ResponseRecorder, context.CancelFunc, chan struct{}) {
		ctx, cancel := context.WithCancel(context.Background())
		req := httptest.NewRequest(http.MethodGet, "http://"+testHost+"/api/v1/events", nil).WithContext(ctx)
		req.Host = testHost
		for name, value := range headers {
			req.Header.Set(name, value)
		}
		req.AddCookie(cookie)
		response := httptest.NewRecorder()
		done := make(chan struct{})
		go func() {
			api.ServeHTTP(response, req)
			close(done)
		}()
		return response, cancel, done
	}
	// Chromium sends no Origin on a same-origin EventSource GET.
	_, cancel, done := open(map[string]string{"Sec-Fetch-Site": "same-origin", "Sec-Fetch-Mode": "cors"})
	select {
	case <-backend.subscribed:
	case <-time.After(time.Second):
		t.Fatal("same-origin SSE without Origin was rejected")
	}
	cancel()
	<-done
	for _, headers := range []map[string]string{
		{"Sec-Fetch-Site": "cross-site"},
		{"Sec-Fetch-Site": "same-site"},
		{"Origin": "http://evil.example"},
	} {
		response, cancel, done := open(headers)
		<-done
		cancel()
		if response.Code != http.StatusForbidden {
			t.Fatalf("SSE with %v = %d, want 403", headers, response.Code)
		}
	}
}

func establish(t *testing.T, api *API) (*http.Cookie, string) {
	t.Helper()
	response := request(api, http.MethodPost, "/api/v1/access/setup", `{"token":"`+validTestToken+`"}`, nil, testOrigin)
	if response.Code != http.StatusCreated {
		t.Fatalf("setup = %d: %s", response.Code, response.Body.String())
	}
	return sessionFromResponse(t, response)
}

func sessionFromResponse(t *testing.T, response *httptest.ResponseRecorder) (*http.Cookie, string) {
	t.Helper()
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie = %#v", cookies)
	}
	var body struct {
		CSRFToken string `json:"csrfToken"`
	}
	if err := json.NewDecoder(bytes.NewReader(response.Body.Bytes())).Decode(&body); err != nil || body.CSRFToken == "" {
		t.Fatalf("session response = %v %q", err, response.Body.String())
	}
	return cookies[0], body.CSRFToken
}

func assertAccessStatus(t *testing.T, response *httptest.ResponseRecorder, initialized, authenticated, csrf bool) {
	t.Helper()
	if response.Code != http.StatusOK && response.Code != http.StatusCreated {
		t.Fatalf("access status response = %d: %s", response.Code, response.Body.String())
	}
	var body struct {
		Initialized   bool       `json:"initialized"`
		Authenticated bool       `json:"authenticated"`
		CSRFToken     string     `json:"csrfToken"`
		ExpiresAt     *time.Time `json:"expiresAt"`
	}
	if err := json.NewDecoder(bytes.NewReader(response.Body.Bytes())).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Initialized != initialized || body.Authenticated != authenticated || (body.CSRFToken != "") != csrf || (body.ExpiresAt != nil) != csrf {
		t.Fatalf("access status = %#v", body)
	}
}

func requestWithCSRF(api *API, method, route, body string, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	return rawRequest(api, method, route, body, cookie, testOrigin, csrf)
}

func request(api *API, method, route, body string, cookie *http.Cookie, origin string) *httptest.ResponseRecorder {
	return rawRequest(api, method, route, body, cookie, origin, "")
}

func rawRequest(api *API, method, route, body string, cookie *http.Cookie, origin, csrf string) *httptest.ResponseRecorder {
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	} else {
		reader = strings.NewReader("")
	}
	req := httptest.NewRequest(method, "http://"+testHost+route, reader)
	req.Host = testHost
	req.RemoteAddr = "127.0.0.1:40000"
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if csrf != "" {
		req.Header.Set(csrfHeaderName, csrf)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	api.ServeHTTP(response, req)
	return response
}

func TestClientIdentityGroupsIPv6Prefixes(t *testing.T) {
	if clientIdentity("[2001:db8:1:2::10]:1000") != clientIdentity("[2001:db8:1:2:ffff::1]:2000") {
		t.Fatal("addresses in one IPv6 /64 have separate budgets")
	}
	if clientIdentity("[2001:db8:1:2::10]:1000") == clientIdentity("[2001:db8:1:3::10]:1000") {
		t.Fatal("different IPv6 /64 prefixes share a budget")
	}
	if clientIdentity("192.0.2.1:1") == clientIdentity("192.0.2.2:1") {
		t.Fatal("IPv4 addresses share a budget")
	}
	if clientIdentity("[::1]:1") != "loopback" || clientIdentity("127.0.0.2:1") != "loopback" {
		t.Fatal("loopback is not grouped")
	}
}

func TestHashedAssetsAreImmutableAndIndexIsNotCached(t *testing.T) {
	api := newTestAPI(t, &fakeBackend{}, newFakeSubscriptions())
	entries, err := fs.ReadDir(staticFiles, "static/dist/assets")
	if err != nil || len(entries) == 0 {
		t.Skip("frontend build output is not embedded")
	}
	asset := request(api, http.MethodGet, "/assets/"+entries[0].Name(), "", nil, "")
	if asset.Code != http.StatusOK || !strings.Contains(asset.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("asset = %d %q", asset.Code, asset.Header().Get("Cache-Control"))
	}
	index := request(api, http.MethodGet, "/", "", nil, "")
	if index.Code != http.StatusOK || index.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("index = %d %q", index.Code, index.Header().Get("Cache-Control"))
	}
	spa := request(api, http.MethodGet, "/signin", "", nil, "")
	if spa.Code != http.StatusOK || spa.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("SPA fallback = %d %q", spa.Code, spa.Header().Get("Cache-Control"))
	}
	missing := request(api, http.MethodGet, "/assets/missing.js", "", nil, "")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing asset = %d", missing.Code)
	}
}

func problemCode(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Code string `json:"code"`
	}
	_ = json.NewDecoder(bytes.NewReader(response.Body.Bytes())).Decode(&body)
	return body.Code
}

func TestStateChangingRequestGuards(t *testing.T) {
	api := newTestAPI(t, &fakeBackend{}, newFakeSubscriptions())
	cookie, csrf := establish(t, api)
	if response := rawRequest(api, http.MethodPost, "/api/v1/control/refresh", `{}`, cookie, testOrigin, "wrong"); response.Code != http.StatusForbidden || problemCode(t, response) != "csrf_failed" {
		t.Fatalf("bad CSRF = %d %s", response.Code, response.Body.String())
	}
	if response := rawRequest(api, http.MethodPost, "/api/v1/control/refresh", `{}`, cookie, "http://evil.example", csrf); response.Code != http.StatusForbidden || problemCode(t, response) != "invalid_origin" {
		t.Fatalf("bad Origin = %d %s", response.Code, response.Body.String())
	}
	large := `{"provider":"` + strings.Repeat("a", 1<<20) + `"}`
	if response := requestWithCSRF(api, http.MethodPost, "/api/v1/control/refresh", large, cookie, csrf); response.Code != http.StatusRequestEntityTooLarge || problemCode(t, response) != "body_too_large" {
		t.Fatalf("oversized body = %d %s", response.Code, response.Body.String())
	}
	if response := requestWithCSRF(api, http.MethodPost, "/api/v1/control/refresh", `{"provider":`, cookie, csrf); response.Code != http.StatusBadRequest || problemCode(t, response) != "invalid_json" {
		t.Fatalf("malformed JSON = %d %s", response.Code, response.Body.String())
	}
	textRequest := httptest.NewRequest(http.MethodPost, "http://"+testHost+"/api/v1/control/refresh", strings.NewReader(`{}`))
	textRequest.Host = testHost
	textRequest.RemoteAddr = "127.0.0.1:40000"
	textRequest.Header.Set("Content-Type", "text/plain")
	textRequest.Header.Set("Origin", testOrigin)
	textRequest.Header.Set(csrfHeaderName, csrf)
	textRequest.AddCookie(cookie)
	textResponse := httptest.NewRecorder()
	api.ServeHTTP(textResponse, textRequest)
	if textResponse.Code != http.StatusUnsupportedMediaType || problemCode(t, textResponse) != "invalid_content_type" {
		t.Fatalf("non-JSON body = %d %s", textResponse.Code, textResponse.Body.String())
	}
}

func TestUnknownHostIsRejected(t *testing.T) {
	api := newTestAPI(t, &fakeBackend{}, newFakeSubscriptions())
	req := httptest.NewRequest(http.MethodGet, "http://attacker.example/api/v1/access/status", nil)
	req.Host = "attacker.example:10809"
	response := httptest.NewRecorder()
	api.ServeHTTP(response, req)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("DNS-rebinding host = %d %s", response.Code, response.Body.String())
	}
}

func TestProviderLoginThrottling(t *testing.T) {
	backend := &fakeBackend{}
	api := newTestAPI(t, backend, newFakeSubscriptions())
	cookie, csrf := establish(t, api)
	login := func(account string) *httptest.ResponseRecorder {
		return requestWithCSRF(api, http.MethodPost, "/api/v1/auth/login", `{"provider":"kuaifan","account":"`+account+`","password":"secret"}`, cookie, csrf)
	}
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	backend.mu.Lock()
	backend.accountLoginStarted, backend.accountLoginRelease = started, release
	backend.mu.Unlock()
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- login("person@example.com") }()
	<-started
	if concurrent := login("person@example.com"); concurrent.Code != http.StatusTooManyRequests || problemCode(t, concurrent) != "login_in_progress" {
		t.Fatalf("concurrent login = %d %s", concurrent.Code, concurrent.Body.String())
	}
	close(release)
	<-first
	backend.mu.Lock()
	backend.accountLoginStarted, backend.accountLoginRelease = nil, nil
	backend.mu.Unlock()
	for range 5 {
		_ = login("fail@example.com")
	}
	if limited := login("fail@example.com"); limited.Code != http.StatusTooManyRequests || problemCode(t, limited) != "login_rate_limited" || limited.Header().Get("Retry-After") == "" {
		t.Fatalf("rate-limited login = %d %s", limited.Code, limited.Body.String())
	}
}

type secretDiagnosticsBackend struct{ fakeBackend }

func (*secretDiagnosticsBackend) Diagnostics(context.Context) (any, error) {
	return map[string]any{"version": "1", "token": "very-secret", "nested": map[string]any{"password": "hunter2"}}, nil
}

func TestDiagnosticsExportIsRedactedAttachment(t *testing.T) {
	backend := &secretDiagnosticsBackend{}
	api, err := NewAPI(testConfig(), Dependencies{Backend: backend, Subscriptions: newFakeSubscriptions()})
	if err != nil {
		t.Fatal(err)
	}
	cookie, csrf := establish(t, api)
	response := requestWithCSRF(api, http.MethodPost, "/api/v1/diagnostics/export", `{}`, cookie, csrf)
	if response.Code != http.StatusOK || !strings.Contains(response.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("diagnostics = %d %#v", response.Code, response.Header())
	}
	body := response.Body.String()
	if strings.Contains(body, "very-secret") || strings.Contains(body, "hunter2") || !strings.Contains(body, `"version":"1"`) {
		t.Fatalf("diagnostics were not redacted: %s", body)
	}
}
