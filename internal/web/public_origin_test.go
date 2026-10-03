package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
	"time"
)

const publicTestOrigin = "https://console.example.com:8443"
const publicTestHost = "console.example.com:8443"

func TestHTTPSReverseProxyConsole(t *testing.T) {
	backend := &fakeBackend{events: make(chan Event), subscribed: make(chan struct{}, 1)}
	subscriptions := newFakeSubscriptions()
	local := httptest.NewUnstartedServer(nil)
	config := Config{
		Listen: local.Listener.Addr().String(), SocksListen: "0.0.0.0:10808",
		Hostname: "socks.example.com", PublicOrigin: publicTestOrigin,
	}
	api, err := NewAPI(config, Dependencies{Backend: backend, Subscriptions: subscriptions})
	if err != nil {
		t.Fatal(err)
	}
	local.Config.Handler = api
	local.Start()
	defer local.Close()
	localURL, _ := url.Parse(local.URL)
	proxy := httptest.NewTLSServer(httputil.NewSingleHostReverseProxy(localURL))
	defer proxy.Close()
	client := proxy.Client()
	client.Jar, _ = cookiejar.New(nil)
	client.Timeout = 3 * time.Second

	do := func(method, route, body, origin, csrf string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, proxy.URL+route, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Host = publicTestHost
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if csrf != "" {
			req.Header.Set(csrfHeaderName, csrf)
		}
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { response.Body.Close() })
		return response
	}
	assertStatus := func(response *http.Response, want int) []byte {
		t.Helper()
		contents, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || response.StatusCode != want {
			t.Fatalf("response = %d %s, error %v; want %d", response.StatusCode, contents, err, want)
		}
		return contents
	}

	// A reverse proxy's loopback connection cannot claim first-time setup.
	status := assertStatus(do(http.MethodGet, "/api/v1/access/status", "", "", ""), http.StatusOK)
	if !strings.Contains(string(status), `"setupAllowed":false`) {
		t.Fatalf("public setup availability = %s", status)
	}
	body := `{"token":"` + validTestToken + `"}`
	assertStatus(do(http.MethodPost, "/api/v1/access/setup", body, publicTestOrigin, ""), http.StatusForbidden)
	if backend.accessSetupCalls != 0 {
		t.Fatal("public proxy reached bootstrap backend")
	}

	// The original local HTTP bootstrap and healthcheck still work.
	health, err := http.Get(local.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	assertStatus(health, http.StatusOK)
	setup, _ := http.NewRequest(http.MethodPost, local.URL+"/api/v1/access/setup", strings.NewReader(body))
	setup.Header.Set("Content-Type", "application/json")
	setup.Header.Set("Origin", local.URL)
	setupResponse, err := http.DefaultClient.Do(setup)
	if err != nil {
		t.Fatal(err)
	}
	if cookies := setupResponse.Cookies(); len(cookies) != 1 || cookies[0].Secure {
		t.Fatalf("local bootstrap cookie = %#v", cookies)
	}
	assertStatus(setupResponse, http.StatusCreated)

	login := do(http.MethodPost, "/api/v1/access/login", body, publicTestOrigin, "")
	cookies := login.Cookies()
	if len(cookies) != 1 || !cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("HTTPS login cookies = %#v", cookies)
	}
	var session struct {
		CSRF string `json:"csrfToken"`
	}
	if err := json.Unmarshal(assertStatus(login, http.StatusOK), &session); err != nil || session.CSRF == "" {
		t.Fatalf("session = %#v, %v", session, err)
	}
	assertStatus(do(http.MethodPost, "/api/v1/control/refresh", `{}`, publicTestOrigin, session.CSRF), http.StatusAccepted)
	assertStatus(do(http.MethodPost, "/api/v1/control/refresh", `{}`, publicTestOrigin, "bad-csrf"), http.StatusForbidden)
	for _, origin := range []string{"http://" + publicTestHost, "https://evil.example", publicTestOrigin + "/", ""} {
		assertStatus(do(http.MethodPost, "/api/v1/control/refresh", `{}`, origin, session.CSRF), http.StatusForbidden)
	}

	status = assertStatus(do(http.MethodGet, "/api/v1/status", "", "", ""), http.StatusOK)
	if !strings.Contains(string(status), `"socksAddress":"socks.example.com:10808"`) {
		t.Fatalf("public SOCKS advertisement = %s", status)
	}
	subscription := assertStatus(do(http.MethodGet, "/api/v1/subscription/url", "", "", ""), http.StatusOK)
	if !strings.Contains(string(subscription), publicTestOrigin+"/sub/"+subscriptions.binding) {
		t.Fatalf("public subscription URL = %s", subscription)
	}
	assertStatus(do(http.MethodGet, "/sub/"+subscriptions.binding, "", "", ""), http.StatusOK)

	// An authenticated browser event stream accepts the HTTPS origin.
	stream := do(http.MethodGet, "/api/v1/events", "", publicTestOrigin, "")
	if stream.StatusCode != http.StatusOK {
		t.Fatalf("HTTPS SSE = %d", stream.StatusCode)
	}
	stream.Body.Close()
	locked := do(http.MethodPost, "/api/v1/access/logout", `{}`, publicTestOrigin, session.CSRF)
	if cookies := locked.Cookies(); len(cookies) != 1 || !cookies[0].Secure || cookies[0].MaxAge != -1 {
		t.Fatalf("HTTPS logout cookie = %#v", cookies)
	}
	assertStatus(locked, http.StatusNoContent)
}

