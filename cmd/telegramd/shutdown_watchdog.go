package main

import (
	"context"
	"log/slog"
	"os"
	"time"
)

const processShutdownTimeout = 90 * time.Second

func startProcessShutdownWatchdog(ctx context.Context, timeout time.Duration, log *slog.Logger) func() {
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
			if log != nil {
				go log.Error("shutdown deadline exceeded; forcing process exit")
			}
			os.Exit(1)
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}
