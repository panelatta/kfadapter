package subscription

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestServeFiltersAcceptedProviderGroups(t *testing.T) {
	for _, test := range []struct {
		name  string
		group string
	}{
		{"quickfox_composite_603_bytes", strings.Repeat("g", 300) + " / " + strings.Repeat("r", 300)},
		{"quickfox_composite_1027_bytes", strings.Repeat("g", 512) + " / " + strings.Repeat("r", 512)},
		{"provider_limit_4096_bytes", strings.Repeat("g", 4096)},
		{"provider_limit_utf8_4096_bytes", strings.Repeat("组", 1365) + "g"},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, persistent := catalogService(t)
			snapshot := persistent.ActiveSession.Clone()
			snapshot.Generation++
			snapshot.Nodes[0].Group = test.group
			kuaifan := snapshot.Providers["kuaifan"]
			kuaifan.Nodes[0].Group = test.group
			if !kuaifan.Valid() {
				t.Fatal("group fixture exceeds the provider contract")
			}
			snapshot.Providers["kuaifan"] = kuaifan
			persistent.ActiveSession = snapshot
			if err := service.Update(persistent); err != nil {
				t.Fatal(err)
			}
			subscriptionURL, err := service.SubscriptionURL("http://127.0.0.1:10809")
			if err != nil {
				t.Fatal(err)
			}
			for _, filter := range []string{"group=" + url.QueryEscape(test.group), "provider=kuaifan&group=" + url.QueryEscape(test.group)} {
				response := serveSubscription(t, service, subscriptionURL+"?"+filter)
				if response.Code != http.StatusOK || response.Body.Len() == 0 {
					t.Fatalf("filter %.40q = %d", filter, response.Code)
				}
			}
		})
	}
}

func TestSubscriptionFilterRetainsInputLimits(t *testing.T) {
	service, _ := catalogService(t)
	subscriptionURL, err := service.SubscriptionURL("http://127.0.0.1:10809")
	if err != nil {
		t.Fatal(err)
	}
	for _, filter := range []string{
		"group=" + strings.Repeat("g", 4097),
		"name=" + strings.Repeat("n", 513),
		"provider=" + strings.Repeat("p", 33),
	} {
		if response := serveSubscription(t, service, subscriptionURL+"?"+filter); response.Code != http.StatusNotFound {
			t.Fatalf("oversized filter %.20q = %d", filter, response.Code)
		}
	}
}

func serveSubscription(t *testing.T, service *Service, rawURL string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, rawURL, nil)
	response := httptest.NewRecorder()
	service.HandleSubscription(response, request, "127.0.0.1:10808")
	return response
}
