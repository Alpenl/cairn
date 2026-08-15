// Package security holds shared, framework-free helpers used by both the
// fetch path and the submission validator. Centralising the unsafe-host
// rules here keeps the SSRF allow/deny list in one canonical place; before
// this package existed, internal/fetcher and internal/service each carried
// a near-identical copy that could (and did) drift apart.
package security

import (
	"net/netip"
	"strings"
)

var (
	// Go's Addr predicates intentionally describe address properties, not the
	// full IANA special-purpose registries. Server-side fetches must also reject
	// ranges that are non-forwardable, documentation-only, benchmarking-only,
	// or reserved for future use.
	specialUsePrefixes = []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"),
		netip.MustParsePrefix("100.64.0.0/10"),
		netip.MustParsePrefix("192.0.0.0/24"),
		netip.MustParsePrefix("192.0.2.0/24"),
		netip.MustParsePrefix("192.88.99.0/24"),
		netip.MustParsePrefix("198.18.0.0/15"),
		netip.MustParsePrefix("198.51.100.0/24"),
		netip.MustParsePrefix("203.0.113.0/24"),
		netip.MustParsePrefix("240.0.0.0/4"),
		netip.MustParsePrefix("64:ff9b:1::/48"),
		netip.MustParsePrefix("100::/64"),
		netip.MustParsePrefix("2001::/23"),
		netip.MustParsePrefix("2001:db8::/32"),
		netip.MustParsePrefix("3fff::/20"),
	}
	nat64WellKnownPrefix = netip.MustParsePrefix("64:ff9b::/96")
	sixToFourPrefix      = netip.MustParsePrefix("2002::/16")
)

// IsUnsafeHost reports whether host should be rejected before any outbound
// HTTP request is made. The rule set covers loopback aliases, RFC1918
// private ranges, link-local space, multicast, the unspecified address,
// and the literal "localhost"/"localhost.localdomain" labels. IPv6 forms
// are normalised by net.ParseIP, so e.g. "::1" and "[::1]" (after the
// caller strips brackets) are both rejected.
//
// Inputs that fail to parse as an IP fall through to false unless they
// match the literal localhost labels — DNS-resolved hosts must be checked
// separately by resolving the name and re-passing each address string.
func IsUnsafeHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	switch host {
	case "localhost", "localhost.localdomain":
		return true
	}

	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	return isUnsafeAddr(addr.Unmap())
}

func isUnsafeAddr(addr netip.Addr) bool {
	if !addr.IsValid() ||
		addr.IsLoopback() ||
		addr.IsPrivate() ||
		addr.IsLinkLocalUnicast() ||
		addr.IsMulticast() ||
		addr.IsUnspecified() {
		return true
	}
	for _, prefix := range specialUsePrefixes {
		if prefix.Contains(addr) {
			return true
		}
	}

	// NAT64 and 6to4 encode an IPv4 destination inside an otherwise public
	// IPv6 address. Validate the embedded destination as well, otherwise a
	// provider can translate a globally-looking IPv6 literal into link-local
	// metadata or RFC1918 space after our check.
	if nat64WellKnownPrefix.Contains(addr) {
		bits := addr.As16()
		return isUnsafeAddr(netip.AddrFrom4([4]byte{bits[12], bits[13], bits[14], bits[15]}))
	}
	if sixToFourPrefix.Contains(addr) {
		bits := addr.As16()
		return isUnsafeAddr(netip.AddrFrom4([4]byte{bits[2], bits[3], bits[4], bits[5]}))
	}
	return false
}
