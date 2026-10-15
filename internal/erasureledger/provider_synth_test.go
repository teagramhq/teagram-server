package erasureledger_test

// The synthetic provider lives in the synthledger package so the store tests
// can share it. This file binds the conformance suite to it: the capability
// vocabulary the suite asserts principals against, the short names the cases
// use, and the walk and completeness checks, which are the suite's own logic
// and not the provider's.

import (
	"bytes"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"testing"

	"github.com/teagramhq/teagram-server/internal/erasureledger"
	"github.com/teagramhq/teagram-server/internal/erasureledger/synthledger"
)

type (
	synthProvider      = synthledger.Provider
	store              = synthledger.Store
	writer             = synthledger.Writer
	replayer           = synthledger.Replayer
	verifier           = synthledger.Verifier
	expirer            = synthledger.Expirer
	sentinelAuthorizer = synthledger.SentinelAuthorizer
	pendingWrite       = synthledger.PendingWrite
	createReceipt      = synthledger.CreateReceipt
	listPage           = synthledger.ListPage
	replayObject       = synthledger.ReplayObject
	objectAttributes   = synthledger.ObjectAttributes
	arrivalRecord      = synthledger.ArrivalRecord
	arrivalEvidence    = synthledger.ArrivalEvidence
	listEntry          = synthledger.ListEntry
	pageFaults         = synthledger.PageFaults
)

var (
	errObjectExists     = synthledger.ErrObjectExists
	errNotFound         = synthledger.ErrNotFound
	errQuarantined      = synthledger.ErrQuarantined
	errSequenceGap      = synthledger.ErrSequenceGap
	errAlreadyConfirmed = synthledger.ErrAlreadyConfirmed
	errCredential       = synthledger.ErrCredential
	errInjectedPage     = synthledger.ErrInjectedPage
	errNotPruned        = synthledger.ErrNotPruned
	errNotDeletable     = synthledger.ErrNotDeletable
	errInjectedVerifier = synthledger.ErrInjectedVerifier

	newSynthProvider          = synthledger.New
	newMemoryStore            = synthledger.NewMemoryStore
	openDirStore              = synthledger.OpenDirStore
	componentCeilingKeepClass = synthledger.ComponentCeilingKeepClass
)

// keyName is the provider's storage name for an opaque identity.
func keyName[T ~[16]byte](id T) string { return synthledger.KeyName(id) }

// writerStream is the replica stream a writer credential is bound to.
func writerStream(w writer) erasureledger.StreamID {
	_, stream := synthledger.WriterIdentity(w)
	return stream
}

// writerEpoch is the epoch a writer credential is bound to.
func writerEpoch(w writer) int64 {
	epoch, _ := synthledger.WriterIdentity(w)
	return epoch
}

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
	open func(t testing.TB) store
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
func streamInventory(p *synthProvider) []seqKey {
	stored := p.StreamInventory()
	out := make([]seqKey, 0, len(stored))
	for _, k := range stored {
		out = append(out, seqKey{epoch: k.Epoch, stream: k.Stream})
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
			if !obj.Arrival.Verify(obj.Body) {
				return out, fmt.Errorf("page %d key %s: the arrival evidence does not describe the stored bytes",
					out.Pages, keyName(e.Key))
			}
			out.Objects = append(out.Objects, obj)
			out.Keys = append(out.Keys, e.Key)
			out.MaxArrival = max(out.MaxArrival, obj.Arrival.ArrivalSeq())
		}
		if page.Complete {
			// The sentinel is a marker and not a ledger object, so it is read
			// by name. A walk that cannot see it reports no sighting, and the
			// completeness checks then refuse to call the enumeration complete.
			if sentinel != (erasureledger.OperationKey{}) {
				if marker, err := r.Get(sentinel); err == nil && marker.Logical() {
					out.Objects = append(out.Objects, marker)
					out.MaxArrival = max(out.MaxArrival, marker.Arrival.ArrivalSeq())
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
		if obj.Logical() {
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
		} else if marker.Arrival.ArrivalSeq() != in.Walk.MaxArrival {
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
		if obj.Logical() {
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
		if obj.Logical() {
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
