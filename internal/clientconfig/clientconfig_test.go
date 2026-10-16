package clientconfig

import (
	"bytes"
	"net/url"
	"strings"
	"testing"

	"github.com/oshinop/chinadapter/internal/credentials"
	"github.com/oshinop/chinadapter/internal/provider"
	"github.com/oshinop/chinadapter/internal/state"
)

func testIdentity(t *testing.T) *credentials.Identity {
	t.Helper()
	identity, err := credentials.New(bytes.Repeat([]byte{7}, 32), "00112233445566778899aabbccddeeff")
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

var testRoute = provider.Route{Provider: "kuaifan", Protocol: "kuaifan-wifiin", Host: "node.example", Port: 11000}

var testPorts = Ports{SOCKS5: 10808, Snell: 10810, Shadowsocks: 10811, SOCKS5TLS: 10812}

func TestSurgeLines(t *testing.T) {
	identity := testIdentity(t)
	socks, _ := identity.Credentials(testRoute)
	psk, _ := identity.SnellPSK(testRoute)
	serverKey, _ := identity.ShadowsocksServerKey()
	userKey, _ := identity.ShadowsocksUserKey(testRoute)
	fingerprint, _ := identity.TLSFingerprint()
	common := ", block-quic=on, test-url=" + TestURL
	for _, test := range []struct {
		protocol Protocol
		want     string
	}{
		{SOCKS5, "上海 01 = socks5, adapter.example.com, 10808, " + socks.Username + ", " + socks.Password + common},
		{Snell, "上海 01 = snell, adapter.example.com, 10810, psk=" + psk + ", version=6, mode=unshaped, reuse=true" + common},
		{Shadowsocks, "上海 01 = ss, adapter.example.com, 10811, encrypt-method=2022-blake3-aes-128-gcm, password=" + credentials.ShadowsocksPassword(serverKey, userKey) + common},
		{SOCKS5TLS, "上海 01 = socks5-tls, adapter.example.com, 10812, " + socks.Username + ", " + socks.Password + ", server-cert-fingerprint-sha256=" + fingerprint + common},
	} {
		profile, ok := RouteProfile(identity, testRoute, test.protocol, "上海 01", "adapter.example.com", testPorts)
		if !ok {
			t.Fatalf("%s: no profile", test.protocol)
		}
		if got := Surge(profile); got != test.want {
			t.Errorf("%s:\n got %s\nwant %s", test.protocol, got, test.want)
		}
	}
}

func TestSmartProfilesUseSmartCredentials(t *testing.T) {
	identity := testIdentity(t)
	smart, _ := identity.SmartCredentials()
	smartPSK, _ := identity.SmartSnellPSK()
	for _, protocol := range Protocols {
		profile, ok := SmartProfile(identity, protocol, "智能线路", "1.2.3.4", testPorts)
		if !ok {
			t.Fatalf("%s: no smart profile", protocol)
		}
		line := Surge(profile)
		switch protocol {
		case SOCKS5, SOCKS5TLS:
			if !strings.Contains(line, smart.Username) {
				t.Errorf("%s: %s", protocol, line)
			}
		case Snell:
			if !strings.Contains(line, "psk="+smartPSK) {
				t.Errorf("%s: %s", protocol, line)
			}
		case Shadowsocks:
			if !strings.Contains(line, "encrypt-method=2022-blake3-aes-128-gcm") {
				t.Errorf("%s: %s", protocol, line)
			}
		}
	}
	uninitialized, _ := credentials.New(nil, "00112233445566778899aabbccddeeff")
	for _, protocol := range Protocols {
		if _, ok := SmartProfile(uninitialized, protocol, "x", "1.2.3.4", testPorts); ok {
			t.Fatalf("%s: uninitialized identity rendered a profile", protocol)
		}
	}
}

func TestPolicyNameReservesSeparators(t *testing.T) {
	for name, want := range map[string]string{
		"a=b,c":     "a＝b，c",
		" 上海\n01 ":  "上海 01",
		"\t":        "chinadapter",
		"香港 | IPLC": "香港 | IPLC",
	} {
		if got := PolicyName(name); got != want {
			t.Errorf("PolicyName(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestURIs(t *testing.T) {
	identity := testIdentity(t)
	profile, _ := RouteProfile(identity, testRoute, Shadowsocks, "节点 01", "2001:db8::1", testPorts)
	uri, ok := URI(profile)
	if !ok || !strings.HasPrefix(uri, "ss://2022-blake3-aes-128-gcm:") || strings.ContainsAny(strings.TrimPrefix(uri, "ss://"), "+=/") {
		t.Fatalf("ss URI = %q", uri)
	}
	parsed, err := url.Parse(uri)
	if err != nil {
		t.Fatal(err)
	}
	password, _ := parsed.User.Password()
	if parsed.User.Username() != "2022-blake3-aes-128-gcm" || password != profile.ShadowsocksPassword || parsed.Host != "[2001:db8::1]:10811" || parsed.Fragment != "节点 01" {
		t.Fatalf("parsed ss URI = %+v", parsed)
	}
	socks, _ := RouteProfile(identity, testRoute, SOCKS5, "节点", "1.2.3.4", testPorts)
	if uri, ok := URI(socks); !ok || !strings.HasPrefix(uri, "socks5://n_") {
		t.Fatalf("socks URI = %q", uri)
	}
	snell, _ := RouteProfile(identity, testRoute, Snell, "节点", "1.2.3.4", testPorts)
	if _, ok := URI(snell); ok {
		t.Fatal("Snell rendered a URI")
	}
}

func TestEnabledAndParse(t *testing.T) {
	preferences := state.InboundPreferences{Snell: true}
	if !Enabled(SOCKS5, preferences) || !Enabled(Snell, preferences) || Enabled(Shadowsocks, preferences) || Enabled(SOCKS5TLS, preferences) {
		t.Fatal("Enabled ignores preferences")
	}
	if protocol, ok := ParseProtocol("socks5-tls"); !ok || protocol != SOCKS5TLS {
		t.Fatal("ParseProtocol rejected socks5-tls")
	}
	if _, ok := ParseProtocol("ss"); ok {
		t.Fatal("ParseProtocol accepted an alias")
	}
}
