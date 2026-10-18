package shadowsocks

import (
	"bytes"
	"testing"
	"time"
)

// dnsTarget is a SOCKS address (8.8.8.8:53) followed by a payload.
func dnsTarget(payload string) []byte {
	return append([]byte{addressIPv4, 8, 8, 8, 8, 0, 53}, payload...)
}

func udpFixture(t *testing.T, users int) (serverKey []byte, userKeys [][]byte, table *Users) {
	t.Helper()
	serverKey = randomKey(t)
	for range users {
		userKeys = append(userKeys, randomKey(t))
	}
	table, err := NewUsers(serverKey, userKeys)
	if err != nil {
		t.Fatal(err)
	}
	return serverKey, userKeys, table
}

func openClientPacket(t *testing.T, users *Users, sessions map[uint64]*UDPSession, packet []byte) ([]byte, int, error) {
	t.Helper()
	identity, found := users.IdentifyPacket(packet)
	if !found {
		return nil, -1, ErrUnauthenticated
	}
	session := sessions[identity.SessionID]
	if session == nil {
		var err error
		if session, err = NewUDPSession(users, identity, time.Now); err != nil {
			t.Fatal(err)
		}
	}
	opened, err := session.Open(identity, packet)
	if err == nil {
		sessions[identity.SessionID] = session
	}
	return opened, identity.User, err
}

func TestUDPRoundTripIdentifiesUsers(t *testing.T) {
	serverKey, userKeys, users := udpFixture(t, 3)
	sessions := make(map[uint64]*UDPSession)
	for user, userKey := range userKeys {
		client, err := NewUDPClient(serverKey, userKey, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		for sequence, padding := range []int{0, 37} {
			want := dnsTarget("query")
			opened, index, err := openClientPacket(t, users, sessions, client.Seal(nil, want, padding))
			if err != nil || index != user || !bytes.Equal(opened, want) {
				t.Fatalf("user %d packet %d = %q user %d, %v", user, sequence, opened, index, err)
			}
		}
		session := sessions[client.SessionID()]
		reply := append([]byte{addressDomain, 7}, "example"...)
		reply = append(reply, 1, 187, 'o', 'k')
		for range 2 {
			opened, err := client.Open(session.Seal(nil, reply))
			if err != nil || !bytes.Equal(opened, reply) {
				t.Fatalf("user %d reply = %q, %v", user, opened, err)
			}
		}
	}
	if len(sessions) != len(userKeys) {
		t.Fatalf("sessions = %d, want one per client", len(sessions))
	}
}

func TestUDPRejectsUnknownKeysSkewAndBadAddresses(t *testing.T) {
	serverKey, userKeys, users := udpFixture(t, 1)
	sessions := make(map[uint64]*UDPSession)
	stranger, _ := NewUDPClient(serverKey, randomKey(t), time.Now)
	wrongServer, _ := NewUDPClient(randomKey(t), userKeys[0], time.Now)
	for name, packet := range map[string][]byte{
		"unknown user": stranger.Seal(nil, dnsTarget("x"), 0),
		"wrong server": wrongServer.Seal(nil, dnsTarget("x"), 0),
		"too short":    make([]byte, minClientPacketSize-1),
	} {
		if _, ok := users.IdentifyPacket(packet); ok {
			t.Errorf("%s: identified", name)
		}
	}
	for name, client := range map[string]*UDPClient{
		"past":   mustClient(t, serverKey, userKeys[0], func() time.Time { return time.Now().Add(-maxTimeSkew - 2*time.Second) }),
		"future": mustClient(t, serverKey, userKeys[0], func() time.Time { return time.Now().Add(maxTimeSkew + 2*time.Second) }),
	} {
		if _, _, err := openClientPacket(t, users, sessions, client.Seal(nil, dnsTarget("x"), 0)); err == nil {
			t.Errorf("%s timestamp accepted", name)
		}
	}
	client := mustClient(t, serverKey, userKeys[0], time.Now)
	for name, address := range map[string][]byte{
		"unknown type":   {9, 1, 2, 3, 4, 0, 53},
		"short IPv6":     {addressIPv6, 1, 2, 3, 4, 5, 6, 7},
		"empty domain":   {addressDomain, 0, 0, 53},
		"cut off domain": {addressDomain, 9, 'a', 'b', 0, 53},
	} {
		if _, _, err := openClientPacket(t, users, sessions, client.Seal(nil, address, 0)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if len(sessions) != 0 {
		t.Fatalf("rejected packets created %d sessions", len(sessions))
	}
}

func mustClient(t *testing.T, serverKey, userKey []byte, now func() time.Time) *UDPClient {
	t.Helper()
	client, err := NewUDPClient(serverKey, userKey, now)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestUDPReplayWindow(t *testing.T) {
	serverKey, userKeys, users := udpFixture(t, 1)
	client := mustClient(t, serverKey, userKeys[0], time.Now)
	sessions := make(map[uint64]*UDPSession)
	packets := make(map[uint64][]byte)
	seal := func(id uint64) []byte {
		client.NextPacketID = id
		packets[id] = client.Seal(nil, dnsTarget("x"), 0)
		return bytes.Clone(packets[id])
	}
	accepts := func(packet []byte) bool {
		_, _, err := openClientPacket(t, users, sessions, packet)
		return err == nil
	}
	// Reordering within the window is accepted once per packet ID.
	for _, id := range []uint64{5, 3, 4, 9} {
		if !accepts(seal(id)) {
			t.Fatalf("packet %d rejected", id)
		}
	}
	for _, id := range []uint64{5, 3, 9} {
		if accepts(bytes.Clone(packets[id])) {
			t.Fatalf("replayed packet %d accepted", id)
		}
	}
	// A failed packet must not advance the window: a tampered packet 5000
	// is rejected, and the genuine one still opens.
	genuine := seal(5000)
	tampered := bytes.Clone(genuine)
	tampered[len(tampered)-1] ^= 1
	if accepts(tampered) || !accepts(genuine) {
		t.Fatal("tampered packet changed the replay window")
	}
	if accepts(seal(5000 - replayWindowSize)) {
		t.Fatal("packet below the window accepted")
	}
	if !accepts(seal(5000 - replayWindowSize + 1)) {
		t.Fatal("oldest packet in the window rejected")
	}
	// Packets skipped by a jump stay acceptable.
	if !accepts(seal(4990)) {
		t.Fatal("skipped packet inside the window rejected")
	}
}

func TestUDPClientRejectsReplayedServerPackets(t *testing.T) {
	serverKey, userKeys, users := udpFixture(t, 1)
	client := mustClient(t, serverKey, userKeys[0], time.Now)
	sessions := make(map[uint64]*UDPSession)
	if _, _, err := openClientPacket(t, users, sessions, client.Seal(nil, dnsTarget("x"), 0)); err != nil {
		t.Fatal(err)
	}
	reply := sessions[client.SessionID()].Seal(nil, dnsTarget("reply"))
	if _, err := client.Open(bytes.Clone(reply)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Open(reply); err == nil {
		t.Fatal("replayed server packet accepted")
	}
}

func TestReplayWindowLargeJump(t *testing.T) {
	var window replayWindow
	for _, id := range []uint64{0, 1, 2} {
		window.update(id)
	}
	window.update(10 * replayWindowSize)
	for _, id := range []uint64{2, 9 * replayWindowSize} {
		if window.check(id) {
			t.Fatalf("packet %d below the window accepted", id)
		}
	}
	if !window.check(10*replayWindowSize-1) || window.check(10*replayWindowSize) {
		t.Fatal("window state wrong after a jump")
	}
}
