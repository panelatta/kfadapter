package credentials

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"io"
	"math/big"
	"strconv"
	"time"
)

// certificateName is the fixed subject and DNS name of the SOCKS5-TLS
// certificate. Clients pin its fingerprint instead of validating a name.
const certificateName = "chinadapter"

// TLSCertificate returns the SOCKS5-TLS server certificate and the lowercase
// hex SHA-256 fingerprint of its DER encoding. The ECDSA P-256 key and the
// certificate are derived deterministically from the instance key, so the
// fingerprint survives restarts and changes only with the access token.
func (identity *Identity) TLSCertificate() (*tls.Certificate, string, error) {
	if identity == nil || !identity.initialized {
		return nil, "", ErrInvalidKey
	}
	identity.certificateOnce.Do(func() {
		identity.certificate, identity.fingerprint, identity.certificateErr = deriveCertificate(identity.key[:])
	})
	return identity.certificate, identity.fingerprint, identity.certificateErr
}

// TLSFingerprint is the SHA-256 fingerprint of TLSCertificate.
func (identity *Identity) TLSFingerprint() (string, bool) {
	_, fingerprint, err := identity.TLSCertificate()
	return fingerprint, err == nil
}

func deriveCertificate(instanceKey []byte) (*tls.Certificate, string, error) {
	var key *ecdsa.PrivateKey
	// A digest is a valid P-256 scalar unless it is zero or not below the
	// group order, which the counter skips with overwhelming probability.
	for counter := range 16 {
		scalar := mac(instanceKey, "chinadapter/tls-key/v1\x00"+strconv.Itoa(counter))
		candidate, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), scalar[:])
		if err == nil {
			key = candidate
			break
		}
	}
	if key == nil {
		return nil, "", errors.New("credentials: no valid TLS key scalar")
	}
	publicKey, err := x509.MarshalPKIXPublicKey(key.Public())
	if err != nil {
		return nil, "", err
	}
	keyID := sha256.Sum256(publicKey)
	template := &x509.Certificate{
		SerialNumber:          new(big.Int).SetBytes(keyID[:16]),
		Subject:               pkix.Name{CommonName: certificateName},
		DNSNames:              []string{certificateName},
		NotBefore:             time.Date(2025, time.January, 1, 0, 0, 0, 0, time.UTC),
		NotAfter:              time.Date(2099, time.December, 31, 0, 0, 0, 0, time.UTC),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		SubjectKeyId:          keyID[:20],
	}
	der, err := x509.CreateCertificate(nil, template, template, key.Public(), deterministicSigner{key})
	if err != nil {
		return nil, "", err
	}
	fingerprint := sha256.Sum256(der)
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, hex.EncodeToString(fingerprint[:]), nil
}

// deterministicSigner signs with RFC 6979 nonces, which crypto/ecdsa uses
// when no randomness source is given, so the certificate is reproducible.
type deterministicSigner struct{ key *ecdsa.PrivateKey }

func (signer deterministicSigner) Public() crypto.PublicKey { return signer.key.Public() }

func (signer deterministicSigner) Sign(_ io.Reader, digest []byte, options crypto.SignerOpts) ([]byte, error) {
	return signer.key.Sign(nil, digest, options)
}
