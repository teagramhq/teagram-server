// Package config loads server configuration from environment variables.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gotd/td/exchange"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/keycrypt"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/stickerbundle"
	"github.com/teagramhq/teagram-server/internal/store"
)

// RegistrationMode controls whether new accounts can be created via auth.signUp.
type RegistrationMode string

// StickerImportMode controls whether accounts may create or add to sticker sets.
type StickerImportMode string

// RecoveryEmailMode controls whether client-supplied recovery emails are retained.
type RecoveryEmailMode string

const (
	RegistrationClosed             RegistrationMode  = "closed"
	RegistrationInvite             RegistrationMode  = "invite"
	RegistrationOpen               RegistrationMode  = "open"
	StickerImportOn                StickerImportMode = "on"
	StickerImportOff               StickerImportMode = "off"
	RecoveryEmailOff               RecoveryEmailMode = "off"
	RecoveryEmailContact           RecoveryEmailMode = "contact"
	DefaultUploadPartTTL                             = 6 * time.Hour
	DefaultUserInviteMinAccountAge                   = 7 * 24 * time.Hour
	DefaultAuthKeySweepIdleWindow                    = 24 * time.Hour
	DefaultAuthKeySweepMaxAge                        = 72 * time.Hour
	DefaultSignUpTermsOfService                      = "Teagram is software. Its authors hold no liability for what operators or users do with a self-hosted Teagram instance. Each instance's operator and its users are responsible for the content on that instance, including sticker and emoji packs users import."
)

// Config holds server configuration.
type Config struct {
	ListenAddr string
	// WebSocketListenAddr is the address the WebSocket MTProto server binds to.
	// Empty disables the endpoint entirely.
	WebSocketListenAddr string
	// WebSocketOriginPatterns is the comma-separated allowlist of browser
	// origins accepted by the WebSocket endpoint. Empty accepts no Origin
	// header, while clients that send no Origin remain valid.
	WebSocketOriginPatterns []string
	// PublicWebSocketListenAddr enables the separate WebSocket listener for
	// public traffic. Empty disables the endpoint.
	PublicWebSocketListenAddr string
	// PublicWebSocketOriginPatterns is the independent browser-origin allowlist
	// for the public WebSocket endpoint.
	PublicWebSocketOriginPatterns []string
	// AdminListenAddr is the address the admin HTTP server binds to.
	// Empty disables the admin server entirely.
	AdminListenAddr string
	// BotAPIListenAddr is the loopback or tailnet-only Bot API HTTP listener.
	// Empty disables the listener.
	BotAPIListenAddr string
	// AdminOrigin is the fixed Origin accepted by admin login and logout.
	// It is configured explicitly or derived from AdminListenAddr at startup.
	AdminOrigin string
	// AdminTokenHash is the hex-encoded SHA-256 digest of the operator token.
	// Only set when AdminListenAddr is non-empty.
	AdminTokenHash string
	// ReplicaID is the optional operator-supplied identity of this process's
	// deployment instance. It is exposed only on authenticated admin metrics.
	ReplicaID    string
	ReplicaCount int
	PostgresDSN  string
	PostgresPool store.PoolConfig
	RSAKeyPath   string
	// ExpectedRSAFingerprint pins the loaded key to the deployment identity.
	// It is optional for legacy single-replica deployments, but required when
	// TG_REPLICA_ID identifies this process as part of a replica deployment.
	ExpectedRSAFingerprint *int64
	// AdvertiseDemoGuestSignUp advertises demo guest sign-up availability in
	// discovery only; it does not change account registration policy.
	AdvertiseDemoGuestSignUp bool
	// RegistrationMode controls whether auth.signUp is available.
	RegistrationMode RegistrationMode
	// StickerImportMode controls whether accounts may create or add to sticker sets.
	StickerImportMode StickerImportMode
	// SignUpTermsOfService is shown to new accounts in auth.authorizationSignUpRequired.
	SignUpTermsOfService string
	// RecoveryEmailMode controls whether password changes retain a contact email.
	RecoveryEmailMode RecoveryEmailMode
	// PublicRegistrationMode controls auth.signUp on the public WebSocket
	// listener. It defaults to closed so enabling that listener alone cannot
	// open registration.
	PublicRegistrationMode RegistrationMode
	// UserInviteQuota enables redemption of user-issued registration invites
	// when positive. It defaults to zero so existing invite-mode deployments
	// must opt in explicitly.
	UserInviteQuota int
	// UserInviteMinAccountAge controls the age gate for creating user invites.
	// It defaults to one week and uses the database's users.created_at clock.
	UserInviteMinAccountAge time.Duration

	// WelcomeChannel is the operator-configured public broadcast channel for new accounts.
	WelcomeChannel string
	// StickerBundle is the validated optional static sticker catalogue bundle.
	// It remains nil when TG_STICKER_CATALOG_BUNDLE is unset or empty.
	StickerBundle *stickerbundle.Bundle `json:"-"`
	// Turnstile contains the optional public-auth challenge credentials. SecretKey
	// is consumed only by the RPC verifier and must never be logged or exposed.
	Turnstile TurnstileConfig `json:"-"`
	// AuthKeyEncKey is the 32-byte master key that encrypts auth keys at rest.
	//
	// Changing it is a total re-auth event: no stored auth key opens under a new
	// key, so every client re-handshakes. Peer access hashes are derived from
	// this material too (internal/peerhash) and carry no key epoch, which is
	// only safe while that stays true — before adding any dual-key rotation that
	// lets a session survive a key change, read the key rotation constraint in
	// the internal/peerhash package doc.
	AuthKeyEncKey []byte `json:"-"`
	// MediaKeys is the media key ring from TG_MEDIA_KEY_DIR and
	// TG_MEDIA_KEY_CURRENT, or nil when neither is set. Fresh S3 stores use this
	// ring to encrypt objects; accepted plaintext cutovers leave it unused.
	MediaKeys *blob.KeyRing `json:"-"`
	// AdvertiseHost and AdvertisePort are the address clients are told to
	// dial, which is not always the one the server binds: a listener on every
	// interface still has to name one address a client can reach it by.
	AdvertiseHost string
	AdvertisePort int
	// PublicWebSocketAdvertiseHost and PublicWebSocketAdvertisePort set the
	// public listener's browser transport host. The MTProto endpoint remains
	// AdvertiseHost and AdvertisePort.
	PublicWebSocketAdvertiseHost string
	PublicWebSocketAdvertisePort int
	// PublicLinkPrefix is the HTTPS prefix returned by help.getConfig and used
	// when rendering server-owned invite links. It is validated only for the
	// MTProto server; administrative commands do not need it.
	PublicLinkPrefix string
	DCID             int
	// LogLoginCodes opts into writing issued login codes to the log in
	// cleartext. Off by default: the log is readable by anyone with the
	// process output, and the code alone signs in any account that has no
	// 2FA cloud password — one with a password still needs the SRP step.
	LogLoginCodes bool
	// LogAuthKeyUnbinds opts into logging the operation, reason, outcome, and
	// identifiers when the server clears an auth-key binding. It defaults off.
	LogAuthKeyUnbinds bool
	// SessionIP controls whether client addresses are stored on authorization
	// rows and returned by account.getAuthorizations. It defaults off.
	SessionIP bool
	// SessionDeviceLabels controls whether initConnection device and app labels
	// are stored on authorization rows and returned by account.getAuthorizations.
	// It defaults on for compatibility with existing device management behavior.
	SessionDeviceLabels bool
	// MaxFileBytes caps one uploaded file. TG_UPLOAD_MAX_BYTES overrides the
	// existing TG_MAX_FILE_BYTES setting when provided. A user's outstanding
	// unassembled bytes are capped at twice this, which is two concurrent
	// max-size uploads.
	MaxFileBytes int64
	// BlobDir is where uploaded file bodies are stored. It must be outside the
	// repository and outside anything a future HTTP surface serves statically.
	BlobDir string
	// BlobS3 selects the S3-compatible blob backend when any TG_BLOB_S3_*
	// setting is non-empty. A nil value keeps the local filesystem backend as
	// the default. The secret is resolved during Load so startup can reject a
	// missing or unreadable credential before serving requests.
	BlobS3 *blob.S3Config
	// MaxUserStorageBytes caps the total size of one account's uploaded files.
	// M5 ships no blob deleter, so nothing decrements this: it is a lifetime
	// quota per account, not a live one, and it is the number that decides
	// whether one account can fill the disk.
	MaxUserStorageBytes int64
	// UploadsDisabled refuses client RPCs that accept or assemble uploaded file
	// bytes. It defaults off so existing deployments keep their current behavior.
	UploadsDisabled bool
	// UploadPartTTL is how long an unassembled upload part is kept before the
	// sweeper deletes it. Short on purpose: a real client uploads and sends
	// within minutes, and the TTL is the term that makes worst-case retained
	// bytes finite at accounts x cap.
	UploadPartTTL time.Duration
	// AuthKeySweepIdleWindow and AuthKeySweepMaxAge bound retention for keys
	// that have never been bound to an account. Setting either to zero disables
	// the sweep.
	AuthKeySweepIdleWindow time.Duration
	AuthKeySweepMaxAge     time.Duration
	// MediaErasureMinAge is how old a file must be before the media erasure
	// report will name it, and before the erasure sweep will remove it.
	//
	// The default is 24 hours, deliberately generous while MAIN-338 remains
	// open. It is an additional retention gate for crashed or unassembled rows,
	// not the live-assembly safety control: assembly holds the files row FOR
	// SHARE before Put through the MarkFileStored commit, and the eraser skips a
	// held row. A process crash releases the lock and leaves the row reclaimable;
	// a smaller cutoff may reclaim that crashed state sooner without deleting a
	// live assembly.
	MediaErasureMinAge time.Duration
	// MediaErasureReportInterval is how often that report runs. Zero disables
	// it, as a zero limit disables a rate limit, and zero is the default: the
	// report is opt-in, and turning it on is a decision made against the size
	// of a particular deployment's media corpus.
	//
	// The reason it is not on by default is measured, not cautious. The
	// reference predicate's EXISTS sits in the report query's select list, so
	// the planner cannot lift it into a semi-join; it stays a SubPlan, and no
	// index on messages leads with file_id. While Postgres can hash that
	// SubPlan the cost is one scan per batch, but past roughly 300k media
	// messages it cannot, and the plan flips to one scan of messages per files
	// row: at 20k files and 300k media messages, one 1000-row batch measures
	// 2.2s and ~302k shared-buffer hits, so an hourly full walk is tens of
	// gigabytes of buffer traffic evicting whatever the download path had
	// cached. An operator turning this on wants either a small corpus or the
	// index the eraser ticket will decide on.
	MediaErasureReportInterval time.Duration
	// MediaErasureIntervalMin and MediaErasureIntervalMax bound how long the
	// erasure sweep waits between passes. Each interval is drawn uniformly from
	// the range; a zero minimum disables the sweep, and zero is the default.
	//
	// A range rather than a period, and the range cannot collapse to a point:
	// both variables are required together and the maximum must exceed the
	// minimum. That is a security requirement rather than a scheduling
	// preference. Per-account usage is summed off the files table, so freeing
	// quota is observable to the uploader — they can fill their quota, send the
	// file to someone else, delete their own copy, and then poll by attempting an
	// upload, which fails until the recipient deletes their copy and succeeds
	// afterwards. That turns another account's private deletion into a receipt,
	// timed to within one sweep interval. It cannot be removed without giving up
	// the quota-freeing this milestone exists to deliver, so it is accepted and
	// blunted: a randomized interval degrades "at 14:32" to "eventually".
	MediaErasureIntervalMin time.Duration
	MediaErasureIntervalMax time.Duration
	// MediaErasureDestructive turns the sweep from a report into a delete. False
	// is the default and it is the whole shipping posture of this capability:
	// with it unset the sweep counts what it could reclaim and removes nothing,
	// which is what a fresh deployment gets.
	//
	// Enabling it is a decision about a particular deployment, not a default,
	// because the blob volume has no backup and no restore path: a file this
	// removes is gone, and no sequence of operator actions brings it back. Turn
	// the sweep on in its reporting mode first and read what it says it would
	// free.
	MediaErasureDestructive bool
	// BlobScanTempMinAge is how old the blob writer's temporary file must be
	// before the disk report or assembled-blob reclaim will name it as
	// abandoned. Age is an additional retention gate, not the live-assembly
	// safety control: the assembly holds the files row FOR SHARE before Put and
	// the reclaim path skips a held row. The default remains 24 hours while
	// MAIN-338 is open, and the value stays configurable for crash-retention
	// policy.
	BlobScanTempMinAge time.Duration
	// BlobScanReportInterval is how often the disk report runs. Zero disables
	// it and is the default, matching the media erasure report: both are
	// opt-in, and both are turned on against the size of a particular
	// deployment.
	//
	// The cost here is a walk of the blob tree — one stat per path — plus one
	// primary-key probe per thousand blobs, so it is dominated by the
	// filesystem rather than by the database, and it scales with the number of
	// files on disk rather than with the media corpus's reference graph. It
	// takes no lock and opens no transaction either way.
	BlobScanReportInterval time.Duration
	// RateLimits holds the per-surface rate-limit configurations. Zero limit
	// disables enforcement for that surface.
	RateLimits RateLimitsConfig
	// DiscoveryLimits bounds valid local-direct preflight requests admitted to
	// the response path. The connection bounds still cover partial requests;
	// these limits cover the response attempts that pass admission.
	DiscoveryLimits mtproto.DiscoveryLimits
	// ClientAddrTrust names where the address a request is attributed to comes
	// from: the connection's own peer address, or a PROXY protocol v2 header.
	ClientAddrTrust ClientAddrTrust
	// ClientAddrProxies is the allowlist of balancer source addresses a PROXY
	// header is honoured from. It is the whole of the trust decision in
	// ClientAddrProxyV2 mode and is empty in every other mode.
	ClientAddrProxies []netip.Prefix
	// PreAuth is the deployment-wide budget for connections that have not
	// authenticated: concurrent sockets, concurrent sockets per client network,
	// and their maximum lifetime. teagramd divides the two concurrency budgets
	// by ReplicaCount; zero disables a bound.
	PreAuth mtproto.PreAuthLimits
	// KeyExchangeLimits bounds unauthenticated RSA key-exchange work. The
	// per-network budgets are shared by Postgres; teagramd divides the global
	// in-memory ceiling by ReplicaCount.
	KeyExchangeLimits mtproto.ExchangeLimits
	// MaxConnsPerUnboundKey is the deployment-wide bound on concurrent
	// connections one auth key with nobody signed in on it may hold. It picks up
	// where PreAuth stops counting and hands over to the per-user connection cap
	// at sign-in. teagramd divides it by ReplicaCount; zero disables it.
	MaxConnsPerUnboundKey int
	// MaxPendingLoginConns bounds connections waiting for auth.checkPassword
	// after SESSION_PASSWORD_NEEDED across the deployment. Postgres leases hold
	// the shared slots; zero disables the cap.
	MaxPendingLoginConns int
	// RPCDeadline is how long a single dispatched RPC may run before its
	// context is cancelled and the client answered with a generic INTERNAL
	// error. The connection survives; the next request starts with a full
	// budget, so a chunked upload or download is bounded per chunk, never per
	// logical transfer. It is what puts an upper bound on how long one RPC can
	// hold a database transaction open — see mtproto.DefaultRPCDeadline for
	// where the shipped number comes from. Startup overrides must be positive
	// and no greater than 45 seconds so admitted work fits the shutdown drain.
	RPCDeadline time.Duration
	// StatementTimeout is the Postgres statement_timeout every pooled
	// connection runs under: one SQL statement past it is cancelled server-side
	// and its transaction rolls back. It is the database-side half of the same
	// bound RPCDeadline puts on the handler side. See
	// store.WithStatementTimeout. Zero disables it.
	StatementTimeout time.Duration
	// ProfileGalleryRetention is how long the identifier-only records of a
	// gallery mutation have to outlive the gallery rows they describe: the
	// off-alpha erasure-ledger record for a gallery delete, and the local rows
	// its replay re-admits.
	//
	// Identifier-only is exact. A record carries the owner, the file id, the
	// client file id, the mutation revision and the opaque server-assigned
	// operation key, and nothing else. An auth key id, session id, message id,
	// file path, access hash, caption, display name, phone number, network
	// address, blob digest, key material, secret-chat content and transport
	// metadata must never enter anything that leaves alpha.
	//
	// The floor is a restore requirement, not a disk budget. A restore that
	// predates a gallery delete revives the photo that delete cleared, and the
	// only thing that clears it again is the ledger record. Set under the floor,
	// the record is compacted while a dump that can resurrect the photo is still
	// retained, and an acknowledged deletion silently becomes an avatar again.
	// Startup therefore refuses a value under the floor, accepts equal-to-floor,
	// and refuses one nanosecond under it.
	//
	// The per-owner mutation revision (profile_photo_state) is not covered by any
	// setting here: that row is never compacted, because an acknowledged
	// deletion has to stay acknowledged across a restore that predates the
	// upload it cleared. Nothing in this configuration expires, compacts, or
	// deletes anything; the values are promises checked at startup, and no
	// retention job is wired to them.
	//
	// The assets, trust boundaries, threats and mitigations this surface exists to
	// cover are written down in ROADMAP.md, "Profile retention threat model".
	ProfileGalleryRetention time.Duration
	// ProfileReceiptRetention is how long a terminal upload receipt is kept: the
	// record that makes a retry under a deleted photo's client file id the
	// uniform unavailable-photo refusal rather than a fresh upload of a photo its
	// owner deleted.
	//
	// Two horizons bound it and it must clear both. The restore horizon, for the
	// same reason as the gallery record. And the retry horizon: a client keeps
	// one upload alive by re-saving parts, so while UploadPartTTL has not expired
	// that client file id is a request the owner is still entitled to finish.
	// A receipt that expires inside that window turns an in-flight completion
	// into a new upload, with a new charge against the owner's lifetime quota,
	// for bytes they already paid for. So the bound is the part TTL plus
	// the deadline that request ran under, and startup refuses a part TTL a
	// receipt cannot outlive, naming TG_UPLOAD_PART_TTL, because that is the
	// variable the operator has to lower.
	//
	// Once a receipt is compacted the same client file id is a new upload, which
	// is a fresh charge and not a resurrection of the deleted photo. That is why
	// this is a startup bound and not a promise of unbounded receipt storage.
	ProfileReceiptRetention time.Duration
	// ProfileDeleteOperationRetention bounds how long a local
	// deletion-operation dedup row is kept: the (owner, auth key, session,
	// message id) record that lets a retry of an acknowledged gallery clear find
	// the operation that already ran, instead of resolving "current" a second
	// time and clearing a photo the owner never named.
	//
	// These rows never leave alpha and are never part of a ledger record: auth
	// key, session and message ids are transport identities, the replayer has no
	// use for them, and shipping them off-alpha breaks the minimal-identifier
	// rule. The event's operation identity is the opaque operation key.
	//
	// The transport has no message-id freshness window, so this bound cannot be
	// derived from a protocol window; it is the restore horizon. A dedup row
	// compacted before the last restore point that can revive its request leaves
	// a late retry to act as a fresh clear, which changes nothing but that
	// account's own avatar.
	ProfileDeleteOperationRetention time.Duration
}

