package main

import (
	"net/netip"
	"testing"
)

func TestAllowedRemote(t *testing.T) {
	gateway := netip.MustParseAddr("172.22.0.1")
	for _, test := range []struct {
		name   string
		remote string
		want   bool
	}{
		{name: "loopback", remote: "127.0.0.1"},
		{name: "tailnet peer", remote: "100.124.236.10", want: true},
		{name: "docker host proxy", remote: "172.22.0.1", want: true},
		{name: "other default network container", remote: "172.22.0.8"},
		{name: "private LAN", remote: "192.168.1.10"},
		{name: "public IPv4", remote: "8.8.8.8"},
		{name: "IPv6", remote: "fd7a:115c:a1e0::1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := allowedRemote(netip.MustParseAddr(test.remote), gateway); got != test.want {
				t.Fatalf("allowedRemote(%s) = %t, want %t", test.remote, got, test.want)
			}
		})
	}
}

func TestDefaultGateway(t *testing.T) {
	contents := "Iface Destination Gateway Flags RefCnt Use Metric Mask\n" +
		"eth0 00000000 010016AC 0003 0 0 0 00000000\n" +
		"eth1 00000000 0100000A 0001 0 0 10 00000000\n"
	got, err := parseDefaultGateway(contents)
	if err != nil {
		t.Fatal(err)
	}
	if want := netip.MustParseAddr("172.22.0.1"); got != want {
		t.Fatalf("defaultGateway() = %s, want %s", got, want)
	}
}

func TestDefaultGatewayRejectsMissingRoute(t *testing.T) {
	if _, err := parseDefaultGateway("Iface Destination Gateway Flags\n"); err == nil {
		t.Fatal("defaultGateway() succeeded without a default route")
	}
}
