#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
TEST_TMP=$(mktemp -d)
trap 'rm -rf -- "$TEST_TMP"' EXIT
TEST_EVENTS="$TEST_TMP/events"
BASELINE_CONTAINER=$(printf 'a%.0s' {1..64})
PUBLIC_HAPROXY_CONTAINER=$(printf 'b%.0s' {1..64})
PUBLIC_REPLICA_ONE_CONTAINER=$(printf 'c%.0s' {1..64})
PUBLIC_REPLICA_TWO_CONTAINER=$(printf 'd%.0s' {1..64})
MOCK_PROJECT=fixture
MOCK_ORPHANS=1
MOCK_STOP_TIMEOUT=120
MOCK_TARGET_SERVICES=$'postgres\nteagramd'

docker() {
	printf 'docker' >> "$TEST_EVENTS"
	printf ' <%s>' "$@" >> "$TEST_EVENTS"
	printf '\n' >> "$TEST_EVENTS"
	if [[ "${1:-}" == inspect && "${2:-}" == "$BASELINE_CONTAINER" && "${3:-}" == --format ]]; then
		[[ "${4:-}" == '{{ index .Config.Labels "com.docker.compose.project" }}' ]]
		printf '%s\n' "$MOCK_PROJECT"
		return 0
	fi
	if [[ "${1:-}" == inspect && "${3:-}" == --format &&
		"${4:-}" == '{{.State.Running}} {{.Config.StopTimeout}}' ]]; then
		case "${2:-}" in
			"$PUBLIC_HAPROXY_CONTAINER"|"$PUBLIC_REPLICA_ONE_CONTAINER"|"$PUBLIC_REPLICA_TWO_CONTAINER")
				printf 'true %s\n' "$MOCK_STOP_TIMEOUT"
				return 0
			;;
		esac
	fi
	if [[ "${1:-}" == ps ]]; then
		local service=''
		for arg in "$@"; do
			case "$arg" in
				label=com.docker.compose.service=*) service=${arg#*=}; service=${service#*=} ;;
			esac
		done
		if [[ "$MOCK_ORPHANS" == 1 ]]; then
			case "$service" in
				public-haproxy) printf '%s\n' "$PUBLIC_HAPROXY_CONTAINER" ;;
				teagramd-public-1) printf '%s\n' "$PUBLIC_REPLICA_ONE_CONTAINER" ;;
				teagramd-public-2) printf '%s\n' "$PUBLIC_REPLICA_TWO_CONTAINER" ;;
			esac
		fi
		return 0
	fi
	if [[ "${1:-}" == compose && "${2:-}" == --profile && "${3:-}" == public &&
		"${4:-}" == config && "${5:-}" == --services ]]; then
		printf '%s\n' "$MOCK_TARGET_SERVICES"
		return 0
	fi
	if [[ "${1:-}" == rm && "${2:-}" == -f ]]; then
		return 0
	fi
	if [[ "${1:-}" == stop && "${2:-}" == --timeout && "${3:-}" == "$MOCK_STOP_TIMEOUT" ]]; then
		return 0
	fi
	if [[ "${1:-}" == rm ]]; then
		return 0
	fi
	if [[ "${1:-}" == compose && "${2:-}" == up && "${3:-}" == -d ]]; then
		return 0
	fi
	if [[ "${1:-}" == compose && "${2:-}" == --profile && "${3:-}" == public &&
		"${4:-}" == up && "${5:-}" == -d ]]; then
		return 0
	fi
	printf 'unexpected docker invocation: %s\n' "$*" >&2
	return 90
}

export ROLLOUT_RUNNER_SOURCE_ONLY=1
# shellcheck disable=SC1091
. "$SCRIPT_DIR/rollout-runner.sh"
unset ROLLOUT_RUNNER_SOURCE_ONLY

apply_cleanup_line=$(grep -nF 'if remove_orphaned_public_profile_services "$baseline_id"; then :; else' "$SCRIPT_DIR/rollout-runner.sh" | cut -d: -f1)
apply_up_line=$(grep -nF 'if docker compose up -d </dev/null; then :; else' "$SCRIPT_DIR/rollout-runner.sh" | cut -d: -f1)
if [[ -z "$apply_cleanup_line" || -z "$apply_up_line" || "$apply_cleanup_line" -ge "$apply_up_line" ]]; then
	echo 'FAIL rollout must remove orphaned public profile containers before recreating teagramd' >&2
	exit 1
fi

remove_orphaned_public_profile_services "$BASELINE_CONTAINER"
docker compose up -d

remove_line=$(grep -n '<rm>' "$TEST_EVENTS" | cut -d: -f1)
up_line=$(grep -n '<compose> <up> <-d>' "$TEST_EVENTS" | cut -d: -f1)
if [[ -z "$remove_line" || -z "$up_line" || "$remove_line" -ge "$up_line" ]] || grep -q '<rm> <-f>' "$TEST_EVENTS"; then
	echo 'FAIL orphaned public profile containers must be removed before Compose starts the replacement' >&2
	exit 1
