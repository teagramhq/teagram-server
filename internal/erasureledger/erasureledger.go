// Package erasureledger is the record vocabulary and strict codec for the
// durable off-alpha erasure ledger specified by MAIN-1360 and amended by
// MAIN-1362 and MAIN-1436.
//
// It is inert by construction: nothing in internal/ or cmd/ encodes or decodes
// these records yet, so no writer, flusher, replay command, admission gate or
// restore path consumes them. What lands here is the vocabulary and the wire
// contract only, so the identifiers a record may name, the widths they keep,
// and the failure a reader must report are provable before any runtime path
// exists that could get them wrong.
//
// # Identifier-only bodies
//
// A record body carries the minimum needed to re-enforce an erasure after a
// restore, and nothing else. It never carries a phone, a display name, message
// text, an access hash, a blob key, an auth key, a session id, an MTProto
// message id, an upload payload digest, or any bytes of stored content. Those
// are enforced structurally: every payload field is a fixed-width integer, a
// fixed-length opaque identifier, an enumerated value, or a nested struct of
// the same. No payload field is a bare string, a byte slice, a boolean, a
// time, a map, an interface, or a pointer, so the format has no way to express
// them. The one string-shaped field is the schema allocator name, a named
// type the codec constrains to lower-case schema identifiers, and the only
// byte-carrying values are the fixed-length opaque identities. The reserved
// transport tags in codec.go are rejected on decode, so a body that tries to
// smuggle one cannot be read at all.
//
// # Opaque operation identity
//
// Record identity is the server-assigned OperationKey: 16 bytes of randomness
// drawn by the caller through NewOperationKey, never derived from an owner id,
// a local id, a transport identity, or any payload field. It is what makes
// a retried write the same record, so duplicates are harmless. Records
// also carry an epoch, a replica stream, and a per-(epoch, stream) sequence for
// enumeration completeness. Ordering of erasures comes from the provider's
// arrival evidence, never from a writer-supplied timestamp: this codec has no
// timestamp field, on purpose, so a writer cannot order a ledger by its own
// clock.
//
// # Monotone semantics
//
// Every kind is a monotone set-to-deleted, a tombstone, a maximum, or an
// immutable binding (KindSpec). A record can name a copy as deleted, an
// identity as tombstoned, an id as permanently excluded, a ceiling as at least
// some value, or a stream's one lineage; no kind can revive, un-delete,
// promote, renumber, revoke, or widen a scope.
// Because of that, replay is order-independent and idempotent:
// applying the same record twice, or a set of records in any order, yields the
// same state. Canonical wire form (sorted, de-duplicated sets; minimal
// integers; ascending fields) is what makes that equality observable: the same
// semantic set always encodes to the same bytes.
//
// # Fail-closed reading
//
// A reader that meets a kind it does not know, one only a newer binary
// writes, returns an error matching ErrNotReady that names the kind, and never
// a zero-value default and never a silently skipped record. A codec
// version above CodecVersion is reported the same way. Malformed, truncated,
// out-of-range, non-canonical and unknown-field input returns an error matching
// ErrRejected. MAIN-1436 requires that a replayer which does not recognise the
// gallery_delete kind leaves admission closed rather than skipping it: the
// NotReadyInfo accessor is the admission contract for that decision, so a
// caller branches on a typed cause without parsing error text.
//
// # Boundaries
//
// This package performs no I/O, no cryptography, and no key handling: it draws
// opaque identity bytes from a caller-supplied io.Reader and does nothing else
// with randomness. It has no database, provider, RPC, configuration, replay,
// expiration, or admission wiring. Its closed reservation classification is
// checked against the executable allocator inventory, but nothing here claims
// that any ledger, provider, restore, or replay capability exists, is
// provisioned, or is ready.
package erasureledger

import (
	"io"
	"math"
	"slices"
)

