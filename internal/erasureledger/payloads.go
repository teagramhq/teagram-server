package erasureledger

import (
	"cmp"
	"slices"
)

// canonicalSet sorts a set into the one order this vocabulary encodes, and
// removes repeats. Canonical form is not cosmetic: the same semantic set
// must encode to the same bytes, so a replay that is required to be
// order-independent and idempotent can be checked by byte equality.
func canonicalSet[T any](items []T, less func(a, b T) bool) []T {
	set := slices.Clone(items)
	slices.SortStableFunc(set, func(a, b T) int {
		switch {
		case less(a, b):
			return -1
		case less(b, a):
			return 1
		default:
			return 0
		}
	})
	return slices.CompactFunc(set, func(a, b T) bool { return !less(a, b) && !less(b, a) })
}

// checkCanonicalSet enforces the form Encode requires of a decoded set: at
// least one member, within the bound, and strictly ascending.
func checkCanonicalSet[T any](set []T, field string, less func(a, b T) bool) error {
	if err := checkSetLen(len(set), field); err != nil {
		return err
	}
	for i := 1; i < len(set); i++ {
		switch {
		case !less(set[i-1], set[i]) && !less(set[i], set[i-1]):
			return newRejected(ReasonDuplicate, field)
		case !less(set[i-1], set[i]):
			return newRejected(ReasonNotCanonical, field)
		}
	}
	return nil
}

// Payload is the one-way effect a record carries. The interface is sealed
// to this package by payloadSeam: the vocabulary is a closed set that grows
// only by a deliberate change, never by a caller inventing a body type.
type Payload interface {
	// Kind reports which kind this body encodes as.
	Kind() Kind
	// validate checks the body's own identifiers and canonical form.
	validate() error
	// payloadSeam keeps third-party payload types impossible.
	payloadSeam()
}

// positiveID is the shared rule for every identifier a body may name: a real
// schema id is positive, so zero is never a valid identity and is therefore
// free to mean "absent" where a body needs to say so.
func positiveID(v int64, field string) error {
	if v < 1 {
		return newRejected(ReasonOutOfRange, field)
	}
	return nil
}

// positiveLocalID bounds a message id to the signed-32-bit wire width.
func positiveLocalID(v int32, field string) error {
	if v < 1 || int64(v) > maxLocalID {
		return newRejected(ReasonOutOfRange, field)
	}
	return nil
}

// Account tombstones one account identity.
type Account struct {
	UserID int64
}

// Kind reports KindAccount.
func (Account) Kind() Kind { return KindAccount }

// NewAccount builds an account tombstone record body.
func NewAccount(userID int64) (Account, error) {
	a := Account{UserID: userID}
	if err := a.validate(); err != nil {
		return Account{}, err
	}
	return a, nil
}

func (a Account) validate() error { return positiveID(a.UserID, "user_id") }

func (Account) payloadSeam() {}

// File tombstones one file identity. The row is tombstoned, not deleted, so
// restored references keep failing closed.
type File struct {
	FileID int64
}

// Kind reports KindFile.
func (File) Kind() Kind { return KindFile }

// NewFile builds a file tombstone record body.
func NewFile(fileID int64) (File, error) {
	f := File{FileID: fileID}
	if err := f.validate(); err != nil {
		return File{}, err
	}
	return f, nil
}

func (f File) validate() error { return positiveID(f.FileID, "file_id") }

func (File) payloadSeam() {}

// MessageCopy names one account's copy of one message: the owner whose
// per-owner id space carries it, and the local id within that owner.
type MessageCopy struct {
	OwnerID int64
	LocalID int32
}

// MessageCopies is the exact set of message copies a committing
// transaction deleted. A self-delete carries the caller's copy only; a
// peer revoke carries both; a group revoke carries the members present at
// commit and omits a removed member's frozen copy. There is no field here
// that can express a revoke, an "all copies" scope, or undeleting a copy, so
// replay cannot widen the scope the live authorization granted.
type MessageCopies struct {
	Copies []MessageCopy
}

// Kind reports KindMessageCopies.
func (MessageCopies) Kind() Kind { return KindMessageCopies }

// NewSelfDelete builds the body for a delete-for-self: exactly the
// caller's copy, and structurally nothing else.
func NewSelfDelete(ownerID int64, localID int32) (MessageCopies, error) {
	return NewMessageCopies(MessageCopy{OwnerID: ownerID, LocalID: localID})
}

