package kuaifan

import (
	"testing"

	"github.com/kfadapter/kfadapter/internal/selector"
)

func TestNodeIDCanonicalPayloadAndLabelIndependence(t *testing.T) {
	identity, err := selector.Canonicalize(selector.NodeIdentity{Provider: "wifiin", Host: "Node.Example.", Port: 11000})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := nodeID(identity, "group"), "kuaifan_IZL4HC1WCgH5U2Ve"; got != want {
		t.Fatalf("node ID = %q, want %q", got, want)
	}
	otherGroup := nodeID(identity, "other")
	if otherGroup == nodeID(identity, "group") {
		t.Fatal("group must affect node identity")
	}
}
