rollback_failed_up() {
  local previous_ref=$1 dump=$2 restore_ok=1 running_containers
  if ! compose down --remove-orphans; then
    printf '%s\n' 'qa-invite: compose down reported an error; verifying the project before database recovery' >&2
  fi
  if ! running_containers=$(docker ps --quiet --filter 'label=com.docker.compose.project=teagram-qa-invite'); then
    restore_ok=0
    printf '%s\n' 'qa-invite: could not verify that project containers stopped; database restore skipped' >&2
  elif [[ -n "$running_containers" ]]; then
    restore_ok=0
    printf '%s\n' 'qa-invite: project containers remain running; database restore skipped' >&2
  fi
  if [[ "$restore_ok" == 1 && -n "$dump" && -s "$dump" ]]; then
    compose up -d --wait postgres || restore_ok=0
    compose exec -T postgres dropdb -U postgres --if-exists teagram_rollback || restore_ok=0
    compose exec -T postgres createdb -U postgres teagram_rollback || restore_ok=0
    if compose exec -T postgres pg_restore -U postgres --no-owner --no-privileges \
      --exit-on-error -d teagram_rollback < "$dump" &&
      compose exec -T postgres dropdb -U postgres teagram </dev/null &&
      compose exec -T postgres createdb -U postgres teagram </dev/null &&
      compose exec -T postgres pg_restore -U postgres --no-owner --no-privileges \
        --exit-on-error -d teagram < "$dump"; then
      compose exec -T postgres dropdb -U postgres --if-exists teagram_rollback || restore_ok=0
    else
      restore_ok=0
      printf '%s\n' 'qa-invite: automatic database restore failed; preserve the dump for manual recovery' >&2
    fi
  fi
  if [[ -n "$previous_ref" && "$restore_ok" == 1 ]]; then
    write_env "$previous_ref"
    compose up -d --wait --no-deps teagramd tls-edge haproxy || {
      restore_ok=0
      printf '%s\n' 'qa-invite: previous image could not be restored automatically' >&2
    }
  fi
  [[ "$restore_ok" == 1 ]] || printf '%s\n' 'qa-invite: rollback incomplete; inspect project containers and preserve the database dump for recovery' >&2
}
