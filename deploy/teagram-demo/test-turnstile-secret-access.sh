#!/usr/bin/env bash
set -Eeuo pipefail

if [[ "$EUID" -ne 0 ]]; then
  echo 'test-turnstile-secret-access.sh must run as root' >&2
  exit 1
fi

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=deploy/teagram-demo/demo-common.sh
source "$SCRIPT_DIR/demo-common.sh"

TMP_DIR=$(mktemp -d)
trap 'rm -rf -- "$TMP_DIR"' EXIT
secret_dir="$TMP_DIR/secrets"
secret_file="$secret_dir/turnstile-secret-key"
install -d -o root -g root -m 0700 "$secret_dir"
printf 'fixture-turnstile-secret\n' > "$secret_file"
chown root:root "$secret_file"
chmod 0600 "$secret_file"

assert_validator() {
  python3 - "$SCRIPT_DIR/validate-env.py" "$secret_file" "$1" <<'PY'
from contextlib import redirect_stderr
from importlib.util import module_from_spec, spec_from_file_location
from io import StringIO
from pathlib import Path
import sys


spec = spec_from_file_location("demo_env_validator", sys.argv[1])
if spec is None or spec.loader is None:
    raise SystemExit("could not load demo env validator")
validator = module_from_spec(spec)
spec.loader.exec_module(validator)
secret_path = Path(sys.argv[2])
if sys.argv[3] == "valid":
    if validator.read_turnstile_secret(secret_path) != "fixture-turnstile-secret":
        raise SystemExit("validator returned an unexpected fixture secret")
else:
    try:
        with redirect_stderr(StringIO()):
            validator.read_turnstile_secret(secret_path)
    except SystemExit:
        pass
    else:
        raise SystemExit("validator accepted an invalid secret file mode")
PY
}

assert_validator reject
demo_prepare_turnstile_secret_access "$secret_file"
[[ "$(stat -c '%u:%g:%a' "$secret_file")" == '0:65532:440' ]] || {
  echo 'Turnstile secret must be root-owned and readable by runtime gid 65532 only' >&2
  exit 1
}
demo_prepare_turnstile_secret_access "$secret_file"
assert_validator valid

# A file-backed Compose secret is a bind mount, so test the mounted file's
# group permissions with the same uid and gid as the server container.
chmod 0711 "$TMP_DIR"
chgrp 65532 "$secret_dir"
chmod 0750 "$secret_dir"
readable=$(setpriv --reuid=65532 --regid=65532 --clear-groups cat "$secret_file")
[[ "$readable" == 'fixture-turnstile-secret' ]] || {
  echo 'server runtime uid could not read the mounted Turnstile secret' >&2
  exit 1
}

chgrp root "$secret_dir"
chmod 0700 "$secret_dir"
chmod 0644 "$secret_file"
assert_validator reject
if demo_prepare_turnstile_secret_access "$secret_file" >/dev/null 2>&1; then
  echo 'Turnstile secret helper accepted a world-readable file' >&2
  exit 1
fi

echo 'demo Turnstile secret access holds'
