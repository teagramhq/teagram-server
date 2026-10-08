package erasureledger_test

// The synthetic provider conformance suite. Every case runs over
// both storage arms, the in-memory map and the disposable local directory, and
// every case is a synthetic capability result: it pins the shape of
// the accepted MAIN-1360 provider contract as the MAIN-1452 acceptance list
// states it. Nothing here asserts provider consistency, Object Lock,
// versioning, retention, restore readiness, or any real deployment property.

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/teagramhq/teagram-server/internal/erasureledger"
)

// conformancePageSize and conformanceObjects are the walk's shape: 34 records
// listed five at a time is a seven-page walk (5,5,5,5,5,5,4), the page count
// the acceptance names.
const (
	conformancePageSize  = 5
	conformanceObjects   = 34
	conformanceWalkPages = 7
)

// drawnKey draws a fresh opaque operation key from the system random source.
// Keys are randomness, so a listing row is an opaque value and a retried write
// is findable.
func drawnKey(t *testing.T) erasureledger.OperationKey {
	t.Helper()
	key, err := erasureledger.NewOperationKey(rand.Reader)
	if err != nil {
		t.Fatalf("draw an operation key: %v", err)
	}
	return key
}

// The body builders keep a case readable: each is an accepted constructor with
// a fatal failure, in the style the codec's own tests use.

func accountBody(t *testing.T, id int64) erasureledger.Account {
	t.Helper()
	b, err := erasureledger.NewAccount(id)
	if err != nil {
		t.Fatalf("NewAccount(%d): %v", id, err)
	}
	return b
}

func fileBody(t *testing.T, id int64) erasureledger.File {
	t.Helper()
	b, err := erasureledger.NewFile(id)
	if err != nil {
		t.Fatalf("NewFile(%d): %v", id, err)
	}
	return b
}

func selfDeleteBody(t *testing.T, owner int64, local int32) erasureledger.MessageCopies {
	t.Helper()
	b, err := erasureledger.NewSelfDelete(owner, local)
	if err != nil {
		t.Fatalf("NewSelfDelete(%d,%d): %v", owner, local, err)
	}
	return b
}

func channelPostsBody(t *testing.T, posts ...erasureledger.ChannelPost) erasureledger.ChannelPostCopies {
	t.Helper()
	b, err := erasureledger.NewChannelPostCopies(posts...)
	if err != nil {
		t.Fatalf("NewChannelPostCopies: %v", err)
	}
	return b
}

func exclusionBody(t *testing.T, class erasureledger.RandomClass, ids ...int64) erasureledger.RandomExclusion {
	t.Helper()
	b, err := erasureledger.NewRandomExclusion(class, ids...)
	if err != nil {
		t.Fatalf("NewRandomExclusion: %v", err)
	}
	return b
}

func reservationBody(t *testing.T, allocator string, ceiling int64) erasureledger.Reservation {
	t.Helper()
	name, err := erasureledger.ValidateAllocator(allocator)
	if err != nil {
		t.Fatalf("ValidateAllocator(%q): %v", allocator, err)
	}
	b, err := erasureledger.NewReservation(name, ceiling)
	if err != nil {
		t.Fatalf("NewReservation(%s,%d): %v", name, ceiling, err)
	}
	return b
}

func epochBody(t *testing.T, number int64, lineage erasureledger.LineageID,
	level erasureledger.EpochLevel) erasureledger.Epoch {
	t.Helper()
	b, err := erasureledger.NewEpoch(number, lineage, level)
	if err != nil {
		t.Fatalf("NewEpoch(%d): %v", number, err)
	}
	return b
}

func galleryBody(t *testing.T, owner, revision int64, entries ...erasureledger.GalleryEntry) erasureledger.GalleryDelete {
	t.Helper()
	b, err := erasureledger.NewGalleryDelete(owner, revision, entries...)
	if err != nil {
		t.Fatalf("NewGalleryDelete(%d): %v", owner, err)
	}
	return b
}

func receiptBody(t *testing.T, owner, clientFileID int64, state erasureledger.ReceiptState,
	fileID int64) erasureledger.ReceiptTerminal {
	t.Helper()
	b, err := erasureledger.NewReceiptTerminal(owner, clientFileID, state, fileID)
	if err != nil {
		t.Fatalf("NewReceiptTerminal(%d): %v", owner, err)
	}
	return b
}

// envelope builds a record with an explicit epoch, stream, sequence and opaque
// key, which is the identity a provider case reasons about.
func envelope(t *testing.T, epoch int64, stream erasureledger.StreamID, seq int64,
	key erasureledger.OperationKey, payload erasureledger.Payload) erasureledger.Record {
	t.Helper()
	rec, err := erasureledger.NewRecord(payload.Kind(), epoch, stream, seq, key, payload)
	if err != nil {
		t.Fatalf("NewRecord kind=%v sequence=%d: %v", payload.Kind(), seq, err)
	}
	return rec
}

// sampleWidths spells one fixture record's identifiers at the widths the
// vocabulary serves: local ids are the signed-32-bit wire width, so the mask is
// the width itself and the fixture's identifiers are exact.
func sampleWidths(seq int64) (int64, int32) {
	id := 0x1000 + seq*7
	return id, int32(id & 0x7fff_ffff)
}

// sampleRecord builds a valid record of a rotating kind, with a drawn opaque
// key, the given envelope, and identifiers distinctive enough that a leak of
// body content into a listing row is detectable. The allocator name is the
// literal users_id_seq, which cannot appear inside a hex key, so the
// listing-privacy case has a real string to scan for.
func sampleRecord(t *testing.T, epoch int64, stream erasureledger.StreamID, seq int64) erasureledger.Record {
	t.Helper()
	id, local := sampleWidths(seq)
	var payload erasureledger.Payload
	switch seq % 9 {
	case 1:
		payload = accountBody(t, id)
	case 2:
		payload = fileBody(t, id)
	case 3:
		payload = selfDeleteBody(t, id, local)
	case 4:
		payload = channelPostsBody(t, erasureledger.ChannelPost{ChannelID: id, LocalID: local})
	case 5:
		payload = exclusionBody(t, erasureledger.RandomClassChannel, id)
	case 6:
		payload = reservationBody(t, "users_id_seq", id)
	case 7:
		payload = epochBody(t, epoch, lineageID(byte(seq&0x7f)), erasureledger.EpochEstablished)
	case 8:
		payload = galleryBody(t, id, id, erasureledger.GalleryEntry{FileID: id, ClientFileID: id})
	default:
		payload = receiptBody(t, id, id, erasureledger.ReceiptComplete, id)
	}
	return envelope(t, epoch, stream, seq, drawnKey(t), payload)
}

// seed writes n records on one stream, confirming each arrival before the next
// write, which is the serial flusher's order. It returns the keys in write
// order, index i being the record at sequence i+1.
func seed(t *testing.T, w writer, n int) []erasureledger.OperationKey {
	t.Helper()
	keys := make([]erasureledger.OperationKey, 0, n)
	for seq := int64(1); seq <= int64(n); seq++ {
		rec := sampleRecord(t, w.epoch, w.stream, seq)
		receipt, err := flush(t, w, rec)
		if err != nil {
			t.Fatalf("seed sequence %d: %v", seq, err)
		}
		if receipt.Seq != seq {
			t.Fatalf("seed sequence %d: provider accepted %d", seq, receipt.Seq)
		}
		keys = append(keys, rec.OpKey)
	}
	return keys
}

// flush performs one writer cycle: conditional create, then the arrival
// confirmation of that same write.
func flush(t *testing.T, w writer, rec erasureledger.Record) (*createReceipt, error) {
	t.Helper()
	h, err := w.Create(rec)
	if err != nil {
		return nil, err
	}
	return w.Confirm(h)
}