fi
for container in "$PUBLIC_HAPROXY_CONTAINER" "$PUBLIC_REPLICA_ONE_CONTAINER" "$PUBLIC_REPLICA_TWO_CONTAINER"; do
	stop_line=$(grep -nF "<stop> <--timeout> <$MOCK_STOP_TIMEOUT> <$container>" "$TEST_EVENTS" | cut -d: -f1)
	rm_command=$(sed -n "${remove_line}p" "$TEST_EVENTS")
	if [[ -z "$stop_line" || "$stop_line" -ge "$remove_line" || "$rm_command" != *"<$container>"* ]]; then
		printf 'FAIL orphaned public profile container was not stopped with its configured timeout before removal: %s\n' "$container" >&2
		exit 1
	fi
done
replica_one_stop_line=$(grep -nF "<stop> <--timeout> <$MOCK_STOP_TIMEOUT> <$PUBLIC_REPLICA_ONE_CONTAINER>" "$TEST_EVENTS" | cut -d: -f1)
replica_two_stop_line=$(grep -nF "<stop> <--timeout> <$MOCK_STOP_TIMEOUT> <$PUBLIC_REPLICA_TWO_CONTAINER>" "$TEST_EVENTS" | cut -d: -f1)
haproxy_stop_line=$(grep -nF "<stop> <--timeout> <$MOCK_STOP_TIMEOUT> <$PUBLIC_HAPROXY_CONTAINER>" "$TEST_EVENTS" | cut -d: -f1)
if [[ "$replica_one_stop_line" -ge "$haproxy_stop_line" || "$replica_two_stop_line" -ge "$haproxy_stop_line" ]]; then
	echo 'FAIL public replicas must drain while HAProxy can still route their existing sessions' >&2
	exit 1
fi

: > "$TEST_EVENTS"
docker compose up -d --no-build --no-deps teagramd
restore_removed_public_profile_services
baseline_up_line=$(grep -n '<compose> <up> <-d> <--no-build> <--no-deps> <teagramd>' "$TEST_EVENTS" | cut -d: -f1)
profile_restore_line=$(grep -n '<compose> <--profile> <public> <up> <-d> <--no-build> <--no-deps>' "$TEST_EVENTS" | cut -d: -f1)
restore_command=$(sed -n "${profile_restore_line}p" "$TEST_EVENTS")
if [[ -z "$baseline_up_line" || -z "$profile_restore_line" || "$baseline_up_line" -ge "$profile_restore_line" ||
	"$restore_command" != *'<public-haproxy>'*'<teagramd-public-1>'*'<teagramd-public-2>'* ]]; then
	echo 'FAIL removed public profile services must be restored after baseline teagramd during rollback' >&2
	exit 1
fi
for service in public-haproxy teagramd-public-1 teagramd-public-2; do
	if ! grep -Fq "<$service>" "$TEST_EVENTS"; then
		printf 'FAIL baseline public profile service was not restored: %s\n' "$service" >&2
		exit 1
	fi
done
rollback_teagramd_line=$(grep -nF 'docker compose up -d --no-build --no-deps "$(rollout_compose_service)" </dev/null || { fail' "$SCRIPT_DIR/rollout-runner.sh" | cut -d: -f1)
rollback_restore_line=$(grep -nF 'restore_removed_public_profile_services || return 1' "$SCRIPT_DIR/rollout-runner.sh" | cut -d: -f1)
if [[ -z "$rollback_teagramd_line" || -z "$rollback_restore_line" || "$rollback_teagramd_line" -ge "$rollback_restore_line" ]]; then
	echo 'FAIL rollback must recreate baseline teagramd before restoring its public profile services' >&2
	exit 1
fi

: > "$TEST_EVENTS"
MOCK_STOP_TIMEOUT=60
if remove_orphaned_public_profile_services "$BASELINE_CONTAINER" >"$TEST_TMP/short-timeout.stdout" 2>"$TEST_TMP/short-timeout.stderr"; then
	echo 'FAIL cleanup must reject a stop timeout shorter than the server drain window' >&2
	exit 1
fi
if grep -q '<stop>' "$TEST_EVENTS" || grep -q '<rm>' "$TEST_EVENTS"; then
	echo 'FAIL unsafe stop timeout must be rejected before stopping or removing containers' >&2
	exit 1
fi
MOCK_STOP_TIMEOUT=120

: > "$TEST_EVENTS"
MOCK_TARGET_SERVICES+=$'\npublic-haproxy\nteagramd-public-1\nteagramd-public-2'
remove_orphaned_public_profile_services "$BASELINE_CONTAINER"
if grep -q '<rm>' "$TEST_EVENTS" || grep -q '<stop>' "$TEST_EVENTS"; then
	echo 'FAIL public profile containers still present in target Compose must remain managed by Compose' >&2
	exit 1
fi
restore_removed_public_profile_services
if grep -q '<compose> <--profile> <public> <up>' "$TEST_EVENTS"; then
	echo 'FAIL baseline public profile services must not be restored when target Compose still manages them' >&2
	exit 1
fi

: > "$TEST_EVENTS"
MOCK_ORPHANS=0
remove_orphaned_public_profile_services "$BASELINE_CONTAINER"
if grep -q '<compose> <--profile> <public> <config>' "$TEST_EVENTS"; then
	echo 'FAIL target Compose was queried despite no old public profile containers' >&2
	exit 1
fi

printf '%s\n' 'public profile orphan cleanup fixtures pass'
