//go:build performance

package perf_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPerfPostgresReadinessUsesTCP(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	bin := t.TempDir()
	cache := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "docker.log")
	docker := `#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$DOCKER_LOG"
case "$1" in
  pull|run) exit 0 ;;
  exec)
    shift 2
    case "$1" in
      pg_isready)
        shift
        for arg in "$@"; do
          [[ "$arg" == "127.0.0.1" ]] && exit 0
        done
        # The temporary server used during image initialization only has a
        # Unix socket. Readiness requires the final server's TCP listener.
        exit 1
        ;;
      psql)
        [[ "$*" == *"CREATE ROLE browser_acceptance_probe NOLOGIN"* ]] || {
          echo "unexpected Postgres setup: $*" >&2
          exit 2
        }
        exit 0
        ;;
      *) echo "unexpected docker exec: $*" >&2; exit 2 ;;
    esac
    ;;
  *) echo "unexpected docker command: $*" >&2; exit 2 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(docker), 0o700); err != nil {
		t.Fatalf("write docker stub: %v", err)
	}
	sleep := "#!/usr/bin/env bash\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "sleep"), []byte(sleep), 0o700); err != nil {
		t.Fatalf("write sleep stub: %v", err)
	}

	cmd := exec.Command("bash", ".github/scripts/perf-postgres.sh")
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"PERF_PG_CONTAINER=perf-pg-test",
		"PERF_CACHE="+cache,
		"DOCKER_LOG="+logPath,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("start performance Postgres with TCP readiness: %v\n%s", err, output)
	}

	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read docker log: %v", err)
	}
	logText := string(log)
	readyAt := strings.Index(logText, "pg_isready -h 127.0.0.1 -p 5432 -U postgres -q")
	if readyAt < 0 {
		t.Fatalf("Postgres readiness did not check its TCP listener:\n%s", log)
	}
	roleAt := strings.Index(logText, "CREATE ROLE browser_acceptance_probe NOLOGIN")
	if roleAt < 0 || roleAt < readyAt {
		t.Fatalf("the fixture restore role was not provisioned after Postgres became ready:\n%s", log)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate perf_postgres_script_test.go")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}
