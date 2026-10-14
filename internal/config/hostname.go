package config

import (
	"errors"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

var errInvalidHostname = errors.New("must be a bare DNS hostname")

// ValidateHostname accepts an ASCII DNS hostname without a scheme, port, or trailing dot.
func ValidateHostname(hostname string) error {
	if hostname == "" || len(hostname) > 253 || strings.HasSuffix(hostname, ".") {
		return errInvalidHostname
	}
	for _, label := range strings.Split(hostname, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return errInvalidHostname
		}
		for _, char := range label {
			if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '-' {
				return errInvalidHostname
			}
		}
	}
	if _, err := netip.ParseAddr(hostname); err == nil {
		return errInvalidHostname
	}
	return nil
}

// ParseHTTPSOrigin validates a canonical browser origin for an explicitly
// configured TLS reverse proxy. Omitting the HTTPS default port keeps this
// value equal to the Origin header serialized by browsers.
func ParseHTTPSOrigin(origin string) (*url.URL, error) {
	invalid := errors.New("must be a canonical HTTPS origin without credentials, path, query, fragment, or default port")
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Opaque != "" || u.User != nil ||
		u.Path != "" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		strings.Contains(origin, "#") || origin != "https://"+u.Host {
		return nil, invalid
	}
	host := u.Hostname()
	authority := host
	if ip, err := netip.ParseAddr(host); err == nil {
		if ip.String() != host || ip.Zone() != "" || ip.IsUnspecified() || ip.IsLoopback() {
			return nil, invalid
		}
		if ip.Is6() {
			authority = "[" + host + "]"
		}
	} else if ValidateHostname(host) != nil || host != strings.ToLower(host) || host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return nil, invalid
	}
	if port := u.Port(); port != "" {
		parsed, err := strconv.ParseUint(port, 10, 16)
		if err != nil || parsed == 0 || parsed == 443 || strconv.FormatUint(parsed, 10) != port {
			return nil, invalid
		}
		authority = net.JoinHostPort(host, port)
	}
	if authority != u.Host {
		return nil, invalid
	}
	return u, nil
}
