# shellcheck shell=bash

qa_tls_name_constraints() {
  printf 'nameConstraints=critical,permitted;DNS:%s,permitted;IP:%s/255.255.255.255,excluded;DNS:.%s' "$1" "$2" "$1"
}

issue_qa_tls_leaf() {
  local stage=$1 ca_key=$2 ca_cert=$3 key=$4 destination=$5
  if ! openssl req -new -key "$key" -subj "/CN=$HOST_NAME" -out "$stage/server.csr" >/dev/null 2>&1; then
    return 1
  fi
  cat > "$stage/server.ext" <<EXT
basicConstraints=critical,CA:FALSE
keyUsage=critical,digitalSignature,keyEncipherment
extendedKeyUsage=serverAuth
subjectAltName=DNS:$HOST_NAME,IP:$HOST_ADDRESS
subjectKeyIdentifier=hash
authorityKeyIdentifier=keyid,issuer
EXT
  if ! openssl x509 -req -in "$stage/server.csr" -CA "$ca_cert" -CAkey "$ca_key" \
    -CAcreateserial -CAserial "$stage/ca.srl" -days 397 -sha256 -extfile "$stage/server.ext" \
    -out "$stage/server-leaf.crt" >/dev/null 2>&1 ||
    ! openssl verify -CAfile "$ca_cert" -purpose sslserver -verify_hostname "$HOST_NAME" "$stage/server-leaf.crt" >/dev/null 2>&1 ||
    ! openssl verify -CAfile "$ca_cert" -purpose sslserver -verify_ip "$HOST_ADDRESS" "$stage/server-leaf.crt" >/dev/null 2>&1 ||
    ! cat "$stage/server-leaf.crt" "$ca_cert" > "$destination"; then
    return 1
  fi
}

verify_served_qa_tls() {
  local address=$1 expected presented
  expected=$(openssl x509 -in "$TLS_PUBLIC_DIR/server.crt" -noout -fingerprint -sha256) || return 1
  presented=$(timeout 5 openssl s_client -connect "$address" -servername "$HOST_NAME" \
    -CAfile "$TLS_PUBLIC_DIR/ca.crt" -verify_hostname "$HOST_NAME" -verify_return_error </dev/null 2>/dev/null |
    openssl x509 -noout -fingerprint -sha256 2>/dev/null) || return 1
  [[ "$presented" == "$expected" ]]
}

wait_for_qa_tls() {
  local address=$1 attempt
  for ((attempt = 0; attempt < 10; attempt++)); do
    if verify_served_qa_tls "$address"; then return 0; fi
    sleep 1
  done
  return 1
}

reload_qa_tls() { compose exec -T tls-edge nginx -s reload; }

validate_qa_ca() {
  local ca_key=$1 ca_cert=$2 ca_constraints expected_constraints ca_eku ca_public ca_key_public
  ca_constraints=$(openssl x509 -in "$ca_cert" -noout -ext nameConstraints | awk '{$1=$1; print}') ||
    fail 'could not inspect QA CA name constraints'
  expected_constraints=$(printf 'X509v3 Name Constraints: critical\nPermitted:\nDNS:%s\nIP:%s/255.255.255.255\nExcluded:\nDNS:.%s' \
    "$HOST_NAME" "$HOST_ADDRESS" "$HOST_NAME")
  [[ "$ca_constraints" == "$expected_constraints" ]] || fail 'QA CA constraints are missing or unexpected; preserve the identity and migrate trust explicitly'
  ca_eku=$(openssl x509 -in "$ca_cert" -noout -ext extendedKeyUsage | awk '{$1=$1; print}') || fail 'could not inspect QA CA usage'
  [[ "$ca_eku" == $'X509v3 Extended Key Usage: critical\nTLS Web Server Authentication' ]] ||
    fail 'QA CA usage is missing or unexpected; preserve the identity and migrate trust explicitly'
  openssl x509 -in "$ca_cert" -noout -checkend 2592000 >/dev/null || fail 'QA CA certificate expires within 30 days'
  openssl verify -check_ss_sig -CAfile "$ca_cert" "$ca_cert" >/dev/null 2>&1 || fail 'QA CA certificate is invalid'
  ca_public=$(openssl x509 -in "$ca_cert" -pubkey -noout | openssl pkey -pubin -outform DER | sha256sum | cut -d' ' -f1) || fail 'could not read QA CA public key'
  ca_key_public=$(openssl pkey -in "$ca_key" -pubout -outform DER | sha256sum | cut -d' ' -f1) || fail 'could not read QA CA private key'
  [[ "$ca_public" == "$ca_key_public" ]] || fail 'QA CA certificate and private key do not match'
}

