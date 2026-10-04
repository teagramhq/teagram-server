// Package fleet publishes bounded, expiring process snapshots to shared
// storage and reads them without returning connected-account identifiers.
package fleet

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"runtime/debug"
	"time"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

var safeVersionPattern = regexp.MustCompile(`^[A-Za-z0-9._+-]{1,128}$`)

// Publisher samples one process registry independently of MTProto requests.
// It allows only one publication in flight and stops on its own write deadline.
type Publisher struct {
	store      *store.Store
	registry   *mtproto.SessionRegistry
	generation string
	replicaID  *string
	version    string
	logger     *slog.Logger
}

// NewPublisher creates the process-lifetime fleet telemetry collector.
func NewPublisher(
	st *store.Store,
	registry *mtproto.SessionRegistry,
	generation string,
	replicaID *string,
	version string,
) *Publisher {
	if replicaID != nil {
		id := *replicaID
		replicaID = &id
	}
	return &Publisher{
		store:      st,
		registry:   registry,
		generation: generation,
		replicaID:  replicaID,
		version:    safeBuildVersion(version),
		logger:     slog.Default(),
	}
}

// BuildVersion returns the module version or, for local builds, the VCS
// revision. Unsafe or unavailable build text is omitted.
func BuildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	version := info.Main.Version
	if version == "" || version == "(devel)" {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				version = "g" + setting.Value
				break
			}
		}
	}
	return safeBuildVersion(version)
}

// Run publishes immediately, then on the fixed sampling cadence until ctx is
// canceled. Shutdown removes this generation's telemetry before returning.
func (p *Publisher) Run(ctx context.Context) error {
	ticker := time.NewTicker(store.FleetSampleInterval)
	defer ticker.Stop()

	if err := p.publish(ctx); err != nil {
		p.logPublicationFailure(err)
	}
	for {
		select {
		case <-ctx.Done():
			cleanupCtx, cancel := context.WithTimeout(context.Background(), store.FleetWriterDeadline)
			defer cancel()
			if err := p.store.DeleteFleetSnapshot(cleanupCtx, p.generation); err != nil {
				return fmt.Errorf("remove fleet telemetry on shutdown: %w", err)
			}
			return nil
		case <-ticker.C:
			if err := p.publish(ctx); err != nil {
				p.logPublicationFailure(err)
			}
		}
	}
}

// Start runs the collector until the returned stop function is called or the
// parent context ends. The stop function waits for bounded cleanup to finish.
func (p *Publisher) Start(ctx context.Context) func() error {
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- p.Run(runCtx)
	}()
	return func() error {
		cancel()
		return <-done
	}
}

func (p *Publisher) publish(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, store.FleetWriterDeadline)
	defer cancel()

	local, err := p.registry.SnapshotLiveAccounts(ctx, store.FleetMaxDistinctAccountsPerGeneration)
	if err != nil {
		return fmt.Errorf("sample live accounts: %w", err)
	}
	return p.store.PublishFleetSnapshot(ctx, store.FleetProcessSample{
		Generation:       p.generation,
		ReplicaID:        p.replicaID,
		Version:          p.version,
		Connections:      local.Connections,
		Sessions:         local.Sessions,
		AccountIDs:       local.AccountIDs,
		AccountsComplete: local.Complete,
	})
}

func (p *Publisher) logPublicationFailure(err error) {
	class := "collection_or_storage"
	if errors.Is(err, context.DeadlineExceeded) {
		class = "deadline"
	} else if errors.Is(err, context.Canceled) {
		class = "canceled"
	}
	p.logger.Error("fleet telemetry publication failed", "error_class", class)
}

func safeBuildVersion(version string) string {
	if !safeVersionPattern.MatchString(version) {
		return ""
	}
	return version
}
