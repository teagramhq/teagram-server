#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)

if [[ "${1:-}" == --service ]]; then
  [[ "$#" -eq 2 ]] || exit 2
  IMAGE_TAG="$2"
  RUNTIME_DIRECTORY="${RUNTIME_DIRECTORY:-}"
  # shellcheck source=deploy/teagram-demo/demo-common.sh
  source "$SCRIPT_DIR/demo-common.sh"
  # shellcheck source=deploy/teagram-demo/activity-credential.sh
  source "$SCRIPT_DIR/activity-credential.sh"

  ACTIVITY_CREDENTIAL_FILE=$(demo_stage_activity_credential)
  trap 'demo_cleanup_activity_credential "$ACTIVITY_CREDENTIAL_FILE"' EXIT
  [[ "$ACTIVITY_CREDENTIAL_FILE" == "$RUNTIME_DIRECTORY/inventory.json" ]]
  docker run --rm --network none --read-only --cap-drop ALL \
    --security-opt no-new-privileges \
    --mount "type=bind,source=$ACTIVITY_CREDENTIAL_FILE,target=/inventory,readonly" \
    "$IMAGE_TAG" /inventory
  exit 0
fi

[[ "$EUID" -eq 0 ]] || {
  printf 'systemd-to-container credential handoff test requires root\n' >&2
  exit 1
}
systemctl --system show-environment >/dev/null 2>&1 || {
  printf 'systemd-to-container credential handoff test requires an active system manager\n' >&2
  exit 1
}
command -v docker >/dev/null 2>&1 || {
  printf 'systemd-to-container credential handoff test requires Docker\n' >&2
  exit 1
}

TMP_DIR=$(mktemp -d)
IMAGE_TAG="teagram-activity-credential-test:${TMP_DIR##*/}"
IMAGE_BUILT=0
cleanup() {
  local status=$?
  trap - EXIT
  if ((IMAGE_BUILT)); then
    docker image rm -f "$IMAGE_TAG" >/dev/null || status=1
  fi
  rm -rf -- "$TMP_DIR"
  exit "$status"
}
trap cleanup EXIT

printf 'credential-handoff-fixture\n' > "$TMP_DIR/inventory.json"
chmod 0400 "$TMP_DIR/inventory.json"
cat > "$TMP_DIR/probe.go" <<'GO'
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) != 2 {
		os.Exit(2)
	}
	data, err := os.ReadFile(os.Args[1])
	if err != nil || string(data) != "credential-handoff-fixture\n" {
		os.Exit(1)
	}
	info, err := os.Stat(os.Args[1])
	if err != nil || info.Mode().Perm() != 0400 {
		os.Exit(1)
	}
	fmt.Println("credential handoff verified")
}
GO
cat > "$TMP_DIR/Dockerfile" <<'DOCKERFILE'
FROM scratch
COPY probe /probe
ENTRYPOINT ["/probe"]
DOCKERFILE
CGO_ENABLED=0 go build -o "$TMP_DIR/probe" "$TMP_DIR/probe.go"
docker build --network none --tag "$IMAGE_TAG" "$TMP_DIR"
IMAGE_BUILT=1

UNIT_TAG="teagram-activity-credential-test-$$-$RANDOM"
RUNTIME_DIRECTORY_NAME="$UNIT_TAG"
RUNTIME_DIRECTORY_PATH="/run/$RUNTIME_DIRECTORY_NAME"
if ! OUTPUT=$(systemd-run --quiet --wait --pipe --collect \
  --unit "$UNIT_TAG" \
  --setenv "PATH=$PATH" \
  --property=Type=oneshot \
  --property=User=root \
  --property=Group=root \
  --property="LoadCredential=inventory.json:$TMP_DIR/inventory.json" \
  --property="RuntimeDirectory=$RUNTIME_DIRECTORY_NAME" \
  --property=RuntimeDirectoryMode=0700 \
  --property=RuntimeDirectoryPreserve=no \
  --property=ProtectSystem=strict \
  --property=PrivateTmp=true \
  --property=NoNewPrivileges=true \
  --property="ReadWritePaths=$RUNTIME_DIRECTORY_PATH /run/docker.sock /var/run/docker.sock" \
  -- /bin/bash "$SCRIPT_DIR/test-activity-credential-handoff.sh" --service "$IMAGE_TAG" 2>&1); then
  printf '%s\n' "$OUTPUT" >&2
  exit 1
fi
[[ "$OUTPUT" == *'credential handoff verified'* ]] || {
  printf 'container did not verify the staged systemd credential\n' >&2
  exit 1
}
[[ ! -e "$RUNTIME_DIRECTORY_PATH" && ! -L "$RUNTIME_DIRECTORY_PATH" ]] || {
  printf 'systemd did not clean the activity credential runtime directory\n' >&2
  exit 1
}

printf 'systemd-to-container credential handoff holds\n'
