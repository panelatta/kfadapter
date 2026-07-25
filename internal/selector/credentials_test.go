package selector

import (
	"bytes"
	"testing"
	"time"

	"github.com/kfadapter/kfadapter/internal/state"
)

func TestDeriveUsesCompleteNodeIDOnly(t *testing.T) {
	selectorKey := bytes.Repeat([]byte{1}, 32)
	proxyKey := bytes.Repeat([]byte{2}, 32)
	identity := NodeIdentity{NodeID: "kuaifan_IZL4HC1WCgH5U2Ve", Provider: "wifiin", Host: "irrelevant.example", Port: 1}
	credentials, err := Derive(identity, selectorKey, proxyKey)
	if err != nil {
		t.Fatal(err)
	}
	if credentials.Selector != "n_oiejDe4bdehSlvRr" || credentials.Password != "p_Nj1QpSyarUQVjKj-p0Sczc_t" {
		t.Fatalf("credentials = %#v", credentials)
	}
	other, err := Derive(NodeIdentity{NodeID: identity.NodeID, Provider: "quickfox", Host: "different.example", Port: 65535}, selectorKey, proxyKey)
	if err != nil || other != credentials {
		t.Fatalf("route affected NodeID-only derivation: %#v, %v", other, err)
	}
}

func TestExactIdentityReactivationIsDeterministic(t *testing.T) {
	registry, err := NewRegistry(state.SubscriptionAuthority{SelectorKey: bytes.Repeat([]byte{3}, 32), ProxyAuthKey: bytes.Repeat([]byte{4}, 32), ActivatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	node := state.Node{ID: "quickfox_oPBFuUUKYh007NL5"}
	first, err := registry.Build([]state.Node{node})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Build(nil); err != nil {
		t.Fatal(err)
	}
	second, err := registry.Build([]state.Node{node})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Selectors) != 1 || len(second.Selectors) != 1 {
		t.Fatalf("selectors = %#v %#v", first.Selectors, second.Selectors)
	}
	for name := range first.Selectors {
		if _, found := second.Selectors[name]; !found {
			t.Fatal("exact identity did not reactivate deterministic selector")
		}
	}
}