// Identifier and framing constants. Lengths are fixed so an opaque identity is
// always exactly what a listing and a body must describe.
const (
	// OperationKeyLen is the opaque server-assigned record identity
	// length, matching MAIN-1360's 128 random bits.
	OperationKeyLen = 16
	// StreamIDLen is the opaque replica-stream identity length. A stream is
	// a writer lease, not a host name, so a listing leaks no infrastructure
	// topology.
	StreamIDLen = 16
	// LineageIDLen is the opaque restore-lineage identity length. Completion is
	// bound to a lineage, so a shared epoch number alone can never mark a
	// restored lineage complete.
	LineageIDLen = 16
	// MaxAllocatorNameLen bounds the schema allocator name a reservation
	// record names.
	MaxAllocatorNameLen = 64

	// maxLocalID is the signed-32-bit wire ceiling for message and channel
	// message ids. Records carry the wire width, so a record can never name a
	// copy the protocol cannot express, and a decoded record cannot be wider
	// than the live delete it describes.
	maxLocalID = math.MaxInt32

	// MaxRecordBytes bounds one encoded record. The largest legal body is a few
	// thousand set members; anything past this bound is malformed, not large.
	MaxRecordBytes = 1 << 16
	// MaxBatchBytes bounds one encoded batch of records.
	MaxBatchBytes = 1 << 22
	// MaxBatchRecords bounds the record count a batch may declare.
	MaxBatchRecords = 4096
	// MaxSetMembers bounds any single monotone set inside one record: message
	// copies, channel posts, excluded ids, gallery entries.
	MaxSetMembers = 4096
)

// Kind is the discriminator for a record body. Kind values are
// wire-frozen: they are the vocabulary a replayer dispatches on, so renumbering
// one silently re-points every stored record at the wrong meaning.
type Kind uint16

// The accepted kinds of MAIN-1360 as amended by MAIN-1362, plus the accepted
// MAIN-1431/MAIN-1436 gallery extension and inert reservation ownership
// forms. A newer binary may add kinds; a reader that does not know one fails
// closed with ErrNotReady.
const (
	// KindAccount tombstones an account identity, so a replayed id can only
	// ever mean the identity that was erased.
	KindAccount Kind = 1
	// KindFile tombstones a file identity: the row is tombstoned, never
	// deleted, so restored references to it fail closed.
	KindFile Kind = 2
	// KindMessageCopies sets an exact set of (owner, local) message copies to
	// deleted. It is not a revoke: the set is the one the live transaction
	// deleted, and replay can neither widen nor narrow it.
	KindMessageCopies Kind = 3
	// KindChannelPosts sets an exact set of (channel, local) post copies to
	// deleted. Channel posts live in their own table and id space.
	KindChannelPosts Kind = 4
	// KindRandomExclusion adds drawn random ids to the permanent no-reuse
	// exclusion set for an allocator class. Membership only ever grows.
	KindRandomExclusion Kind = 5
	// KindReservation records an absolute global sequence ceiling. Its scalar
	// form is deliberately insufficient for per-scope component-sum readiness.
	KindReservation Kind = 6
	// KindEpoch establishes a restore epoch for a lineage, and records its
	// completion against that same lineage.
	KindEpoch Kind = 7
	// KindGalleryDelete records the exact gallery entries a profile-photo clear
	// deleted, with the per-owner mutation revision it cleared at.
	KindGalleryDelete Kind = 8
	// KindReceiptTerminal upserts a terminal upload receipt, including
	// one the restored database never held.
	KindReceiptTerminal Kind = 9
	// KindStreamBinding immutably binds one (epoch, stream) envelope to a
	// restore lineage. The envelope supplies epoch and stream; the body supplies
	// the opaque lineage identity.
	KindStreamBinding Kind = 10
	// KindComponentReservation records a component-sum ceiling and its restored
	// baseline. Its key also includes the bound lineage and envelope stream.
	KindComponentReservation Kind = 11
)

// Monotone is the one-way meaning a kind carries. It is a property of the
// vocabulary, not of a record: there is no kind, and no field of any kind, that
// can mean the reverse.
type Monotone uint8

const (
	// MonotoneSetToDeleted grows a set of names toward deleted, excluded, or
	// terminal, and never back.
	MonotoneSetToDeleted Monotone = 1
	// MonotoneTombstone marks one identity permanently dead while
	// keeping the identity itself out of reuse.
	MonotoneTombstone Monotone = 2
	// MonotoneMaximum raises a counter to at least a value, and never lowers it.
	MonotoneMaximum Monotone = 3
	// MonotoneBinding adds one immutable (epoch, stream) to lineage binding.
	// Repeating it is idempotent; a conflict does not replace the first binding.
	MonotoneBinding Monotone = 4
)

