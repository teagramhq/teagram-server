package erasureledger_test

// The files in this provider_* family are the synthetic ledger provider
// harness for MAIN-1452: the capability surface the accepted MAIN-1360 model
// (as amended on MAIN-1362) specifies for the off-alpha erasure ledger, built
// as a test-only seam with two storage arms, an in-memory map and a disposable
// local directory.
//
// What the suite pins is the shape of the capability contract: a writer that
// can only create-once, a replayer that can only get and list, a verifier that
// can only read attributes and arrival records, an expirer that is the only
// principal able to delete, a paginated walk whose completeness is provable,
// serial per-stream sequencing, an arrival high-water cross-check, and a fenced
// old-epoch write that lands in quarantine.
//
// What it does not pin, and never claims, is any real provider property. There
// is no S3, RustFS, MinIO, or filesystem-provider client here, no credential,
// no bucket, no Object Lock, no versioning, no retention, and no provider
// consistency guarantee. The arrival evidence below is a reading from a
// simulated clock, and the checksum is a crc32 over the stored bytes: enough
// for the conformance suite to assert that a receipt describes the stored
// object rather than the writer's claim, and nothing more. Passing this
// suite establishes no provider capability, no restore readiness, and no
// deployment evidence.

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/teagramhq/teagram-server/internal/erasureledger"
)

// The four accepted ledger principals plus the separately authorized sentinel
// authorizer. These interfaces are the seam's least-privilege vocabulary: the
// conformance suite asserts that each principal satisfies exactly its own
// interface and none of the others, so a capability cannot reach a credential
// by adding a method.

// ledgerCreator is the writer's whole capability: a conditional create, and the
// arrival confirmation of the object that create just stored.
type ledgerCreator interface {
	Create(rec erasureledger.Record) (*pendingWrite, error)
	Confirm(*pendingWrite) (*createReceipt, error)
}

// ledgerReader is the replayer's capability: read one record by opaque key, and
// list opaque keys a page at a time. It is the escrow-side pair from MAIN-1360.
type ledgerReader interface {
	Get(erasureledger.OperationKey) (replayObject, error)
	List(after string, pageSize int) (listPage, error)
}

// ledgerArrivalReader is the verifier's capability: provider attributes for one
// key, and the independent per-stream arrival record used for the dead-man
// check and the high-water cross-check.
type ledgerArrivalReader interface {
	Attributes(erasureledger.OperationKey) (objectAttributes, error)
	Arrival(epoch int64, stream erasureledger.StreamID) (arrivalRecord, error)
}

// ledgerDeleter is the expirer's capability: the pruned-through checkpoint, and
// the delete that may only follow it. It is the only delete right in the seam.
type ledgerDeleter interface {
	PruneThrough(epoch int64, stream erasureledger.StreamID, through int64) error
	Delete(erasureledger.OperationKey) error
}

// ledgerSentinelCreator is the logical-sentinel capability. MAIN-1362 accepts
// the sentinel as a separately authorized logical marker, so this seam gives it
// its own principal: none of the four ledger credentials above can obtain it,
// and the replayer in particular cannot write the marker that is meant to
// vouch for its own listing.
type ledgerSentinelCreator interface {
	CreateSentinel() (erasureledger.OperationKey, error)
}

// Compile-time proof that each principal offers the capability it is authorized
// to offer. The negative direction, that no principal carries a capability
// outside its own interface, is a runtime check in
// TestLedgerPrincipalCapabilitiesAreLeastPrivileged: an interface
// assertion on a value cannot be expressed as a declaration.
var (
	_ ledgerCreator         = writer{}
	_ ledgerReader          = replayer{}
	_ ledgerArrivalReader   = verifier{}
	_ ledgerDeleter         = expirer{}
	_ ledgerSentinelCreator = sentinelAuthorizer{}
)

// Seam errors. They are typed causes, not strings, so a conformance case
// asserts on the refusal it expects.
var (
	// errObjectExists is the conditional-create refusal: the key is taken, so
	// a create cannot overwrite it.
	errObjectExists = errors.New("synthledger: object exists")
	// errNotFound is the read result for a key with no replayable object. An
	// absent key, a write that is not confirmed yet, and a quarantined write
	// are indistinguishable from the replayer's seat, which is the point.
	errNotFound = errors.New("synthledger: no replayable object")
	// errQuarantined is the provider's answer to a write on a fenced stream:
	// the bytes were kept as evidence, and they did not enter the ledger.
	errQuarantined = errors.New("synthledger: write quarantined")
	// errSequenceGap is the serial-flusher rule: sequence n+1 is refused while
	// n is unconfirmed.
	errSequenceGap = errors.New("synthledger: predecessor unconfirmed")
	// errAlreadyConfirmed means an arrival was confirmed once, so a second
	// confirmation of the same write is not a retry and is not a
	// way to re-open a published record.
	errAlreadyConfirmed = errors.New("synthledger: arrival already confirmed")
	// errCredential is the refusal of a write whose envelope does not match the
	// writer credential that made it.
	errCredential = errors.New("synthledger: credential does not cover this write")
	// errInjectedPage is the conformance suite's injected listing failure.
	errInjectedPage = errors.New("synthledger: injected list failure")
	// errNotPruned is the expirer's ordering rule: no delete without a
	// pruned-through checkpoint covering the record's stream.
	errNotPruned = errors.New("synthledger: pruned-through checkpoint missing")
	// errNotDeletable is the expirer's guard on the records that keep
	// contiguity checkable: the newest reservation per allocator, the newest
	// epoch record per lineage, and a channel-id exclusion inside retention.
	errNotDeletable = errors.New("synthledger: record is not deletable")
	// errInjectedVerifier is the arrival-log tamper marker.
	errInjectedVerifier = errors.New("synthledger: injected verifier entry")
)

// arrivalEvidence is the provider's own account of an arrival. Nothing in it
// comes from the writer: the record has no timestamp field, the create call has
// no clock argument, and the clock and arrival counter are the provider's
// simulated state. The checksum is a crc32 of the stored bytes: a simulated
// integrity marker over what the provider kept, not a MAC, not a signature, and
// not a provider integrity proof.
type arrivalEvidence struct {
	clockNanos int64
	arrivalSeq int64
	checksum   uint32
}

