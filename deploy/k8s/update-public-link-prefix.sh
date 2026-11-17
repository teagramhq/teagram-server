#!/usr/bin/env bash
set -Eeuo pipefail
set -f
umask 077

readonly INSTANCE=teagram-k3s
readonly NAMESPACE=teagram
readonly BOOTSTRAP_MARKER=MAIN-3278
readonly CONFIGMAP=teagramd-runtime
readonly STATEFULSET=teagramd
readonly OLD_PREFIX=https://teagram.tailaa4918.ts.net/
readonly PUBLIC_PREFIX=https://web.teagram.io/

fail() {
  printf 'public link prefix update: %s\n' "$*" >&2
  exit 1
}

usage() {
  cat <<'EOF'
Usage: update-public-link-prefix.sh --dry-run | --apply | --rollback

  --dry-run  Server-side validate the public prefix change without applying it.
  --apply    Set the public prefix and retain the tailnet prefix for revocation.
  --rollback Restore the tailnet prefix and retain the public prefix for
             revocation.

Run as root on alpha. The apply and rollback modes restart the teagramd
StatefulSet after changing its runtime ConfigMap.
EOF
}

[[ $# -eq 1 ]] || { usage >&2; exit 64; }
case "$1" in
  --dry-run|--apply|--rollback) mode=$1 ;;
  --help|-h) usage; exit 0 ;;
  *) usage >&2; exit 64 ;;
esac

[[ $(id -u) == 0 ]] || fail 'run as root on alpha'
[[ $(hostname -s) == alpha ]] || fail 'run on alpha'
for command_name in incus python3; do
  command -v "$command_name" >/dev/null 2>&1 || fail "required command is missing: $command_name"
done
incus info "$INSTANCE" >/dev/null || fail "Incus instance $INSTANCE is unavailable"
marker=$(incus config get "$INSTANCE" user.teagram-k3s-bootstrap)
[[ $marker == "$BOOTSTRAP_MARKER" ]] || fail 'the k3s instance bootstrap marker does not match'

kubectl() {
  incus exec --mode=non-interactive "$INSTANCE" -- k3s kubectl "$@"
}

runtime_json=$(kubectl get configmap "$CONFIGMAP" --namespace "$NAMESPACE" --output=json) ||
  fail 'could not read the runtime ConfigMap'
resource_version=$(python3 -c '
import json, sys
value = json.load(sys.stdin).get("metadata", {}).get("resourceVersion")
if not isinstance(value, str) or not value:
    raise SystemExit(1)
sys.stdout.write(value)
' <<< "$runtime_json") || fail 'the runtime ConfigMap has no resource version'

read_config_value() {
  local key=$1
  python3 -c '
import json, sys
value = json.load(sys.stdin).get("data", {}).get(sys.argv[1])
if not isinstance(value, str):
    raise SystemExit(1)
sys.stdout.write(value)
' "$key" <<< "$runtime_json"
}

read_previous_state() {
  python3 -c '
import json, sys
data = json.load(sys.stdin).get("data", {})
if "TG_PREVIOUS_PUBLIC_LINK_PREFIXES" not in data:
    print("absent")
else:
    value = data["TG_PREVIOUS_PUBLIC_LINK_PREFIXES"]
    if not isinstance(value, str):
        raise SystemExit(1)
    sys.stdout.write(value)
' <<< "$runtime_json"
}

current_prefix=$(read_config_value TG_PUBLIC_LINK_PREFIX) || fail 'the runtime ConfigMap has no valid public link prefix'
advertise_before=$(read_config_value TG_ADVERTISE_ADDR) || fail 'the runtime ConfigMap has no valid advertise address'
media_key_version_before=$(read_config_value TG_MEDIA_KEY_CURRENT) || fail 'the runtime ConfigMap has no valid media key version'
previous_state=$(read_previous_state) || fail 'the previous-prefix setting is not a string'

statefulset_json=$(kubectl get statefulset "$STATEFULSET" --namespace "$NAMESPACE" --output=json) ||
  fail 'could not read the teagramd StatefulSet'
if ! python3 -c '
import json, sys
statefulset = json.load(sys.stdin)
mode = sys.argv[1]
spec = statefulset.get("spec", {})
status = statefulset.get("status", {})
containers = spec.get("template", {}).get("spec", {}).get("containers", [])
server = next((container for container in containers if container.get("name") == "teagramd"), None)
if spec.get("replicas") != 1 or server is None:
    raise SystemExit(1)
if mode != "--rollback" and status.get("readyReplicas") != 1:
    raise SystemExit(1)
environment = {entry.get("name"): entry for entry in server.get("env", [])}
for name in ("TG_PUBLIC_LINK_PREFIX", "TG_PREVIOUS_PUBLIC_LINK_PREFIXES"):
    reference = environment.get(name, {}).get("valueFrom", {}).get("configMapKeyRef", {})
    if reference.get("name") != "teagramd-runtime" or reference.get("key") != name:
        raise SystemExit(1)
if environment["TG_PREVIOUS_PUBLIC_LINK_PREFIXES"]["valueFrom"]["configMapKeyRef"].get("optional") is not True:
    raise SystemExit(1)
' "$mode" <<< "$statefulset_json"; then
  if [[ $mode == --rollback ]]; then
    fail 'teagramd no longer has the reviewed runtime ConfigMap references; refusing rollback'
  fi
  fail 'teagramd must be ready with the reviewed runtime ConfigMap references before applying this change'
fi

case "$mode:$current_prefix:$previous_state" in
  --dry-run:"$OLD_PREFIX":absent|--apply:"$OLD_PREFIX":absent|--dry-run:"$OLD_PREFIX":"$PUBLIC_PREFIX"|--apply:"$OLD_PREFIX":"$PUBLIC_PREFIX"|--dry-run:"$PUBLIC_PREFIX":"$OLD_PREFIX"|--apply:"$PUBLIC_PREFIX":"$OLD_PREFIX")
    expected_prefix=$PUBLIC_PREFIX
    expected_previous=$OLD_PREFIX
    patch='{"data":{"TG_PUBLIC_LINK_PREFIX":"https://web.teagram.io/","TG_PREVIOUS_PUBLIC_LINK_PREFIXES":"https://teagram.tailaa4918.ts.net/"}}'
    ;;
  --rollback:"$PUBLIC_PREFIX":"$OLD_PREFIX")
    expected_prefix=$OLD_PREFIX
    expected_previous=$PUBLIC_PREFIX
    patch='{"data":{"TG_PUBLIC_LINK_PREFIX":"https://teagram.tailaa4918.ts.net/","TG_PREVIOUS_PUBLIC_LINK_PREFIXES":"https://web.teagram.io/"}}'
    ;;
  --rollback:"$OLD_PREFIX":"$PUBLIC_PREFIX")
    expected_prefix=$OLD_PREFIX
    expected_previous=$PUBLIC_PREFIX
    already_rolled_back=true
    ;;
  *)
    observed_prefix_state=$(printf 'current_prefix=%q previous_prefix=%q' \
      "$current_prefix" "$previous_state")
    fail "runtime link prefix values differ from the reviewed before or after state ($observed_prefix_state);" \
      'refusing to overwrite them'
    ;;
