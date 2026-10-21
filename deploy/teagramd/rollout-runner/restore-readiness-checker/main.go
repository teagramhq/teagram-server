package main

import (
	"context"
	"io"
	"os"
	"time"

	restoredrill "github.com/teagramhq/teagram-server/test/e2e/restore_drill"
)

func main() {
	os.Exit(run())
}

func run() int {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return restoredrill.RunCLI(
		ctx,
		"/run/invocation/invocation.json",
		os.Stdout,
		io.Discard,
	)
}