// simTickNanos is how far the simulated provider clock advances per arrival.
// The spacing is arbitrary; the strict monotonicity is what the cases assert.
const simTickNanos = 1_000_000

// objectState is where one write stands in the seam.
type objectState uint8

const (
	// statePending means the object is stored and the provider has read its
	// arrival, but the writer has not confirmed that arrival: the record is not
	// in the ledger, is not listable, is not readable, and its sequence is not
	// confirmed, so the next sequence is refused.
	statePending objectState = iota
	// stateConfirmed means the arrival is confirmed: the record is replayable.
	stateConfirmed
)

// object is one stored ledger object: the encoded record bytes plus the
// provider's account of them.
type object struct {
	name    string
	key     erasureledger.OperationKey
	body    []byte
	epoch   int64
	stream  erasureledger.StreamID
	seq     int64
	arrival arrivalEvidence
	state   objectState
	// keep names the provider's keep class while this object is the one the
	// expirer must not delete. keepClass derives it from the record's kind
	// plus one identifying body field, the allocator name, the lineage id, or
	// the random class, and never from a set member.
	keep string
}

// streamRec is the provider's per-(epoch, stream) state: the serial flusher's
// position, the arrival count, the pruned-through checkpoint, and the
// fence. The fence carries its own presence flag: fencing a stream that has
// never written is a real fence, and a zero sequence alone cannot say so, so a
// fence at sequence zero fences that stream's first write too.
type streamRec struct {
	pendingSeq   int64
	confirmedSeq int64
	arrivalCount int64
	pruned       int64
	fenced       bool
	fencedSeq    int64
}

// highWater is the highest confirmed sequence on the stream.
func (r streamRec) highWater() int64 { return r.confirmedSeq }

// streamKey is the provider's per-stream key. Sequences are contiguous
// per (epoch, replica stream), and nothing in the seam keeps a global sequence
// that a writer can address.
type streamKey struct {
	epoch  int64
	stream erasureledger.StreamID
}

// store is the storage arm under the seam. The memory arm answers these from
// maps; the disposable-directory arm answers them from files, with the
// conditional create coming from the filesystem's exclusive create.
type store interface {
	providerState() (clock, arrivals int64)
	saveProviderState(clock, arrivals int64)

	streamState(sk streamKey) (streamRec, bool)
	saveStreamState(sk streamKey, rec streamRec)

	// streams is the ledger's own per-(epoch, stream) inventory, read from
	// the medium: it is what the verifier's arrival records are read for, so
	// the expected stream set never comes from what a listing showed.
	streams() []streamKey

	getObject(name string) *object
	saveObjectOnce(obj *object) error
	saveObject(obj *object)
	deleteObject(name string)

	// page returns up to limit objects whose names sort strictly after
	// after, in name order, plus the greatest name the store holds, so the
	// caller can tell a last page from a page with more behind it.
	page(after string, limit int) (objs []*object, last string)

	quarantineObject(obj *object)
	quarantinedObjects() []*object

	writeMarker(name string, obj *object) bool
	readMarker(name string) *object

	keepMarker(class string) string
	saveKeepMarker(class, name string)
}

// synthProvider is the synthetic ledger provider: the shared capability,
// sequencing, evidence and fence logic, over one storage arm. The conformance
// suite runs this identical logic over both arms, so what differs between them
// is the medium, and every capability assertion below holds for both.
type synthProvider struct {
	t      *testing.T
	s      store
	clock  int64
	arrive int64
	pages  int
	faults pageFaults
	verifs []verifierEntry
	// hideMarkers is the conformance suite's marker fault: the store keeps a
	// sentinel the replayer can no longer read.
	hideMarkers bool
}

// pageFaults are the conformance suite's injected listing faults.
type pageFaults struct {
	failPage     int
	dropPage     int
	truncatePage int
}

// verifierEntry is one line in the verifier's independent arrival log.
type verifierEntry struct {
	epoch  int64
	stream erasureledger.StreamID
	count  int64
	high   int64
	tamper bool
}

func newSynthProvider(t *testing.T, s store) *synthProvider {
	t.Helper()
	clock, arrivals := s.providerState()
	return &synthProvider{t: t, s: s, clock: clock, arrive: arrivals}
}

// arriveAt advances the simulated provider clock, mints the next provider
// arrival sequence, and computes the checksum over the bytes being stored.
func (p *synthProvider) arriveAt(body []byte) arrivalEvidence {
	p.clock += simTickNanos
	p.arrive++
	p.s.saveProviderState(p.clock, p.arrive)
	return arrivalEvidence{
		clockNanos: p.clock,
		arrivalSeq: p.arrive,
		checksum:   crc32.ChecksumIEEE(body),
	}
}

// verify is the conformance suite's integrity check on the seam's own
// evidence: the receipt's checksum describes the bytes the provider kept.
func (a arrivalEvidence) verify(body []byte) bool {
	return a.checksum == crc32.ChecksumIEEE(body)
}

// newWriter mints a writer credential for one (epoch, replica stream). The
// model issues a writer credential per epoch with a stream prefix and revokes
// it at the fence, so the credential is bound: it cannot address another
// stream, and it cannot write past its fence.
func (p *synthProvider) newWriter(epoch int64, stream erasureledger.StreamID) writer {
	return writer{p: p, epoch: epoch, stream: stream}
}

// fenceStream marks a stream fenced for conformance purposes. It is a
// bookkeeping marker in the seam, not a credential revocation, and it
// establishes no revocation capability.
func (p *synthProvider) fenceStream(epoch int64, stream erasureledger.StreamID) {
	sk := streamKey{epoch: epoch, stream: stream}
	rec, ok := p.s.streamState(sk)
	if !ok {
		rec = streamRec{}
	}
	rec.fenced = true
	rec.fencedSeq = max(rec.confirmedSeq, rec.pendingSeq, rec.highWater())
	p.s.saveStreamState(sk, rec)
}

