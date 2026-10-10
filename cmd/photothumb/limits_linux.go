//go:build linux

package main

import (
	"fmt"
	"os"
	"syscall"
)

func setWorkerLimits() error {
	if err := os.WriteFile("/proc/self/oom_score_adj", []byte("1000"), 0); err != nil {
		return fmt.Errorf("raise worker OOM preference: %w", err)
	}
	limits := []struct {
		resource int
		soft     uint64
		hard     uint64
	}{
		{resource: syscall.RLIMIT_DATA, soft: 320 << 20, hard: 320 << 20},
		{resource: syscall.RLIMIT_CPU, soft: 2, hard: 3},
		{resource: syscall.RLIMIT_FSIZE, soft: 0, hard: 0},
		{resource: syscall.RLIMIT_NOFILE, soft: 8, hard: 8},
	}
	for _, limit := range limits {
		resourceLimit := syscall.Rlimit{Cur: limit.soft, Max: limit.hard}
		if err := syscall.Setrlimit(limit.resource, &resourceLimit); err != nil {
			return fmt.Errorf("set worker resource limit %d: %w", limit.resource, err)
		}
	}
	return nil
}
