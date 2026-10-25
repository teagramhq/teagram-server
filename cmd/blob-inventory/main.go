// Command blob-inventory reports the representation and key version of every
// media object referenced by Postgres or present without a database reference.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/blobinventory"
	"github.com/teagramhq/teagram-server/internal/blobmode"
	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/store"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
		logger.Error("blob-inventory failed", "err", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) (err error) {
	flags := flag.NewFlagSet("blob-inventory", flag.ContinueOnError)
	flags.SetOutput(stderr)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: blob-inventory")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	quietLog := slog.New(slog.DiscardHandler)
	cfg, err := config.LoadReadOnly(quietLog)
	if err != nil {
		return err
	}
	authority, err := blobmode.Read(blobmode.DefaultDirectory, blobmode.EffectiveConfig{
		BlobDir: cfg.BlobDir,
		BlobS3:  cfg.BlobS3,
	})
	if err != nil {
		return err
	}

	blobs, closeBlobs, err := openBlobStore(ctx, cfg, quietLog)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := closeBlobs(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close blob store: %w", closeErr))
		}
	}()

	st, err := store.Open(ctx, cfg.PostgresDSN, cfg.AuthKeyEncKey,
		store.WithLogger(quietLog),
		store.WithPoolConfig(cfg.PostgresPool),
		store.WithStatementTimeout(cfg.StatementTimeout),
		store.WithoutBlobStore(),
	)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() {
		if closeErr := st.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close store: %w", closeErr))
		}
	}()

	storeRefs, err := st.BlobInventoryReferences(ctx)
	if err != nil {
		return err
	}
	refs := make([]blobinventory.Reference, len(storeRefs))
	for i, ref := range storeRefs {
		refs[i] = blobinventory.Reference{
			Key:          ref.Key,
			Kind:         ref.Kind,
			ID:           ref.ID,
			LogicalBytes: ref.LogicalBytes,
		}
	}
	inventoryBlobs, err := inventoryStoreWithAuthority(blobs, authority)
	if err != nil {
		return err
	}
	report, err := blobinventory.Scan(ctx, inventoryBlobs, cfg.MediaKeys, refs)
	if err != nil {
		return err
	}
	if err := writeReport(stdout, stderr, report); err != nil {
		return fmt.Errorf("write inventory report: %w", err)
	}
	return nil
}

func inventoryStoreWithAuthority(blobs blob.Store, authority blobmode.Authority) (blob.Store, error) {
	if authority.Present && authority.Outcome == blobmode.OutcomeEncrypted {
		return blob.NewPrefixedStore(blobs, blob.EncryptedObjectsPrefix)
	}
	return blobs, nil
}

func openBlobStore(ctx context.Context, cfg config.Config, log *slog.Logger) (blob.Store, func() error, error) {
	if cfg.BlobS3 == nil {
		local, err := blob.OpenLocal(cfg.BlobDir)
		if err != nil {
			return nil, nil, fmt.Errorf("open blob directory: %w", err)
		}
		return local, local.Close, nil
	}
	s3Config := *cfg.BlobS3
	s3Config.Logger = log
	remote, err := blob.NewS3(s3Config)
	if err != nil {
		return nil, nil, fmt.Errorf("configure object store: %w", err)
	}
	if err := remote.Check(ctx); err != nil {
		return nil, nil, fmt.Errorf("check object store: %w", err)
	}
	return remote, func() error { return nil }, nil
}

func writeReport(stdout, stderr io.Writer, report blobinventory.Report) error {
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		return fmt.Errorf("encode JSON report: %w", err)
	}
	if _, err := fmt.Fprintln(stderr, report.Summary); err != nil {
		return fmt.Errorf("write summary: %w", err)
	}
	return nil
}
