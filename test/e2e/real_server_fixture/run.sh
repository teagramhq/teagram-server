#!/usr/bin/env bash
set -Eeuo pipefail

SERVER_REVISION=
WEB_REVISION=
RUN_ID=
EXPECTED_WEB_REVISION=84961bf77003a1bdb582d1096d988f1d304e3d1f

usage() {
	cat >&2 <<'EOF'
usage: run.sh --server-revision <40-char-sha> --web-revision <40-char-sha> --run-id <32-char-hex>

The fixture always uses https://telegramd.test and wss://telegramd.test/apiws.
It does not accept endpoints, environment files, or production credentials.
EOF
}

while (($#)); do
	case "$1" in
		--server-revision|--web-revision|--run-id)
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
			esac
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
if [[ $WEB_REVISION != "$EXPECTED_WEB_REVISION" ]]; then
	printf 'web revision does not match the accepted fixture pin\n' >&2
	exit 2
fi
if [[ ! $RUN_ID =~ ^[0-9a-f]{32}$ ]]; then
	printf 'run ID must be 128 bits of lowercase hexadecimal\n' >&2
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
exec bash "$SCRIPT_DIR/start.sh" "$SERVER_REVISION" "$WEB_REVISION" "$RUN_ID"
