#!/usr/bin/env bash
set -Eeuo pipefail

SERVER_REVISION=
WEB_REVISION=
RUN_ID=
AUTH_PROGRESS=0
AUTH_CONTROLS=0
AUTH_PROGRESS_OUTPUT=
PAIR_KEY_INPUT=
PAIR_KEY_OUTPUT=
AUTH_PROGRESS_PREBASELINE_CONTROLS=0
GUEST_SIGNUP=0

usage() {
	cat >&2 <<'EOF'
usage: run.sh --server-revision <40-char-sha> --web-revision <40-char-sha> --run-id <32-char-hex> [--guest-signup | --auth-progress --auth-progress-output <path> | --auth-controls] [--auth-progress-prebaseline-controls] [--pair-key-input <tmpfs-path> | --pair-key-output <tmpfs-path>]

The fixture always uses https://teagramd.test and wss://teagramd.test/apiws.
It does not accept endpoints, environment files, or production credentials.
EOF
}

while (($#)); do
	case "$1" in
		--server-revision|--web-revision|--run-id|--auth-progress-output|--pair-key-input|--pair-key-output)
			if (($# < 2)); then
				printf 'missing value for %s\n' "$1" >&2
				usage
				exit 2
			fi
			option=$1
			value=$2
			shift 2
			case "$option" in
				--server-revision) SERVER_REVISION=$value ;;
				--web-revision) WEB_REVISION=$value ;;
				--run-id) RUN_ID=$value ;;
				--auth-progress-output) AUTH_PROGRESS_OUTPUT=$value ;;
				--pair-key-input) PAIR_KEY_INPUT=$value ;;
				--pair-key-output) PAIR_KEY_OUTPUT=$value ;;
			esac
			;;
		--auth-progress)
			AUTH_PROGRESS=1
			AUTH_CONTROLS=1
			shift
			;;
		--auth-controls)
			AUTH_CONTROLS=1
			shift
			;;
		--auth-progress-prebaseline-controls)
			AUTH_PROGRESS_PREBASELINE_CONTROLS=1
			shift
			;;
		--guest-signup)
			GUEST_SIGNUP=1
			shift
			;;
		--endpoint|--target|--env-file)
			printf '%s is not accepted by the disposable fixture\n' "$1" >&2
			exit 2
			;;
		-h|--help)
			usage
			exit 0
			;;
		*)
			printf 'unknown argument: %s\n' "$1" >&2
			usage
			exit 2
			;;
	esac
done

if [[ ! $SERVER_REVISION =~ ^[0-9a-f]{40}$ ]]; then
	printf 'server revision must be a full lowercase 40-character SHA\n' >&2
	exit 2
fi
if [[ ! $WEB_REVISION =~ ^[0-9a-f]{40}$ ]]; then
	printf 'web revision must be a full lowercase 40-character SHA\n' >&2
	exit 2
fi
if [[ ! $RUN_ID =~ ^[0-9a-f]{32}$ ]]; then
	printf 'run ID must be 128 bits of lowercase hexadecimal\n' >&2
	exit 2
fi
if ((AUTH_PROGRESS)); then
	AUTH_PROGRESS_OUTPUT_PARENT="$(dirname -- "$AUTH_PROGRESS_OUTPUT")"
	if [[ $WEB_REVISION != 569529c1f36923c098760ef727808254dcb6a662 ]]; then
		printf 'auth-progress mode requires its pinned artifact source revision\n' >&2
		exit 2
	fi
	if [[ ! $AUTH_PROGRESS_OUTPUT =~ ^/[^[:space:]]+$ || -e $AUTH_PROGRESS_OUTPUT || -L $AUTH_PROGRESS_OUTPUT || ! -d $AUTH_PROGRESS_OUTPUT_PARENT ]]; then
		printf 'auth-progress output must be a new absolute file under an existing directory\n' >&2
		exit 2
	fi
	if [[ "$(cd -- "$AUTH_PROGRESS_OUTPUT_PARENT" && pwd -P)" != "$AUTH_PROGRESS_OUTPUT_PARENT" ]]; then
		printf 'auth-progress output directory must not traverse symlinks\n' >&2
		exit 2
	fi
elif [[ -n $AUTH_PROGRESS_OUTPUT ]]; then
	printf 'auth-progress output requires --auth-progress\n' >&2
	exit 2
fi
if [[ $AUTH_PROGRESS_PREBASELINE_CONTROLS == 1 && $AUTH_CONTROLS != 1 ]]; then
	printf 'prebaseline auth-progress controls require auth controls\n' >&2
	exit 2
fi
if [[ $GUEST_SIGNUP == 1 && ( $AUTH_PROGRESS == 1 || $AUTH_CONTROLS == 1 || $AUTH_PROGRESS_PREBASELINE_CONTROLS == 1 ) ]]; then
	printf 'guest signup mode cannot be combined with auth-progress controls\n' >&2
	exit 2
fi
if [[ -n $PAIR_KEY_INPUT && -n $PAIR_KEY_OUTPUT ]]; then
	printf 'fixture pair key cannot be both imported and exported\n' >&2
	exit 2
fi
for pair_key_path in "$PAIR_KEY_INPUT" "$PAIR_KEY_OUTPUT"; do
	[[ -n $pair_key_path ]] || continue
	if [[ ! $pair_key_path =~ ^/dev/shm/[^[:space:]]+$ ]]; then
		printf 'fixture pair key must be in tmpfs under /dev/shm\n' >&2
		exit 2
	fi
	pair_key_parent="$(dirname -- "$pair_key_path")"
	if [[ ! -d $pair_key_parent || -L $pair_key_parent || "$(cd -- "$pair_key_parent" && pwd -P)" != "$pair_key_parent" ]]; then
		printf 'fixture pair key directory is unsafe\n' >&2
		exit 2
	fi
done
if [[ -n $PAIR_KEY_INPUT ]]; then
	if [[ ! -f $PAIR_KEY_INPUT || -L $PAIR_KEY_INPUT || "$(stat -c '%a' "$PAIR_KEY_INPUT")" != 400 ]]; then
		printf 'fixture pair key input is missing or unsafe\n' >&2
		exit 2
	fi
elif [[ -n $PAIR_KEY_OUTPUT && ( -e $PAIR_KEY_OUTPUT || -L $PAIR_KEY_OUTPUT ) ]]; then
	printf 'fixture pair key output already exists\n' >&2
	exit 2
fi

while IFS='=' read -r name _; do
	case "$name" in
		TG_*|MTPROTO_*)
			printf 'prohibited environment variable is set: %s\n' "$name" >&2
			exit 2
			;;
	esac
done < <(env)

printf 'fixture argument validation passed\n' >&2
SCRIPT_DIR="$(cd -- "$(dirname -- "$0")" && pwd)"
exec bash "$SCRIPT_DIR/start.sh" "$SERVER_REVISION" "$WEB_REVISION" "$RUN_ID" "$AUTH_PROGRESS" "$AUTH_CONTROLS" "$AUTH_PROGRESS_OUTPUT" "$PAIR_KEY_INPUT" "$PAIR_KEY_OUTPUT" "$AUTH_PROGRESS_PREBASELINE_CONTROLS" "$GUEST_SIGNUP"
