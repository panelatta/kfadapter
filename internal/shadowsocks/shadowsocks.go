// Package shadowsocks implements the server side of Shadowsocks 2022 TCP
// relay with the 2022-blake3-aes-128-gcm method and extensible identity
// headers (one server PSK, one user PSK per route), the multi-user form Surge
// configures as `password=serverKey:userKey`.
//
// Specifications: SIP022 (Shadowsocks 2022 Edition) and SIP023 (Extensible
// Identity Headers), https://github.com/Shadowsocks-NET/shadowsocks-specs.
package shadowsocks

import (
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"sync"
	"time"

	"lukechampine.com/blake3"
)

const (
	// Method is the only cipher this server speaks.
	Method = "2022-blake3-aes-128-gcm"
	// KeySize is the PSK and salt length of Method.
	KeySize = 16

	saltSize         = KeySize
	tagSize          = 16
	nonceSize        = 12
	maxChunkPayload  = 0xffff
	maxPaddingLength = 900
	headerTypeClient = 0
	headerTypeServer = 1
	// Messages more than 30 seconds off the local clock are replays.
	maxTimeSkew = 30 * time.Second
	// Salts are remembered for twice the accepted skew.
	saltWindow = 60 * time.Second

	sessionSubkeyContext  = "shadowsocks 2022 session subkey"
	identitySubkeyContext = "shadowsocks 2022 identity subkey"
)

var (
	// ErrUnauthenticated covers every request that fails identity, AEAD,
	// timestamp or replay checks; callers must not reveal which.
	ErrUnauthenticated = errors.New("shadowsocks: request authentication failed")
	errBadHeader       = errors.New("shadowsocks: malformed request header")
)

// Users is an immutable identity table: the server PSK and the user PSKs,
// indexed by the first 16 bytes of each user PSK's BLAKE3 hash.
type Users struct {
	serverKey []byte
	keys      [][]byte
	byHash    map[[16]byte]int
}

// NewUsers builds the identity table. Each key must be KeySize bytes and
// user keys must be distinct.
func NewUsers(serverKey []byte, userKeys [][]byte) (*Users, error) {
	if len(serverKey) != KeySize {
		return nil, errors.New("shadowsocks: invalid server key")
	}
	users := &Users{serverKey: append([]byte(nil), serverKey...), keys: make([][]byte, len(userKeys)), byHash: make(map[[16]byte]int, len(userKeys))}
	for index, key := range userKeys {
		if len(key) != KeySize {
			return nil, errors.New("shadowsocks: invalid user key")
		}
		hash := keyHash(key)
		if _, duplicate := users.byHash[hash]; duplicate {
			return nil, errors.New("shadowsocks: duplicate user key")
		}
		users.keys[index] = append([]byte(nil), key...)
		users.byHash[hash] = index
	}
	return users, nil
}

// identify decrypts an identity header with the server PSK's identity
// subkey for salt and returns the matching user index.
func (users *Users) identify(salt, header []byte) (int, bool) {
	if users == nil {
		return -1, false
	}
	block, err := aes.NewCipher(deriveKey(identitySubkeyContext, users.serverKey, salt))
	if err != nil {
		return -1, false
	}
	var hash [16]byte
	block.Decrypt(hash[:], header)
	index, found := users.byHash[hash]
	return index, found
}

func keyHash(key []byte) [16]byte {
	sum := blake3.Sum256(key)
	var hash [16]byte
	copy(hash[:], sum[:16])
	return hash
}

func deriveKey(context string, key, salt []byte) []byte {
	material := make([]byte, 0, len(key)+len(salt))
	material = append(append(material, key...), salt...)
	subkey := make([]byte, KeySize)
	blake3.DeriveKey(subkey, context, material)
	return subkey
}

type sealer struct {
	aead  cipher.AEAD
	nonce [nonceSize]byte
}

func newSealer(key, salt []byte) (*sealer, error) {
	block, err := aes.NewCipher(deriveKey(sessionSubkeyContext, key, salt))
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &sealer{aead: aead}, nil
}

func (s *sealer) seal(dst, plaintext []byte) []byte {
	dst = s.aead.Seal(dst, s.nonce[:], plaintext, nil)
	s.increment()
	return dst
}

func (s *sealer) open(dst, ciphertext []byte) ([]byte, error) {
	plaintext, err := s.aead.Open(dst, s.nonce[:], ciphertext, nil)
	if err != nil {
		return nil, err
	}
	s.increment()
	return plaintext, nil
}

func (s *sealer) increment() {
	for index := range s.nonce {
		s.nonce[index]++
		if s.nonce[index] != 0 {
			return
		}
	}
}

// SaltFilter remembers accepted request salts for at least saltWindow. Two
// rotating generations bound memory without false positives.
type SaltFilter struct {
	mu        sync.Mutex
	current   map[[saltSize]byte]struct{}
	previous  map[[saltSize]byte]struct{}
	rotatedAt time.Time
}

func NewSaltFilter() *SaltFilter {
	return &SaltFilter{current: make(map[[saltSize]byte]struct{}), previous: make(map[[saltSize]byte]struct{})}
}

// Add records salt and reports false when it was already seen.
func (filter *SaltFilter) Add(salt []byte, now time.Time) bool {
	var key [saltSize]byte
	copy(key[:], salt)
	filter.mu.Lock()
	defer filter.mu.Unlock()
	if filter.rotatedAt.IsZero() {
		filter.rotatedAt = now
	}
	if elapsed := now.Sub(filter.rotatedAt); elapsed >= saltWindow || elapsed < 0 {
		filter.previous, filter.current = filter.current, make(map[[saltSize]byte]struct{})
		if elapsed >= 2*saltWindow {
			filter.previous = make(map[[saltSize]byte]struct{})
		}
		filter.rotatedAt = now
	}
	if _, seen := filter.current[key]; seen {
		return false
	}
	if _, seen := filter.previous[key]; seen {
		return false
	}
	filter.current[key] = struct{}{}
	return true
}
