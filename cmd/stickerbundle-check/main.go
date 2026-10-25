// Command stickerbundle-check validates a generated sticker catalogue bundle.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/teagramhq/teagram-server/internal/stickerbundle"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	if len(args) != 1 || args[0] == "" {
		return errors.New("usage: stickerbundle-check <bundle-directory>")
	}
	bundle, err := stickerbundle.Load(args[0])
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "validated %d sticker packs\n", len(bundle.Packs)); err != nil {
		return fmt.Errorf("write validation result: %w", err)
	}
	return nil
}
