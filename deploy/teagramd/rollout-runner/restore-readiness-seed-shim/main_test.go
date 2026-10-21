package main

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestProxyConnectionRelaysUnixSocketTrafficToScratchLoopback(t *testing.T) {
	t.Setenv("TMPDIR", "/tmp")
	backend, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("could not open scratch backend")
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Errorf("could not close scratch backend: %v", err)
		}
	})
	backendDone := make(chan error, 1)
	go func() {
		connection, acceptErr := backend.Accept()
		if acceptErr != nil {
			backendDone <- acceptErr
			return
		}
		var backendErr error
		request, readErr := io.ReadAll(connection)
		if readErr != nil {
			backendErr = readErr
		}
		if backendErr == nil && string(request) != "ping" {
			backendErr = io.ErrUnexpectedEOF
		}
		if backendErr == nil {
			_, backendErr = connection.Write([]byte("pong"))
		}
		backendDone <- errors.Join(backendErr, connection.Close())
	}()

	directory := t.TempDir()
	socketPath := filepath.Join(directory, "postgres.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatal("could not create fixture Unix socket: ", err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Errorf("could not close fixture Unix socket: %v", err)
		}
	})
	proxyDone := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.AcceptUnix()
		if acceptErr == nil {
			if err := proxyConnection(context.Background(), connection, backend.Addr().String()); err != nil {
				proxyDone <- err
				return
			}
			proxyDone <- nil
		}
	}()

	client, err := (&net.Dialer{Timeout: time.Second}).DialContext(context.Background(), "unix", socketPath)
	if err != nil {
		t.Fatal("could not connect to fixture Unix socket")
	}
	unixClient, ok := client.(*net.UnixConn)
	if !ok {
		t.Fatal("fixture client is not a Unix socket")
	}
	if _, err := unixClient.Write([]byte("ping")); err != nil {
		t.Fatal("could not write through proxy")
	}
	if err := unixClient.CloseWrite(); err != nil {
		t.Fatal("could not half-close fixture client")
	}
	response, err := io.ReadAll(unixClient)
	if err != nil || string(response) != "pong" {
		t.Fatalf("proxy response = %q, %v; want pong", response, err)
	}
	if err := unixClient.Close(); err != nil {
		t.Fatal("could not close fixture client")
	}
	select {
	case err := <-backendDone:
		if err != nil {
			t.Fatal("scratch backend relay failed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("scratch backend did not finish")
	}
	select {
	case err := <-proxyDone:
		if err != nil {
			t.Fatal("Unix socket proxy failed: ", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Unix socket proxy did not finish")
	}
}

func TestRemoveOwnedSocketRejectsReplacement(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "owned.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal("could not create fixture Unix socket")
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal("could not inspect fixture Unix socket")
	}
	if err := listener.Close(); err != nil {
		t.Fatal("could not close fixture Unix socket")
	}
	if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
		t.Fatal("could not replace fixture socket")
	}
	if err := removeOwnedSocket(path, info); err == nil {
		t.Fatal("replacement at the owned socket path was removed")
	}
}

func TestRuntimeEnvironmentAllowsOnlyDockerDefaults(t *testing.T) {
	if !runtimeDefaultsOnly([]string{"PATH=/usr/bin:/bin", "HOSTNAME=abc", "HOME=/root"}) {
		t.Fatal("rejected the environment Docker injects into every container")
	}
	if !runtimeDefaultsOnly(nil) {
		t.Fatal("rejected an empty environment")
	}
	for _, entries := range [][]string{{"TG_POSTGRES_DSN=x"}, {"PATH=/bin", "PGPASSWORD=x"}, {"PATH"}} {
		if runtimeDefaultsOnly(entries) {
			t.Fatalf("accepted unreviewed environment %q", entries)
		}
	}
}
