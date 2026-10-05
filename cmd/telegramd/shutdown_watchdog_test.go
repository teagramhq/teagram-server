package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestProcessShutdownWatchdogSubprocess(t *testing.T) {
	mode := os.Getenv("TELEGRAMD_SHUTDOWN_WATCHDOG_CHILD")
	if mode != "" {
		ctx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stopSignals()
		stopWatchdog := startProcessShutdownWatchdog(ctx, 600*time.Millisecond)
		defer stopWatchdog()
		defer func() {
			if _, err := fmt.Fprintln(os.Stdout, "CLEANUP"); err != nil {
				panic(err)
			}
		}()
		if _, err := fmt.Fprintln(os.Stdout, "READY"); err != nil {
			panic(err)
		}
		<-ctx.Done()
		if _, err := fmt.Fprintln(os.Stdout, "SIGNALLED"); err != nil {
			panic(err)
		}
		switch mode {
		case "handler":
			handlerDone := make(chan struct{})
			go func() { <-make(chan struct{}); close(handlerDone) }()
			<-handlerDone
		case "cleanup":
			defer func() { <-make(chan struct{}) }()
			return
		case "signals":
			select {}
		case "blocked-stderr":
			select {}
		default:
			panic("unknown child mode")
		}
		return
	}

	for _, mode := range []string{"handler", "cleanup", "signals", "blocked-stderr"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			executable, err := os.Executable()
			if err != nil {
				t.Fatalf("executable path: %v", err)
			}
			// This test binary must run in a child so os.Exit cannot terminate the parent.
			cmd := exec.CommandContext(ctx, executable, "-test.run=^TestProcessShutdownWatchdogSubprocess$") //nolint:gosec // Test subprocess isolates the intentional os.Exit.
			cmd.Env = append(os.Environ(), "TELEGRAMD_SHUTDOWN_WATCHDOG_CHILD="+mode)
			stdout, stdoutWriter := io.Pipe()
			cmd.Stdout = stdoutWriter
			var stderr bytes.Buffer
			if mode == "blocked-stderr" {
				stderrReader, stderrWriter, err := os.Pipe()
				if err != nil {
					t.Fatalf("create stderr pipe: %v", err)
				}
				t.Cleanup(func() {
					if err := stderrReader.Close(); err != nil {
						t.Errorf("close stderr reader: %v", err)
					}
					if err := stderrWriter.Close(); err != nil {
						t.Errorf("close stderr writer: %v", err)
					}
				})
				fillUndrainedPipe(t, stderrWriter)
				cmd.Stderr = stderrWriter
			} else {
				cmd.Stderr = &stderr
			}
			if err := cmd.Start(); err != nil {
				if closeErr := stdout.Close(); closeErr != nil {
					t.Errorf("close stdout reader: %v", closeErr)
				}
				if closeErr := stdoutWriter.Close(); closeErr != nil {
					t.Errorf("close stdout writer: %v", closeErr)
				}
				t.Fatalf("start child: %v", err)
			}
			defer func() {
				if err := stdout.Close(); err != nil && !errors.Is(err, io.ErrClosedPipe) {
					t.Errorf("close stdout reader: %v", err)
				}
			}()
			done := make(chan struct {
				waitErr  error
				closeErr error
			}, 1)
			go func() {
				waitErr := cmd.Wait()
				closeErr := stdoutWriter.Close()
				done <- struct {
					waitErr  error
					closeErr error
				}{waitErr: waitErr, closeErr: closeErr}
			}()
			reader := bufio.NewReader(stdout)
			if line, err := reader.ReadString('\n'); err != nil || strings.TrimSpace(line) != "READY" {
				if killErr := cmd.Process.Kill(); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
					t.Errorf("kill child: %v", killErr)
				}
				t.Fatalf("child ready line = %q, %v", line, err)
			}
			started := time.Now()
			if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
				if killErr := cmd.Process.Kill(); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
					t.Errorf("kill child: %v", killErr)
				}
				t.Fatalf("signal child: %v", err)
			}
			if line, err := reader.ReadString('\n'); err != nil || strings.TrimSpace(line) != "SIGNALLED" {
				if killErr := cmd.Process.Kill(); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
					t.Errorf("kill child: %v", killErr)
				}
				t.Fatalf("child signal line = %q, %v", line, err)
			}
			if mode == "signals" {
				timer := time.NewTimer(300 * time.Millisecond)
				<-timer.C
				if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
					if killErr := cmd.Process.Kill(); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
						t.Errorf("kill child: %v", killErr)
					}
					t.Fatalf("repeat signal: %v", err)
				}
			}
			remaining, err := io.ReadAll(reader)
			if err != nil {
				t.Fatalf("read child output after exit: %v", err)
			}
			waitResult := <-done
			if waitResult.closeErr != nil && !errors.Is(waitResult.closeErr, io.ErrClosedPipe) {
				t.Fatalf("close stdout writer: %v", waitResult.closeErr)
			}
			err = waitResult.waitErr
			if ctx.Err() != nil {
				t.Fatalf("watchdog did not terminate child before test deadline: %s", stderr.String())
			}
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
				t.Fatalf("child exit = %v, stderr %q, want forced nonzero exit", err, stderr.String())
			}
			if mode != "blocked-stderr" && !strings.Contains(stderr.String(), "shutdown deadline exceeded; forcing process exit") {
				t.Fatalf("stderr %q does not contain the forced-shutdown diagnostic", stderr.String())
			}
			limit := 1200 * time.Millisecond
			if mode == "signals" {
				limit = 850 * time.Millisecond
			}
			if elapsed := time.Since(started); elapsed > limit {
				t.Fatalf("child exit took %s, want at most %s from first signal", elapsed, limit)
			}
			if strings.Contains(string(remaining), "CLEANUP") {
				t.Fatal("deferred cleanup sentinel ran before the forced exit")
			}
		})
	}
}

func fillUndrainedPipe(t *testing.T, pipe *os.File) {
	t.Helper()
	fd := int(pipe.Fd())
	if err := syscall.SetNonblock(fd, true); err != nil {
		t.Fatalf("make stderr pipe nonblocking: %v", err)
	}
	defer func() {
		if err := syscall.SetNonblock(fd, false); err != nil {
			t.Errorf("restore blocking stderr pipe: %v", err)
		}
	}()

	buffer := make([]byte, 64*1024)
	for {
		n, err := syscall.Write(fd, buffer)
		if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
			return
		}
		if err != nil {
			t.Fatalf("fill stderr pipe: %v", err)
		}
		if n == 0 {
			t.Fatal("fill stderr pipe: write returned no bytes")
		}
	}
}
