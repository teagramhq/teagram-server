websocket_exchange_marker_absent() {
  local path="$1" marker="$2" status

  if grep --recursive --binary-files=text --fixed-strings --quiet -- "$marker" "$path"; then
    return 1
  else
    status=$?
    if [[ "$status" -eq 1 ]]; then
      return 0
    fi
    return "$status"
  fi
}
