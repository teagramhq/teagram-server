// Command blob-key-rotate rewraps every referenced media envelope under the
// current media key and reports whether older versions are retirable.
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
	"github.com/teagramhq/teagram-server/internal/blobmigration"
	"github.com/teagramhq/teagram-server/internal/blobmode"
	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/store"
)

const defaultProgressPath = "/run/teagramd/reports/blob-key-rotation.jsonl"

type commandResult struct {
	Mode            string               `json:"mode"`
	KeyVersion      uint32               `json:"key_version"`
	Objects         int                  `json:"objects"`
	Rotated         int                  `json:"rotated"`
	AlreadyVerified int                  `json:"already_verified"`
	LogicalBytes    int64                `json:"logical_bytes"`
	ProgressSHA256  string               `json:"progress_sha256"`
	Inventory       blobinventory.Report `json:"inventory"`
}

type inventoryReferenceSource interface {
	BlobInventoryReferences(context.Context) ([]store.BlobInventoryReference, error)
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
		logger.Error("blob-key-rotate failed", "err", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) (retErr error) {
	flags := flag.NewFlagSet("blob-key-rotate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	progress := flags.String("progress", defaultProgressPath, "persistent private JSONL progress journal")
	authorityDir := flags.String("authority-dir", blobmode.DefaultDirectory, "durable blob-mode authority directory")
	targetVersion := flags.Uint64("target-version", 0, "expected current key version (defaults to TG_MEDIA_KEY_CURRENT)")
	rewrite := flags.Bool("rewrite", false, "assign each object a fresh data key and rewrite its chunks")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: blob-key-rotate [--target-version N] [--rewrite] [--progress PATH]")
	}
	if *progress == "" || *authorityDir == "" {
		return errors.New("--progress and --authority-dir must be non-empty")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	quietLog := slog.New(slog.DiscardHandler)
	cfg, err := config.LoadReadOnly(quietLog)
	if err != nil {
		return err
	}
	if cfg.MediaKeys == nil {
		return errors.New("media key rotation requires TG_MEDIA_KEY_DIR and TG_MEDIA_KEY_CURRENT")
	}
	if err := requireCurrentTarget(cfg.MediaKeys.Current(), *targetVersion); err != nil {
		return err
	}
	authority, err := blobmode.Read(*authorityDir, blobmode.EffectiveConfig{
		BlobDir: cfg.BlobDir,
		BlobS3:  cfg.BlobS3,
	})
	if err != nil {
		return err
	}
	if err := requireEncryptedAuthority(authority); err != nil {
		return err
	}

	backend, closeBackend, err := openEncryptedStore(ctx, cfg, quietLog)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := closeBackend(); closeErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close blob store: %w", closeErr))
		}
	}()

	database, err := store.Open(ctx, cfg.PostgresDSN, cfg.AuthKeyEncKey,
		store.WithLogger(quietLog),
		store.WithPoolConfig(cfg.PostgresPool),
		store.WithStatementTimeout(cfg.StatementTimeout),
		store.WithoutBlobStore(),
	)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() {
		if closeErr := database.Close(); closeErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close store: %w", closeErr))
		}
	}()

	references, err := readReferences(ctx, database)
	if err != nil {
		return err
	}
	rotation, err := blobmigration.RotateKeyVersion(ctx, backend, cfg.MediaKeys, references,
		blobmigration.RotationOptions{ProgressPath: *progress, Rewrite: *rewrite})
	if err != nil {
		return err
	}
	inventory, err := scanFinalInventory(ctx, database, backend, cfg.MediaKeys)
	if err != nil {
		return err
	}
	if inventory.Classes.Missing.Count != 0 || inventory.Classes.NoValidEnvelope.Count != 0 {
		return fmt.Errorf("media key rotation: final referenced inventory is incomplete: %s", inventory.Summary)
	}
	result := commandResult{
		Mode: rotation.Mode, KeyVersion: rotation.KeyVersion,
		Objects: rotation.Objects, Rotated: rotation.Rotated,
		AlreadyVerified: rotation.AlreadyVerified, LogicalBytes: rotation.LogicalBytes,
		ProgressSHA256: rotation.ProgressSHA256, Inventory: inventory,
	}
	if err := json.NewEncoder(stdout).Encode(result); err != nil {
		return fmt.Errorf("write rotation summary: %w", err)
	}
	if _, err := fmt.Fprintln(stderr, inventory.Summary); err != nil {
		return fmt.Errorf("write inventory summary: %w", err)
	}
	return nil
}

