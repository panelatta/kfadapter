package quickfox

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/kfadapter/kfadapter/internal/provider"
	"github.com/kfadapter/kfadapter/internal/selector"
	"github.com/kfadapter/kfadapter/internal/state"
	"github.com/kfadapter/kfadapter/internal/subscription"
)

func TestQuickFoxSnapshotSurvivesSQLiteRestart(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := state.NewSQLiteStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	persistent, err := store.LoadOrCreate()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	providerSnapshot := provider.Snapshot{
		Provider:  ProviderID,
		Account:   provider.Account{UserID: "15904241", Display: "a•••@example.com", Tier: vipTier, SubscriptionActive: true, SubscriptionEndsAt: now.Add(24 * time.Hour)},
		ExpiresAt: now.Add(time.Hour), RefreshState: []byte(`{"token":"0123456789abcdef0123456789abcdef","userId":15904241,"deviceCode":"00112233-4455-4677-8899-aabbccddeeff"}`),
		Authorities: map[string]provider.Authority{authorityID: {Protocol: NativeProtocol, Data: []byte(`{"token":"0123456789abcdef0123456789abcdef","userId":15904241,"appId":0}`)}},
		Nodes: []provider.Node{{
			ID: "qf_persisted", AuthorityID: authorityID, Protocol: NativeProtocol, Host: "34.160.111.145", Port: 80,
			Name: "西雅图专线", Group: "国内模式 / 美国节点", Model: "国内模式", Eligible: true,
		}},
	}
	if err := provider.ValidateSnapshot(providerSnapshot, ProviderID, now); err != nil {
		t.Fatal(err)
	}
	bindingSnapshot := &state.RuntimeSnapshot{Providers: map[provider.ID]provider.Snapshot{ProviderID: providerSnapshot}}
	bindingID := bindingSnapshot.AccountBindingID()
	persistent, err = store.Update(func(candidate *state.PersistentState) error {
		if err := candidate.SetAccessToken("correct horse battery token"); err != nil {
			return err
		}
		_, err := state.EnsureSubscriptionAccountBinding(candidate, bindingID, now)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := subscription.NewService(subscription.ServiceConfig{Store: store, SocksAddress: "127.0.0.1:10808", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := service.PrepareRuntimeCommit(context.Background(), bindingID)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := selector.NewRegistry(plan.Authority)
	if err != nil {
		t.Fatal(err)
	}
	built, err := registry.Build([]state.Node{{
		ID: "qf_persisted", Provider: ProviderID, Protocol: NativeProtocol, AuthorityID: authorityID,
		Host: "34.160.111.145", Port: 80, Name: "西雅图专线", Group: "国内模式 / 美国节点", Model: "国内模式", Eligible: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &state.RuntimeSnapshot{
		Generation: 1, CreatedAt: now, ExpiresAt: providerSnapshot.ExpiresAt,
		Providers: map[provider.ID]provider.Snapshot{ProviderID: providerSnapshot}, Nodes: built.Nodes, Selectors: built.Selectors,
	}
	if _, err := service.CommitRuntimeSnapshot(context.Background(), plan, snapshot); err != nil {
		t.Fatal(err)
	}
	if url, err := service.SubscriptionURL("http://console.example"); err != nil || url == "" {
		t.Fatalf("published SubscriptionURL = %q, %v", url, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := state.NewSQLiteStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restoredService, err := subscription.NewService(subscription.ServiceConfig{Store: reopened, SocksAddress: "127.0.0.1:10808", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if url, err := restoredService.SubscriptionURL("http://console.example"); err != nil || url == "" {
		t.Fatalf("restored SubscriptionURL = %q, %v", url, err)
	}
	loaded, err := reopened.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ActiveSession == nil || !reflect.DeepEqual(loaded.ActiveSession.Providers[ProviderID], providerSnapshot) || loaded.ActiveSession.Nodes[0].Protocol != NativeProtocol {
		t.Fatalf("QuickFox snapshot did not round trip: %#v", loaded.ActiveSession)
	}
	if !loaded.MatchesAccount(bindingID) || !reflect.DeepEqual(loaded.Subscription.SelectorKey, persistent.Subscription.SelectorKey) || !reflect.DeepEqual(loaded.Subscription.ProxyAuthKey, persistent.Subscription.ProxyAuthKey) {
		t.Fatalf("QuickFox account binding or keys did not round trip: %#v", loaded.Subscription)
	}
}