// seedConformanceSet writes the 34-record set for the walk cases, alternating
// between two replica streams, so the enumeration covers two per-stream
// sequence spaces at once.
func seedConformanceSet(t *testing.T, p *synthProvider) []erasureledger.OperationKey {
	t.Helper()
	streamA, streamB := streamID(0x50), streamID(0x60)
	wA, wB := p.newWriter(1, streamA), p.newWriter(1, streamB)
	keys := make([]erasureledger.OperationKey, 0, conformanceObjects)
	nextA, nextB := int64(1), int64(1)
	for i := 1; i <= conformanceObjects; i++ {
		w, seq := wA, nextA
		if i%2 == 0 {
			w, seq = wB, nextB
		}
		rec := sampleRecord(t, 1, w.stream, seq)
		if _, err := flush(t, w, rec); err != nil {
			t.Fatalf("seed arrival %d: %v", i, err)
		}
		if i%2 == 0 {
			nextB++
		} else {
			nextA++
		}
		keys = append(keys, rec.OpKey)
	}
	return keys
}

// TestLedgerPrincipalCapabilitiesAreLeastPrivileged is the accepted writer,
// verifier, replayer and expirer authorization, observed at the seam: each
// principal offers exactly the methods it is authorized to
// offer, satisfies exactly its own capability interface, and so cannot exercise
// any operation outside it.
func TestLedgerPrincipalCapabilitiesAreLeastPrivileged(t *testing.T) {
	t.Parallel()
	runOverArms(t, func(t *testing.T, arm providerArm, p *synthProvider) {
		t.Helper()
		w := p.newWriter(1, streamID(0x50))
		rep, v, ex := replayer{p: p}, verifier{p: p}, expirer{p: p}
		sentinel := sentinelAuthorizer{p: p}

		checkPrincipal(t, w, principalCapability{
			name:     "writer",
			methods:  []string{"Confirm", "Create"},
			capacity: "ledgerCreator",
		})
		checkPrincipal(t, rep, principalCapability{
			name:     "replayer",
			methods:  []string{"Get", "List"},
			capacity: "ledgerReader",
		})
		checkPrincipal(t, v, principalCapability{
			name:     "verifier",
			methods:  []string{"Arrival", "Attributes"},
			capacity: "ledgerArrivalReader",
		})
		checkPrincipal(t, ex, principalCapability{
			name:     "expirer",
			methods:  []string{"Delete", "PruneThrough"},
			capacity: "ledgerDeleter",
		})
		checkPrincipal(t, sentinel, principalCapability{
			name:     "sentinelAuthorizer",
			methods:  []string{"CreateSentinel"},
			capacity: "ledgerSentinelCreator",
		})

		// The matrix, in both directions: a principal satisfies its own
		// capability interface and no other, so a write, a delete, a read, an
		// arrival read, and a sentinel are each reachable through exactly one
		// credential. A method added to a principal in a later change widens
		// a credential and fails here.
		for _, tc := range []struct {
			name      string
			principal any
			capacity  reflect.Type
		}{
			{"writer", w, reflect.TypeFor[ledgerCreator]()},
			{"replayer", rep, reflect.TypeFor[ledgerReader]()},
			{"verifier", v, reflect.TypeFor[ledgerArrivalReader]()},
			{"expirer", ex, reflect.TypeFor[ledgerDeleter]()},
			{"sentinelAuthorizer", sentinel, reflect.TypeFor[ledgerSentinelCreator]()},
		} {
			for _, typ := range capabilityTypes {
				want := typ == tc.capacity
				if got := reflect.TypeOf(tc.principal).Implements(typ); got != want {
					t.Errorf("%s satisfies %s = %v, want %v", tc.name, typ.Name(), got, want)
				}
			}
		}

		// The replayer in particular cannot write the sentinel that is meant to
		// vouch for its own listing: MAIN-1362 accepts the sentinel as a
		// separately authorized marker, so no ledger credential carries it.
		for _, principal := range []any{w, rep, v, ex} {
			if _, ok := principal.(ledgerSentinelCreator); ok {
				t.Errorf("%T can mint a sentinel", principal)
			}
		}

		// Create takes a ledger record and nothing else, so a writer cannot
		// address a bucket, a prefix, a version, a lock mode, a retention
		// deadline, a clock, or a key of its own choosing. Confirm takes the
		// handle Create returned, so it carries no read capability.
		assertUnarySignature(t, w, "Create", reflect.TypeFor[erasureledger.Record]())
		assertUnarySignature(t, w, "Confirm", reflect.TypeFor[*pendingWrite]())
		assertUnarySignature(t, rep, "Get", reflect.TypeFor[erasureledger.OperationKey]())
		assertUnarySignature(t, v, "Attributes", reflect.TypeFor[erasureledger.OperationKey]())
		assertUnarySignature(t, ex, "Delete", reflect.TypeFor[erasureledger.OperationKey]())
	})
}

// forbiddenProviderWords are the provider properties this seam must never
// claim, as method names.
var forbiddenProviderWords = []string{
	"lock", "version", "retention", "consistency", "restore", "ready",
	"policy", "credential", "acl", "encryption", "replicat", "presign",
	"multipart", "tag",
}

// assertUnarySignature asserts a method has exactly the one argument named,
// which is how a case proves a capability cannot address anything
// beyond its own object.
func assertUnarySignature(t *testing.T, principal any, method string, want reflect.Type) {
	t.Helper()
	mv := reflect.ValueOf(principal).MethodByName(method)
	if !mv.IsValid() {
		t.Fatalf("%T has no method %s", principal, method)
	}
	typ := mv.Type()
	if typ.NumIn() != 1 {
		t.Errorf("%T.%s takes %d arguments, want exactly one", principal, method, typ.NumIn())
		return
	}
	if typ.In(0) != want {
		t.Errorf("%T.%s argument = %v, want %v", principal, method, typ.In(0), want)
	}
}