// create is the conditional create. The writer's envelope must match its
// credential, the sequence must be the next one on the stream, and no
// unconfirmed predecessor may be outstanding.
func (p *synthProvider) create(w writer, rec erasureledger.Record) (*pendingWrite, error) {
	if rec.Epoch != w.epoch || rec.Stream != w.stream {
		return nil, fmt.Errorf("%w: envelope epoch and stream are not the credential's", errCredential)
	}
	if _, known := erasureledger.LookupKind(rec.Kind); !known {
		return nil, fmt.Errorf("%w: kind %v is outside the accepted vocabulary", errCredential, rec.Kind)
	}
	body, err := erasureledger.Encode(rec)
	if err != nil {
		return nil, fmt.Errorf("%w: record does not encode: %w", errCredential, err)
	}
	name := keyName(rec.OpKey)
	if prev := p.s.getObject(name); prev != nil {
		return nil, fmt.Errorf("%w: key %s was created at arrival %d with a %d byte body",
			errObjectExists, name, prev.arrival.arrivalSeq, len(prev.body))
	}
	sk := streamKey{epoch: w.epoch, stream: w.stream}
	str, ok := p.s.streamState(sk)
	if !ok {
		str = streamRec{}
	}
	if str.fenced && rec.Seq > str.fencedSeq {
		// The bytes go to the quarantine arm, which no listing and no read
		// reaches, and the stream's sequence does not move: a fenced write
		// cannot spend a sequence a walk has to account for.
		obj := &object{
			name: name, key: rec.OpKey, body: body,
			epoch: rec.Epoch, stream: rec.Stream, seq: rec.Seq,
			arrival: p.arriveAt(body), state: stateConfirmed,
		}
		p.s.quarantineObject(obj)
		return nil, fmt.Errorf("%w: stream %s is fenced at sequence %d",
			errQuarantined, name, str.fencedSeq)
	}
	if str.pendingSeq != 0 && str.pendingSeq == rec.Seq {
		return nil, fmt.Errorf("%w: sequence %d is stored and unconfirmed", errSequenceGap, rec.Seq)
	}
	if rec.Seq != str.confirmedSeq+1 {
		return nil, fmt.Errorf("%w: stream has confirmed %d, so %d is not the next sequence",
			errSequenceGap, str.confirmedSeq, rec.Seq)
	}
	obj := &object{
		name: name, key: rec.OpKey, body: body,
		epoch: rec.Epoch, stream: rec.Stream, seq: rec.Seq,
		arrival: p.arriveAt(body), state: statePending,
	}
	if err := p.s.saveObjectOnce(obj); err != nil {
		return nil, err
	}
	str.pendingSeq = rec.Seq
	p.s.saveStreamState(sk, str)
	return &pendingWrite{
		epoch: w.epoch, stream: w.stream,
		key: rec.OpKey, seq: rec.Seq, arrival: obj.arrival,
	}, nil
}

// confirm publishes an arrival: the provider's arrival evidence for the object
// create just stored is accepted, the record becomes replayable, and only now
// does the stream's confirmed sequence move, so the next sequence becomes
// writable. The handle is confirm's only input, so confirm carries no read
// capability: it cannot name a key, a stream, or any other object. The handle
// is bound to the credential that Create issued it to and the binding is
// checked before the provider reads anything, so one stream's credential cannot
// publish another stream's record.
func (p *synthProvider) confirm(w writer, h *pendingWrite) (*createReceipt, error) {
	if h.epoch != w.epoch || h.stream != w.stream {
		return nil, fmt.Errorf("%w: the handle was issued to another stream credential", errCredential)
	}
	obj := p.s.getObject(keyName(h.key))
	if obj == nil {
		return nil, fmt.Errorf("%w: no stored object behind this handle", errNotFound)
	}
	if obj.epoch != w.epoch || obj.stream != w.stream {
		return nil, fmt.Errorf("%w: the stored record is not on the credential's stream", errCredential)
	}
	if obj.state == stateConfirmed {
		return nil, fmt.Errorf("%w: sequence %d on stream %s",
			errAlreadyConfirmed, obj.seq, keyName(obj.stream))
	}
	sk := streamKey{epoch: obj.epoch, stream: obj.stream}
	str, ok := p.s.streamState(sk)
	if !ok || str.pendingSeq != obj.seq {
		return nil, fmt.Errorf("%w: sequence %d is not the stream's pending write",
			errSequenceGap, obj.seq)
	}
	if !obj.arrival.verify(obj.body) {
		return nil, errors.New("synthledger: arrival evidence does not describe the stored bytes")
	}
	rec, err := erasureledger.Decode(obj.body)
	if err != nil {
		return nil, fmt.Errorf("synthledger: stored body does not decode: %w", err)
	}
	obj.state = stateConfirmed
	obj.keep = keepClass(rec)
	p.moveKeepMarker(obj)
	p.s.saveObject(obj)

	str.confirmedSeq = obj.seq
	str.pendingSeq = 0
	str.arrivalCount++
	p.s.saveStreamState(sk, str)
	p.verifs = append(p.verifs, verifierEntry{
		epoch: obj.epoch, stream: obj.stream,
		count: str.arrivalCount, high: str.confirmedSeq,
	})
	return &createReceipt{
		Key: obj.key, Arrival: obj.arrival, Seq: obj.seq, Bytes: len(obj.body),
	}, nil
}

// moveKeepMarker hands the keep class to the object that now holds it, so the
// expirer's guard follows the newest reservation and the newest epoch
// record rather than the first one written.
func (p *synthProvider) moveKeepMarker(obj *object) {
	if obj.keep == "" || !movedKeep(obj.keep) {
		return
	}
	if prev := p.s.keepMarker(obj.keep); prev != "" && prev != obj.name {
		if po := p.s.getObject(prev); po != nil {
			po.keep = ""
			p.s.saveObject(po)
		}
	}
	p.s.saveKeepMarker(obj.keep, obj.name)
}

