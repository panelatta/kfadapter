// Package clientconfig renders what a proxy client needs to reach one route
// through one inbound protocol: a Surge policy line and, where a standard
// form exists, a URI. It holds no secrets of its own; callers pass the
// locally derived credentials of a single route.
package clientconfig

import (
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/oshinop/chinadapter/internal/credentials"
	"github.com/oshinop/chinadapter/internal/provider"
	"github.com/oshinop/chinadapter/internal/shadowsocks"
	"github.com/oshinop/chinadapter/internal/state"
)

// Protocol names an inbound protocol in APIs and subscription queries.
type Protocol string

const (
	SOCKS5      Protocol = "socks5"
	Snell       Protocol = "snell"
	Shadowsocks Protocol = "shadowsocks"
	SOCKS5TLS   Protocol = "socks5-tls"
)

// Protocols lists every inbound protocol in console order.
var Protocols = []Protocol{SOCKS5, Snell, Shadowsocks, SOCKS5TLS}

// ParseProtocol accepts the API name of an inbound protocol.
func ParseProtocol(value string) (Protocol, bool) {
	for _, protocol := range Protocols {
		if string(protocol) == value {
			return protocol, true
		}
	}
	return "", false
}

// Enabled reports whether protocol is served under preferences; SOCKS5 always is.
func Enabled(protocol Protocol, preferences state.InboundPreferences) bool {
	switch protocol {
	case SOCKS5:
		return !preferences.SOCKS5Disabled
	case Snell:
		return preferences.Snell
	case Shadowsocks:
		return preferences.Shadowsocks
	case SOCKS5TLS:
		return preferences.SocksTLS
	}
	return false
}

// Ports are the listener ports of every inbound protocol.
type Ports struct {
	SOCKS5, Snell, Shadowsocks, SOCKS5TLS uint16
}

// Port returns the listener port of protocol.
func (ports Ports) Port(protocol Protocol) uint16 {
	switch protocol {
	case SOCKS5:
		return ports.SOCKS5
	case Snell:
		return ports.Snell
	case Shadowsocks:
		return ports.Shadowsocks
	case SOCKS5TLS:
		return ports.SOCKS5TLS
	}
	return 0
}

// Client tuning shared by every rendered policy. The adapter relays Chinese
// sites from abroad, where QUIC through a TCP tunnel only adds a fallback
// delay (video players such as Bilibili's retry over HTTP/2), and a probe of
// Surge's default overseas test URL would measure the wrong side of the
// tunnel.
const (
	// TestURL answers 204 from mainland China.
	TestURL         = "http://connectivitycheck.platform.hicloud.com/generate_204"
	snellParameters = "version=6, mode=unshaped, reuse=true"
)

// Profile is one route reached through one inbound protocol.
type Profile struct {
	Protocol Protocol
	Name     string
	Host     string
	Port     uint16
	// Username and Password authenticate SOCKS5 and SOCKS5-TLS.
	Username, Password string
	// PSK is the Snell pre-shared key.
	PSK string
	// ShadowsocksPassword is the multi-user "serverKey:userKey" password.
	ShadowsocksPassword string
	// TLSFingerprint pins the SOCKS5-TLS certificate (lowercase hex SHA-256).
	TLSFingerprint string
}

// RouteProfile derives route's credentials for protocol from identity.
func RouteProfile(identity *credentials.Identity, route provider.Route, protocol Protocol, name, host string, ports Ports) (Profile, bool) {
	profile := Profile{Protocol: protocol, Name: name, Host: host, Port: ports.Port(protocol)}
	switch protocol {
	case SOCKS5, SOCKS5TLS:
		socks, available := identity.Credentials(route)
		if !available {
			return Profile{}, false
		}
		profile.Username, profile.Password = socks.Username, socks.Password
	case Snell:
		psk, available := identity.SnellPSK(route)
		if !available {
			return Profile{}, false
		}
		profile.PSK = psk
	case Shadowsocks:
		serverKey, serverAvailable := identity.ShadowsocksServerKey()
		userKey, userAvailable := identity.ShadowsocksUserKey(route)
		if !serverAvailable || !userAvailable {
			return Profile{}, false
		}
		profile.ShadowsocksPassword = credentials.ShadowsocksPassword(serverKey, userKey)
	default:
		return Profile{}, false
	}
	return withFingerprint(identity, profile)
}

