package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/teagramhq/teagram-server/internal/discovery"
)

func TestCheckAcceptsReadyListener(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	for _, proxyV2 := range []bool{false, true} {
		t.Run(map[bool]string{false: "socket", true: "proxy-v2"}[proxyV2], func(t *testing.T) {
			var lc net.ListenConfig
			listener, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			t.Cleanup(func() {
				if err := listener.Close(); err != nil {
					t.Errorf("close listener: %v", err)
				}
			})
			serverErr := make(chan error, 1)
			go func() { serverErr <- servePreflightProbe(listener, proxyV2, &key.PublicKey) }()

			ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
			defer cancel()
			if err := check(ctx, listener.Addr().String(), proxyV2); err != nil {
				t.Fatalf("check: %v", err)
			}
			if err := <-serverErr; err != nil {
				t.Fatalf("probe server: %v", err)
			}
		})
	}
}

func servePreflightProbe(listener net.Listener, proxyV2 bool, key *rsa.PublicKey) (retErr error) {
	conn, err := listener.Accept()
	if err != nil {
		return err
	}
	defer func() {
		retErr = errors.Join(retErr, conn.Close())
	}()
	if err := conn.SetDeadline(time.Now().Add(probeTimeout)); err != nil {
		return err
	}
	if proxyV2 {
		header := make([]byte, len(proxyV2LocalHeader))
		if _, err := io.ReadFull(conn, header); err != nil {
			return err
		}
		if !bytes.Equal(header, proxyV2LocalHeader) {
			return errors.New("unexpected PROXY v2 header")
		}
	}
	request, err := io.ReadAll(conn)
	if err != nil {
		return err
	}
	if len(request) != discovery.RequestMagicSize+discovery.NonceSize || string(request[:discovery.RequestMagicSize]) != discovery.RequestMagic {
		return errors.New("unexpected preflight request")
	}
	response, err := discovery.BuildPreflightResponse(2, key, request[discovery.RequestMagicSize:])
	if err != nil {
		return err
	}
	if _, err := io.Copy(conn, bytes.NewReader(response)); err != nil {
		return err
	}
	return nil
}

func TestCheckRejectsUnreachableListener(t *testing.T) {
	var lc net.ListenConfig
	listener, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	if err := check(ctx, address, false); err == nil {
		t.Fatal("check succeeded for an unreachable listener")
	}
}
