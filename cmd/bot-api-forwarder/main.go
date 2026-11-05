package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	listenAddress   = "0.0.0.0:2448"
	upstreamAddress = "127.0.0.1:2447"
)

var tailnetIPv4 = netip.MustParsePrefix("100.64.0.0/10")

func main() {
	if len(os.Args) == 2 && os.Args[1] == "-healthcheck" {
		conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(context.Background(), "tcp4", upstreamAddress)
		if err != nil {
			slog.Error("health check failed", "err", err)
			os.Exit(1)
		}
		if err := conn.Close(); err != nil {
			slog.Error("health check close failed", "err", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) != 1 {
		slog.Error("unexpected command line")
		os.Exit(64)
	}

	gateway, err := defaultGateway()
	if err != nil {
		slog.Error("cannot determine Docker bridge gateway", "err", err)
		os.Exit(1)
	}
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(context.Background(), "tcp4", listenAddress) // #nosec G102 -- host publish is limited to the tailnet IP and this listener rejects other sources.
	if err != nil {
		slog.Error("cannot listen", "err", err)
		os.Exit(1)
	}
	defer closeQuietly(listener)
	slog.Info("Bot API forwarder ready", "listen", listenAddress, "upstream", upstreamAddress)

	for {
		client, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			slog.Error("accept failed", "err", err)
			continue
		}
		remote, ok := client.RemoteAddr().(*net.TCPAddr)
		var remoteIP netip.Addr
		validIP := false
		if ok && remote != nil {
			remoteIP, validIP = netip.AddrFromSlice(remote.IP)
		}
		if !ok || !validIP || !allowedRemote(remoteIP, gateway) {
			if remote != nil {
				slog.Warn("rejected connection", "remote_ip", remote.IP.String())
			}
			closeQuietly(client)
			continue
		}
		go forward(client)
	}
}

func allowedRemote(remote, gateway netip.Addr) bool {
	remote = remote.Unmap()
	return tailnetIPv4.Contains(remote) || remote == gateway.Unmap()
}

func defaultGateway() (netip.Addr, error) {
	contents, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return netip.Addr{}, fmt.Errorf("read route table: %w", err)
	}
	return parseDefaultGateway(string(contents))
}

func parseDefaultGateway(contents string) (netip.Addr, error) {
	lines := strings.Split(contents, "\n")
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[1] != "00000000" {
			continue
		}
		flags, err := strconv.ParseUint(fields[3], 16, 16)
		if err != nil || flags&0x1 == 0 || flags&0x2 == 0 {
			continue
		}
		gateway, err := strconv.ParseUint(fields[2], 16, 32)
		if err != nil {
			return netip.Addr{}, errors.New("route table has an invalid default gateway")
		}
		var bytes [4]byte
		binary.LittleEndian.PutUint32(bytes[:], uint32(gateway))
		return netip.AddrFrom4(bytes), nil
	}
	return netip.Addr{}, errors.New("route table has no active IPv4 default gateway")
}

func forward(client net.Conn) {
	defer closeQuietly(client)
	upstream, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(context.Background(), "tcp4", upstreamAddress)
	if err != nil {
		slog.Warn("Bot API upstream unavailable")
		return
	}
	defer closeQuietly(upstream)

	copyDone := make(chan struct{})
	go func() {
		if _, err := io.Copy(upstream, client); err != nil && !errors.Is(err, net.ErrClosed) {
			slog.Debug("client to Bot API copy ended", "err", err)
		}
		if conn, ok := upstream.(*net.TCPConn); ok {
			if err := conn.CloseWrite(); err != nil && !errors.Is(err, net.ErrClosed) {
				slog.Debug("Bot API write half-close failed", "err", err)
			}
		}
		close(copyDone)
	}()
	if _, err := io.Copy(client, upstream); err != nil && !errors.Is(err, net.ErrClosed) {
		slog.Debug("Bot API to client copy ended", "err", err)
	}
	if conn, ok := client.(*net.TCPConn); ok {
		if err := conn.CloseWrite(); err != nil && !errors.Is(err, net.ErrClosed) {
			slog.Debug("client write half-close failed", "err", err)
		}
	}
	closeQuietly(client)
	closeQuietly(upstream)
	<-copyDone
}

func closeQuietly(closer io.Closer) {
	if err := closer.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		slog.Debug("connection close failed", "err", err)
	}
}
