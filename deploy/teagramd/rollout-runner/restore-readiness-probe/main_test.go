package main

import (
	"bytes"
	"net"
	"testing"
)

func TestLoopbackOnlyRequiresNoOtherInterfaceOrAddress(t *testing.T) {
	loopback, err := net.InterfaceByName("lo")
	if err != nil {
		t.Skip("loopback interface is unavailable")
	}
	if !loopbackOnly([]net.Interface{*loopback}) {
		t.Fatal("loopback-only namespace was rejected")
	}
	if loopbackOnly([]net.Interface{*loopback, {Index: 99, Name: "eth0"}}) {
		t.Fatal("namespace with a second interface was accepted")
	}
	invalid := *loopback
	invalid.Flags &^= net.FlagLoopback
	if loopbackOnly([]net.Interface{invalid}) {
		t.Fatal("non-loopback interface was accepted")
	}
}

func TestDNSFixtureQueryIsBoundedAndSynthetic(t *testing.T) {
	query := dnsQuery()
	if len(query) > 128 || !bytes.Contains(query, []byte("restore-readiness-fixture")) ||
		!bytes.Contains(query, []byte("invalid")) {
		t.Fatal("DNS probe query is not the expected synthetic fixture")
	}
}
