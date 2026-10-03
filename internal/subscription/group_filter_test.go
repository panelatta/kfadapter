package subscription

import (
	"context"
	"net/http"
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
			service, binding := publishedService(t, "127.0.0.1:10808")
			persistent, err := service.store.Load()
			if err != nil {
				t.Fatal(err)
			}
			snapshot := persistent.ActiveSession.Clone()
			snapshot.Generation++
			for index := range snapshot.Nodes {
				if snapshot.Nodes[index].ID == "alpha_tokyo" {
					snapshot.Nodes[index].Group = test.group
				}
			}
			alpha := snapshot.Providers["alpha"]
			alpha.Nodes[0].Group = test.group
			if !alpha.Valid() {
				t.Fatal("group fixture exceeds the provider contract")
			}
			snapshot.Providers["alpha"] = alpha
			plan, err := service.PrepareRuntimeCommit(context.Background(), snapshot.AccountBindingID())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.CommitRuntimeSnapshot(context.Background(), plan, snapshot); err != nil {
				t.Fatal(err)
			}
			if code, links := fetch(t, service, binding, "", ""); code != http.StatusOK || len(links) != 3 {
				t.Fatalf("unfiltered subscription = %d, %d links", code, len(links))
			}
			query := url.Values{"group": {test.group}}.Encode()
			code, links := fetch(t, service, binding, query, "")
			if code != http.StatusOK || len(links) != 1 {
				t.Fatalf("group filter with %d bytes = %d, %d links", len(test.group), code, len(links))
			}
			link, err := url.Parse(links[0])
			if err != nil || !strings.Contains(link.Fragment, "Tokyo") {
				t.Fatalf("filtered link = %q, error = %v", links[0], err)
			}
		})
	}
}

func TestSubscriptionFilterRetainsInputLimits(t *testing.T) {
	for _, test := range []struct {
		name   string
		values url.Values
	}{
		{"group_over_limit", url.Values{"group": {strings.Repeat("g", 4097)}}},
		{"group_utf8_over_limit", url.Values{"group": {strings.Repeat("组", 1365) + "gg"}}},
		{"name_over_limit", url.Values{"name": {strings.Repeat("n", 513)}}},
		{"group_empty", url.Values{"group": {""}}},
		{"group_duplicate", url.Values{"group": {"Asia", "Asia"}}},
		{"group_invalid_utf8", url.Values{"group": {string([]byte{0xff})}}},
		{"group_nul", url.Values{"group": {"Asia\x00"}}},
		{"group_leading_whitespace", url.Values{"group": {" Asia"}}},
		{"group_trailing_whitespace", url.Values{"group": {"Asia "}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, valid := parseSubscriptionFilter(test.values.Encode()); valid {
				t.Fatal("invalid filter was accepted")
			}
		})
	}
	if _, valid := parseSubscriptionFilter(url.Values{"name": {strings.Repeat("n", 512)}}.Encode()); !valid {
		t.Fatal("name filter at its existing limit was rejected")
	}
}
