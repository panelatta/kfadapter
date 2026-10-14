package config

import (
	"strings"
	"testing"
)

func TestManagementPublicOriginValidation(t *testing.T) {
	for _, origin := range []string{"https://adapter.example.com", "https://adapter.example.com:8443", "https://192.0.2.10", "https://[2001:db8::1]:8443"} {
		t.Run(origin, func(t *testing.T) {
			body := strings.Replace(validConfig, "management:\n", "management:\n  publicOrigin: \""+origin+"\"\n", 1)
			cfg, err := Load(writeConfig(t, body))
			if err != nil || cfg.Management.PublicOrigin != origin {
				t.Fatalf("load public origin = %q, %v", cfg.Management.PublicOrigin, err)
			}
		})
	}
	for _, origin := range []string{
		"http://adapter.example.com", "https://Adapter.example.com", "https://adapter.example.com/",
		"https://adapter.example.com/path", "https://user@adapter.example.com", "https://adapter.example.com?",
		"https://adapter.example.com#", "https://adapter.example.com?q=x", "https://adapter.example.com#x",
		"https://adapter.example.com:", "https://adapter.example.com:443", "https://adapter.example.com:08443",
		"https://adapter.example.com:0", "https://adapter.example.com:65536", "https://adapter.example.com.",
		"https://127.0.0.1:10809", "https://[::1]", "https://localhost", "https://console.localhost",
		"https://0.0.0.0", "https://[::]", "https://[fe80::1%25en0]", "https://[2001:DB8::1]",
		"https://adapter.example.com\\evil", "https://adapter.example.com,evil.example", " https://adapter.example.com",
	} {
		t.Run(origin, func(t *testing.T) {
			cfg := Default()
			cfg.Management.PublicOrigin = origin
			if err := cfg.Validate(); err == nil {
				t.Fatalf("accepted invalid public origin %q", origin)
			}
		})
	}
}