// NewMessageCopies canonicalizes copies into the record's set: sorted by
// (owner, local) and de-duplicated. Two callers that deleted the same set in
// different orders therefore produce byte-identical records, which is what a
// replay that must be order-independent and idempotent needs.
func NewMessageCopies(copies ...MessageCopy) (MessageCopies, error) {
	m := MessageCopies{Copies: canonicalSet(copies, copyLess)}
	if err := m.validate(); err != nil {
		return MessageCopies{}, err
	}
	return m, nil
}

func (m MessageCopies) validate() error {
	if err := checkCanonicalSet(m.Copies, "copies", copyLess); err != nil {
		return err
	}
	for _, c := range m.Copies {
		if err := positiveID(c.OwnerID, "owner_id"); err != nil {
			return err
		}
		if err := positiveLocalID(c.LocalID, "local_id"); err != nil {
			return err
		}
	}
	return nil
}

// copyLess orders copies by owner, then by the local id inside that owner.
func copyLess(a, b MessageCopy) bool {
	return cmp.Or(cmp.Compare(a.OwnerID, b.OwnerID), cmp.Compare(a.LocalID, b.LocalID)) < 0
}

func (MessageCopies) payloadSeam() {}

// ChannelPost names one channel's copy of one post in the channel's own
// local id space.
type ChannelPost struct {
	ChannelID int64
	LocalID   int32
}

// ChannelPostCopies is the exact set of channel post copies a committing
// transaction deleted. Channel posts are stored separately from account
// message copies, so they are a kind of their own rather than a widening of
// MessageCopies.
type ChannelPostCopies struct {
	Posts []ChannelPost
}

// Kind reports KindChannelPosts.
func (ChannelPostCopies) Kind() Kind { return KindChannelPosts }

// NewChannelPostCopies canonicalizes posts into the record's set.
func NewChannelPostCopies(posts ...ChannelPost) (ChannelPostCopies, error) {
	c := ChannelPostCopies{Posts: canonicalSet(posts, postLess)}
	if err := c.validate(); err != nil {
		return ChannelPostCopies{}, err
	}
	return c, nil
}

func (c ChannelPostCopies) validate() error {
	if err := checkCanonicalSet(c.Posts, "posts", postLess); err != nil {
		return err
	}
	for _, p := range c.Posts {
		if err := positiveID(p.ChannelID, "channel_id"); err != nil {
			return err
		}
		if err := positiveLocalID(p.LocalID, "local_id"); err != nil {
			return err
		}
	}
	return nil
}

// postLess orders posts by channel, then by local id in that channel's space.
func postLess(a, b ChannelPost) bool {
	return cmp.Or(cmp.Compare(a.ChannelID, b.ChannelID), cmp.Compare(a.LocalID, b.LocalID)) < 0
}

func (ChannelPostCopies) payloadSeam() {}

// RandomClass is the random-id allocator class an exclusion names. Sequence
// allocators are covered by Reservation, so only genuinely drawn ids
// need a denylist.
type RandomClass uint8

const (
	// RandomClassChannel is the random channels.id space.
	RandomClassChannel RandomClass = 1
	// RandomClassPoll is the random polls.id space.
	RandomClassPoll RandomClass = 2
)

// Valid reports whether class names a random allocator this
// vocabulary covers.
func (class RandomClass) Valid() bool {
	return class == RandomClassChannel || class == RandomClassPoll
}

// RandomExclusion adds drawn ids to the permanent no-reuse
// exclusion set for one class. Membership only grows: a record can exclude an
// id, and no record can re-issue one. Replaying the set after a restore
// makes a draw reject every id the live system handed out in the loss window.
type RandomExclusion struct {
	Class RandomClass
	IDs   []int64
}

// Kind reports KindRandomExclusion.
func (RandomExclusion) Kind() Kind { return KindRandomExclusion }

// NewRandomExclusion canonicalizes ids into the exclusion set.
func NewRandomExclusion(class RandomClass, ids ...int64) (RandomExclusion, error) {
	r := RandomExclusion{Class: class, IDs: canonicalSet(ids, func(a, b int64) bool { return a < b })}
	if err := r.validate(); err != nil {
		return RandomExclusion{}, err
	}
	return r, nil
}

func (r RandomExclusion) validate() error {
	if !r.Class.Valid() {
		return newRejected("random allocator class is unknown", "class")
	}
	if err := checkCanonicalSet(r.IDs, "ids", func(a, b int64) bool { return a < b }); err != nil {
		return err
	}
	for _, id := range r.IDs {
		if err := positiveID(id, "id"); err != nil {
			return err
		}
	}
	return nil
}

func (RandomExclusion) payloadSeam() {}