// SmartProfile derives the automatic route's credentials for protocol.
func SmartProfile(identity *credentials.Identity, protocol Protocol, name, host string, ports Ports) (Profile, bool) {
	profile := Profile{Protocol: protocol, Name: name, Host: host, Port: ports.Port(protocol)}
	switch protocol {
	case SOCKS5, SOCKS5TLS:
		smart, available := identity.SmartCredentials()
		if !available {
			return Profile{}, false
		}
		profile.Username, profile.Password = smart.Username, smart.Password
	case Snell:
		psk, available := identity.SmartSnellPSK()
		if !available {
			return Profile{}, false
		}
		profile.PSK = psk
	case Shadowsocks:
		serverKey, serverAvailable := identity.ShadowsocksServerKey()
		userKey, userAvailable := identity.SmartShadowsocksUserKey()
		if !serverAvailable || !userAvailable {
			return Profile{}, false
		}
		profile.ShadowsocksPassword = credentials.ShadowsocksPassword(serverKey, userKey)
	default:
		return Profile{}, false
	}
	return withFingerprint(identity, profile)
}

func withFingerprint(identity *credentials.Identity, profile Profile) (Profile, bool) {
	if profile.Protocol != SOCKS5TLS {
		return profile, true
	}
	fingerprint, available := identity.TLSFingerprint()
	profile.TLSFingerprint = fingerprint
	return profile, available
}

// Surge renders the profile as one Surge [Proxy] line.
func Surge(profile Profile) string {
	var line strings.Builder
	line.WriteString(PolicyName(profile.Name))
	line.WriteString(" = ")
	host, port := profile.Host, strconv.Itoa(int(profile.Port))
	switch profile.Protocol {
	case SOCKS5:
		line.WriteString("socks5, " + host + ", " + port + ", " + profile.Username + ", " + profile.Password)
	case SOCKS5TLS:
		line.WriteString("socks5-tls, " + host + ", " + port + ", " + profile.Username + ", " + profile.Password +
			", server-cert-fingerprint-sha256=" + profile.TLSFingerprint)
	case Snell:
		line.WriteString("snell, " + host + ", " + port + ", psk=" + profile.PSK + ", " + snellParameters)
	case Shadowsocks:
		line.WriteString("ss, " + host + ", " + port + ", encrypt-method=" + shadowsocks.Method + ", password=" + profile.ShadowsocksPassword +
			", udp-relay=true")
	}
	line.WriteString(", block-quic=on, test-url=" + TestURL)
	return line.String()
}

// PolicyName makes a node name safe as a Surge policy name: the line format
// reserves "=" and "," and a name must stay on one line.
func PolicyName(name string) string {
	name = strings.Map(func(character rune) rune {
		switch {
		case character == '=':
			return '＝'
		case character == ',':
			return '，'
		case character < 0x20 || character == 0x7f:
			return ' '
		}
		return character
	}, name)
	name = strings.TrimSpace(name)
	if name == "" {
		return "chinadapter"
	}
	return name
}

// URI renders the standard URI of a SOCKS5 or Shadowsocks profile; the other
// protocols have no widely accepted URI form.
func URI(profile Profile) (string, bool) {
	address := net.JoinHostPort(profile.Host, strconv.Itoa(int(profile.Port)))
	fragment := (&url.URL{Fragment: profile.Name}).EscapedFragment()
	switch profile.Protocol {
	case SOCKS5:
		return (&url.URL{Scheme: "socks5", User: url.UserPassword(profile.Username, profile.Password), Host: address, Fragment: profile.Name}).String(), true
	case Shadowsocks:
		// SIP002 percent-encodes the user info of 2022 methods instead of
		// Base64-encoding it.
		return "ss://" + shadowsocks.Method + ":" + escapeUserinfo(profile.ShadowsocksPassword) + "@" + address + "#" + fragment, true
	case Snell, SOCKS5TLS:
	}
	return "", false
}

func escapeUserinfo(value string) string {
	var escaped strings.Builder
	for index := range len(value) {
		character := value[index]
		if 'a' <= character && character <= 'z' || 'A' <= character && character <= 'Z' || '0' <= character && character <= '9' ||
			character == '-' || character == '.' || character == '_' || character == '~' {
			escaped.WriteByte(character)
			continue
		}
		escaped.WriteString("%" + strings.ToUpper(strconv.FormatUint(uint64(character)>>4, 16)+strconv.FormatUint(uint64(character)&0xf, 16)))
	}
	return escaped.String()
}
