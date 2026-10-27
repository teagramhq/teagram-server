#!/usr/bin/env bash

rollout_mark_owned_dir() {
  [ "$#" -eq 1 ] || return 64
  local directory=$1 resolved_directory token marker directory_owner directory_mode current_owner first_entry
  case "$directory" in /*) ;; *) return 64 ;; esac
  case "$directory" in *$'\n'*|*//*|*/../*|*/./*) return 64 ;; esac
  [ -d "$directory" ] && [ ! -L "$directory" ] || return 1
  resolved_directory=$(realpath -e -- "$directory") || return 1
  [ "$resolved_directory" = "$directory" ] || return 1
  directory_owner=$(stat -c %u -- "$directory") || return 1
  directory_mode=$(stat -c %a -- "$directory") || return 1
  current_owner=$(id -u) || return 1
  [ "$directory_owner" = "$current_owner" ] && [ "$directory_mode" = 700 ] || return 1
  first_entry=$(find "$directory" -mindepth 1 -maxdepth 1 -print -quit) || return 1
  [ -z "$first_entry" ] || return 1
  token=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n') || return 1
  [[ "$token" =~ ^[0-9a-f]{32}$ ]] || return 1
  marker="$directory/.rollout-owned-$token"
  (umask 077; set -o noclobber; printf '%s\n' "$token" > "$marker") || return 1
  printf '%s\n' "$token"
}

rollout_create_owned_dir() {
  [ "$#" -eq 1 ] || return 64
  local directory=$1 parent name resolved_parent token
  case "$directory" in /*) ;; *) return 64 ;; esac
  [ ! -e "$directory" ] && [ ! -L "$directory" ] || return 1
  parent=$(dirname -- "$directory") || return 1
  name=$(basename -- "$directory") || return 1
  [ -d "$parent" ] && [ ! -L "$parent" ] || return 1
  resolved_parent=$(realpath -e -- "$parent") || return 1
  [ "$resolved_parent/$name" = "$directory" ] || return 1
  mkdir -m 700 -- "$directory" || return 1
  token=$(rollout_mark_owned_dir "$directory") || {
    rmdir -- "$directory" || return 1
    return 1
  }
  printf '%s\n' "$token"
}

rollout_safe_remove_tree() {
  [ "$#" -eq 4 ] || return 64
  local target=$1 owned_directory=$2 token=$3 checkout=$4
  local target_real owned_real checkout_real marker
  local current_owner directory_owner directory_mode marker_owner marker_mode
  case "$target:$owned_directory:$checkout" in *$'\n'*) return 64 ;; esac
  case "$target:$owned_directory:$checkout" in *'//'*|*'/../'*|*'/./'*) return 64 ;; esac
  case "$target:$owned_directory:$checkout" in /*:/*:/*) ;; *) return 64 ;; esac
  [[ "$token" =~ ^[0-9a-f]{32}$ ]] || return 64

  checkout_real=$(realpath -m -- "$checkout") || return 1
  case "$target" in
    /|/root|/root/*|/opt|/opt/*|"$checkout_real"|"$checkout_real"/*) return 1 ;;
  esac

  [ -d "$owned_directory" ] && [ ! -L "$owned_directory" ] || return 1
  [ -d "$target" ] && [ ! -L "$target" ] || return 1
  owned_real=$(realpath -e -- "$owned_directory") || return 1
  target_real=$(realpath -e -- "$target") || return 1
  [ "$owned_real" = "$owned_directory" ] && [ "$target_real" = "$target" ] || return 1
  case "$target_real" in "$owned_real"/*) ;; *) return 1 ;; esac
  case "$target_real" in
    /|/root|/root/*|/opt|/opt/*|"$checkout_real"|"$checkout_real"/*) return 1 ;;
  esac

  marker="$owned_real/.rollout-owned-$token"
  [ -f "$marker" ] && [ ! -L "$marker" ] || return 1
  directory_owner=$(stat -c %u -- "$owned_real") || return 1
  directory_mode=$(stat -c %a -- "$owned_real") || return 1
  marker_owner=$(stat -c %u -- "$marker") || return 1
  marker_mode=$(stat -c %a -- "$marker") || return 1
  current_owner=$(id -u) || return 1
  [ "$directory_owner" = "$current_owner" ] && [ "$marker_owner" = "$current_owner" ] && \
    [ "$directory_mode" = 700 ] && [ "$marker_mode" = 600 ] || return 1
  [ "$(cat -- "$marker")" = "$token" ] || return 1

  /bin/rm -rf -- "$target_real"
}