// Reservation records an absolute global sequence ceiling. A restore takes
// the highest ceiling across streams, so a lower ceiling is stale, never an
// instruction to roll an allocator back. Per-scope counters use
// ComponentReservation because this scalar form cannot identify independent
// lineage and stream components.
type Reservation struct {
	Allocator AllocatorName
	Ceiling   int64
}

// Kind reports KindReservation.
func (Reservation) Kind() Kind { return KindReservation }

// NewReservation builds a scalar reservation for an absolute-max allocator.
func NewReservation(allocator AllocatorName, ceiling int64) (Reservation, error) {
	r := Reservation{Allocator: allocator, Ceiling: ceiling}
	if err := r.validate(); err != nil {
		return Reservation{}, err
	}
	return r, nil
}

func (r Reservation) validate() error {
	if err := validateReservationClass(r.Allocator, AllocatorAbsoluteMax, KindReservation); err != nil {
		return err
	}
	return positiveID(r.Ceiling, "ceiling")
}

func (Reservation) payloadSeam() {}

// StreamBinding binds the record envelope's (epoch, stream) to one restore
// lineage. A reader adds the binding only after its record is confirmed; a
// conflicting later binding makes the stream unusable and never replaces the
// first lineage.
type StreamBinding struct {
	Lineage LineageID
}

// Kind reports KindStreamBinding.
func (StreamBinding) Kind() Kind { return KindStreamBinding }

// NewStreamBinding builds an immutable stream-to-lineage binding body.
func NewStreamBinding(lineage LineageID) (StreamBinding, error) {
	b := StreamBinding{Lineage: lineage}
	if err := b.validate(); err != nil {
		return StreamBinding{}, err
	}
	return b, nil
}

func (b StreamBinding) validate() error {
	if b.Lineage == (LineageID{}) {
		return newRejected(ReasonAllZero, "lineage")
	}
	return nil
}

func (StreamBinding) payloadSeam() {}

// ComponentReservation records a confirmed ceiling and the restored baseline
// for one independently owned component. Its record envelope supplies the
// stream; a confirmed StreamBinding supplies the lineage. Both identities are
// required to form the component key, so equal allocator names on two streams
// stay distinct.
type ComponentReservation struct {
	Allocator AllocatorName
	Baseline  int64
	Ceiling   int64
}

// Kind reports KindComponentReservation.
func (ComponentReservation) Kind() Kind { return KindComponentReservation }

// NewComponentReservation builds a component-sum reservation with its
// restored baseline and confirmed ceiling.
func NewComponentReservation(allocator AllocatorName, baseline, ceiling int64) (ComponentReservation, error) {
	r := ComponentReservation{Allocator: allocator, Baseline: baseline, Ceiling: ceiling}
	if err := r.validate(); err != nil {
		return ComponentReservation{}, err
	}
	return r, nil
}

func (r ComponentReservation) validate() error {
	if err := validateReservationClass(r.Allocator, AllocatorComponentSum, KindComponentReservation); err != nil {
		return err
	}
	if r.Baseline < 0 {
		return newRejected(ReasonOutOfRange, "baseline")
	}
	if err := positiveID(r.Ceiling, "ceiling"); err != nil {
		return err
	}
	if r.Baseline > r.Ceiling {
		return newRejected("baseline exceeds ceiling", "baseline")
	}
	return nil
}

func (ComponentReservation) payloadSeam() {}

// ComponentKey is the identity used to keep component ceilings separate:
// restore lineage, envelope stream and allocator/shard name.
type ComponentKey struct {
	Lineage   LineageID
	Stream    StreamID
	Allocator AllocatorName
}

// NewComponentKey builds a validated identity for one component.
func NewComponentKey(lineage LineageID, stream StreamID, allocator AllocatorName) (ComponentKey, error) {
	key := ComponentKey{Lineage: lineage, Stream: stream, Allocator: allocator}
	if err := key.validate(); err != nil {
		return ComponentKey{}, err
	}
	return key, nil
}

func (key ComponentKey) validate() error {
	if key.Lineage == (LineageID{}) {
		return newRejected(ReasonAllZero, "lineage")
	}
	if key.Stream == (StreamID{}) {
		return newRejected(ReasonAllZero, "stream")
	}
	return validateReservationClass(key.Allocator, AllocatorComponentSum, KindComponentReservation)
}

// ComponentOrigin identifies whether a restored component belongs to the
// lineage being opened or is inherited from an earlier snapshot.
type ComponentOrigin uint8

const (
	// ComponentFresh starts at a zero baseline and may receive confirmed
	// reservations in its lineage.
	ComponentFresh ComponentOrigin = 1
	// ComponentInherited is carried forward as anti-reuse evidence but is closed
	// to further reservations.
	ComponentInherited ComponentOrigin = 2
)