esac
verify_config() {
  local config_json=$1 expected_prefix_value=$2 expected_previous_value=$3
  python3 -c '
import json, sys
data = json.load(sys.stdin).get("data", {})
expected_prefix, expected_previous, expected_advertise, expected_media = sys.argv[1:]
if data.get("TG_PUBLIC_LINK_PREFIX") != expected_prefix:
    raise SystemExit(1)
if data.get("TG_ADVERTISE_ADDR") != expected_advertise:
    raise SystemExit(1)
if data.get("TG_MEDIA_KEY_CURRENT") != expected_media:
    raise SystemExit(1)
if expected_previous == "absent":
    if "TG_PREVIOUS_PUBLIC_LINK_PREFIXES" in data:
        raise SystemExit(1)
elif data.get("TG_PREVIOUS_PUBLIC_LINK_PREFIXES") != expected_previous:
    raise SystemExit(1)
' "$expected_prefix_value" "$expected_previous_value" \
    "$advertise_before" "$media_key_version_before" <<< "$config_json"
}

if [[ ${already_rolled_back:-false} == true ]]; then
  verify_config "$runtime_json" "$expected_prefix" "$expected_previous" ||
    fail 'runtime ConfigMap no longer matches the reviewed rollback state; inspect the live state before retrying'
else
  patch=$(python3 -c '
import json, sys
patch = json.loads(sys.argv[1])
patch["metadata"] = {"resourceVersion": sys.argv[2]}
print(json.dumps(patch, separators=(",", ":")))
' "$patch" "$resource_version") || fail 'could not prepare the guarded runtime patch'

  if ! dry_run_json=$(kubectl patch configmap "$CONFIGMAP" --namespace "$NAMESPACE" \
    --type=merge --patch "$patch" --dry-run=server --output=json); then
    fail 'server-side ConfigMap patch validation failed'
  fi
  verify_config "$dry_run_json" "$expected_prefix" "$expected_previous" ||
    fail 'server-side patch would change unrelated runtime values or produced an unexpected result'

  if [[ $mode == --dry-run ]]; then
    printf 'public link prefix dry_run=accepted prefix=%s previous_prefix=%s\n' \
      "$expected_prefix" "$expected_previous"
    exit 0
  fi

  if ! applied_json=$(kubectl patch configmap "$CONFIGMAP" --namespace "$NAMESPACE" \
    --type=merge --patch "$patch" --output=json); then
    fail 'could not apply the runtime ConfigMap patch; inspect the live state, then rerun --apply or --rollback'
  fi
  verify_config "$applied_json" "$expected_prefix" "$expected_previous" ||
    fail 'applied ConfigMap does not match the reviewed values; inspect the live state, then rerun --apply or --rollback'
fi

if ! kubectl rollout restart "statefulset/$STATEFULSET" --namespace "$NAMESPACE"; then
  fail 'ConfigMap updated but teagramd restart could not be started; run --rollback after inspecting the live state'
fi
if ! kubectl rollout status "statefulset/$STATEFULSET" --namespace "$NAMESPACE" --timeout=180s; then
  fail 'teagramd rollout did not become ready; run --rollback after inspecting the live state'
fi

if [[ ${already_rolled_back:-false} == true ]]; then
  printf 'public link prefix update rollback already applied rollout=verified prefix=%s previous_prefix=%s\n' \
    "$expected_prefix" "$expected_previous"
else
  printf 'public link prefix update rollout=verified prefix=%s previous_prefix=%s\n' \
    "$expected_prefix" "$expected_previous"
fi
