package main

import (
	"context"
	"os"

	restoredrill "github.com/teagramhq/teagram-server/test/e2e/restore_drill"
)

func main() {
	code := restoredrill.RunCLI(
		context.Background(),
		os.Getenv("TEAGRAM_RESTORE_DRILL_INVOCATION_FILE"),
		restoredrill.DefaultAdminBinary,
		os.Stdout,
		os.Stderr,
	)
	os.Exit(code)
}