// TestLedgerCreateOnceRefusesOverwrite is the accepted conditional-create rule:
// the writer has a create-once condition and no overwrite. The second create
// for a key is refused, and the first body is what the ledger keeps.
func TestLedgerCreateOnceRefusesOverwrite(t *testing.T) {
	t.Parallel()
	runOverArms(t, func(t *testing.T, arm providerArm, p *synthProvider) {
		t.Helper()
		w, rep := p.newWriter(1, streamID(0x50)), replayer{p: p}
		rec := sampleRecord(t, 1, w.stream, 1)
		first, err := flush(t, w, rec)
		if err != nil {
			t.Fatalf("first create: %v", err)
		}

		// A second create for the same opaque key is the create-once refusal.
		// The envelope of the retry is the same record, which is what a
		// retried flush looks like.
		if _, err := w.Create(rec); !errors.Is(err, errObjectExists) {
			t.Errorf("second create: err = %v, want the create-once refusal", err)
		}
		// A body change under the same key is refused too: it is an
		// overwrite, and the writer has no overwrite.
		changed := sampleRecord(t, 1, w.stream, 2)
		changed.OpKey = rec.OpKey
		if _, err := w.Create(changed); !errors.Is(err, errObjectExists) {
			t.Errorf("create with a new body under a taken key: err = %v, want the create-once refusal", err)
		}

		// The first body survives both refusals, byte for byte.
		got, err := rep.Get(rec.OpKey)
		if err != nil {
			t.Fatalf("get after the refusals: %v", err)
		}
		want, err := erasureledger.Encode(rec)
		if err != nil {
			t.Fatalf("encode the first record: %v", err)
		}
		if !bytes.Equal(got.Body, want) {
			t.Errorf("stored body changed under a taken key: %d bytes, want %d", len(got.Body), len(want))
		}
		if got.Arrival != first.Arrival {
			t.Errorf("stored arrival = %v, want the first arrival %v", got.Arrival, first.Arrival)
		}

		// A refusal is not a read. The writer has no get, so the refusal text
		// must not disclose the stored body: it can name the opaque key, and
		// that is all a writer is entitled to learn.
		_, err = w.Create(rec)
		if err == nil {
			t.Fatal("the create-once refusal disappeared")
		}
		denied := hex.EncodeToString(want)
		if strings.Contains(err.Error(), denied) {
			t.Errorf("the create-once refusal discloses the stored body: %s", err)
		}

		// The medium enforces create-once in its own right, and that is pinned
		// here at the arm level. The seam pre-checks the key, so the arms' own
		// refusal branch needs its own assertion: on the directory arm that
		// branch is the filesystem's exclusive create, so a dropped O_EXCL
		// fails here, and an arm that always refuses fails the free-name
		// control below.
		if err := p.s.saveObjectOnce(&object{
			name: keyName(rec.OpKey), key: rec.OpKey, body: slices.Clone(want),
			epoch: 1, stream: w.stream, seq: 2, state: statePending,
		}); !errors.Is(err, errObjectExists) {
			t.Errorf("second saveObjectOnce under a taken name: err = %v, want the medium's create-once refusal", err)
		}
		// The refusal leaves the stored object as the medium held it: the file
		// is never opened for writing, so the first body and arrival stand.
		after, err := rep.Get(rec.OpKey)
		if err != nil {
			t.Fatalf("get after the arm-level refusal: %v", err)
		}
		if !bytes.Equal(after.Body, want) || after.Arrival != first.Arrival {
			t.Errorf("the arm-level refusal changed the stored object: %d bytes, arrival %v",
				len(after.Body), after.Arrival)
		}
		// A free name goes in, so the refusal is about the taken name and not
		// about the call always failing. What the seam never confirmed stays
		// unreadable: reaching the confirmed state is the seam's rule, not
		// the medium's.
		free := drawnKey(t)
		if err := p.s.saveObjectOnce(&object{
			name: keyName(free), key: free, body: slices.Clone(want),
			epoch: 1, stream: w.stream, seq: 2,
		}); err != nil {
			t.Errorf("saveObjectOnce under a free name: %v", err)
		}
		if _, err := rep.Get(free); !errors.Is(err, errNotFound) {
			t.Errorf("get of an unconfirmed store-level object: err = %v, want not-found", err)
		}
	})
}

// TestLedgerArrivalEvidenceIsTheProviders is the accepted ordering rule and the
// MAIN-1452 acceptance that a successful create exposes confirmed arrival and
// checksum evidence distinct from a writer-supplied timestamp. The evidence is
// simulated, and it is the provider's: the codec has no timestamp field, the
// create call has no clock argument, the arrival type cannot be constructed by
// a caller, and the checksum is a function of the stored bytes.
func TestLedgerArrivalEvidenceIsTheProviders(t *testing.T) {
	t.Parallel()
	runOverArms(t, func(t *testing.T, arm providerArm, p *synthProvider) {
		t.Helper()
		w, rep, v := p.newWriter(1, streamID(0x50)), replayer{p: p}, verifier{p: p}

		// A record has no timestamp field, so a writer has nothing to order a
		// ledger by.
		for f := range reflect.TypeFor[erasureledger.Record]().Fields() {
			if strings.Contains(strings.ToLower(f.Name), "time") ||
				strings.Contains(strings.ToLower(f.Name), "stamp") {
				t.Errorf("Record has a time-shaped field %s", f.Name)
			}
		}
		// The arrival evidence is not a value a writer can build or edit: every
		// field is unexported, and none of them is a wall clock.
		arrivalType := reflect.TypeFor[arrivalEvidence]()
		if arrivalType.NumField() != 3 {
			t.Errorf("arrivalEvidence has %d fields, want the provider's three readings", arrivalType.NumField())
		}
		for f := range arrivalType.Fields() {
			if f.PkgPath == "" {
				t.Errorf("arrivalEvidence.%s is exported, so a writer could set it", f.Name)
			}
			if f.Type == reflect.TypeFor[time.Time]() {
				t.Errorf("arrivalEvidence.%s is a wall clock", f.Name)
			}
		}

		rec := sampleRecord(t, 1, w.stream, 1)
		body, err := erasureledger.Encode(rec)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		h, err := w.Create(rec)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		arrivalOfWrite := h.Arrival()

		// The create's own reading already describes the stored bytes,
		// and the record is not readable before the arrival is confirmed.
		if !arrivalOfWrite.verify(body) {
			t.Error("the arrival evidence does not describe the bytes the provider stored")
		}
		if arrivalOfWrite.verify([]byte{0x00}) {
			t.Error("the arrival evidence accepts bytes it did not store")
		}
		if _, err := rep.Get(rec.OpKey); !errors.Is(err, errNotFound) {
			t.Errorf("get of an unconfirmed write: err = %v, want not-found", err)
		}

		receipt, err := w.Confirm(h)
		if err != nil {
			t.Fatalf("confirm: %v", err)
		}
		if receipt.Arrival != arrivalOfWrite {
			t.Errorf("confirmation arrival = %v, want the create's arrival %v", receipt.Arrival, arrivalOfWrite)
		}
		if receipt.Key != rec.OpKey || receipt.Seq != 1 || receipt.Bytes != len(body) {
			t.Errorf("receipt = %+v, want the opaque key, sequence 1 and %d bytes", receipt, len(body))
		}

		// The verifier reads the same provider arrival for the dead-man
		// check, and reports no body.
		attrs, err := v.Attributes(rec.OpKey)
		if err != nil {
			t.Fatalf("attributes: %v", err)
		}
		if attrs.Arrival != arrivalOfWrite || attrs.Bytes != len(body) || !attrs.Confirmed {
			t.Errorf("verifier attributes = %+v, want the provider arrival and %d bytes confirmed", attrs, len(body))
		}

		// The provider clock advances on its own, arrival by
		// arrival, and no writer argument moves it.
		next := sampleRecord(t, 1, w.stream, 2)
		rc2, err := flush(t, w, next)
		if err != nil {
			t.Fatalf("second flush: %v", err)
		}
		if rc2.Arrival.clockNanos <= receipt.Arrival.clockNanos {
			t.Errorf("provider clock did not advance: %d then %d",
				receipt.Arrival.clockNanos, rc2.Arrival.clockNanos)
		}
		if rc2.Arrival.arrivalSeq != receipt.Arrival.arrivalSeq+1 {
			t.Errorf("provider arrival sequence = %d, want %d", rc2.Arrival.arrivalSeq, receipt.Arrival.arrivalSeq+1)
		}

		// A second confirmation of the same write is refused: publishing an
		// arrival is not a way to re-open a record, and it is not a get.
		if _, err := w.Confirm(h); !errors.Is(err, errAlreadyConfirmed) {
			t.Errorf("second confirm: err = %v, want the already-confirmed refusal", err)
		}
		// A handle from another provider names nothing here, so confirm
		// cannot be aimed at an arbitrary key.
		other2 := arm.provider(t)
		foreign, err := other2.newWriter(1, streamID(0x50)).Create(sampleRecord(t, 1, streamID(0x50), 1))
		if err != nil {
			t.Fatalf("foreign create: %v", err)
		}
		if _, err := w.Confirm(foreign); !errors.Is(err, errNotFound) {
			t.Errorf("confirm of a foreign handle: err = %v, want not-found", err)
		}
	})
}

