package main

import (
	"context"
	"os"
	"time"

	"golang.org/x/sys/unix"
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
			writeShutdownDeadlineDiagnostic()
			os.Exit(1)
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

func writeShutdownDeadlineDiagnostic() {
	// Raw nonblocking I/O avoids waiting on os.Stderr's writer lock or pipe backpressure.
	fd := os.Stderr.Fd()
	flags, err := unix.FcntlInt(fd, unix.F_GETFL, 0)
	if err != nil {
		return
	}
	if _, err := unix.FcntlInt(fd, unix.F_SETFL, flags|unix.O_NONBLOCK); err != nil {
		return
	}
	_, writeErr := unix.Write(int(fd), []byte(shutdownDeadlineDiagnostic))
	_, restoreErr := unix.FcntlInt(fd, unix.F_SETFL, flags)
	if writeErr != nil || restoreErr != nil {
		return
	}
}
