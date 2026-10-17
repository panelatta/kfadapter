package subscription

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/oshinop/chinadapter/internal/clientconfig"
	"github.com/oshinop/chinadapter/internal/credentials"
	"github.com/oshinop/chinadapter/internal/state"
)

func serveProfiles(t *testing.T, service *Service, query string) *httptest.ResponseRecorder {
	t.Helper()
	subscriptionURL, err := service.SubscriptionURL("http://127.0.0.1:10809")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(subscriptionURL)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "http://adapter.example.com:10809"+parsed.Path+"?"+query, nil)
	response := httptest.NewRecorder()
	service.HandleSubscription(response, request, "adapter.example.com:10808")
	return response
}

func TestSurgeSubscriptionPerProtocol(t *testing.T) {
	service, persistent := catalogService(t)
	inbounds := state.InboundPreferences{Snell: true, Shadowsocks: true}
	smart := false
	service.ports = clientconfig.Ports{Snell: 10810, Shadowsocks: 10811, SOCKS5TLS: 10812}
	service.inbounds = func() state.InboundPreferences { return inbounds }
	service.smartEnabled = func() bool { return smart }
	identity, err := persistent.Identity()
	if err != nil {
		t.Fatal(err)
	}
	route := persistent.ActiveSession.Nodes[0].Route()
	psk, _ := identity.SnellPSK(route)

	response := serveProfiles(t, service, "format=surge&protocol=snell")
	want := "Node 01 # / 测试 = snell, adapter.example.com, 10810, psk=" + psk + ", version=6, mode=unshaped, reuse=true, block-quic=on, test-url=" + clientconfig.TestURL + "\n"
	if response.Code != http.StatusOK || response.Body.String() != want {
		t.Fatalf("snell list = %d %q", response.Code, response.Body.String())
	}
	socks := serveProfiles(t, service, "format=surge")
	if socks.Code != http.StatusOK || !strings.Contains(socks.Body.String(), "= socks5, adapter.example.com, 10808, n_") {
		t.Fatalf("default Surge list = %d %q", socks.Code, socks.Body.String())
	}

	// Disabled protocols, unknown values and Snell URIs are the empty 404.
	for _, query := range []string{"format=surge&protocol=socks5-tls", "format=clash", "protocol=ss", "protocol=snell", "format=surge&format=surge"} {
		if response := serveProfiles(t, service, query); response.Code != http.StatusNotFound || response.Body.Len() != 0 {
			t.Errorf("%s = %d %q", query, response.Code, response.Body.String())
		}
	}

	shadowsocks := serveProfiles(t, service, "protocol=shadowsocks")
	decoded, err := base64.StdEncoding.DecodeString(shadowsocks.Body.String())
	if shadowsocks.Code != http.StatusOK || err != nil || !strings.HasPrefix(string(decoded), "ss://2022-blake3-aes-128-gcm:") {
		t.Fatalf("ss list = %d %q %v", shadowsocks.Code, decoded, err)
	}

	smart = true
	lines := strings.Split(strings.TrimSuffix(serveProfiles(t, service, "format=surge&protocol=snell").Body.String(), "\n"), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], SmartRouteName+" = snell, ") {
		t.Fatalf("smart Surge list = %q", lines)
	}
	if filtered := serveProfiles(t, service, "format=surge&protocol=snell&provider=kuaifan").Body.String(); strings.Contains(filtered, SmartRouteName) {
		t.Fatal("row filters kept the automatic route")
	}
}

func TestSurgeSubscriptionDeduplicatesPolicyNames(t *testing.T) {
	_, persistent := catalogService(t)
	snapshot := persistent.ActiveSession.Clone()
	twin := snapshot.Nodes[0]
	twin.ID, twin.Port, twin.Name = "kuaifan_twin", twin.Port+1, "Node 01 # / 测试"
	snapshot.Nodes = append(snapshot.Nodes, twin)
	body, count, err := RenderProfiles(snapshot.Nodes, Filter{Format: FormatSurge}, Options{Host: "1.2.3.4", Ports: clientconfig.Ports{SOCKS5: 10808}}, mustIdentity(t, persistent))
	if err != nil || count != 2 {
		t.Fatalf("RenderProfiles = %d, %v", count, err)
	}
	lines := strings.Split(strings.TrimSpace(body), "\n")
	if !strings.HasPrefix(lines[0], "Node 01 # / 测试 = ") || !strings.HasPrefix(lines[1], "Node 01 # / 测试 2 = ") {
		t.Fatalf("names = %q", lines)
	}
}

func mustIdentity(t *testing.T, persistent state.PersistentState) *credentials.Identity {
	t.Helper()
	identity, err := persistent.Identity()
	if err != nil {
		t.Fatal(err)
	}
	return identity
}