// TestLedgerSerialSequencingConfirmsBeforeTheNextSequence is the accepted
// serial flusher rule: the flusher never uses n+1 before n is
// confirmed, and a gap fails the enumeration.
func TestLedgerSerialSequencingConfirmsBeforeTheNextSequence(t *testing.T) {
	t.Parallel()
	runOverArms(t, func(t *testing.T, arm providerArm, p *synthProvider) {
		t.Helper()
		w, rep := p.newWriter(1, streamID(0x50)), replayer{p: p}

		h1, err := w.Create(sampleRecord(t, 1, w.stream, 1))
		if err != nil {
			t.Fatalf("create sequence 1: %v", err)
		}
		// While sequence 1 is unconfirmed, the stream is closed for
		// business: neither a retry of 1 nor the next sequence is accepted.
		if _, err := w.Create(sampleRecord(t, 1, w.stream, 1)); !errors.Is(err, errSequenceGap) {
			t.Errorf("create sequence 1 twice: err = %v, want the unconfirmed-predecessor refusal", err)
		}
		if _, err := w.Create(sampleRecord(t, 1, w.stream, 2)); !errors.Is(err, errSequenceGap) {
			t.Errorf("create sequence 2 with 1 unconfirmed: err = %v, want the unconfirmed-predecessor refusal", err)
		}
		// An unconfirmed write is not in the ledger: it is not readable, and
		// the listing shows nothing.
		if _, err := rep.Get(h1.key); !errors.Is(err, errNotFound) {
			t.Errorf("get of an unconfirmed write: err = %v, want not-found", err)
		}
		// A handle is bound to the credential that Create issued it to: another
		// stream's credential cannot publish this stream's record, and neither
		// can the same stream id under another epoch. The refusal reads
		// nothing, so the record stays unconfirmed.
		if _, err := p.newWriter(1, streamID(0x60)).Confirm(h1); !errors.Is(err, errCredential) {
			t.Errorf("confirm of another stream's handle: err = %v, want the credential refusal", err)
		}
		if _, err := p.newWriter(2, w.stream).Confirm(h1); !errors.Is(err, errCredential) {
			t.Errorf("confirm of another epoch's handle: err = %v, want the credential refusal", err)
		}
		if _, err := rep.Get(h1.key); !errors.Is(err, errNotFound) {
			t.Errorf("another credential's confirm published the record: err = %v, want not-found", err)
		}
		page, err := rep.List("", conformancePageSize)
		if err != nil {
			t.Fatalf("list with an unconfirmed write: %v", err)
		}
		if page.Count != 0 || len(page.Entries) != 0 {
			t.Errorf("listing with an unconfirmed write shows %d of %d entries", len(page.Entries), page.Count)
		}

		if _, err := w.Confirm(h1); err != nil {
			t.Fatalf("confirm sequence 1: %v", err)
		}
		// Now the next sequence is writable, and a gap is not.
		if _, err := flush(t, w, sampleRecord(t, 1, w.stream, 2)); err != nil {
			t.Errorf("create sequence 2 after confirming 1: %v", err)
		}
		if _, err := w.Create(sampleRecord(t, 1, w.stream, 4)); !errors.Is(err, errSequenceGap) {
			t.Errorf("create sequence 4 with 3 missing: err = %v, want the gap refusal", err)
		}
		if _, err := flush(t, w, sampleRecord(t, 1, w.stream, 3)); err != nil {
			t.Errorf("create sequence 3: %v", err)
		}
		// A retry of a confirmed write is the create-once refusal, which a
		// flusher reads as already durable. It is not an overwrite.
		if _, err := w.Create(sampleRecord(t, 1, w.stream, 3)); !errors.Is(err, errSequenceGap) &&
			!errors.Is(err, errObjectExists) {
			t.Errorf("retry of a confirmed sequence: err = %v, want a refusal that is not an overwrite", err)
		}
	})
}

// TestLedgerStreamsKeepTheirOwnSequences is the accepted per-(epoch, replica
// stream) contiguity rule, and the MAIN-1452 requirement that
// separate streams do not share a fabricated global sequence. The provider's
// own arrival counter is separate from what a writer claims.
func TestLedgerStreamsKeepTheirOwnSequences(t *testing.T) {
	t.Parallel()
	runOverArms(t, func(t *testing.T, arm providerArm, p *synthProvider) {
		t.Helper()
		streamA, streamB := streamID(0x50), streamID(0x60)
		wA, wB := p.newWriter(1, streamA), p.newWriter(1, streamB)

		for seq := int64(1); seq <= 3; seq++ {
			if _, err := flush(t, wA, sampleRecord(t, 1, streamA, seq)); err != nil {
				t.Fatalf("stream A sequence %d: %v", seq, err)
			}
		}
		// Stream B starts at 1. It is not asked to continue A's numbering.
		b1, err := flush(t, wB, sampleRecord(t, 1, streamB, 1))
		if err != nil {
			t.Fatalf("stream B sequence 1: %v", err)
		}
		if b1.Seq != 1 {
			t.Errorf("stream B accepted sequence %d, want 1", b1.Seq)
		}
		// The provider's arrival reading is not the writer's claim: this is
		// the stream's first record and the provider's fourth arrival.
		if b1.Arrival.arrivalSeq <= b1.Seq {
			t.Errorf("arrival %d does not exceed the claimed sequence %d: the seam has collapsed a claim into an arrival",
				b1.Arrival.arrivalSeq, b1.Seq)
		}
		if _, err := flush(t, wB, sampleRecord(t, 1, streamB, 2)); err != nil {
			t.Fatalf("stream B sequence 2: %v", err)
		}
		// B's own contiguity is what is enforced, not a global counter.
		if _, err := wB.Create(sampleRecord(t, 1, streamB, 4)); !errors.Is(err, errSequenceGap) {
			t.Errorf("stream B sequence 4 with 3 missing: err = %v, want the gap refusal", err)
		}
		if _, err := flush(t, wA, sampleRecord(t, 1, streamA, 4)); err != nil {
			t.Errorf("stream A sequence 4: %v", err)
		}

		v := verifier{p: p}
		// Each stream's arrival record is its own: there is no shared
		// sequence to disagree about.
		aRec, err := v.Arrival(1, streamA)
		if err != nil {
			t.Fatalf("arrival for stream A: %v", err)
		}
		bRec, err := v.Arrival(1, streamB)
		if err != nil {
			t.Fatalf("arrival for stream B: %v", err)
		}
		if aRec.HighWater != 4 || aRec.Arrivals != 4 {
			t.Errorf("stream A arrival record = %+v, want high water 4 and 4 arrivals", aRec)
		}
		if bRec.HighWater != 2 || bRec.Arrivals != 2 {
			t.Errorf("stream B arrival record = %+v, want high water 2 and 2 arrivals", bRec)
		}
		// The arrival record is per stream: an epoch and a stream, a count
		// and a high water, and no global position a writer could address.
		typ := reflect.TypeFor[arrivalRecord]()
		var exported []string
		for f := range typ.Fields() {
			if f.PkgPath == "" {
				exported = append(exported, f.Name)
			}
		}
		if want := []string{"Epoch", "Stream", "Arrivals", "HighWater"}; !slices.Equal(exported, want) {
			t.Errorf("arrivalRecord fields = %v, want %v", exported, want)
		}
	})
}