// TurnstileConfig holds the public widget key, private Siteverify key, and the
// independent opt-in for signin challenges. Configured credentials continue to
// protect signup when SignInEnabled is false.
type TurnstileConfig struct {
	SiteKey       string
	SecretKey     string   `json:"-"`
	SignInEnabled bool     `json:"-"`
	Hostnames     []string `json:"-"`
	TestMode      bool     `json:"-"`
	QABypassToken string   `json:"-"`
}

// Enabled reports whether the signup Siteverify gate is configured.
func (c TurnstileConfig) Enabled() bool { return c.SecretKey != "" }

// ClientConfig contains the settings needed to render the public discovery
// document. Unlike Config, loading it does not require Postgres, the auth-key
// encryption master key, blob storage, or any other service resource.
type ClientConfig struct {
	ListenAddr                string
	PublicWebSocketListenAddr string
	RSAKeyPath                string
	AdvertiseHost             string
	AdvertisePort             int
	DCID                      int
	TurnstileSiteKey          string
	TurnstileEnabled          bool
	// AdvertiseDemoGuestSignUp is an availability hint only; registration
	// policy remains controlled by the server's registration configuration.
	AdvertiseDemoGuestSignUp bool
}

// ClientAddrTrust names the source a client address is taken from.
type ClientAddrTrust string

// ClientAddrSocket attributes every request to the peer address of the socket
// it arrived on. It is the only source that cannot be influenced by the client:
// MTProto carries no address field, and any header a future one gained would be
// forgeable by whoever sends it.
const ClientAddrSocket ClientAddrTrust = "socket"

// ClientAddrProxyV2 attributes every request to the address a PROXY protocol v2
// header names, and only when that header arrives from an address in
// ClientAddrProxies. It is what makes per-IP limits mean anything behind an L4
// load balancer, where every peer address is the balancer's and socket keying
// collapses into one global bucket.
//
// The allowlist is the entire reason the header can be believed: it is
// client-supplied bytes, so a sender outside the allowlist is refused rather
// than credited with an address it chose. There is no third source.
const ClientAddrProxyV2 ClientAddrTrust = "proxy-v2"

// clientAddrTrustValues lists every supported value, for the startup error. An
// operator naming a mode this build does not implement must be told plainly
// rather than silently served socket addresses that all belong to the balancer.
var clientAddrTrustValues = []ClientAddrTrust{ClientAddrSocket, ClientAddrProxyV2}

// RateLimitsConfig holds the rate-limit parameters for each RPC surface.
type RateLimitsConfig struct {
	// MessageSend limits all client-visible message sends (1:1, chat, channel
	// post, media send, forward) to one shared budget per account.
	MessageSend store.RateLimitConfig
	// CreateChat limits messages.createChat per account.
	CreateChat store.RateLimitConfig
	// AddChatUser limits messages.addChatUser per account.
	AddChatUser store.RateLimitConfig
	// CreateChannel limits channels.createChannel per account.
	CreateChannel store.RateLimitConfig
	// SearchMessages limits messages.search per account.
	SearchMessages store.RateLimitConfig
	// GetMessages limits messages.getMessages per account, shared across sessions.
	GetMessages store.RateLimitConfig
	// SearchContacts limits contacts.search per account.
	SearchContacts store.RateLimitConfig
	// SearchGlobal limits messages.searchGlobal per account, on a budget of its
	// own: a cross-dialog search reads every dialog the caller is in, so one call
	// is not the same unit of work as a search inside a named peer.
	SearchGlobal store.RateLimitConfig
	// PollVote limits messages.sendVote per account.
	PollVote store.RateLimitConfig
	// CreateStickerSet limits expensive stickers.createStickerSet parsing per account.
	CreateStickerSet store.RateLimitConfig
	// SaveFilePart limits upload.saveFilePart and upload.saveBigFilePart per
	// account, on one shared budget: both write the same rows, so a budget each
	// would let an account double its part rate by alternating between them.
	SaveFilePart store.RateLimitConfig
	// UploadBytes limits uploaded bytes per account. The limit uses raw byte
	// units, defaults to a one-hour window, and is disabled until
	// TG_RATE_LIMIT_UPLOAD_BYTES is set.
	UploadBytes store.RateLimitConfig
	// GetFile limits upload.getFile per account. It is backed by the existing
	// Postgres rate-limit counter, so concurrent requests for one account are
	// admitted exactly across connections and replicas.
	GetFile store.RateLimitConfig
	// GetFileReplica limits upload.getFile across every teagramd replica through
	// the shared Postgres rate-limit counter.
	GetFileReplica store.RateLimitConfig
	// SendCodeIP limits auth.sendCode per client network. It is keyed on the
	// connection's address rather than an account because the surface is
	// unauthenticated: there is no account yet to hold a budget.
	SendCodeIP store.SendCodeIPLimits
	// SignInFailIP limits failed auth.signIn attempts per client network.
	// Keyed on the connection's address, not the identifier: only the
	// attacker's own IP budget is consumed, never the victim's.
	SignInFailIP store.RateLimitConfig
	// CheckPassword limits failed auth.checkPassword attempts per account.
	// Charged only on failed SRP proofs; a valid proof is never charged.
	CheckPassword store.RateLimitConfig
	// CheckPasswordIP limits failed auth.checkPassword attempts per client
	// network. Keyed on the connection's address. Charged only on failures.
	CheckPasswordIP store.RateLimitConfig
	// GetPasswordIP limits account.getPassword calls per client network, but
	// only for unauthenticated callers (pending state). A fully authorized
	// caller is not subject to this limit. The per-call 2048-bit modexp is
	// the cost being bounded.
	GetPasswordIP store.RateLimitConfig
	// SignUpIP limits auth.signUp calls per client network. Applied only when
	// TG_REGISTRATION is invite or open; no-op in closed mode.
	SignUpIP store.RateLimitConfig
	// CheckUsernameIP limits unauthenticated account.checkUsername calls per
	// client network. The 20/hour default is four times the active sign-up call
	// budget (5/hour) to allow candidate corrections; it throttles bulk scans,
	// not targeted or distributed enumeration, and is not a privacy boundary.
	CheckUsernameIP store.RateLimitConfig
	// PasswordProof limits account.getPasswordSettings and
	// account.updatePasswordSettings (the proof-required path) per account, on
	// one shared budget. Both call consumeAndVerify against the same secret,
	// so alternating between them must not double the guess budget.
	PasswordProof store.RateLimitConfig
	// GetPassword limits account.getPassword per account for fully authorized
	// callers (r.UserID != 0 && hasPw). Provisional accounts (hasPw == false)
	// are not subject to this limit. The per-call 2048-bit modexp and SRP
	// challenge issuance are the costs being bounded.
	GetPassword store.RateLimitConfig
	// UpdateProfile limits account.updateProfile per account. Display-name
	// changes are a rare account mutation, not a chatty RPC: 20 per 24h covers
	// a typo retry at signup and an occasional later rename without letting a
	// client churn the generated name_tsv index that contacts.search uses.
	UpdateProfile store.RateLimitConfig
	// RequestEncryption limits messages.requestEncryption per account. Every
	// accepted request is a durable secret_chats row plus a push to the
	// responder, and rows are never deleted, so the 10-outstanding cap alone
	// bounds nothing: a request-then-discard loop frees the cap on each iteration
	// and keeps producing rows. This surface is what caps new rows per window.
	// The charge runs inside the insert transaction, so a denied request
	// allocates no row and no chat id.
	RequestEncryption store.RateLimitConfig
	// DiscardEncryption limits messages.discardEncryption per account. It is a
	// separate budget on purpose: sharing the request allowance would let one
	// account exhaust its own quota and then strand every chat it holds, unable
	// to clean up. 30 per hour covers a full window of the caller's own requests,
	// the 10-row outstanding cap, and room to decline inbound requests. An
	// already-discarded chat is an idempotent success and is never charged.
	DiscardEncryption store.RateLimitConfig
}

// DefaultRateLimits returns the shipped per-surface defaults: 60 sends per 60s,
// 20 chat creates per 24h, 120 member adds per 24h, 20 channel creates per 24h,
// 300 message searches per hour, 300 contacts searches per hour, 300 global
// searches per hour, 60 poll votes per 60s, 3 sticker-set creates per 60s,
// 600 upload parts per 60s, per client
// network 10 sendCode calls per hour across at most 20 distinct phone numbers
// per 24h, 10 failed
// signIn attempts per hour per client network, 5 failed checkPassword attempts
// per 10 min per account, 10 failed checkPassword attempts per hour per client
// network, 20 getPassword calls per hour per client network (unauthenticated
// callers only), 5 signUp calls and 20 unauthenticated account.checkUsername
// calls per hour per client network, 5 password proof attempts per 10 min per
// account (shared by getPasswordSettings and
// updatePasswordSettings), 20 getPassword calls per hour per account
// (authorized callers only), 120 getMessages calls per minute per account,
// 20 updateProfile calls per 24h per account, 10
// secret-chat requests per hour per account, 30 secret-chat discards per hour
// per account, 50
// upload.getFile calls per second per account, and 400 upload.getFile calls per
// second across the deployment.
// Zero disables enforcement for a surface. UploadBytes is an optional budget
// in byte tokens with a one-hour default window; deployments enable it by
// setting TG_RATE_LIMIT_UPLOAD_BYTES.
//
// The upload number is the one derived rather than chosen: at the 512 KiB
// protocol part size it is roughly 300 MB/min, past any real client's upload
// rate, and it is what bounds how often an account can drive the per-user cap
// to rollover — the write amplification each rejected save still costs.
//
// Exported so a test can drive a surface at the numbers that actually ship
// instead of at ones it invented.
func DefaultRateLimits() RateLimitsConfig {
	return RateLimitsConfig{
		MessageSend:      store.RateLimitConfig{Limit: 60, Window: 60 * time.Second},
		CreateChat:       store.RateLimitConfig{Limit: 20, Window: 24 * time.Hour},
		AddChatUser:      store.RateLimitConfig{Limit: 120, Window: 24 * time.Hour},
		CreateChannel:    store.RateLimitConfig{Limit: 20, Window: 24 * time.Hour},
		SearchMessages:   store.RateLimitConfig{Limit: 300, Window: time.Hour},
		GetMessages:      store.RateLimitConfig{Limit: 120, Window: time.Minute},
		SearchContacts:   store.RateLimitConfig{Limit: 300, Window: time.Hour},
		SearchGlobal:     store.RateLimitConfig{Limit: 300, Window: time.Hour},
		PollVote:         store.RateLimitConfig{Limit: 60, Window: 60 * time.Second},
		CreateStickerSet: store.RateLimitConfig{Limit: 3, Window: time.Minute},
		SaveFilePart:     store.RateLimitConfig{Limit: 600, Window: 60 * time.Second},
		UploadBytes:      store.RateLimitConfig{Window: time.Hour},
		GetFile:          store.RateLimitConfig{Limit: 50, Window: time.Second},
		GetFileReplica:   store.RateLimitConfig{Limit: 400, Window: time.Second},
		SendCodeIP: store.SendCodeIPLimits{
			Calls:  store.RateLimitConfig{Limit: 10, Window: time.Hour},
			Phones: store.RateLimitConfig{Limit: 20, Window: 24 * time.Hour},
		},
		SignInFailIP:    store.RateLimitConfig{Limit: 10, Window: time.Hour},
		CheckPassword:   store.RateLimitConfig{Limit: 5, Window: 10 * time.Minute},
		CheckPasswordIP: store.RateLimitConfig{Limit: 10, Window: time.Hour},
		GetPasswordIP:   store.RateLimitConfig{Limit: 20, Window: time.Hour},
		SignUpIP:        store.RateLimitConfig{Limit: 5, Window: time.Hour},
		CheckUsernameIP: store.RateLimitConfig{Limit: 20, Window: time.Hour},
		PasswordProof:   store.RateLimitConfig{Limit: 5, Window: 10 * time.Minute},
		GetPassword:     store.RateLimitConfig{Limit: 20, Window: time.Hour},
		UpdateProfile:   store.RateLimitConfig{Limit: 20, Window: 24 * time.Hour},
		// The request number is the row-production bound: 10 new secret_chats rows
		// per account per fixed window, equal to the outstanding cap, so a lone
		// account cannot grow the table faster than that per window however often
		// it discards. The discard number is cleanup headroom, not a second row
		// budget: a discard adds no rows and re-dates at most the rows the request
		// budget already allowed.
		RequestEncryption: store.RateLimitConfig{Limit: 10, Window: time.Hour},
		DiscardEncryption: store.RateLimitConfig{Limit: 30, Window: time.Hour},
	}
}

// validateRateLimitLimit rejects limits that cannot mean a bound: negative
// values disable enforcement, and values above MaxInt32 narrow when passed to
// Postgres as int4, preventing the counter row from being inserted.
func validateRateLimitLimit(limitName string, limit int) error {
	if limit < 0 {
		return fmt.Errorf("%s must not be negative; 0 disables the bound", limitName)
	}
	if limit > math.MaxInt32 {
		return fmt.Errorf("%s must not exceed math.MaxInt32", limitName)
	}
	return nil
}

// validateRateLimitSurface also ensures that a positive limit has an active
// window. Surfaces that document a non-positive window as a disable use only
// validateRateLimitLimit instead.
func validateRateLimitSurface(limitName, windowName string, cfg store.RateLimitConfig) error {
	if err := validateRateLimitLimit(limitName, cfg.Limit); err != nil {
		return err
	}
	if cfg.Window < 0 {
		return fmt.Errorf("%s must not be negative", windowName)
	}
	if cfg.Limit > 0 && cfg.Window == 0 {
		return fmt.Errorf("%s must be positive when %s is enabled", windowName, limitName)
	}
	return nil
}