// KindSpec describes one kind: its monotone class, the identifiers it names,
// and the scope it must not cross. The table is the documented contract a
// future writer, replay command, and admission gate share.
type KindSpec struct {
	Kind     Kind
	Monotone Monotone
	Scope    string
}

// kindSpecs is the closed vocabulary this binary understands. Adding a kind
// means adding a payload type, a body codec arm, and a row here in the same
// change, so a kind can never exist on the wire without a meaning.
var kindSpecs = []KindSpec{
	{Kind: KindAccount, Monotone: MonotoneTombstone,
		Scope: "one account id; leaves a tombstone and never touches another account"},
	{Kind: KindFile, Monotone: MonotoneTombstone,
		Scope: "one file id; tombstones the row and re-issues the unlink"},
	{Kind: KindMessageCopies, Monotone: MonotoneSetToDeleted,
		Scope: "the exact (owner, local) copies the committing transaction deleted"},
	{Kind: KindChannelPosts, Monotone: MonotoneSetToDeleted,
		Scope: "the exact (channel, local) post copies the committing transaction deleted"},
	{Kind: KindRandomExclusion, Monotone: MonotoneSetToDeleted,
		Scope: "ids drawn for one random allocator class; excluded for their lifetime"},
	{Kind: KindReservation, Monotone: MonotoneMaximum,
		Scope: "one absolute global sequence ceiling; a lower ceiling is stale, not a rollback"},
	{Kind: KindEpoch, Monotone: MonotoneMaximum,
		Scope: "one epoch for one lineage; completion is bound to that lineage"},
	{Kind: KindGalleryDelete, Monotone: MonotoneSetToDeleted,
		Scope: "one owner's exact deleted gallery tuples and the revision cleared at"},
	{Kind: KindReceiptTerminal, Monotone: MonotoneSetToDeleted,
		Scope: "one owner's upload receipt identity, moved to a terminal state"},
	{Kind: KindStreamBinding, Monotone: MonotoneBinding,
		Scope: "one immutable lineage for an (epoch, stream); conflict is not replacement"},
	{Kind: KindComponentReservation, Monotone: MonotoneMaximum,
		Scope: "one (lineage, stream, component) ceiling against its explicit restored baseline"},
}

func init() {
	slices.SortFunc(kindSpecs, func(a, b KindSpec) int { return int(a.Kind) - int(b.Kind) })
}

// KindSpecs returns a copy of the vocabulary this binary understands.
func KindSpecs() []KindSpec {
	return slices.Clone(kindSpecs)
}

// LookupKind reports the spec for k, and whether this binary knows k at all. A
// false result is what a decoder turns into ErrNotReady.
func LookupKind(k Kind) (KindSpec, bool) {
	i, found := slices.BinarySearchFunc(kindSpecs, k, func(s KindSpec, k Kind) int {
		return int(s.Kind) - int(k)
	})
	if !found {
		return KindSpec{}, false
	}
	return kindSpecs[i], true
}

// String names a kind for logs and error text.
func (k Kind) String() string {
	spec, known := LookupKind(k)
	if !known {
		return "Kind(" + itoa(uint64(k)) + ")"
	}
	return spec.Name()
}

// Name returns the Go identifier of the payload type for a known kind.
func (s KindSpec) Name() string {
	switch s.Kind {
	case KindAccount:
		return "Account"
	case KindFile:
		return "File"
	case KindMessageCopies:
		return "MessageCopies"
	case KindChannelPosts:
		return "ChannelPostCopies"
	case KindRandomExclusion:
		return "RandomExclusion"
	case KindReservation:
		return "Reservation"
	case KindEpoch:
		return "Epoch"
	case KindGalleryDelete:
		return "GalleryDelete"
	case KindReceiptTerminal:
		return "ReceiptTerminal"
	case KindStreamBinding:
		return "StreamBinding"
	case KindComponentReservation:
		return "ComponentReservation"
	default:
		return "Kind(" + itoa(uint64(s.Kind)) + ")"
	}
}

