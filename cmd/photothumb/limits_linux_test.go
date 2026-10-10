//go:build linux

package main

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

const resourceLimitProbeEnv = "PHOTOTHUMB_RESOURCE_LIMIT_PROBE"

var cpuLimitProbeCounter uint64

func TestLoweredResourceLimitsTerminateChild(t *testing.T) {
	for _, probe := range []string{"cpu", "memory"} {
		t.Run(probe, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLoweredResourceLimitChild$") // #nosec G204,G702 -- fixed test selector on this test binary.
			cmd.Env = append(os.Environ(), resourceLimitProbeEnv+"="+probe)
			output, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("limited child did not terminate before timeout: %s", output)
			}
			if err == nil {
				t.Fatalf("child survived the lowered %s limit", probe)
			}
			if strings.Contains(string(output), "resource limit setup failed") {
				t.Fatalf("could not install lowered %s limit: %s", probe, output)
			}
		})
	}
}

func TestLoweredResourceLimitChild(t *testing.T) {
	switch os.Getenv(resourceLimitProbeEnv) {
	case "cpu":
		limit := syscall.Rlimit{Cur: 1, Max: 1}
		if err := syscall.Setrlimit(syscall.RLIMIT_CPU, &limit); err != nil {
			writeLimitProbeError("resource limit setup failed: " + err.Error())
			os.Exit(3)
		}
		for {
			atomic.AddUint64(&cpuLimitProbeCounter, 1)
		}
	case "memory":
		limit := syscall.Rlimit{Cur: 32 << 20, Max: 32 << 20}
		if err := syscall.Setrlimit(syscall.RLIMIT_DATA, &limit); err != nil {
			writeLimitProbeError("resource limit setup failed: " + err.Error())
			os.Exit(3)
		}
		allocation := make([]byte, 64<<20)
		for position := 0; position < len(allocation); position += 4096 {
			allocation[position] = 1
		}
		writeLimitProbeError("lowered memory limit did not stop allocation")
		os.Exit(0)
	}
}

func writeLimitProbeError(message string) {
	if _, err := os.Stderr.WriteString(message); err != nil {
		os.Exit(4)
	}
}