func requireEncryptedAuthority(authority blobmode.Authority) error {
	if !authority.Present || authority.Outcome != blobmode.OutcomeEncrypted {
		return errors.New("media key rotation requires encrypted blob-mode authority")
	}
	return nil
}

func requireCurrentTarget(current uint32, target uint64) error {
	if current == 0 {
		return errors.New("media key rotation requires a current media key version")
	}
	if target != 0 && target != uint64(current) {
		return fmt.Errorf("--target-version %d must match current media key version %d", target, current)
	}
	return nil
}

func openEncryptedStore(ctx context.Context, cfg config.Config, log *slog.Logger) (blob.Store, func() error, error) {
	if cfg.BlobS3 == nil {
		local, err := blob.OpenLocal(cfg.BlobDir)
		if err != nil {
			return nil, nil, fmt.Errorf("open blob directory: %w", err)
		}
		prefixed, err := blob.NewPrefixedStore(local, blob.EncryptedObjectsPrefix)
		if err != nil {
			if closeErr := local.Close(); closeErr != nil {
				return nil, nil, errors.Join(err, fmt.Errorf("close blob store: %w", closeErr))
			}
			return nil, nil, err
		}
		return prefixed, local.Close, nil
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
	prefixed, err := blob.NewPrefixedStore(remote, blob.EncryptedObjectsPrefix)
	if err != nil {
		return nil, nil, err
	}
	return prefixed, func() error { return nil }, nil
}

func scanFinalInventory(
	ctx context.Context,
	database inventoryReferenceSource,
	backend blob.Store,
	ring *blob.KeyRing,
) (blobinventory.Report, error) {
	references, err := readReferences(ctx, database)
	if err != nil {
		return blobinventory.Report{}, err
	}
	inventory, err := blobinventory.Scan(ctx, backend, ring, references)
	if err != nil {
		return blobinventory.Report{}, err
	}
	reconciledReferences, err := readReferences(ctx, database)
	if err != nil {
		return blobinventory.Report{}, err
	}
	if err := requireReferencesUnchanged(references, reconciledReferences); err != nil {
		return blobinventory.Report{}, err
	}
	return inventory, nil
}

func readReferences(ctx context.Context, database inventoryReferenceSource) ([]blobinventory.Reference, error) {
	rows, err := database.BlobInventoryReferences(ctx)
	if err != nil {
		return nil, err
	}
	references := make([]blobinventory.Reference, len(rows))
	for index, row := range rows {
		references[index] = blobinventory.Reference{
			Key: row.Key, Kind: row.Kind, ID: row.ID, LogicalBytes: row.LogicalBytes,
		}
	}
	return references, nil
}

func requireReferencesUnchanged(beforeScan, afterScan []blobinventory.Reference) error {
	remaining := make(map[blobinventory.Reference]int, len(beforeScan))
	for _, ref := range beforeScan {
		remaining[ref]++
	}
	for _, ref := range afterScan {
		if remaining[ref] == 0 {
			return errors.New("media key rotation: references changed during final inventory; rerun rotation")
		}
		remaining[ref]--
	}
	for _, count := range remaining {
		if count != 0 {
			return errors.New("media key rotation: references changed during final inventory; rerun rotation")
		}
	}
	return nil
}
