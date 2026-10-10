//go:build fixtureauthprogress

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/gotd/td/tdsync"

	"github.com/teagramhq/teagram-server/internal/mtproto"
)

const (
	fixtureAuthProgressPathEnv           = "TG_FIXTURE_AUTH_PROGRESS"
	fixtureAuthProgressRunIDEnv          = "TG_FIXTURE_AUTH_PROGRESS_RUN_ID"
	fixtureAuthProgressRunnerEnv         = "TG_FIXTURE_AUTH_PROGRESS_RUNNER_REVISION"
	fixtureAuthProgressArtifactSourceEnv = "TG_FIXTURE_AUTH_PROGRESS_ARTIFACT_SOURCE_REVISION"
	fixtureAuthProgressArtifactDigestEnv = "TG_FIXTURE_AUTH_PROGRESS_ARTIFACT_DIGEST"
	fixtureAuthProgressHarnessEnv        = "TG_FIXTURE_AUTH_PROGRESS_HARNESS_REVISION"
	fixtureAuthProgressRoot              = "/run/log"
)

var ErrFixtureAuthProgressIncomplete = errors.New("fixture auth-progress output is incomplete")

type fixtureAuthProgressConfig struct {
	root         string
	relativePath string
	identity     mtproto.RPCProgressIdentity
}

type fixtureAuthProgressReport struct {
	SchemaVersion int32                         `json:"schema_version"`
	Identity      mtproto.RPCProgressIdentity   `json:"identity"`
	Snapshots     []mtproto.RPCProgressSnapshot `json:"snapshots"`
	Incomplete    bool                          `json:"incomplete"`
}

func validateServeEnvironment() error {
	_, err := fixtureAuthProgressConfigFromEnv()
	return err
}

func fixtureAuthProgressConfigFromEnv() (fixtureAuthProgressConfig, error) {
	values := make(map[string]string, 6)
	for _, name := range []string{
		fixtureAuthProgressPathEnv,
		fixtureAuthProgressRunIDEnv,
		fixtureAuthProgressRunnerEnv,
		fixtureAuthProgressArtifactSourceEnv,
		fixtureAuthProgressArtifactDigestEnv,
		fixtureAuthProgressHarnessEnv,
	} {
		value, ok := os.LookupEnv(name)
		if !ok || value == "" {
			return fixtureAuthProgressConfig{}, fmt.Errorf("%s is required for fixtureauthprogress serve", name)
		}
		values[name] = value
	}
	serverRevision, err := fixtureBuildRevision()
	if err != nil {
		return fixtureAuthProgressConfig{}, err
	}
	return newFixtureAuthProgressConfig(values, fixtureAuthProgressRoot, serverRevision)
}

func fixtureBuildRevision() (string, error) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", errors.New("fixture server build identity is unavailable")
	}
	var revision string
	var modified bool
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	if modified || !validFixtureRevision(revision) {
		return "", errors.New("fixture server build must have a clean immutable revision")
	}
	return revision, nil
}

func validFixtureRevision(value string) bool {
	if len(value) != 40 {
		return false
	}
	for i := range value {
		if !((value[i] >= '0' && value[i] <= '9') || (value[i] >= 'a' && value[i] <= 'f')) {
			return false
		}
	}
	return true
}

func newFixtureAuthProgressConfig(values map[string]string, root, serverRevision string) (fixtureAuthProgressConfig, error) {
	identity := mtproto.RPCProgressIdentity{
		RunID: values[fixtureAuthProgressRunIDEnv],
		SourceRefs: mtproto.RPCProgressSourceRefs{
			RunnerRevision:         values[fixtureAuthProgressRunnerEnv],
			ArtifactSourceRevision: values[fixtureAuthProgressArtifactSourceEnv],
			ArtifactDigest:         values[fixtureAuthProgressArtifactDigestEnv],
			HarnessRevision:        values[fixtureAuthProgressHarnessEnv],
			ServerRevision:         serverRevision,
		},
	}
	if err := identity.Validate(); err != nil {
		return fixtureAuthProgressConfig{}, errors.New("fixture auth-progress source references are invalid")
	}
	relativePath, err := validateFixtureAuthProgressPath(values[fixtureAuthProgressPathEnv], root)
	if err != nil {
		return fixtureAuthProgressConfig{}, err
	}
	return fixtureAuthProgressConfig{root: root, relativePath: relativePath, identity: identity}, nil
}