// MaxFileBytesLimit is the ceiling on TG_MAX_FILE_BYTES and TG_UPLOAD_MAX_BYTES.
// It is a bound on the arithmetic, not a product decision: the derived part
// count rounds up by the part size and the per-user cap doubles the per-file
// one, so a value near MaxInt64 overflows both and turns every save into a
// rejection. 1 TiB is far past any file a client can upload and leaves both
// terms in range.
const MaxFileBytesLimit int64 = 1 << 40

// DefaultStatementTimeout is the shipped Postgres statement_timeout. It bounds
// one SQL statement, server-side, on every pooled connection — the database
// half of the per-RPC ceiling, catching a wedged statement even where no
// request context flows (a background sweep's query, for instance).
//
// The shipped number is derived from measurement rather than chosen. The
// slowest single statement any code path legitimately runs is the media-erasure
// report's reference-predicate batch: 2.2s measured at ~300k media messages and
// ~20k files, where the planner flips to one scan of messages per files row.
// The slowest full RPC the e2e suite performs — messages.sendMessage — measured
// 638 ms wall. 17s is roughly eight times the first number and 26 times the
// second — enough headroom for hardware several times slower than the host
// either was measured on — and it sits below the RPC deadline so the database
// aborts a wedged statement before the handler's own budget expires, keeping
// the failure inside the transaction that caused it. Deliberately not a round
// number: rounding would suggest it was picked rather than measured. An
// operator whose erasure corpus outgrows this ceiling will see statement
// cancellations in the sweep's log, not silent truncation.
const DefaultStatementTimeout = 17 * time.Second

// maxRPCDeadline leaves time inside the fixed shutdown drain for response and
// push writes, followed by staggered connection retirement.
const maxRPCDeadline = 45 * time.Second

// ProfileRestoreCeiling is the accepted PostgreSQL backup policy's absolute age
// ceiling: no retained dump may exceed 90 days, and that ceiling is the policy's
// maximum backup erasure lag. It is exported because every identifier-only
// profile retention bound is measured from it, so the number lives in one place
// and moves with the backup policy, not with the code that reads it.
const ProfileRestoreCeiling = 90 * 24 * time.Hour

// ProfileBackupCleanupGrace is the delayed-cleanup allowance added on top of the
// restore ceiling. The ceiling bounds a dump's age, not the moment its bytes are
// gone: the accepted rotation keeps 7 daily, 4 weekly and 3 monthly points, so a
// point that reaches the ceiling is only released by the rotation pass it belongs
// to, and the design's own verifier tolerates 30 hours without a verified
// arrival, so a pass can be missed. The weekly class is the slowest rotation
// able to be holding a point at the ceiling, so records survive one week past
// it: a gallery record is never compacted while a dump that can revive the
// deletion it acknowledges still exists.
const ProfileBackupCleanupGrace = 7 * 24 * time.Hour

// ProfileIdentifierRetentionFloor is the shortest horizon any identifier-only
// profile retention setting may be given: every restore point that can revive a
// gallery mutation, plus every copy of that point cleanup has not removed yet.
const ProfileIdentifierRetentionFloor = ProfileRestoreCeiling + ProfileBackupCleanupGrace

// LoadClientConfig is the resource-free subset used by the client-config
// command. Keep it separate from Load: a public document needs the advertised
// identity, listener scope, and validated Turnstile settings, but not Postgres,
// the auth-key master secret, blob storage, or any other service resource.
func LoadClientConfig() (ClientConfig, error) {
	cfg, err := loadClientIdentityConfig()
	if err != nil {
		return ClientConfig{}, err
	}
	turnstile, err := loadTurnstileConfig()
	if err != nil {
		return ClientConfig{}, err
	}
	cfg.TurnstileSiteKey = turnstile.SiteKey
	cfg.TurnstileEnabled = turnstile.Enabled()
	return cfg, nil
}

func loadClientIdentityConfig() (ClientConfig, error) {
	listenAddr := envOr("TG_LISTEN_ADDR", ":2443")
	cfg := ClientConfig{
		ListenAddr:                listenAddr,
		PublicWebSocketListenAddr: os.Getenv("TG_PUBLIC_WEBSOCKET_LISTEN_ADDR"),
		RSAKeyPath:                RSAKeyPath(),
		DCID:                      2,
	}
	if v := os.Getenv("TG_DC_ID"); v != "" {
		id, err := strconv.Atoi(v)
		if err != nil {
			return ClientConfig{}, errors.New("TG_DC_ID must be an integer")
		}
		cfg.DCID = id
	}
	if cfg.DCID <= 0 || int64(cfg.DCID) > math.MaxInt32 {
		return ClientConfig{}, errors.New("TG_DC_ID must be positive and fit int32")
	}
	if v := os.Getenv("TG_ADVERTISE_DEMO_GUEST_SIGN_UP"); v != "" {
		enabled, err := strconv.ParseBool(v)
		if err != nil {
			return ClientConfig{}, errors.New("TG_ADVERTISE_DEMO_GUEST_SIGN_UP must be a boolean")
		}
		cfg.AdvertiseDemoGuestSignUp = enabled
	}
	if os.Getenv("TG_ADVERTISE_ADDR") == "" {
		if _, _, err := net.SplitHostPort(listenAddr); err != nil {
			return ClientConfig{}, errors.New("TG_LISTEN_ADDR must be host:port when TG_ADVERTISE_ADDR is unset")
		}
	}
	advertiseHost, advertisePort, err := advertiseAddr(os.Getenv("TG_ADVERTISE_ADDR"), listenAddr)
	if err != nil {
		return ClientConfig{}, err
	}
	cfg.AdvertiseHost = advertiseHost
	cfg.AdvertisePort = advertisePort
	return cfg, nil
}

// RSAKeyPath returns the configured server key path without loading any other
// server settings. It is shared by the explicit identity bootstrap command.
func RSAKeyPath() string {
	return envOr("TG_RSA_KEY_PATH", "server_key.pem")
}

// ValidatePublicLinkPrefix rejects values that could make the server advertise
// or issue links on an ambiguous, official, or non-DNS origin.
func ValidatePublicLinkPrefix(prefix string) error {
	const name = "TG_PUBLIC_LINK_PREFIX"
	invalid := func() error {
		return fmt.Errorf("%s must be a lowercase ASCII HTTPS prefix with a DNS hostname and root path", name)
	}
	if prefix == "" {
		return fmt.Errorf("%s is required", name)
	}
	if !strings.HasPrefix(prefix, "https://") || !strings.HasSuffix(prefix, "/") {
		return invalid()
	}

	host := prefix[len("https://") : len(prefix)-1]
	if host == "" || len(host) > 253 || strings.ContainsAny(host, "/:?#@\\%") || strings.HasSuffix(host, ".") {
		return invalid()
	}
	for i := range len(prefix) {
		if prefix[i] > 0x7f {
			return invalid()
		}
	}

	for label := range strings.SplitSeq(host, ".") {
		if len(label) == 0 || len(label) > 63 || strings.HasPrefix(label, "xn--") || label[0] == '-' || label[len(label)-1] == '-' {
			return invalid()
		}
		for i := range len(label) {
			c := label[i]
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return invalid()
			}
		}
	}
	if isIPAddressLiteral(host) {
		return invalid()
	}
	// Keep the first hostname split so source audits do not mistake a rejected
	// origin for a generated link.
	for _, official := range [...]string{"t" + ".me", "telegram.me", "telegram.dog", "telegram.org"} {
		if host == official || strings.HasSuffix(host, "."+official) {
			return invalid()
		}
	}
	return nil
}

func isIPAddressLiteral(host string) bool {
	if address, err := netip.ParseAddr(host); err == nil && address.Is4() {
		return true
	}
	// Browsers also interpret abbreviated, octal, and hexadecimal numeric
	// hosts as IPv4 addresses, so reject those forms as well.
	parts := strings.Split(host, ".")
	if len(parts) == 0 || len(parts) > 4 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		if strings.HasPrefix(part, "0x") {
			part = part[2:]
			if part == "" {
				continue
			}
			for i := range len(part) {
				c := part[i]
				if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
					return false
				}
			}
			continue
		}
		for i := range len(part) {
			if part[i] < '0' || part[i] > '9' {
				return false
			}
		}
	}
	return true
}

// LoadServerConfig requires the public link prefix before loading configuration
// that may generate key material. Administrative commands use Load directly and
// remain independent of this server-only setting.
func LoadServerConfig(log *slog.Logger) (Config, error) {
	prefix := os.Getenv("TG_PUBLIC_LINK_PREFIX")
	if err := ValidatePublicLinkPrefix(prefix); err != nil {
		return Config{}, err
	}
	cfg, err := Load(log)
	if err != nil {
		return Config{}, err
	}
	cfg.PublicLinkPrefix = prefix
	return cfg, nil
}

func loadRecoveryEmailMode() (RecoveryEmailMode, error) {
	raw, ok := os.LookupEnv("TG_RECOVERY_EMAIL")
	if !ok {
		return RecoveryEmailOff, nil
	}
	mode := RecoveryEmailMode(raw)
	switch mode {
	case RecoveryEmailOff, RecoveryEmailContact:
		return mode, nil
	default:
		return "", errors.New("TG_RECOVERY_EMAIL must be exactly off or contact")
	}
}

// Load reads configuration from environment variables, applying defaults. The
// logger is used only when a missing auth-key master key file is generated for
// a non-replica development start, which has to be logged as such.
func Load(log *slog.Logger) (Config, error) {
	return load(log, true)
}

// LoadReadOnly reads the same configuration as Load but never creates an
// auth-key master key file. Maintenance commands that must not write can use
// it and require that key to be provisioned already.
func LoadReadOnly(log *slog.Logger) (Config, error) {
	return load(log, false)
}