func TestPublicOriginDoesNotTrustForwardedHeaders(t *testing.T) {
	config := testConfig()
	config.PublicOrigin = publicTestOrigin
	api, err := NewAPI(config, Dependencies{Backend: &fakeBackend{}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		host, origin string
		want         int
	}{
		{"evil.example:8443", publicTestOrigin, http.StatusBadRequest},
		{"console.example.com:9443", publicTestOrigin, http.StatusBadRequest},
		{"console.example.com", publicTestOrigin, http.StatusBadRequest},
		{"console.example.com.evil.example:8443", publicTestOrigin, http.StatusBadRequest},
		{testHost, publicTestOrigin, http.StatusForbidden},
		{publicTestHost, testOrigin, http.StatusForbidden},
	} {
		t.Run(tc.host+tc.origin, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://"+tc.host+"/api/v1/access/login", strings.NewReader(`{"token":"`+validTestToken+`"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Origin", tc.origin)
			req.Header.Set("Forwarded", `for=127.0.0.1;host="`+publicTestHost+`";proto=https`)
			req.Header.Set("X-Forwarded-Host", publicTestHost)
			req.Header.Set("X-Forwarded-Proto", "https")
			req.Header.Set("X-Forwarded-For", "127.0.0.1")
			response := httptest.NewRecorder()
			api.ServeHTTP(response, req)
			if response.Code != tc.want {
				t.Fatalf("forged forwarded headers = %d %s, want %d", response.Code, response.Body.String(), tc.want)
			}
		})
	}
	for _, host := range []string{testHost, publicTestHost} {
		if got := api.baseURL(host); got != publicTestOrigin {
			t.Fatalf("baseURL(%s) = %s", host, got)
		}
		req := httptest.NewRequest(http.MethodGet, "http://"+host+"/", nil)
		if got := api.socksAddress(req); got != "console.example.com:10808" {
			t.Fatalf("SOCKS address = %s", got)
		}
	}
}

func TestHTTPSOriginSSEWithoutOriginAndMalformedConfig(t *testing.T) {
	for _, origin := range []string{"http://console.example.com", "https://console.example.com/", "https://127.0.0.1:10809", "https://localhost:10809"} {
		if _, err := NewAPI(Config{PublicOrigin: origin}, Dependencies{}); err == nil {
			t.Fatalf("accepted invalid public origin %q", origin)
		}
	}
	for _, tc := range []struct {
		origin, site string
		allowed      bool
	}{
		{publicTestOrigin, "same-origin", true}, {"", "same-origin", true},
		{"https://evil.example", "same-origin", false}, {"", "cross-site", false},
	} {
		r := httptest.NewRequest(http.MethodGet, publicTestOrigin+"/api/v1/events", nil)
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("Sec-Fetch-Site", tc.site)
		if got := eventStreamOriginAllowed(r, publicTestOrigin); got != tc.allowed {
			t.Fatalf("SSE origin %q site %q = %v", tc.origin, tc.site, got)
		}
	}
}
