package main

import (
	"context"
	"os"
	"time"
)

const processShutdownTimeout = 90 * time.Second

const shutdownDeadlineDiagnostic = "shutdown deadline exceeded; forcing process exit\n"

func startProcessShutdownWatchdog(ctx context.Context, timeout time.Duration) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-ctx.Done():
		case <-stop:
			return
		}

		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case <-stop:
			return
		case <-timer.C:
			if _, err := os.Stderr.WriteString(shutdownDeadlineDiagnostic); err != nil {
				os.Exit(1)
			}
			os.Exit(1)
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}