// OperationKey is the opaque, server-assigned identity of one ledger operation.
// It is randomness, verbatim: NewOperationKey copies the caller's bytes and
// mixes in nothing, so a key can never embed an owner id, a local id, a
// transport identity, a phone, or any payload value. It is an array type, not a
// string, so it cannot carry text.
type OperationKey [OperationKeyLen]byte

// StreamID is the opaque identity of the replica stream (writer lease) that
// produced a record. Sequences are contiguous per (epoch, stream).
type StreamID [StreamIDLen]byte

// LineageID is the opaque identity of one restored lineage. Epoch completion is
// recorded against it, so an older lineage's completion evidence cannot be
// mistaken for the restored one's.
type LineageID [LineageIDLen]byte

// NewOperationKey draws an opaque operation key from rand. The caller supplies
// the entropy source (crypto/rand.Reader in production); this package
// performs no key derivation, no encryption, and no MAC. A short reader is an
// error, never a zero-padded key.
func NewOperationKey(rand io.Reader) (OperationKey, error) {
	var key OperationKey
	if _, err := io.ReadFull(rand, key[:]); err != nil {
		return OperationKey{}, wrapRejected("entropy source short", "operation_key", err)
	}
	if key == (OperationKey{}) {
		return OperationKey{}, newRejected(ReasonAllZero, "operation_key")
	}
	return key, nil
}

// NewStreamID draws an opaque replica-stream identity from rand.
func NewStreamID(rand io.Reader) (StreamID, error) {
	var id StreamID
	if _, err := io.ReadFull(rand, id[:]); err != nil {
		return StreamID{}, wrapRejected("entropy source short", "stream", err)
	}
	if id == (StreamID{}) {
		return StreamID{}, newRejected(ReasonAllZero, "stream")
	}
	return id, nil
}

// NewLineageID draws an opaque restore-lineage identity from rand.
func NewLineageID(rand io.Reader) (LineageID, error) {
	var id LineageID
	if _, err := io.ReadFull(rand, id[:]); err != nil {
		return LineageID{}, wrapRejected("entropy source short", "lineage", err)
	}
	if id == (LineageID{}) {
		return LineageID{}, newRejected(ReasonAllZero, "lineage")
	}
	return id, nil
}

// AllocatorName names a reservation component. It is constrained to
// lower-case schema identifiers so this one string-shaped field cannot carry
// user data.
type AllocatorName string

// AllocatorClassification describes how confirmed reservation ceilings
// combine for an allocator.
type AllocatorClassification uint8

const (
	// AllocatorAbsoluteMax is a globally shared sequence whose ceilings merge
	// across streams by maximum.
	AllocatorAbsoluteMax AllocatorClassification = 1
	// AllocatorComponentSum is an independently owned counter whose stream
	// component maxima remain distinct and contribute deltas by sum.
	AllocatorComponentSum AllocatorClassification = 2
)

// ClassifyAllocator returns the accepted reservation classification for a
// known schema allocator. Random draws and unknown names are intentionally
// absent until their separate exclusion contract exists.
func ClassifyAllocator(name AllocatorName) (AllocatorClassification, bool) {
	if _, err := ValidateAllocator(string(name)); err != nil {
		return 0, false
	}
	switch name {
	case "users_id_seq", "files_id_seq", "chats_id_seq", "secret_chats_id_seq",
		"message_fanout_seq", "chat_admin_events_id_seq", "phone_codes_id_seq",
		"registration_invites_id_seq", "language_catalog_publication_audit_id_seq":
		return AllocatorAbsoluteMax, true
	case "update_state_next_local_id", "update_state_pts",
		"channel_state_next_local_id", "channel_state_pts",
		"profile_photo_state_mutation_revision":
		return AllocatorComponentSum, true
	default:
		return 0, false
	}
}

func validateReservationClass(name AllocatorName, want AllocatorClassification, kind Kind) error {
	if _, err := ValidateAllocator(string(name)); err != nil {
		return err
	}
	got, known := ClassifyAllocator(name)
	if !known {
		return newAllocatorNotReady(CauseUnknownAllocator, kind, string(name))
	}
	if got != want {
		return newAllocatorNotReady(CauseAllocatorClassification, kind, string(name))
	}
	return nil
}

