// Command blob-encrypt-migrate encrypts every database-referenced object in
// the accepted plaintext S3 namespace, verifies the encrypted copies, and then
// publishes encrypted blob-mode authority. Plaintext removal is a separate,
// explicit --cleanup invocation.
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
	"sort"
	"strings"
	"syscall"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/blobinventory"
	"github.com/teagramhq/teagram-server/internal/blobmigration"
	"github.com/teagramhq/teagram-server/internal/blobmode"
	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/store"
)

const (
	defaultProgressPath = "/run/teagramd/reports/blob-encryption-progress.jsonl"
	defaultReportDir    = "/run/teagramd/reports"
)

type commandResult struct {
	Mode            string                   `json:"mode"`
	Authority       string                   `json:"authority"`
	Writes          string                   `json:"writes"`
	Objects         int                      `json:"objects"`
	Bytes           int64                    `json:"bytes"`
	Removed         int                      `json:"plaintext_objects_removed,omitempty"`
	BytesRemoved    int64                    `json:"plaintext_bytes_removed,omitempty"`
	InventorySHA256 string                   `json:"inventory_sha256,omitempty"`
	ProgressSHA256  string                   `json:"progress_sha256,omitempty"`
	AlreadyVerified int                      `json:"already_verified,omitempty"`
	Unaccounted     []blobinventory.ObjectID `json:"unaccounted,omitempty"`
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
		logger.Error("blob-encrypt-migrate failed", "err", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) (retErr error) {
	flags := flag.NewFlagSet("blob-encrypt-migrate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	writes := flags.String("writes", "", "required write-quiescence choice: stopped or drained")
	progress := flags.String("progress", defaultProgressPath, "persistent private JSONL progress journal")
	cleanup := flags.Bool("cleanup", false, "explicitly remove verified plaintext copies after encrypted authority is active")
	authorityDir := flags.String("authority-dir", blobmode.DefaultDirectory, "durable blob-mode authority directory")
	reportDir := flags.String("report-dir", defaultReportDir, "root-owned blob-mode report directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: blob-encrypt-migrate --writes stopped|drained [--progress PATH] [--cleanup]")
	}
	if *writes == "" {
		return errors.New("media encryption migration requires --writes stopped or --writes drained")
	}
	if *writes != "stopped" && *writes != "drained" {
		return errors.New("--writes must be stopped or drained")
	}
	if *progress == "" {
		return errors.New("--progress must name a persistent journal path")
	}
	if *authorityDir == "" || *reportDir == "" {
		return errors.New("--authority-dir and --report-dir must be non-empty")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	quietLog := slog.New(slog.DiscardHandler)
	cfg, err := config.LoadReadOnly(quietLog)
	if err != nil {
		return err
	}
	if cfg.BlobS3 == nil {
		return errors.New("media encryption migration requires the configured S3 backend")
	}
	if cfg.MediaKeys == nil {
		return errors.New("media encryption migration requires TG_MEDIA_KEY_DIR and a current media key")
	}
	effective := blobmode.EffectiveConfig{BlobDir: cfg.BlobDir, BlobS3: cfg.BlobS3}
	authority, err := blobmode.Read(*authorityDir, effective)
	if err != nil {
		return err
	}
	if *cleanup {
		if !authority.Present || authority.Outcome != blobmode.OutcomeEncrypted {
			return errors.New("plaintext cleanup requires encrypted blob-mode authority")
		}
	} else if !authority.Present ||
		(authority.Outcome != blobmode.OutcomeS3Accepted && authority.Outcome != blobmode.OutcomeEncrypted) {
		return errors.New("media encryption migration requires s3-accepted or encrypted blob-mode authority")
	}

	s3Config := *cfg.BlobS3
	s3Config.Logger = quietLog
	remote, err := blob.NewS3(s3Config)
	if err != nil {
		return fmt.Errorf("configure object store: %w", err)
	}
	if err := remote.Check(ctx); err != nil {
		return fmt.Errorf("check object store: %w", err)
	}
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
	options := blobmigration.EncryptionOptions{ProgressPath: *progress, Writes: *writes}
	result := commandResult{Writes: *writes, Authority: authority.Outcome}
	if *cleanup {
		summary, err := blobmigration.CleanupPlaintext(ctx, remote, cfg.MediaKeys, references, options)
		if err != nil {
			return err
		}
		if err := requireUnchangedReferences(ctx, database, references, "plaintext cleanup"); err != nil {
			return err
		}
		result.Mode = "cleanup"
		result.Objects = len(references)
		result.Removed = summary.ObjectsRemoved
		result.BytesRemoved = summary.BytesRemoved
	} else {
		summary, err := blobmigration.EncryptReferenced(ctx, remote, cfg.MediaKeys, references, options)
		if err != nil {
			return err
		}
		if err := requireUnchangedReferences(ctx, database, references, "media encryption migration"); err != nil {
			return err
		}
		if authority.Outcome == blobmode.OutcomeS3Accepted {
			proof := blobmode.EncryptedEvidence{
				InventorySHA256: summary.InventorySHA256,
				InventoryReport: summary.InventoryReport,
				ObjectCount:     int64(summary.Objects), ByteTotal: summary.Bytes,
				KeyVersion: cfg.MediaKeys.Current(), Writes: *writes,
			}
			if err := blobmode.PublishEncrypted(*authorityDir, *reportDir, effective, proof); err != nil {
				return fmt.Errorf("publish encrypted blob-mode authority: %w", err)
			}
		}
		result.Mode = "encrypt"
		result.Authority = blobmode.OutcomeEncrypted
		result.Objects = summary.Objects
		result.Bytes = summary.Bytes
		result.InventorySHA256 = summary.InventorySHA256
		result.ProgressSHA256 = summary.ProgressSHA256
		result.AlreadyVerified = summary.AlreadyVerified
		result.Unaccounted = summary.Unaccounted
	}
	if err := json.NewEncoder(stdout).Encode(result); err != nil {
		return fmt.Errorf("write migration summary: %w", err)
	}
	if _, err := fmt.Fprintf(stderr, "blob_encryption=%s authority=%s objects=%d writes=%s unaccounted=%d\n",
		result.Mode, result.Authority, result.Objects, result.Writes, len(result.Unaccounted)); err != nil {
		return fmt.Errorf("write migration status: %w", err)
	}
	return nil
}

func readReferences(ctx context.Context, database *store.Store) ([]blobinventory.Reference, error) {
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
	sort.Slice(references, func(i, j int) bool { return references[i].Key < references[j].Key })
	return references, nil
}

func requireUnchangedReferences(ctx context.Context, database *store.Store, before []blobinventory.Reference, operation string) error {
	after, err := readReferences(ctx, database)
	if err != nil {
		return err
	}
	if len(before) == len(after) {
		unchanged := true
		for index := range before {
			if before[index] != after[index] {
				unchanged = false
				break
			}
		}
		if unchanged {
			return nil
		}
	}
	beforeByKey := make(map[string]blobinventory.Reference, len(before))
	afterByKey := make(map[string]blobinventory.Reference, len(after))
	for _, ref := range before {
		beforeByKey[ref.Key] = ref
	}
	for _, ref := range after {
		afterByKey[ref.Key] = ref
	}
	keys := make(map[string]struct{}, len(beforeByKey)+len(afterByKey))
	for key := range beforeByKey {
		keys[key] = struct{}{}
	}
	for key := range afterByKey {
		keys[key] = struct{}{}
	}
	ids := make(map[string]struct{})
	for key := range keys {
		oldRef, oldExists := beforeByKey[key]
		newRef, newExists := afterByKey[key]
		if oldExists && newExists && oldRef == newRef {
			continue
		}
		if oldExists {
			ids[oldRef.Kind+":"+oldRef.ID] = struct{}{}
		}
		if newExists {
			ids[newRef.Kind+":"+newRef.ID] = struct{}{}
		}
	}
	changed := make([]string, 0, len(ids))
	for id := range ids {
		changed = append(changed, id)
	}
	sort.Strings(changed)
	return fmt.Errorf("%s: database media references changed; no authority change was made; review references %s",
		operation, strings.Join(changed, ","))
}