// TestLedgerWriteCredentialIsBoundToItsStream is the accepted
// per-epoch writer credential with a stream prefix: a credential writes on its
// own (epoch, stream) and refuses to speak for another.
func TestLedgerWriteCredentialIsBoundToItsStream(t *testing.T) {
	t.Parallel()
	runOverArms(t, func(t *testing.T, arm providerArm, p *synthProvider) {
		t.Helper()
		w := p.newWriter(1, streamID(0x50))
		// A record naming another stream is refused before anything is
		// stored, so a credential cannot spend another stream's sequence.
		foreign := sampleRecord(t, 1, streamID(0x60), 1)
		if _, err := w.Create(foreign); !errors.Is(err, errCredential) {
			t.Errorf("create on another stream: err = %v, want the credential refusal", err)
		}
		otherEpoch := sampleRecord(t, 2, w.stream, 1)
		if _, err := w.Create(otherEpoch); !errors.Is(err, errCredential) {
			t.Errorf("create in another epoch: err = %v, want the credential refusal", err)
		}
		// Nothing was stored by the refusals.
		page, err := replayer{p: p}.List("", conformancePageSize)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if page.Count != 0 {
			t.Errorf("a refused write left %d entries behind", page.Count)
		}
		if _, err := flush(t, w, sampleRecord(t, 1, w.stream, 1)); err != nil {
			t.Errorf("the credential's own write: %v", err)
		}
	})
}

// TestLedgerSevenPageWalkCompletes is the accepted complete-enumeration
// rule, the paginated half: continuation
// tokens until a completion marker, and a checked count per page.
func TestLedgerSevenPageWalkCompletes(t *testing.T) {
	t.Parallel()
	runOverArms(t, func(t *testing.T, arm providerArm, p *synthProvider) {
		t.Helper()
		keys := seedConformanceSet(t, p)
		rep, v := replayer{p: p}, verifier{p: p}

		out, err := walk(rep, conformancePageSize, erasureledger.OperationKey{})
		if err != nil {
			t.Fatalf("walk: %v", err)
		}
		if out.Pages != conformanceWalkPages {
			t.Errorf("walk took %d pages, want %d", out.Pages, conformanceWalkPages)
		}
		if len(out.Keys) != conformanceObjects {
			t.Errorf("walk found %d keys, want %d", len(out.Keys), conformanceObjects)
		}
		if !slices.Equal(sortedKeys(out.Keys), sortedKeys(keys)) {
			t.Errorf("the walk's key set is not the written set")
		}
		if len(slices.Compact(sortedKeys(out.Keys))) != conformanceObjects {
			t.Error("the walk showed a key more than once")
		}

		// The page shape: five entries on each of six pages, four on the
		// last, a continuation token on every page but the last, and the
		// completion marker exactly once, on the last page.
		var counts []int
		var tokens, markers int
		after := ""
		for {
			page, err := rep.List(after, conformancePageSize)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			counts = append(counts, page.Count)
			if page.Count != len(page.Entries) {
				t.Errorf("page declared %d and carried %d", page.Count, len(page.Entries))
			}
			if page.Continuation != "" {
				tokens++
			}
			if page.Complete {
				markers++
				if page.Continuation == "" {
					t.Error("the completing page carries no continuation token")
				}
				break
			}
			after = page.Continuation
		}
		if want := []int{5, 5, 5, 5, 5, 5, 4}; !slices.Equal(counts, want) {
			t.Errorf("per-page counts = %v, want %v", counts, want)
		}
		if markers != 1 {
			t.Errorf("the walk saw %d completion markers, want exactly 1", markers)
		}
		if tokens != conformanceWalkPages {
			t.Errorf("the walk saw %d continuation tokens, want one per page", tokens)
		}

		// The gate: per-stream contiguity and agreement with the verifier's
		// independent arrival records.
		report := checkEnumeration(gateInput{
			Walk:     out,
			Arrivals: arrivalRecords(t, v, out),
		})
		if !report.Complete {
			t.Errorf("a complete enumeration reported: %v", report.Reasons)
		}
		if len(report.HighWaters) != 2 {
			t.Fatalf("the walk found %d streams, want 2", len(report.HighWaters))
		}
		waters := make([]int64, 0, len(report.HighWaters))
		for _, high := range report.HighWaters {
			waters = append(waters, high)
		}
		slices.Sort(waters)
		// 34 records over two streams, 17 each: no stream carries a
		// fabricated global position.
		if want := []int64{17, 17}; !slices.Equal(waters, want) {
			t.Errorf("per-stream high waters = %v, want %v", waters, want)
		}
	})
}

// TestLedgerEnumerationFailsOnPageFaults is the accepted paginated-completeness
// rule under fault injection: a missing page, a truncated page, and an
// injected failure on page 3 each fail the enumeration, and a walk that resumes
// after a failure reuses the same operation keys.
func TestLedgerEnumerationFailsOnPageFaults(t *testing.T) {
	t.Parallel()
	for _, arm := range providerArms {
		t.Run(arm.name, func(t *testing.T) {
			t.Parallel()

			t.Run("injected failure on page 3", func(t *testing.T) {
				p := arm.provider(t)
				keys := seedConformanceSet(t, p)
				p.faults = pageFaults{failPage: 3}
				rep := replayer{p: p}

				// The two pages before the fault read normally, and the fault stops
				// the walk: no completion marker, so there is no enumeration.
				one, err := rep.List("", conformancePageSize)
				if err != nil {
					t.Fatalf("page 1: %v", err)
				}
				two, err := rep.List(one.Continuation, conformancePageSize)
				if err != nil {
					t.Fatalf("page 2: %v", err)
				}
				if two.Complete {
					t.Fatal("the enumeration completed in two pages")
				}
				before := append(pageKeys(one), pageKeys(two)...)
				if _, err := rep.List(two.Continuation, conformancePageSize); !errors.Is(err, errInjectedPage) {
					t.Fatalf("page 3: err = %v, want the injected failure", err)
				}

				// With the fault cleared, the walk resumes from the last token it
				// held, and it reuses the operation keys: the opaque keys of the
				// resumed pages plus the keys of the two pages already walked are
				// exactly the written set, and page 1 lists the same keys again.
				p.faults = pageFaults{}
				resumed, err := walkFrom(rep, two.Continuation, conformancePageSize, erasureledger.OperationKey{})
				if err != nil {
					t.Fatalf("resumed walk: %v", err)
				}
				if len(resumed.Keys) != conformanceObjects-len(before) {
					t.Errorf("the resumed walk found %d keys, want %d",
						len(resumed.Keys), conformanceObjects-len(before))
				}
				all := append(slices.Clone(before), resumed.Keys...)
				if !slices.Equal(sortedKeys(all), sortedKeys(keys)) {
					t.Error("a resumed walk did not reuse the written operation keys")
				}
				again, err := rep.List("", conformancePageSize)
				if err != nil {
					t.Fatalf("re-list page 1: %v", err)
				}
				if !slices.Equal(pageKeys(again), pageKeys(one)) {
					t.Error("a re-listed page shows different operation keys")
				}
			})

			t.Run("truncated page 3", func(t *testing.T) {
				p := arm.provider(t)
				seedConformanceSet(t, p)
				p.faults = pageFaults{truncatePage: 3}
				out, err := walk(replayer{p: p}, conformancePageSize, erasureledger.OperationKey{})
				if err == nil || !strings.Contains(err.Error(), "per-page count check") {
					t.Fatalf("walk over a truncated page: err = %v, want the per-page count check to fail", err)
				}
				if out.Pages != 3 {
					t.Errorf("the walk noticed at page %d, want page 3", out.Pages)
				}
			})

			t.Run("missing page 3", func(t *testing.T) {
				p := arm.provider(t)
				keys := seedConformanceSet(t, p)
				p.faults = pageFaults{dropPage: 3}
				rep, v := replayer{p: p}, verifier{p: p}

				// The page machinery sees a normal walk: a page that never
				// arrives leaves no hole in a continuation chain. The completeness
				// checks are what fail, which is why contiguity per stream and the
				// verifier's arrival records are required and a completion marker
				// alone is not proof.
				out, err := walk(rep, conformancePageSize, erasureledger.OperationKey{})
				if err != nil {
					t.Fatalf("walk over a missing page: %v", err)
				}
				if out.Pages != conformanceWalkPages-1 {
					t.Errorf("the walk took %d pages, want %d", out.Pages, conformanceWalkPages-1)
				}
				if len(out.Keys) >= len(keys) {
					t.Fatalf("the walk found %d keys of %d, want a shortfall", len(out.Keys), len(keys))
				}
				report := checkEnumeration(gateInput{Walk: out, Arrivals: arrivalRecords(t, v, out)})
				if report.Complete {
					t.Fatal("the gate passed an enumeration with a page missing")
				}
				if !slices.ContainsFunc(report.Reasons, func(r string) bool {
					return strings.Contains(r, "arrival count disagreement")
				}) {
					t.Errorf("the gate reported %v, want an arrival count disagreement", report.Reasons)
				}
				if !slices.ContainsFunc(report.Reasons, func(r string) bool {
					return strings.Contains(r, "missing sequence") || strings.Contains(r, "high-water disagreement")
				}) {
					t.Errorf("the gate reported %v, want a sequence gap or a high-water disagreement", report.Reasons)
				}
			})
		})
	}
}

