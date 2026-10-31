#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
command -v kubectl >/dev/null || { echo "kubectl is required" >&2; exit 1; }
command -v jq >/dev/null || { echo "jq is required" >&2; exit 1; }

rendered="$(kubectl kustomize "$SCRIPT_DIR/testdata/public-websocket")"
json="$(printf '%s\n' "$rendered" | kubectl create --dry-run=client -f - -o json | jq -s '.')"

jq -e '
def one($kind): map(select(.kind == $kind))[0];
one("Deployment") as $deployment
| one("Service") as $service
| one("NetworkPolicy") as $policy
| ($deployment.spec.template.spec.containers[]
    | select(.name == "teagramd") | .env | map({(.name): (.value // "")}) | add) as $env
| ([.[].kind] | sort) == ["ConfigMap", "Deployment", "NetworkPolicy", "Service"]
  and all(.[]; .metadata.namespace == "teagram")
  and $deployment.spec.replicas == 2
  and $deployment.spec.template.spec.terminationGracePeriodSeconds == 120
  and ($deployment.spec.template.spec.containers | map(.name) | sort) == ["public-haproxy", "teagramd"]
  and $env.TG_CLIENT_ADDR_TRUST == "proxy-v2"
  and $env.TG_CLIENT_ADDR_PROXY_CIDRS == "127.0.0.1/32"
  and $env.TG_PUBLIC_WEBSOCKET_LISTEN_ADDR == "127.0.0.1:2447"
  and $env.TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR == "server.teagram.io:443"
  and $env.TG_PUBLIC_REGISTRATION == "invite"
  and $env.TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS == "https://web.teagram.io"
  and $env.TG_REPLICA_COUNT == "2"
  and ([ $deployment.spec.template.spec.containers[]
    | select(.name == "teagramd") | .env[]
    | select(.name == "TG_RSA_KEY_FINGERPRINT")
    | .valueFrom.configMapKeyRef.name == "teagramd-public-identity"
      and .valueFrom.configMapKeyRef.key == "TG_RSA_KEY_FINGERPRINT"] | any)
  and $env.TG_WEBSOCKET_LISTEN_ADDR == ""
  and $env.TG_WEBSOCKET_ALLOWED_ORIGINS == ""
  and $service.spec.type == "ClusterIP"
  and ($service.spec.ports | length) == 1
  and $service.spec.ports[0].targetPort == "public-proxy"
  and ($policy.spec.ingress | length) == 1
  and ($policy.spec.ingress[0].from | length) == 1
  and $policy.spec.ingress[0].from[0].podSelector.matchLabels["app.kubernetes.io/name"] == "cloudflared"
  and $policy.spec.ingress[0].ports[0].port == 2446
  and $policy.spec.policyTypes == ["Ingress"]
  and (($deployment.spec.template.spec.hostNetwork // false) == false)
  and all($deployment.spec.template.spec.containers[]; all((.ports // [])[]?; has("hostPort") | not))
  and ([ $deployment.spec.template.spec.containers[]
    | select(.name == "public-haproxy") | .readinessProbe.exec.command[]]
    | any(. == "wget -q -O /dev/null http://127.0.0.1:8404/healthz"))
  and ([ $deployment.spec.template.spec.containers[]
    | select(.name == "public-haproxy") | .args] | .[0] == ["-f", "/usr/local/etc/haproxy/haproxy.cfg"])
  and ([ $deployment.spec.template.spec.containers[]
    | select(.name == "public-haproxy") | .lifecycle.preStop.exec.command[]]
    | any(. == "kill -USR1 1; sleep 90"))
' <<<"$json" >/dev/null

haproxy_config="$(jq -r '.[] | select(.kind == "ConfigMap" and (.metadata.name | startswith("teagramd-public-haproxy-"))) | .data["haproxy.cfg"]' <<<"$json")"
grep -Fq 'req.hdr_cnt(CF-Connecting-IP) eq 1' <<<"$haproxy_config"
grep -Fq 'req.hdr_ip(CF-Connecting-IP) -m found' <<<"$haproxy_config"
grep -Fq 'http-reuse never' <<<"$haproxy_config"
grep -Fq 'send-proxy-v2' <<<"$haproxy_config"
if grep -Fq 'X-Forwarded-For' <<<"$haproxy_config"; then
  echo "public HAProxy config must not trust X-Forwarded-For" >&2
  exit 1
fi

printf '%s\n' 'public WebSocket Kustomize component passed'
