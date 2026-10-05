// Command blob-migrate copies the legacy local blob volume into the configured
// S3 namespace and verifies every object before reporting success.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/blobmigration"
	"github.com/teagramhq/teagram-server/internal/config"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		slog.New(slog.NewTextHandler(os.Stderr, nil)).Error("blob-migrate failed", "err", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("blob-migrate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	sourceDir := flags.String("source", "/source", "read-only root of the legacy blob volume")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *sourceDir == "" {
		return errors.New("usage: blob-migrate [--source /source]")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	local, err := blob.NewLocal(*sourceDir)
	if err != nil {
		return fmt.Errorf("open source blob volume: %w", err)
	}
	s3Config, err := config.LoadBlobS3Config()
	if err != nil {
		return err
	}
	remote, err := blob.NewS3(*s3Config)
	if err != nil {
		return fmt.Errorf("configure destination object store: %w", err)
	}
	if err := remote.Check(ctx); err != nil {
		return fmt.Errorf("check destination object store: %w", err)
	}
	if _, err := blobmigration.Migrate(ctx, local, remote, stdout); err != nil {
		return err
	}
	return nil
}