// TestLedgerListingCarriesOnlyOpaqueKeys is the MAIN-1436 privacy
// control applied to the provider surface: a listing
// includes opaque keys, counts and the provider's arrival timing, and never an
// identifier from a record body. The keys are also not derived from body
// content, so a listing row cannot be walked back to a record.
func TestLedgerListingCarriesOnlyOpaqueKeys(t *testing.T) {
	t.Parallel()
	runOverArms(t, func(t *testing.T, arm providerArm, p *synthProvider) {
		t.Helper()
		keys := seedConformanceSet(t, p)
		rep, v := replayer{p: p}, verifier{p: p}

		// Structurally, a listing row is an opaque key plus the provider's
		// arrival reading, and exposes one member: the key.
		typ := reflect.TypeFor[listEntry]()
		if typ.NumField() != 2 {
			t.Errorf("listEntry has %d fields, want an opaque key and the arrival reading", typ.NumField())
		}
		var exported []string
		for f := range typ.Fields() {
			if f.PkgPath == "" {
				exported = append(exported, f.Name)
			}
			if f.Type.Kind() == reflect.Slice || f.Type.Kind() == reflect.String ||
				f.Type.Kind() == reflect.Struct || f.Type.Kind() == reflect.Pointer {
				t.Errorf("listEntry.%s has kind %v: a listing row carries an opaque key and a count", f.Name, f.Type.Kind())
			}
		}
		if want := []string{"Key"}; !slices.Equal(exported, want) {
			t.Errorf("listEntry exposes %v, want %v", exported, want)
		}
		if keyField, ok := typ.FieldByName("Key"); !ok || keyField.Type != reflect.TypeFor[erasureledger.OperationKey]() {
			t.Errorf("listEntry.Key is %v, want the opaque operation key type", keyField.Type)
		}
		// A page carries the count, the continuation token, and the
		// completion marker, and no body field.
		if n := reflect.TypeFor[listPage]().NumField(); n != 4 {
			t.Errorf("listPage has %d fields, want entries, count, continuation, completion", n)
		}

		drawn := map[erasureledger.OperationKey]bool{}
		for _, k := range keys {
			drawn[k] = true
		}
		after := ""
		for {
			page, err := rep.List(after, conformancePageSize)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			dump := fmt.Sprintf("%+v", page)
			if strings.Contains(dump, "users_id_seq") {
				t.Errorf("a listing page exposes a body identifier: %s", dump)
			}
			if strings.Contains(dump, "body") || strings.Contains(dump, "Body") {
				t.Errorf("a listing page carries a body: %s", dump)
			}
			for _, e := range page.Entries {
				if !drawn[e.Key] {
					t.Errorf("a listing row shows %s, which is not a key the writer drew", keyName(e.Key))
				}
				// The timing on the row is the provider's arrival reading,
				// the same one the verifier reports, not a value a
				// writer supplied.
				attrs, err := v.Attributes(e.Key)
				if err != nil {
					t.Fatalf("attributes for %s: %v", keyName(e.Key), err)
				}
				if attrs.Arrival.clockNanos != e.arrivalNanos {
					t.Errorf("listing timing for %s = %d, the provider reports %d",
						keyName(e.Key), e.arrivalNanos, attrs.Arrival.clockNanos)
				}
			}
			if page.Complete {
				break
			}
			after = page.Continuation
		}

		// The keys are not derived from content: two records with the same body
		// on one stream carry different opaque keys, and the listing shows both.
		fresh := arm.provider(t)
		w := fresh.newWriter(2, streamID(0x70))
		body := fileBody(t, 0xfeed)
		one := envelope(t, 2, w.stream, 1, drawnKey(t), body)
		two := envelope(t, 2, w.stream, 2, drawnKey(t), body)
		if _, err := flush(t, w, one); err != nil {
			t.Fatalf("first copy: %v", err)
		}
		if _, err := flush(t, w, two); err != nil {
			t.Fatalf("second copy: %v", err)
		}
		if one.OpKey == two.OpKey {
			t.Error("two records with the same body share one opaque key")
		}
		page, err := replayer{p: fresh}.List("", conformancePageSize)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		seen := map[erasureledger.OperationKey]bool{}
		for _, e := range page.Entries {
			seen[e.Key] = true
		}
		if !seen[one.OpKey] || !seen[two.OpKey] {
			t.Error("identical bodies collapsed to one listing row")
		}
	})
}

// TestLedgerArrivalHighWaterDisagreementIsDetected is the accepted
// complete-enumeration requirement that the highest
// sequence per stream match the verifier's independent arrival records. The
// disagreement is injected in the seam's arrival log, which is the shape a
// real verifier disagreement takes; nothing here asserts that a real verifier
// exists.
func TestLedgerArrivalHighWaterDisagreementIsDetected(t *testing.T) {
	t.Parallel()
	runOverArms(t, func(t *testing.T, arm providerArm, p *synthProvider) {
		t.Helper()
		stream := streamID(0x50)
		w := p.newWriter(1, stream)
		seed(t, w, 5)
		rep, v := replayer{p: p}, verifier{p: p}

		out, err := walk(rep, conformancePageSize, erasureledger.OperationKey{})
		if err != nil {
			t.Fatalf("walk: %v", err)
		}
		if report := checkEnumeration(gateInput{Walk: out, Arrivals: arrivalRecords(t, v, out)}); !report.Complete {
			t.Fatalf("the healthy case reported: %v", report.Reasons)
		}

		// A verifier record that puts the stream's high water above what the
		// walk found fails the gate.
		p.verifs = append(p.verifs, verifierEntry{epoch: 1, stream: stream, count: 9, high: 9})
		bad := checkEnumeration(gateInput{Walk: out, Arrivals: arrivalRecords(t, v, out)})
		if bad.Complete {
			t.Fatal("the gate passed a 9-versus-5 high-water disagreement")
		}
		if !slices.ContainsFunc(bad.Reasons, func(r string) bool {
			return strings.Contains(r, "arrival high-water disagreement") && strings.Contains(r, "verifier 9, walk 5")
		}) {
			t.Errorf("the gate reported %v, want the high-water disagreement", bad.Reasons)
		}

		// A verifier record that agrees again passes, so the failure is the
		// disagreement and not the gate.
		p.verifs = append(p.verifs, verifierEntry{epoch: 1, stream: stream, count: 5, high: 5})
		if report := checkEnumeration(gateInput{Walk: out, Arrivals: arrivalRecords(t, v, out)}); !report.Complete {
			t.Errorf("the gate reported after the records agreed: %v", report.Reasons)
		}

		// An arrival record that cannot be verified fails closed, so a gate
		// cannot pass on an unreadable verifier.
		p.verifs = append(p.verifs, verifierEntry{epoch: 1, stream: stream, tamper: true})
		if _, err := v.Arrival(1, stream); !errors.Is(err, errInjectedVerifier) {
			t.Errorf("tampered arrival record: err = %v, want the tamper marker", err)
		}

		// A record naming a stream the walk never saw fails the gate
		// too: the two accounts of the ledger have to agree on every
		// stream, not only the convenient ones.
		ghost := arrivalRecord{Epoch: 1, Stream: streamID(0x60), Arrivals: 3, HighWater: 3}
		report := checkEnumeration(gateInput{Walk: out, Arrivals: []arrivalRecord{ghost}})
		if report.Complete {
			t.Error("the gate passed a verifier record for a stream the walk never saw")
		}
	})
}

