package socks

import (
	"crypto/subtle"
	"crypto/tls"
	"fmt"
	"sync"

	credentialspkg "github.com/oshinop/chinadapter/internal/credentials"
	"github.com/oshinop/chinadapter/internal/shadowsocks"
	"github.com/oshinop/chinadapter/internal/state"
)

// credentialRegistry is one immutable generation of every frontend's
// credentials. Each frontend credential names a route: a node's SOCKS
// username, or the automatic route's username for its smart credentials.
type credentialRegistry struct {
	identity *credentialspkg.Identity
	nodes    map[string]struct{}
	smart    string

	snellKeys  [][]byte
	snellRoute []string
	snellIndex map[string]int

	shadowsocks      *shadowsocks.Users
	shadowsocksKeys  [][]byte
	shadowsocksRoute []string
	shadowsocksIndex map[string]int

	certificate *tls.Certificate
}

func buildCredentialRegistry(authority *credentialspkg.Identity, snapshot *state.RuntimeSnapshot) (*credentialRegistry, error) {
	nodeCount := 0
	if snapshot != nil {
		nodeCount = len(snapshot.Nodes)
	}
	registry := &credentialRegistry{
		identity: authority, nodes: make(map[string]struct{}, nodeCount),
		snellIndex: make(map[string]int, nodeCount+1), shadowsocksIndex: make(map[string]int, nodeCount+1),
	}
	smart, initialized := authority.SmartCredentials()
	if !initialized {
		// Without an access token no route has credentials to admit.
		if snapshot != nil {
			for _, node := range snapshot.Nodes {
				if _, available := snapshot.Providers[node.Provider]; available && node.TunnelEligible() {
					return nil, credentialspkg.ErrInvalidKey
				}
			}
		}
		return registry, nil
	}
	registry.smart = smart.Username
	if certificate, _, err := authority.TLSCertificate(); err == nil {
		registry.certificate = certificate
	}
	smartPSK, _ := authority.SmartSnellPSK()
	smartUserKey, _ := authority.SmartShadowsocksUserKey()
	registry.addRoute(smart.Username, smartPSK, smartUserKey)
	if snapshot != nil {
		for _, node := range snapshot.Nodes {
			if !node.TunnelEligible() {
				continue
			}
			if _, available := snapshot.Providers[node.Provider]; !available {
				continue
			}
			route := node.Route()
			credentials, available := authority.Credentials(route)
			psk, pskAvailable := authority.SnellPSK(route)
			userKey, userKeyAvailable := authority.ShadowsocksUserKey(route)
			if !available || !pskAvailable || !userKeyAvailable {
				return nil, credentialspkg.ErrInvalidKey
			}
			if _, duplicate := registry.nodes[credentials.Username]; duplicate {
				return nil, fmt.Errorf("%w: duplicate node route", state.ErrInvalidSnapshot)
			}
			registry.nodes[credentials.Username] = struct{}{}
			registry.addRoute(credentials.Username, psk, userKey)
		}
	}
	serverKey, _ := authority.ShadowsocksServerKey()
	users, err := shadowsocks.NewUsers(serverKey, registry.shadowsocksKeys)
	if err != nil {
		return nil, err
	}
	registry.shadowsocks = users
	return registry, nil
}

func (registry *credentialRegistry) addRoute(route, psk string, userKey []byte) {
	registry.snellIndex[psk] = len(registry.snellKeys)
	registry.snellKeys = append(registry.snellKeys, []byte(psk))
	registry.snellRoute = append(registry.snellRoute, route)
	registry.shadowsocksIndex[string(userKey)] = len(registry.shadowsocksKeys)
	registry.shadowsocksKeys = append(registry.shadowsocksKeys, userKey)
	registry.shadowsocksRoute = append(registry.shadowsocksRoute, route)
}

func (registry *credentialRegistry) authenticate(username, password string) bool {
	if registry == nil {
		return false
	}
	_, available := registry.nodes[username]
	return available && registry.identity.AuthenticatePassword(password)
}

// authenticateSmart accepts the automatic-route username, which lives in its
// own namespace and never appears in the node map.
func (registry *credentialRegistry) authenticateSmart(username, password string) bool {
	return registry != nil && registry.smart != "" && subtle.ConstantTimeCompare([]byte(username), []byte(registry.smart)) == 1 &&
		registry.identity.AuthenticatePassword(password)
}

// snellRouteFor returns the route a Snell PSK currently names.
func (registry *credentialRegistry) snellRouteFor(psk []byte) (string, bool) {
	if registry == nil {
		return "", false
	}
	index, found := registry.snellIndex[string(psk)]
	if !found {
		return "", false
	}
	return registry.snellRoute[index], true
}

// shadowsocksRouteFor returns the route a Shadowsocks user key currently names.
func (registry *credentialRegistry) shadowsocksRouteFor(userKey []byte) (string, bool) {
	if registry == nil {
		return "", false
	}
	index, found := registry.shadowsocksIndex[string(userKey)]
	if !found {
		return "", false
	}
	return registry.shadowsocksRoute[index], true
}

// recentKeys orders Snell trial decryption: PSKs that matched recently are
// tried first, so a client reusing a few routes costs one or two Argon2id
// derivations per connection instead of one per route.
type recentKeys struct {
	mu   sync.Mutex
	keys []string
}

const recentKeyLimit = 16

func (recent *recentKeys) order(registry *credentialRegistry) [][]byte {
	recent.mu.Lock()
	first := make([]int, 0, len(recent.keys))
	for _, key := range recent.keys {
		if index, found := registry.snellIndex[key]; found {
			first = append(first, index)
		}
	}
	recent.mu.Unlock()
	if len(first) == 0 {
		return registry.snellKeys
	}
	ordered := make([][]byte, 0, len(registry.snellKeys))
	seen := make(map[int]struct{}, len(first))
	for _, index := range first {
		seen[index] = struct{}{}
		ordered = append(ordered, registry.snellKeys[index])
	}
	for index, key := range registry.snellKeys {
		if _, duplicate := seen[index]; !duplicate {
			ordered = append(ordered, key)
		}
	}
	return ordered
}

func (recent *recentKeys) touch(key []byte) {
	recent.mu.Lock()
	defer recent.mu.Unlock()
	for index, existing := range recent.keys {
		if existing == string(key) {
			copy(recent.keys[1:index+1], recent.keys[:index])
			recent.keys[0] = existing
			return
		}
	}
	if len(recent.keys) < recentKeyLimit {
		recent.keys = append(recent.keys, "")
	}
	copy(recent.keys[1:], recent.keys)
	recent.keys[0] = string(key)
}
