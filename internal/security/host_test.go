package security

import "testing"

func TestIsUnsafeHostBlocksLocalhostAliases(t *testing.T) {
	for _, host := range []string{"localhost", "LOCALHOST", "localhost.localdomain", " localhost "} {
		if !IsUnsafeHost(host) {
			t.Fatalf("IsUnsafeHost(%q) = false, want true", host)
		}
	}
}

func TestIsUnsafeHostBlocksLoopbackAndPrivateRanges(t *testing.T) {
	for _, host := range []string{
		"127.0.0.1", "127.255.255.254",
		"10.0.0.1", "192.168.1.1", "172.16.0.1",
		"169.254.0.1", "224.0.0.1",
		"0.0.0.0",
		"::1", "fe80::1", "fd00::1",
		// RFC 6598 CGNAT (100.64.0.0/10): not in net.IP.IsPrivate, but
		// frequently used to address ISP and platform-internal NAT ranges
		// that should never be reachable from server-side fetchers.
		"100.64.1.1", "100.127.255.254",
	} {
		if !IsUnsafeHost(host) {
			t.Fatalf("IsUnsafeHost(%q) = false, want true", host)
		}
	}
}

func TestIsUnsafeHostBlocksSpecialUseAndAddressTranslationRanges(t *testing.T) {
	for _, host := range []string{
		"0.0.0.1",            // RFC 1122 this-network space.
		"192.0.0.1",          // IETF protocol assignments.
		"192.0.2.1",          // TEST-NET-1.
		"198.18.0.1",         // Benchmarking range.
		"198.51.100.1",       // TEST-NET-2.
		"203.0.113.1",        // TEST-NET-3.
		"240.0.0.1",          // Reserved for future use.
		"255.255.255.254",    // Reserved high IPv4 space.
		"100::1",             // Discard-only IPv6 prefix.
		"2001:db8::1",        // IPv6 documentation prefix.
		"64:ff9b::a9fe:a9fe", // NAT64 translation of 169.254.169.254.
		"64:ff9b:1::1",       // Local-use NAT64 prefix.
		"2002:a9fe:a9fe::1",  // 6to4 embedding 169.254.169.254.
	} {
		if !IsUnsafeHost(host) {
			t.Errorf("IsUnsafeHost(%q) = false, want true", host)
		}
	}
}

func TestIsUnsafeHostAllowsPublicHosts(t *testing.T) {
	for _, host := range []string{
		"8.8.8.8",
		"example.com",
		"1.1.1.1",
		"2606:4700:4700::1111",
		"64:ff9b::808:808",  // NAT64 translation of public 8.8.8.8.
		"2002:0808:0808::1", // 6to4 embedding public 8.8.8.8.
	} {
		if IsUnsafeHost(host) {
			t.Fatalf("IsUnsafeHost(%q) = true, want false", host)
		}
	}
}

func TestIsUnsafeHostHandlesEmpty(t *testing.T) {
	if IsUnsafeHost("") {
		t.Fatal("IsUnsafeHost(\"\") = true, want false (empty input must not trigger blocking)")
	}
}

// TestIsUnsafeHostRejectsBroadcast pins the limited-broadcast guard
// added in Wave 11 L1. net.IP.IsMulticast() does not cover
// 255.255.255.255, so the explicit IPv4bcast check has to keep
// living in IsUnsafeHost; a regression here would let SSRF probes
// reach the local broadcast address.
func TestIsUnsafeHostRejectsBroadcast(t *testing.T) {
	if !IsUnsafeHost("255.255.255.255") {
		t.Fatal("IsUnsafeHost(\"255.255.255.255\") = false, want true (limited broadcast)")
	}
}
