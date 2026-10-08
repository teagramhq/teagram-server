package e2e_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/teagramhq/teagram-server/internal/rsakey"
)

const (
	realFixtureServerRevision = "7b5fcc9c68c1b275cad7d076a343d6d476cd447d"
	realFixtureWebRevision    = "69bd2c7dc25b6e92630d04363c8460cfd2ab000e"
)

// fixtureProductReferences are ordinary HTTPS product links. The fixture audit
// records them as evidence and never treats them as allowed destinations.
var fixtureProductReferences = []string{
	"https://web.telegram.org/a/",
	"https://telegram.org/android",
	"https://t.me/botfather",
}

func TestRealServerFixtureUsesCIAMD64BrowserImage(t *testing.T) {
	workflow, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	ciImage := regexp.MustCompile(`(?m)^[ \t]*-[ \t]*arch:[ \t]*amd64\n[ \t]*runner:[ \t]*ubuntu-26\.04\n[ \t]*platform:[ \t]*linux/amd64\n[ \t]*image:[ \t]*(\S+)$`).FindSubmatch(workflow)
	if len(ciImage) != 2 {
		t.Fatal("CI workflow has no pinned amd64 Playwright image")
	}
	dockerfile, err := os.ReadFile(filepath.Join("real_server_fixture", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	fixtureImage := regexp.MustCompile(`(?m)^FROM (mcr\.microsoft\.com/playwright[^\r\n]+)$`).FindSubmatch(dockerfile)
	if len(fixtureImage) != 2 {
		t.Fatal("fixture Dockerfile has no pinned Playwright image")
	}
	if !bytes.Equal(fixtureImage[1], ciImage[1]) {
		t.Fatalf("fixture Playwright image = %q, want CI amd64 image %q", fixtureImage[1], ciImage[1])
	}
}

func TestRealServerFixtureValidatesDockerEndpointBeforeDaemonAccess(t *testing.T) {
	cases := []struct {
		name              string
		dockerHost        string
		dockerContext     string
		activeContext     string
		contextEndpoint   string
		allowDaemonAccess bool
		allowedCalls      []string
		pinnedDockerEnv   string
	}{
		{
			name:       "DOCKER_HOST",
			dockerHost: "tcp://198.51.100.23:2375",
		},
		{
			name:          "DOCKER_CONTEXT overrides DOCKER_HOST",
			dockerHost:    "unix:///var/run/docker.sock",
			dockerContext: "fixture-remote",
			allowedCalls:  []string{"context inspect fixture-remote --format {{.Endpoints.docker.Host}}"},
		},
		{
			name:          "saved active context",
			activeContext: "fixture-remote",
			allowedCalls: []string{
				"context show",
				"context inspect fixture-remote --format {{.Endpoints.docker.Host}}",
			},
		},
		{
			name:              "workspace-local daemon is pinned",
			dockerHost:        "tcp://multica-dind:2375",
			contextEndpoint:   "tcp://multica-dind:2375",
			allowDaemonAccess: true,
			allowedCalls:      []string{"container inspect telegram-fixture-00000000000000000000000000000000browser"},
			pinnedDockerEnv:   "tcp://multica-dind:2375|",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			binDir := t.TempDir()
			callLog := filepath.Join(binDir, "docker-calls")
			mockDocker := `#!/usr/bin/env bash
set -eu
printf '%s\n' "$*" >> "$FIXTURE_DOCKER_CALL_LOG"
if [[ ${1:-} == context && ${2:-} == show ]]; then
	printf '%s\n' "$FIXTURE_ACTIVE_CONTEXT"
	exit 0
fi
if [[ ${1:-} == context && ${2:-} == inspect ]]; then
	printf '%s\n' "${FIXTURE_CONTEXT_ENDPOINT:-tcp://198.51.100.23:2375}"
	exit 0
fi
if [[ ${1:-} == container && ${2:-} == inspect && ${FIXTURE_ALLOW_DAEMON_ACCESS:-} == 1 ]]; then
	printf '%s|%s\n' "${DOCKER_HOST:-}" "${DOCKER_CONTEXT:-}" > "$FIXTURE_DOCKER_PIN_LOG"
	exit 0
fi
printf 'unexpected Docker daemon operation: %s\n' "$*" >&2
exit 89
`
			dockerPath := filepath.Join(binDir, "docker")
			if err := os.WriteFile(dockerPath, []byte(mockDocker), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dockerPath, 0o700); err != nil { //nolint:gosec // The fixture runner needs an executable temporary Docker wrapper.
				t.Fatal(err)
			}

			command := fixtureCommand(context.Background(), "bash", filepath.Join("real_server_fixture", "run.sh"),
				"--server-revision", currentServerRevision(t),
				"--web-revision", realFixtureWebRevision,
				"--run-id", "00000000000000000000000000000000",
			)
			command.Env = fixtureEnvironment(map[string]string{
				"PATH":                     binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
				"DOCKER_HOST":              testCase.dockerHost,
				"DOCKER_CONTEXT":           testCase.dockerContext,
				"FIXTURE_ACTIVE_CONTEXT":   testCase.activeContext,
				"FIXTURE_CONTEXT_ENDPOINT": testCase.contextEndpoint,
				"FIXTURE_DOCKER_CALL_LOG":  callLog,
				"FIXTURE_DOCKER_PIN_LOG":   callLog + ".pin",
				"FIXTURE_ALLOW_DAEMON_ACCESS": func() string {
					if testCase.allowDaemonAccess {
						return "1"
					}
					return ""
				}(),
			})
			var stdout, stderr bytes.Buffer
			command.Stdout = &stdout
			command.Stderr = &stderr
			err := command.Run()
			var exitErr *osexec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
				t.Fatalf("fixture endpoint case exit = %v, want exit 2; stderr=%q", err, stderr.String())
			}
			if testCase.allowDaemonAccess {
				if !strings.Contains(stderr.String(), "resource name already exists: telegram-fixture-") {
					t.Fatalf("local Docker endpoint did not reach the first resource check: %q", stderr.String())
				}
			} else if !strings.Contains(stderr.String(), "fixture requires a local Docker daemon") {
				t.Fatalf("non-local Docker endpoint diagnostic = %q", stderr.String())
			}
			if stdout.Len() != 0 {
				t.Fatalf("rejected Docker endpoint wrote readiness output %q", stdout.String())
			}
			calls, err := os.ReadFile(callLog)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			gotCalls := strings.FieldsFunc(string(calls), func(r rune) bool { return r == '\n' })
			if len(gotCalls) != len(testCase.allowedCalls) {
				t.Fatalf("Docker calls before fixture outcome = %q, want %q", gotCalls, testCase.allowedCalls)
			}
			for index, call := range testCase.allowedCalls {
				if gotCalls[index] != call {
					t.Fatalf("Docker call %d = %q, want %q", index, gotCalls[index], call)
				}
			}
			if testCase.pinnedDockerEnv != "" {
				pinnedEnv, err := os.ReadFile(callLog + ".pin")
				if err != nil {
					t.Fatal(err)
				}
				if string(pinnedEnv) != testCase.pinnedDockerEnv+"\n" {
					t.Fatalf("Docker selection after preflight = %q, want %q", pinnedEnv, testCase.pinnedDockerEnv)
				}
			}
		})
	}
}

