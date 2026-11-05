// Package proxyv1 reads bounded PROXY protocol v1 connection prefaces.
package proxyv1

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

const (
	maxHeader     = 107
	HeaderTimeout = 3 * time.Second
)

// Read consumes an optional or required PROXY preface, preserving any bytes
// read past it. Only a trusted peer may supply a preface. The returned header
// aliases the read buffer and is valid only until the returned connection is read.
func Read(conn net.Conn, required, trusted bool) (net.Conn, []byte, error) {
	if err := conn.SetReadDeadline(time.Now().Add(HeaderTimeout)); err != nil {
		return nil, nil, fmt.Errorf("set preface deadline: %w", err)
	}
	reader := bufio.NewReaderSize(conn, maxHeader+1)
	prefix, err := reader.Peek(6)
	if err != nil {
		return nil, nil, fmt.Errorf("read preface: %w", err)
	}
	var header []byte
	var source net.Addr
	if bytes.Equal(prefix, []byte("PROXY ")) {
		if !trusted {
			return nil, nil, errors.New("untrusted PROXY preface")
		}
		header, err = reader.ReadSlice('\n')
		if err != nil {
			return nil, nil, fmt.Errorf("read PROXY preface: %w", err)
		}
		if len(header) > maxHeader || !bytes.HasSuffix(header, []byte("\r\n")) {
			return nil, nil, errors.New("invalid PROXY preface length or terminator")
		}
		source, err = parseHeader(string(header[:len(header)-2]))
		if err != nil {
			return nil, nil, err
		}
	} else if required {
		return nil, nil, errors.New("missing PROXY preface")
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		return nil, nil, fmt.Errorf("clear preface deadline: %w", err)
	}
	return &bufferedConn{Conn: conn, reader: reader, source: source}, header, nil
}

func parseHeader(line string) (net.Addr, error) {
	fields := strings.Split(line, " ")
	if len(fields) != 6 || fields[0] != "PROXY" || (fields[1] != "TCP4" && fields[1] != "TCP6") {
		return nil, errors.New("invalid PROXY preface fields")
	}
	var addresses [2]netip.Addr
	for i := range addresses {
		ip, err := netip.ParseAddr(fields[i+2])
		if err != nil || ip.Zone() != "" || (fields[1] == "TCP4" && !ip.Is4()) || (fields[1] == "TCP6" && !ip.Is6()) {
			return nil, errors.New("invalid PROXY address")
		}
		addresses[i] = ip
	}
	var ports [2]uint16
	for i := range ports {
		value := fields[i+4]
		if value == "" || strings.Trim(value, "0123456789") != "" {
			return nil, errors.New("invalid PROXY port")
		}
		port, err := strconv.ParseUint(value, 10, 16)
		if err != nil {
			return nil, errors.New("invalid PROXY port")
		}
		ports[i] = uint16(port)
	}
	return net.TCPAddrFromAddrPort(netip.AddrPortFrom(addresses[0].Unmap(), ports[0])), nil
}

type bufferedConn struct {
	net.Conn

	reader *bufio.Reader
	source net.Addr
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }
func (c *bufferedConn) RemoteAddr() net.Addr {
	if c.source != nil {
		return c.source
	}
	return c.Conn.RemoteAddr()
}