// ComponentBaseline is the restored G value and ownership state for one
// component. It is supplied by a future synthetic reader from the restored
// snapshot; it does not claim that a deployed G or restore path exists.
type ComponentBaseline struct {
	Key    ComponentKey
	Value  int64
	Origin ComponentOrigin
}

// NewComponentBaseline builds an explicit restored component baseline.
func NewComponentBaseline(key ComponentKey, value int64, origin ComponentOrigin) (ComponentBaseline, error) {
	b := ComponentBaseline{Key: key, Value: value, Origin: origin}
	if err := b.validate(); err != nil {
		return ComponentBaseline{}, err
	}
	return b, nil
}

func (b ComponentBaseline) validate() error {
	if err := b.Key.validate(); err != nil {
		return err
	}
	if b.Value < 0 {
		return newRejected(ReasonOutOfRange, "baseline")
	}
	switch b.Origin {
	case ComponentFresh:
		if b.Value != 0 {
			return newRejected("fresh component baseline must be zero", "baseline")
		}
	case ComponentInherited:
	default:
		return newRejected("component origin is unknown", "origin")
	}
	return nil
}

// CanReserve reports whether this baseline represents a fresh component that
// is open to confirmed capacity. Inherited components remain closed.
func (b ComponentBaseline) CanReserve() bool {
	return b.validate() == nil && b.Origin == ComponentFresh
}

// EpochLevel is the monotone position of an epoch within one lineage.
// The ordering is the meaning: established precedes completed, and no value
// means "un-complete" or "return to an older epoch".
type EpochLevel uint8

const (
	// EpochEstablished fences the previous epoch's writer and opens a new
	// epoch for this lineage.
	EpochEstablished EpochLevel = 1
	// EpochCompleted is the durable evidence that this lineage finished
	// replay for this epoch. It is the fact that lets a later restore skip
	// records already applied by this lineage.
	EpochCompleted EpochLevel = 2
)

// Valid reports whether level is one of the accepted monotone
// positions.
func (level EpochLevel) Valid() bool {
	return level == EpochEstablished || level == EpochCompleted
}

// Epoch carries an epoch number, the lineage it belongs to, and how far that
// lineage has got. The lineage id is not decoration: MAIN-1362 requires
// completed-lineage evidence before epoch skipping, so sharing an epoch number
// with a completed epoch of a different lineage must not count as completion.
type Epoch struct {
	Number  int64
	Lineage LineageID
	Level   EpochLevel
}

// Kind reports KindEpoch.
func (Epoch) Kind() Kind { return KindEpoch }

// NewEpoch builds an epoch body.
func NewEpoch(number int64, lineage LineageID, level EpochLevel) (Epoch, error) {
	e := Epoch{Number: number, Lineage: lineage, Level: level}
	if err := e.validate(); err != nil {
		return Epoch{}, err
	}
	return e, nil
}

func (e Epoch) validate() error {
	if e.Number < 1 {
		return newRejected(ReasonOutOfRange, "epoch_number")
	}
	if e.Lineage == (LineageID{}) {
		return newRejected(ReasonAllZero, "lineage")
	}
	if !e.Level.Valid() {
		return newRejected("epoch level is unknown", "level")
	}
	return nil
}

func (Epoch) payloadSeam() {}

// GalleryEntry names one deleted profile-gallery entry by both of the
// identifiers a restore needs: the file id, which is half of the gallery
// primary key and the target of the current-selection pointer, and the
// client upload id, which is the identity of the upload receipt that has to
// end up terminal.
type GalleryEntry struct {
	FileID       int64
	ClientFileID int64
}

// GalleryDelete records the exact gallery entries one owner's clear deleted,
// plus the per-owner mutation revision it cleared at. It keeps
// the exact tuples: a clear of B removes B and promotes nothing, so a record
// has no field naming a replacement, no field naming the current
// selection, and no field that can add a gallery entry back. The revision
// is the maximum the clear reached: a restored counter above it is newer, and
// the replay rule (drop the named tuples, null any pointer to them, clear when
// the restored revision is not above this one) is a monotone one.
type GalleryDelete struct {
	OwnerID  int64
	Entries  []GalleryEntry
	Revision int64
}

// Kind reports KindGalleryDelete.
func (GalleryDelete) Kind() Kind { return KindGalleryDelete }

// NewGalleryDelete canonicalizes entries into the record's set.
func NewGalleryDelete(ownerID int64, revision int64, entries ...GalleryEntry) (GalleryDelete, error) {
	g := GalleryDelete{OwnerID: ownerID, Entries: canonicalSet(entries, entryLess), Revision: revision}
	if err := g.validate(); err != nil {
		return GalleryDelete{}, err
	}
	return g, nil
}

