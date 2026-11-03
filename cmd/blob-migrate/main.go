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
		logFailure(os.Args[1:], err, os.Stderr)
		os.Exit(1)
	}
}

func logFailure(args []string, err error, stderr io.Writer) {
	logger := slog.New(slog.NewTextHandler(stderr, nil))
	if hasPrivateManifestOutput(args) {
		logger.Error("blob-migrate private evidence operation failed")
		return
	}
	logger.Error("blob-migrate failed", "err", err)
}

func hasPrivateManifestOutput(args []string) bool {
	for index, arg := range args {
		if arg == "--manifest=stdout" || arg == "--manifest=discard" ||
			(arg == "--manifest" && index+1 < len(args) && (args[index+1] == "stdout" || args[index+1] == "discard")) {
			return true
		}
	}
	return false
}

func run(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("blob-migrate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	direction := flags.String("direction", "local-to-s3", "operation: local-to-s3, s3-to-local, local-census, or s3-census")
	sourceDir := flags.String("source", "/source", "read-only root of the legacy blob volume")
	destinationDir := flags.String("destination", "/destination", "writable root of the local blob volume")
	manifest := flags.String("manifest", "", "write a private manifest to stdout or discard it after verification")
	encrypt := flags.Bool("encrypt", false, "encrypt local-to-S3 objects with the configured current media key")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: blob-migrate [--direction local-to-s3 --source /source [--manifest stdout|discard] [--encrypt] | --direction s3-to-local --destination /destination [--manifest stdout|discard] | --direction local-census --manifest stdout | --direction s3-census --manifest stdout]")
	}
	if (*direction == "local-to-s3" || *direction == "local-census") && *sourceDir == "" {
		return errors.New("blob migration source and destination paths must be non-empty")
	}
	if *direction == "s3-to-local" && *destinationDir == "" {
		return errors.New("blob migration source and destination paths must be non-empty")
	}
	if *direction != "local-to-s3" && *direction != "s3-to-local" &&
		*direction != "local-census" && *direction != "s3-census" {
		return fmt.Errorf("unsupported blob migration direction %q", *direction)
	}
	if *manifest != "" && *manifest != "stdout" && *manifest != "discard" {
		return errors.New("--manifest must be stdout or discard when provided")
	}
	if (*direction == "local-census" || *direction == "s3-census") && *manifest != "stdout" {
		return errors.New("census operations require --manifest stdout")
	}
	if *encrypt && *direction != "local-to-s3" {
		return errors.New("--encrypt is supported only for local-to-s3")
	}
	if *encrypt && *manifest == "" {
		return errors.New("--encrypt requires --manifest discard or stdout for private evidence handling")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *direction == "local-census" {
		local, err := blob.NewLocal(*sourceDir)
		if err != nil {
			return fmt.Errorf("open source blob volume: %w", err)
		}
		summary, err := blobmigration.Census(ctx, local, stdout)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stderr, "blob_census=pass objects=%d bytes=%d manifest_sha256=%s\n", summary.Objects, summary.Bytes, summary.ManifestSHA256)
		return err
	}

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
		manifestWriter := io.Discard
		if *manifest == "stdout" {
			manifestWriter = stdout
		}
		if *encrypt {
			cfg, err := config.LoadReadOnly(slog.New(slog.DiscardHandler))
			if err != nil {
				return fmt.Errorf("load media encryption configuration: %w", err)
			}
			if cfg.MediaKeys == nil {
				return errors.New("encrypted local-to-S3 migration requires TG_MEDIA_KEY_DIR and a current media key")
			}
			encrypted, encryptedTree, err := encryptedDestination(remote, cfg.MediaKeys)
			if err != nil {
				return err
			}
			summary, err := blobmigration.MigrateEncryptedWithManifest(ctx, local, encrypted, encryptedTree, io.Discard, manifestWriter)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(stderr, "blob_copy=pass mode=encrypted objects=%d bytes=%d source_manifest_sha256=%s destination_manifest_sha256=%s\n", summary.Objects, summary.Bytes, summary.SourceManifestSHA256, summary.DestinationManifestSHA256)
			return err
		}
		if *manifest != "" {
			summary, err := blobmigration.MigrateWithManifest(ctx, local, remote, io.Discard, manifestWriter)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(stderr, "blob_copy=pass objects=%d bytes=%d source_manifest_sha256=%s destination_manifest_sha256=%s\n", summary.Objects, summary.Bytes, summary.SourceManifestSHA256, summary.DestinationManifestSHA256)
			return err
		}
		if _, err := blobmigration.Migrate(ctx, local, remote, stdout); err != nil {
			return err
		}
	case "s3-to-local":
		local, err := blob.NewLocal(*destinationDir)
		if err != nil {
			return fmt.Errorf("open destination blob volume: %w", err)
		}
		if *manifest != "" {
			manifestWriter := io.Discard
			if *manifest == "stdout" {
				manifestWriter = stdout
			}
			summary, err := blobmigration.RestoreWithManifest(ctx, remote, local, io.Discard, manifestWriter)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(stderr, "blob_restore=pass objects=%d bytes=%d source_manifest_sha256=%s destination_manifest_sha256=%s\n", summary.Objects, summary.Bytes, summary.SourceManifestSHA256, summary.DestinationManifestSHA256)
			return err
		}
		if _, err := blobmigration.Restore(ctx, remote, local, stdout); err != nil {
			return err
		}
	case "s3-census":
		summary, err := blobmigration.Census(ctx, remote, stdout)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stderr, "blob_census=pass objects=%d bytes=%d manifest_sha256=%s\n", summary.Objects, summary.Bytes, summary.ManifestSHA256)
		return err
	}
	return nil
}

// encryptedDestination returns the encrypted writer for S3 objects and the tree
// it verifies against. Writes go to the encrypted namespace the serving store
// reads, so an envelope never replaces the accepted plaintext object at the
// same logical key.
func encryptedDestination(remote blob.Store, keys *blob.KeyRing) (*blob.Encrypted, *blob.PrefixedStore, error) {
	prefixed, err := blob.NewPrefixedStore(remote, blob.EncryptedObjectsPrefix)
	if err != nil {
		return nil, nil, fmt.Errorf("construct encrypted object namespace: %w", err)
	}
	encrypted, err := blob.NewEncrypted(prefixed, keys, blob.DefaultChunkSize)
	if err != nil {
		return nil, nil, fmt.Errorf("construct encrypted S3 destination: %w", err)
	}
	return encrypted, prefixed, nil
}