// keepClass names the expirer's guard a record earns: the newest reservation
// per allocator, the newest epoch record per lineage, and a channel-id
// exclusion inside retention. It switches on the kind and then reads exactly
// one identifying field of the body: the allocator name, the lineage id, or the
// random class. It never reads a member of a copy, post, gallery, or exclusion
// set, so a keep class names no owner id, local id, file id, channel id, client
// upload id, or excluded id, and it carries no text beyond a schema allocator
// name.
func keepClass(rec erasureledger.Record) string {
	switch body := rec.Payload.(type) {
	case erasureledger.Reservation:
		return "newest-reservation/" + string(body.Allocator)
	case erasureledger.Epoch:
		return "newest-epoch/" + keyName(body.Lineage)
	case erasureledger.RandomExclusion:
		if body.Class == erasureledger.RandomClassChannel {
			return "channel-id-exclusion"
		}
	}
	return ""
}

// movedKeep reports whether a keep class belongs to the newest record of its
// class, so the marker moves to each new arrival. A channel-id exclusion is
// excluded for the identity's lifetime, so its guard never moves.
func movedKeep(class string) bool {
	return strings.HasPrefix(class, "newest-reservation/") ||
		strings.HasPrefix(class, "newest-epoch/")
}

// list is the paginated listing of opaque keys. Each page carries the
// count the provider intended for it, which is the count the walk checks, and
// the final page carries the completion marker. Entries are opaque keys and the
// provider's own arrival reading: nothing derived from a body.
func (p *synthProvider) list(after string, pageSize int) (listPage, error) {
	if pageSize < 1 {
		return listPage{}, fmt.Errorf("synthledger: page size %d is not positive", pageSize)
	}
	p.pages++
	pageNo := p.pages
	if p.faults.failPage == pageNo {
		return listPage{}, errInjectedPage
	}
	objs, last := p.s.page(after, pageSize)
	if p.faults.dropPage == pageNo && len(objs) > 0 {
		// The whole page vanishes and the walk continues past it, so the
		// records it carried are simply missing from the enumeration.
		objs, last = p.s.page(objs[len(objs)-1].name, pageSize)
	}
	declared := len(objs)
	if p.faults.truncatePage == pageNo && declared > 1 {
		objs = objs[:declared-1]
	}
	page := listPage{Count: declared, Entries: make([]listEntry, 0, len(objs))}
	for _, obj := range objs {
		page.Entries = append(page.Entries, listEntry{Key: obj.key, arrivalNanos: obj.arrival.clockNanos})
	}
	if len(objs) > 0 {
		page.Continuation = objs[len(objs)-1].name
	}
	// A page with nothing after its token is the last page: a listing that
	// ends on an empty page completes, so a walk can tell "nothing left" from
	// "the provider stopped answering".
	page.Complete = last == "" || len(objs) == 0 || page.Continuation >= last
	return page, nil
}

// get is the replayer's read. An unconfirmed write and a quarantined write are
// both indistinguishable from an absent key.
func (p *synthProvider) get(key erasureledger.OperationKey) (replayObject, error) {
	obj := p.s.getObject(keyName(key))
	if obj == nil {
		if marker := p.s.readMarker(keyName(key)); marker != nil && !p.hideMarkers {
			return replayObject{Key: key, Arrival: marker.arrival, logical: true}, nil
		}
		return replayObject{}, fmt.Errorf("%w: key %s", errNotFound, keyName(key))
	}
	if obj.state != stateConfirmed {
		return replayObject{}, fmt.Errorf("%w: key %s is stored and unconfirmed",
			errNotFound, keyName(key))
	}
	rec, err := erasureledger.Decode(obj.body)
	if err != nil {
		return replayObject{}, fmt.Errorf("synthledger: stored body does not decode: %w", err)
	}
	if rec.OpKey != key {
		return replayObject{}, fmt.Errorf("synthledger: stored body names key %s, the listing named %s",
			keyName(rec.OpKey), keyName(key))
	}
	return replayObject{
		Key: key, Record: rec, Body: slices.Clone(obj.body),
		Arrival: obj.arrival, keep: obj.keep,
	}, nil
}

// attributes is the verifier's attribute read: the provider's account of a key,
// with no body. A quarantined write reports its quarantine, which is how a
// dead-man check sees a fenced write that a listing will never show.
func (p *synthProvider) attributes(key erasureledger.OperationKey) (objectAttributes, error) {
	name := keyName(key)
	if obj := p.s.getObject(name); obj != nil {
		return objectAttributes{
			Key: key, Bytes: len(obj.body), Arrival: obj.arrival,
			Epoch: obj.epoch, Stream: obj.stream, Seq: obj.seq,
			Confirmed: obj.state == stateConfirmed, keep: obj.keep,
		}, nil
	}
	if marker := p.s.readMarker(name); marker != nil {
		return objectAttributes{Key: key, Arrival: marker.arrival, Logical: true}, nil
	}
	for _, obj := range p.s.quarantinedObjects() {
		if obj.name == name {
			return objectAttributes{
				Key: key, Bytes: len(obj.body), Arrival: obj.arrival,
				Epoch: obj.epoch, Stream: obj.stream, Seq: obj.seq,
				Quarantined: true,
			}, nil
		}
	}
	return objectAttributes{}, fmt.Errorf("%w: key %s", errNotFound, name)
}

// arrival is the verifier's independent per-stream arrival record.
func (p *synthProvider) arrival(epoch int64, stream erasureledger.StreamID) (arrivalRecord, error) {
	out := arrivalRecord{Epoch: epoch, Stream: stream, found: false}
	for _, v := range p.verifs {
		if v.epoch == epoch && v.stream == stream {
			if v.tamper {
				return arrivalRecord{}, fmt.Errorf("%w: epoch %d stream %s",
					errInjectedVerifier, epoch, keyName(stream))
			}
			out = arrivalRecord{
				Epoch: epoch, Stream: stream,
				Arrivals: v.count, HighWater: v.high, found: true,
			}
		}
	}
	if !out.found {
		return arrivalRecord{}, fmt.Errorf("%w: no arrival record for epoch %d stream %s",
			errNotFound, epoch, keyName(stream))
	}
	return out, nil
}

