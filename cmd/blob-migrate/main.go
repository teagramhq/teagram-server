// Command blob-migrate copies media between the local blob volume and the
// configured S3 namespace, verifying every object before reporting success.
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
	direction := flags.String("direction", "local-to-s3", "copy direction: local-to-s3 or s3-to-local")
	sourceDir := flags.String("source", "/source", "read-only root of the legacy blob volume")
	destinationDir := flags.String("destination", "/destination", "writable root of the local blob volume")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: blob-migrate [--direction local-to-s3 --source /source | --direction s3-to-local --destination /destination]")
	}
	if (*direction == "local-to-s3" && *sourceDir == "") || (*direction == "s3-to-local" && *destinationDir == "") {
		return errors.New("blob migration source and destination paths must be non-empty")
	}
	if *direction != "local-to-s3" && *direction != "s3-to-local" {
		return fmt.Errorf("unsupported blob migration direction %q", *direction)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	s3Config, err := config.LoadBlobS3Config()
	if err != nil {
		return err
	}
	remote, err := blob.NewS3(*s3Config)
	if err != nil {
		return fmt.Errorf("configure object store: %w", err)
	}
	if err := remote.Check(ctx); err != nil {
		return fmt.Errorf("check object store: %w", err)
	}
	switch *direction {
	case "local-to-s3":
		local, err := blob.NewLocal(*sourceDir)
		if err != nil {
			return fmt.Errorf("open source blob volume: %w", err)
		}
		if _, err := blobmigration.Migrate(ctx, local, remote, stdout); err != nil {
			return err
		}
	case "s3-to-local":
		local, err := blob.NewLocal(*destinationDir)
		if err != nil {
			return fmt.Errorf("open destination blob volume: %w", err)
		}
		if _, err := blobmigration.Restore(ctx, remote, local, stdout); err != nil {
			return err
		}
	}
	return nil
}
