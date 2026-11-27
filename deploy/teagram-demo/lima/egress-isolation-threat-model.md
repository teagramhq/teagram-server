# Lima demo egress isolation threat model

## Scope and assets

This policy limits network access from the Lima demo's Docker networks. It
protects Lima's LAN and tailnet, services listening on the Lima host and demo
bridge gateways, and other hosts in private address ranges from a compromised
demo container. It is not a general Docker host firewall or a public egress
allowlist.

The trusted computing base is the Lima host, its root-owned demo checkout, the
Docker daemon, Compose network configuration, OpenRC, and the kernel firewall.
An attacker who can become host root or control Docker can change or bypass
these rules and is outside this policy's protection.

## Trust boundaries and controls

- Forwarded packets from each configured demo source subnet enter
  `TEAGRAM_DEMO_EGRESS` through source-scoped hooks at the start of `FORWARD`
  and in `DOCKER-USER`. Other forwarded packets retain their existing rule
  order; packets from demo subnets continue through host rules when the egress
  chain returns them.
- The egress chain returns traffic addressed to the demo's own subnets and
  related or established replies, drops RFC1918, carrier-grade NAT, link-local
  and multicast destinations, then returns other traffic. This preserves
  internal Compose traffic, embedded DNS at `127.0.0.11`, and public egress.
  The egress proxy, edge and cloudflared services pin upstream DNS to the public
  resolvers `1.1.1.1` and `1.0.0.1`, avoiding private Lima DHCP or MagicDNS
  upstreams that the policy blocks.
- Packets from demo bridges addressed to Lima enter a separate
  `TEAGRAM_DEMO_INPUT` chain. It returns established replies, drops the bridge
  gateway addresses and the same blocked destination ranges, then returns
  remaining traffic. It has no demo-subnet return rule, so host services on
  those gateways are covered.
- When Docker enables IPv6 on a demo network, the same source-scoped forwarding
  and host-input controls drop ULA, link-local and multicast destinations.
  Missing demo networks defer IPv6 changes without removing existing rules;
  read-only `check` rejects until all configured networks can be inspected.
- OpenRC installs the firewall before the stack. Compose restart policies stay
  disabled so dockerd cannot start containers ahead of the firewall. A
  supervised OpenRC process reapplies the rules before recovering stopped
  services and takes a shared deployment lock to avoid racing a release.
- Tailscale can insert a forwarding jump ahead of host hooks when it starts.
  OpenRC orders firewall installation after Tailscale, and the stack supervisor
  checks hook order on each Docker-ready cycle and reapplies drift under the
  shared deployment lock.
- Firewall apply and removal delete listed hooks by exact rule specification.
  If Docker or Tailscale changes another rule between inspection and deletion,
  the update cannot remove that unrelated rule by a stale line number.
- Shadowed-hook repair stages and replaces only demo-owned hooks at the start
  of parent chains; it preserves the content and order of Docker, Tailscale and
  host rules.
- During a hook rebuild, temporary source-scoped hooks reach the same isolation
  chains before stale hooks are removed. A failed replacement insert therefore
  leaves an active hook in place until the stable prefix is installed.
- Apply, supervisor repair and removal serialize through a bounded lock at
  `/run/teagram-demo-egress.lock`; read-only `check` takes no lock. Callers
  holding the shared deploy lock acquire this leaf lock second, and no other
  application lock is acquired while it is held.

## Verification and residual risk

The read-only installer check verifies rule contents, hook counts and hook
precedence. Fake-firewall tests cover idempotent apply, drift detection and
non-mutation by check. The Lima probe verifies that its host-side control
targets answer before testing that those destinations time out from the demo
namespace, while embedded DNS and public HTTPS still work.

This policy blocks the listed private and special-use ranges; it does not
restrict access to public addresses. IPv6 protections apply only to networks
that Docker reports as IPv6-enabled. The host-side probe can verify only
addresses and ports that answer from Lima itself. Host root, Docker, or other
firewall managers remain able to alter the policy and are trusted components.
When tailscaled starts or restarts, its filter-table forwarding jump can briefly
precede the demo hooks and admit demo-to-tailnet packets. The supervisor repairs
that drift on its next Docker-ready cycle; Docker unavailability or deploy-lock
contention can extend the window.
