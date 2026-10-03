package subscription

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kfadapter/kfadapter/internal/provider"
	"github.com/kfadapter/kfadapter/internal/selector"
	"github.com/kfadapter/kfadapter/internal/state"
)

func TestSubscriptionMetadataHasNoFetchProof(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := state.NewSQLiteStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.LoadOrCreate(); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(ServiceConfig{Store: store, SocksAddress: "127.0.0.1:10808"})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := service.Metadata()
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Active || metadata.NodeCount != 0 {
		t.Fatalf("unexpected inactive metadata: %#v", metadata)
	}
}

const testProtocol provider.Protocol = "test-native"

func testProviderSnapshot(id provider.ID, now time.Time, nodes ...provider.Node) provider.Snapshot {
	return provider.Snapshot{
		Provider:     id,
		Account:      provider.Account{UserID: string(id) + "-user", Display: "a•••@example.com", Tier: "standard"},
		ExpiresAt:    now.Add(20 * time.Hour),
		RefreshState: []byte("refresh"),
		Authorities:  map[string]provider.Authority{"default": {Protocol: testProtocol, Data: []byte("authority")}},
		Nodes:        nodes,
	}
}

// publishedService returns a service over a store holding one published
// two-provider subscription, and its subscription path token.
func publishedService(t *testing.T, listen string) (*Service, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := state.NewSQLiteStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.LoadOrCreate(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(func(candidate *state.PersistentState) error {
		return candidate.SetAccessToken("correct horse battery token")
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	providers := map[provider.ID]provider.Snapshot{
		"alpha": testProviderSnapshot("alpha", now,
			provider.Node{ID: "alpha_tokyo", AuthorityID: "default", Protocol: testProtocol, Host: "192.0.2.1", Port: 443, Name: "Tokyo", Group: "Asia", Eligible: true},
			provider.Node{ID: "alpha_paris", AuthorityID: "default", Protocol: testProtocol, Host: "192.0.2.2", Port: 443, Name: "Paris", Group: "Europe", Eligible: true}),
		"beta": testProviderSnapshot("beta", now,
			provider.Node{ID: "beta_osaka", AuthorityID: "default", Protocol: testProtocol, Host: "192.0.2.3", Port: 443, Name: "Osaka", Group: "Asia", Eligible: true}),
	}
	service, err := NewService(ServiceConfig{Store: store, SocksAddress: listen, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	bindingSnapshot := &state.RuntimeSnapshot{Providers: providers}
	plan, err := service.PrepareRuntimeCommit(context.Background(), bindingSnapshot.AccountBindingID())
	if err != nil {
		t.Fatal(err)
	}
	registry, err := selector.NewRegistry(plan.Authority)
	if err != nil {
		t.Fatal(err)
	}
	var nodes []state.Node
	for id, snapshot := range providers {
		for _, node := range snapshot.Nodes {
			nodes = append(nodes, state.Node{ID: node.ID, Provider: id, Protocol: node.Protocol, AuthorityID: node.AuthorityID, Host: node.Host, Port: node.Port, Name: node.Name, Group: node.Group, Eligible: true})
		}
	}
	built, err := registry.Build(nodes)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &state.RuntimeSnapshot{Generation: 1, CreatedAt: now, ExpiresAt: state.ProviderExpiresAt(providers), Providers: providers, Nodes: built.Nodes, Selectors: built.Selectors}
	if _, err := service.CommitRuntimeSnapshot(context.Background(), plan, snapshot); err != nil {
		t.Fatal(err)
	}
	subscriptionURL, err := service.SubscriptionURL("http://console.example")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(subscriptionURL)
	if err != nil {
		t.Fatal(err)
	}
	return service, strings.TrimPrefix(parsed.Path, "/sub/")
}

func fetch(t *testing.T, service *Service, binding, query, socksAddress string) (int, []string) {
	t.Helper()
	target := "http://console.example/sub/" + binding
	if query != "" {
		target += "?" + query
	}
	recorder := httptest.NewRecorder()
	service.ServeSubscriptionAt(recorder, httptest.NewRequest(http.MethodGet, target, nil), binding, socksAddress)
	if recorder.Code != http.StatusOK {
		return recorder.Code, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(recorder.Body.String())
	if err != nil {
		t.Fatalf("body is not base64: %v", err)
	}
	return recorder.Code, strings.Split(strings.TrimSuffix(string(decoded), "\n"), "\n")
}

func TestServeRendersAllEligibleNodes(t *testing.T) {
	service, binding := publishedService(t, "127.0.0.1:10808")
	code, links := fetch(t, service, binding, "", "")
	if code != http.StatusOK || len(links) != 3 {
		t.Fatalf("subscription = %d %v", code, links)
	}
	for _, link := range links {
		parsed, err := url.Parse(link)
		if err != nil || parsed.Scheme != "socks5" || parsed.Host != "127.0.0.1:10808" || parsed.User == nil {
			t.Fatalf("link = %q", link)
		}
	}
}

func TestServeFiltersByProviderGroupAndName(t *testing.T) {
	service, binding := publishedService(t, "127.0.0.1:10808")
	for query, want := range map[string]int{"provider=alpha": 2, "provider=beta": 1, "group=Asia": 2, "name=pari": 1, "provider=alpha&group=Asia": 1} {
		if code, links := fetch(t, service, binding, query, ""); code != http.StatusOK || len(links) != want {
			t.Fatalf("%s = %d %d links", query, code, len(links))
		}
	}
	if code, _ := fetch(t, service, binding, "name=nowhere", ""); code != http.StatusNotFound {
		t.Fatalf("empty filter result = %d", code)
	}
	if code, _ := fetch(t, service, binding, "unknown=1", ""); code != http.StatusNotFound {
		t.Fatalf("unknown filter key = %d", code)
	}
}

func TestServeRejectsWrongBinding(t *testing.T) {
	service, binding := publishedService(t, "127.0.0.1:10808")
	wrong := []byte(binding)
	if wrong[0] == 'A' {
		wrong[0] = 'B'
	} else {
		wrong[0] = 'A'
	}
	if code, _ := fetch(t, service, string(wrong), "", ""); code != http.StatusNotFound {
		t.Fatalf("wrong binding = %d", code)
	}
}

func TestServeRendersRequestAddressWithoutMutatingState(t *testing.T) {
	service, binding := publishedService(t, "0.0.0.0:10808")
	before, err := service.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for index, address := range []string{"192.0.2.10:10808", "198.51.100.20:10808", "192.0.2.10:10808", "198.51.100.20:10808"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, links := fetch(t, service, binding, "", address)
			if code != http.StatusOK {
				t.Errorf("request %d = %d", index, code)
				return
			}
			for _, link := range links {
				if parsed, err := url.Parse(link); err != nil || parsed.Host != address {
					t.Errorf("request %d for %s got link host %q", index, address, parsed.Host)
				}
			}
		}()
	}
	wg.Wait()
	after, err := service.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if before.LastGood.RenderedSubscription != after.LastGood.RenderedSubscription {
		t.Fatal("serving a subscription rewrote persisted state")
	}
}
