package endpoint

import (
	"strings"
	"testing"
)

func TestValidateHostname(t *testing.T) {
	for _, valid := range []string{"adapter", "adapter.example.com", "Router-1.lan", strings.Repeat("a", 63) + ".example"} {
		if err := ValidateHostname(valid); err != nil {
			t.Errorf("%q rejected: %v", valid, err)
		}
	}
	for _, invalid := range []string{
		"", "adapter.example.com.", "http://adapter", "adapter:10809", "-adapter.example", "adapter-.example",
		"a..b", strings.Repeat("a", 64) + ".example", strings.Repeat("a.", 127) + "a", "192.168.1.1", "例子.example", "under_score.example",
	} {
		if err := ValidateHostname(invalid); err == nil {
			t.Errorf("%q accepted", invalid)
		}
	}
}
