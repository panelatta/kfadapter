package credentials

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/oshinop/chinadapter/internal/provider"
)

func TestIdentityDerivationAndStrictUUIDPath(t *testing.T) {
	hash := bytes.Repeat([]byte{7}, 32)
	installation := "00112233445566778899aabbccddeeff"
	identity, err := New(hash, installation)
	if err != nil {
		t.Fatal(err)
	}
	reference := func(key []byte, message string) []byte {
		m := hmac.New(sha256.New, key)
		_, _ = m.Write([]byte(message))
		return m.Sum(nil)
	}
	instance := reference(hash, "chinadapter/instance/v3\x00"+installation)
	route := provider.Route{Provider: "kuaifan", Protocol: "kuaifan-wifiin", Host: "Node.Example.", Port: 11000}
	key, err := provider.NodeKey(route)
	if err != nil {
		t.Fatal(err)
	}
	if key != "kuaifan\x00kuaifan-wifiin\x00node.example\x0011000" {
		t.Fatal("incorrect node key")
	}
	credentials, ok := identity.Credentials(route)
	if !ok {
		t.Fatal("missing credentials")
	}
	expected := reference(instance, "chinadapter/socks-user/v3\x00"+key)
	if credentials.Username != "n_"+base64.RawURLEncoding.EncodeToString(expected[:12]) {
		t.Fatal("incorrect username derivation")
	}
	if !ValidUUID(identity.SubscriptionID()) || !ValidUUID(credentials.Password) || identity.SubscriptionID() == credentials.Password {
		t.Fatal("UUID derivation or domain separation failed")
	}
	if id, ok := ParseSubscriptionPath("/sub/" + identity.SubscriptionID()); !ok || id != identity.SubscriptionID() {
		t.Fatal("canonical path rejected")
	}
	for _, path := range []string{identity.SubscriptionID(), "/sub/" + strings.ToUpper(identity.SubscriptionID()), "/sub/" + identity.SubscriptionID() + "/", "/sub/%30" + identity.SubscriptionID()[1:], "/sub/00000000-0000-4000-8000-000000000000", "/sub/00000000-0000-8000-0000-000000000000"} {
		if _, ok := ParseSubscriptionPath(path); ok {
			t.Fatalf("accepted noncanonical path %q", path)
		}
	}
	if !identity.AuthenticatePassword(credentials.Password) || identity.AuthenticatePassword("incorrect") {
		t.Fatal("password authentication failed")
	}
	fresh, err := New(bytes.Repeat([]byte{8}, 32), installation)
	if err != nil {
		t.Fatal(err)
	}
	changed, _ := fresh.Credentials(route)
	if changed == credentials || fresh.SubscriptionID() == identity.SubscriptionID() || fresh.Password() == identity.Password() {
		t.Fatal("token verifier change did not rotate credentials")
	}
	uninitialized, err := New(nil, installation)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := uninitialized.Credentials(route); ok || uninitialized.SubscriptionID() != "" || uninitialized.AuthenticatePassword(credentials.Password) {
		t.Fatal("uninitialized installation exposes authority")
	}
}

func TestSmartCredentialsUseSeparateNamespace(t *testing.T) {
	installation := "00112233445566778899aabbccddeeff"
	if smart, ok := (&Identity{}).SmartCredentials(); ok || smart != (Credentials{}) {
		t.Fatal("uninitialized identity issued smart credentials")
	}
	identity, err := New(bytes.Repeat([]byte{7}, 32), installation)
	if err != nil {
		t.Fatal(err)
	}
	smart, ok := identity.SmartCredentials()
	if !ok || !strings.HasPrefix(smart.Username, "a_") || smart.Password != identity.Password() {
		t.Fatal("smart credentials not derived from the instance identity")
	}
	node, _ := identity.Credentials(provider.Route{Provider: "kuaifan", Protocol: "kuaifan-wifiin", Host: "node.example", Port: 11000})
	if node.Username == smart.Username {
		t.Fatal("smart username collides with a node username")
	}
	again, _ := identity.SmartCredentials()
	if again != smart {
		t.Fatal("smart credentials are not stable")
	}
	rotated, err := New(bytes.Repeat([]byte{8}, 32), installation)
	if err != nil {
		t.Fatal(err)
	}
	if next, _ := rotated.SmartCredentials(); next.Username == smart.Username || next.Password == smart.Password {
		t.Fatal("smart credentials survived an access token change")
	}
}