func load(log *slog.Logger, allowKeyGeneration bool) (Config, error) {
	for _, name := range [...]string{
		"TG_BOOTSTRAP_USERNAME",
		"TG_BOOTSTRAP_PASSWORD",
		"TG_BOOTSTRAP_PASSWORD_FILE",
	} {
		if _, ok := os.LookupEnv(name); ok {
			return Config{}, fmt.Errorf("%s is no longer supported", name)
		}
	}
	recoveryEmailMode, err := loadRecoveryEmailMode()
	if err != nil {
		return Config{}, err
	}
	stickerImportMode, err := parseStickerImportMode(os.Getenv("TG_STICKER_IMPORT"))
	if err != nil {
		return Config{}, err
	}
	identity, err := loadClientIdentityConfig()
	if err != nil {
		return Config{}, err
	}
	publicAdvertiseHost, publicAdvertisePort, err := publicWebSocketAdvertiseAddr(
		os.Getenv("TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR"), identity.PublicWebSocketListenAddr,
	)
	if err != nil {
		return Config{}, err
	}
	originPatterns, err := parseWebSocketOriginPatterns(os.Getenv("TG_WEBSOCKET_ALLOWED_ORIGINS"), "TG_WEBSOCKET_ALLOWED_ORIGINS")
	if err != nil {
		return Config{}, err
	}
	publicOriginPatterns, err := parseWebSocketOriginPatterns(os.Getenv("TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS"), "TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS")
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		ListenAddr:                    identity.ListenAddr,
		WebSocketListenAddr:           os.Getenv("TG_WEBSOCKET_LISTEN_ADDR"),
		WebSocketOriginPatterns:       originPatterns,
		PublicWebSocketListenAddr:     identity.PublicWebSocketListenAddr,
		PublicWebSocketOriginPatterns: publicOriginPatterns,
		PublicWebSocketAdvertiseHost:  publicAdvertiseHost,
		PublicWebSocketAdvertisePort:  publicAdvertisePort,
		AdminListenAddr:               os.Getenv("TG_ADMIN_LISTEN_ADDR"),
		BotAPIListenAddr:              os.Getenv("TG_BOT_API_LISTEN_ADDR"),
		AdminOrigin:                   os.Getenv("TG_ADMIN_ORIGIN"),
		ReplicaID:                     os.Getenv("TG_REPLICA_ID"),
		ReplicaCount:                  1,
		PostgresDSN:                   os.Getenv("TG_POSTGRES_DSN"),
		PostgresPool:                  store.PoolConfig{MaxConns: 8, MinConns: 2, MaxConnIdleTime: 5 * time.Minute},
		RSAKeyPath:                    identity.RSAKeyPath,
		AdvertiseHost:                 identity.AdvertiseHost,
		AdvertisePort:                 identity.AdvertisePort,
		AdvertiseDemoGuestSignUp:      identity.AdvertiseDemoGuestSignUp,
		DCID:                          identity.DCID,
		BlobDir:                       envOr("TG_BLOB_DIR", "blobs"),

		MaxFileBytes:           100 << 20,
		MaxUserStorageBytes:    2 << 30,
		UploadPartTTL:          DefaultUploadPartTTL,
		AuthKeySweepIdleWindow: DefaultAuthKeySweepIdleWindow,
		AuthKeySweepMaxAge:     DefaultAuthKeySweepMaxAge,
		MediaErasureMinAge:     24 * time.Hour,
		BlobScanTempMinAge:     24 * time.Hour,

		MediaErasureReportInterval: 0,
		BlobScanReportInterval:     0,

		// The sweep is off and non-destructive unless an operator says
		// otherwise, which is what "ships disabled" means for a pass that
		// unlinks user bytes from a volume with no backup.
		MediaErasureIntervalMin: 0,
		MediaErasureIntervalMax: 0,
		MediaErasureDestructive: false,

		MaxConnsPerUnboundKey: mtproto.DefaultMaxConnsPerUnboundKey,
		MaxPendingLoginConns:  mtproto.DefaultMaxPendingLoginConns,
		KeyExchangeLimits:     mtproto.DefaultExchangeLimits(),

		RPCDeadline:      mtproto.DefaultRPCDeadline,
		StatementTimeout: DefaultStatementTimeout,

		// Identifier-only profile retention starts at the restore horizon. The
		// receipt bound is raised below to the widest horizon it has to clear: the
		// configured gallery horizon, or one part TTL plus one request deadline.
		ProfileGalleryRetention:         ProfileIdentifierRetentionFloor,
		ProfileReceiptRetention:         ProfileIdentifierRetentionFloor,
		ProfileDeleteOperationRetention: ProfileIdentifierRetentionFloor,

		RegistrationMode:        RegistrationClosed,
		UserInviteMinAccountAge: DefaultUserInviteMinAccountAge,
		StickerImportMode:       stickerImportMode,
		SignUpTermsOfService:    envOr("TG_TERMS_OF_SERVICE", DefaultSignUpTermsOfService),
		RecoveryEmailMode:       recoveryEmailMode,
		SessionDeviceLabels:     true,
	}
	if value := os.Getenv("TG_UPLOADS_DISABLED"); value != "" {
		disabled, err := strconv.ParseBool(value)
		if err != nil {
			return Config{}, errors.New("TG_UPLOADS_DISABLED must be a boolean")
		}
		cfg.UploadsDisabled = disabled
	}
	if bundleDir := os.Getenv("TG_STICKER_CATALOG_BUNDLE"); bundleDir != "" {
		cfg.StickerBundle, err = stickerbundle.Load(bundleDir)
		if err != nil {
			return Config{}, fmt.Errorf("TG_STICKER_CATALOG_BUNDLE: %w", err)
		}
	}
	turnstile, err := loadTurnstileConfig()
	if err != nil {
		return Config{}, err
	}
	cfg.Turnstile = turnstile
	if err := validateReplicaID(cfg.ReplicaID); err != nil {
		return Config{}, err
	}
	if raw := os.Getenv("TG_RSA_KEY_FINGERPRINT"); raw != "" {
		fingerprint, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return Config{}, errors.New("TG_RSA_KEY_FINGERPRINT must be a signed 64-bit integer")
		}
		cfg.ExpectedRSAFingerprint = &fingerprint
	}
	if cfg.ReplicaID != "" && cfg.ExpectedRSAFingerprint == nil {
		return Config{}, errors.New("TG_RSA_KEY_FINGERPRINT is required when TG_REPLICA_ID is set")
	}
	if v := os.Getenv("TG_MAX_FILE_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return Config{}, errors.New("TG_MAX_FILE_BYTES must be an integer")
		}
		if n <= 0 {
			return Config{}, errors.New("TG_MAX_FILE_BYTES must be positive")
		}
		if n > MaxFileBytesLimit {
			return Config{}, errors.New("TG_MAX_FILE_BYTES must be at most 1099511627776")
		}
		cfg.MaxFileBytes = n
	}
	if v := os.Getenv("TG_UPLOAD_MAX_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return Config{}, errors.New("TG_UPLOAD_MAX_BYTES must be an integer")
		}
		if n <= 0 {
			return Config{}, errors.New("TG_UPLOAD_MAX_BYTES must be positive")
		}
		if n > MaxFileBytesLimit {
			return Config{}, errors.New("TG_UPLOAD_MAX_BYTES must be at most 1099511627776")
		}
		cfg.MaxFileBytes = n
	}
	if v := os.Getenv("TG_MAX_USER_STORAGE_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return Config{}, errors.New("TG_MAX_USER_STORAGE_BYTES must be an integer")
		}
		if n <= 0 {
			return Config{}, errors.New("TG_MAX_USER_STORAGE_BYTES must be positive")
		}
		cfg.MaxUserStorageBytes = n
	}
	if v := os.Getenv("TG_UPLOAD_PART_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, errors.New("TG_UPLOAD_PART_TTL must be a duration")
		}
		if d <= 0 {
			return Config{}, errors.New("TG_UPLOAD_PART_TTL must be positive")
		}
		cfg.UploadPartTTL = d
	}
	if v := os.Getenv("TG_AUTH_KEY_SWEEP_IDLE_WINDOW"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, errors.New("TG_AUTH_KEY_SWEEP_IDLE_WINDOW must be a duration")
		}
		if d < 0 {
			return Config{}, errors.New("TG_AUTH_KEY_SWEEP_IDLE_WINDOW must not be negative; 0 disables the sweep")
		}
		cfg.AuthKeySweepIdleWindow = d
	}
	if v := os.Getenv("TG_AUTH_KEY_SWEEP_MAX_AGE"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, errors.New("TG_AUTH_KEY_SWEEP_MAX_AGE must be a duration")
		}
		if d < 0 {
			return Config{}, errors.New("TG_AUTH_KEY_SWEEP_MAX_AGE must not be negative; 0 disables the sweep")
		}
		cfg.AuthKeySweepMaxAge = d
	}
	if cfg.AuthKeySweepIdleWindow > 0 {
		minimumIdleWindow := mtproto.DefaultPendingLoginLifetime + mtproto.AuthKeyTouchInterval
		if cfg.AuthKeySweepIdleWindow < minimumIdleWindow {
			return Config{}, fmt.Errorf("TG_AUTH_KEY_SWEEP_IDLE_WINDOW must be at least %s", minimumIdleWindow)
		}
	}
	if cfg.AuthKeySweepIdleWindow > 0 && cfg.AuthKeySweepMaxAge > 0 && cfg.AuthKeySweepMaxAge < cfg.AuthKeySweepIdleWindow {
		return Config{}, errors.New("TG_AUTH_KEY_SWEEP_MAX_AGE must be at least TG_AUTH_KEY_SWEEP_IDLE_WINDOW")
	}
	if v := os.Getenv("TG_MEDIA_ERASURE_MIN_AGE"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, errors.New("TG_MEDIA_ERASURE_MIN_AGE must be a duration")
		}
		if d <= 0 {
			return Config{}, errors.New("TG_MEDIA_ERASURE_MIN_AGE must be positive")
		}
		cfg.MediaErasureMinAge = d
	}
	if v := os.Getenv("TG_MEDIA_ERASURE_REPORT_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, errors.New("TG_MEDIA_ERASURE_REPORT_INTERVAL must be a duration")
		}
		if d < 0 {
			return Config{}, errors.New("TG_MEDIA_ERASURE_REPORT_INTERVAL must not be negative")
		}
		cfg.MediaErasureReportInterval = d
	}
	if v := os.Getenv("TG_MEDIA_ERASURE_INTERVAL_MIN"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, errors.New("TG_MEDIA_ERASURE_INTERVAL_MIN must be a duration")
		}
		if d < 0 {
			return Config{}, errors.New("TG_MEDIA_ERASURE_INTERVAL_MIN must not be negative")
		}
		cfg.MediaErasureIntervalMin = d
	}
	if v := os.Getenv("TG_MEDIA_ERASURE_INTERVAL_MAX"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, errors.New("TG_MEDIA_ERASURE_INTERVAL_MAX must be a duration")
		}
		if d < 0 {
			return Config{}, errors.New("TG_MEDIA_ERASURE_INTERVAL_MAX must not be negative")
		}
		cfg.MediaErasureIntervalMax = d
	}
	// Refused rather than defaulted, in both directions. A minimum on its own
	// would have to fall back to a fixed tick, and the fixed tick is the thing
	// the range exists to prevent: it is what turns another account's deletion
	// into a receipt timed to the second. A maximum on its own is a variable the
	// operator believes is in effect and is not.
	if (cfg.MediaErasureIntervalMin > 0) != (cfg.MediaErasureIntervalMax > 0) {
		return Config{}, errors.New("TG_MEDIA_ERASURE_INTERVAL_MIN and TG_MEDIA_ERASURE_INTERVAL_MAX must be set together")
	}
	if cfg.MediaErasureIntervalMin > 0 && cfg.MediaErasureIntervalMax <= cfg.MediaErasureIntervalMin {
		return Config{}, errors.New("TG_MEDIA_ERASURE_INTERVAL_MAX must be greater than TG_MEDIA_ERASURE_INTERVAL_MIN")
	}
	if v := os.Getenv("TG_MEDIA_ERASURE_DESTRUCTIVE"); v != "" {
		on, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, errors.New("TG_MEDIA_ERASURE_DESTRUCTIVE must be a boolean")
		}
		cfg.MediaErasureDestructive = on
	}
	// Refused for the reason a lone interval bound is, and it is the worse of
	// the two: an operator who set this believes reclamation is running and is
	// watching for the disk to come back, while nothing is scheduled to run at
	// all. Silence there reads as "there was nothing to reclaim". Turning
	// destruction on is the one setting in this file that cannot be quietly
	// inert.
	if cfg.MediaErasureDestructive && cfg.MediaErasureIntervalMin == 0 {
		return Config{}, errors.New("TG_MEDIA_ERASURE_DESTRUCTIVE needs TG_MEDIA_ERASURE_INTERVAL_MIN and TG_MEDIA_ERASURE_INTERVAL_MAX; nothing runs the sweep without them")
	}
	if v := os.Getenv("TG_BLOB_SCAN_TEMP_MIN_AGE"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, errors.New("TG_BLOB_SCAN_TEMP_MIN_AGE must be a duration")
		}
		if d <= 0 {
			return Config{}, errors.New("TG_BLOB_SCAN_TEMP_MIN_AGE must be positive")
		}
		cfg.BlobScanTempMinAge = d
	}
	if v := os.Getenv("TG_BLOB_SCAN_REPORT_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, errors.New("TG_BLOB_SCAN_REPORT_INTERVAL must be a duration")
		}
		if d < 0 {
			return Config{}, errors.New("TG_BLOB_SCAN_REPORT_INTERVAL must not be negative")
		}
		cfg.BlobScanReportInterval = d
	}
	// SIGTERM grants admitted RPCs their ordinary deadline inside the fixed
	// shutdown drain. An unbounded or oversized RPC deadline cannot fit beside
	// response writes and staggered connection retirement, so the override is
	// deliberately positive and capped at 45 seconds.
	if v := os.Getenv("TG_RPC_DEADLINE"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, errors.New("TG_RPC_DEADLINE must be a duration")
		}
		if d <= 0 || d > maxRPCDeadline {
			return Config{}, fmt.Errorf("TG_RPC_DEADLINE must be between 1ns and %s", maxRPCDeadline)
		}
		cfg.RPCDeadline = d
	}
	for _, setting := range []struct {
		name    string
		target  *int32
		minimum int64
	}{
		{"TG_PG_MAX_CONNS", &cfg.PostgresPool.MaxConns, 2},
		{"TG_PG_MIN_CONNS", &cfg.PostgresPool.MinConns, 0},
	} {
		if v := os.Getenv(setting.name); v != "" {
			n, err := strconv.ParseInt(v, 10, 32)
			if err != nil || n < setting.minimum {
				return Config{}, fmt.Errorf("%s must be an integer between %d and %d", setting.name, setting.minimum, math.MaxInt32)
			}
			*setting.target = int32(n)
		}
	}
	if cfg.PostgresPool.MinConns > cfg.PostgresPool.MaxConns {
		return Config{}, errors.New("TG_PG_MIN_CONNS must not exceed TG_PG_MAX_CONNS")
	}
	if v := os.Getenv("TG_PG_MAX_CONN_IDLE_TIME"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return Config{}, errors.New("TG_PG_MAX_CONN_IDLE_TIME must be a positive duration")
		}
		cfg.PostgresPool.MaxConnIdleTime = d
	}
	if v := os.Getenv("TG_STATEMENT_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, errors.New("TG_STATEMENT_TIMEOUT must be a duration")
		}
		if d < 0 {
			return Config{}, errors.New("TG_STATEMENT_TIMEOUT must not be negative, and 0 disables the timeout")
		}
		cfg.StatementTimeout = d
	}
	// Identifier-only profile retention, validated against the restore horizon.
	// The receipt bound also carries the upload variables, so it is checked here,
	// after the part TTL and the RPC deadline it has to outlive are final.
	galleryRetention, err := profileRetention("TG_PROFILE_GALLERY_RETENTION", ProfileIdentifierRetentionFloor)
	if err != nil {
		return Config{}, err
	}
	cfg.ProfileGalleryRetention = galleryRetention
	deleteOpRetention, err := profileRetention("TG_PROFILE_DELETE_OP_RETENTION", ProfileIdentifierRetentionFloor)
	if err != nil {
		return Config{}, err
	}
	cfg.ProfileDeleteOperationRetention = deleteOpRetention
	receiptDefault, ok := profileReceiptRetentionDefault(cfg.ProfileGalleryRetention, cfg.UploadPartTTL, cfg.RPCDeadline)
	if !ok {
		// A part TTL so long that TTL+deadline is unrepresentable keeps the floor
		// default, which the bound below refuses by naming TG_UPLOAD_PART_TTL.
		receiptDefault = ProfileIdentifierRetentionFloor
	}
	receiptRetention, err := profileRetention("TG_PROFILE_RECEIPT_RETENTION", receiptDefault)
	if err != nil {
		return Config{}, err
	}
	cfg.ProfileReceiptRetention = receiptRetention
	if err := validateProfileReceiptRetention(cfg.ProfileReceiptRetention, cfg.UploadPartTTL, cfg.RPCDeadline, cfg.ProfileGalleryRetention); err != nil {
		return Config{}, err
	}
	if v := os.Getenv("TG_LOG_LOGIN_CODES"); v != "" {
		on, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, errors.New("TG_LOG_LOGIN_CODES must be a boolean")
		}
		cfg.LogLoginCodes = on
	}
	if v := os.Getenv("TG_LOG_AUTH_KEY_UNBINDS"); v != "" {
		on, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, errors.New("TG_LOG_AUTH_KEY_UNBINDS must be a boolean")
		}
		cfg.LogAuthKeyUnbinds = on
	}
	if v := os.Getenv("TG_SESSION_IP"); v != "" {
		on, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, errors.New("TG_SESSION_IP must be a boolean")
		}
		cfg.SessionIP = on
	}
	if v := os.Getenv("TG_SESSION_DEVICE_LABELS"); v != "" {
		on, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, errors.New("TG_SESSION_DEVICE_LABELS must be a boolean")
		}
		cfg.SessionDeviceLabels = on
	}
	// TG_REGISTRATION resolves the registration mode.
	cfg.RegistrationMode = registrationMode(os.Getenv("TG_REGISTRATION"))
	// TG_PUBLIC_REGISTRATION defaults closed independently from the tailnet
	// listeners so enabling the public endpoint cannot inherit an open policy.
	cfg.PublicRegistrationMode = registrationMode(os.Getenv("TG_PUBLIC_REGISTRATION"))
	if value := os.Getenv("TG_USER_INVITE_QUOTA"); value != "" {
		quota, err := strconv.Atoi(value)
		if err != nil || quota < 0 {
			return Config{}, errors.New("TG_USER_INVITE_QUOTA must be a non-negative integer")
		}
		cfg.UserInviteQuota = quota
	}
	if value := os.Getenv("TG_USER_INVITE_MIN_ACCOUNT_AGE"); value != "" {
		minimumAge, err := time.ParseDuration(value)
		if err != nil || minimumAge < 0 {
			return Config{}, errors.New("TG_USER_INVITE_MIN_ACCOUNT_AGE must be a non-negative duration")
		}
		cfg.UserInviteMinAccountAge = minimumAge
	}
	// TG_WELCOME_CHANNEL is resolved after account admission by the auth handler.
	cfg.WelcomeChannel = os.Getenv("TG_WELCOME_CHANNEL")
	// Zero or unset disables enforcement for a surface; the numbers and why they
	// are those numbers are on DefaultRateLimits.
	cfg.RateLimits = DefaultRateLimits()
	if v := os.Getenv("TG_RATE_LIMIT_SEND"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, errors.New("TG_RATE_LIMIT_SEND must be an integer")
		}
		cfg.RateLimits.MessageSend.Limit = n
	}
	if v := os.Getenv("TG_RATE_LIMIT_SEND_WINDOW"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, errors.New("TG_RATE_LIMIT_SEND_WINDOW must be a duration")
		}
		cfg.RateLimits.MessageSend.Window = d
	}
	if v := os.Getenv("TG_RATE_LIMIT_CREATE_CHAT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, errors.New("TG_RATE_LIMIT_CREATE_CHAT must be an integer")
		}
		cfg.RateLimits.CreateChat.Limit = n
	}
	if v := os.Getenv("TG_RATE_LIMIT_CREATE_CHAT_WINDOW"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, errors.New("TG_RATE_LIMIT_CREATE_CHAT_WINDOW must be a duration")
		}
		cfg.RateLimits.CreateChat.Window = d
	}
	if v := os.Getenv("TG_RATE_LIMIT_ADD_CHAT_USER"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, errors.New("TG_RATE_LIMIT_ADD_CHAT_USER must be an integer")
		}
		cfg.RateLimits.AddChatUser.Limit = n
	}
	if v := os.Getenv("TG_RATE_LIMIT_ADD_CHAT_USER_WINDOW"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, errors.New("TG_RATE_LIMIT_ADD_CHAT_USER_WINDOW must be a duration")
		}
		cfg.RateLimits.AddChatUser.Window = d
	}
	if v := os.Getenv("TG_RATE_LIMIT_CREATE_CHANNEL"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, errors.New("TG_RATE_LIMIT_CREATE_CHANNEL must be an integer")
		}
		cfg.RateLimits.CreateChannel.Limit = n
	}
	if v := os.Getenv("TG_RATE_LIMIT_CREATE_CHANNEL_WINDOW"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, errors.New("TG_RATE_LIMIT_CREATE_CHANNEL_WINDOW must be a duration")
		}
		cfg.RateLimits.CreateChannel.Window = d
	}
	if v := os.Getenv("TG_RATE_LIMIT_SEARCH_MESSAGES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, errors.New("TG_RATE_LIMIT_SEARCH_MESSAGES must be an integer")
		}
		cfg.RateLimits.SearchMessages.Limit = n
	}
	if v := os.Getenv("TG_RATE_LIMIT_SEARCH_MESSAGES_WINDOW"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, errors.New("TG_RATE_LIMIT_SEARCH_MESSAGES_WINDOW must be a duration")
		}
		cfg.RateLimits.SearchMessages.Window = d
	}
	if v := os.Getenv("TG_RATE_LIMIT_SEARCH_CONTACTS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, errors.New("TG_RATE_LIMIT_SEARCH_CONTACTS must be an integer")
		}
		cfg.RateLimits.SearchContacts.Limit = n
	}
	if v := os.Getenv("TG_RATE_LIMIT_SEARCH_CONTACTS_WINDOW"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, errors.New("TG_RATE_LIMIT_SEARCH_CONTACTS_WINDOW must be a duration")
		}
		cfg.RateLimits.SearchContacts.Window = d
	}
	if v := os.Getenv("TG_RATE_LIMIT_SEARCH_GLOBAL"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, errors.New("TG_RATE_LIMIT_SEARCH_GLOBAL must be an integer")
		}
		cfg.RateLimits.SearchGlobal.Limit = n
	}
	if v := os.Getenv("TG_RATE_LIMIT_SEARCH_GLOBAL_WINDOW"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, errors.New("TG_RATE_LIMIT_SEARCH_GLOBAL_WINDOW must be a duration")
		}
		cfg.RateLimits.SearchGlobal.Window = d
	}
	if v := os.Getenv("TG_RATE_LIMIT_POLL_VOTE"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, errors.New("TG_RATE_LIMIT_POLL_VOTE must be an integer")
		}
		cfg.RateLimits.PollVote.Limit = n
	}
	if v := os.Getenv("TG_RATE_LIMIT_POLL_VOTE_WINDOW"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, errors.New("TG_RATE_LIMIT_POLL_VOTE_WINDOW must be a duration")
		}
		cfg.RateLimits.PollVote.Window = d
	}
	if v := os.Getenv("TG_RATE_LIMIT_SAVE_FILE_PART"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, errors.New("TG_RATE_LIMIT_SAVE_FILE_PART must be an integer")
		}
		cfg.RateLimits.SaveFilePart.Limit = n
	}
	if v := os.Getenv("TG_RATE_LIMIT_SAVE_FILE_PART_WINDOW"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, errors.New("TG_RATE_LIMIT_SAVE_FILE_PART_WINDOW must be a duration")
		}
		cfg.RateLimits.SaveFilePart.Window = d
	}
	if v := os.Getenv("TG_RATE_LIMIT_UPLOAD_BYTES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, errors.New("TG_RATE_LIMIT_UPLOAD_BYTES must be an integer")
		}
		cfg.RateLimits.UploadBytes.Limit = n
	}
	if v := os.Getenv("TG_RATE_LIMIT_UPLOAD_BYTES_WINDOW"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, errors.New("TG_RATE_LIMIT_UPLOAD_BYTES_WINDOW must be a duration")
		}
		cfg.RateLimits.UploadBytes.Window = d
	}
	if v := os.Getenv("TG_RATE_LIMIT_GET_FILE"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, errors.New("TG_RATE_LIMIT_GET_FILE must be an integer")
		}
		if n < 0 {
			return Config{}, errors.New("TG_RATE_LIMIT_GET_FILE must not be negative; 0 disables the bound")
		}
		if n > math.MaxInt32 {
			return Config{}, errors.New("TG_RATE_LIMIT_GET_FILE must not exceed math.MaxInt32")
		}
		cfg.RateLimits.GetFile.Limit = n
	}
	if v := os.Getenv("TG_RATE_LIMIT_GET_FILE_WINDOW"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, errors.New("TG_RATE_LIMIT_GET_FILE_WINDOW must be a duration")
		}
		if d < 0 {
			return Config{}, errors.New("TG_RATE_LIMIT_GET_FILE_WINDOW must not be negative")
		}
		cfg.RateLimits.GetFile.Window = d
	}
	if v := os.Getenv("TG_RATE_LIMIT_GET_FILE_REPLICA"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, errors.New("TG_RATE_LIMIT_GET_FILE_REPLICA must be an integer")
		}
		if n < 0 {
			return Config{}, errors.New("TG_RATE_LIMIT_GET_FILE_REPLICA must not be negative; 0 disables the bound")
		}
		cfg.RateLimits.GetFileReplica.Limit = n
	}
	if v := os.Getenv("TG_RATE_LIMIT_GET_FILE_REPLICA_WINDOW"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, errors.New("TG_RATE_LIMIT_GET_FILE_REPLICA_WINDOW must be a duration")
		}
		if d < 0 {
			return Config{}, errors.New("TG_RATE_LIMIT_GET_FILE_REPLICA_WINDOW must not be negative")
		}
		cfg.RateLimits.GetFileReplica.Window = d
	}
	if err := validateRateLimitSurface("TG_RATE_LIMIT_GET_FILE", "TG_RATE_LIMIT_GET_FILE_WINDOW", cfg.RateLimits.GetFile); err != nil {
		return Config{}, err
	}
	if err := validateRateLimitSurface("TG_RATE_LIMIT_GET_FILE_REPLICA", "TG_RATE_LIMIT_GET_FILE_REPLICA_WINDOW", cfg.RateLimits.GetFileReplica); err != nil {
		return Config{}, err
	}
	if err := validateRateLimitSurface("TG_RATE_LIMIT_UPLOAD_BYTES", "TG_RATE_LIMIT_UPLOAD_BYTES_WINDOW", cfg.RateLimits.UploadBytes); err != nil {
		return Config{}, err
	}
	if v := os.Getenv("TG_RATE_LIMIT_SEND_CODE_IP"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, errors.New("TG_RATE_LIMIT_SEND_CODE_IP must be an integer")
		}
		cfg.RateLimits.SendCodeIP.Calls.Limit = n
	}
	if v := os.Getenv("TG_RATE_LIMIT_SEND_CODE_IP_WINDOW"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, errors.New("TG_RATE_LIMIT_SEND_CODE_IP_WINDOW must be a duration")
		}
		cfg.RateLimits.SendCodeIP.Calls.Window = d
	}
	if v := os.Getenv("TG_RATE_LIMIT_SEND_CODE_IP_PHONES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, errors.New("TG_RATE_LIMIT_SEND_CODE_IP_PHONES must be an integer")
		}
		cfg.RateLimits.SendCodeIP.Phones.Limit = n
	}
	if v := os.Getenv("TG_RATE_LIMIT_SEND_CODE_IP_PHONES_WINDOW"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, errors.New("TG_RATE_LIMIT_SEND_CODE_IP_PHONES_WINDOW must be a duration")
		}
		cfg.RateLimits.SendCodeIP.Phones.Window = d
	}
	// CheckPassword per-account rate limit.
	if v := os.Getenv("TG_RATE_LIMIT_CHECK_PASSWORD"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, errors.New("TG_RATE_LIMIT_CHECK_PASSWORD must be an integer")
		}
		cfg.RateLimits.CheckPassword.Limit = n
	}
	if v := os.Getenv("TG_RATE_LIMIT_CHECK_PASSWORD_WINDOW"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, errors.New("TG_RATE_LIMIT_CHECK_PASSWORD_WINDOW must be a duration")
		}
		cfg.RateLimits.CheckPassword.Window = d
	}
	// CheckPassword per-IP rate limit.
	if v := os.Getenv("TG_RATE_LIMIT_CHECK_PASSWORD_IP"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, errors.New("TG_RATE_LIMIT_CHECK_PASSWORD_IP must be an integer")
		}
		cfg.RateLimits.CheckPasswordIP.Limit = n
	}
	if v := os.Getenv("TG_RATE_LIMIT_CHECK_PASSWORD_IP_WINDOW"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, errors.New("TG_RATE_LIMIT_CHECK_PASSWORD_IP_WINDOW must be a duration")
		}
		cfg.RateLimits.CheckPasswordIP.Window = d
	}
	// GetPassword per-IP rate limit (unauthenticated callers only).
	if v := os.Getenv("TG_RATE_LIMIT_GET_PASSWORD_IP"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, errors.New("TG_RATE_LIMIT_GET_PASSWORD_IP must be an integer")
		}
		cfg.RateLimits.GetPasswordIP.Limit = n
	}
	if v := os.Getenv("TG_RATE_LIMIT_GET_PASSWORD_IP_WINDOW"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, errors.New("TG_RATE_LIMIT_GET_PASSWORD_IP_WINDOW must be a duration")
		}
		cfg.RateLimits.GetPasswordIP.Window = d
	}
	// SignUp per-IP rate limit (open mode only).
	if v := os.Getenv("TG_RATE_LIMIT_SIGN_UP_IP"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, errors.New("TG_RATE_LIMIT_SIGN_UP_IP must be an integer")
		}
		cfg.RateLimits.SignUpIP.Limit = n
	}
	if v := os.Getenv("TG_RATE_LIMIT_SIGN_UP_IP_WINDOW"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, errors.New("TG_RATE_LIMIT_SIGN_UP_IP_WINDOW must be a duration")
		}
		cfg.RateLimits.SignUpIP.Window = d
	}
	// Unauthenticated account.checkUsername calls (username enumeration).
	if v := os.Getenv("TG_RATE_LIMIT_CHECK_USERNAME_IP"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, errors.New("TG_RATE_LIMIT_CHECK_USERNAME_IP must be an integer")
		}
		cfg.RateLimits.CheckUsernameIP.Limit = n
	}
	if v := os.Getenv("TG_RATE_LIMIT_CHECK_USERNAME_IP_WINDOW"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, errors.New("TG_RATE_LIMIT_CHECK_USERNAME_IP_WINDOW must be a duration")
		}
		cfg.RateLimits.CheckUsernameIP.Window = d
	}
	if err := validateRateLimitLimit("TG_RATE_LIMIT_CHECK_USERNAME_IP", cfg.RateLimits.CheckUsernameIP.Limit); err != nil {
		return Config{}, err
	}
	// PasswordProof per-account rate limit (shared by getPasswordSettings and
	// updatePasswordSettings proof path).
	if v := os.Getenv("TG_RATE_LIMIT_PASSWORD_PROOF"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, errors.New("TG_RATE_LIMIT_PASSWORD_PROOF must be an integer")
		}
		cfg.RateLimits.PasswordProof.Limit = n
	}
	if v := os.Getenv("TG_RATE_LIMIT_PASSWORD_PROOF_WINDOW"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, errors.New("TG_RATE_LIMIT_PASSWORD_PROOF_WINDOW must be a duration")
		}
		cfg.RateLimits.PasswordProof.Window = d
	}
	// GetPassword per-account rate limit (authorized callers only).
	if v := os.Getenv("TG_RATE_LIMIT_GET_PASSWORD"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, errors.New("TG_RATE_LIMIT_GET_PASSWORD must be an integer")
		}
		cfg.RateLimits.GetPassword.Limit = n
	}
	if v := os.Getenv("TG_RATE_LIMIT_GET_PASSWORD_WINDOW"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, errors.New("TG_RATE_LIMIT_GET_PASSWORD_WINDOW must be a duration")
		}
		cfg.RateLimits.GetPassword.Window = d
	}
	if v := os.Getenv("TG_RATE_LIMIT_UPDATE_PROFILE"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, errors.New("TG_RATE_LIMIT_UPDATE_PROFILE must be an integer")
		}
		cfg.RateLimits.UpdateProfile.Limit = n
	}
	if v := os.Getenv("TG_RATE_LIMIT_UPDATE_PROFILE_WINDOW"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, errors.New("TG_RATE_LIMIT_UPDATE_PROFILE_WINDOW must be a duration")
		}
		cfg.RateLimits.UpdateProfile.Window = d
	}
	if v := os.Getenv("TG_RATE_LIMIT_CREATE_STICKER_SET"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, errors.New("TG_RATE_LIMIT_CREATE_STICKER_SET must be an integer")
		}
		cfg.RateLimits.CreateStickerSet.Limit = n
	}
	if v := os.Getenv("TG_RATE_LIMIT_CREATE_STICKER_SET_WINDOW"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, errors.New("TG_RATE_LIMIT_CREATE_STICKER_SET_WINDOW must be a duration")
		}
		cfg.RateLimits.CreateStickerSet.Window = d
	}
	if err := validateRateLimitSurface("TG_RATE_LIMIT_CREATE_STICKER_SET", "TG_RATE_LIMIT_CREATE_STICKER_SET_WINDOW", cfg.RateLimits.CreateStickerSet); err != nil {
		return Config{}, err
	}
	if v := os.Getenv("TG_RATE_LIMIT_REQUEST_ENCRYPTION"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, errors.New("TG_RATE_LIMIT_REQUEST_ENCRYPTION must be an integer")
		}
		cfg.RateLimits.RequestEncryption.Limit = n
	}
	if v := os.Getenv("TG_RATE_LIMIT_REQUEST_ENCRYPTION_WINDOW"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, errors.New("TG_RATE_LIMIT_REQUEST_ENCRYPTION_WINDOW must be a duration")
		}
		cfg.RateLimits.RequestEncryption.Window = d
	}
	if v := os.Getenv("TG_RATE_LIMIT_DISCARD_ENCRYPTION"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, errors.New("TG_RATE_LIMIT_DISCARD_ENCRYPTION must be an integer")
		}
		cfg.RateLimits.DiscardEncryption.Limit = n
	}
	if v := os.Getenv("TG_RATE_LIMIT_DISCARD_ENCRYPTION_WINDOW"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, errors.New("TG_RATE_LIMIT_DISCARD_ENCRYPTION_WINDOW must be a duration")
		}
		cfg.RateLimits.DiscardEncryption.Window = d
	}
	// Both lifecycle budgets carry a shipped bound, so a negative limit or a
	// zero window with a positive limit is a misconfiguration, not a disable.
	if err := validateRateLimitSurface("TG_RATE_LIMIT_REQUEST_ENCRYPTION", "TG_RATE_LIMIT_REQUEST_ENCRYPTION_WINDOW", cfg.RateLimits.RequestEncryption); err != nil {
		return Config{}, err
	}
	if err := validateRateLimitSurface("TG_RATE_LIMIT_DISCARD_ENCRYPTION", "TG_RATE_LIMIT_DISCARD_ENCRYPTION_WINDOW", cfg.RateLimits.DiscardEncryption); err != nil {
		return Config{}, err
	}
	preAuth, err := preAuthLimits()
	if err != nil {
		return Config{}, err
	}
	cfg.PreAuth = preAuth
	keyExchange, err := keyExchangeLimits()
	if err != nil {
		return Config{}, err
	}
	cfg.KeyExchangeLimits = keyExchange
	discoveryLimits, err := discoveryLimits()
	if err != nil {
		return Config{}, err
	}
	cfg.DiscoveryLimits = discoveryLimits
	if v := os.Getenv("TG_MAX_CONNS_PER_UNBOUND_KEY"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, errors.New("TG_MAX_CONNS_PER_UNBOUND_KEY must be an integer")
		}
		if n < 0 {
			return Config{}, errors.New("TG_MAX_CONNS_PER_UNBOUND_KEY must not be negative; 0 disables the cap")
		}
		cfg.MaxConnsPerUnboundKey = n
	}
	if v := os.Getenv("TG_MAX_PENDING_LOGIN_CONNS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, errors.New("TG_MAX_PENDING_LOGIN_CONNS must be an integer")
		}
		if n < 0 {
			return Config{}, errors.New("TG_MAX_PENDING_LOGIN_CONNS must not be negative; 0 disables the cap")
		}
		cfg.MaxPendingLoginConns = n
	}
	if v := os.Getenv("TG_REPLICA_COUNT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, errors.New("TG_REPLICA_COUNT must be an integer")
		}
		if n < 1 {
			return Config{}, errors.New("TG_REPLICA_COUNT must be at least 1")
		}
		cfg.ReplicaCount = n
	}
	trust, err := clientAddrTrust(os.Getenv("TG_CLIENT_ADDR_TRUST"))
	if err != nil {
		return Config{}, err
	}
	cfg.ClientAddrTrust = trust
	proxies, err := clientAddrProxies(os.Getenv("TG_CLIENT_ADDR_PROXY_CIDRS"), trust)
	if err != nil {
		return Config{}, err
	}
	cfg.ClientAddrProxies = proxies
	// Admin server requires both env vars or neither: a listener without auth
	// is a denial-of-service vector, and a hash with no listener is wasted work.
	adminErr := validateAdmin(&cfg)
	if adminErr != nil {
		return Config{}, adminErr
	}
	if err := validateBotAPI(&cfg); err != nil {
		return Config{}, err
	}
	cfg.AdminTokenHash = os.Getenv("TG_ADMIN_TOKEN_HASH")
	if blobS3Configured() {
		blobS3, err := loadBlobS3Config()
		if err != nil {
			return Config{}, err
		}
		cfg.BlobS3 = blobS3
	}

	if cfg.PostgresDSN == "" {
		return Config{}, errors.New("TG_POSTGRES_DSN is required")
	}
	encKey, err := loadEncKey(log, allowKeyGeneration && cfg.ReplicaID == "" && cfg.ExpectedRSAFingerprint == nil)
	if err != nil {
		return Config{}, err
	}
	cfg.AuthKeyEncKey = encKey
	if mediaKeysConfigured() {
		mediaKeys, err := loadMediaKeys(encKey)
		if err != nil {
			return Config{}, err
		}
		cfg.MediaKeys = mediaKeys
	}
	return cfg, nil
}

