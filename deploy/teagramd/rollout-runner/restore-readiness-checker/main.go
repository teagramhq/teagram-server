package main

import (
	"context"
	"io"
	"os"
	"time"

	restoredrill "github.com/teagramhq/teagram-server/test/e2e/restore_drill"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	status := restoredrill.RunCLI(
		ctx,
		"/run/invocation/invocation.json",
		os.Stdout,
		io.Discard,
	)
	os.Exit(status)
}