func validateFixtureAuthProgressPath(path, root string) (string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", errors.New("fixture auth-progress path must be absolute and clean")
	}
	root = filepath.Clean(root)
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("fixture auth-progress root is unavailable or unsafe")
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", errors.New("fixture auth-progress root is invalid")
	}
	relativePath, err := filepath.Rel(rootAbs, path)
	if err != nil || relativePath == "." || relativePath == ".." || strings.HasPrefix(relativePath, ".."+string(filepath.Separator)) {
		return "", errors.New("fixture auth-progress path must be strictly below /run/log")
	}
	parent := filepath.Dir(path)
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", errors.New("fixture auth-progress parent is unavailable or unsafe")
	}
	parentAbs, err := filepath.Abs(parent)
	if err != nil {
		return "", errors.New("fixture auth-progress parent is invalid")
	}
	resolvedParentAbs, err := filepath.Abs(resolvedParent)
	if err != nil || resolvedParentAbs != parentAbs {
		return "", errors.New("fixture auth-progress path must not traverse symlinks")
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("fixture auth-progress target must be a regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("fixture auth-progress target is unavailable")
	}
	return relativePath, nil
}

func serveMTProto(ctx context.Context, server *mtproto.Server, tcp net.Listener, websocket net.Listener) error {
	config, err := fixtureAuthProgressConfigFromEnv()
	if err != nil {
		return err
	}
	tracer := mtproto.NewRPCTracer(mtproto.RPCTracerConfig{Exporter: fixtureRPCSpanExporter{}})
	progress, err := newFixtureAuthProgressWriter(config, tracer)
	if err != nil {
		if closeErr := tracer.Close(); closeErr != nil {
			return errors.Join(err, closeErr)
		}
		return err
	}
	server.SetRPCTracer(tracer)
	var serveErr error
	if websocket == nil {
		serveErr = server.Serve(ctx, tcp)
	} else {
		group := tdsync.NewCancellableGroup(ctx)
		group.Go(func(ctx context.Context) error { return server.Serve(ctx, tcp) })
		group.Go(func(ctx context.Context) error { return server.ServeWebSocket(ctx, websocket) })
		serveErr = group.Wait()
	}
	return errors.Join(serveErr, progress.Close())
}

type fixtureRPCSpanExporter struct{}

func (fixtureRPCSpanExporter) Export(context.Context, mtproto.RPCSpan) error { return nil }

type fixtureAuthProgressWriter struct {
	root         *os.Root
	relativePath string
	identity     mtproto.RPCProgressIdentity
	tracer       *mtproto.RPCTracer
	report       fixtureAuthProgressReport
	close        chan struct{}
	done         chan struct{}
	signals      chan os.Signal
	boundaries   chan chan error
	closeErr     error
	firstErr     error
}

func newFixtureAuthProgressWriter(config fixtureAuthProgressConfig, tracer *mtproto.RPCTracer) (*fixtureAuthProgressWriter, error) {
	root, err := os.OpenRoot(config.root)
	if err != nil {
		return nil, errors.New("fixture auth-progress root could not be opened")
	}
	if err := validateFixtureAuthProgressRelativePath(root, config.relativePath); err != nil {
		if closeErr := root.Close(); closeErr != nil {
			return nil, errors.Join(err, closeErr)
		}
		return nil, err
	}
	writer := &fixtureAuthProgressWriter{
		root:         root,
		relativePath: config.relativePath,
		identity:     config.identity,
		tracer:       tracer,
		report: fixtureAuthProgressReport{
			SchemaVersion: mtproto.RPCProgressSchemaVersion,
			Identity:      config.identity,
			Snapshots:     make([]mtproto.RPCProgressSnapshot, 0, 3),
		},
		close:      make(chan struct{}),
		done:       make(chan struct{}),
		signals:    make(chan os.Signal, 1),
		boundaries: make(chan chan error),
	}
	signal.Notify(writer.signals, syscall.SIGUSR1)
	go writer.run()
	return writer, nil
}

func validateFixtureAuthProgressRelativePath(root *os.Root, relativePath string) error {
	if relativePath == "" || filepath.IsAbs(relativePath) || filepath.Clean(relativePath) != relativePath || strings.HasPrefix(relativePath, ".."+string(filepath.Separator)) || relativePath == ".." {
		return errors.New("fixture auth-progress relative path is invalid")
	}
	parent := filepath.Dir(relativePath)
	for current := parent; current != "."; current = filepath.Dir(current) {
		info, err := root.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("fixture auth-progress parent is unavailable or unsafe")
		}
	}
	if info, err := root.Lstat(relativePath); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("fixture auth-progress target must be a regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("fixture auth-progress target is unavailable")
	}
	return nil
}