func TestInboundSecretsAreStableAndDomainSeparated(t *testing.T) {
	installation := "00112233445566778899aabbccddeeff"
	identity, err := New(bytes.Repeat([]byte{7}, 32), installation)
	if err != nil {
		t.Fatal(err)
	}
	first := provider.Route{Provider: "kuaifan", Protocol: "kuaifan-wifiin", Host: "node.example", Port: 11000}
	second := provider.Route{Provider: "kuaifan", Protocol: "kuaifan-wifiin", Host: "node.example", Port: 11001}
	firstPSK, ok := identity.SnellPSK(first)
	secondPSK, _ := identity.SnellPSK(second)
	smartPSK, smartOK := identity.SmartSnellPSK()
	if !ok || !smartOK || len(firstPSK) != 24 || firstPSK == secondPSK || firstPSK == smartPSK || strings.ContainsAny(firstPSK, "+/=,") {
		t.Fatalf("Snell PSKs %q %q %q", firstPSK, secondPSK, smartPSK)
	}
	if again, _ := identity.SnellPSK(provider.Route{Provider: "kuaifan", Protocol: "kuaifan-wifiin", Host: "Node.Example.", Port: 11000}); again != firstPSK {
		t.Fatal("Snell PSK is not bound to the canonical route")
	}
	serverKey, _ := identity.ShadowsocksServerKey()
	firstUser, _ := identity.ShadowsocksUserKey(first)
	secondUser, _ := identity.ShadowsocksUserKey(second)
	smartUser, _ := identity.SmartShadowsocksUserKey()
	keys := [][]byte{serverKey, firstUser, secondUser, smartUser}
	for index, key := range keys {
		if len(key) != ShadowsocksKeySize {
			t.Fatalf("key %d has %d bytes", index, len(key))
		}
		for _, other := range keys[index+1:] {
			if bytes.Equal(key, other) {
				t.Fatal("Shadowsocks keys collide")
			}
		}
	}
	if password := ShadowsocksPassword(serverKey, firstUser); password != base64.StdEncoding.EncodeToString(serverKey)+":"+base64.StdEncoding.EncodeToString(firstUser) {
		t.Fatalf("password = %q", password)
	}
	uninitialized, _ := New(nil, installation)
	if _, ok := uninitialized.SnellPSK(first); ok {
		t.Fatal("uninitialized identity issued a Snell PSK")
	}
	if _, ok := uninitialized.ShadowsocksServerKey(); ok {
		t.Fatal("uninitialized identity issued a Shadowsocks key")
	}
	if _, _, err := uninitialized.TLSCertificate(); err == nil {
		t.Fatal("uninitialized identity issued a TLS certificate")
	}
}

func TestTLSCertificateIsDeterministicPerAccessToken(t *testing.T) {
	installation := "00112233445566778899aabbccddeeff"
	identity, _ := New(bytes.Repeat([]byte{7}, 32), installation)
	same, _ := New(bytes.Repeat([]byte{7}, 32), installation)
	rotated, _ := New(bytes.Repeat([]byte{8}, 32), installation)
	certificate, fingerprint, err := identity.TLSCertificate()
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(certificate.Certificate[0])
	if len(fingerprint) != 64 || fingerprint != hex.EncodeToString(digest[:]) {
		t.Fatalf("fingerprint %q does not hash the DER certificate", fingerprint)
	}
	if _, again, _ := same.TLSCertificate(); again != fingerprint {
		t.Fatal("certificate changed for the same instance key")
	}
	if _, other, _ := rotated.TLSCertificate(); other == fingerprint {
		t.Fatal("certificate survived an access token change")
	}
	parsed, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil || parsed.Subject.CommonName != "chinadapter" || parsed.NotAfter.Year() != 2099 {
		t.Fatalf("certificate %+v, %v", parsed, err)
	}
	pair, err := tls.X509KeyPair(pemEncode("CERTIFICATE", certificate.Certificate[0]), pemKey(t, certificate))
	if err != nil || len(pair.Certificate) != 1 {
		t.Fatalf("certificate and key do not match: %v", err)
	}
}

func pemEncode(kind string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der})
}

func pemKey(t *testing.T, certificate *tls.Certificate) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(certificate.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	return pemEncode("PRIVATE KEY", der)
}
