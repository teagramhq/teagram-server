// Package synthledger is the synthetic erasure-ledger provider: the capability
// surface the accepted MAIN-1360 model (as amended on MAIN-1362) specifies for
// the off-alpha erasure ledger, built as a test-only seam with two storage arms,
// an in-memory map and a disposable local directory. Only _test.go files may
// import it; TestNoNonTestFileImportsTheProvider holds that for the module.
//
// The principals are the shape of the capability contract: a writer that can
// only create-once, a replayer that can only get and list, a verifier that can
// only read attributes and arrival records, an expirer that is the only
// principal able to delete, a sentinel authorizer, a flusher that hands the
// provider stored record bytes, and a proof reader that reports what the
// provider holds at a stream position.
//
// What it does not pin, and never claims, is any real provider property. There
// is no S3, RustFS, MinIO, or filesystem-provider client here, no credential,
// no bucket, no Object Lock, no versioning, no retention, and no provider
// consistency guarantee. The arrival evidence below is a reading from a
// simulated clock, and the checksum is a SHA-256 the provider computes over the
// bytes it holds: enough for a test to assert that evidence describes the
// stored object rather than the writer's claim, and nothing more. It is not the
// real provider's checksum. Using this package establishes no provider
// capability, no restore readiness, and no deployment evidence.
package synthledger

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/teagramhq/teagram-server/internal/erasureledger"
)

// Seam errors. They are typed causes, not strings, so a conformance case
// asserts on the refusal it expects.
var (
	// ErrObjectExists is the conditional-create refusal: the key is taken, so
	// a create cannot overwrite it.
	ErrObjectExists = errors.New("synthledger: object exists")
	// ErrNotFound is the read result for a key with no replayable object. An
	// absent key, a write that is not confirmed yet, and a quarantined write
	// are indistinguishable from the replayer's seat, which is the point.
	ErrNotFound = errors.New("synthledger: no replayable object")
	// ErrQuarantined is the provider's answer to a write on a fenced stream:
	// the bytes were kept as evidence, and they did not enter the ledger.
	ErrQuarantined = errors.New("synthledger: write quarantined")
	// ErrSequenceGap is the serial-flusher rule: sequence n+1 is refused while
	// n is unconfirmed.
	ErrSequenceGap = errors.New("synthledger: predecessor unconfirmed")
	// ErrAlreadyConfirmed means an arrival was confirmed once, so a second
	// confirmation of the same write is not a retry and is not a
	// way to re-open a published record.
	ErrAlreadyConfirmed = errors.New("synthledger: arrival already confirmed")
	// ErrCredential is the refusal of a write whose envelope does not match the
	// writer credential that made it.
	ErrCredential = errors.New("synthledger: credential does not cover this write")
	// ErrInjectedPage is the conformance suite's injected listing failure.
	ErrInjectedPage = errors.New("synthledger: injected list failure")
	// ErrNotPruned is the expirer's ordering rule: no delete without a
	// pruned-through checkpoint covering the record's stream.
	ErrNotPruned = errors.New("synthledger: pruned-through checkpoint missing")
	// ErrNotDeletable is the expirer guard on records needed to keep recovery
	// checkable: the newest scalar reservation per allocator, the maximum
	// component ceiling per full key, bindings needed by surviving component
	// ceilings, the newest epoch per lineage, and the newest confirmed covering
	// exclusion snapshot per random-id class.
	ErrNotDeletable = errors.New("synthledger: record is not deletable")
	// ErrInjectedVerifier is the arrival-log tamper marker.
	ErrInjectedVerifier = errors.New("synthledger: injected verifier entry")
)

// ArrivalEvidence is the provider's own account of an arrival. Nothing in it
// comes from the writer: the record has no timestamp field, the create call has
// no clock argument, and the clock and arrival counter are the provider's
// simulated state. The checksum is a SHA-256 of the stored bytes: a simulated
// integrity marker over what the provider kept, not a MAC, not a signature, and
// not a provider integrity proof. Every field is unexported, so only the
// provider makes one.
type ArrivalEvidence struct {
	clockNanos int64
	arrivalSeq int64
	checksum   [sha256.Size]byte
}

// ClockNanos is the simulated provider clock at the arrival.
func (a ArrivalEvidence) ClockNanos() int64 { return a.clockNanos }

// ArrivalSeq is the provider's arrival sequence for the object.
func (a ArrivalEvidence) ArrivalSeq() int64 { return a.arrivalSeq }

// Checksum is the SHA-256 the provider computed over the bytes it stored.
func (a ArrivalEvidence) Checksum() [sha256.Size]byte { return a.checksum }