var blobS3ConfigEnvNames = [...]string{
	"TG_BLOB_S3_ENDPOINT",
	"TG_BLOB_S3_BUCKET",
	"TG_BLOB_S3_PREFIX",
	"TG_BLOB_S3_REGION",
	"TG_BLOB_S3_ACCESS_KEY_ID",
	"TG_BLOB_S3_SECRET_ACCESS_KEY",
	"TG_BLOB_S3_SECRET_ACCESS_KEY_FILE",
	"TG_BLOB_S3_CA_PATH",
	"TG_BLOB_S3_ALLOW_INSECURE_HTTP",
}

func loadBlobS3Config() (*blob.S3Config, error) {
	endpoint, err := requiredBlobS3Value("TG_BLOB_S3_ENDPOINT")
	if err != nil {
		return nil, err
	}
	bucket, err := requiredBlobS3Value("TG_BLOB_S3_BUCKET")
	if err != nil {
		return nil, err
	}
	prefix, err := requiredBlobS3Value("TG_BLOB_S3_PREFIX")
	if err != nil {
		return nil, err
	}
	accessKey, err := requiredBlobS3Value("TG_BLOB_S3_ACCESS_KEY_ID")
	if err != nil {
		return nil, err
	}
	secret, err := loadBlobS3Secret()
	if err != nil {
		return nil, err
	}
	allowInsecureHTTP := false
	if raw := os.Getenv("TG_BLOB_S3_ALLOW_INSECURE_HTTP"); raw != "" {
		allowInsecureHTTP, err = strconv.ParseBool(raw)
		if err != nil {
			return nil, errors.New("TG_BLOB_S3_ALLOW_INSECURE_HTTP must be a boolean")
		}
	}
	return &blob.S3Config{
		Endpoint:          endpoint,
		Bucket:            bucket,
		Prefix:            prefix,
		Region:            envOr("TG_BLOB_S3_REGION", "us-east-1"),
		AccessKeyID:       accessKey,
		SecretAccessKey:   secret,
		CAPath:            os.Getenv("TG_BLOB_S3_CA_PATH"),
		AllowInsecureHTTP: allowInsecureHTTP,
	}, nil
}