func (g GalleryDelete) validate() error {
	if err := positiveID(g.OwnerID, "owner_id"); err != nil {
		return err
	}
	if g.Revision < 0 {
		return newRejected(ReasonOutOfRange, "revision")
	}
	if err := checkCanonicalSet(g.Entries, "entries", entryLess); err != nil {
		return err
	}
	for _, e := range g.Entries {
		if err := positiveID(e.FileID, "file_id"); err != nil {
			return err
		}
		if err := positiveID(e.ClientFileID, "client_file_id"); err != nil {
			return err
		}
	}
	return nil
}

// entryLess orders gallery entries by file id, then by client upload id.
func entryLess(a, b GalleryEntry) bool {
	return cmp.Or(cmp.Compare(a.FileID, b.FileID), cmp.Compare(a.ClientFileID, b.ClientFileID)) < 0
}

func (GalleryDelete) payloadSeam() {}

// NoFileID is the value ReceiptTerminal carries when the receipt has no
// file reference at all, matching the nullable file_id of the durable
// receipt row. Zero is available for that meaning because a real id is
// positive.
const NoFileID int64 = 0

// ReceiptState is the terminal position of a profile upload receipt.
// The ordering is the meaning: complete precedes deleted, and no value means
// pending, live, or restored.
type ReceiptState uint8

const (
	// ReceiptComplete is a finished upload: a retry under the same client id
	// must not allocate a second file id or charge quota twice.
	ReceiptComplete ReceiptState = 1
	// ReceiptDeleted is the terminal state after the owner deleted the photo.
	// A retry under that client id gets the uniform unavailable error, so the
	// dedup key outlives the gallery entry it referred to.
	ReceiptDeleted ReceiptState = 2
)

// Terminal reports whether state is one of the accepted terminal positions.
// A non-terminal state is not encodable: the ledger records acknowledged
// outcomes, and the live receipt row is the record of a pending upload.
func (state ReceiptState) Terminal() bool {
	return state == ReceiptComplete || state == ReceiptDeleted
}

// ReceiptTerminal is the upsert identity for one owner's upload
// receipt in a terminal state. It is exactly the durable row's primary key
// (owner, client upload id) plus the terminal state and, when one existed, the
// file id. It carries no request size, part count, payload digest, media mode,
// timestamp, transport identity, or content of any kind: a replayer can create
// the terminal receipt row for a receipt the restored database never held from
// these identifiers alone, and a digest of deleted photo bytes has no
// reason to travel off-alpha.
type ReceiptTerminal struct {
	OwnerID      int64
	ClientFileID int64
	State        ReceiptState
	FileID       int64
}

// Kind reports KindReceiptTerminal.
func (ReceiptTerminal) Kind() Kind { return KindReceiptTerminal }

// NewReceiptDeleted builds the terminal receipt for a photo the owner deleted,
// with or without a surviving file reference.
func NewReceiptDeleted(ownerID, clientFileID, fileID int64) (ReceiptTerminal, error) {
	return NewReceiptTerminal(ownerID, clientFileID, ReceiptDeleted, fileID)
}

// NewReceiptTerminal builds a terminal receipt body. Pass NoFileID when the
// receipt names no file.
func NewReceiptTerminal(ownerID, clientFileID int64, state ReceiptState, fileID int64) (ReceiptTerminal, error) {
	r := ReceiptTerminal{
		OwnerID:      ownerID,
		ClientFileID: clientFileID,
		State:        state,
		FileID:       fileID,
	}
	if err := r.validate(); err != nil {
		return ReceiptTerminal{}, err
	}
	return r, nil
}

func (r ReceiptTerminal) validate() error {
	if err := positiveID(r.OwnerID, "owner_id"); err != nil {
		return err
	}
	if err := positiveID(r.ClientFileID, "client_file_id"); err != nil {
		return err
	}
	if !r.State.Terminal() {
		return newRejected("receipt state is not terminal", "state")
	}
	if r.FileID != NoFileID {
		return positiveID(r.FileID, "file_id")
	}
	return nil
}

func (ReceiptTerminal) payloadSeam() {}

// checkSetLen enforces the per-record set bound and the rule that a record
// which names a set must name at least one member: an empty set is not a
// monotone step, and accepting it would let a writer publish a record that
// means nothing while consuming sequence space.
func checkSetLen(n int, field string) error {
	if n < 1 {
		return newRejected(ReasonNotCanonical, field)
	}
	if n > MaxSetMembers {
		return newRejected(ReasonTooLarge, field)
	}
	return nil
}
