package config

import (
	"strings"
	"testing"
)

func TestProviderQuickFoxUDPValidation(t *testing.T) {
	cfg, err := Load(writeConfig(t, validConfig))
	if err != nil || cfg.Provider.QuickFoxUDP != "" {
		t.Fatalf("default QuickFox UDP mode = %q, %v", cfg.Provider.QuickFoxUDP, err)
	}
	for _, mode := range []string{QuickFoxUDPOverTCP, QuickFoxUDPNative} {
		t.Run(mode, func(t *testing.T) {
			body := strings.Replace(validConfig, "provider:\n", "provider:\n  quickfoxUDP: "+mode+"\n", 1)
			cfg, err := Load(writeConfig(t, body))
			if err != nil || cfg.Provider.QuickFoxUDP != mode {
				t.Fatalf("load QuickFox UDP mode = %q, %v", cfg.Provider.QuickFoxUDP, err)
			}
		})
	}
	for _, mode := range []string{"Native", "tcp", "udp", " native"} {
		t.Run(mode, func(t *testing.T) {
			cfg := Default()
			cfg.Provider.QuickFoxUDP = mode
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "provider.quickfoxUDP") {
				t.Fatalf("validate QuickFox UDP mode %q = %v", mode, err)
			}
		})
	}
}
