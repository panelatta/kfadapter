// Package snell implements the server side of the Snell v6 wire protocol in
// its "unshaped" mode, the AES-128-GCM record layer Surge uses with
// `version=6, mode=unshaped`.
//
// Each direction of a connection starts with a random 16-byte salt. The
// session key is Argon2id(psk, salt, t=3, m=8 KiB, p=1) truncated to 16 bytes,
// and a 12-byte little-endian nonce counter advances after every seal or open.
// A record is seal(header) followed by seal(payload) when the payload is not
// empty; the 7-byte header is version 4, two reserved zero bytes, a zero
// padding length and the big-endian payload length. A record without payload
// is a zero chunk, the in-band half-close of one request.
//
// The record layer, request header, reuse and UDP-over-TCP framing follow the
// MIT-licensed meow-rs client, which was verified against snell-server
// v6.0.0rc2 (https://github.com/meow-rs/meow-rs/pull/658). The PSK-derived
// traffic shaping of the "default" mode is not implemented.
package snell

import (
	"crypto/aes"
	"crypto/cipher"
	"errors"

	"golang.org/x/crypto/argon2"
)

const (
	// SaltSize is the length of the salt that opens each direction.
	SaltSize = 16
	// MaxRecordPayload is the largest payload one record can carry.
	MaxRecordPayload = 0xffff

	recordVersion    = 4
	headerSize       = 7
	tagSize          = 16
	sealedHeaderSize = headerSize + tagSize
	nonceSize        = 12
	keySize          = 16
)

// Request header constants. The first byte of every request has been 1 since
// Snell v1.
const (
	Version byte = 1

	CommandPing      byte = 0
	CommandConnect   byte = 1
	CommandConnectV2 byte = 5 // reuse-capable connect
	CommandUDP       byte = 6

	ResponseTunnel byte = 0
	ResponsePong   byte = 1
	ResponseError  byte = 2

	udpForward byte = 1
)

var (
	// ErrZeroChunk reports the peer's in-band half-close of the current request.
	ErrZeroChunk = errors.New("snell: zero chunk")
	// ErrUnauthenticated means no candidate PSK opened the first record.
	ErrUnauthenticated = errors.New("snell: unknown pre-shared key")
	errBadRecord       = errors.New("snell: malformed record")
)

// SessionKey derives one direction's AES-128-GCM key.
func SessionKey(psk, salt []byte) []byte {
	return argon2.IDKey(psk, salt, 3, 8, 1, 32)[:keySize]
}

type sealer struct {
	aead  cipher.AEAD
	nonce [nonceSize]byte
}

func newSealer(psk, salt []byte) (*sealer, error) {
	block, err := aes.NewCipher(SessionKey(psk, salt))
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

func encodeHeader(payloadLength int) [headerSize]byte {
	return [headerSize]byte{recordVersion, 0, 0, 0, 0, byte(payloadLength >> 8), byte(payloadLength)}
}

func decodeHeader(header []byte) (int, error) {
	if len(header) != headerSize || header[0] != recordVersion || header[1] != 0 || header[2] != 0 || header[3] != 0 || header[4] != 0 {
		return 0, errBadRecord
	}
	return int(header[5])<<8 | int(header[6]), nil
}