// ValidateAllocator checks the schema-identifier syntax and returns the typed
// name. It accepts names such as "users_id_seq" and rejects anything that
// is not a plain identifier of the shape the schema uses.
func ValidateAllocator(name string) (AllocatorName, error) {
	if name == "" || len(name) > MaxAllocatorNameLen {
		return "", newRejected(ReasonOutOfRange, "allocator")
	}
	if !isLowerIdentifier(name) {
		return "", newRejected("allocator name is not a schema identifier", "allocator")
	}
	return AllocatorName(name), nil
}

// isLowerIdentifier accepts [a-z][a-z0-9_]* and nothing else.
func isLowerIdentifier(name string) bool {
	for i := range len(name) {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z':
		case i > 0 && c >= '0' && c <= '9':
		case i > 0 && c == '_':
		default:
			return false
		}
	}
	return name[0] >= 'a' && name[0] <= 'z'
}

// Record is one ledger record: the ordering identity a complete enumeration
// needs, plus a body whose meaning is one-way.
type Record struct {
	Kind Kind

	// Epoch is the restore epoch the record was written under. Epoch 0 is not
	// encodable: every record belongs to an epoch.
	Epoch int64

	// Stream is the opaque replica stream that wrote the record.
	Stream StreamID

	// Seq is the record's position within (Epoch, Stream). It starts at 1, so a
	// gap in a walk is detectable rather than ignorable.
	Seq int64

	// OpKey is the opaque operation identity that makes a retry idempotent.
	OpKey OperationKey

	// Payload is the one-way effect. It must be a supported concrete value, and
	// its Kind must match the record.
	Payload Payload
}

// NewRecord builds a validated record. Validation is not a formality: the
// epoch, stream, sequence and operation key are the completeness and idempotency
// contract, so a zero in any of them is a rejected record, not an empty one.
func NewRecord(kind Kind, epoch int64, stream StreamID, seq int64, key OperationKey, payload Payload) (Record, error) {
	r := Record{Kind: kind, Epoch: epoch, Stream: stream, Seq: seq, OpKey: key, Payload: payload}
	if err := r.validate(); err != nil {
		return Record{}, err
	}
	return r, nil
}

// validateEnvelope checks the ordering and identity fields, the parts that
// mean the same thing for every kind, including a kind this binary cannot
// read. A decoder runs them before it reports an unknown kind as not-ready,
// because epoch, stream, sequence and operation key are the completeness and
// idempotency contract: a zero in one of them is a bad write for every reader,
// not evidence of a newer format.
func (r Record) validateEnvelope() error {
	if r.Epoch < 1 {
		return newRejected(ReasonOutOfRange, "epoch")
	}
	if r.Stream == (StreamID{}) {
		return newRejected(ReasonAllZero, "stream")
	}
	if r.Seq < 1 {
		return newRejected(ReasonOutOfRange, "seq")
	}
	if r.OpKey == (OperationKey{}) {
		return newRejected(ReasonAllZero, "operation_key")
	}
	return nil
}

// validate checks the envelope and the body together.
func (r Record) validate() error {
	if err := r.validateEnvelope(); err != nil {
		return err
	}
	if r.Payload == nil {
		return newRejected("record has no payload", "payload")
	}
	// The codec handles only these exact values. Embedding a payload type can
	// promote its unexported interface methods to an external wrapper, so the
	// interface alone does not guarantee the concrete value is encodable.
	switch r.Payload.(type) {
	case Account, File, MessageCopies, ChannelPostCopies, RandomExclusion,
		Reservation, Epoch, GalleryDelete, ReceiptTerminal, StreamBinding,
		ComponentReservation:
	default:
		return newRejected("record payload type is not supported", "payload")
	}
	if _, known := LookupKind(r.Kind); !known {
		return newRejected("kind is not in the accepted vocabulary", "kind")
	}
	if bodyKind := r.Payload.Kind(); bodyKind != r.Kind {
		return newRejected("payload kind does not match the record kind", "kind")
	}
	return r.Payload.validate()
}