// pruneThrough writes the expirer's per-stream checkpoint. Expiry keeps
// contiguity checkable: the checkpoint is the ledger's statement that the
// sequences at or below it were pruned on purpose, so a walk can tell a prune
// from a gap.
func (p *synthProvider) pruneThrough(epoch int64, stream erasureledger.StreamID, through int64) error {
	sk := streamKey{epoch: epoch, stream: stream}
	str, ok := p.s.streamState(sk)
	if !ok {
		return fmt.Errorf("%w: epoch %d stream %s has no arrivals", errNotFound, epoch, keyName(stream))
	}
	if through > str.confirmedSeq {
		return fmt.Errorf("synthledger: pruned-through %d is past the confirmed high water %d",
			through, str.confirmedSeq)
	}
	if through < str.pruned {
		return fmt.Errorf("synthledger: pruned-through may not move backwards from %d", str.pruned)
	}
	str.pruned = through
	p.s.saveStreamState(sk, str)
	return nil
}

// delete is the expirer's delete, gated on the checkpoint and the keep classes.
func (p *synthProvider) delete(key erasureledger.OperationKey) error {
	obj := p.s.getObject(keyName(key))
	if obj == nil {
		return fmt.Errorf("%w: key %s", errNotFound, keyName(key))
	}
	sk := streamKey{epoch: obj.epoch, stream: obj.stream}
	str, ok := p.s.streamState(sk)
	if !ok || str.pruned < obj.seq {
		return fmt.Errorf("%w: sequence %d is not covered by a pruned-through checkpoint",
			errNotPruned, obj.seq)
	}
	if obj.keep != "" {
		return fmt.Errorf("%w: key %s holds the %s guard", errNotDeletable, keyName(key), obj.keep)
	}
	p.s.deleteObject(obj.name)
	str.arrivalCount--
	p.s.saveStreamState(sk, str)
	for i, v := range p.verifs {
		if v.epoch == obj.epoch && v.stream == obj.stream {
			p.verifs[i].count = str.arrivalCount
		}
	}
	return nil
}

// createSentinel mints the logical completion marker. It lives in its own
// marker area, so it is never part of a listing of ledger objects, and it
// carries no ledger record body.
func (p *synthProvider) createSentinel() (erasureledger.OperationKey, error) {
	key, err := erasureledger.NewOperationKey(rand.Reader)
	if err != nil {
		return erasureledger.OperationKey{}, err
	}
	marker := &object{
		name: keyName(key), key: key,
		arrival: p.arriveAt(nil), state: stateConfirmed,
	}
	if !p.s.writeMarker(marker.name, marker) {
		return erasureledger.OperationKey{}, fmt.Errorf("%w: sentinel %s", errObjectExists, marker.name)
	}
	return key, nil
}

// Principals. Each is one authorized credential over the same provider, and the
// method set below is the whole capability. The least-privilege conformance
// case reads that method set by reflection, so a method added to a principal
// fails the case rather than widening a credential quietly.

// writer is the on-alpha ledger credential: conditional create, plus the arrival
// confirmation of its own write. It has no overwrite, delete, get, list,
// retention, policy, fence, sentinel, prune, or verifier capability, and
// Create's one argument is a ledger record, so it cannot address a bucket, a
// prefix, a version, a lock mode, a retention deadline, a clock, or a key of
// its own choosing.
type writer struct {
	p      *synthProvider
	epoch  int64
	stream erasureledger.StreamID
}

// Create stores rec under its opaque key with a create-once condition.
func (w writer) Create(rec erasureledger.Record) (*pendingWrite, error) {
	return w.p.create(w, rec)
}

// Confirm accepts the provider's arrival evidence for the write the handle
// names. The handle is bound to this credential: one issued to another stream
// or epoch is refused, and the refusal reads nothing.
func (w writer) Confirm(h *pendingWrite) (*createReceipt, error) {
	return w.p.confirm(w, h)
}

// replayer is the escrow-side credential: get and list, nothing else. It
// cannot in particular create the sentinel that is meant to vouch for its own
// listing, and Get's argument is an opaque key, so a replayer cannot
// enumerate bodies by prefix, fetch a body range, or read attributes.
type replayer struct{ p *synthProvider }

// Get reads one replayable record by opaque key.
func (r replayer) Get(key erasureledger.OperationKey) (replayObject, error) {
	return r.p.get(key)
}

// List reads one page of opaque keys.
func (r replayer) List(after string, pageSize int) (listPage, error) {
	return r.p.list(after, pageSize)
}

// verifier is the off-alpha credential: attributes and arrival records only.
// It confirms arrival for the dead-man check and never writes, so a
// verification failure cannot nudge the ledger.
type verifier struct{ p *synthProvider }

// Attributes reads the provider's account of one key, with no body.
func (v verifier) Attributes(key erasureledger.OperationKey) (objectAttributes, error) {
	return v.p.attributes(key)
}

// Arrival reads the independent per-stream arrival record.
func (v verifier) Arrival(epoch int64, stream erasureledger.StreamID) (arrivalRecord, error) {
	return v.p.arrival(epoch, stream)
}

// expirer is the only credential with a delete right, and it must checkpoint
// before it exercises it.
type expirer struct{ p *synthProvider }

// PruneThrough records the per-stream pruned-through checkpoint.
func (e expirer) PruneThrough(epoch int64, stream erasureledger.StreamID, through int64) error {
	return e.p.pruneThrough(epoch, stream, through)
}

// Delete removes one record whose stream is pruned through its sequence.
func (e expirer) Delete(key erasureledger.OperationKey) error {
	return e.p.delete(key)
}

// sentinelAuthorizer is the separately authorized logical-sentinel creator.
// MAIN-1362 accepts the sentinel as a separately authorized marker, and this
// is the only principal that can mint one.
type sentinelAuthorizer struct{ p *synthProvider }