ensure_tls() {
  local ca_key=$TLS_PRIVATE_DIR/ca.key ca_cert=$TLS_PUBLIC_DIR/ca.crt
  local key=$TLS_PUBLIC_DIR/server.key cert=$TLS_PUBLIC_DIR/server.crt
  local stage leaf_public key_public
  [[ ! -L "$TLS_DIR" ]] || fail 'TLS directory must not be a symlink'
  install -d -o root -g 101 -m 0750 "$TLS_DIR"
  [[ "$(stat -c '%u:%g:%a' "$TLS_DIR")" == '0:101:750' ]] || fail 'TLS directory has unsafe ownership or mode'
  if [[ -e "$TLS_IDENTITY_DIR" || -L "$TLS_IDENTITY_DIR" ]]; then
    [[ -d "$TLS_IDENTITY_DIR" && ! -L "$TLS_IDENTITY_DIR" && "$(stat -c '%u:%g:%a' "$TLS_IDENTITY_DIR")" == '0:0:700' &&
      -d "$TLS_PRIVATE_DIR" && ! -L "$TLS_PRIVATE_DIR" && "$(stat -c '%u:%g:%a' "$TLS_PRIVATE_DIR")" == '0:0:700' &&
      -d "$TLS_PUBLIC_DIR" && ! -L "$TLS_PUBLIC_DIR" && "$(stat -c '%u:%g:%a' "$TLS_PUBLIC_DIR")" == '0:101:750' &&
      -f "$ca_key" && ! -L "$ca_key" && "$(stat -c '%u:%g:%a' "$ca_key")" == '0:0:600' &&
      -f "$ca_cert" && ! -L "$ca_cert" && "$(stat -c '%u:%a' "$ca_cert")" == '0:644' &&
      -f "$key" && ! -L "$key" && "$(stat -c '%u:%g:%a' "$key")" == '0:101:640' &&
      -f "$cert" && ! -L "$cert" && "$(stat -c '%u:%a' "$cert")" == '0:644' ]] ||
      fail 'QA CA identity has unsafe or incomplete files'
  else
    stage=$(mktemp -d "$TLS_DIR/.identity.XXXXXXXX") || fail 'could not stage the private QA TLS identity'
    TEMP_DIRS+=("$stage")
    install -d -o root -g root -m 0700 "$stage/private"
    install -d -o root -g 101 -m 0750 "$stage/public"
    if ! openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$stage/private/ca.key" >/dev/null 2>&1 ||
      ! openssl req -x509 -new -key "$stage/private/ca.key" -sha256 -days 3650 \
        -subj '/CN=Teagram QA Invite Root CA' -out "$stage/public/ca.crt" \
        -addext 'basicConstraints=critical,CA:TRUE,pathlen:0' \
        -addext 'keyUsage=critical,keyCertSign,cRLSign' \
        -addext 'extendedKeyUsage=critical,serverAuth' \
        -addext 'subjectKeyIdentifier=hash' \
        -addext "$(qa_tls_name_constraints "$HOST_NAME" "$HOST_ADDRESS")" >/dev/null 2>&1 ||
      ! openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$stage/public/server.key" >/dev/null 2>&1 ||
      ! issue_qa_tls_leaf "$stage" "$stage/private/ca.key" "$stage/public/ca.crt" "$stage/public/server.key" "$stage/public/server.crt"; then
      fail 'could not create the private QA TLS identity'
    fi
    rm -f -- "$stage/ca.srl" "$stage/server.csr" "$stage/server.ext" "$stage/server-leaf.crt"
    chown root:root "$stage/private/ca.key" "$stage/public/ca.crt" "$stage/public/server.crt" || fail 'could not protect QA TLS identity'
    chmod 0600 "$stage/private/ca.key" || fail 'could not protect QA CA key'
    chmod 0644 "$stage/public/ca.crt" "$stage/public/server.crt" || fail 'could not protect QA certificates'
    chown root:101 "$stage/public/server.key" || fail 'could not protect QA server key'
    chmod 0640 "$stage/public/server.key" || fail 'could not protect QA server key'
    validate_qa_ca "$stage/private/ca.key" "$stage/public/ca.crt"
    [[ ! -e "$TLS_IDENTITY_DIR" && ! -L "$TLS_IDENTITY_DIR" ]] ||
      fail 'QA TLS identity path appeared while it was being created'
    mv -T -- "$stage" "$TLS_IDENTITY_DIR" || fail 'could not publish the QA TLS identity'
  fi

  validate_qa_ca "$ca_key" "$ca_cert"
  cmp -s "$cert" <(openssl x509 -in "$cert" -outform PEM && cat "$ca_cert") || fail 'QA server certificate chain is inconsistent'
  openssl verify -no_check_time -CAfile "$ca_cert" -purpose sslserver -verify_hostname "$HOST_NAME" "$cert" >/dev/null ||
    fail 'QA server certificate chain or DNS name does not verify'
  openssl verify -no_check_time -CAfile "$ca_cert" -purpose sslserver -verify_ip "$HOST_ADDRESS" "$cert" >/dev/null ||
    fail 'QA server certificate IP address does not verify'
  leaf_public=$(openssl x509 -in "$cert" -pubkey -noout | openssl pkey -pubin -outform DER | sha256sum | cut -d' ' -f1) || fail 'could not read QA server certificate key'
  key_public=$(openssl pkey -in "$key" -pubout -outform DER | sha256sum | cut -d' ' -f1) || fail 'could not read QA server private key'
  [[ "$leaf_public" == "$key_public" ]] || fail 'QA server certificate and private key do not match'

  if ! openssl x509 -in "$cert" -noout -checkend 2592000 >/dev/null; then
    stage=$(mktemp -d "$TLS_DIR/.leaf.XXXXXXXX") || fail 'could not stage QA leaf renewal'
    TEMP_DIRS+=("$stage")
    issue_qa_tls_leaf "$stage" "$ca_key" "$ca_cert" "$key" "$stage/server.crt" ||
      fail 'could not renew the QA TLS server certificate'
    chown root:root "$stage/server.crt" || fail 'could not protect renewed QA certificate'
    chmod 0644 "$stage/server.crt" || fail 'could not protect renewed QA certificate'
    mv -fT -- "$stage/server.crt" "$cert" || fail 'could not publish renewed QA certificate'
    printf '%s\n' 'qa-invite: renewed the TLS leaf using the existing QA CA' >&2
  fi
  openssl x509 -in "$cert" -noout -checkend 2592000 >/dev/null || fail 'QA server certificate expires within 30 days'
  openssl verify -CAfile "$ca_cert" -purpose sslserver -verify_hostname "$HOST_NAME" "$cert" >/dev/null ||
    fail 'QA server certificate chain or DNS name does not verify'
  openssl verify -CAfile "$ca_cert" -purpose sslserver -verify_ip "$HOST_ADDRESS" "$cert" >/dev/null ||
    fail 'QA server certificate IP address does not verify'
}
