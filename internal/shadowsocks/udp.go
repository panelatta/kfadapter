package shadowsocks

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"slices"
	"sync/atomic"
	"time"
)

// UDP relay (SIP022 section 3.2 with SIP023 identity headers). A client
// packet is
//
//	AES(serverPSK, sessionID || packetID)              separate header
//	AES(serverPSK, hash(userPSK)[:16] ^ separate)      identity header
//	AEAD(subkey(userPSK, sessionID), separate[4:16],
//	     0 | timestamp | padding length | padding | SOCKS address | payload)
//
// and a server packet, which carries no identity header, is
//
//	AES(userPSK, serverSessionID || packetID)
//	AEAD(subkey(userPSK, serverSessionID), separate[4:16],
//	     1 | timestamp | client session ID | padding length | padding | SOCKS address | payload)
//
// All integers are big-endian. Sessions are identified by the client session
// ID, not the source address, so a client that changes networks keeps its
// session.
const (
	packetHeaderSize = 16
	// clientMainHeaderSize is type, timestamp and padding length.
	clientMainHeaderSize = 1 + 8 + 2
	// serverMainHeaderSize adds the client session ID.
	serverMainHeaderSize = 1 + 8 + 8 + 2
	// minSOCKSAddressSize is ATYP, a four-byte IPv4 address and the port.
	minSOCKSAddressSize = 1 + 4 + 2
	minClientPacketSize = 2*packetHeaderSize + clientMainHeaderSize + minSOCKSAddressSize + tagSize
	// replayWindowSize is the number of packet IDs below the highest one
	// that are still accepted once, in order or not.
	replayWindowSize = 2048
)

// PacketIdentity is the decrypted separate header of a client packet and the
// user its identity header names. It does not authenticate the packet.
type PacketIdentity struct {
	User      int
	SessionID uint64
	PacketID  uint64
	separate  [packetHeaderSize]byte
}

// IdentifyPacket decrypts the separate and identity headers of a client
// packet with the server PSK and finds the user. Only UDPSession.Open
// authenticates the packet.
func (users *Users) IdentifyPacket(packet []byte) (PacketIdentity, bool) {
	if users == nil || users.serverBlock == nil || len(packet) < minClientPacketSize {
		return PacketIdentity{}, false
	}
	var identity PacketIdentity
	users.serverBlock.Decrypt(identity.separate[:], packet[:packetHeaderSize])
	var hash [16]byte
	users.serverBlock.Decrypt(hash[:], packet[packetHeaderSize:2*packetHeaderSize])
	for index := range hash {
		hash[index] ^= identity.separate[index]
	}
	user, found := users.byHash[hash]
	if !found {
		return PacketIdentity{}, false
	}
	identity.User = user
	identity.SessionID = binary.BigEndian.Uint64(identity.separate[:8])
	identity.PacketID = binary.BigEndian.Uint64(identity.separate[8:])
	return identity, true
}

// UDPSession is the server side of one client UDP session. Open must not be
// called concurrently with itself; Seal may be called from any goroutine.
type UDPSession struct {
	clientSession uint64
	opener        cipher.AEAD
	window        replayWindow
	now           func() time.Time

	serverSession [8]byte
	headerBlock   cipher.Block
	sealer        cipher.AEAD
	nextPacket    atomic.Uint64
}

// NewUDPSession prepares the session that identity's client packet opens,
// with a new random server session ID.
func NewUDPSession(users *Users, identity PacketIdentity, now func() time.Time) (*UDPSession, error) {
	if users == nil || identity.User < 0 || identity.User >= len(users.keys) || now == nil {
		return nil, errors.New("shadowsocks: invalid UDP session")
	}
	userKey := users.keys[identity.User]
	opener, err := newSessionAEAD(userKey, identity.separate[:8])
	if err != nil {
		return nil, err
	}
	session := &UDPSession{clientSession: identity.SessionID, opener: opener, now: now}
	if _, err := rand.Read(session.serverSession[:]); err != nil {
		return nil, err
	}
	if session.headerBlock, err = aes.NewCipher(userKey); err != nil {
		return nil, err
	}
	if session.sealer, err = newSessionAEAD(userKey, session.serverSession[:]); err != nil {
		return nil, err
	}
	return session, nil
}

