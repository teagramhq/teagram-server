package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

func TestLinkselectorLaunchHelper(t *testing.T) {
	if os.Getenv("LINKSELECTOR_LAUNCH_HELPER") != "1" {
		return
	}

	flag.CommandLine = flag.NewFlagSet("linkselector", flag.ExitOnError)
	os.Args = []string{
		"linkselector",
		"-listen-address", "0.0.0.0",
		"-port", os.Getenv("LINKSELECTOR_TEST_PORT"),
		"-web-upstream", "http://web:8080",
		"-landing-upstream", "http://linklanding:8082",
	}
	main()
}

func TestLinkselectorLaunchWithoutServerUpstreamFailsClosed(t *testing.T) {
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve selector port: %v", err)
	}
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		if err := listener.Close(); err != nil {
			t.Errorf("release selector port: %v", err)
		}
		t.Fatalf("reserved selector address has type %T, want *net.TCPAddr", listener.Addr())
	}
	port := address.Port
	if err := listener.Close(); err != nil {
		t.Fatalf("release selector port: %v", err)
	}

	//nolint:gosec // Re-executes this test binary with fixed helper arguments.
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestLinkselectorLaunchHelper$")
	command.Env = []string{
		"LINKSELECTOR_LAUNCH_HELPER=1",
		"LINKSELECTOR_TEST_PORT=" + strconv.Itoa(port),
		"TG_INVITE_WEB_SIGNUP_URL=https://web.example.test/signup",
		"TG_INVITE_DESKTOP_URL=",
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatalf("start linkselector test process: %v", err)
	}
	processExited := make(chan struct{})
	var processErr error
	go func() {
		processErr = command.Wait()
		close(processExited)
	}()
	t.Cleanup(func() {
		select {
		case <-processExited:
			return
		default:
		}
		if err := command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("stop linkselector test process: %v", err)
		}
		<-processExited
	})

	client := &http.Client{Timeout: time.Second}
	apiURL := "http://127.0.0.1:" + strconv.Itoa(port) + "/apiws"
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, apiURL, nil)
		if err != nil {
			t.Fatalf("build fail-closed request: %v", err)
		}
		response, err := client.Do(request)
		if err == nil {
			if closeErr := response.Body.Close(); closeErr != nil {
				t.Errorf("close fail-closed response body: %v", closeErr)
			}
			if response.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("unconfigured /apiws status = %d, want %d", response.StatusCode, http.StatusServiceUnavailable)
			}
			return
		}
		select {
		case <-processExited:
			t.Fatalf("linkselector exited before listening: %v; stderr=%s", processErr, stderr.String())
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("linkselector did not start without -server-upstream")
}
