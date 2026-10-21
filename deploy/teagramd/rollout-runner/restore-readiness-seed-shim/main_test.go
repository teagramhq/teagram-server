package main

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestProxyConnectionRelaysUnixSocketTrafficToScratchLoopback(t *testing.T) {
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("could not open scratch backend")
	}
	defer backend.Close()
	backendDone := make(chan error, 1)
	go func() {
		connection, acceptErr := backend.Accept()
		if acceptErr != nil {
			backendDone <- acceptErr
			return
		}
		defer connection.Close()
		request, readErr := io.ReadAll(connection)
		if readErr != nil {
			backendDone <- readErr
			return
		}
		if string(request) != "ping" {
			backendDone <- io.ErrUnexpectedEOF
			return
		}
		_, writeErr := connection.Write([]byte("pong"))
		backendDone <- writeErr
	}()

	directory, err := os.MkdirTemp("/tmp", "rr-")
	if err != nil {
		t.Fatal("could not create fixture directory")
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socketPath := filepath.Join(directory, "postgres.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatal("could not create fixture Unix socket: ", err)
	}
	defer listener.Close()
	proxyDone := make(chan struct{})
	go func() {
		connection, acceptErr := listener.AcceptUnix()
		if acceptErr == nil {
			proxyConnection(connection, backend.Addr().String())
		}
		close(proxyDone)
	}()

	client, err := net.DialTimeout("unix", socketPath, time.Second)
	if err != nil {
		t.Fatal("could not connect to fixture Unix socket")
	}
	unixClient := client.(*net.UnixConn)
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
	case <-proxyDone:
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