func TestRealServerFixtureRejectsLiveEndpointBeforeMutation(t *testing.T) {
	script := filepath.Join("real_server_fixture", "run.sh")
	command := fixtureCommand(context.Background(), "bash", script,
		"--server-revision", realFixtureServerRevision,
		"--web-revision", realFixtureWebRevision,
		"--run-id", "00000000000000000000000000000000",
		"--endpoint", "wss://telegram-server.tailaa4918.ts.net/apiws",
	)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if err == nil {
		t.Fatal("fixture accepted a caller-supplied endpoint")
	}
	if !strings.Contains(stderr.String(), "--endpoint is not accepted") {
		t.Fatalf("fixture rejection = %q, want explicit endpoint rejection", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("fixture rejection wrote readiness output %q", stdout.String())
	}
}

func TestRealServerFixtureRejectsMovingWebRevisionBeforeMutation(t *testing.T) {
	command := fixtureCommand(context.Background(), "bash", filepath.Join("real_server_fixture", "run.sh"),
		"--server-revision", currentServerRevision(t),
		"--web-revision", "master",
		"--run-id", "00000000000000000000000000000000",
	)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	var exitErr *osexec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
		t.Fatalf("unapproved web revision exit = %v, want exit 2; stderr=%q", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "web revision must be a full lowercase 40-character SHA") {
		t.Fatalf("unapproved web revision diagnostic = %q", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("unapproved web revision wrote readiness output %q", stdout.String())
	}
}

func TestRealServerFixtureAcceptsImmutableHistoricalRevisionPairBeforeMutation(t *testing.T) {
	binDir := t.TempDir()
	callLog := filepath.Join(binDir, "docker-calls")
	mockDocker := `#!/usr/bin/env bash
set -eu
printf '%s\n' "$*" >> "$FIXTURE_DOCKER_CALL_LOG"
if [[ ${1:-} == container && ${2:-} == inspect ]]; then
	printf 'fixture preflight collision\n' >&2
	exit 0
fi
printf 'unexpected Docker operation: %s\n' "$*" >&2
exit 89
`
	dockerPath := filepath.Join(binDir, "docker")
	if err := os.WriteFile(dockerPath, []byte(mockDocker), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dockerPath, 0o700); err != nil { //nolint:gosec // The fixture runner needs an executable temporary Docker wrapper.
		t.Fatal(err)
	}
	command := fixtureCommand(context.Background(), "bash", filepath.Join("real_server_fixture", "run.sh"),
		"--server-revision", "6668a0a3519909ef512fdc59e4937975f108671d",
		"--web-revision", "09373cc2713d31e93664c38a4fd0335ea37a5f01",
		"--run-id", "00000000000000000000000000000000",
	)
	command.Env = fixtureEnvironment(map[string]string{
		"PATH":                    binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"DOCKER_HOST":             "tcp://multica-dind:2375",
		"FIXTURE_DOCKER_CALL_LOG": callLog,
	})
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	var exitErr *osexec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
		t.Fatalf("historical revision pair preflight exit = %v, want exit 2; stderr=%q", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "resource name already exists: telegram-fixture-00000000000000000000000000000000browser") {
		t.Fatalf("historical revision pair did not reach the owned-resource boundary: %q", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("historical pair preflight wrote readiness output %q", stdout.String())
	}
	calls, err := os.ReadFile(callLog)
	if err != nil || string(calls) != "container inspect telegram-fixture-00000000000000000000000000000000browser\n" {
		t.Fatalf("historical pair preflight Docker calls = %q, err=%v", calls, err)
	}
}

func TestRealServerFixtureRejectsResourceCollisionBeforeMutation(t *testing.T) {
	runID := newRealFixtureRunID(t)
	network := "telegram-fixture-" + runID + "edge"
	if output, err := fixtureCommand(context.Background(), "docker", "network", "create", "--driver", "bridge", "--internal", "--ipv6=false", network).CombinedOutput(); err != nil {
		t.Fatalf("create unowned collision network: %v: %s", err, strings.TrimSpace(string(output)))
	}
	t.Cleanup(func() {
		if output, err := fixtureCommand(context.Background(), "docker", "network", "rm", network).CombinedOutput(); err != nil {
			t.Errorf("remove collision test network: %v: %s", err, strings.TrimSpace(string(output)))
		}
	})

	command := fixtureCommand(context.Background(), "bash", filepath.Join("real_server_fixture", "run.sh"),
		"--server-revision", currentServerRevision(t),
		"--web-revision", realFixtureWebRevision,
		"--run-id", runID,
	)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	var exitErr *osexec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
		t.Fatalf("resource collision exit = %v, want exit 2; stderr=%q", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "resource name already exists: "+network) {
		t.Fatalf("resource collision diagnostic = %q", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("resource collision wrote readiness output %q", stdout.String())
	}
	if err := fixtureCommand(context.Background(), "docker", "network", "inspect", network).Run(); err != nil {
		t.Fatalf("fixture removed an unowned collision network: %v", err)
	}
	for _, suffix := range []string{"browser", "tls-front", "telegramd", "database", "client", "atlas"} {
		if err := fixtureCommand(context.Background(), "docker", "container", "inspect", "telegram-fixture-"+runID+suffix).Run(); err == nil {
			t.Errorf("fixture created container %s before rejecting the collision", suffix)
		}
	}
}

func TestRealServerFixtureContextOutlivesCleanup(t *testing.T) {
	var canceled <-chan struct{}
	t.Run("fixture cleanup", func(t *testing.T) {
		ctx := realFixtureTestContext(t)
		canceled = ctx.Done()
		t.Cleanup(func() {
			if err := ctx.Err(); err != nil {
				t.Errorf("fixture context canceled before resource cleanup: %v", err)
			}
		})
	})
	select {
	case <-canceled:
	default:
		t.Fatal("fixture context was not canceled after cleanup")
	}
}

func TestRealServerFixtureCommandRunsCleanupOnDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	cleanupMarker := filepath.Join(t.TempDir(), "cleanup-ran")
	command := fixtureCommand(ctx, "bash", "-c", `
trap 'printf cleaned > "$FIXTURE_CLEANUP_MARKER"; exit 143' TERM
while :; do :; done
`)
	command.Env = fixtureEnvironment(map[string]string{"FIXTURE_CLEANUP_MARKER": cleanupMarker})
	err := command.Run()
	var exitErr *osexec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 143 {
		t.Fatalf("deadline cancellation exit = %v, want graceful shell exit 143", err)
	}
	if marker, err := os.ReadFile(cleanupMarker); err != nil || string(marker) != "cleaned" {
		t.Fatalf("deadline cancellation did not run fixture cleanup: marker=%q err=%v", marker, err)
	}
}

func TestRealServerFixtureCommandRunsCleanupWhileChildActive(t *testing.T) {
	binDir := t.TempDir()
	childActiveMarker := filepath.Join(binDir, "child-active")
	docker := filepath.Join(binDir, "docker")
	if err := os.WriteFile(docker, []byte(`#!/usr/bin/env bash
set -euo pipefail
printf active > "$FIXTURE_CHILD_ACTIVE_MARKER"
exec /bin/sleep 2
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(docker, 0o700); err != nil { //nolint:gosec // The test invokes its temporary Docker stub.
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cleanupMarker := filepath.Join(t.TempDir(), "cleanup-ran")
	command := fixtureCommand(ctx, "bash", "-c", `
trap 'printf cleaned > "$FIXTURE_CLEANUP_MARKER"; exit 143' TERM
docker container inspect fixture
`)
	command.WaitDelay = 500 * time.Millisecond
	command.Env = fixtureEnvironment(map[string]string{
		"PATH":                        binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"FIXTURE_CHILD_ACTIVE_MARKER": childActiveMarker,
		"FIXTURE_CLEANUP_MARKER":      cleanupMarker,
	})
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}

	childActive := false
	var markerErr error
	startupDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(startupDeadline) {
		if _, err := os.Stat(childActiveMarker); err == nil {
			childActive = true
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			markerErr = err
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	err := command.Wait()
	if markerErr != nil {
		t.Fatalf("inspect active child marker: %v", markerErr)
	}
	if !childActive {
		t.Fatalf("foreground Docker child did not become active: %v", err)
	}
	var exitErr *osexec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 143 {
		t.Fatalf("active-child cancellation exit = %v, want graceful shell exit 143", err)
	}
	if marker, err := os.ReadFile(cleanupMarker); err != nil || string(marker) != "cleaned" {
		t.Fatalf("active-child cancellation did not run fixture cleanup: marker=%q err=%v", marker, err)
	}
}

func realFixtureTestContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	// Cleanups run in reverse order, after resource cleanup registered by the caller.
	t.Cleanup(cancel)
	return ctx
}

func TestRealServerFixturePreservesLaunchOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := &realFixtureProcess{ready: realFixtureReady{RunID: "a"}}
		b := &realFixtureProcess{ready: realFixtureReady{RunID: "b"}}
		bStarted := make(chan struct{})
		fixtures, err := startConcurrentRealFixtures([]string{"a", "b"}, func(id string) (*realFixtureProcess, error) {
			if id == "a" {
				<-bStarted
				// Model a slower first startup after the second startup returns.
				time.Sleep(20 * time.Millisecond)
				return a, nil
			}
			defer close(bStarted)
			return b, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(fixtures) != 2 || fixtures[0] != a || fixtures[1] != b {
			t.Fatal("concurrent fixtures were returned in completion order instead of launch order")
		}
	})
}

func startConcurrentRealFixtures(runIDs []string, start func(string) (*realFixtureProcess, error)) ([]*realFixtureProcess, error) {
	type startResult struct {
		index   int
		fixture *realFixtureProcess
		err     error
	}
	started := make(chan startResult, len(runIDs))
	var wait sync.WaitGroup
	for index, runID := range runIDs {
		wait.Add(1)
		go func(index int, id string) {
			defer wait.Done()
			fixture, err := start(id)
			started <- startResult{index: index, fixture: fixture, err: err}
		}(index, runID)
	}
	wait.Wait()
	close(started)
	fixtures := make([]*realFixtureProcess, len(runIDs))
	var startErr error
	for result := range started {
		if result.err != nil {
			startErr = errors.Join(startErr, result.err)
		} else {
			fixtures[result.index] = result.fixture
		}
	}
	if startErr != nil {
		for _, fixture := range fixtures {
			if fixture != nil {
				startErr = errors.Join(startErr, fixture.stopBySignal())
			}
		}
		return nil, startErr
	}
	return fixtures, nil
}

func TestRealServerFixture(t *testing.T) {
	ctx := realFixtureTestContext(t)
	serverRevision := currentServerRevision(t)
	runA, runB := newRealFixtureRunID(t), newRealFixtureRunID(t)
	if runA == runB {
		t.Fatal("random fixture run IDs collided")
	}

	fixtures, err := startConcurrentRealFixtures([]string{runA, runB}, func(id string) (*realFixtureProcess, error) {
		return startRealServerFixture(ctx, id, serverRevision, realFixtureWebRevision, nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fixtures) != 2 {
		t.Fatalf("ready fixtures = %d, want 2", len(fixtures))
	}
	a, b := fixtures[0], fixtures[1]
	t.Cleanup(func() {
		for _, fixture := range fixtures {
			if !fixture.stopped {
				if err := fixture.stopBySignal(); err != nil {
					t.Errorf("stop fixture during test cleanup: %v", err)
				}
			}
		}
	})
	validateRealFixtureReady(t, a, runA, serverRevision, realFixtureWebRevision)
	validateRealFixtureReady(t, b, runB, serverRevision, realFixtureWebRevision)
	if a.ready.RunID == b.ready.RunID || fixtureUsername(runA, 0) == fixtureUsername(runB, 0) {
		t.Fatal("concurrent fixtures reused a run identity or synthetic username")
	}
	if a.ready.Fingerprint == b.ready.Fingerprint {
		t.Fatal("concurrent fixtures reused an MTProto RSA identity")
	}

	artifactA := filepath.Join(t.TempDir(), "private-web-artifact")
	writeFixtureArtifact(t, artifactA, a.ready, realFixtureWebRevision, false)
	callerIndex, err := os.ReadFile(filepath.Join(artifactA, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	var artifactReady *realFixtureArtifactReady
	t.Run("ArtifactAttachment", func(t *testing.T) {
		var attachErr error
		artifactReady, attachErr = a.attachArtifact(artifactA)
		if attachErr != nil {
			t.Fatalf("attach the run-A private artifact: %v", attachErr)
		}
		validateRealFixtureArtifactReady(t, artifactReady, a.ready, realFixtureWebRevision)
	})

	marker := "fixture-isolation-" + newRealFixtureRunID(t)
	if output, err := sendFixtureMessage(ctx, a, fixtureUsername(runA, 1), marker); err != nil {
		t.Fatalf("send isolation probe: %v: %s", err, output)
	}
	if got := fixtureSQL(ctx, t, a, "SELECT count(*) FROM messages WHERE message = '"+marker+"'"); got != "2" {
		t.Fatalf("sender fixture message rows = %q, want 2", got)
	}
	if got := fixtureSQL(ctx, t, b, "SELECT count(*) FROM messages WHERE message = '"+marker+"'"); got != "0" {
		t.Fatalf("second fixture can see first fixture's message: rows=%q", got)
	}
	if got := fixtureSQL(ctx, t, b, "SELECT count(*) FROM auth_keys WHERE user_id IS NOT NULL"); got != "2" {
		t.Fatalf("fresh second fixture auth keys = %q, want only its two new client sessions", got)
	}

	if ready, err := b.attachArtifact(artifactA); err == nil || ready != nil {
		t.Fatalf("run B accepted run A's target manifest: ready=%+v err=%v", ready, err)
	}
	if !strings.Contains(b.stderr.String(), "manifestFingerprint") {
		t.Fatalf("cross-run artifact rejection did not identify the target mismatch: %q", b.stderr.String())
	}
	assertFixtureCleanupVerified(t, b.stderr.String())
	assertNamedFixtureResourcesAbsent(t, runB)
	if got, err := os.ReadFile(filepath.Join(artifactA, "index.html")); err != nil || !bytes.Equal(got, callerIndex) {
		t.Fatalf("fixture modified or removed caller-owned artifact: err=%v", err)
	}
	if output, err := sendFixtureMessage(ctx, a, fixtureUsername(runA, 1), marker+"-after-b-cleanup"); err != nil {
		t.Fatalf("run A stopped when run B rejected its artifact: %v: %s", err, output)
	}

	volumesA := fixtureContainerVolumeNames(t, fixtureResourcePrefix(a.ready.RunID))
	if err := a.stopByEOF(); err != nil {
		t.Fatalf("stop first fixture: %v", err)
	}
	assertFixtureResourcesAbsent(t, a.ready, volumesA)
}

func TestRealServerFixtureArtifactAttachmentFailureCleanup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	runID := newRealFixtureRunID(t)
	fixture, err := startRealServerFixture(ctx, runID, currentServerRevision(t), realFixtureWebRevision, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ready, err := fixture.attachArtifact(filepath.Join(t.TempDir(), "missing-artifact")); err == nil || ready != nil {
		t.Fatalf("fixture accepted a missing artifact: ready=%+v err=%v", ready, err)
	}
	if strings.Contains(fixture.stderr.String(), "artifact-ready") || !strings.Contains(fixture.stderr.String(), "artifact path is missing or unreadable") {
		t.Fatalf("missing artifact failure was not fail-closed: %q", fixture.stderr.String())
	}
	assertFixtureCleanupVerified(t, fixture.stderr.String())
	assertNamedFixtureResourcesAbsent(t, runID)
}

func TestRealServerFixtureArtifactProductHostAttemptFailsRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	runID := newRealFixtureRunID(t)
	fixture, err := startRealServerFixture(ctx, runID, currentServerRevision(t), realFixtureWebRevision, nil)
	if err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(t.TempDir(), "product-attempt-artifact")
	writeFixtureArtifact(t, artifact, fixture.ready, realFixtureWebRevision, true)
	if ready, err := fixture.attachArtifact(artifact); err == nil || ready != nil {
		t.Fatalf("fixture reported readiness for an artifact with product-host attempts: ready=%+v err=%v", ready, err)
	}
	stderr := fixture.stderr.String()
	if strings.Contains(stderr, "artifact-ready") {
		t.Fatalf("fixture advertised artifact readiness after an unexpected attempt: %q", stderr)
	}
	if strings.Contains(stderr, "artifact audit failed") {
		t.Fatalf("permitted product references were rejected as staged content: %q", stderr)
	}
	if !strings.Contains(stderr, "unexpected_attempt_details") {
		t.Fatalf("artifact browser failure did not report the observed attempts: %q", stderr)
	}
	for _, attempt := range []string{
		"https://telegram.org/fixture/missing.png",
		"https://t.me/botfather",
		"https://telesco.pe/fixture-ping",
	} {
		if !strings.Contains(stderr, attempt) {
			t.Errorf("observer did not record the %s attempt: %q", attempt, stderr)
		}
	}
	assertFixtureCleanupVerified(t, stderr)
	assertNamedFixtureResourcesAbsent(t, runID)
}

func TestRealServerFixtureCancellationDuringAttachmentCleanup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	runID := newRealFixtureRunID(t)
	fixture, err := startRealServerFixture(ctx, runID, currentServerRevision(t), realFixtureWebRevision, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !fixture.stopped {
			if err := fixture.stopBySignal(); err != nil {
				t.Errorf("stop fixture during test cleanup: %v", err)
			}
		}
	})
	validateRealFixtureReady(t, fixture, runID, currentServerRevision(t), realFixtureWebRevision)
	if err := fixture.stopBySignal(); err != nil {
		t.Fatalf("cancel fixture while it waits for artifact attachment: %v", err)
	}
	assertFixtureCleanupVerified(t, fixture.stderr.String())
	assertNamedFixtureResourcesAbsent(t, runID)
}

func TestRealServerFixtureStartupFailureCleanup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	serverRevision := currentServerRevision(t)
	runID := newRealFixtureRunID(t)
	command := fixtureCommand(ctx, "bash", filepath.Join("real_server_fixture", "run.sh"),
		"--server-revision", serverRevision,
		"--web-revision", realFixtureWebRevision,
		"--run-id", runID,
	)
	command.Env = append(os.Environ(), "TELEGRAM_FIXTURE_TEST_FAIL_AFTER=server-ready")
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if err == nil {
		t.Fatal("injected startup failure returned success")
	}
	var exitErr *osexec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 42 {
		t.Fatalf("injected startup failure = %v, want exit 42; stderr=%q", err, stderr.String())
	}
	assertServerReadyRecord(t, stdout.Bytes())
	if strings.Contains(stdout.String(), "artifact-ready") {
		t.Fatalf("failed fixture advertised artifact readiness: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "injected startup failure after server-ready") {
		t.Fatalf("failure diagnostic = %q", stderr.String())
	}
	assertFixtureCleanupVerified(t, stderr.String())
	assertNamedFixtureResourcesAbsent(t, runID)
}

func TestRealServerFixtureReadinessTimeoutCleanup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	runID := newRealFixtureRunID(t)
	command := fixtureCommand(ctx, "bash", filepath.Join("real_server_fixture", "run.sh"),
		"--server-revision", currentServerRevision(t),
		"--web-revision", realFixtureWebRevision,
		"--run-id", runID,
	)
	command.Env = append(os.Environ(), "TELEGRAM_FIXTURE_TEST_READINESS_TIMEOUT=1")
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	var exitErr *osexec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("injected readiness timeout exit = %v, want exit 1; stderr=%q", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "telegramd readiness timeout") {
		t.Fatalf("readiness timeout diagnostic = %q", stderr.String())
	}
	assertFixtureCleanupVerified(t, stderr.String())
	if stdout.Len() != 0 {
		t.Fatalf("unready fixture wrote readiness output %q", stdout.String())
	}
	assertNamedFixtureResourcesAbsent(t, runID)
}

func TestRealServerFixtureCleanupFailureIsNonzero(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	runID := newRealFixtureRunID(t)
	command := fixtureCommand(ctx, "bash", filepath.Join("real_server_fixture", "run.sh"),
		"--server-revision", currentServerRevision(t),
		"--web-revision", realFixtureWebRevision,
		"--run-id", runID,
	)
	command.Env = append(os.Environ(), "TELEGRAM_FIXTURE_TEST_FAIL_AFTER=server-ready", "TELEGRAM_FIXTURE_TEST_CLEANUP_FAILURE=1")
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	var exitErr *osexec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("injected cleanup failure exit = %v, want exit 1; stderr=%q", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "injected cleanup failure") || !strings.Contains(stderr.String(), "cleanup=failed") {
		t.Fatalf("cleanup failure diagnostic = %q", stderr.String())
	}
	for _, diagnostic := range []string{"cleanup left owned volume", "cleanup could not record volume mounts", "cleanup could not verify volume"} {
		if strings.Contains(stderr.String(), diagnostic) {
			t.Fatalf("owned container volume cleanup failed: %q", stderr.String())
		}
	}
	assertServerReadyRecord(t, stdout.Bytes())
	if strings.Contains(stdout.String(), "artifact-ready") {
		t.Fatalf("fixture with a cleanup failure advertised artifact readiness %q", stdout.String())
	}
	assertNamedFixtureResourcesAbsent(t, runID)
}

func TestRealServerFixtureRejectsTargetKeyMismatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	runID := newRealFixtureRunID(t)
	command := fixtureCommand(ctx, "bash", filepath.Join("real_server_fixture", "run.sh"),
		"--server-revision", currentServerRevision(t),
		"--web-revision", realFixtureWebRevision,
		"--run-id", runID,
	)
	command.Env = append(os.Environ(), "TELEGRAM_FIXTURE_TEST_MISMATCH_TARGET=1")
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	var exitErr *osexec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("target key mismatch exit = %v, want exit 1; stderr=%q", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), `"code":"target_manifest_mismatch"`) {
		t.Fatalf("target key mismatch diagnostic = %q", stderr.String())
	}
	assertFixtureCleanupVerified(t, stderr.String())
	if stdout.Len() != 0 {
		t.Fatalf("mismatched fixture wrote readiness output %q", stdout.String())
	}
	assertNamedFixtureResourcesAbsent(t, runID)
}

type realFixtureEvidence struct {
	HTTPStatus                  int                     `json:"httpStatus"`
	WSSUpgradeStatus            int                     `json:"wssUpgradeStatus"`
	AllowedWSSObserved          int                     `json:"allowedWssObserved"`
	WorkerProbes                map[string]fixtureProbe `json:"workerProbes"`
	ObserverControlledAttempts  map[string]int          `json:"observerControlledAttempts"`
	DirectTCP                   fixtureProbe            `json:"directTCP"`
	UnexpectedAttempts          int                     `json:"unexpectedAttempts"`
	UnexpectedDetectionVerified bool                    `json:"unexpectedDetectionVerified"`
}

type fixtureProbe struct {
	Attempted int `json:"attempted"`
	Blocked   int `json:"blocked"`
}

type realFixtureSecurity struct {
	RegistrationClosed  bool `json:"registrationClosed"`
	LoginCodeLogging    bool `json:"loginCodeLogging"`
	ElectionClosed      bool `json:"electionClosed"`
	AdministratorIsNull bool `json:"administratorIsNull"`
	OrdinaryUsers       int  `json:"ordinaryUsers"`
	UsernameAccounts    int  `json:"usernameAccounts"`
	PasswordVerifiers   int  `json:"passwordVerifiers"`
	InitialAuthKeys     int  `json:"initialAuthKeys"`
	InitialMessages     int  `json:"initialMessages"`
	FinalAuthKeys       int  `json:"finalAuthKeys"`
}

type realFixtureReady struct {
	Event           string              `json:"event"`
	Status          string              `json:"status"`
	RunID           string              `json:"runId"`
	Mode            string              `json:"mode"`
	HarnessRevision string              `json:"harnessRevision"`
	ServerRevision  string              `json:"serverRevision"`
	WebRevision     string              `json:"webRevision"`
	EvidenceClass   string              `json:"evidenceClass"`
	Endpoint        string              `json:"endpoint"`
	WSSEndpoint     string              `json:"wssEndpoint"`
	PublicKeyPEM    string              `json:"mtprotoPublicKeyPEM"`
	PublicKeySHA256 string              `json:"publicKeySHA256"`
	Fingerprint     string              `json:"fingerprint"`
	Security        realFixtureSecurity `json:"security"`
	Evidence        realFixtureEvidence `json:"evidence"`
}

type realFixtureArtifactBrowserEvidence struct {
	Status                          string         `json:"status"`
	EntrySHA256                     string         `json:"entrySHA256"`
	ManifestSHA256                  string         `json:"manifestSHA256"`
	EntryResponseStatus             int            `json:"entryResponseStatus"`
	ManifestResponseStatus          int            `json:"manifestResponseStatus"`
	ArtifactResponses               int            `json:"artifactResponses"`
	ArtifactResponsesWithPrivateCSP int            `json:"artifactResponsesWithPrivateCSP"`
	WorkerTargets                   map[string]int `json:"workerTargets"`
	UnexpectedAttempts              int            `json:"unexpectedAttempts"`
	ObserverErrors                  int            `json:"observerErrors"`
}

type realFixtureArtifactProductReference struct {
	File      string `json:"file"`
	Reference string `json:"reference"`
}

type realFixtureArtifactReady struct {
	Event                 string                                `json:"event"`
	Status                string                                `json:"status"`
	RunID                 string                                `json:"runId"`
	HarnessRevision       string                                `json:"harnessRevision"`
	ServerRevision        string                                `json:"serverRevision"`
	WebRevision           string                                `json:"webRevision"`
	Endpoint              string                                `json:"endpoint"`
	WSSEndpoint           string                                `json:"wssEndpoint"`
	Fingerprint           string                                `json:"fingerprint"`
	ArtifactDigest        string                                `json:"artifactDigest"`
	ManifestSHA256        string                                `json:"manifestSHA256"`
	IndexSHA256           string                                `json:"indexSHA256"`
	FileCount             int                                   `json:"fileCount"`
	TotalBytes            int64                                 `json:"totalBytes"`
	AuditChecks           map[string]bool                       `json:"auditChecks"`
	ProductReferences     []realFixtureArtifactProductReference `json:"productReferences"`
	ProductReferenceCount int                                   `json:"productReferenceCount"`
	Browser               realFixtureArtifactBrowserEvidence    `json:"browser"`
}

type realFixtureProcess struct {
	cmd       *osexec.Cmd
	stdin     io.WriteCloser
	stdout    *bufio.Reader
	ready     realFixtureReady
	readyLine string
	stderr    *synchronizedBuffer
	stopped   bool
}

type synchronizedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *synchronizedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(data)
}

func (b *synchronizedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

func startRealServerFixture(ctx context.Context, runID, serverRevision, webRevision string, extraEnv []string) (*realFixtureProcess, error) {
	command := fixtureCommand(ctx, "bash", filepath.Join("real_server_fixture", "run.sh"),
		"--server-revision", serverRevision,
		"--web-revision", webRevision,
		"--run-id", runID,
	)
	command.Env = append(os.Environ(), extraEnv...)
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("fixture stdout pipe: %w", err)
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("fixture stdin pipe: %w", err)
	}
	stderr := &synchronizedBuffer{}
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start real-server fixture: %w", err)
	}
	reader := bufio.NewReader(stdout)
	line, err := reader.ReadString('\n')
	if err != nil {
		closeErr := stdin.Close()
		waitErr := command.Wait()
		return nil, fmt.Errorf("fixture did not report readiness (stderr=%q): %w", stderr.String(), errors.Join(err, closeErr, waitErr))
	}
	var ready realFixtureReady
	if err := json.Unmarshal([]byte(line), &ready); err != nil {
		closeErr := stdin.Close()
		waitErr := command.Wait()
		return nil, fmt.Errorf("decode fixture readiness: %w", errors.Join(err, closeErr, waitErr))
	}
	fixture := &realFixtureProcess{cmd: command, stdin: stdin, stdout: reader, ready: ready, readyLine: line, stderr: stderr}
	return fixture, nil
}

func (f *realFixtureProcess) attachArtifact(path string) (*realFixtureArtifactReady, error) {
	if f.stopped {
		return nil, errors.New("fixture process is already stopped")
	}
	if !filepath.IsAbs(path) {
		return nil, errors.New("test artifact path must be absolute")
	}
	if _, err := io.WriteString(f.stdin, "attach "+path+"\n"); err != nil {
		return nil, fmt.Errorf("write fixture attachment command: %w", err)
	}
	line, err := f.stdout.ReadString('\n')
	if err != nil {
		closeErr := f.stdin.Close()
		waitErr := f.cmd.Wait()
		f.stopped = true
		return nil, fmt.Errorf("fixture did not report artifact readiness (stderr=%q): %w", f.stderr.String(), errors.Join(err, closeErr, waitErr))
	}
	var ready realFixtureArtifactReady
	if err := json.Unmarshal([]byte(line), &ready); err != nil {
		closeErr := f.stdin.Close()
		waitErr := f.cmd.Wait()
		f.stopped = true
		return nil, fmt.Errorf("decode fixture artifact readiness: %w", errors.Join(err, closeErr, waitErr))
	}
	if ready.Event != "artifact-ready" || ready.Status != "ready" {
		return nil, fmt.Errorf("fixture artifact readiness record is invalid: event=%q status=%q", ready.Event, ready.Status)
	}
	return &ready, nil
}

func (f *realFixtureProcess) stopByEOF() error {
	if f.stopped {
		return nil
	}
	if err := f.stdin.Close(); err != nil {
		return fmt.Errorf("close fixture control stream: %w", err)
	}
	err := f.cmd.Wait()
	f.stopped = true
	if err != nil {
		return fmt.Errorf("fixture cleanup failed: %w: %s", err, f.stderr.String())
	}
	return nil
}

func (f *realFixtureProcess) stopBySignal() error {
	if f.stopped {
		return nil
	}
	if err := f.cmd.Cancel(); err != nil {
		return fmt.Errorf("signal fixture cleanup: %w", err)
	}
	err := f.cmd.Wait()
	f.stopped = true
	var exitErr *osexec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 143 {
		return fmt.Errorf("fixture cancellation exit = %w, want 143: %s", err, f.stderr.String())
	}
	return nil
}

func currentServerRevision(t *testing.T) string {
	t.Helper()
	output, err := fixtureCommand(context.Background(), "git", "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("read server revision: %v", err)
	}
	return strings.TrimSpace(string(output))
}

func newRealFixtureRunID(t *testing.T) string {
	t.Helper()
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		t.Fatalf("generate fixture identity: %v", err)
	}
	return hex.EncodeToString(random)
}

func validateRealFixtureReady(t *testing.T, fixture *realFixtureProcess, runID, serverRevision, webRevision string) {
	t.Helper()
	assertServerReadyRecord(t, []byte(fixture.readyLine))
	ready := fixture.ready
	if ready.Event != "server-ready" || ready.Status != "ready" || ready.RunID != runID || ready.ServerRevision != serverRevision || ready.WebRevision != webRevision {
		t.Fatalf("fixture readiness metadata does not match immutable inputs: event=%q status=%q run=%q server=%q web=%q", ready.Event, ready.Status, ready.RunID, ready.ServerRevision, ready.WebRevision)
	}
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(ready.HarnessRevision) {
		t.Fatalf("fixture harness revision is not a full SHA: %q", ready.HarnessRevision)
	}
	if ready.Mode != "private" {
		t.Fatalf("fixture target mode = %q, want private", ready.Mode)
	}
	if ready.EvidenceClass != "production-telegramd" {
		t.Fatalf("fixture evidence class = %q, want production-telegramd", ready.EvidenceClass)
	}
	if ready.Endpoint != "https://telegramd.test" || ready.WSSEndpoint != "wss://telegramd.test/apiws" {
		t.Fatalf("fixture endpoints = %q / %q, want fixed synthetic origin", ready.Endpoint, ready.WSSEndpoint)
	}
	if ready.Fingerprint == "fbb62871f07fae2a" || !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(ready.Fingerprint) {
		t.Fatalf("fixture fingerprint is invalid or matches the production identity: %q", ready.Fingerprint)
	}
	block, rest := pemDecode([]byte(ready.PublicKeyPEM))
	if block == nil || len(strings.TrimSpace(string(rest))) != 0 {
		t.Fatal("fixture did not return a single PEM public key")
	}
	publicKey, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		t.Fatalf("parse fixture public key: %v", err)
	}
	rsaPublicKey, ok := publicKey.(*rsa.PublicKey)
	if !ok || rsaPublicKey.N.BitLen() != 2048 || rsaPublicKey.E != 65537 {
		t.Fatalf("fixture public key = %T, want RSA-2048/65537", publicKey)
	}
	fingerprint := rsakey.Fingerprint(rsaPublicKey)
	//nolint:gosec // Preserve the MTProto fingerprint's full 64-bit wire representation.
	if got := fmt.Sprintf("%016x", uint64(fingerprint)); got != ready.Fingerprint {
		t.Fatalf("fixture public key fingerprint = %q, descriptor says %q", got, ready.Fingerprint)
	}
	publicKeyHash := sha256.Sum256([]byte(ready.PublicKeyPEM))
	if got := hex.EncodeToString(publicKeyHash[:]); got != ready.PublicKeySHA256 {
		t.Fatalf("inline public key SHA-256 = %q, descriptor says %q", got, ready.PublicKeySHA256)
	}
	if fixtureUsername(runID, 0) == fixtureUsername(runID, 1) {
		t.Fatal("fixture synthetic usernames are not distinct")
	}
	if !ready.Security.RegistrationClosed || ready.Security.LoginCodeLogging || !ready.Security.ElectionClosed || !ready.Security.AdministratorIsNull || ready.Security.OrdinaryUsers != 2 || ready.Security.UsernameAccounts != 2 || ready.Security.PasswordVerifiers != 2 || ready.Security.InitialAuthKeys != 0 || ready.Security.InitialMessages != 0 || ready.Security.FinalAuthKeys != 2 {
		t.Fatalf("fixture security state did not hold before and after authentication: %+v", ready.Security)
	}
	if ready.Evidence.HTTPStatus != 200 || ready.Evidence.WSSUpgradeStatus != 101 || ready.Evidence.AllowedWSSObserved != 1 {
		t.Fatalf("fixture HTTPS/WSS evidence = %+v", ready.Evidence)
	}
	for _, contextName := range []string{"page", "shared_worker", "service_worker"} {
		probe, ok := ready.Evidence.WorkerProbes[contextName]
		if !ok || probe.Attempted != 8 || probe.Blocked != 8 || ready.Evidence.ObserverControlledAttempts[contextName] != 8 {
			t.Fatalf("%s egress probe was skipped, unblocked, or unobserved: probe=%+v evidence=%+v", contextName, probe, ready.Evidence.ObserverControlledAttempts)
		}
	}
	if ready.Evidence.DirectTCP.Attempted != 5 || ready.Evidence.DirectTCP.Blocked != 5 || ready.Evidence.UnexpectedAttempts != 0 || !ready.Evidence.UnexpectedDetectionVerified {
		t.Fatalf("fixture egress boundary evidence = %+v", ready.Evidence)
	}
}

func validateRealFixtureArtifactReady(t *testing.T, ready *realFixtureArtifactReady, serverReady realFixtureReady, webRevision string) {
	t.Helper()
	if ready.Event != "artifact-ready" || ready.Status != "ready" || ready.RunID != serverReady.RunID ||
		ready.HarnessRevision != serverReady.HarnessRevision || ready.ServerRevision != serverReady.ServerRevision ||
		ready.WebRevision != webRevision || ready.Endpoint != serverReady.Endpoint || ready.WSSEndpoint != serverReady.WSSEndpoint ||
		ready.Fingerprint != serverReady.Fingerprint {
		t.Fatalf("artifact readiness did not match the server-ready inputs: %+v", ready)
	}
	if !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(ready.ArtifactDigest) {
		t.Fatalf("artifact digest is invalid: %q", ready.ArtifactDigest)
	}
	if len(ready.AuditChecks) == 0 {
		t.Fatal("artifact readiness omitted fixture-owned audit checks")
	}
	for name, passed := range ready.AuditChecks {
		if !passed {
			t.Errorf("fixture-owned artifact audit %q failed", name)
		}
	}
	if ready.Browser.Status != "passed" || ready.Browser.EntrySHA256 != ready.IndexSHA256 ||
		ready.Browser.ManifestSHA256 != ready.ManifestSHA256 || ready.Browser.EntryResponseStatus != 200 ||
		ready.Browser.ManifestResponseStatus != 200 || ready.Browser.ArtifactResponses == 0 ||
		ready.Browser.ArtifactResponsesWithPrivateCSP != ready.Browser.ArtifactResponses ||
		ready.Browser.UnexpectedAttempts != 0 || ready.Browser.ObserverErrors != 0 {
		t.Fatalf("fresh production artifact browser evidence is incomplete: %+v", ready.Browser)
	}
	if ready.Browser.WorkerTargets["shared_worker"] == 0 || ready.Browser.WorkerTargets["service_worker"] == 0 {
		t.Fatalf("artifact worker targets were not independently observed: %+v", ready.Browser.WorkerTargets)
	}
	recorded := make(map[string]string, len(ready.ProductReferences))
	for _, reference := range ready.ProductReferences {
		if reference.File == "" || reference.Reference == "" {
			t.Fatalf("product reference evidence is missing its staged location: %+v", reference)
		}
		recorded[reference.Reference] = reference.File
	}
	for _, reference := range fixtureProductReferences {
		if _, ok := recorded[reference]; !ok {
			t.Fatalf("audit evidence omitted the permitted product reference %q: %+v", reference, ready.ProductReferences)
		}
	}
	if ready.ProductReferenceCount < len(fixtureProductReferences) {
		t.Fatalf("product reference count = %d, want at least %d", ready.ProductReferenceCount, len(fixtureProductReferences))
	}
}

func assertServerReadyRecord(t *testing.T, output []byte) {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) != 1 || lines[0] == "" {
		t.Fatalf("fixture did not emit exactly one server-ready record: %q", output)
	}
	var record map[string]json.RawMessage
	if err := json.Unmarshal([]byte(lines[0]), &record); err != nil {
		t.Fatalf("decode server-ready record: %v", err)
	}
	var event, status string
	if err := json.Unmarshal(record["event"], &event); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(record["status"], &status); err != nil {
		t.Fatal(err)
	}
	if event != "server-ready" || status != "ready" {
		t.Fatalf("readiness record event/status = %q/%q", event, status)
	}
	allowed := map[string]struct{}{
		"event": {}, "status": {}, "runId": {}, "harnessRevision": {}, "serverRevision": {}, "webRevision": {},
		"evidenceClass": {}, "endpoint": {}, "wssEndpoint": {}, "mode": {}, "mtprotoPublicKeyPEM": {},
		"publicKeySHA256": {}, "fingerprint": {}, "security": {}, "evidence": {},
	}
	for name := range allowed {
		if _, exists := record[name]; !exists {
			t.Fatalf("server-ready omitted required field %q", name)
		}
	}
	for name := range record {
		if _, exists := allowed[name]; !exists {
			t.Fatalf("server-ready exposed an unapproved field %q", name)
		}
	}
	if bytes.Contains(output, []byte("PRIVATE KEY")) {
		t.Fatal("server-ready exposed private key material")
	}
}

func fixtureResourcePrefix(runID string) string {
	return "telegram-fixture-" + runID
}

func fixtureUsername(runID string, index int) string {
	if len(runID) < 30 {
		return "u" + runID + strconv.Itoa(index)
	}
	suffix := "a"
	if index == 1 {
		suffix = "b"
	}
	return "u" + runID[:30] + suffix
}

func writeFixtureArtifact(t *testing.T, directory string, ready realFixtureReady, webRevision string, productHostAttempts bool) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(directory, "assets"), 0o700); err != nil {
		t.Fatal(err)
	}
	endpoint := "wss://telegramd.test/apiws"
	fingerprint := ready.Fingerprint
	csp := "default-src 'self'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'; " +
		"object-src 'none'; script-src 'self' 'wasm-unsafe-eval'; style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data: blob:; font-src 'self' data:; media-src 'self' blob:; " +
		"worker-src 'self' blob:; manifest-src 'self'; connect-src 'self' " + endpoint + ";"
	productLinks := "const productLinks = [" + strings.Join(quoteAll(fixtureProductReferences), ", ") + "];\n"
	appJS := "const target = { endpoint: '" + endpoint + "', fingerprint: '" + fingerprint + "' };\n" +
		"window.fixtureTarget = target;\n" +
		productLinks +
		"const worker = new SharedWorker('/shared-worker.js'); worker.port.start();\n" +
		"navigator.serviceWorker.register('./service-worker.js', { type: 'module', scope: './' });\n" +
		"const socket = new WebSocket(target.endpoint); socket.addEventListener('open', () => socket.close(), { once: true });\n"
	indexHTML := "<!doctype html><html><head><meta charset=\"utf-8\"><meta http-equiv=\"Content-Security-Policy\" content=\"" + csp +
		"\"><link rel=\"stylesheet\" href=\"/assets/app.css?cache=1\"></head><body><main>fixture bundle</main><script type=\"module\" src=\"/assets/app.js?cache=1\"></script></body></html>\n"
	if productHostAttempts {
		appJS += "fetch('https://t.me/botfather', { mode: 'no-cors' }).catch(() => {});\n"
		indexHTML = strings.Replace(indexHTML, "<main>fixture bundle</main>",
			"<main>fixture bundle</main><img src=\"https://telegram.org/fixture/missing.png\" alt=\"\">"+
				"<a href=\"https://t.me/botfather\" ping=\"https://telesco.pe/fixture-ping\">links</a>", 1)
	}
	files := map[string][]byte{
		"assets/app.css": []byte("body { color: rgb(1, 2, 3); }\n"),
		"assets/app.js":  []byte(appJS),
		"index.html":     []byte(indexHTML),
		"service-worker.js": []byte("self.addEventListener('install', event => event.waitUntil(self.skipWaiting()));\n" +
			"self.addEventListener('activate', event => event.waitUntil(self.clients.claim()));\n"),
		"shared-worker.js": []byte("onconnect = event => { const port = event.ports[0]; port.start(); };\n"),
	}
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	digest := sha256.New()
	for _, path := range paths {
		content := files[path]
		_, _ = digest.Write([]byte(path + "\x00" + strconv.Itoa(len(content)) + "\x00"))
		_, _ = digest.Write(content)
		_, _ = digest.Write([]byte{0})
		fullPath := filepath.Join(directory, filepath.FromSlash(path))
		if err := os.WriteFile(fullPath, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manifest := struct {
		Mode           string `json:"mode"`
		Endpoint       string `json:"endpoint"`
		Fingerprint    string `json:"fingerprint"`
		SourceCommit   string `json:"sourceCommit"`
		ArtifactDigest string `json:"artifactDigest"`
	}{"private", endpoint, fingerprint, webRevision, "sha256:" + hex.EncodeToString(digest.Sum(nil))}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	manifestBytes = append(manifestBytes, '\n')
	if err := os.WriteFile(filepath.Join(directory, "mtproto-target.json"), manifestBytes, 0o600); err != nil {
		t.Fatal(err)
	}
}

func quoteAll(values []string) []string {
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, "'"+value+"'")
	}
	return quoted
}

func sendFixtureMessage(ctx context.Context, fixture *realFixtureProcess, recipient, marker string) (string, error) {
	command := fixtureCommand(ctx, "docker", "exec", fixtureResourcePrefix(fixture.ready.RunID)+"client",
		"/run/app/fixture-auth-check", "--endpoint", "wss://telegramd.test/apiws",
		"--tls-root-file", "/run/secrets/tls.crt",
		"--public-key", "/run/secrets/server.pub.pem",
		"--username", fixtureUsername(fixture.ready.RunID, 0),
		"--password-file", "/run/secrets/a-password",
		"--send-message-to", recipient,
		"--message", marker,
	)
	output, err := command.CombinedOutput()
	return strings.TrimSpace(string(output)), err
}

func fixtureSQL(ctx context.Context, t *testing.T, fixture *realFixtureProcess, query string) string {
	t.Helper()
	command := fixtureCommand(ctx, "docker", "exec", fixtureResourcePrefix(fixture.ready.RunID)+"database",
		"psql", "-U", "postgres", "-d", "telegram", "-X", "-qAt", "-c", query)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("fixture database assertion failed: %v: %s", err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output))
}

func assertFixtureResourcesAbsent(t *testing.T, ready realFixtureReady, containerVolumes map[string][]string) {
	t.Helper()
	var volumeNames []string
	for _, volumes := range containerVolumes {
		volumeNames = append(volumeNames, volumes...)
	}
	assertNamedFixtureResourcesAbsent(t, ready.RunID, volumeNames...)
}

func fixtureContainerVolumeNames(t *testing.T, resourcePrefix string) map[string][]string {
	t.Helper()
	volumesByContainer := make(map[string][]string, 2)
	for _, container := range []struct {
		name   string
		suffix string
	}{
		{name: "backend", suffix: "telegramd"},
		{name: "client", suffix: "client"},
	} {
		command := fixtureCommand(context.Background(), "docker", "inspect", "--format",
			`{{range .Mounts}}{{if eq .Type "volume"}}{{.Name}}{{"\n"}}{{end}}{{end}}`,
			resourcePrefix+container.suffix)
		output, err := command.Output()
		if err != nil {
			t.Fatalf("inspect %s fixture volume mounts: %v", container.name, err)
		}
		volumes := strings.Fields(string(output))
		if len(volumes) == 0 {
			t.Fatalf("%s fixture container has no recorded volume mounts", container.name)
		}
		volumesByContainer[container.name] = volumes
	}
	return volumesByContainer
}

func assertNamedFixtureResourcesAbsent(t *testing.T, runID string, volumeNames ...string) {
	t.Helper()
	prefix := "telegram-fixture-" + runID
	for _, suffix := range []string{"browser", "tls-front", "telegramd", "database", "client", "atlas"} {
		assertDockerResourceAbsent(t, "container", prefix+suffix)
	}
	for _, suffix := range []string{"edge", "server"} {
		assertDockerResourceAbsent(t, "network", prefix+suffix)
	}
	for _, volumeName := range volumeNames {
		assertDockerResourceAbsent(t, "volume", volumeName)
	}
}

func assertFixtureCleanupVerified(t *testing.T, stderr string) {
	t.Helper()
	if !strings.Contains(stderr, "cleanup=verified") {
		t.Fatalf("fixture cleanup did not verify all owned resources: %q", stderr)
	}
}

func assertDockerResourceAbsent(t *testing.T, kind, name string) {
	t.Helper()
	output, err := fixtureCommand(context.Background(), "docker", kind, "inspect", name).CombinedOutput()
	if err == nil {
		t.Errorf("fixture left owned %s %s", kind, name)
		return
	}
	var exitErr *osexec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Errorf("inspect fixture %s %s: %v: %s", kind, name, err, strings.TrimSpace(string(output)))
		return
	}
	message := string(output)
	missing := kind == "container" && (strings.Contains(message, "No such object:") || strings.Contains(message, "No such container:"))
	missing = missing || kind == "network" && strings.Contains(message, "network "+name+" not found")
	missing = missing || kind == "volume" && strings.Contains(strings.ToLower(message), "no such volume")
	if !missing {
		t.Errorf("cannot verify fixture %s %s is absent: %v: %s", kind, name, err, strings.TrimSpace(message))
	}
}

func fixtureCommand(ctx context.Context, name string, args ...string) *osexec.Cmd {
	if ctx == nil {
		ctx = context.Background()
	}
	switch name {
	case "bash", "docker", "git":
	default:
		panic("unexpected fixture command: " + name)
	}
	//nolint:gosec // The executable is allowlisted and arguments are passed as distinct argv fields, never shell text.
	command := osexec.CommandContext(ctx, name, args...)
	if name == "bash" {
		// Stop foreground Docker children with Bash so its cleanup trap can run.
		command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		command.Cancel = func() error {
			if command.Process == nil {
				return nil
			}
			err := syscall.Kill(-command.Process.Pid, syscall.SIGTERM)
			if errors.Is(err, syscall.ESRCH) {
				return nil
			}
			return err
		}
		command.WaitDelay = 10 * time.Second
	}
	return command
}

func fixtureEnvironment(overrides map[string]string) []string {
	env := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if _, replaced := overrides[name]; !replaced {
			env = append(env, entry)
		}
	}
	for name, value := range overrides {
		env = append(env, name+"="+value)
	}
	return env
}

func pemDecode(data []byte) (*pem.Block, []byte) {
	return pem.Decode(data)
}
