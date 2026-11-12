# Threat model: public user-issued registration invites

## Scope and assets

This model covers user-issued registration invites on the production public
WebSocket listener at `server.teagram.io`. The listener is routed through
Cloudflare Tunnel to `teagramd:2446` and uses `TG_PUBLIC_REGISTRATION=invite`
with `TG_USER_INVITE_QUOTA=5`. `TG_USER_INVITE_MIN_ACCOUNT_AGE` is unset in the
manifest, so the 168-hour default applies. The tailnet listener has a separate
registration policy. Operator-issued invites are covered where they share the
public redemption path; this model does not change their issuance policy.

The protected assets are controlled account creation, invite secrets, and the
record linking an inviter to a redeemed invite. Anonymous internet clients,
invite-link holders, and compromised or malicious eligible accounts are
untrusted. The server, PostgreSQL, deployment configuration, mounted
Turnstile secret, and Cloudflare Tunnel are trusted. Public listener ingress
is admitted only from the `cloudflared` pod identity; the public listener
accepts `CF-Connecting-IP` only from the configured pod CIDR. These checks
together define the proxy boundary. The browser origin allowlist narrows normal
browser use but is not an authorization control for RPC clients.

## Issuance and redemption

A signed-in, non-provisional account at least seven days old can create user
invites while the accepting listener is in `invite` mode. Each account may
create at most five invites in a trailing seven-day window. Used, expired, and
revoked invites still count, and revocation does not restore a quota slot.
Issuance checks the age and quota within an account-scoped transaction, so
concurrent requests cannot exceed that account's quota.

The server generates a 32-byte random secret and returns it once in the create
response as a 64-character hexadecimal bearer link. PostgreSQL stores only its
SHA-256 digest. User-issued links are not bound to a username; any holder may
use one to register the first time it is redeemed. A link expires after seven
days, and redemption atomically consumes it with account creation, preventing
replay or concurrent double use. Invite redemption follows the normal
username sign-up flow and does not prove the invitee's identity or grant the
inviter's permissions.

The public listener also requires the configured signup Turnstile check and
the normal signup IP limit. Production sets the exact Turnstile hostname to
`web.teagram.io`, uses the Siteverify secret from a mounted secret file, keeps
test mode off, and enables a challenge for sign-in. Turnstile is an
anti-automation signal, not identity proof; see the [signup Turnstile threat
model](../../docs/security.md#threat-model-signup-turnstile-verifier). The
signup IP limit defaults to five `auth.signUp` calls per observed client IP
per hour. The trusted Cloudflare header supplies that address only from the
configured forwarder range, and the NetworkPolicy restricts the listener port
to cloudflared pods.

## Abuse cases and controls

| Abuse case | Control and remaining risk |
|---|---|
| An invite link is leaked or publicly reposted | The link is a bearer secret: whoever redeems it first can register an account. It is single-use, expires after seven days, and can be revoked while live. Only the create response contains the secret; list responses omit it. Link recipients must still be treated as unverified people. |
| An eligible account is compromised or deliberately issues invites to abusers | Account age and a per-account rolling quota limit issuance to five per week. Sign-up Turnstile and the per-IP signup budget raise the cost of automated account creation, but do not stop human-assisted or distributed abuse. Newly created accounts can become eligible to issue invites after seven days, so a patient invite chain can grow beyond one inviter's quota. |
| A client replays an invite or races multiple redemptions | Redemption locks and consumes the live invite in the same transaction as account creation. Only one request can succeed; used, expired, revoked, or invalid secrets are rejected. |
| A caller forges `CF-Connecting-IP` to evade per-IP limits | Only requests from the configured Cloudflare pod CIDR may supply the address, and NetworkPolicy permits public listener ingress only from pods labelled `cloudflared`. A compromised trusted forwarder or cluster control plane remains inside the trusted boundary. Clients behind the same NAT share a rate-limit bucket, while distributed source addresses can multiply their budgets. |
| An attacker automates signup or solves Turnstile challenges | Siteverify checks the signup action and exact configured hostname, fails closed on provider errors, and does not accept test credentials in production. A valid challenge is not identity proof; determined attackers may still obtain challenges or use people and distributed addresses. |

## Detection and response

Users can list their own invite metadata and revoke their own live invites;
neither operation reveals another user's invite data. Operators can inspect
`teagramd invite list`, use `teagramd invite invitees <user-id>` to see
redeemers and redemption times, and run `teagramd invite revoke-user
<user-id>` to revoke that inviter's live links. Revoke links shared beyond
their intended recipients and review the inviter and invitee records when
investigating abuse.

To disable only user-issued invite creation and redemption, deploy
`TG_USER_INVITE_QUOTA=0`; operator-issued invites remain usable in invite mode.
To stop every new public registration, deploy
`TG_PUBLIC_REGISTRATION=closed`. Existing accounts can still sign in, and the
separate tailnet registration policy is unchanged. These controls take effect
after the updated configuration is deployed.

The accepted residual risk is that an eligible account can intentionally invite
untrusted people, and invitation chains can grow over time. Turnstile, account
age, per-account quota, per-IP limits, one-time redemption, expiration,
revocation, and operator-visible inviter records reduce and help investigate
abuse; they do not establish invitee identity or guarantee that every abusive
account is detected before use.