// CreateSentinel mints the logical completion marker.
func (a sentinelAuthorizer) CreateSentinel() (erasureledger.OperationKey, error) {
	return a.p.createSentinel()
}

// pendingWrite is the handle Create returns. Its fields are unexported, so a
// caller cannot forge one, point it at another key, or use it to read
// anything. It carries the credential that issued it, and Confirm accepts it
// only from that credential: a handle is a stream's own in-flight write.
type pendingWrite struct {
	epoch   int64
	stream  erasureledger.StreamID
	key     erasureledger.OperationKey
	seq     int64
	arrival arrivalEvidence
}

// Arrival is the provider's reading for the write the handle names.
func (h *pendingWrite) Arrival() arrivalEvidence { return h.arrival }

// createReceipt is the writer's confirmation of a durable arrival: the opaque
// key, the provider's arrival reading, the stream sequence the provider
// accepted, and the byte count it stored.
type createReceipt struct {
	Key     erasureledger.OperationKey
	Arrival arrivalEvidence
	Seq     int64
	Bytes   int
}

// listEntry is one listing row. It is an opaque key and the provider's own
// arrival reading, and nothing else: no body, no owner id, no file id, no
// allocator, no epoch, no stream, no sequence, no record kind, and no
// exported member that could expose a body.
type listEntry struct {
	Key          erasureledger.OperationKey
	arrivalNanos int64
}

// listPage is one page of the walk.
type listPage struct {
	Entries      []listEntry
	Count        int
	Continuation string
	Complete     bool
}

// replayObject is the replayer's read of one record.
type replayObject struct {
	Key     erasureledger.OperationKey
	Record  erasureledger.Record
	Body    []byte
	Arrival arrivalEvidence

	// logical marks the sentinel marker: a marker the replayer can see, with
	// no ledger record behind it.
	logical bool
	keep    string
}

// objectAttributes is the verifier's attribute reading. It has no body, no
// record, and no exported member that exposes one.
type objectAttributes struct {
	Key     erasureledger.OperationKey
	Bytes   int
	Arrival arrivalEvidence
	Epoch   int64
	Stream  erasureledger.StreamID
	Seq     int64

	Confirmed   bool
	Logical     bool
	Quarantined bool
	keep        string
}

// arrivalRecord is the verifier's per-stream arrival record. Arrivals counts
// the stream's confirmed arrivals and HighWater is the highest confirmed
// sequence, which a prune never lowers.
type arrivalRecord struct {
	Epoch     int64
	Stream    erasureledger.StreamID
	Arrivals  int64
	HighWater int64
	found     bool
}

// opaqueID is the constraint every fixed-length opaque identity satisfies: the
// operation key, the replica stream, and the restore lineage.
type opaqueID interface {
	~[16]byte
}

// keyName is the storage name for an opaque identity: its own bytes in hex. A
// name is therefore an opaque value that carries no body identifier, and a
// stream's name is no more revealing than a record's.
func keyName[T opaqueID](id T) string { return hex.EncodeToString(id[:]) }

// keyFromName is keyName's inverse.
func keyFromName(name string) (erasureledger.OperationKey, error) {
	var key erasureledger.OperationKey
	raw, err := hex.DecodeString(name)
	if err != nil {
		return key, fmt.Errorf("synthledger: key name is not hex: %w", err)
	}
	if len(raw) != erasureledger.OperationKeyLen {
		return key, fmt.Errorf("synthledger: key name is %d bytes, want %d",
			len(raw), erasureledger.OperationKeyLen)
	}
	copy(key[:], raw)
	return key, nil
}

// methodNames lists a value's exported method names, which is the capability a
// credential actually offers.
func methodNames(v any) []string {
	typ := reflect.TypeOf(v)
	out := make([]string, 0, typ.NumMethod())
	for m := range typ.Methods() {
		out = append(out, m.Name)
	}
	slices.Sort(out)
	return out
}

// capabilitiesOf lists the capability interfaces above a value satisfies. The
// list is built from the interface types themselves, so a capability interface
// added to this file is picked up by the assertion without editing the case.
func capabilitiesOf(v any) []string {
	var out []string
	for _, typ := range capabilityTypes {
		if reflect.TypeOf(v).Implements(typ) {
			out = append(out, typ.Name())
		}
	}
	slices.Sort(out)
	return out
}

// capabilityTypes is the seam's capability vocabulary.
var capabilityTypes = []reflect.Type{
	reflect.TypeFor[ledgerCreator](),
	reflect.TypeFor[ledgerReader](),
	reflect.TypeFor[ledgerArrivalReader](),
	reflect.TypeFor[ledgerDeleter](),
	reflect.TypeFor[ledgerSentinelCreator](),
}

// principalCapability is one principal's authorized capability: its name, its
// exact method set, and the one capability interface it may satisfy.
type principalCapability struct {
	name     string
	methods  []string
	capacity string
}

// checkPrincipal asserts a principal offers exactly the named methods
// and satisfies exactly the named capability interface.
func checkPrincipal(t *testing.T, p any, want principalCapability) {
	t.Helper()
	if got := methodNames(p); !slices.Equal(got, want.methods) {
		t.Errorf("%s methods = %v, want exactly %v", want.name, got, want.methods)
	}
	gotCaps := capabilitiesOf(p)
	if wantCaps := []string{want.capacity}; !slices.Equal(gotCaps, wantCaps) {
		t.Errorf("%s satisfies %v, want exactly %v", want.name, gotCaps, wantCaps)
	}
}

// providerArm is one storage arm under the seam: a name for the subtest and a
// way to open a fresh medium.
type providerArm struct {
	name string
	open func(t *testing.T) store
}

// provider opens a fresh provider over a fresh medium, which is what a case
// that injects several listing faults needs.
func (a providerArm) provider(t *testing.T) *synthProvider {
	t.Helper()
	return newSynthProvider(t, a.open(t))
}