func (w *fixtureAuthProgressWriter) run() {
	defer close(w.done)
	for {
		select {
		case <-w.close:
			w.finish()
			return
		case <-w.signals:
			if err := w.recordBoundary(); err != nil {
				w.rememberError(err)
			}
		case response := <-w.boundaries:
			response <- w.recordBoundary()
		}
	}
}

func (w *fixtureAuthProgressWriter) recordNextBoundary() error {
	response := make(chan error, 1)
	select {
	case w.boundaries <- response:
	case <-w.done:
		return ErrFixtureAuthProgressIncomplete
	}
	select {
	case err := <-response:
		return err
	case <-w.done:
		return ErrFixtureAuthProgressIncomplete
	}
}

func (w *fixtureAuthProgressWriter) recordBoundary() error {
	if len(w.report.Snapshots) >= 2 {
		w.report.Incomplete = true
		return ErrFixtureAuthProgressIncomplete
	}
	boundary := mtproto.RPCProgressBoundaryArtifactReady
	if len(w.report.Snapshots) == 1 {
		boundary = mtproto.RPCProgressBoundaryRunnerExit
	}
	snapshot, err := w.tracer.ProgressSnapshot(w.identity, boundary)
	if err != nil {
		w.report.Incomplete = true
		return err
	}
	w.report.Snapshots = append(w.report.Snapshots, snapshot)
	if err := snapshot.Validate(); err != nil {
		w.report.Incomplete = true
		if writeErr := w.writeReport(); writeErr != nil {
			return errors.Join(err, writeErr)
		}
		return err
	}
	if err := w.writeReport(); err != nil {
		w.report.Incomplete = true
		return err
	}
	return nil
}

func (w *fixtureAuthProgressWriter) rememberError(err error) {
	w.report.Incomplete = true
	if w.firstErr == nil {
		w.firstErr = err
	}
}

func (w *fixtureAuthProgressWriter) Close() error {
	select {
	case <-w.done:
		return w.closeErr
	default:
	}
	select {
	case w.close <- struct{}{}:
	case <-w.done:
	}
	<-w.done
	return w.closeErr
}

func (w *fixtureAuthProgressWriter) finish() {
	signal.Stop(w.signals)
	tracerErr := w.tracer.Close()
	if tracerErr != nil {
		w.report.Incomplete = true
	}
	final, snapshotErr := w.tracer.ProgressSnapshot(w.identity, mtproto.RPCProgressBoundaryShutdownFinal)
	if snapshotErr != nil {
		w.report.Incomplete = true
	} else {
		w.report.Snapshots = append(w.report.Snapshots, final)
		if err := final.Validate(); err != nil {
			w.report.Incomplete = true
			snapshotErr = errors.Join(snapshotErr, err)
		}
	}
	if len(w.report.Snapshots) != 3 || w.firstErr != nil {
		w.report.Incomplete = true
	}
	writeErr := w.writeReport()
	rootErr := w.root.Close()
	if w.report.Incomplete || tracerErr != nil || snapshotErr != nil || writeErr != nil {
		w.closeErr = errors.Join(ErrFixtureAuthProgressIncomplete, w.firstErr, tracerErr, snapshotErr, writeErr, rootErr)
	} else {
		w.closeErr = rootErr
	}
}

func (w *fixtureAuthProgressWriter) writeReport() error {
	data, err := json.Marshal(w.report)
	if err != nil {
		return errors.New("fixture auth-progress report could not be encoded")
	}
	data = append(data, '\n')
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return errors.New("fixture auth-progress temporary identity could not be generated")
	}
	temporary := ".auth-progress-" + hex.EncodeToString(nonce[:]) + ".tmp"
	file, err := w.root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.New("fixture auth-progress temporary file could not be created")
	}
	_, writeErr := io.Copy(file, bytes.NewReader(data))
	syncErr := file.Sync()
	closeErr := file.Close()
	if combined := errors.Join(writeErr, syncErr, closeErr); combined != nil {
		removeErr := w.root.Remove(temporary)
		return errors.Join(errors.New("fixture auth-progress report could not be written"), combined, removeErr)
	}
	if err := w.root.Rename(temporary, w.relativePath); err != nil {
		removeErr := w.root.Remove(temporary)
		return errors.Join(errors.New("fixture auth-progress report could not be installed"), err, removeErr)
	}
	return nil
}
