package app

import (
	"bytes"
	"testing"
	"time"

	"github.com/kfadapter/kfadapter/internal/selector"
	"github.com/kfadapter/kfadapter/internal/state"
)

func TestSelectorCoordinatorInstallsLiveOnlyMap(t *testing.T) {
	registry, err := selector.NewRegistry(state.SubscriptionAuthority{SelectorKey: bytes.Repeat([]byte{1}, 32), ProxyAuthKey: bytes.Repeat([]byte{2}, 32), ActivatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := NewSelectorCoordinator(nil, registry, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	nodes := []state.Node{{ID: "kuaifan_IZL4HC1WCgH5U2Ve"}}
	refs, err := coordinator.Build(nodes)
	if err != nil || len(refs) != 1 {
		t.Fatalf("refs = %#v, %v", refs, err)
	}
}