// providerArms is the pair of storage arms the conformance suite runs: the same
// seam logic over an in-memory map, and over a disposable local directory that
// the test deletes.
var providerArms = []providerArm{
	{name: "memory", open: newMemoryStore},
	{name: "dir", open: openDirStore},
}

// runOverArms runs fn over both arms in parallel subtests, so every conformance
// case is asserted for the in-memory arm and the disposable-directory arm.
func runOverArms(t *testing.T, fn func(t *testing.T, arm providerArm, p *synthProvider)) {
	t.Helper()
	for _, arm := range providerArms {
		t.Run(arm.name, func(t *testing.T) {
			t.Parallel()
			fn(t, arm, arm.provider(t))
		})
	}
}

// streamInventory is the ledger's own (epoch, stream) set, read from the
// medium. A completeness check reads the verifier's arrival record for every
// stream in it, so the expected set is supplied independently of the walk: a
// listing that hides a stream cannot hide it from the gate too.
func (p *synthProvider) streamInventory() []seqKey {
	stored := p.s.streams()
	out := make([]seqKey, 0, len(stored))
	for _, k := range stored {
		out = append(out, seqKey(k))
	}
	slices.SortFunc(out, compareSeqKeys)
	return out
}

// maxWalkPages bounds a walk that never reports completion, so a broken
// provider fails the gate instead of spinning.
const maxWalkPages = 64

// walkOutcome is the result of a full paginated walk.
type walkOutcome struct {
	Pages      int
	Objects    []replayObject
	Keys       []erasureledger.OperationKey
	MaxArrival int64
}

// walk drains the replayer's listing from the first page.
func walk(r replayer, pageSize int, sentinel erasureledger.OperationKey) (walkOutcome, error) {
	return walkFrom(r, "", pageSize, sentinel)
}

// walkFrom drains the replayer's listing with continuation tokens, starting at a
// token so an interrupted walk can resume where it stopped. It checks the count
// of every page against the entries the listing returned, reads every key the
// listing showed, and refuses to call a walk complete without the completion
// marker.
func walkFrom(r replayer, after string, pageSize int, sentinel erasureledger.OperationKey) (walkOutcome, error) {
	var out walkOutcome
	for {
		page, err := r.List(after, pageSize)
		if err != nil {
			return out, fmt.Errorf("page %d: %w", out.Pages+1, err)
		}
		out.Pages++
		if page.Count != len(page.Entries) {
			return out, fmt.Errorf("page %d declared %d entries and carried %d: the per-page count check failed",
				out.Pages, page.Count, len(page.Entries))
		}
		for _, e := range page.Entries {
			obj, err := r.Get(e.Key)
			if err != nil {
				return out, fmt.Errorf("page %d key %s: %w", out.Pages, keyName(e.Key), err)
			}
			if !obj.Arrival.verify(obj.Body) {
				return out, fmt.Errorf("page %d key %s: the arrival evidence does not describe the stored bytes",
					out.Pages, keyName(e.Key))
			}
			out.Objects = append(out.Objects, obj)
			out.Keys = append(out.Keys, e.Key)
			out.MaxArrival = max(out.MaxArrival, obj.Arrival.arrivalSeq)
		}
		if page.Complete {
			// The sentinel is a marker and not a ledger object, so it is read
			// by name. A walk that cannot see it reports no sighting, and the
			// completeness checks then refuse to call the enumeration complete.
			if sentinel != (erasureledger.OperationKey{}) {
				if marker, err := r.Get(sentinel); err == nil && marker.logical {
					out.Objects = append(out.Objects, marker)
					out.MaxArrival = max(out.MaxArrival, marker.Arrival.arrivalSeq)
				}
			}
			return out, nil
		}
		if page.Continuation == "" {
			return out, fmt.Errorf("page %d is not marked complete and carries no continuation token", out.Pages)
		}
		after = page.Continuation
		if out.Pages > maxWalkPages {
			return out, fmt.Errorf("the walk passed %d pages without a completion marker", maxWalkPages)
		}
	}
}

// seqKey is a per-(epoch, stream) position, the unit the completeness
// checks work on.
type seqKey struct {
	epoch  int64
	stream erasureledger.StreamID
}

// pruneMark is one expirer checkpoint as the gate reads it.
type pruneMark struct {
	key     seqKey
	through int64
}

// gateInput is the evidence a complete enumeration needs from MAIN-1360's
// complete-enumeration list: the walk's per-stream sequences, the verifier's
// independent arrival records, the expirer's checkpoints, and the sentinel
// sighting. A provider consistency attestation is deliberately absent: this
// seam cannot produce one, and writing one here would be a false claim.
type gateInput struct {
	Walk walkOutcome
	// Streams is the expected (epoch, stream) set, supplied from the ledger's
	// own inventory. Every stream in it has to be answered for by the
	// verifier's arrival record and by the walk, and a stream the walk found
	// without being expected is a disagreement of its own.
	Streams  []seqKey
	Arrivals []arrivalRecord
	Pruned   []pruneMark
	// Sentinel is the expected logical completion marker. MAIN-1360's
	// complete-enumeration list ends with the replayer's sentinel, so
	// naming no marker is a failure: a listing that looks finished is not
	// proof that it is.
	Sentinel erasureledger.OperationKey
}

// gateReport is the gate's answer with a reason per failure.
type gateReport struct {
	Complete   bool
	Reasons    []string
	HighWaters map[seqKey]int64
}

