#!/bin/sh
set -eu
umask 077

if [ "$#" -gt 1 ]; then
	printf '%s\n' 'usage: bootstrap-rustfs-secrets.sh [env-file]' >&2
	exit 2
fi

env_file=${1:-.env}
if [ ! -f "$env_file" ] || [ -L "$env_file" ]; then
	printf '%s\n' 'expected a regular .env file; copy .env.example first' >&2
	exit 1
fi
if [ ! -r "$env_file" ] || [ ! -w "$env_file" ]; then
	printf '%s\n' '.env must be readable and writable by the current user' >&2
	exit 1
fi
chmod 600 "$env_file"

get_value() {
	awk -v key="$1" '
		index($0, key "=") == 1 {
			count++
			value = substr($0, length(key) + 2)
		}
		END {
			if (count > 1) exit 2
			if (count == 1) printf "%s", value
		}
	' "$env_file"
}

if ! root_access=$(get_value RUSTFS_ROOT_ACCESS_KEY) ||
	! root_secret=$(get_value RUSTFS_ROOT_SECRET_KEY) ||
	! app_access=$(get_value TG_BLOB_S3_ACCESS_KEY_ID) ||
	! app_secret=$(get_value TG_BLOB_S3_SECRET_ACCESS_KEY); then
	printf '%s\n' '.env contains duplicate RustFS credential entries' >&2
	exit 1
fi

configured=0
for value in "$root_access" "$root_secret" "$app_access" "$app_secret"; do
	if [ -n "$value" ]; then
		configured=$((configured + 1))
	fi
done

random_hex() {
	od -An -N "$1" -tx1 /dev/urandom | tr -d ' \n'
}

if [ "$configured" -eq 0 ]; then
	root_access=$(random_hex 10)
	root_secret=$(random_hex 32)
	app_access=$(random_hex 10)
	app_secret=$(random_hex 32)
	if [ "$root_access" = "$app_access" ]; then
		app_access=$(random_hex 10)
	fi
elif [ "$configured" -ne 4 ]; then
	printf '%s\n' 'RustFS credentials are partially configured; restore the complete set instead of rotating them' >&2
	exit 1
fi

case "$root_access:$app_access" in
	*[!0-9a-f:]*|'')
		printf '%s\n' 'RustFS access keys must be lowercase hexadecimal values' >&2
		exit 1
		;;
esac
if [ "${#root_access}" -ne 20 ] || [ "${#app_access}" -ne 20 ] || [ "${#root_secret}" -ne 64 ] || [ "${#app_secret}" -ne 64 ] || [ "$root_access" = "$app_access" ]; then
	printf '%s\n' 'RustFS credential values have an invalid length or duplicate access keys' >&2
	exit 1
fi
case "$root_secret:$app_secret" in
	*[!0-9a-f:]*|'')
		printf '%s\n' 'RustFS secret keys must be lowercase hexadecimal values' >&2
		exit 1
		;;
esac

case "$env_file" in
	*/*) env_dir=${env_file%/*} ;;
	*) env_dir=. ;;
esac
secret_dir=$env_dir/.secrets
app_secret_file=$secret_dir/telegramd-blob-secret-key
if [ -L "$secret_dir" ] || [ -L "$app_secret_file" ]; then
	printf '%s\n' 'RustFS secret paths must not be symlinks' >&2
	exit 1
fi
mkdir -p -- "$secret_dir"
chmod 700 "$secret_dir"

temp_file=$(mktemp "${env_file}.tmp.XXXXXX")
temp_secret_file=
cleanup() {
	if [ -n "${temp_file:-}" ]; then
		rm -f -- "$temp_file"
	fi
	if [ -n "${temp_secret_file:-}" ]; then
		rm -f -- "$temp_secret_file"
	fi
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

awk \
	-v root_access="$root_access" \
	-v root_secret="$root_secret" \
	-v app_access="$app_access" \
	-v app_secret="$app_secret" '
	BEGIN {
		names[1] = "RUSTFS_ROOT_ACCESS_KEY"; values[1] = root_access
		names[2] = "RUSTFS_ROOT_SECRET_KEY"; values[2] = root_secret
		names[3] = "TG_BLOB_S3_ACCESS_KEY_ID"; values[3] = app_access
		names[4] = "TG_BLOB_S3_SECRET_ACCESS_KEY"; values[4] = app_secret
	}
	{
		for (i = 1; i <= 4; i++) {
			if (index($0, names[i] "=") == 1) {
				if (!written[i]++) print names[i] "=" values[i]
				next
			}
		}
		print
	}
	END {
		for (i = 1; i <= 4; i++) {
			if (!written[i]) print names[i] "=" values[i]
		}
	}
' "$env_file" > "$temp_file"
chmod 600 "$temp_file"
temp_secret_file=$(mktemp "$secret_dir/telegramd-blob-secret-key.tmp.XXXXXX")
printf '%s' "$app_secret" > "$temp_secret_file"
chmod 444 "$temp_secret_file"
mv -- "$temp_file" "$env_file"
temp_file=
mv -- "$temp_secret_file" "$app_secret_file"
temp_secret_file=
trap - EXIT HUP INT TERM

printf '%s\n' 'RustFS credentials are provisioned in .env (0600) and .secrets (0700); Compose mounts the app key as a read-only secret file.'
