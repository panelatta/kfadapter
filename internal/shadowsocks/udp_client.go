package shadowsocks

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"slices"
	"time"
)

// UDPClient is the client side of one UDP session through the identity of
// serverKey:userKey. It exists for tests and interoperability checks; the
// daemon only serves UDP sessions.
type UDPClient struct {
	serverBlock cipher.Block
	userHash    [16]byte
	userKey     []byte
	userBlock   cipher.Block
	session     [8]byte
	sealer      cipher.AEAD
	now         func() time.Time
	// NextPacketID is the packet ID of the next sealed packet.
	NextPacketID uint64
	servers      map[[8]byte]*clientServerSession
}

type clientServerSession struct {
	opener cipher.AEAD
	window replayWindow
}

func NewUDPClient(serverKey, userKey []byte, now func() time.Time) (*UDPClient, error) {
	if len(serverKey) != KeySize || len(userKey) != KeySize || now == nil {
		return nil, errors.New("shadowsocks: invalid UDP client keys")
	}
	client := &UDPClient{userHash: keyHash(userKey), userKey: append([]byte(nil), userKey...), now: now, servers: make(map[[8]byte]*clientServerSession)}
	var err error
	if client.serverBlock, err = aes.NewCipher(serverKey); err != nil {
		return nil, err
	}
	if client.userBlock, err = aes.NewCipher(userKey); err != nil {
		return nil, err
	}
	if _, err := rand.Read(client.session[:]); err != nil {
		return nil, err
	}
	if client.sealer, err = newSessionAEAD(userKey, client.session[:]); err != nil {
		return nil, err
	}
	return client, nil
}

// SessionID is the client session ID.
func (client *UDPClient) SessionID() uint64 { return binary.BigEndian.Uint64(client.session[:]) }

// Seal appends a client packet carrying addressAndPayload, a SOCKS address
// followed by the payload, with padding zero bytes of padding.
func (client *UDPClient) Seal(dst, addressAndPayload []byte, padding int) []byte {
	var separate [packetHeaderSize]byte
	copy(separate[:8], client.session[:])
	binary.BigEndian.PutUint64(separate[8:], client.NextPacketID)
	client.NextPacketID++
	dst = slices.Grow(dst, 2*packetHeaderSize+clientMainHeaderSize+padding+len(addressAndPayload)+tagSize)
	start := len(dst)
	dst = dst[:start+2*packetHeaderSize]
	client.serverBlock.Encrypt(dst[start:], separate[:])
	var identity [16]byte
	for index := range identity {
		identity[index] = client.userHash[index] ^ separate[index]
	}
	client.serverBlock.Encrypt(dst[start+packetHeaderSize:], identity[:])
	bodyStart := len(dst)
	dst = append(dst, headerTypeClient)
	dst = binary.BigEndian.AppendUint64(dst, uint64(client.now().Unix()))
	dst = binary.BigEndian.AppendUint16(dst, uint16(padding))
	dst = append(dst, make([]byte, padding)...)
	dst = append(dst, addressAndPayload...)
	sealed := client.sealer.Seal(dst[bodyStart:bodyStart], separate[4:], dst[bodyStart:], nil)
	return dst[:bodyStart+len(sealed)]
}

// Open authenticates a server packet, decrypting it in place, and returns its
// SOCKS address and payload, which alias packet.
func (client *UDPClient) Open(packet []byte) ([]byte, error) {
	if len(packet) < packetHeaderSize+serverMainHeaderSize+minSOCKSAddressSize+tagSize {
		return nil, ErrUnauthenticated
	}
	var separate [packetHeaderSize]byte
	client.userBlock.Decrypt(separate[:], packet[:packetHeaderSize])
	serverSession := [8]byte(separate[:8])
	packetID := binary.BigEndian.Uint64(separate[8:])
	server := client.servers[serverSession]
	if server == nil {
		opener, err := newSessionAEAD(client.userKey, serverSession[:])
		if err != nil {
			return nil, err
		}
		server = &clientServerSession{opener: opener}
	}
	if !server.window.check(packetID) {
		return nil, ErrUnauthenticated
	}
	sealed := packet[packetHeaderSize:]
	body, err := server.opener.Open(sealed[:0], separate[4:], sealed, nil)
	if err != nil || body[0] != headerTypeServer || !withinSkew(body[1:9], client.now()) ||
		binary.BigEndian.Uint64(body[9:17]) != client.SessionID() {
		return nil, ErrUnauthenticated
	}
	padding := int(binary.BigEndian.Uint16(body[17:19]))
	rest := body[serverMainHeaderSize:]
	if padding > len(rest) || !validSOCKSAddress(rest[padding:]) {
		return nil, ErrUnauthenticated
	}
	server.window.update(packetID)
	client.servers[serverSession] = server
	return rest[padding:], nil
}
