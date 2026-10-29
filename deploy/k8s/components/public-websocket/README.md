# Public WebSocket ingress component

This Kustomize component adds the HAProxy public ingress to the dedicated
`Deployment/teagramd-public`. The base deployment must already contain the
`teagramd` container and keep the two public replicas configured by this
component; the public app image and its application secrets remain owned by
the base and prod overlay. Include `../components/public-websocket` in the base
Kustomization's `components` list.

HAProxy and teagramd share a Pod network namespace. HAProxy sends PROXY v2 to
`127.0.0.1:2447`, and the app trusts only `127.0.0.1/32`. The Service is
ClusterIP-only. The NetworkPolicy permits its public port only from pods labeled
`app.kubernetes.io/name: cloudflared` in the `teagram` namespace. Keep the
Cloudflare connector in that namespace with that label and point the tunnel's
`server.teagram.io` ingress at
`http://teagramd-public.teagram.svc.cluster.local:2446`.
Do not add another NetworkPolicy that selects these pods and grants broader
ingress; Kubernetes combines matching policies additively.

The HAProxy config validates exactly one parseable `CF-Connecting-IP` value,
sets the source from it, and never reads `X-Forwarded-For`. `http-reuse never`
ensures each cloudflared keep-alive request gets a fresh backend connection and
PROXY v2 header. Public registration stays invite-only and the public Origin
allowlist stays `https://web.teagram.io`.

The `teagramd-public` deployment must have two replicas while
`TG_REPLICA_COUNT=2`; change both values together so process-local admission
limits retain their intended total. Supply a `teagramd-public-identity`
ConfigMap containing `TG_RSA_KEY_FINGERPRINT`, matching the primary server
identity created by `teagramd ensure-identity`. This component deliberately does not
publish the ordinary MTProto or WebSocket ports. During a rollout HAProxy
soft-stops accepting new connections and waits up to the app's 90-second drain
window before Kubernetes sends its final termination signal.