// profileRetention reads one identifier-only retention horizon. A missing value
// takes the documented default; a value under the restore horizon is refused
// rather than rounded up, because the floor is what keeps an acknowledged
// deletion acknowledged across the last restore point that can revive it. A
// value that is not a duration fails by name for the same reason a typo in a
// rate limit does: the operator believes they set a horizon, and a default
// silently in their place is a horizon they did not choose.
func profileRetention(env string, def time.Duration) (time.Duration, error) {
	raw := os.Getenv(env)
	if raw == "" {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration", env)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s must be positive", env)
	}
	if d < ProfileIdentifierRetentionFloor {
		return 0, fmt.Errorf("%s must cover every resurrectable restore point: %s is under the %s floor of a %s backup age ceiling plus %s of delayed backup cleanup",
			env, d, ProfileIdentifierRetentionFloor, ProfileRestoreCeiling, ProfileBackupCleanupGrace)
	}
	return d, nil
}

// profileReceiptRetentionDefault is the smallest receipt bound that clears both
// of its horizons: the configured gallery horizon, and the retry horizon of one
// part TTL plus one request deadline. It reports false when the retry horizon is
// unrepresentable, which is the configuration no receipt can satisfy; the
// floor default then fails by naming TG_UPLOAD_PART_TTL, the variable that has
// to come down.
func profileReceiptRetentionDefault(gallery, partTTL, deadline time.Duration) (time.Duration, bool) {
	if partTTL > time.Duration(math.MaxInt64)-deadline {
		return 0, false
	}
	retry := partTTL + deadline
	switch {
	case gallery > retry:
		return gallery, true
	case retry > ProfileIdentifierRetentionFloor:
		return retry, true
	default:
		return ProfileIdentifierRetentionFloor, true
	}
}

// validateProfileReceiptRetention enforces the two horizons a terminal upload
// receipt has to clear, and names the variable the operator has to change.
//
// The part TTL is checked first, and by its own name, because a TTL the receipt
// cannot outlive is the misconfiguration: the receipt is the thing that has to
// grow only up to the TTL, and past that the TTL is what has to shrink.
//
// The remaining bounds compare by subtraction. The plain sum `partTTL+deadline`
// is what looks natural and is what must not be written: with a part TTL near
// the duration ceiling it overflows int64 nanoseconds and reads as a number no
// receipt bound can exceed, so the configuration the bound exists to refuse
// starts. The sum is only ever formed where it is checked for
// representability, in profileReceiptRetentionDefault.
func validateProfileReceiptRetention(receipt, partTTL, deadline, gallery time.Duration) error {
	if receipt <= partTTL {
		return fmt.Errorf("TG_UPLOAD_PART_TTL is not covered by TG_PROFILE_RECEIPT_RETENTION: a terminal upload receipt must outlive the longest retry one client file id can still make, and %s does not outlive a %s part TTL", receipt, partTTL)
	}
	if receipt-partTTL < deadline {
		return fmt.Errorf("TG_PROFILE_RECEIPT_RETENTION must cover TG_UPLOAD_PART_TTL plus TG_RPC_DEADLINE: %s leaves %s past the part TTL, and the request deadline is %s", receipt, receipt-partTTL, deadline)
	}
	if receipt < gallery {
		return fmt.Errorf("TG_PROFILE_RECEIPT_RETENTION must cover TG_PROFILE_GALLERY_RETENTION: a terminal upload receipt has to outlive the gallery record for the same client file id, and %s is under %s", receipt, gallery)
	}
	return nil
}

// LoadBlobS3Config loads only the object-store settings. Maintenance commands
// that operate on the blob backend must not need the server's database,
// identity, or auth-key encryption configuration.
func LoadBlobS3Config() (*blob.S3Config, error) {
	if !blobS3Configured() {
		return nil, errors.New("TG_BLOB_S3_* settings are required")
	}
	return loadBlobS3Config()
}

func blobS3Configured() bool {
	for _, name := range blobS3ConfigEnvNames {
		if os.Getenv(name) != "" {
			return true
		}
	}
	return false
}

func requiredBlobS3Value(name string) (string, error) {
	value := os.Getenv(name)
	if value == "" {
		return "", fmt.Errorf("%s is required when object-store configuration is enabled", name)
	}
	return value, nil
}

func loadBlobS3Secret() (string, error) {
	secret := os.Getenv("TG_BLOB_S3_SECRET_ACCESS_KEY")
	secretFile := os.Getenv("TG_BLOB_S3_SECRET_ACCESS_KEY_FILE")
	if secret != "" && secretFile != "" {
		return "", errors.New("TG_BLOB_S3_SECRET_ACCESS_KEY and TG_BLOB_S3_SECRET_ACCESS_KEY_FILE are both set: use only one")
	}
	if secret != "" {
		return secret, nil
	}
	if secretFile == "" {
		return "", errors.New("TG_BLOB_S3_SECRET_ACCESS_KEY or TG_BLOB_S3_SECRET_ACCESS_KEY_FILE is required when object-store configuration is enabled")
	}
	data, err := os.ReadFile(secretFile) // #nosec G304,G703 -- operator-configured secret path.
	if err != nil {
		return "", fmt.Errorf("TG_BLOB_S3_SECRET_ACCESS_KEY_FILE: read secret file: %w", err)
	}
	secret = strings.TrimSpace(string(data))
	clear(data)
	if secret == "" {
		return "", errors.New("TG_BLOB_S3_SECRET_ACCESS_KEY_FILE is empty")
	}
	return secret, nil
}

var turnstileKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)
var turnstileQABypassTokenPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{32,256}$`)
var turnstileTestSiteKeys = map[string]struct{}{
	"1x00000000000000000000AA": {},
	"2x00000000000000000000AB": {},
}
var turnstileTestSecrets = map[string]struct{}{
	"1x0000000000000000000000000000000AA": {},
	"2x0000000000000000000000000000000AA": {},
	"3x0000000000000000000000000000000AA": {},
}

const defaultTurnstileHostnames = "web.teagram.io"

func loadTurnstileConfig() (TurnstileConfig, error) {
	secret := os.Getenv("TG_TURNSTILE_SECRET_KEY")
	secretFile := os.Getenv("TG_TURNSTILE_SECRET_KEY_FILE")
	siteKey := strings.TrimSpace(os.Getenv("TG_TURNSTILE_SITE_KEY"))
	qaBypassPathRaw := os.Getenv("TG_TURNSTILE_QA_BYPASS_TOKEN_FILE")
	qaBypassPath := strings.TrimSpace(qaBypassPathRaw)
	signInEnabled := false
	testMode := false
	if raw := os.Getenv("TG_TURNSTILE_SIGNIN_ENABLED"); raw != "" {
		var err error
		signInEnabled, err = strconv.ParseBool(raw)
		if err != nil {
			return TurnstileConfig{}, errors.New("TG_TURNSTILE_SIGNIN_ENABLED must be a boolean")
		}
	}
	if raw := os.Getenv("TG_TURNSTILE_TEST_MODE"); raw != "" {
		var err error
		testMode, err = strconv.ParseBool(raw)
		if err != nil {
			return TurnstileConfig{}, errors.New("TG_TURNSTILE_TEST_MODE must be a boolean")
		}
	}
	hostnames, err := loadTurnstileHostnames()
	if err != nil {
		return TurnstileConfig{}, err
	}
	if testMode && hasPublicTeagramHostname(false) {
		return TurnstileConfig{}, errors.New("TG_TURNSTILE_TEST_MODE must not be enabled on public teagram.io hostnames")
	}
	if qaBypassPathRaw != "" && qaBypassPath == "" {
		return TurnstileConfig{}, errors.New("TG_TURNSTILE_QA_BYPASS_TOKEN_FILE must be a non-empty path")
	}
	if qaBypassPath != "" && (hasPublicTeagramHostname(true) || !hasDemoTeagramHostname()) {
		return TurnstileConfig{}, errors.New("TG_TURNSTILE_QA_BYPASS_TOKEN_FILE is allowed only on the demo.teagram.io stack")
	}
	if secret != "" && secretFile != "" {
		return TurnstileConfig{}, errors.New("TG_TURNSTILE_SECRET_KEY and TG_TURNSTILE_SECRET_KEY_FILE are both set: use only one")
	}
	if secret == "" && secretFile == "" {
		if qaBypassPath != "" {
			return TurnstileConfig{}, errors.New("TG_TURNSTILE_QA_BYPASS_TOKEN_FILE requires Siteverify credentials")
		}
		if signInEnabled {
			return TurnstileConfig{}, errors.New("TG_TURNSTILE_SIGNIN_ENABLED requires valid TG_TURNSTILE_SITE_KEY and Siteverify credentials")
		}
		if siteKey == "" {
			return TurnstileConfig{SignInEnabled: signInEnabled, Hostnames: hostnames, TestMode: testMode}, nil
		}
		return TurnstileConfig{}, errors.New("TG_TURNSTILE_SECRET_KEY or TG_TURNSTILE_SECRET_KEY_FILE is required when TG_TURNSTILE_SITE_KEY is set")
	}
	if siteKey == "" || !turnstileKeyPattern.MatchString(siteKey) {
		return TurnstileConfig{}, errors.New("TG_TURNSTILE_SITE_KEY is required and must contain only letters, digits, '_' or '-' (maximum 256 characters)")
	}

	if secretFile != "" {
		data, err := os.ReadFile(secretFile) // #nosec G304,G703 -- operator-configured secret path.
		if err != nil {
			return TurnstileConfig{}, fmt.Errorf("TG_TURNSTILE_SECRET_KEY_FILE: read secret file: %w", err)
		}
		secret = strings.TrimSpace(string(data))
		clear(data)
	} else {
		secret = strings.TrimSpace(secret)
	}
	if secret == "" || !turnstileKeyPattern.MatchString(secret) {
		return TurnstileConfig{}, errors.New("TG_TURNSTILE_SECRET_KEY must contain only letters, digits, '_' or '-' (maximum 256 characters)")
	}
	_, testSiteKey := turnstileTestSiteKeys[siteKey]
	_, testSecret := turnstileTestSecrets[secret]
	if testMode {
		if !testSiteKey || !testSecret {
			return TurnstileConfig{}, errors.New("TG_TURNSTILE_TEST_MODE requires Cloudflare test site and secret keys; use TG_TURNSTILE_SITE_KEY and TG_TURNSTILE_SECRET_KEY")
		}
	} else if testSiteKey || testSecret {
		return TurnstileConfig{}, errors.New("cloudflare test keys require TG_TURNSTILE_TEST_MODE=true; check TG_TURNSTILE_SITE_KEY and TG_TURNSTILE_SECRET_KEY")
	}
	qaBypassToken := ""
	if qaBypassPath != "" {
		data, err := os.ReadFile(qaBypassPath) // #nosec G304,G703 -- operator-configured QA secret path.
		if err != nil {
			return TurnstileConfig{}, fmt.Errorf("TG_TURNSTILE_QA_BYPASS_TOKEN_FILE: read token file: %w", err)
		}
		qaBypassToken = strings.TrimSpace(string(data))
		clear(data)
		if !turnstileQABypassTokenPattern.MatchString(qaBypassToken) {
			return TurnstileConfig{}, errors.New("TG_TURNSTILE_QA_BYPASS_TOKEN_FILE must contain 32 to 256 letters, digits, '_' or '-'")
		}
		if qaBypassToken == secret {
			return TurnstileConfig{}, errors.New("TG_TURNSTILE_QA_BYPASS_TOKEN_FILE must not reuse the Siteverify secret")
		}
	}
	return TurnstileConfig{
		SiteKey:       siteKey,
		SecretKey:     secret,
		SignInEnabled: signInEnabled,
		Hostnames:     hostnames,
		TestMode:      testMode,
		QABypassToken: qaBypassToken,
	}, nil
}

func loadTurnstileHostnames() ([]string, error) {
	raw := strings.TrimSpace(os.Getenv("TG_TURNSTILE_HOSTNAMES"))
	if raw == "" {
		raw = defaultTurnstileHostnames
	}
	seen := make(map[string]struct{})
	hostnames := make([]string, 0)
	for part := range strings.SplitSeq(raw, ",") {
		hostname := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(part), "."))
		if !validTurnstileHostname(hostname) {
			return nil, errors.New("TG_TURNSTILE_HOSTNAMES must be a comma-separated list of DNS hostnames")
		}
		if _, exists := seen[hostname]; exists {
			continue
		}
		seen[hostname] = struct{}{}
		hostnames = append(hostnames, hostname)
	}
	return hostnames, nil
}

func validTurnstileHostname(hostname string) bool {
	if len(hostname) == 0 || len(hostname) > 253 {
		return false
	}
	for label := range strings.SplitSeq(hostname, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, character := range label {
			isLowercaseLetter := character >= 'a' && character <= 'z'
			isDigit := character >= '0' && character <= '9'
			if !isLowercaseLetter && !isDigit && character != '-' {
				return false
			}
		}
	}
	return true
}

func hasPublicTeagramHostname(productionOnly bool) bool {
	for _, hostname := range effectivePublicTeagramHostnames() {
		if hostname == "" || !strings.HasSuffix(hostname, ".teagram.io") && hostname != "teagram.io" {
			continue
		}
		if !productionOnly || hostname != "demo.teagram.io" {
			return true
		}
	}
	return false
}

func hasDemoTeagramHostname() bool {
	return slices.Contains(effectivePublicTeagramHostnames(), "demo.teagram.io")
}

func effectivePublicTeagramHostnames() []string {
	advertiseAddr := os.Getenv("TG_ADVERTISE_ADDR")
	if advertiseAddr == "" {
		advertiseAddr = os.Getenv("TG_LISTEN_ADDR")
	}
	values := []string{
		advertiseAddr,
		os.Getenv("TG_PUBLIC_LINK_PREFIX"),
		os.Getenv("TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR"),
	}
	hostnames := make([]string, 0, len(values))
	for _, value := range values {
		hostnames = append(hostnames, configuredHostname(value))
	}
	return hostnames
}

func configuredHostname(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if parsed, err := url.Parse(value); err == nil && parsed.Hostname() != "" {
		return strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	}
	if hostname, _, err := net.SplitHostPort(value); err == nil {
		return strings.ToLower(strings.TrimSuffix(hostname, "."))
	}
	return strings.ToLower(strings.TrimSuffix(strings.Trim(value, "[]"), "."))
}

// preAuthLimits resolves the bounds on what an unauthenticated connection may
// hold, starting from the shipped defaults.
//
// Zero is accepted and means the bound is off, matching every rate-limit
// surface: an operator taking one out has to be able to say so. Negative is not
// the same statement — it is a typo or a unit mistake — and a bound that silently
// became "off" because of one is the failure this whole surface exists to
// prevent, so it fails the start by name.
func preAuthLimits() (mtproto.PreAuthLimits, error) {
	limits := mtproto.DefaultPreAuthLimits()
	if v := os.Getenv("TG_MAX_PREAUTH_CONNS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return mtproto.PreAuthLimits{}, errors.New("TG_MAX_PREAUTH_CONNS must be an integer")
		}
		if n < 0 {
			return mtproto.PreAuthLimits{}, errors.New("TG_MAX_PREAUTH_CONNS must not be negative; 0 disables the cap")
		}
		limits.MaxConns = n
	}
	if v := os.Getenv("TG_MAX_PREAUTH_CONNS_PER_IP"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return mtproto.PreAuthLimits{}, errors.New("TG_MAX_PREAUTH_CONNS_PER_IP must be an integer")
		}
		if n < 0 {
			return mtproto.PreAuthLimits{}, errors.New("TG_MAX_PREAUTH_CONNS_PER_IP must not be negative; 0 disables the cap")
		}
		limits.MaxConnsPerNet = n
	}
	if v := os.Getenv("TG_PREAUTH_LIFETIME"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return mtproto.PreAuthLimits{}, errors.New("TG_PREAUTH_LIFETIME must be a duration")
		}
		if d < 0 {
			return mtproto.PreAuthLimits{}, errors.New("TG_PREAUTH_LIFETIME must not be negative; 0 disables the ceiling")
		}
		limits.Lifetime = d
	}
	return limits, nil
}

// keyExchangeLimits resolves the three anonymous key-exchange budgets. Zero
// turns a layer off; a negative value is a startup error so a typo cannot
// silently remove the bound.
func keyExchangeLimits() (mtproto.ExchangeLimits, error) {
	limits := mtproto.DefaultExchangeLimits()
	for _, setting := range []struct {
		name   string
		target *int
	}{
		{name: "TG_KEY_EXCHANGE_PER_CONNECTION", target: &limits.PerConnection},
		{name: "TG_RATE_LIMIT_KEY_EXCHANGE_IP", target: &limits.PerNetwork.Limit},
		{name: "TG_KEY_EXCHANGE_GLOBAL_RATE", target: &limits.GlobalRate},
		{name: "TG_KEY_EXCHANGE_GLOBAL_BURST", target: &limits.GlobalBurst},
	} {
		if value := os.Getenv(setting.name); value != "" {
			n, err := strconv.Atoi(value)
			if err != nil {
				return mtproto.ExchangeLimits{}, fmt.Errorf("%s must be an integer", setting.name)
			}
			if n < 0 {
				return mtproto.ExchangeLimits{}, fmt.Errorf("%s must not be negative; 0 disables the limit", setting.name)
			}
			*setting.target = n
		}
	}
	if value := os.Getenv("TG_RATE_LIMIT_KEY_EXCHANGE_IP"); value != "" {
		limits.TailnetPerNetwork.Limit = limits.PerNetwork.Limit
	}
	if value := os.Getenv("TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil {
			return mtproto.ExchangeLimits{}, errors.New("TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET must be an integer")
		}
		if n < 0 {
			return mtproto.ExchangeLimits{}, errors.New("TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET must not be negative; 0 disables the limit")
		}
		limits.TailnetPerNetwork.Limit = n
	}
	if value := os.Getenv("TG_RATE_LIMIT_KEY_EXCHANGE_IP_WINDOW"); value != "" {
		window, err := time.ParseDuration(value)
		if err != nil {
			return mtproto.ExchangeLimits{}, errors.New("TG_RATE_LIMIT_KEY_EXCHANGE_IP_WINDOW must be a duration")
		}
		if window < 0 {
			return mtproto.ExchangeLimits{}, errors.New("TG_RATE_LIMIT_KEY_EXCHANGE_IP_WINDOW must not be negative; 0 disables the network limit")
		}
		limits.PerNetwork.Window = window
		limits.TailnetPerNetwork.Window = window
	}
	return limits, nil
}

// discoveryLimits resolves the fixed-window bounds for valid local-direct
// preflight requests. The TG_RATE_LIMIT_* spelling follows the other RPC
// surfaces; the TG_DISCOVERY_* aliases keep the setting discoverable for
// deployments that group discovery controls together.
func discoveryLimits() (mtproto.DiscoveryLimits, error) {
	limits := mtproto.DefaultDiscoveryLimits()
	global, globalName, err := discoveryEnvValue("TG_RATE_LIMIT_DISCOVERY", "TG_DISCOVERY_RATE_LIMIT")
	if err != nil {
		return mtproto.DiscoveryLimits{}, err
	}
	if global != "" {
		n, err := strconv.Atoi(global)
		if err != nil {
			return mtproto.DiscoveryLimits{}, fmt.Errorf("%s must be an integer", globalName)
		}
		if n < 0 {
			return mtproto.DiscoveryLimits{}, fmt.Errorf("%s must not be negative; 0 disables the cap", globalName)
		}
		limits.MaxRequests = n
	}
	globalWindow, globalWindowName, err := discoveryEnvValue("TG_RATE_LIMIT_DISCOVERY_WINDOW", "TG_DISCOVERY_RATE_LIMIT_WINDOW")
	if err != nil {
		return mtproto.DiscoveryLimits{}, err
	}
	if globalWindow != "" {
		d, err := time.ParseDuration(globalWindow)
		if err != nil {
			return mtproto.DiscoveryLimits{}, fmt.Errorf("%s must be a duration", globalWindowName)
		}
		if d < 0 {
			return mtproto.DiscoveryLimits{}, fmt.Errorf("%s must not be negative", globalWindowName)
		}
		limits.Window = d
	}
	perNet, perNetName, err := discoveryEnvValue("TG_RATE_LIMIT_DISCOVERY_IP", "TG_DISCOVERY_RATE_LIMIT_PER_IP")
	if err != nil {
		return mtproto.DiscoveryLimits{}, err
	}
	if perNet != "" {
		n, err := strconv.Atoi(perNet)
		if err != nil {
			return mtproto.DiscoveryLimits{}, fmt.Errorf("%s must be an integer", perNetName)
		}
		if n < 0 {
			return mtproto.DiscoveryLimits{}, fmt.Errorf("%s must not be negative; 0 disables the cap", perNetName)
		}
		limits.MaxRequestsPerNet = n
	}
	perNetWindow, perNetWindowName, err := discoveryEnvValue("TG_RATE_LIMIT_DISCOVERY_IP_WINDOW", "TG_DISCOVERY_RATE_LIMIT_PER_IP_WINDOW")
	if err != nil {
		return mtproto.DiscoveryLimits{}, err
	}
	if perNetWindow != "" {
		d, err := time.ParseDuration(perNetWindow)
		if err != nil {
			return mtproto.DiscoveryLimits{}, fmt.Errorf("%s must be a duration", perNetWindowName)
		}
		if d < 0 {
			return mtproto.DiscoveryLimits{}, fmt.Errorf("%s must not be negative", perNetWindowName)
		}
		limits.PerNetWindow = d
	}
	if limits.MaxRequests > 0 && limits.Window <= 0 {
		return mtproto.DiscoveryLimits{}, errors.New("TG_RATE_LIMIT_DISCOVERY_WINDOW must be positive when TG_RATE_LIMIT_DISCOVERY is enabled")
	}
	if limits.MaxRequestsPerNet > 0 && limits.PerNetWindow <= 0 {
		return mtproto.DiscoveryLimits{}, errors.New("TG_RATE_LIMIT_DISCOVERY_IP_WINDOW must be positive when TG_RATE_LIMIT_DISCOVERY_IP is enabled")
	}
	return limits, nil
}

func discoveryEnvValue(primary, alias string) (value, name string, err error) {
	primaryValue := os.Getenv(primary)
	aliasValue := os.Getenv(alias)
	if primaryValue != "" && aliasValue != "" {
		return "", "", fmt.Errorf("%s and %s must not both be set", primary, alias)
	}
	if primaryValue != "" {
		return primaryValue, primary, nil
	}
	return aliasValue, alias, nil
}

// clientAddrTrust resolves the client-address source. An unset value is the
// socket, which is the only one implemented; anything else is refused by name
// rather than falling back, because a silent fallback to socket addresses
// behind a load balancer would key every client in the world to one bucket.
func clientAddrTrust(raw string) (ClientAddrTrust, error) {
	if raw == "" {
		return ClientAddrSocket, nil
	}
	for _, v := range clientAddrTrustValues {
		if ClientAddrTrust(raw) == v {
			return v, nil
		}
	}
	names := make([]string, len(clientAddrTrustValues))
	for i, v := range clientAddrTrustValues {
		names[i] = string(v)
	}
	return "", fmt.Errorf("TG_CLIENT_ADDR_TRUST must be one of: %s", strings.Join(names, ", "))
}

// clientAddrProxies resolves the balancer allowlist and refuses every way of
// getting it wrong, in both directions.
//
// In proxy-v2 mode an empty list cannot be defaulted: read as "trust nobody" it
// refuses every connection, read as "trust anybody" it lets any sender name any
// client address, and guessing between them is not the config loader's call. So
// the mode requires the list.
//
// The reverse mismatch fails just as hard. A list set while the mode is socket
// is an operator who believes they are behind a balancer and is in fact keying
// every client on the balancer's address — the collapsed global bucket this mode
// exists to prevent, and the one misconfiguration that looks fine at startup and
// locks out every login at once.
func clientAddrProxies(raw string, trust ClientAddrTrust) ([]netip.Prefix, error) {
	prefixes, err := parsePrefixes(raw)
	if err != nil {
		return nil, err
	}
	if trust != ClientAddrProxyV2 {
		if len(prefixes) > 0 {
			return nil, fmt.Errorf("TG_CLIENT_ADDR_PROXY_CIDRS is set while TG_CLIENT_ADDR_TRUST is %q: the allowlist is only read in %q mode, and %q keys every client on the balancer's own address", trust, ClientAddrProxyV2, trust)
		}
		return nil, nil
	}
	if len(prefixes) == 0 {
		return nil, fmt.Errorf("TG_CLIENT_ADDR_TRUST=%s requires TG_CLIENT_ADDR_PROXY_CIDRS to list the balancer addresses a PROXY header is accepted from", ClientAddrProxyV2)
	}
	return prefixes, nil
}

// mappedPrefixOffset is where the IPv4 part of an IPv4-mapped address starts:
// the first 96 bits are the ::ffff: wrapper.
const mappedPrefixOffset = 96

// normalizePrefix rewrites an IPv4-mapped prefix into the IPv4 one it means.
//
// It has to, because the two sides would otherwise never meet: the listener
// unmaps every peer address it reports, so a prefix left in mapped form is 128
// bits wide against a 32-bit address and matches nothing. The entry would start
// the server and then refuse every connection from the balancer it was written
// to name — an outage that looks like the limiter working, which is a worse
// failure than the default route, not a lesser one.
//
// A mapped prefix shorter than /96 covers more than the mapped range and has no
// IPv4 network to take, so it is refused rather than guessed at. Everything from
// /96 down takes its IPv4 meaning, which puts a disguised default route
// (::ffff:0.0.0.0/96) in front of the zero-length guard rather than past it.
func normalizePrefix(p netip.Prefix, entry string) (netip.Prefix, error) {
	if !p.Addr().Is4In6() {
		return p, nil
	}
	if p.Bits() < mappedPrefixOffset {
		return netip.Prefix{}, fmt.Errorf("TG_CLIENT_ADDR_PROXY_CIDRS entry %q is an IPv4-mapped prefix shorter than /%d, which names no IPv4 network: write it as an IPv4 CIDR", entry, mappedPrefixOffset)
	}
	return netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-mappedPrefixOffset), nil
}

// parsePrefixes reads a comma-separated allowlist. A bare address is its own
// single-host prefix, which is what an operator with one balancer writes.
// Anything that does not parse fails the start naming the entry: dropping it
// would leave a balancer whose connections are all refused, which reads as an
// outage rather than a typo.
func parsePrefixes(raw string) ([]netip.Prefix, error) {
	var prefixes []netip.Prefix
	for entry := range strings.SplitSeq(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if strings.Contains(entry, "/") {
			p, err := netip.ParsePrefix(entry)
			if err != nil {
				return nil, fmt.Errorf("TG_CLIENT_ADDR_PROXY_CIDRS entry %q is not a CIDR", entry)
			}
			// Before the guard below, so a default route written in mapped form
			// is caught by it rather than slipping past as a 96-bit prefix.
			if p, err = normalizePrefix(p, entry); err != nil {
				return nil, err
			}
			// A default route trusts every peer on the internet as a balancer,
			// which hands any client the right to name any source address: to
			// step out of its own bucket, or to pin an innocent address into
			// flood wait. That is the spoofable identity the whole mode exists
			// to prevent, and it is the one allowlist mistake that would
			// otherwise start cleanly, so it fails the start like the rest.
			if p.Bits() == 0 {
				return nil, fmt.Errorf("TG_CLIENT_ADDR_PROXY_CIDRS entry %q trusts every address: name the balancers, not a default route", entry)
			}
			prefixes = append(prefixes, p)
			continue
		}
		addr, err := netip.ParseAddr(entry)
		if err != nil {
			return nil, fmt.Errorf("TG_CLIENT_ADDR_PROXY_CIDRS entry %q is not an address or CIDR", entry)
		}
		// Unmapped so a 4-in-6 spelling matches the same peer addresses the
		// listener reports, which are unmapped for the same reason.
		addr = addr.Unmap()
		prefixes = append(prefixes, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return prefixes, nil
}

// parseWebSocketOriginPatterns reads and validates the browser-origin
// allowlist. Empty entries are ignored so an unset variable and a trailing
// comma have the same fail-closed meaning: no cross-origin browser request is
// accepted.
func parseWebSocketOriginPatterns(raw, name string) ([]string, error) {
	var patterns []string
	for entry := range strings.SplitSeq(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry != "" {
			if _, err := path.Match(entry, ""); err != nil {
				return nil, fmt.Errorf("%s entry %q is malformed: %w", name, entry, err)
			}
			patterns = append(patterns, entry)
		}
	}
	return patterns, nil
}

// WarnClientAddrTrust states the operational assumption socket mode makes,
// once, at startup.
//
// It is not decoration. Per-IP limits in socket mode assume one peer address is
// one client, and the same collapse breaks that from either end. In front of
// the server, a proxy or an L4 load balancer makes every peer address the
// balancer's, so one bucket holds every client on earth and the per-IP cap
// becomes a global cap that locks everybody out at the same moment. In front of
// the clients, a carrier NAT puts thousands of mobile subscribers behind one
// IPv4 address, so the whole carrier spends a single key's budget and the 21st
// distinct number from it in a day is refused for up to a day.
//
// The balancer half has an answer now — ClientAddrProxyV2 keys on the address
// the balancer reports rather than its own — and this line is what tells an
// operator running without it where the support tickets will come from before
// they arrive. The carrier-NAT half remains an accepted risk in every mode.
// Silent in proxy-v2 mode, where the assumption it names is not the one being
// made, and silent when no per-IP limit is enabled, since nothing is then keyed
// on an address at all.
//
// Every limit keyed on a client address has to be in the condition below, or an
// operator turns one off and stops being warned about the other. The pre-auth
// connection cap is the harsher of the two to get wrong: the sendCode limits
// collapsing into one bucket refuses code requests, while a pre-auth cap
// collapsing into one bucket refuses handshakes server-wide, which is every
// client at once and looks like an outage rather than a limit.
func (c Config) WarnClientAddrTrust(log *slog.Logger) {
	perIPLimits := c.RateLimits.SendCodeIP.Enabled() || c.RateLimits.SignInFailIP.Enabled() ||
		c.RateLimits.CheckPasswordIP.Enabled() || c.RateLimits.GetPasswordIP.Enabled() ||
		c.RateLimits.SignUpIP.Enabled() || c.RateLimits.CheckUsernameIP.Enabled() ||
		c.PreAuth.MaxConnsPerNet > 0
	if c.ClientAddrTrust != ClientAddrSocket || !perIPLimits {
		return
	}
	log.Warn("per-IP limits are keyed on each connection's own peer address",
		"trust", string(c.ClientAddrTrust),
		"assumes", "one peer address is one client, reaching this process directly",
		"risk", "behind a proxy or L4 load balancer every client shares one bucket: the per-IP cap becomes a global one, and the pre-auth connection cap becomes a server-wide ceiling on concurrent handshakes that refuses every client past it; behind a carrier NAT a whole mobile network shares one bucket and its subscribers are refused on each other's traffic")
}

// handshakeReadTimeout is the timeout gotd applies to each read inside key
// exchange (its DefaultTimeout). It is not this server's number to set, and it
// is the floor a pre-auth lifetime ceiling has to clear: a ceiling under it
// expires while a handshake is still legitimately waiting on one read.
const handshakeReadTimeout = exchange.DefaultTimeout

// WarnPreAuthLifetime states, once at startup, that the configured pre-auth
// ceiling is short enough to cut handshakes rather than holds.
//
// It is a warning and not a startup failure because the value is legitimate
// under load shedding, and because zero — the ceiling off entirely — is a
// supported setting that this must not appear to forbid. What it prevents is the
// silent case: an operator tuning the ceiling down to shed a flood, and getting
// intermittent handshake failures from slow clients that look like a network
// fault rather than the number they just set.
func (c Config) WarnPreAuthLifetime(log *slog.Logger) {
	if c.PreAuth.Lifetime <= 0 || c.PreAuth.Lifetime >= handshakeReadTimeout {
		return
	}
	log.Warn("TG_PREAUTH_LIFETIME is below the handshake's own read timeout",
		"lifetime", c.PreAuth.Lifetime,
		"handshake_read_timeout", handshakeReadTimeout,
		"risk", "a client still inside key exchange can be closed for being slow rather than for holding the connection, which reads as an intermittent connection failure")
}

// advertiseAddr resolves the address clients are told to dial. An explicit
// TG_ADVERTISE_ADDR is used verbatim and must parse, since a wrong one is only
// visible as clients failing to reconnect; an empty one is derived from the
// listen address, which stays as loosely validated as it was — a bad one fails
// loudly at net.Listen.
func advertiseAddr(advertise, listen string) (string, int, error) {
	if advertise == "" {
		host, port := splitHostPort(listen)
		return host, port, nil
	}
	return explicitAdvertiseAddr("TG_ADVERTISE_ADDR", advertise)
}

func publicWebSocketAdvertiseAddr(advertise, publicListen string) (string, int, error) {
	if advertise == "" {
		return "", 0, nil
	}
	if publicListen == "" {
		return "", 0, errors.New("TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR requires TG_PUBLIC_WEBSOCKET_LISTEN_ADDR")
	}
	return explicitAdvertiseAddr("TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR", advertise)
}

func explicitAdvertiseAddr(name, advertise string) (string, int, error) {
	host, portStr, err := net.SplitHostPort(advertise)
	if err != nil {
		return "", 0, fmt.Errorf("%s must be host:port", name)
	}
	if host == "" {
		return "", 0, fmt.Errorf("%s must name a host clients can reach", name)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", 0, fmt.Errorf("%s port must be an integer", name)
	}
	if port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("%s port must be between 1 and 65535", name)
	}
	return host, port, nil
}

// splitHostPort derives an advertisable address from a listen address. A host
// that is empty or a wildcard binds every interface but names none, so it
// becomes loopback — the one address that is always reachable.
func splitHostPort(addr string) (string, int) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return "127.0.0.1", 2443
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		port = 2443
	}
	return host, port
}

// loadEncKey resolves the auth-key master key from the two sources that can
// carry it. TG_AUTHKEY_ENC_KEY wins and is never written anywhere. Failing
// that, TG_AUTHKEY_ENC_KEY_FILE names a file the key is read from and, when
// allowed, generated into on first boot. A pinned server identity requires a
// provisioned key, so it never generates one.
func loadEncKey(log *slog.Logger, allowGenerate bool) ([]byte, error) {
	if raw := os.Getenv("TG_AUTHKEY_ENC_KEY"); raw != "" {
		return decodeEncKey(raw, "TG_AUTHKEY_ENC_KEY")
	}
	path := os.Getenv("TG_AUTHKEY_ENC_KEY_FILE")
	if path == "" {
		return nil, errors.New("TG_AUTHKEY_ENC_KEY is required (64 hex chars = 32 bytes), or set TG_AUTHKEY_ENC_KEY_FILE to a path the key is kept in")
	}
	key, generated, err := encKeyFromFile(path, allowGenerate)
	if err != nil {
		return nil, err
	}
	if generated {
		log.Warn("TG_AUTHKEY_ENC_KEY not set: generated a dev key", "path", path, "action", "do not use in production")
	} else {
		log.Info("auth-key master key loaded from file", "path", path)
	}
	return key, nil
}

// encKeyFromFile reads the master key at path, optionally generating and
// persisting one when the file is absent.
//
// The new key is written to a temporary file and linked into place, so the key
// file only ever becomes visible complete. Creating it with O_EXCL and writing
// afterwards looks equivalent and is not: it leaves a window where the path
// exists and is empty, and a second server starting inside that window reads
// nothing and fails to boot. os.Link is what closes it — it publishes the
// finished file in one step and fails with ErrExist rather than replacing a
// key another start already published, which a rename would do. A concurrent
// first start sharing the same file adopts the winner's key. Pinned-identity
// mode disables generation so each process cannot mint a separate master key.
func encKeyFromFile(path string, allowGenerate bool) (key []byte, generated bool, err error) {
	key, err = readEncKeyFile(path)
	switch {
	case err == nil:
		return key, false, nil
	case !os.IsNotExist(err):
		return nil, false, err
	case !allowGenerate:
		return nil, false, fmt.Errorf("TG_AUTHKEY_ENC_KEY_FILE %q must already exist when a server identity is pinned: %w", path, err)
	}

	buf := make([]byte, keycrypt.KeyLen)
	if _, err := rand.Read(buf); err != nil {
		return nil, false, fmt.Errorf("generate auth-key master key: %w", err)
	}
	// Same directory as the key file: os.Link cannot cross filesystems, and
	// TempDir would be a different one under the container's read-only root.
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".enc_key-*")
	if err != nil {
		return nil, false, fmt.Errorf("create temp key file in %s: %w", dir, err)
	}
	// Reported rather than dropped: what is left behind is master-key material
	// under a name nothing will ever read again, and a filesystem that cannot
	// unlink is worth failing the start over. Only the first start of a fresh
	// volume reaches here, and the next start reads the published file without
	// creating a temp at all, so this cannot become a crash loop.
	defer func() {
		if rmErr := os.Remove(tmp.Name()); rmErr != nil && !os.IsNotExist(rmErr) { // #nosec G703 -- os.CreateTemp's own name in the operator-configured directory.
			err = errors.Join(err, fmt.Errorf("remove temp key file: %w", rmErr))
		}
	}()

	_, writeErr := tmp.WriteString(hex.EncodeToString(buf))
	// Sync before the link, not after: the master key must be on the disk
	// before anything can be sealed under it. Postgres commits auth-key
	// ciphertext durably, so a key that is only in the page cache when the host
	// loses power comes back missing while the rows it opens come back intact —
	// every session undecryptable, which is the one failure this file exists to
	// prevent.
	if err := errors.Join(writeErr, tmp.Chmod(0o600), tmp.Sync(), tmp.Close()); err != nil {
		return nil, false, fmt.Errorf("write temp key file: %w", err)
	}
	if err := os.Link(tmp.Name(), path); err != nil {
		if os.IsExist(err) {
			// Another start published its key first. Its key is the real one,
			// and it is complete the moment the path exists.
			key, err := readEncKeyFile(path)
			return key, false, err
		}
		return nil, false, fmt.Errorf("create %s: %w", path, err)
	}
	// The file's bytes are durable, but the directory entry naming them is not
	// until the directory itself is synced.
	if err := syncDir(dir); err != nil {
		return nil, false, err
	}
	return buf, true, nil
}

// syncDir flushes a directory's own entries, which is what makes a newly linked
// name survive a crash. Opened read-only: a directory cannot be opened for
// writing, and fsync does not need it.
func syncDir(dir string) error {
	d, err := os.Open(dir) // #nosec G304,G703 -- the directory of the operator-configured key file.
	if err != nil {
		return fmt.Errorf("open %s: %w", dir, err)
	}
	return errors.Join(d.Sync(), d.Close())
}

// readEncKeyFile reads and validates the key file. A NotExist error is returned
// unwrapped so the caller can tell "no key yet" from a real read failure.
func readEncKeyFile(path string) ([]byte, error) {
	raw, err := os.ReadFile(path) // #nosec G304,G703 -- path is the operator-configured key file, not untrusted input.
	if err != nil {
		if os.IsNotExist(err) {
			return nil, err
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return decodeEncKey(strings.TrimSpace(string(raw)), path)
}

// decodeEncKey parses the hex-encoded auth-key encryption master key. It is
// required and must decode to exactly keycrypt.KeyLen bytes, so the server fails
// fast rather than starting without at-rest encryption or with a weak key. src
// names where the value came from — the env var or a file path — because the
// two failure modes are diagnosed in different places.
func decodeEncKey(raw, src string) ([]byte, error) {
	if raw == "" {
		return nil, fmt.Errorf("%s is empty: expected 64 hex chars (32 bytes)", src)
	}
	key, err := hex.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("%s must be valid hex", src)
	}
	if len(key) != keycrypt.KeyLen {
		return nil, fmt.Errorf("%s must be 64 hex chars (32 bytes)", src)
	}
	return key, nil
}

// adminHashRe matches a 64-character lowercase hex string (SHA-256 digest).
var adminHashRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

var replicaIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

var (
	botAPICGNATPrefix   = netip.MustParsePrefix("100.64.0.0/10")
	botAPITailnetPrefix = netip.MustParsePrefix("fd7a:115c:a1e0::/48")
)

func validateReplicaID(replicaID string) error {
	if replicaID != "" && !replicaIDRe.MatchString(replicaID) {
		return errors.New("TG_REPLICA_ID must be 1-64 characters from [A-Za-z0-9._-]")
	}
	return nil
}

// validateAdmin checks that the admin server env vars are consistent.
//
// TG_ADMIN_LISTEN_ADDR and TG_ADMIN_TOKEN_HASH must be set together, and a
// configured TG_ADMIN_ORIGIN requires the listener. A listener without a token
// hash is an unauthenticated admin surface; a token hash with no listener is
// wasted work. The hash must be a 64-char lowercase hex string (a SHA-256
// digest).
func validateAdmin(cfg *Config) error {
	listenAddr := cfg.AdminListenAddr
	tokenHash := os.Getenv("TG_ADMIN_TOKEN_HASH")

	if listenAddr == "" {
		if tokenHash != "" {
			return errors.New("TG_ADMIN_TOKEN_HASH is set but TG_ADMIN_LISTEN_ADDR is missing: both are required to start the admin HTTP server")
		}
		if strings.TrimSpace(cfg.AdminOrigin) != "" {
			return errors.New("TG_ADMIN_ORIGIN is set but TG_ADMIN_LISTEN_ADDR is missing: both are required to start the admin HTTP server")
		}
		cfg.AdminOrigin = ""
		return nil
	}
	if tokenHash == "" {
		return errors.New("TG_ADMIN_LISTEN_ADDR is set but TG_ADMIN_TOKEN_HASH is missing: both are required to start the admin HTTP server")
	}
	if !adminHashRe.MatchString(tokenHash) {
		return errors.New("TG_ADMIN_TOKEN_HASH must be a 64-character lowercase hex string (SHA-256 digest of the operator token)")
	}

	if strings.TrimSpace(cfg.AdminOrigin) == "" {
		host, port, err := net.SplitHostPort(listenAddr)
		if err != nil {
			return fmt.Errorf("TG_ADMIN_LISTEN_ADDR: %w", err)
		}
		if host == "" {
			host = "localhost"
		}
		cfg.AdminOrigin = "http://" + net.JoinHostPort(host, port)
		return nil
	}
	if err := validateAdminOrigin(cfg.AdminOrigin); err != nil {
		return fmt.Errorf("TG_ADMIN_ORIGIN %w", err)
	}
	return nil
}

func validateBotAPI(cfg *Config) error {
	addr := cfg.BotAPIListenAddr
	if addr == "" {
		return nil
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return errors.New("TG_BOT_API_LISTEN_ADDR must be an explicit loopback or tailnet IP literal with a port")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return errors.New("TG_BOT_API_LISTEN_ADDR must use a port from 1 to 65535")
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || ip.Zone() != "" {
		return errors.New("TG_BOT_API_LISTEN_ADDR host must be an unscoped IP literal")
	}
	ip = ip.Unmap()
	if !ip.IsLoopback() && !botAPICGNATPrefix.Contains(ip) && !botAPITailnetPrefix.Contains(ip) {
		return errors.New("TG_BOT_API_LISTEN_ADDR host must be loopback, in 100.64.0.0/10, or in fd7a:115c:a1e0::/48")
	}

	for _, other := range []string{
		cfg.ListenAddr,
		cfg.WebSocketListenAddr,
		cfg.PublicWebSocketListenAddr,
		cfg.AdminListenAddr,
	} {
		if other != "" && botAPIAddressConflicts(ip, portNumber, other) {
			return errors.New("TG_BOT_API_LISTEN_ADDR must not overlap another listener address")
		}
	}
	return nil
}

func botAPIAddressConflicts(botIP netip.Addr, botPort int, other string) bool {
	if other == "" {
		return false
	}
	otherHost, otherPort, err := net.SplitHostPort(other)
	if err != nil {
		return false
	}
	otherPortNumber, err := strconv.Atoi(otherPort)
	if err != nil || otherPortNumber != botPort {
		return false
	}
	if otherHost == "" {
		return true
	}
	otherIP, err := netip.ParseAddr(otherHost)
	if err != nil {
		// A same-port hostname can resolve to the Bot API address at bind time.
		return true
	}
	otherIP = otherIP.Unmap()
	return otherIP.IsUnspecified() || otherIP == botIP
}

func validateAdminOrigin(origin string) error {
	parsed, err := url.Parse(origin)
	if err != nil {
		return fmt.Errorf("is invalid: %w", err)
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return errors.New("must use https, or http for a loopback host")
	}
	if parsed.Opaque != "" || parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawFragment != "" {
		return errors.New("must contain only a scheme and host with an optional port")
	}
	for i := range len(origin) {
		if origin[i] > 0x7f {
			return errors.New("must use ASCII characters")
		}
	}

	host := parsed.Hostname()
	if !validAdminHostname(host) {
		return errors.New("must contain a lowercase canonical ASCII host")
	}
	port := parsed.Port()
	if port != "" {
		portNumber, err := strconv.Atoi(port)
		if err != nil || portNumber < 1 || portNumber > 65535 || strconv.Itoa(portNumber) != port {
			return errors.New("must contain a canonical port from 1 through 65535")
		}
		if (parsed.Scheme == "https" && portNumber == 443) || (parsed.Scheme == "http" && portNumber == 80) {
			return errors.New("must omit the scheme's default port")
		}
	}
	if parsed.Scheme == "http" && !adminHTTPHostAllowed(host) {
		return errors.New("http is allowed only for localhost, 127.0.0.0/8, or ::1")
	}

	authority := host
	if strings.Contains(host, ":") {
		authority = "[" + host + "]"
	}
	if port != "" {
		authority += ":" + port
	}
	if origin != parsed.Scheme+"://"+authority {
		return errors.New("must use canonical scheme://host[:port] form")
	}
	return nil
}

func validAdminHostname(host string) bool {
	if host == "" || host == "null" || strings.ToLower(host) != host || len(host) > 253 {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.String() == host
	}
	for label := range strings.SplitSeq(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := range len(label) {
			c := label[i]
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	return true
}

func adminHTTPHostAllowed(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if ipv4 := ip.To4(); ipv4 != nil {
		return ipv4[0] == 127
	}
	return ip.Equal(net.ParseIP("::1"))
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// registrationMode resolves the registration mode from the env var. Unset or
// empty string produces the closed mode, so registration stays disabled by
// default until the operator opts in with a recognized value.
func registrationMode(raw string) RegistrationMode {
	switch {
	case raw == "":
		return RegistrationClosed
	case RegistrationMode(raw) == RegistrationClosed:
		return RegistrationClosed
	case RegistrationMode(raw) == RegistrationInvite:
		return RegistrationInvite
	case RegistrationMode(raw) == RegistrationOpen:
		return RegistrationOpen
	}
	return RegistrationMode(raw)
}

func parseStickerImportMode(raw string) (StickerImportMode, error) {
	switch StickerImportMode(raw) {
	case "", StickerImportOn:
		return StickerImportOn, nil
	case StickerImportOff:
		return StickerImportOff, nil
	default:
		return "", fmt.Errorf("TG_STICKER_IMPORT must be unset, %q, or %q; got %q", StickerImportOn, StickerImportOff, raw)
	}
}

// ValidateRegistrationMode checks that the registration mode is one of the
// recognized values. It is called after Load to fail the start when an
// operator names a mode this build does not implement.
func (c Config) ValidateRegistrationMode() error {
	if err := validateRegistrationMode("TG_REGISTRATION", c.RegistrationMode); err != nil {
		return err
	}
	return validateRegistrationMode("TG_PUBLIC_REGISTRATION", c.PublicRegistrationMode)
}

func validateRegistrationMode(name string, mode RegistrationMode) error {
	switch mode {
	case RegistrationClosed, RegistrationInvite, RegistrationOpen:
		return nil
	}
	return fmt.Errorf("%s must be unset, %q, %q, or %q; got %q", name, RegistrationClosed, RegistrationInvite, RegistrationOpen, mode)
}
