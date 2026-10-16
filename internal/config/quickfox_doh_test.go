package config

import (
	"strings"
	"testing"
)

func TestProviderQuickFoxDoHValidation(t *testing.T) {
	cfg, err := Load(writeConfig(t, validConfig))
	if err != nil || cfg.Provider.QuickFoxDoH != "" {
		t.Fatalf("default QuickFox DoH = %q, %v", cfg.Provider.QuickFoxDoH, err)
	}
	for _, endpoint := range []string{"https://223.5.5.5/dns-query", "https://dns.alidns.com/dns-query", "https://dns.example:8443/resolve"} {
		t.Run(endpoint, func(t *testing.T) {
			body := strings.Replace(validConfig, "provider:\n", "provider:\n  quickfoxDoH: \""+endpoint+"\"\n", 1)
			cfg, err := Load(writeConfig(t, body))
			if err != nil || cfg.Provider.QuickFoxDoH != endpoint {
				t.Fatalf("load QuickFox DoH = %q, %v", cfg.Provider.QuickFoxDoH, err)
			}
		})
	}
	for _, endpoint := range []string{
		"dns.example", "http://dns.example/dns-query", "https:///dns-query", "https://user@dns.example/dns-query",
		"https://dns.example/dns-query?dns=x", "https://dns.example/dns-query?", "https://dns.example/dns-query#", "https:dns.example",
	} {
		t.Run(endpoint, func(t *testing.T) {
			cfg := Default()
			cfg.Provider.QuickFoxDoH = endpoint
			if err := cfg.Validate(); err == nil {
				t.Fatalf("accepted invalid QuickFox DoH %q", endpoint)
			}
		})
	}
}
