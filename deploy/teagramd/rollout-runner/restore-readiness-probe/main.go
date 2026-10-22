package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"
)

const probeName = "restore-readiness-fixture.invalid"
const metadataFixture = "restore-metadata.fixture.invalid"
const ipv4Fixture = "restore-ipv4.fixture.invalid"

func main() {
	if len(os.Args) == 3 && os.Args[1] == "--wait-loopback" {
		port, err := parsePort(os.Args[2])
		if err != nil || !waitLoopback(port) {
			os.Exit(1)
		}
		if _, err := fmt.Fprintln(os.Stdout, `{"loopback_ready":true}`); err != nil {
			os.Exit(1)
		}
		return
	}
	if len(os.Args) != 3 {
		os.Exit(2)
	}
	port, err := parsePort(os.Args[2])
	if err != nil {
		os.Exit(2)
	}
	if !isolated(os.Args[1], port) {
		os.Exit(1)
	}
	if _, err := fmt.Fprintln(os.Stdout, `{"host_gateway_denied":true,"metadata_fixture_denied":true,"external_dns_denied":true,"ipv4_fixture_denied":true,"ipv6_fixture_denied":true,"loopback_only":true}`); err != nil {
		os.Exit(1)
	}
}

func parsePort(value string) (int, error) {
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return 0, errors.New("invalid port")
	}
	return port, nil
}

func isolated(gateway string, port int) bool {
	interfaces, err := net.Interfaces()
	if err != nil || !loopbackOnly(interfaces) {
		return false
	}
	fixture := net.JoinHostPort(gateway, strconv.Itoa(port))
	if canConnect(fixture) {
		return false
	}
	lookupCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	metadataAddresses, err := net.DefaultResolver.LookupHost(lookupCtx, metadataFixture)
	cancel()
	if err != nil || len(metadataAddresses) != 1 || metadataAddresses[0] != gateway ||
		canConnect(net.JoinHostPort(metadataFixture, strconv.Itoa(port))) {
		return false
	}
	lookupCtx, cancel = context.WithTimeout(context.Background(), time.Second)
	ipv4Addresses, err := net.DefaultResolver.LookupHost(lookupCtx, ipv4Fixture)
	cancel()
	if err != nil || len(ipv4Addresses) != 1 || ipv4Addresses[0] != gateway ||
		canConnect(net.JoinHostPort(ipv4Fixture, strconv.Itoa(port))) {
		return false
	}
	if canResolve(gateway, port) || !dnsRejected() {
		return false
	}
	if canConnect(net.JoinHostPort("::1", strconv.Itoa(port))) {
		return false
	}
	return true
}

func dnsRejected() bool {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	addresses, err := net.DefaultResolver.LookupHost(ctx, probeName)
	return err != nil && len(addresses) == 0
}

func waitLoopback(port int) bool {
	deadline := time.Now().Add(90 * time.Second)
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		connection, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
		cancel()
		if err == nil {
			if closeErr := connection.Close(); closeErr != nil {
				return false
			}
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

func loopbackOnly(interfaces []net.Interface) bool {
	if len(interfaces) != 1 || interfaces[0].Name != "lo" || interfaces[0].Flags&net.FlagLoopback == 0 {
		return false
	}
	addresses, err := interfaces[0].Addrs()
	if err != nil || len(addresses) == 0 {
		return false
	}
	for _, address := range addresses {
		prefix, ok := address.(*net.IPNet)
		if !ok || !prefix.IP.IsLoopback() {
			return false
		}
	}
	return true
}

func canConnect(address string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	connection, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		return false
	}
	if closeErr := connection.Close(); closeErr != nil {
		return true
	}
	return true
}

func canResolve(gateway string, port int) (reached bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	connection, err := (&net.Dialer{}).DialContext(ctx, "udp", net.JoinHostPort(gateway, strconv.Itoa(port)))
	if err != nil {
		return false
	}
	defer func() {
		if err := connection.Close(); err != nil {
			reached = true
		}
	}()
	query := dnsQuery()
	if _, err := connection.Write(query); err != nil {
		return false
	}
	if err := connection.SetReadDeadline(time.Now().Add(500 * time.Millisecond)); err != nil {
		return false
	}
	response := make([]byte, 512)
	n, err := connection.Read(response)
	if err != nil {
		return false
	}
	reached = n >= 12 && binary.BigEndian.Uint16(response[0:2]) == binary.BigEndian.Uint16(query[0:2]) &&
		binary.BigEndian.Uint16(response[2:4])&0x8000 != 0
	return reached
}

func dnsQuery() []byte {
	query := []byte{0x72, 0x64, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	for _, label := range []string{"restore-readiness-fixture", "invalid"} {
		length, ok := dnsLabelLength(label)
		if !ok {
			return nil
		}
		query = append(query, length)
		query = append(query, label...)
	}
	query = append(query, 0x00, 0x00, 0x01, 0x00, 0x01)
	return query
}

func dnsLabelLength(label string) (byte, bool) {
	if len(label) == 0 || len(label) > 63 {
		return 0, false
	}
	var length byte
	for range []byte(label) {
		length++
	}
	return length, true
}