// TestLedgerFencedOldEpochWriteIsQuarantined is the accepted epoch
// fencing rule: a record written to a fenced stream after
// its fence is quarantined, and it cannot enter replayable evidence. The fence
// is a bookkeeping marker in the seam, not a credential revocation, and this
// case asserts no revocation capability.
func TestLedgerFencedOldEpochWriteIsQuarantined(t *testing.T) {
	t.Parallel()
	runOverArms(t, func(t *testing.T, arm providerArm, p *synthProvider) {
		t.Helper()
		stream := streamID(0x50)
		w := p.newWriter(1, stream)
		seed(t, w, 3)
		p.fenceStream(1, stream)

		late := sampleRecord(t, 1, stream, 4)
		_, err := w.Create(late)
		if !errors.Is(err, errQuarantined) {
			t.Fatalf("write on a fenced stream: err = %v, want the quarantine result", err)
		}

		// It is not in the ledger: not readable, not listed, and not counted
		// by the verifier.
		rep, v := replayer{p: p}, verifier{p: p}
		if _, err := rep.Get(late.OpKey); !errors.Is(err, errNotFound) {
			t.Errorf("get of the quarantined write: err = %v, want not-found", err)
		}
		out, err := walk(rep, conformancePageSize, erasureledger.OperationKey{})
		if err != nil {
			t.Fatalf("walk after the fence: %v", err)
		}
		if len(out.Keys) != 3 {
			t.Errorf("the enumeration shows %d records, want the 3 pre-fence records", len(out.Keys))
		}
		if slices.Contains(out.Keys, late.OpKey) {
			t.Error("the quarantined write entered the enumeration")
		}
		if report := checkEnumeration(gateInput{Walk: out, Arrivals: arrivalRecords(t, v, out)}); !report.Complete {
			t.Errorf("the enumeration of the pre-fence set reported: %v", report.Reasons)
		}
		arr, err := v.Arrival(1, stream)
		if err != nil {
			t.Fatalf("arrival: %v", err)
		}
		if arr.Arrivals != 3 || arr.HighWater != 3 {
			t.Errorf("arrival record = %+v, want 3 arrivals at high water 3", arr)
		}

		// The provider kept the bytes as evidence, and the only seat that can
		// see them is the verifier's attribute read.
		attrs, err := v.Attributes(late.OpKey)
		if err != nil {
			t.Fatalf("attributes of the quarantined write: %v", err)
		}
		if !attrs.Quarantined {
			t.Errorf("the verifier reports the quarantined write as %+v", attrs)
		}
		if attrs.Epoch != 1 || attrs.Stream != stream || attrs.Seq != 4 {
			t.Errorf("quarantine attributes = epoch %d stream %s seq %d, want 1, the fenced stream, 4",
				attrs.Epoch, keyName(attrs.Stream), attrs.Seq)
		}
		if len(p.s.quarantinedObjects()) != 1 {
			t.Errorf("the quarantine holds %d entries, want 1", len(p.s.quarantinedObjects()))
		}

		// A fence on a stream that has never written is a fence. The sequence
		// it parks at is zero, so a zero-valued position must not read as
		// unfenced: that stream's first old-epoch write is quarantined too,
		// and it leaves no arrival record and nothing listable behind.
		empty := streamID(0x60)
		p.fenceStream(1, empty)
		first := sampleRecord(t, 1, empty, 1)
		if _, err := p.newWriter(1, empty).Create(first); !errors.Is(err, errQuarantined) {
			t.Errorf("first write on a fenced empty stream: err = %v, want the quarantine result", err)
		}
		if _, err := rep.Get(first.OpKey); !errors.Is(err, errNotFound) {
			t.Errorf("the fenced first write is readable: err = %v, want not-found", err)
		}
		if _, err := v.Arrival(1, empty); !errors.Is(err, errNotFound) {
			t.Errorf("the fenced empty stream reports arrivals: %v", err)
		}
		after, err := walk(rep, conformancePageSize, erasureledger.OperationKey{})
		if err != nil {
			t.Fatalf("walk after the empty-stream fence: %v", err)
		}
		if len(after.Keys) != 3 || slices.Contains(after.Keys, first.OpKey) {
			t.Errorf("the enumeration after the empty-stream fence holds %d keys: %v", len(after.Keys), after.Keys)
		}
		if n := len(p.s.quarantinedObjects()); n != 2 {
			t.Errorf("the quarantine holds %d entries, want 2", n)
		}

		// A fence does not reach across epochs: a new epoch's stream on the
		// same stream id writes normally, which is what the model fences.
		if _, err := flush(t, p.newWriter(2, stream), sampleRecord(t, 2, stream, 1)); err != nil {
			t.Errorf("a new epoch's write: %v", err)
		}
	})
}

// TestLedgerSentinelIsSeparatelyAuthorized is the accepted
// logical sentinel rule: the sentinel is a separately
// authorized marker, the replayer cannot mint one, and a marker that is not
// the newest arrival does not vouch for a complete enumeration.
func TestLedgerSentinelIsSeparatelyAuthorized(t *testing.T) {
	t.Parallel()
	runOverArms(t, func(t *testing.T, arm providerArm, p *synthProvider) {
		t.Helper()
		stream := streamID(0x50)
		w := p.newWriter(1, stream)
		seed(t, w, 4)
		rep, v := replayer{p: p}, verifier{p: p}

		// The four ledger credentials carry no sentinel capability.
		for _, principal := range []any{w, rep, v, expirer{p: p}} {
			if _, ok := principal.(ledgerSentinelCreator); ok {
				t.Errorf("%T can mint a sentinel", principal)
			}
		}

		key, err := sentinelAuthorizer{p: p}.CreateSentinel()
		if err != nil {
			t.Fatalf("create sentinel: %v", err)
		}
		marker, err := rep.Get(key)
		if err != nil {
			t.Fatalf("the sentinel is not visible to the replayer: %v", err)
		}
		if !marker.logical || len(marker.Body) != 0 {
			t.Errorf("the sentinel is %+v, want a marker with no record body", marker)
		}

		// A walk that sees the sentinel as the newest arrival is complete.
		out, err := walk(rep, conformancePageSize, key)
		if err != nil {
			t.Fatalf("walk: %v", err)
		}
		if report := checkEnumeration(gateInput{
			Walk: out, Arrivals: arrivalRecords(t, v, out), Sentinel: key,
		}); !report.Complete {
			t.Errorf("the gate reported with the sentinel in place: %v", report.Reasons)
		}

		// The sentinel is not a ledger object, so it is not in a listing row.
		if slices.Contains(out.Keys, key) {
			t.Error("the sentinel appears in the ledger listing")
		}

		// A marker written before later records does not vouch for the
		// enumeration: it is not the newest arrival.
		if _, err := flush(t, w, sampleRecord(t, 1, stream, 5)); err != nil {
			t.Fatalf("write after the sentinel: %v", err)
		}
		stale, err := walk(rep, conformancePageSize, key)
		if err != nil {
			t.Fatalf("walk after the sentinel: %v", err)
		}
		report := checkEnumeration(gateInput{
			Walk: stale, Arrivals: arrivalRecords(t, v, stale), Sentinel: key,
		})
		if report.Complete {
			t.Error("the gate passed a sentinel that is not the newest arrival")
		}
	})
}