// checkEnumeration runs the completeness checks over a walk: a per-stream
// sequence below the high water and above the prune checkpoint must be present,
// must be present once, and must agree with the verifier's independent arrival
// record.
func checkEnumeration(in gateInput) gateReport {
	rep := gateReport{HighWaters: map[seqKey]int64{}}
	seen := map[seqKey]map[int64]erasureledger.OperationKey{}
	for _, obj := range in.Walk.Objects {
		if obj.logical {
			continue
		}
		k := seqKey{epoch: obj.Record.Epoch, stream: obj.Record.Stream}
		if seen[k] == nil {
			seen[k] = map[int64]erasureledger.OperationKey{}
		}
		if _, dup := seen[k][obj.Record.Seq]; dup {
			rep.Reasons = append(rep.Reasons, fmt.Sprintf("epoch %d stream %s lists sequence %d twice",
				k.epoch, keyName(k.stream), obj.Record.Seq))
		}
		seen[k][obj.Record.Seq] = obj.Key
		rep.HighWaters[k] = max(rep.HighWaters[k], obj.Record.Seq)
	}
	pruned := map[seqKey]int64{}
	for _, mark := range in.Pruned {
		pruned[mark.key] = max(pruned[mark.key], mark.through)
	}
	for k, high := range rep.HighWaters {
		for seq := range high {
			if seq <= pruned[k] {
				continue
			}
			if _, ok := seen[k][seq]; !ok {
				rep.Reasons = append(rep.Reasons, fmt.Sprintf(
					"epoch %d stream %s is missing sequence %d below its high water %d",
					k.epoch, keyName(k.stream), seq, high))
			}
		}
	}
	expected := map[seqKey]bool{}
	for _, k := range in.Streams {
		expected[k] = true
	}
	arrivals := map[seqKey]arrivalRecord{}
	for _, a := range in.Arrivals {
		arrivals[seqKey{epoch: a.Epoch, stream: a.Stream}] = a
	}
	// Every expected stream has to be answered for, by the verifier and by
	// the walk. A stream the listing hid is expected all the same, because
	// the expected set comes from the ledger and not from the walk.
	for _, k := range in.Streams {
		a, answered := arrivals[k]
		if !answered {
			rep.Reasons = append(rep.Reasons, fmt.Sprintf(
				"the verifier reports no arrival record for epoch %d stream %s", k.epoch, keyName(k.stream)))
			continue
		}
		delete(arrivals, k)
		high, found := rep.HighWaters[k]
		if !found {
			rep.Reasons = append(rep.Reasons, fmt.Sprintf(
				"the verifier reports %d arrivals on epoch %d stream %s and the walk found none",
				a.Arrivals, k.epoch, keyName(k.stream)))
			continue
		}
		if a.HighWater != high {
			rep.Reasons = append(rep.Reasons, fmt.Sprintf(
				"arrival high-water disagreement on epoch %d stream %s: verifier %d, walk %d",
				k.epoch, keyName(k.stream), a.HighWater, high))
		}
		if a.Arrivals != int64(len(seen[k])) {
			rep.Reasons = append(rep.Reasons, fmt.Sprintf(
				"arrival count disagreement on epoch %d stream %s: verifier %d, walk %d",
				k.epoch, keyName(k.stream), a.Arrivals, len(seen[k])))
		}
	}
	leftover := slices.Collect(maps.Keys(arrivals))
	slices.SortFunc(leftover, compareSeqKeys)
	for _, k := range leftover {
		rep.Reasons = append(rep.Reasons, fmt.Sprintf(
			"the verifier reports %d arrivals on epoch %d stream %s, which the expected stream set does not name",
			arrivals[k].Arrivals, k.epoch, keyName(k.stream)))
	}
	found := slices.Collect(maps.Keys(rep.HighWaters))
	slices.SortFunc(found, compareSeqKeys)
	for _, k := range found {
		if !expected[k] {
			rep.Reasons = append(rep.Reasons, fmt.Sprintf(
				"the walk found records on epoch %d stream %s, which the expected stream set does not name",
				k.epoch, keyName(k.stream)))
		}
	}
	if in.Sentinel == (erasureledger.OperationKey{}) {
		rep.Reasons = append(rep.Reasons,
			"the gate names no expected sentinel marker, so nothing closes this enumeration")
	} else {
		marker, seenMarker := sentinelOf(in.Walk)
		if !seenMarker || marker.Key != in.Sentinel {
			// The sighting has to be the expected marker: a marker for some
			// other ledger, or none at all, closes nothing.
			rep.Reasons = append(rep.Reasons, fmt.Sprintf(
				"the sentinel marker %s is not visible in the walk", keyName(in.Sentinel)))
		} else if marker.Arrival.arrivalSeq != in.Walk.MaxArrival {
			rep.Reasons = append(rep.Reasons, "the sentinel is not the newest arrival in the enumeration")
		}
	}
	rep.Complete = len(rep.Reasons) == 0
	return rep
}

// compareSeqKeys orders (epoch, stream) pairs, so gate failures read the same
// way every run.
func compareSeqKeys(a, b seqKey) int {
	if c := int(a.epoch - b.epoch); c != 0 {
		return c
	}
	return bytes.Compare(a.stream[:], b.stream[:])
}

// sentinelOf finds the sentinel sighting in a walk, if any.
func sentinelOf(out walkOutcome) (replayObject, bool) {
	for _, obj := range out.Objects {
		if obj.logical {
			return obj, true
		}
	}
	return replayObject{}, false
}

// streamsOf collects the walk's (epoch, stream) pairs. The gate does not use
// it, and must not: evidence derived from what a listing showed has nothing to
// disagree with when the listing hid a stream. The page-fault case keeps it as
// the unsound derivation, so the difference stays visible.
func streamsOf(out walkOutcome) []seqKey {
	seen := map[seqKey]bool{}
	var out2 []seqKey
	for _, obj := range out.Objects {
		if obj.logical {
			continue
		}
		k := seqKey{epoch: obj.Record.Epoch, stream: obj.Record.Stream}
		if !seen[k] {
			seen[k] = true
			out2 = append(out2, k)
		}
	}
	slices.SortFunc(out2, compareSeqKeys)
	return out2
}

// arrivalRecords reads the verifier's arrival record for every expected stream.
func arrivalRecords(t *testing.T, v verifier, keys []seqKey) []arrivalRecord {
	t.Helper()
	recs := make([]arrivalRecord, 0, len(keys))
	for _, k := range keys {
		rec, err := v.Arrival(k.epoch, k.stream)
		if err != nil {
			t.Fatalf("verifier arrival epoch=%d stream=%s: %v", k.epoch, keyName(k.stream), err)
		}
		recs = append(recs, rec)
	}
	return recs
}
