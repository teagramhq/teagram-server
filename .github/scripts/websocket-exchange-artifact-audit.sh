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

websocket_exchange_print_server_dh_boundary() {
  local line candidate subject result unknown_all=false
  declare -A seen=() results=()
  while IFS= read -r line; do
    [[ "$line" == *synthetic_server_dh_boundary* ]] || continue
    candidate="synthetic_server_dh_boundary${line#*synthetic_server_dh_boundary}"
    if [[ "$candidate" =~ ^synthetic_server_dh_boundary\ subject=(actual|control)\ result=(decrypt_entered|not_received|envelope_rejected|constructor_rejected|nonce_mismatch|unknown)$ ]]; then
      subject="${BASH_REMATCH[1]}"
      result="${BASH_REMATCH[2]}"
    elif [[ "$candidate" =~ ^synthetic_server_dh_boundary\ subject=(actual|control)([[:space:]]|$) ]]; then
      subject="${BASH_REMATCH[1]}"
      result=unknown
    else
      unknown_all=true
      continue
    fi
    if [[ "${seen[$subject]:-}" == true ]]; then
      results[$subject]=unknown
    else
      seen[$subject]=true
      results[$subject]="$result"
    fi
  done < "$1"
  if [[ "$unknown_all" == true ]]; then
    printf 'synthetic_server_dh_boundary subject=actual result=unknown\n'
    printf 'synthetic_server_dh_boundary subject=control result=unknown\n'
    return
  fi
  for subject in actual control; do
    if [[ "${seen[$subject]:-}" == true ]]; then
      printf 'synthetic_server_dh_boundary subject=%s result=%s\n' "$subject" "${results[$subject]}"
    fi
  done
}

websocket_exchange_print_server_dh_control_checkpoint() {
  local line candidate result=unknown found=false duplicate=false overflow=false
  local boundary actual=unknown control=unknown subject boundary_result boundary_count=0
  while IFS= read -r line; do
    [[ "$line" == *synthetic_server_dh_control_checkpoint* ]] || continue
    if [[ "$found" == true ]]; then
      duplicate=true
      result=unknown
      continue
    fi
    found=true
    candidate="synthetic_server_dh_control_checkpoint${line#*synthetic_server_dh_control_checkpoint}"
    if [[ "${#line}" -gt 512 ]]; then
      overflow=true
    fi
    if [[ "$overflow" == true ]]; then
      result=unknown
    elif [[ "$candidate" =~ ^synthetic_server_dh_control_checkpoint\ result=(capture_incomplete|control_not_entered|decrypt_entered|unknown)$ ]]; then
      result="${BASH_REMATCH[1]}"
    else
      result=unknown
    fi
  done < "$1"

  if [[ "$duplicate" == true ]]; then
    result=unknown
  fi
  boundary="$(websocket_exchange_print_server_dh_boundary "$1")"
  while IFS= read -r line; do
    [[ -n "$line" ]] || continue
    if [[ "$line" =~ ^synthetic_server_dh_boundary\ subject=(actual|control)\ result=(decrypt_entered|not_received|envelope_rejected|constructor_rejected|nonce_mismatch|unknown)$ ]]; then
      subject="${BASH_REMATCH[1]}"
      boundary_result="${BASH_REMATCH[2]}"
      boundary_count=$((boundary_count + 1))
      if [[ "$subject" == actual ]]; then
        actual="$boundary_result"
      elif [[ "$subject" == control ]]; then
        control="$boundary_result"
      fi
    else
      boundary_count=0
      actual=invalid
      control=invalid
      break
    fi
  done <<< "$boundary"

  if [[ "$result" == decrypt_entered && ("$boundary_count" -ne 2 || "$control" != decrypt_entered) ]]; then
    result=unknown
  elif [[ "$result" == capture_incomplete || "$result" == control_not_entered ]]; then
    if [[ "$boundary_count" -ne 2 || "$actual" != unknown || "$control" != unknown ]]; then
      result=unknown
    fi
  fi
  printf 'synthetic_server_dh_control_checkpoint result=%s\n' "$result"
}