func newSessionAEAD(key, sessionID []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(deriveKey(sessionSubkeyContext, key, sessionID))
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Continue starts session's replay window where previous, an ended session
// with the same client session ID, left it, so that session's packets cannot
// be replayed into this one. Neither session may be opening packets.
func (session *UDPSession) Continue(previous *UDPSession) {
	if previous != nil && previous.clientSession == session.clientSession {
		session.window = previous.window
	}
}

// Open authenticates a client packet of this session, decrypting it in place,
// and returns its SOCKS address and payload, which alias packet. Every
// failure, including a replayed packet ID, is ErrUnauthenticated.
func (session *UDPSession) Open(identity PacketIdentity, packet []byte) ([]byte, error) {
	if identity.SessionID != session.clientSession || len(packet) < minClientPacketSize {
		return nil, ErrUnauthenticated
	}
	// The window changes only after the packet authenticates.
	if !session.window.check(identity.PacketID) {
		return nil, ErrUnauthenticated
	}
	sealed := packet[2*packetHeaderSize:]
	body, err := session.opener.Open(sealed[:0], identity.separate[4:], sealed, nil)
	if err != nil || body[0] != headerTypeClient || !withinSkew(body[1:9], session.now()) {
		return nil, ErrUnauthenticated
	}
	padding := int(binary.BigEndian.Uint16(body[9:11]))
	rest := body[clientMainHeaderSize:]
	if padding > len(rest) || !validSOCKSAddress(rest[padding:]) {
		return nil, ErrUnauthenticated
	}
	session.window.update(identity.PacketID)
	return rest[padding:], nil
}

// Seal appends to dst a server packet that carries addressAndPayload, a SOCKS
// address followed by the payload.
func (session *UDPSession) Seal(dst, addressAndPayload []byte) []byte {
	var separate [packetHeaderSize]byte
	copy(separate[:8], session.serverSession[:])
	binary.BigEndian.PutUint64(separate[8:], session.nextPacket.Add(1)-1)
	dst = slices.Grow(dst, packetHeaderSize+serverMainHeaderSize+len(addressAndPayload)+tagSize)
	start := len(dst)
	dst = dst[:start+packetHeaderSize]
	session.headerBlock.Encrypt(dst[start:], separate[:])
	bodyStart := len(dst)
	dst = append(dst, headerTypeServer)
	dst = binary.BigEndian.AppendUint64(dst, uint64(session.now().Unix()))
	dst = binary.BigEndian.AppendUint64(dst, session.clientSession)
	dst = binary.BigEndian.AppendUint16(dst, 0)
	dst = append(dst, addressAndPayload...)
	sealed := session.sealer.Seal(dst[bodyStart:bodyStart], separate[4:], dst[bodyStart:], nil)
	return dst[:bodyStart+len(sealed)]
}

// validSOCKSAddress checks that packet starts with a complete SOCKS address.
func validSOCKSAddress(packet []byte) bool {
	if len(packet) < 1 {
		return false
	}
	switch packet[0] {
	case addressIPv4:
		return len(packet) >= 1+4+2
	case addressIPv6:
		return len(packet) >= 1+16+2
	case addressDomain:
		return len(packet) >= 2 && packet[1] != 0 && len(packet) >= 2+int(packet[1])+2
	default:
		return false
	}
}

// replayWindow is a per-session sliding window filter over packet IDs.
type replayWindow struct {
	started bool
	highest uint64
	seen    [replayWindowSize / 64]uint64
}

// check reports whether id is new: above the highest seen, or within the
// window and not yet seen. It does not record id.
func (window *replayWindow) check(id uint64) bool {
	if !window.started || id > window.highest {
		return true
	}
	if window.highest-id >= replayWindowSize {
		return false
	}
	bit := id % replayWindowSize
	return window.seen[bit/64]&(1<<(bit%64)) == 0
}

// update records id, which check accepted.
func (window *replayWindow) update(id uint64) {
	if !window.started || id > window.highest {
		if window.started && id-window.highest < replayWindowSize {
			for skipped := window.highest + 1; skipped < id; skipped++ {
				bit := skipped % replayWindowSize
				window.seen[bit/64] &^= 1 << (bit % 64)
			}
		} else {
			clear(window.seen[:])
		}
		window.started, window.highest = true, id
	}
	bit := id % replayWindowSize
	window.seen[bit/64] |= 1 << (bit % 64)
}