// TestLedgerExpiryCheckpointsBeforeDelete is the accepted expiry rule
// at the provider seam: the expirer is the only principal
// with a delete right, it must write a pruned-through checkpoint first, and it
// leaves the records that keep contiguity checkable in place.
func TestLedgerExpiryCheckpointsBeforeDelete(t *testing.T) {
	t.Parallel()
	runOverArms(t, func(t *testing.T, arm providerArm, p *synthProvider) {
		t.Helper()
		stream := streamID(0x70)
		w := p.newWriter(1, stream)
		keys := seed(t, w, 6)
		rep, v, ex := replayer{p: p}, verifier{p: p}, expirer{p: p}

		// No delete before a checkpoint, and no checkpoint past the live high
		// water.
		if err := ex.Delete(keys[0]); !errors.Is(err, errNotPruned) {
			t.Errorf("delete before any checkpoint: err = %v, want the checkpoint refusal", err)
		}
		if err := ex.PruneThrough(1, stream, 9); err == nil {
			t.Error("the expirer pruned past the confirmed high water")
		}
		if err := ex.PruneThrough(1, stream, 2); err != nil {
			t.Fatalf("prune through 2: %v", err)
		}
		if err := ex.Delete(keys[0]); err != nil {
			t.Fatalf("delete the pruned record: %v", err)
		}
		if _, err := rep.Get(keys[0]); !errors.Is(err, errNotFound) {
			t.Errorf("get of the deleted record: err = %v, want not-found", err)
		}

		// The records that keep contiguity checkable stay: the newest
		// reservation for its allocator, and a channel-id exclusion inside
		// retention. keys[4] is the sequence 5 channel-id exclusion and
		// keys[5] is the sequence 6 reservation.
		if err := ex.PruneThrough(1, stream, 6); err != nil {
			t.Fatalf("prune through 6: %v", err)
		}
		for _, idx := range []int{4, 5} {
			if err := ex.Delete(keys[idx]); !errors.Is(err, errNotDeletable) {
				t.Errorf("delete of a kept record at sequence %d: err = %v, want the keep refusal", idx+1, err)
			}
		}
		// A plain record whose stream is pruned through it is deletable.
		if err := ex.Delete(keys[1]); err != nil {
			t.Errorf("delete of a pruned record at sequence 2: %v", err)
		}

		// The walk after expiry is complete once the checkpoint is part of the
		// evidence, and the verifier's count follows the deletes while
		// its high water does not move.
		out, err := walk(rep, conformancePageSize, erasureledger.OperationKey{})
		if err != nil {
			t.Fatalf("walk after expiry: %v", err)
		}
		arr, err := v.Arrival(1, stream)
		if err != nil {
			t.Fatalf("arrival: %v", err)
		}
		if arr.Arrivals != 4 || arr.HighWater != 6 {
			t.Errorf("arrival record after expiry = %+v, want 4 arrivals at high water 6", arr)
		}
		marks := []pruneMark{{key: seqKey{epoch: 1, stream: stream}, through: 6}}
		if report := checkEnumeration(gateInput{
			Walk: out, Arrivals: []arrivalRecord{arr}, Pruned: marks,
		}); !report.Complete {
			t.Errorf("the gate reported on a pruned stream: %v", report.Reasons)
		}
		// Without the checkpoint the same walk reports the holes, which is
		// why the checkpoint is required before a delete.
		report := checkEnumeration(gateInput{Walk: out, Arrivals: []arrivalRecord{arr}})
		if report.Complete {
			t.Error("the gate passed a pruned stream with no checkpoint in evidence")
		}
	})
}

// TestLedgerSuiteClaimsNoRealProviderCapability is the MAIN-1452
// boundary: these are simulated capability results. The
// seam's capability vocabulary names no provider consistency, Object Lock,
// versioning, retention, restore-readiness, or credential property, the arrival
// marker is a 32-bit simulated checksum rather than a content digest, and no
// evidence type carries a wall clock.
func TestLedgerSuiteClaimsNoRealProviderCapability(t *testing.T) {
	t.Parallel()

	// The capability vocabulary names no provider property.
	for _, typ := range capabilityTypes {
		for m := range typ.Methods() {
			name := strings.ToLower(m.Name)
			for _, word := range forbiddenProviderWords {
				if strings.Contains(name, word) {
					t.Errorf("%s.%s names provider property %q", typ.Name(), m.Name, word)
				}
			}
		}
	}

	// The arrival evidence is a simulated reading: three unexported integers,
	// with a 32-bit checksum that is far too small to be a content digest, and
	// no time value that a test could read as a provider date.
	arrival := reflect.TypeFor[arrivalEvidence]()
	if n := arrival.NumField(); n != 3 {
		t.Fatalf("arrivalEvidence has %d fields, want three simulated readings", n)
	}
	checksum, ok := arrival.FieldByName("checksum")
	if !ok || checksum.Type != reflect.TypeFor[uint32]() {
		t.Errorf("the arrival checksum is %v, want a 32-bit simulated marker", checksum.Type)
	}
	for f := range arrival.Fields() {
		if f.Type == reflect.TypeFor[string]() {
			t.Errorf("arrivalEvidence.%s carries text: the arrival is a simulated reading", f.Name)
		}
	}
	for _, typ := range []reflect.Type{
		reflect.TypeFor[createReceipt](),
		reflect.TypeFor[objectAttributes](),
		reflect.TypeFor[listEntry](),
		reflect.TypeFor[walkOutcome](),
	} {
		for f := range typ.Fields() {
			if f.Type == reflect.TypeFor[time.Time]() {
				t.Errorf("%s.%s is a wall clock: this suite has no provider clock", typ.Name(), f.Name)
			}
		}
	}

	// No seam type is a provider client: the storage arm is a Go interface with
	// two implementations in this package, and the provider type exposes no
	// exported method at all.
	if n := reflect.TypeFor[*synthProvider]().NumMethod(); n != 0 {
		t.Errorf("synthProvider exposes %d methods; the seam is test-only", n)
	}
}

// pageKeys lists the opaque keys one page showed.
func pageKeys(page listPage) []erasureledger.OperationKey {
	out := make([]erasureledger.OperationKey, 0, len(page.Entries))
	for _, e := range page.Entries {
		out = append(out, e.Key)
	}
	return out
}

// sortedKeys returns a sorted copy, for set comparison.
func sortedKeys(keys []erasureledger.OperationKey) []erasureledger.OperationKey {
	out := slices.Clone(keys)
	slices.SortFunc(out, func(a, b erasureledger.OperationKey) int { return bytes.Compare(a[:], b[:]) })
	return out
}
