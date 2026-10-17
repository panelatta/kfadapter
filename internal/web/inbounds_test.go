package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
)

type inboundProblem struct{}

func (inboundProblem) Error() string   { return "inbound listener could not bind its port" }
func (inboundProblem) Code() string    { return "inbound_listen_failed" }
func (inboundProblem) HTTPStatus() int { return http.StatusConflict }

type fakeInbounds struct {
	mu        sync.Mutex
	choices   InboundChoices
	configErr error
	hosts     []string
}

func (f *fakeInbounds) Status() InboundsStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return InboundsStatus{TLSFingerprint: "ab", Inbounds: []InboundStatus{
		{Protocol: "socks5", Port: 10808, Enabled: true, Listening: true},
		{Protocol: "snell", Port: 10810, Enabled: f.choices.Snell, Listening: f.choices.Snell},
	}}
}

func (f *fakeInbounds) Configure(_ context.Context, choices InboundChoices) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.configErr != nil {
		return f.configErr
	}
	f.choices = choices
	return nil
}

func (f *fakeInbounds) NodeProfiles(nodeID, host string) ([]ProxyProfile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hosts = append(f.hosts, host)
	return []ProxyProfile{{Protocol: "snell", Port: 10810, Surge: nodeID + " = snell, " + host + ", 10810, psk=x"}}, nil
}

func (f *fakeInbounds) SmartProfiles(host string) ([]ProxyProfile, error) {
	return []ProxyProfile{{Protocol: "snell", Port: 10810, Surge: "智能线路 = snell, " + host + ", 10810, psk=y"}}, nil
}

func TestInboundsAPIRequiresSessionAndConfiguresChoices(t *testing.T) {
	inbounds := &fakeInbounds{}
	api, err := NewAPI(testConfig(), Dependencies{Backend: &fakeBackend{}, Subscriptions: newFakeSubscriptions(), Inbounds: inbounds})
	if err != nil {
		t.Fatal(err)
	}
	if response := request(api, http.MethodGet, "/api/v1/inbounds", "", nil, ""); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", response.Code)
	}
	cookie := establish(t, api)
	if response := rawRequest(api, http.MethodPut, "/api/v1/inbounds/config", `{"snell":true,"shadowsocks":false,"socksTLS":false}`, cookie, "http://evil.example", "cross-site"); response.Code != http.StatusForbidden {
		t.Fatalf("cross-site configure = %d", response.Code)
	}
	if response := request(api, http.MethodPut, "/api/v1/inbounds/config", `{"snell":true}`, cookie, testOrigin); response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid_inbounds") {
		t.Fatalf("partial configure = %d %s", response.Code, response.Body.String())
	}
	response := request(api, http.MethodPut, "/api/v1/inbounds/config", `{"snell":true,"shadowsocks":false,"socksTLS":true}`, cookie, testOrigin)
	if response.Code != http.StatusOK {
		t.Fatalf("configure = %d %s", response.Code, response.Body.String())
	}
	var status InboundsStatus
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil || len(status.Inbounds) != 2 || !status.Inbounds[1].Enabled {
		t.Fatalf("configure status = %+v, %v", status, err)
	}
	if inbounds.choices != (InboundChoices{Snell: true, SocksTLS: true}) {
		t.Fatalf("choices = %+v", inbounds.choices)
	}
	inbounds.configErr = inboundProblem{}
	if response := request(api, http.MethodPut, "/api/v1/inbounds/config", `{"snell":false,"shadowsocks":true,"socksTLS":false}`, cookie, testOrigin); response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "inbound_listen_failed") {
		t.Fatalf("bind failure = %d %s", response.Code, response.Body.String())
	}
	if response := request(api, http.MethodGet, "/api/v1/inbounds", "", cookie, ""); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"tlsFingerprint":"ab"`) {
		t.Fatalf("status = %d %s", response.Code, response.Body.String())
	}
}

func TestDetailsCarryInboundProfilesForAdvertisedHost(t *testing.T) {
	inbounds := &fakeInbounds{}
	backend := &fakeBackend{nodeDetails: NodeDetails{ID: "node-alpha", Name: "Alpha", SocksUsername: "u", SocksPassword: "p"}}
	smart := &fakeSmartProxy{enabled: true, interval: 30}
	api, err := NewAPI(testConfig(), Dependencies{Backend: backend, Subscriptions: newFakeSubscriptions(), Inbounds: inbounds, Smart: smart})
	if err != nil {
		t.Fatal(err)
	}
	cookie := establish(t, api)
	response := request(api, http.MethodGet, "/api/v1/nodes/node-alpha/details", "", cookie, "")
	var details NodeDetails
	if err := json.Unmarshal(response.Body.Bytes(), &details); err != nil || len(details.Profiles) != 1 || details.Profiles[0].Surge != "node-alpha = snell, 127.0.0.1, 10810, psk=x" {
		t.Fatalf("node details = %d %s", response.Code, response.Body.String())
	}
	smartResponse := request(api, http.MethodGet, "/api/v1/smart-proxy/details", "", cookie, "")
	if smartResponse.Code != http.StatusOK || !strings.Contains(smartResponse.Body.String(), "智能线路 = snell, 127.0.0.1, 10810") {
		t.Fatalf("smart details = %d %s", smartResponse.Code, smartResponse.Body.String())
	}
}
