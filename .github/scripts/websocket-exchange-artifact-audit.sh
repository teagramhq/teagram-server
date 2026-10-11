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

websocket_exchange_print_pre_request_markers() {
  local line candidate case_name category
  declare -A printed=()
  while IFS= read -r line; do
    [[ "$line" == *synthetic_pre_request_failure* ]] || continue
    candidate="synthetic_pre_request_failure${line#*synthetic_pre_request_failure}"
    if [[ "$candidate" =~ ^synthetic_pre_request_failure\ case=(A|B|C|N1|N2|N3)\ category=(client_initialization|transport_selection|transport_construction|unknown)$ ]]; then
      case_name="${BASH_REMATCH[1]}"
      category="${BASH_REMATCH[2]}"
    elif [[ "$candidate" =~ ^synthetic_pre_request_failure\ case=(A|B|C|N1|N2|N3)([[:space:]]|$) ]]; then
      case_name="${BASH_REMATCH[1]}"
      category=unknown
    else
      for case_name in A B C N1 N2 N3; do
        if [[ -z "${printed[$case_name]:-}" ]]; then
          printf 'synthetic_pre_request_failure case=%s category=unknown\n' "$case_name"
          printed[$case_name]=true
        fi
      done
      continue
    fi
    if [[ -z "${printed[$case_name]:-}" ]]; then
      printf 'synthetic_pre_request_failure case=%s category=%s\n' "$case_name" "$category"
      printed[$case_name]=true
    fi
  done < "$1"
}

websocket_exchange_print_client_output_failure() {
  local line candidate category=unknown found=false
  while IFS= read -r line; do
    [[ "$line" == *synthetic_client_output_failure* ]] || continue
    if [[ "$found" == true ]]; then
      category=unknown
      continue
    fi
    found=true
    candidate="synthetic_client_output_failure${line#*synthetic_client_output_failure}"
    if [[ "$candidate" =~ ^synthetic_client_output_failure\ category=(file_read|json_decode|unknown)$ ]]; then
      category="${BASH_REMATCH[1]}"
    else
      category=unknown
    fi
  done < "$1"
  if [[ "$found" == true ]]; then
    printf 'synthetic_client_output_failure category=%s\n' "$category"
  fi
}
