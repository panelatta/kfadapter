// Package credentials derives installation-scoped subscription and proxy credentials.
package credentials

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"sync"

	"github.com/oshinop/chinadapter/internal/provider"
)

var ErrInvalidKey = errors.New("invalid installation identity key")

// Identity is immutable and safe to share between goroutines. An uninitialized
// installation has an Identity but no usable subscription or SOCKS authority.
type Identity struct {
	key            [sha256.Size]byte
	initialized    bool
	subscriptionID string
	password       string

	certificateOnce sync.Once
	certificate     *tls.Certificate
	fingerprint     string
	certificateErr  error
}

func New(derivedHash []byte, installationID string) (*Identity, error) {
	raw, err := hex.DecodeString(installationID)
	if err != nil || len(raw) != 16 || hex.EncodeToString(raw) != installationID {
		return nil, ErrInvalidKey
	}
	result := &Identity{}
	if len(derivedHash) == 0 {
		return result, nil
	}
	if len(derivedHash) != sha256.Size {
		return nil, ErrInvalidKey
	}
	result.key = mac(derivedHash, "chinadapter/instance/v3\x00"+installationID)
	result.initialized = true
	result.subscriptionID = uuid(mac(result.key[:], "chinadapter/subscription/v3"))
	result.password = uuid(mac(result.key[:], "chinadapter/socks-password/v3"))
	return result, nil
}

func (identity *Identity) SubscriptionID() string {
	if identity == nil {
		return ""
	}
	return identity.subscriptionID
}
func (identity *Identity) Password() string {
	if identity == nil {
		return ""
	}
	return identity.password
}

type Credentials struct{ Username, Password string }

func (identity *Identity) Credentials(route provider.Route) (Credentials, bool) {
	if identity == nil || !identity.initialized {
		return Credentials{}, false
	}
	key, err := provider.NodeKey(route)
	if err != nil {
		return Credentials{}, false
	}
	digest := mac(identity.key[:], "chinadapter/socks-user/v3\x00"+key)
	return Credentials{Username: "n_" + base64.RawURLEncoding.EncodeToString(digest[:12]), Password: identity.password}, true
}

// SmartCredentials names the automatic route. Its "a_" prefix keeps it out of
// the "n_" node namespace, and it shares the instance password, so it changes
// only when the access token does.
func (identity *Identity) SmartCredentials() (Credentials, bool) {
	if identity == nil || !identity.initialized {
		return Credentials{}, false
	}
	digest := mac(identity.key[:], "chinadapter/smart-user/v1")
	return Credentials{Username: "a_" + base64.RawURLEncoding.EncodeToString(digest[:12]), Password: identity.password}, true
}

// ShadowsocksKeySize is the key length of 2022-blake3-aes-128-gcm.
const ShadowsocksKeySize = 16

// SnellPSK is the route's Snell pre-shared key. Snell feeds the text itself
// to its key derivation, so the URL-safe Base64 form is the key.
func (identity *Identity) SnellPSK(route provider.Route) (string, bool) {
	key, available := identity.routeDigest("chinadapter/snell-psk/v1\x00", route)
	if !available {
		return "", false
	}
	return base64.RawURLEncoding.EncodeToString(key[:18]), true
}

// SmartSnellPSK is the Snell pre-shared key of the automatic route.
func (identity *Identity) SmartSnellPSK() (string, bool) {
	if identity == nil || !identity.initialized {
		return "", false
	}
	digest := mac(identity.key[:], "chinadapter/snell-smart-psk/v1")
	return base64.RawURLEncoding.EncodeToString(digest[:18]), true
}

// ShadowsocksServerKey is the identity PSK shared by every Shadowsocks 2022
// route; each route adds its own user key.
func (identity *Identity) ShadowsocksServerKey() ([]byte, bool) {
	if identity == nil || !identity.initialized {
		return nil, false
	}
	digest := mac(identity.key[:], "chinadapter/ss2022-server/v1")
	return digest[:ShadowsocksKeySize], true
}

// ShadowsocksUserKey is the route's Shadowsocks 2022 user PSK.
func (identity *Identity) ShadowsocksUserKey(route provider.Route) ([]byte, bool) {
	digest, available := identity.routeDigest("chinadapter/ss2022-user/v1\x00", route)
	if !available {
		return nil, false
	}
	return digest[:ShadowsocksKeySize], true
}

// SmartShadowsocksUserKey is the automatic route's Shadowsocks 2022 user PSK.
func (identity *Identity) SmartShadowsocksUserKey() ([]byte, bool) {
	if identity == nil || !identity.initialized {
		return nil, false
	}
	digest := mac(identity.key[:], "chinadapter/ss2022-smart-user/v1")
	return digest[:ShadowsocksKeySize], true
}

// ShadowsocksPassword renders the multi-user password "serverKey:userKey".
func ShadowsocksPassword(serverKey, userKey []byte) string {
	return base64.StdEncoding.EncodeToString(serverKey) + ":" + base64.StdEncoding.EncodeToString(userKey)
}

func (identity *Identity) routeDigest(label string, route provider.Route) ([sha256.Size]byte, bool) {
	if identity == nil || !identity.initialized {
		return [sha256.Size]byte{}, false
	}
	key, err := provider.NodeKey(route)
	if err != nil {
		return [sha256.Size]byte{}, false
	}
	return mac(identity.key[:], label+key), true
}

func (identity *Identity) AuthenticatePassword(password string) bool {
	return identity != nil && identity.initialized && subtle.ConstantTimeCompare([]byte(password), []byte(identity.password)) == 1
}

func (identity *Identity) MatchesSubscriptionID(id string) bool {
	return identity != nil && identity.initialized && subtle.ConstantTimeCompare([]byte(id), []byte(identity.subscriptionID)) == 1
}

// ParseSubscriptionPath accepts only /sub/<lowercase canonical UUIDv8>, with
// the RFC 9562 variant bits. It deliberately rejects escaped aliases.
func ParseSubscriptionPath(path string) (string, bool) {
	if len(path) != 41 || !strings.HasPrefix(path, "/sub/") {
		return "", false
	}
	id := path[5:]
	if !ValidUUID(id) {
		return "", false
	}
	return id, true
}
func ValidUUID(id string) bool {
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' || id[14] != '8' {
		return false
	}
	raw := strings.ReplaceAll(id, "-", "")
	decoded, err := hex.DecodeString(raw)
	return err == nil && len(decoded) == 16 && decoded[8]>>6 == 2 && hex.EncodeToString(decoded) == raw
}
func uuid(digest [sha256.Size]byte) string {
	digest[6] = digest[6]&0x0f | 0x80
	digest[8] = digest[8]&0x3f | 0x80
	encoded := hex.EncodeToString(digest[:16])
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
}
func mac(key []byte, message string) [sha256.Size]byte {
	hash := hmac.New(sha256.New, key)
	_, _ = hash.Write([]byte(message))
	var digest [sha256.Size]byte
	hash.Sum(digest[:0])
	return digest
}