// Verify is the integrity check on the seam's own evidence: the checksum
// describes body.
func (a ArrivalEvidence) Verify(body []byte) bool {
	return a.checksum == sha256.Sum256(body)
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
	arrival ArrivalEvidence
	state   objectState
	// keep names the provider's keep class while this object is the one the
	// expirer must not delete. Component ceilings use their full key; random
	// exclusions use the class and never a member of the set.
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

// Store is the storage arm under the seam. The memory arm answers these from
// maps; the disposable-directory arm answers them from files, with the
// conditional create coming from the filesystem's exclusive create.
type Store interface {
	providerState() (clock, arrivals int64)
	saveProviderState(clock, arrivals int64)

	streamState(sk streamKey) (streamRec, bool)
	saveStreamState(sk streamKey, rec streamRec)

	// streams is the ledger's own per-(epoch, stream) inventory, read from
	// the medium: it is what the verifier's arrival records are read for, so
	// the expected stream set never comes from what a listing showed.
	streams() []streamKey

	getObject(name string) *object
	// objectAt is the stored object, in any state, at one stream position.
	objectAt(sk streamKey, seq int64) *object
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

// Provider is the synthetic ledger provider: the shared capability,
// sequencing, evidence and fence logic, over one storage arm. The conformance
// suite runs this identical logic over both arms, so what differs between them
// is the medium. mu serialises every principal call and every fault setter, so
// the provider is safe for concurrent use.
type Provider struct {
	mu     sync.Mutex
	s      Store
	clock  int64
	arrive int64
	pages  int
	faults PageFaults
	verifs []verifierEntry
	// hideMarkers is the marker fault: the store keeps a sentinel the replayer
	// can no longer read.
	hideMarkers bool

	// The flusher's faults. gateOff disables the provider's own sequence
	// gate, stalls hold a create at a position until its release channel
	// closes, drops lose the answer of a create at a position after the
	// provider acted on it, and calls is the log of every flusher create in
	// the order it reached the provider.
	gateOff bool
	stalls  map[Position]stall
	drops   map[Position]bool
	calls   []Position
}

// stall is one held create: entered closes when the call arrives, and the call
// waits for release before it touches provider state.
type stall struct {
	entered chan struct{}
	release <-chan struct{}
}

// PageFaults are injected listing faults: the page number that fails, the page
// that vanishes from the walk, and the page that carries one entry fewer than
// it declares. Zero disables a fault.
type PageFaults struct {
	FailPage     int
	DropPage     int
	TruncatePage int
}

// verifierEntry is one line in the verifier's independent arrival log.
type verifierEntry struct {
	epoch  int64
	stream erasureledger.StreamID
	count  int64
	high   int64
	tamper bool
}

// New opens a provider over a storage arm.
func New(tb testing.TB, s Store) *Provider {
	tb.Helper()
	clock, arrivals := s.providerState()
	return &Provider{
		s: s, clock: clock, arrive: arrivals,
		stalls: map[Position]stall{}, drops: map[Position]bool{},
	}
}

// arriveAt advances the simulated provider clock, mints the next provider
// arrival sequence, and computes the checksum over the bytes being stored.
func (p *Provider) arriveAt(body []byte) ArrivalEvidence {
	p.clock += simTickNanos
	p.arrive++
	p.s.saveProviderState(p.clock, p.arrive)
	return ArrivalEvidence{
		clockNanos: p.clock,
		arrivalSeq: p.arrive,
		checksum:   sha256.Sum256(body),
	}
}

// NewWriter mints a writer credential for one (epoch, replica stream). The
// model issues a writer credential per epoch with a stream prefix and revokes
// it at the fence, so the credential is bound: it cannot address another
// stream, and it cannot write past its fence.
func (p *Provider) NewWriter(epoch int64, stream erasureledger.StreamID) Writer {
	return Writer{p: p, epoch: epoch, stream: stream}
}

// FenceStream marks a stream fenced. It is a bookkeeping marker in the seam,
// not a credential revocation, and it establishes no revocation capability.
func (p *Provider) FenceStream(epoch int64, stream erasureledger.StreamID) {
	p.mu.Lock()
	defer p.mu.Unlock()
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
// unconfirmed predecessor may be outstanding. A key that is taken has two
// answers and they must not read alike: a confirmed object is durable, so the
// create-once condition refuses the write; an unconfirmed one is the same flush
// arriving again, and the retry gets its handle back so the resume can
// confirm and the stream keeps moving.
func (p *Provider) create(w Writer, rec erasureledger.Record) (*PendingWrite, error) {
	if rec.Epoch != w.epoch || rec.Stream != w.stream {
		return nil, fmt.Errorf("%w: envelope epoch and stream are not the credential's", ErrCredential)
	}
	if _, known := erasureledger.LookupKind(rec.Kind); !known {
		return nil, fmt.Errorf("%w: kind %v is outside the accepted vocabulary", ErrCredential, rec.Kind)
	}
	body, err := erasureledger.Encode(rec)
	if err != nil {
		return nil, fmt.Errorf("%w: record does not encode: %w", ErrCredential, err)
	}
	name := KeyName(rec.OpKey)
	if prev := p.s.getObject(name); prev != nil {
		// A durable key is the create-once refusal, and the refusal names the
		// key only: it is not a read, so the stored arrival and the stored byte
		// count stay the provider's.
		if prev.state == stateConfirmed {
			return nil, fmt.Errorf("%w: key %s", ErrObjectExists, name)
		}
		// An unconfirmed key is a resumed flush, and the answer cannot be
		// "already durable" because the record is not replayable yet. The
		// handle is re-issued from the caller's own bytes, so the write can be
		// confirmed and the stream's sequence keeps moving. The retry has to be
		// the caller's own write on the credential's stream, byte for byte:
		// nothing stored is read out, and no overwrite happens.
		if prev.epoch != w.epoch || prev.stream != w.stream {
			return nil, fmt.Errorf("%w: key %s is stored on another stream", ErrCredential, name)
		}
		if !bytes.Equal(prev.body, body) {
			return nil, fmt.Errorf("%w: key %s is stored and unconfirmed with a different body",
				ErrObjectExists, name)
		}
		return &PendingWrite{
			epoch: prev.epoch, stream: prev.stream, key: prev.key,
			seq: prev.seq, arrival: prev.arrival,
		}, nil
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
			ErrQuarantined, name, str.fencedSeq)
	}
	if str.pendingSeq != 0 && str.pendingSeq == rec.Seq {
		return nil, fmt.Errorf("%w: sequence %d is stored and unconfirmed", ErrSequenceGap, rec.Seq)
	}
	if rec.Seq != str.confirmedSeq+1 {
		return nil, fmt.Errorf("%w: stream has confirmed %d, so %d is not the next sequence",
			ErrSequenceGap, str.confirmedSeq, rec.Seq)
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
	return &PendingWrite{
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
func (p *Provider) confirm(w Writer, h *PendingWrite) (*CreateReceipt, error) {
	if h.epoch != w.epoch || h.stream != w.stream {
		return nil, fmt.Errorf("%w: the handle was issued to another stream credential", ErrCredential)
	}
	obj := p.s.getObject(KeyName(h.key))
	if obj == nil {
		return nil, fmt.Errorf("%w: no stored object behind this handle", ErrNotFound)
	}
	if obj.epoch != w.epoch || obj.stream != w.stream {
		return nil, fmt.Errorf("%w: the stored record is not on the credential's stream", ErrCredential)
	}
	if obj.state == stateConfirmed {
		return nil, fmt.Errorf("%w: sequence %d on stream %s",
			ErrAlreadyConfirmed, obj.seq, KeyName(obj.stream))
	}
	sk := streamKey{epoch: obj.epoch, stream: obj.stream}
	str, ok := p.s.streamState(sk)
	if !ok || str.pendingSeq != obj.seq {
		return nil, fmt.Errorf("%w: sequence %d is not the stream's pending write",
			ErrSequenceGap, obj.seq)
	}
	if !obj.arrival.Verify(obj.body) {
		return nil, errors.New("synthledger: arrival evidence does not describe the stored bytes")
	}
	if err := p.publish(obj); err != nil {
		return nil, err
	}

	str.confirmedSeq = obj.seq
	str.pendingSeq = 0
	str.arrivalCount++
	p.s.saveStreamState(sk, str)
	p.verifs = append(p.verifs, verifierEntry{
		epoch: obj.epoch, stream: obj.stream,
		count: str.arrivalCount, high: str.confirmedSeq,
	})
	return &CreateReceipt{
		Key: obj.key, Arrival: obj.arrival, Seq: obj.seq, Bytes: len(obj.body),
	}, nil
}

// publish moves a stored object into the confirmed state: it derives the
// object's keep class, settles the keep marker, and saves the object. The
// stream's sequence bookkeeping is the caller's.
func (p *Provider) publish(obj *object) error {
	rec, err := erasureledger.Decode(obj.body)
	if err != nil {
		return fmt.Errorf("synthledger: stored body does not decode: %w", err)
	}
	obj.keep, err = p.keepClass(rec)
	if err != nil {
		return fmt.Errorf("synthledger: derive keep class: %w", err)
	}
	obj.state = stateConfirmed
	if exclusion, ok := rec.Payload.(erasureledger.RandomExclusion); ok {
		covered, err := p.coversConfirmedExclusions(obj, exclusion)
		if err != nil {
			return fmt.Errorf("synthledger: check exclusion snapshot coverage: %w", err)
		}
		if !covered {
			// The delta remains protected by the coverage check in delete, but
			// it cannot replace the current complete snapshot as the keeper.
			obj.keep = ""
		}
	}
	if err := p.moveKeepMarker(obj); err != nil {
		return fmt.Errorf("synthledger: move keep marker: %w", err)
	}
	p.s.saveObject(obj)
	return nil
}

// moveKeepMarker follows the newest scalar reservation and epoch record, the
// maximum component ceiling per full key, and the newest covering exclusion
// snapshot. A stale lower component ceiling leaves the existing marker in place.
func (p *Provider) moveKeepMarker(obj *object) error {
	if obj.keep == "" || !movedKeep(obj.keep) {
		return nil
	}
	if prev := p.s.keepMarker(obj.keep); prev != "" && prev != obj.name {
		if po := p.s.getObject(prev); po != nil {
			if strings.HasPrefix(obj.keep, "newest-component-ceiling/") {
				oldRecord, err := erasureledger.Decode(po.body)
				if err != nil {
					return fmt.Errorf("decode prior component ceiling %s: %w", prev, err)
				}
				oldCeiling, ok := oldRecord.Payload.(erasureledger.ComponentReservation)
				if !ok {
					return fmt.Errorf("prior component ceiling %s has payload %T", prev, oldRecord.Payload)
				}
				newRecord, err := erasureledger.Decode(obj.body)
				if err != nil {
					return fmt.Errorf("decode new component ceiling %s: %w", obj.name, err)
				}
				newCeiling, ok := newRecord.Payload.(erasureledger.ComponentReservation)
				if !ok {
					return fmt.Errorf("new component ceiling %s has payload %T", obj.name, newRecord.Payload)
				}
				if oldCeiling.Ceiling > newCeiling.Ceiling {
					obj.keep = ""
					return nil
				}
			}
			po.keep = ""
			p.s.saveObject(po)
		}
	}
	p.s.saveKeepMarker(obj.keep, obj.name)
	return nil
}

func (p *Provider) confirmedObjects() []*object {
	const pageSize = 256
	var out []*object
	after := ""
	for {
		objects, last := p.s.page(after, pageSize)
		out = append(out, objects...)
		if len(objects) == 0 || objects[len(objects)-1].name >= last {
			return out
		}
		after = objects[len(objects)-1].name
	}
}

func (p *Provider) keepClass(rec erasureledger.Record) (string, error) {
	if _, ok := rec.Payload.(erasureledger.ComponentReservation); !ok {
		return keepClass(rec), nil
	}
	bindings := erasureledger.NewStreamBindings()
	for _, obj := range p.confirmedObjects() {
		prior, err := erasureledger.Decode(obj.body)
		if err != nil {
			return "", fmt.Errorf("decode confirmed binding evidence %s: %w", obj.name, err)
		}
		if prior.Kind == erasureledger.KindStreamBinding {
			if err := bindings.AddConfirmed(prior); err != nil {
				return "", fmt.Errorf("add confirmed stream binding %s: %w", obj.name, err)
			}
		}
	}
	key, err := bindings.ComponentKeyFor(rec)
	if err != nil {
		return "", fmt.Errorf("derive component key: %w", err)
	}
	return ComponentCeilingKeepClass(key), nil
}

func (p *Provider) coversConfirmedExclusions(candidate *object, exclusion erasureledger.RandomExclusion) (bool, error) {
	for _, old := range p.confirmedObjects() {
		if old.name == candidate.name {
			continue
		}
		record, err := erasureledger.Decode(old.body)
		if err != nil {
			return false, fmt.Errorf("decode confirmed record %s: %w", old.name, err)
		}
		prior, ok := record.Payload.(erasureledger.RandomExclusion)
		if !ok || prior.Class != exclusion.Class {
			continue
		}
		for _, id := range prior.IDs {
			if !slices.Contains(exclusion.IDs, id) {
				return false, nil
			}
		}
	}
	return true, nil
}

func (p *Provider) hasDependentComponentCeiling(binding *object) (bool, error) {
	for _, candidate := range p.confirmedObjects() {
		if candidate.name == binding.name || candidate.epoch != binding.epoch || candidate.stream != binding.stream {
			continue
		}
		record, err := erasureledger.Decode(candidate.body)
		if err != nil {
			return false, fmt.Errorf("decode confirmed component evidence %s: %w", candidate.name, err)
		}
		if _, ok := record.Payload.(erasureledger.ComponentReservation); ok {
			return true, nil
		}
	}
	return false, nil
}

func (p *Provider) exclusionCoveredByKeeper(candidate *object, exclusion erasureledger.RandomExclusion) (bool, error) {
	keep := randomExclusionKeepClass(exclusion.Class)
	name := p.s.keepMarker(keep)
	if name == "" || name == candidate.name {
		return false, nil
	}
	keeper := p.s.getObject(name)
	if keeper == nil || keeper.state != stateConfirmed {
		return false, nil
	}
	record, err := erasureledger.Decode(keeper.body)
	if err != nil {
		return false, fmt.Errorf("decode exclusion keeper %s: %w", name, err)
	}
	cover, ok := record.Payload.(erasureledger.RandomExclusion)
	if !ok || cover.Class != exclusion.Class {
		return false, nil
	}
	for _, id := range exclusion.IDs {
		if !slices.Contains(cover.IDs, id) {
			return false, nil
		}
	}
	return true, nil
}

// keepClass names the expirer guard for payloads whose key is in the payload:
// newest scalar reservation per allocator, newest epoch per lineage, and the
// covering exclusion snapshot per random-id class. Component reservations
// need their stream binding and use the full ComponentKey in Provider.
func keepClass(rec erasureledger.Record) string {
	switch body := rec.Payload.(type) {
	case erasureledger.Reservation:
		return "newest-reservation/" + string(body.Allocator)
	case erasureledger.Epoch:
		return "newest-epoch/" + KeyName(body.Lineage)
	case erasureledger.RandomExclusion:
		return randomExclusionKeepClass(body.Class)
	}
	return ""
}

func randomExclusionKeepClass(class erasureledger.RandomClass) string {
	return fmt.Sprintf("random-id-exclusion/%d", class)
}

func ComponentCeilingKeepClass(key erasureledger.ComponentKey) string {
	return fmt.Sprintf("newest-component-ceiling/%s/%s/%s",
		base64.RawURLEncoding.EncodeToString(key.Lineage[:]),
		base64.RawURLEncoding.EncodeToString(key.Stream[:]), key.Allocator)
}

// movedKeep reports whether a keep class belongs to the newest record of its
// class, so the marker moves to each new arrival. A random-id marker moves only
// after confirmation proved the new record covers all existing exclusions in
// that class.
func movedKeep(class string) bool {
	return strings.HasPrefix(class, "newest-reservation/") ||
		strings.HasPrefix(class, "newest-epoch/") ||
		strings.HasPrefix(class, "newest-component-ceiling/") ||
		strings.HasPrefix(class, "random-id-exclusion/")
}

// list is the paginated listing of opaque keys. Each page carries the
// count the provider intended for it, which is the count the walk checks, and
// the final page carries the completion marker. Entries are opaque keys and the
// provider's own arrival reading: nothing derived from a body.
func (p *Provider) list(after string, pageSize int) (ListPage, error) {
	if pageSize < 1 {
		return ListPage{}, fmt.Errorf("synthledger: page size %d is not positive", pageSize)
	}
	p.pages++
	pageNo := p.pages
	if p.faults.FailPage == pageNo {
		return ListPage{}, ErrInjectedPage
	}
	objs, last := p.s.page(after, pageSize)
	if p.faults.DropPage == pageNo && len(objs) > 0 {
		// The whole page vanishes and the walk continues past it, so the
		// records it carried are simply missing from the enumeration.
		objs, last = p.s.page(objs[len(objs)-1].name, pageSize)
	}
	declared := len(objs)
	if p.faults.TruncatePage == pageNo && declared > 1 {
		objs = objs[:declared-1]
	}
	page := ListPage{Count: declared, Entries: make([]ListEntry, 0, len(objs))}
	for _, obj := range objs {
		page.Entries = append(page.Entries, ListEntry{Key: obj.key, arrivalNanos: obj.arrival.clockNanos})
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
func (p *Provider) get(key erasureledger.OperationKey) (ReplayObject, error) {
	obj := p.s.getObject(KeyName(key))
	if obj == nil {
		if marker := p.s.readMarker(KeyName(key)); marker != nil && !p.hideMarkers {
			return ReplayObject{Key: key, Arrival: marker.arrival, logical: true}, nil
		}
		return ReplayObject{}, fmt.Errorf("%w: key %s", ErrNotFound, KeyName(key))
	}
	if obj.state != stateConfirmed {
		return ReplayObject{}, fmt.Errorf("%w: key %s is stored and unconfirmed",
			ErrNotFound, KeyName(key))
	}
	rec, err := erasureledger.Decode(obj.body)
	if err != nil {
		return ReplayObject{}, fmt.Errorf("synthledger: stored body does not decode: %w", err)
	}
	if rec.OpKey != key {
		return ReplayObject{}, fmt.Errorf("synthledger: stored body names key %s, the listing named %s",
			KeyName(rec.OpKey), KeyName(key))
	}
	return ReplayObject{
		Key: key, Record: rec, Body: slices.Clone(obj.body),
		Arrival: obj.arrival, keep: obj.keep,
	}, nil
}

// attributes is the verifier's attribute read: the provider's account of a key,
// with no body. A quarantined write reports its quarantine, which is how a
// dead-man check sees a fenced write that a listing will never show.
func (p *Provider) attributes(key erasureledger.OperationKey) (ObjectAttributes, error) {
	name := KeyName(key)
	if obj := p.s.getObject(name); obj != nil {
		return ObjectAttributes{
			Key: key, Bytes: len(obj.body), Arrival: obj.arrival,
			Epoch: obj.epoch, Stream: obj.stream, Seq: obj.seq,
			Confirmed: obj.state == stateConfirmed, keep: obj.keep,
		}, nil
	}
	if marker := p.s.readMarker(name); marker != nil {
		return ObjectAttributes{Key: key, Arrival: marker.arrival, Logical: true}, nil
	}
	for _, obj := range p.s.quarantinedObjects() {
		if obj.name == name {
			return ObjectAttributes{
				Key: key, Bytes: len(obj.body), Arrival: obj.arrival,
				Epoch: obj.epoch, Stream: obj.stream, Seq: obj.seq,
				Quarantined: true,
			}, nil
		}
	}
	return ObjectAttributes{}, fmt.Errorf("%w: key %s", ErrNotFound, name)
}

// arrival is the verifier's independent per-stream arrival record.
func (p *Provider) arrival(epoch int64, stream erasureledger.StreamID) (ArrivalRecord, error) {
	out := ArrivalRecord{Epoch: epoch, Stream: stream, found: false}
	for _, v := range p.verifs {
		if v.epoch == epoch && v.stream == stream {
			if v.tamper {
				return ArrivalRecord{}, fmt.Errorf("%w: epoch %d stream %s",
					ErrInjectedVerifier, epoch, KeyName(stream))
			}
			out = ArrivalRecord{
				Epoch: epoch, Stream: stream,
				Arrivals: v.count, HighWater: v.high, found: true,
			}
		}
	}
	if !out.found {
		return ArrivalRecord{}, fmt.Errorf("%w: no arrival record for epoch %d stream %s",
			ErrNotFound, epoch, KeyName(stream))
	}
	return out, nil
}

// pruneThrough writes the expirer's per-stream checkpoint. Expiry keeps
// contiguity checkable: the checkpoint is the ledger's statement that the
// sequences at or below it were pruned on purpose, so a walk can tell a prune
// from a gap.
func (p *Provider) pruneThrough(epoch int64, stream erasureledger.StreamID, through int64) error {
	sk := streamKey{epoch: epoch, stream: stream}
	str, ok := p.s.streamState(sk)
	if !ok {
		return fmt.Errorf("%w: epoch %d stream %s has no arrivals", ErrNotFound, epoch, KeyName(stream))
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
func (p *Provider) delete(key erasureledger.OperationKey) error {
	obj := p.s.getObject(KeyName(key))
	if obj == nil {
		return fmt.Errorf("%w: key %s", ErrNotFound, KeyName(key))
	}
	sk := streamKey{epoch: obj.epoch, stream: obj.stream}
	str, ok := p.s.streamState(sk)
	if !ok || str.pruned < obj.seq {
		return fmt.Errorf("%w: sequence %d is not covered by a pruned-through checkpoint",
			ErrNotPruned, obj.seq)
	}
	if obj.keep != "" {
		return fmt.Errorf("%w: key %s holds the %s guard", ErrNotDeletable, KeyName(key), obj.keep)
	}
	record, err := erasureledger.Decode(obj.body)
	if err != nil {
		return fmt.Errorf("synthledger: decode record before expiry: %w", err)
	}
	if _, ok := record.Payload.(erasureledger.StreamBinding); ok {
		dependent, err := p.hasDependentComponentCeiling(obj)
		if err != nil {
			return fmt.Errorf("synthledger: check binding dependencies before expiry: %w", err)
		}
		if dependent {
			return fmt.Errorf("%w: key %s carries a binding required by a component ceiling",
				ErrNotDeletable, KeyName(key))
		}
	}
	if exclusion, ok := record.Payload.(erasureledger.RandomExclusion); ok {
		covered, err := p.exclusionCoveredByKeeper(obj, exclusion)
		if err != nil {
			return fmt.Errorf("synthledger: check exclusion coverage before expiry: %w", err)
		}
		if !covered {
			return fmt.Errorf("%w: key %s has an uncovered random-id exclusion", ErrNotDeletable, KeyName(key))
		}
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
func (p *Provider) createSentinel() (erasureledger.OperationKey, error) {
	key, err := erasureledger.NewOperationKey(rand.Reader)
	if err != nil {
		return erasureledger.OperationKey{}, err
	}
	marker := &object{
		name: KeyName(key), key: key,
		arrival: p.arriveAt(nil), state: stateConfirmed,
	}
	if !p.s.writeMarker(marker.name, marker) {
		return erasureledger.OperationKey{}, fmt.Errorf("%w: sentinel %s", ErrObjectExists, marker.name)
	}
	return key, nil
}

// Principals. Each is one authorized credential over the same provider, and the
// method set below is the whole capability. The least-privilege conformance
// case reads that method set by reflection, so a method added to a principal
// fails the case rather than widening a credential quietly.

// Writer is the on-alpha ledger credential: conditional create, plus the arrival
// confirmation of its own write. It has no overwrite, delete, get, list,
// retention, policy, fence, sentinel, prune, or verifier capability, and
// Create's one argument is a ledger record, so the writer addresses no bucket,
// prefix, version, lock mode, retention deadline, clock or storage name. The
// operation key is a field of the record and the caller supplies it: this seam
// mints no ledger key and claims no entropy of its own, because key generation
// is the codec's NewOperationKey contract, which a producer has to enforce
// itself. See TestLedgerOperationKeyIsTheCallers for the key rules the seam
// does hold.
type Writer struct {
	p      *Provider
	epoch  int64
	stream erasureledger.StreamID
}

// Create stores rec under its opaque key with a create-once condition.
func (w Writer) Create(rec erasureledger.Record) (*PendingWrite, error) {
	w.p.mu.Lock()
	defer w.p.mu.Unlock()
	return w.p.create(w, rec)
}

// Confirm accepts the provider's arrival evidence for the write the handle
// names. The handle is bound to this credential: one issued to another stream
// or epoch is refused, and the refusal reads nothing.
func (w Writer) Confirm(h *PendingWrite) (*CreateReceipt, error) {
	w.p.mu.Lock()
	defer w.p.mu.Unlock()
	return w.p.confirm(w, h)
}

// Replayer is the escrow-side credential: get and list, nothing else. It
// cannot in particular create the sentinel that is meant to vouch for its own
// listing, and Get's argument is an opaque key, so a replayer cannot
// enumerate bodies by prefix, fetch a body range, or read attributes.
type Replayer struct{ p *Provider }

// Get reads one replayable record by opaque key.
func (r Replayer) Get(key erasureledger.OperationKey) (ReplayObject, error) {
	r.p.mu.Lock()
	defer r.p.mu.Unlock()
	return r.p.get(key)
}

// List reads one page of opaque keys.
func (r Replayer) List(after string, pageSize int) (ListPage, error) {
	r.p.mu.Lock()
	defer r.p.mu.Unlock()
	return r.p.list(after, pageSize)
}

// Verifier is the off-alpha credential: attributes and arrival records only.
// It confirms arrival for the dead-man check and never writes, so a
// verification failure cannot nudge the ledger.
type Verifier struct{ p *Provider }

// Attributes reads the provider's account of one key, with no body.
func (v Verifier) Attributes(key erasureledger.OperationKey) (ObjectAttributes, error) {
	v.p.mu.Lock()
	defer v.p.mu.Unlock()
	return v.p.attributes(key)
}

// Arrival reads the independent per-stream arrival record.
func (v Verifier) Arrival(epoch int64, stream erasureledger.StreamID) (ArrivalRecord, error) {
	v.p.mu.Lock()
	defer v.p.mu.Unlock()
	return v.p.arrival(epoch, stream)
}

// Expirer is the only credential with a delete right, and it must checkpoint
// before it exercises it.
type Expirer struct{ p *Provider }

// PruneThrough records the per-stream pruned-through checkpoint.
func (e Expirer) PruneThrough(epoch int64, stream erasureledger.StreamID, through int64) error {
	e.p.mu.Lock()
	defer e.p.mu.Unlock()
	return e.p.pruneThrough(epoch, stream, through)
}

// Delete removes one record whose stream is pruned through its sequence.
func (e Expirer) Delete(key erasureledger.OperationKey) error {
	e.p.mu.Lock()
	defer e.p.mu.Unlock()
	return e.p.delete(key)
}

// SentinelAuthorizer is the separately authorized logical-sentinel creator.
// MAIN-1362 accepts the sentinel as a separately authorized marker, and this
// is the only principal that can mint one.
type SentinelAuthorizer struct{ p *Provider }

// CreateSentinel mints the logical completion marker.
func (a SentinelAuthorizer) CreateSentinel() (erasureledger.OperationKey, error) {
	a.p.mu.Lock()
	defer a.p.mu.Unlock()
	return a.p.createSentinel()
}

// PendingWrite is the handle Create returns. Its fields are unexported, so a
// caller cannot forge one, point it at another key, or use it to read
// anything. It carries the credential that issued it, and Confirm accepts it
// only from that credential: a handle is a stream's own in-flight write.
type PendingWrite struct {
	epoch   int64
	stream  erasureledger.StreamID
	key     erasureledger.OperationKey
	seq     int64
	arrival ArrivalEvidence
}

// Arrival is the provider's reading for the write the handle names.
func (h *PendingWrite) Arrival() ArrivalEvidence { return h.arrival }

// Seq is the stream sequence of the write the handle names.
func (h *PendingWrite) Seq() int64 { return h.seq }

// Key is the operation key of the write the handle names.
func (h *PendingWrite) Key() erasureledger.OperationKey { return h.key }

// CreateReceipt is the writer's confirmation of a durable arrival: the opaque
// key, the provider's arrival reading, the stream sequence the provider
// accepted, and the byte count it stored.
type CreateReceipt struct {
	Key     erasureledger.OperationKey
	Arrival ArrivalEvidence
	Seq     int64
	Bytes   int
}

// ListEntry is one listing row. It is an opaque key and the provider's own
// arrival reading, and nothing else: no body, no owner id, no file id, no
// allocator, no epoch, no stream, no sequence, no record kind, and no
// exported member that could expose a body.
type ListEntry struct {
	Key          erasureledger.OperationKey
	arrivalNanos int64
}

// ArrivalNanos is the provider's arrival clock reading for the listed key.
func (e ListEntry) ArrivalNanos() int64 { return e.arrivalNanos }

// ListPage is one page of the walk.
type ListPage struct {
	Entries      []ListEntry
	Count        int
	Continuation string
	Complete     bool
}

// ReplayObject is the replayer's read of one record.
type ReplayObject struct {
	Key     erasureledger.OperationKey
	Record  erasureledger.Record
	Body    []byte
	Arrival ArrivalEvidence

	// logical marks the sentinel marker: a marker the replayer can see, with
	// no ledger record behind it.
	logical bool
	keep    string
}

// Logical reports whether the read is the sentinel marker rather than a
// ledger record.
func (o ReplayObject) Logical() bool { return o.logical }

// Keep is the expirer keep class the record holds, or "".
func (o ReplayObject) Keep() string { return o.keep }

// ObjectAttributes is the verifier's attribute reading. It has no body, no
// record, and no exported member that exposes one.
type ObjectAttributes struct {
	Key     erasureledger.OperationKey
	Bytes   int
	Arrival ArrivalEvidence
	Epoch   int64
	Stream  erasureledger.StreamID
	Seq     int64

	Confirmed   bool
	Logical     bool
	Quarantined bool
	keep        string
}

// Keep is the expirer keep class the object holds, or "".
func (a ObjectAttributes) Keep() string { return a.keep }

// ArrivalRecord is the verifier's per-stream arrival record. Arrivals counts
// the stream's confirmed arrivals and HighWater is the highest confirmed
// sequence, which a prune never lowers.
type ArrivalRecord struct {
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

// KeyName is the storage name for an opaque identity: its own bytes in hex. A
// name is therefore an opaque value that carries no body identifier, and a
// stream's name is no more revealing than a record's.
func KeyName[T opaqueID](id T) string { return hex.EncodeToString(id[:]) }

// keyFromName is KeyName's inverse.
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

// Replayer mints the replayer credential.
func (p *Provider) Replayer() Replayer { return Replayer{p: p} }

// Verifier mints the verifier credential.
func (p *Provider) Verifier() Verifier { return Verifier{p: p} }

// Expirer mints the expirer credential.
func (p *Provider) Expirer() Expirer { return Expirer{p: p} }

// SentinelAuthorizer mints the separately authorized sentinel credential.
func (p *Provider) SentinelAuthorizer() SentinelAuthorizer { return SentinelAuthorizer{p: p} }

// WriterIdentity is the (epoch, stream) a writer credential is bound to. It is
// a function and not a method so the writer's method set stays its capability.
func WriterIdentity(w Writer) (int64, erasureledger.StreamID) { return w.epoch, w.stream }

// SetPageFaults replaces the listing faults. Page numbers count List calls
// over the provider's life.
func (p *Provider) SetPageFaults(f PageFaults) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.faults = f
}

// HideMarkers makes stored sentinel markers unreadable to the replayer.
func (p *Provider) HideMarkers() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hideMarkers = true
}

// InjectArrivalRecord appends a verifier arrival record for a stream, which
// the verifier reports in place of the provider's own until a later one.
func (p *Provider) InjectArrivalRecord(epoch int64, stream erasureledger.StreamID, arrivals, highWater int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.verifs = append(p.verifs, verifierEntry{epoch: epoch, stream: stream, count: arrivals, high: highWater})
}

// TamperArrivalRecord appends an unverifiable arrival record for a stream.
func (p *Provider) TamperArrivalRecord(epoch int64, stream erasureledger.StreamID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.verifs = append(p.verifs, verifierEntry{epoch: epoch, stream: stream, tamper: true})
}

// AdvanceTime moves the simulated provider clock forward by nanos.
func (p *Provider) AdvanceTime(nanos int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clock += nanos
	p.s.saveProviderState(p.clock, p.arrive)
}

// StoredBody reads the bytes the medium holds under a storage name, in any
// state, bypassing every principal.
func (p *Provider) StoredBody(name string) ([]byte, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	obj := p.s.getObject(name)
	if obj == nil {
		return nil, false
	}
	return slices.Clone(obj.body), true
}

// MediumCreateOnce stores an unconfirmed object straight into the medium's
// create-once path, bypassing the seam's own key check, so a test can pin the
// arm's refusal in its own right.
func (p *Provider) MediumCreateOnce(key erasureledger.OperationKey, body []byte, pos Position) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.s.saveObjectOnce(&object{
		name: KeyName(key), key: key, body: slices.Clone(body),
		epoch: pos.Epoch, stream: pos.Stream, seq: pos.Seq, state: statePending,
	})
}

// QuarantineCount is the number of writes the quarantine holds.
func (p *Provider) QuarantineCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.s.quarantinedObjects())
}

// StreamRef names one (epoch, replica stream).
type StreamRef struct {
	Epoch  int64
	Stream erasureledger.StreamID
}

// StreamInventory is the ledger's own (epoch, stream) set, read from the
// medium rather than from a listing.
func (p *Provider) StreamInventory() []StreamRef {
	p.mu.Lock()
	defer p.mu.Unlock()
	stored := p.s.streams()
	out := make([]StreamRef, 0, len(stored))
	for _, k := range stored {
		out = append(out, StreamRef{Epoch: k.epoch, Stream: k.stream})
	}
	return out
}
