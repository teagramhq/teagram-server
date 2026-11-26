#!/usr/bin/env bash

demo_deploy_command_valid() {
  [[ $# == 1 ]] || return 1
  local command=$1
  [[ "$command" =~ ^[0-9a-f]{40}\ (sha256:[0-9a-f]{64})$ ]] ||
    [[ "$command" =~ ^qa-invite\ [0-9a-f]{40}\ (up)\ (sha256:[0-9a-f]{64})$ ]] ||
    [[ "$command" =~ ^qa-invite\ [0-9a-f]{40}\ (down|status)$ ]] ||
    [[ "$command" =~ ^qa-invite\ [0-9a-f]{40}\ (create-account)\ ([A-Za-z][A-Za-z0-9_]{1,31})$ ]] ||
    [[ "$command" =~ ^web\ [0-9a-f]{40}\ [0-9a-f]{40}\ (sha256:[0-9a-f]{64})\ [0-9a-f]{64}\ (0|[1-9][0-9]*)$ ]]
}
