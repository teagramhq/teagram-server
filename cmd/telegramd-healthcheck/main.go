// Command telegramd-healthcheck runs the Compose readiness probe.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/teagramhq/teagram-server/internal/discovery"
)

const (
	listenAddr   = "127.0.0.1:2443"
	probeTimeout = 3 * time.Second
)

var proxyV2LocalHeader = []byte{
	0x0d, 0x0a, 0x0d, 0x0a, 0x00, 0x0d, 0x0a, 0x51,
	0x55, 0x49, 0x54, 0x0a, 0x20, 0x00, 0x00, 0x00,
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "MTProto readiness probe failed: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	return check(ctx, listenAddr, os.Getenv("TG_CLIENT_ADDR_TRUST") == "proxy-v2")
}

func check(ctx context.Context, address string, proxyV2 bool) (err error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		return fmt.Errorf("dial MTProto listener: %w", err)
	}
	defer func() {
		err = errors.Join(err, conn.Close())
	}()

	deadline := time.Now().Add(probeTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return fmt.Errorf("set MTProto probe deadline: %w", err)
	}

	var nonce [discovery.NonceSize]byte
	if _, err := io.ReadFull(rand.Reader, nonce[:]); err != nil {
		return fmt.Errorf("generate preflight nonce: %w", err)
	}
	request, err := discovery.BuildPreflightRequest(nonce[:])
	if err != nil {
		return fmt.Errorf("build preflight request: %w", err)
	}
	if proxyV2 {
		request = append(bytes.Clone(proxyV2LocalHeader), request...)
	}
	if _, err := io.Copy(conn, bytes.NewReader(request)); err != nil {
		return fmt.Errorf("write preflight request: %w", err)
	}
	tcpConn, ok := conn.(*net.TCPConn)
	if !ok {
		return errors.New("MTProto probe connection is not TCP")
	}
	if err := tcpConn.CloseWrite(); err != nil {
		return fmt.Errorf("finish preflight request: %w", err)
	}

	const maxResponse = discovery.ResponseMagicSize + discovery.NonceSize + 4 + 6 + discovery.MaxSPKILength
	response, err := io.ReadAll(io.LimitReader(conn, int64(maxResponse+1)))
	if err != nil {
		return fmt.Errorf("read preflight response: %w", err)
	}
	if len(response) > maxResponse {
		return errors.New("preflight response exceeds the maximum size")
	}
	if _, _, err := discovery.ParsePreflightResponse(response, nonce[:]); err != nil {
		return fmt.Errorf("validate preflight response: %w", err)
	}
	return nil
}
