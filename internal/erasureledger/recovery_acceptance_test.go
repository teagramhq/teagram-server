package erasureledger_test

// These tests are an inert acceptance model for MAIN-1497. They exercise the
// frozen record vocabulary and synthetic recovery arithmetic only; no runtime
// allocator, replay caller, or admission path consumes this model. One case
// uses the existing test-only provider seam to distinguish pending from
// confirmed records.

import (
	"errors"
	"maps"
	"math"
	"reflect"
	"slices"
	"testing"

	"github.com/teagramhq/teagram-server/internal/erasureledger"
)

var errSyntheticRecoveryNotReady = errors.New("synthetic recovery: not ready")
var errSyntheticOperationUnderLock = errors.New("synthetic recovery: provider operation under lock")
var errSyntheticProviderStalled = errors.New("synthetic recovery: provider stalled")
var errSyntheticRandomIDExcluded = errors.New("synthetic recovery: random id excluded")
var errSyntheticRandomIDCollision = errors.New("synthetic recovery: random id already live")

type syntheticRandomObjectKey struct {
	class erasureledger.RandomClass
	id    int64
}

type syntheticRandomObjects map[syntheticRandomObjectKey]struct{}

type syntheticRecoveryProof struct {
	paginationComplete bool
	streamsComplete    bool
	checkpointsCovered bool
}

func completeSyntheticProof() syntheticRecoveryProof {
	return syntheticRecoveryProof{
		paginationComplete: true,
		streamsComplete:    true,
		checkpointsCovered: true,
	}
}

func (p syntheticRecoveryProof) complete() bool {
	return p.paginationComplete && p.streamsComplete && p.checkpointsCovered
}

type syntheticEvidence struct {
	frame     []byte
	confirmed bool
}

func syntheticEvidenceFor(t *testing.T, record erasureledger.Record, confirmed bool) syntheticEvidence {
	t.Helper()
	frame, err := erasureledger.Encode(record)
	if err != nil {
		t.Fatalf("Encode synthetic evidence: %v", err)
	}
	return syntheticEvidence{frame: frame, confirmed: confirmed}
}

func syntheticRecord(t *testing.T, kind erasureledger.Kind, epoch int64, stream erasureledger.StreamID,
	seq int64, keySeed byte, payload erasureledger.Payload,
) erasureledger.Record {
	t.Helper()
	record, err := erasureledger.NewRecord(kind, epoch, stream, seq, opKey(keySeed), payload)
	if err != nil {
		t.Fatalf("NewRecord kind=%v epoch=%d seq=%d: %v", kind, epoch, seq, err)
	}
	return record
}

func syntheticBinding(t *testing.T, epoch int64, stream erasureledger.StreamID, lineage erasureledger.LineageID,
	keySeed byte,
) syntheticEvidence {
	t.Helper()
	body, err := erasureledger.NewStreamBinding(lineage)
	if err != nil {
		t.Fatalf("NewStreamBinding: %v", err)
	}
	record := syntheticRecord(t, erasureledger.KindStreamBinding, epoch, stream, 1, keySeed, body)
	return syntheticEvidenceFor(t, record, true)
}

func syntheticReservation(t *testing.T, epoch int64, stream erasureledger.StreamID, seq int64,
	keySeed byte, allocator string, ceiling int64,
) syntheticEvidence {
	t.Helper()
	name := mustAllocator(t, allocator)
	body, err := erasureledger.NewReservation(name, ceiling)
	if err != nil {
		t.Fatalf("NewReservation(%s, %d): %v", allocator, ceiling, err)
	}
	record := syntheticRecord(t, erasureledger.KindReservation, epoch, stream, seq, keySeed, body)
	return syntheticEvidenceFor(t, record, true)
}

func syntheticComponentReservation(t *testing.T, epoch int64, stream erasureledger.StreamID,
	seq int64, keySeed byte, allocator string, baseline, ceiling int64,
) syntheticEvidence {
	t.Helper()
	name := mustAllocator(t, allocator)
	body, err := erasureledger.NewComponentReservation(name, baseline, ceiling)
	if err != nil {
		t.Fatalf("NewComponentReservation(%s, %d, %d): %v", allocator, baseline, ceiling, err)
	}
	record := syntheticRecord(t, erasureledger.KindComponentReservation, epoch, stream, seq, keySeed, body)
	return syntheticEvidenceFor(t, record, true)
}

func syntheticComponentKey(t *testing.T, lineage erasureledger.LineageID,
	stream erasureledger.StreamID, allocator string,
) erasureledger.ComponentKey {
	t.Helper()
	key, err := erasureledger.NewComponentKey(lineage, stream, mustAllocator(t, allocator))
	if err != nil {
		t.Fatalf("NewComponentKey(%s): %v", allocator, err)
	}
	return key
}

func decodeSyntheticEvidence(evidence []syntheticEvidence, proof syntheticRecoveryProof) ([]erasureledger.Record, error) {
	if !proof.complete() {
		return nil, errSyntheticRecoveryNotReady
	}
	records := make([]erasureledger.Record, 0, len(evidence))
	for _, item := range evidence {
		if !item.confirmed {
			return nil, errSyntheticRecoveryNotReady
		}
		record, err := erasureledger.Decode(item.frame)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, nil
}

func syntheticAbsoluteNext(restored int64, allocator string, wireMax int64,
	evidence []syntheticEvidence, proof syntheticRecoveryProof,
) (int64, error) {
	if restored < 0 || wireMax < 1 {
		return 0, errSyntheticRecoveryNotReady
	}
	name, err := erasureledger.ValidateAllocator(allocator)
	if err != nil {
		return 0, err
	}
	classification, known := erasureledger.ClassifyAllocator(name)
	if !known || classification != erasureledger.AllocatorAbsoluteMax {
		return 0, errSyntheticRecoveryNotReady
	}
	records, err := decodeSyntheticEvidence(evidence, proof)
	if err != nil {
		return 0, err
	}
	ceiling := restored
	for _, record := range records {
		reservation, ok := record.Payload.(erasureledger.Reservation)
		if !ok || reservation.Allocator != name {
			continue
		}
		ceiling = max(ceiling, reservation.Ceiling)
	}
	if ceiling == math.MaxInt64 || ceiling >= wireMax {
		return 0, errSyntheticRecoveryNotReady
	}
	return ceiling + 1, nil
}

func syntheticComponentDelta(evidence []syntheticEvidence, proof syntheticRecoveryProof,
	expected []erasureledger.ComponentKey, snapshot map[erasureledger.ComponentKey]erasureledger.ComponentBaseline,
) (int64, error) {
	records, err := decodeSyntheticEvidence(evidence, proof)
	if err != nil {
		return 0, err
	}
	bindings := erasureledger.NewStreamBindings()
	for _, record := range records {
		if record.Kind != erasureledger.KindStreamBinding {
			continue
		}
		if err := bindings.AddConfirmed(record); err != nil {
			return 0, err
		}
	}

	expectedSet := make(map[erasureledger.ComponentKey]struct{}, len(expected))
	for _, key := range expected {
		expectedSet[key] = struct{}{}
	}
	if len(expectedSet) == 0 {
		return 0, errSyntheticRecoveryNotReady
	}
	ceilings := make(map[erasureledger.ComponentKey]int64, len(expectedSet))
	snapshotBaselines := make(map[erasureledger.ComponentKey]int64, len(expectedSet))
	evidenceBaselines := make(map[erasureledger.ComponentKey]int64, len(expectedSet))
	for _, record := range records {
		reservation, ok := record.Payload.(erasureledger.ComponentReservation)
		if !ok {
			continue
		}
		key, err := bindings.ComponentKeyFor(record)
		if err != nil {
			return 0, err
		}
		if _, applies := expectedSet[key]; !applies {
			continue
		}
		snapshotBaseline := int64(0)
		if restored, found := snapshot[key]; found {
			if restored.Key != key {
				return 0, errSyntheticRecoveryNotReady
			}
			snapshotBaseline = restored.Value
		}
		// The restored snapshot can be newer than a historical reservation's
		// baseline. Count only the ceiling remaining above that snapshot, while
		// rejecting evidence that starts beyond the restored value.
		if reservation.Baseline > snapshotBaseline || reservation.Ceiling < snapshotBaseline {
			return 0, errSyntheticRecoveryNotReady
		}
		if previous, found := evidenceBaselines[key]; found && previous != reservation.Baseline {
			return 0, errSyntheticRecoveryNotReady
		}
		evidenceBaselines[key] = reservation.Baseline
		if previous, found := snapshotBaselines[key]; found && previous != snapshotBaseline {
			return 0, errSyntheticRecoveryNotReady
		}
		snapshotBaselines[key] = snapshotBaseline
		ceilings[key] = max(ceilings[key], reservation.Ceiling)
	}

	var total int64
	for key := range expectedSet {
		ceiling, found := ceilings[key]
		if !found {
			return 0, errSyntheticRecoveryNotReady
		}
		baseline := snapshotBaselines[key]
		if ceiling < baseline {
			return 0, errSyntheticRecoveryNotReady
		}
		delta := ceiling - baseline
		if total > math.MaxInt64-delta {
			return 0, errSyntheticRecoveryNotReady
		}
		total += delta
	}
	return total, nil
}

func syntheticRandomIDExcluded(class erasureledger.RandomClass, id int64,
	evidence []syntheticEvidence, proof syntheticRecoveryProof,
) (bool, error) {
	records, err := decodeSyntheticEvidence(evidence, proof)
	if err != nil {
		return false, err
	}
	for _, record := range records {
		exclusion, ok := record.Payload.(erasureledger.RandomExclusion)
		if ok && exclusion.Class == class && slices.Contains(exclusion.IDs, id) {
			return true, nil
		}
	}
	return false, nil
}

func syntheticRandomCreate(class erasureledger.RandomClass, id int64,
	evidence []syntheticEvidence, proof syntheticRecoveryProof, objects syntheticRandomObjects,
) error {
	excluded, err := syntheticRandomIDExcluded(class, id, evidence, proof)
	if err != nil {
		return err
	}
	if excluded {
		return errSyntheticRandomIDExcluded
	}
	key := syntheticRandomObjectKey{class: class, id: id}
	if _, exists := objects[key]; exists {
		return errSyntheticRandomIDCollision
	}
	objects[key] = struct{}{}
	return nil
}

func syntheticFreshRandomCreate(class erasureledger.RandomClass, id int64,
	prior []syntheticEvidence, candidate syntheticEvidence, proof syntheticRecoveryProof,
	objects syntheticRandomObjects,
) error {
	if !candidate.confirmed || !proof.complete() {
		return errSyntheticRecoveryNotReady
	}
	excluded, err := syntheticRandomIDExcluded(class, id, prior, proof)
	if err != nil {
		return err
	}
	if excluded {
		return errSyntheticRandomIDExcluded
	}
	record, err := erasureledger.Decode(candidate.frame)
	if err != nil {
		return err
	}
	exclusion, ok := record.Payload.(erasureledger.RandomExclusion)
	if !ok || exclusion.Class != class || !slices.Contains(exclusion.IDs, id) {
		return errSyntheticRecoveryNotReady
	}
	key := syntheticRandomObjectKey{class: class, id: id}
	if _, exists := objects[key]; exists {
		return errSyntheticRandomIDCollision
	}
	objects[key] = struct{}{}
	return nil
}

type syntheticComponentProgress struct {
	snapshotBaseline int64
	value            int64
	applied          map[erasureledger.ComponentKey]int64
}

func newSyntheticComponentProgress(snapshotBaseline int64) syntheticComponentProgress {
	return syntheticComponentProgress{
		snapshotBaseline: snapshotBaseline,
		value:            snapshotBaseline,
		applied:          make(map[erasureledger.ComponentKey]int64),
	}
}

func (p *syntheticComponentProgress) apply(key erasureledger.ComponentKey, delta int64) error {
	if prior, found := p.applied[key]; found {
		if prior != delta {
			return errSyntheticRecoveryNotReady
		}
		return nil
	}
	if delta < 0 || p.value < 0 || delta > math.MaxInt64-p.value {
		return errSyntheticRecoveryNotReady
	}
	p.value += delta
	p.applied[key] = delta
	return nil
}

func (p *syntheticComponentProgress) persist() syntheticComponentProgress {
	persisted := newSyntheticComponentProgress(p.snapshotBaseline)
	persisted.value = p.value
	maps.Copy(persisted.applied, p.applied)
	return persisted
}

type syntheticCounter struct {
	value           int64
	capacity        int64
	spent           int64
	wireMax         int64
	confirmed       bool
	providerStalled bool
	providerWaits   int
}

// syntheticProviderProbe keeps confirmation and refill outside the modeled
// transaction and allocator locks. It is an inert acceptance probe; no runtime
// recovery or allocator path consumes it.
type syntheticProviderProbe struct {
	transactionOpen bool
	ownerLocked     bool
	channelLocked   bool
}

func (p *syntheticProviderProbe) requireOutsideLocks() error {
	if p.transactionOpen || p.ownerLocked || p.channelLocked {
		return errSyntheticOperationUnderLock
	}
	return nil
}

func (p *syntheticProviderProbe) run(operation func() error) error {
	if err := p.requireOutsideLocks(); err != nil {
		return err
	}
	return operation()
}

func (c *syntheticCounter) advance(units int64) error {
	if !c.confirmed || units < 1 || c.spent > c.capacity {
		return errSyntheticRecoveryNotReady
	}
	if units > c.capacity-c.spent {
		if c.providerStalled {
			return errSyntheticRecoveryNotReady
		}
		return c.waitForProvider()
	}
	if units > math.MaxInt64-c.value || c.value+units > c.wireMax {
		return errSyntheticRecoveryNotReady
	}
	c.value += units
	c.spent += units
	return nil
}

func (c *syntheticCounter) waitForProvider() error {
	c.providerWaits++
	if c.providerStalled {
		return errSyntheticProviderStalled
	}
	return errSyntheticRecoveryNotReady
}

func (c *syntheticCounter) advanceRecovery(units int64) error { return c.advance(units) }
func (c *syntheticCounter) issue(units int64) error           { return c.advance(units) }

func requireRefusal(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("operation succeeded without complete confirmed capacity")
	}
}

func TestSyntheticProviderWaitAttemptsCountEvenWhenStalled(t *testing.T) {
	t.Parallel()
	counter := syntheticCounter{providerStalled: true}
	if err := counter.waitForProvider(); !errors.Is(err, errSyntheticProviderStalled) {
		t.Fatalf("stalled provider wait = %v, want stalled error", err)
	}
	if counter.providerWaits != 1 {
		t.Fatalf("stalled provider wait attempts = %d, want 1", counter.providerWaits)
	}
}

func TestAbsoluteReservationRecoveryBounds(t *testing.T) {
	t.Parallel()
	absolute := []string{
		"users_id_seq", "files_id_seq", "chats_id_seq", "secret_chats_id_seq",
		"message_fanout_seq", "chat_admin_events_id_seq",
		// These internal-only identities remain explicitly classified too.
		"phone_codes_id_seq", "registration_invites_id_seq",
		"language_catalog_publication_audit_id_seq",
	}
	keySeeds := [][2]byte{
		{0x10, 0x11}, {0x12, 0x13}, {0x14, 0x15},
		{0x16, 0x17}, {0x18, 0x19}, {0x1a, 0x1b},
		{0x1c, 0x1d}, {0x1e, 0x1f}, {0x20, 0x21},
	}
	lowerKeySeeds := [...]byte{0x22, 0x23, 0x24, 0x25, 0x26, 0x27, 0x28, 0x29, 0x2a}
	for i, allocator := range absolute {
		t.Run(allocator, func(t *testing.T) {
			t.Parallel()
			streamA, streamB := streamID(0x50), streamID(0x60)
			evidence := []syntheticEvidence{
				syntheticReservation(t, 1, streamA, 1, keySeeds[i][0], allocator, 100),
				syntheticReservation(t, 1, streamB, 1, keySeeds[i][1], allocator, 140),
				// This later record is a stale lower ceiling, so replay must
				// retain the confirmed maximum rather than use the last write.
				syntheticReservation(t, 1, streamB, 2, lowerKeySeeds[i], allocator, 100),
			}
			got, err := syntheticAbsoluteNext(80, allocator, math.MaxInt64, evidence, completeSyntheticProof())
			if err != nil {
				t.Fatalf("syntheticAbsoluteNext: %v", err)
			}
			if got != 141 {
				t.Errorf("next legal ID = %d, want 141 after the lower ceiling", got)
			}
		})
	}
}

func TestProviderConfirmationPrecedesReservationVisibility(t *testing.T) {
	t.Parallel()
	allocator := "secret_chats_id_seq"
	stream := streamID(0x50)
	body, err := erasureledger.NewReservation(mustAllocator(t, allocator), 100)
	if err != nil {
		t.Fatalf("NewReservation: %v", err)
	}
	record := syntheticRecord(t, erasureledger.KindReservation, 1, stream, 1, 0x10, body)
	frame, err := erasureledger.Encode(record)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	provider := newSynthProvider(t, newMemoryStore(t))
	writer := provider.newWriter(1, stream)
	handle, err := writer.Create(record)
	if err != nil {
		t.Fatalf("Create pending reservation: %v", err)
	}
	if _, err := (replayer{p: provider}).Get(record.OpKey); !errors.Is(err, errNotFound) {
		t.Fatalf("pending reservation visibility = %v, want not-found", err)
	}
	if _, err := syntheticAbsoluteNext(80, allocator, math.MaxInt64,
		[]syntheticEvidence{{frame: frame, confirmed: false}}, completeSyntheticProof()); err == nil {
		t.Fatal("an unconfirmed ceiling opened restored allocation")
	}
	counter := syntheticCounter{}
	confirmationCalls, refillCalls := 0, 0
	confirm := func() error {
		if _, err := writer.Confirm(handle); err != nil {
			return err
		}
		confirmationCalls++
		return nil
	}
	refill := func() error {
		counter.capacity = 100
		refillCalls++
		return nil
	}
	for _, probe := range []syntheticProviderProbe{
		{transactionOpen: true},
		{ownerLocked: true},
		{channelLocked: true},
	} {
		if err := probe.run(confirm); !errors.Is(err, errSyntheticOperationUnderLock) {
			t.Errorf("confirmation under transaction or allocator lock = %v, want refusal", err)
		}
		if err := probe.run(refill); !errors.Is(err, errSyntheticOperationUnderLock) {
			t.Errorf("refill under transaction or allocator lock = %v, want refusal", err)
		}
	}
	if confirmationCalls != 0 || refillCalls != 0 || counter.capacity != 0 {
		t.Fatalf("operations ran under transaction or allocator lock: confirmations=%d refills=%d capacity=%d",
			confirmationCalls, refillCalls, counter.capacity)
	}
	probe := syntheticProviderProbe{}
	if err := probe.run(confirm); err != nil {
		t.Fatalf("Confirm reservation outside transaction and locks: %v", err)
	}
	if err := probe.run(refill); err != nil {
		t.Fatalf("refill outside transaction and locks: %v", err)
	}
	visible, err := (replayer{p: provider}).Get(record.OpKey)
	if err != nil {
		t.Fatalf("confirmed reservation visibility: %v", err)
	}
	next, err := syntheticAbsoluteNext(80, allocator, math.MaxInt64,
		[]syntheticEvidence{{frame: visible.Body, confirmed: true}}, completeSyntheticProof())
	if err != nil {
		t.Fatalf("read confirmed reservation: %v", err)
	}
	if next != 101 {
		t.Errorf("next legal secret-chat ID = %d, want 101", next)
	}
	if confirmationCalls != 1 || refillCalls != 1 || counter.capacity != 100 {
		t.Errorf("unlocked operations: confirmations=%d refills=%d capacity=%d, want 1, 1, 100",
			confirmationCalls, refillCalls, counter.capacity)
	}
}

func TestSecretChatReservationReplayHasNoContentPath(t *testing.T) {
	t.Parallel()
	reservation, err := erasureledger.NewReservation(mustAllocator(t, "secret_chats_id_seq"), 100)
	if err != nil {
		t.Fatalf("NewReservation: %v", err)
	}
	if got := reflect.TypeFor[erasureledger.Reservation]().NumField(); got != 2 {
		t.Fatalf("Reservation has %d fields, want only allocator and ceiling", got)
	}
	record := syntheticRecord(t, erasureledger.KindReservation, 1, streamID(0x50), 1, 0x10, reservation)
	next, err := syntheticAbsoluteNext(80, "secret_chats_id_seq", math.MaxInt64,
		[]syntheticEvidence{syntheticEvidenceFor(t, record, true)}, completeSyntheticProof())
	if err != nil || next != 101 {
		t.Errorf("secret-chat identifier replay = %d, %v; want next ID 101", next, err)
	}
	if _, err := erasureledger.Decode(frameOf(erasureledger.KindReservation, join(
		bytesField(1, []byte("secret_chats_id_seq")),
		varintField(2, 100),
		bytesField(21, []byte("ciphertext")),
	))); !errors.Is(err, erasureledger.ErrRejected) {
		t.Fatalf("secret-chat content entered reservation replay: %v", err)
	}
	if reservation.Ceiling != 100 {
		t.Errorf("reservation ceiling = %d, want the identifier-only ceiling", reservation.Ceiling)
	}
}

func TestReservationRecoveryRefusesUnknownEvidenceAndWireExhaustion(t *testing.T) {
	t.Parallel()
	proof := completeSyntheticProof()
	unknownKind := syntheticEvidence{
		frame:     frameOf(erasureledger.Kind(99), nil),
		confirmed: true,
	}
	if _, err := syntheticAbsoluteNext(80, "users_id_seq", math.MaxInt64,
		[]syntheticEvidence{unknownKind}, proof); !errors.Is(err, erasureledger.ErrNotReady) {
		t.Errorf("unknown record kind readiness = %v, want ErrNotReady", err)
	}
	unknownAllocator := syntheticEvidence{
		frame: frameOf(erasureledger.KindReservation, join(
			bytesField(1, []byte("unclassified_allocator")),
			varintField(2, 100),
		)),
		confirmed: true,
	}
	if _, err := syntheticAbsoluteNext(80, "users_id_seq", math.MaxInt64,
		[]syntheticEvidence{unknownAllocator}, proof); !errors.Is(err, erasureledger.ErrNotReady) {
		t.Errorf("unknown allocator readiness = %v, want ErrNotReady", err)
	}
	if _, err := syntheticAbsoluteNext(math.MaxInt64, "users_id_seq", math.MaxInt64, nil, proof); err == nil {
		t.Error("int64 sequence overflow opened readiness")
	}

	for _, allocator := range []string{"users_id_seq", "files_id_seq", "chats_id_seq", "secret_chats_id_seq"} {
		t.Run(allocator, func(t *testing.T) {
			t.Parallel()
			atEdge := []syntheticEvidence{syntheticReservation(t, 1, streamID(0x50), 1, 0x10, allocator, math.MaxInt32)}
			if _, err := syntheticAbsoluteNext(0, allocator, math.MaxInt32, atEdge, proof); err == nil {
				t.Errorf("ceiling %d issued an out-of-width next ID", math.MaxInt32)
			}
			belowEdge := []syntheticEvidence{syntheticReservation(t, 1, streamID(0x50), 1, 0x11, allocator, math.MaxInt32-1)}
			got, err := syntheticAbsoluteNext(0, allocator, math.MaxInt32, belowEdge, proof)
			if err != nil || got != math.MaxInt32 {
				t.Errorf("next ID at the width boundary = %d, %v; want %d", got, err, math.MaxInt32)
			}
		})
	}
}

type twoStreamShardFixture struct {
	evidence []syntheticEvidence
	expected []erasureledger.ComponentKey
	snapshot map[erasureledger.ComponentKey]erasureledger.ComponentBaseline
}

func makeTwoStreamShardFixture(t *testing.T) twoStreamShardFixture {
	t.Helper()
	const epoch = int64(4)
	lineage := lineageID(0x20)
	streamA, streamB := streamID(0x50), streamID(0x60)
	keyANext := syntheticComponentKey(t, lineage, streamA, "update_state_next_local_id")
	keyBNext := syntheticComponentKey(t, lineage, streamB, "update_state_next_local_id")
	keyAPts := syntheticComponentKey(t, lineage, streamA, "update_state_pts")
	keyBPts := syntheticComponentKey(t, lineage, streamB, "update_state_pts")
	baseA, err := erasureledger.NewComponentBaseline(keyANext, 10, erasureledger.ComponentInherited)
	if err != nil {
		t.Fatalf("NewComponentBaseline A local ID: %v", err)
	}
	baseB, err := erasureledger.NewComponentBaseline(keyBNext, 20, erasureledger.ComponentInherited)
	if err != nil {
		t.Fatalf("NewComponentBaseline B local ID: %v", err)
	}
	basePtsA, err := erasureledger.NewComponentBaseline(keyAPts, 7, erasureledger.ComponentInherited)
	if err != nil {
		t.Fatalf("NewComponentBaseline A pts: %v", err)
	}
	return twoStreamShardFixture{
		evidence: []syntheticEvidence{
			syntheticBinding(t, epoch, streamA, lineage, 0x10),
			syntheticBinding(t, epoch, streamB, lineage, 0x11),
			syntheticComponentReservation(t, epoch, streamA, 2, 0x12, "update_state_next_local_id", 10, 14),
			syntheticComponentReservation(t, epoch, streamA, 3, 0x13, "update_state_pts", 7, 12),
			// A stale lower ceiling for one existing key is a maximum, not a
			// second component and never a rollback.
			syntheticComponentReservation(t, epoch, streamA, 4, 0x14, "update_state_next_local_id", 10, 13),
			syntheticComponentReservation(t, epoch, streamB, 2, 0x15, "update_state_next_local_id", 20, 23),
			// This fresh loss-window component is absent from the snapshot, so
			// its baseline is zero.
			syntheticComponentReservation(t, epoch, streamB, 3, 0x16, "update_state_pts", 0, 6),
		},
		// keyANext appears twice to model two state views sharing the same
		// physical component; the sum counts its confirmed delta once.
		expected: []erasureledger.ComponentKey{keyANext, keyBNext, keyAPts, keyBPts, keyANext},
		snapshot: map[erasureledger.ComponentKey]erasureledger.ComponentBaseline{
			keyANext: baseA, keyBNext: baseB, keyAPts: basePtsA,
		},
	}
}

func TestReservationSumAcrossTwoStreamsTwoShards(t *testing.T) {
	t.Parallel()
	fixture := makeTwoStreamShardFixture(t)
	got, err := syntheticComponentDelta(fixture.evidence, completeSyntheticProof(), fixture.expected, fixture.snapshot)
	if err != nil {
		t.Fatalf("syntheticComponentDelta: %v", err)
	}
	if got != 18 {
		t.Fatalf("component delta D = %d, want 18", got)
	}
	if got == 6 || got == 11 {
		t.Errorf("component delta D = %d, the incomplete sums 6 and 11 are forbidden", got)
	}

	counters := []struct {
		name    string
		start   int64
		want    int64
		wireMax int64
	}{
		{"owner next-ID", 100, 118, math.MaxInt32},
		{"owner pts", 200, 218, math.MaxInt64},
		{"profile revision", 30, 48, math.MaxInt64},
		{"channel next-ID", 50, 68, math.MaxInt32},
		{"channel pts", 60, 78, math.MaxInt64},
	}
	unconfirmed := syntheticCounter{value: 100, capacity: got, wireMax: math.MaxInt32, providerStalled: true}
	if err := unconfirmed.issue(1); err == nil || unconfirmed.spent != 0 {
		t.Errorf("unconfirmed component capacity issued an ID: err=%v spent=%d", err, unconfirmed.spent)
	}
	for _, tc := range counters {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			counter := syntheticCounter{
				value: tc.start, capacity: got, wireMax: tc.wireMax,
				confirmed: true, providerStalled: true,
			}
			if err := counter.advanceRecovery(8); err != nil {
				t.Fatalf("recovery advancement: %v", err)
			}
			if err := counter.issue(10); err != nil {
				t.Fatalf("post-recovery advancement: %v", err)
			}
			if counter.value < tc.want {
				t.Errorf("restored value = %d, want at least %d", counter.value, tc.want)
			}
			if counter.spent != got {
				t.Errorf("spent confirmed capacity = %d, want %d", counter.spent, got)
			}
			if counter.providerWaits != 0 {
				t.Errorf("waited %d times on the stalled provider with confirmed capacity", counter.providerWaits)
			}
			requireRefusal(t, counter.issue(1))
			if counter.providerWaits != 0 {
				t.Errorf("exhausted capacity attempted %d provider waits", counter.providerWaits)
			}
		})
	}
}

func TestProfileRevisionCeilingSurvivesCompaction(t *testing.T) {
	t.Parallel()
	const restoredRevision = int64(30)
	lineage := lineageID(0x20)
	streamA, streamB := streamID(0x50), streamID(0x60)
	keyA := syntheticComponentKey(t, lineage, streamA, "profile_photo_state_mutation_revision")
	keyB := syntheticComponentKey(t, lineage, streamB, "profile_photo_state_mutation_revision")
	if keyA == keyB {
		t.Fatal("profile revision component keys collapsed across streams")
	}
	baseA, err := erasureledger.NewComponentBaseline(keyA, restoredRevision, erasureledger.ComponentInherited)
	if err != nil {
		t.Fatalf("stream A snapshot baseline: %v", err)
	}
	baseB, err := erasureledger.NewComponentBaseline(keyB, restoredRevision, erasureledger.ComponentInherited)
	if err != nil {
		t.Fatalf("stream B snapshot baseline: %v", err)
	}
	evidence := []syntheticEvidence{
		syntheticBinding(t, 4, streamA, lineage, 0x10),
		syntheticBinding(t, 4, streamB, lineage, 0x11),
		// A later confirmed ceiling is retained for stream A's component key.
		syntheticComponentReservation(t, 4, streamA, 2, 0x12,
			"profile_photo_state_mutation_revision", restoredRevision, 40),
		syntheticComponentReservation(t, 4, streamA, 3, 0x13,
			"profile_photo_state_mutation_revision", restoredRevision, 41),
		syntheticComponentReservation(t, 4, streamB, 2, 0x14,
			"profile_photo_state_mutation_revision", restoredRevision, 37),
	}
	// Gallery events have been compacted away. Only the non-compacting per-owner
	// state baselines and the two stream-keyed confirmed ceilings establish the
	// recovery bound; there is no per-owner activity record in this model.
	delta, err := syntheticComponentDelta(evidence, completeSyntheticProof(),
		[]erasureledger.ComponentKey{keyA, keyB}, map[erasureledger.ComponentKey]erasureledger.ComponentBaseline{
			keyA: baseA,
			keyB: baseB,
		})
	if err != nil {
		t.Fatalf("profile component ceiling replay: %v", err)
	}
	if delta != 18 {
		t.Fatalf("profile recovery delta D = %d, want 18 from both stream components", delta)
	}
	if got := restoredRevision + delta; got != 48 {
		t.Fatalf("restored revision lower bound = %d, want 48", got)
	}
	if got := reflect.TypeFor[erasureledger.ComponentReservation]().NumField(); got != 3 {
		t.Fatalf("component ceiling has %d fields, want only allocator, baseline and ceiling", got)
	}

	unconfirmed := append([]syntheticEvidence(nil), evidence...)
	unconfirmed[len(unconfirmed)-1].confirmed = false
	if _, err := syntheticComponentDelta(unconfirmed, completeSyntheticProof(),
		[]erasureledger.ComponentKey{keyA, keyB}, map[erasureledger.ComponentKey]erasureledger.ComponentBaseline{
			keyA: baseA,
			keyB: baseB,
		}); !errors.Is(err, errSyntheticRecoveryNotReady) {
		t.Errorf("unconfirmed profile component ceiling readiness = %v, want not-ready", err)
	}
}

func TestComponentReservationMaxWithinKeyAndSumAcrossKeys(t *testing.T) {
	t.Parallel()
	fixture := makeTwoStreamShardFixture(t)
	streamA := streamID(0x50)
	fixture.evidence = append(fixture.evidence,
		syntheticComponentReservation(t, 4, streamA, 5, 0x17, "update_state_next_local_id", 10, 16))
	got, err := syntheticComponentDelta(fixture.evidence, completeSyntheticProof(), fixture.expected, fixture.snapshot)
	if err != nil {
		t.Fatalf("syntheticComponentDelta: %v", err)
	}
	if got != 20 {
		t.Errorf("same-key maximum plus distinct-key deltas = %d, want 20", got)
	}
}

func TestComponentRecoveryRequiresCompleteAndConsistentEvidence(t *testing.T) {
	t.Parallel()
	fixture := makeTwoStreamShardFixture(t)
	proofs := []struct {
		name  string
		proof syntheticRecoveryProof
	}{
		{"pagination", syntheticRecoveryProof{streamsComplete: true, checkpointsCovered: true}},
		{"stream inventory", syntheticRecoveryProof{paginationComplete: true, checkpointsCovered: true}},
		{"checkpoint coverage", syntheticRecoveryProof{paginationComplete: true, streamsComplete: true}},
	}
	for _, tc := range proofs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := syntheticComponentDelta(fixture.evidence, tc.proof, fixture.expected, fixture.snapshot); err == nil {
				t.Fatal("incomplete recovery proof opened readiness")
			}
		})
	}

	missing := fixture.evidence[:len(fixture.evidence)-1]
	if _, err := syntheticComponentDelta(missing, completeSyntheticProof(), fixture.expected, fixture.snapshot); err == nil {
		t.Error("missing covering component evidence opened readiness")
	}
	if _, err := syntheticComponentDelta(fixture.evidence, completeSyntheticProof(), nil, fixture.snapshot); err == nil {
		t.Error("an empty expected component set opened readiness")
	}

	componentOnly := []syntheticEvidence{
		syntheticComponentReservation(t, 4, streamID(0x50), 2, 0x30, "channel_state_pts", 0, 4),
	}
	missingKey := syntheticComponentKey(t, lineageID(0x20), streamID(0x50), "channel_state_pts")
	if _, err := syntheticComponentDelta(componentOnly, completeSyntheticProof(),
		[]erasureledger.ComponentKey{missingKey}, nil); !errors.Is(err, erasureledger.ErrNotReady) {
		t.Errorf("unbound component readiness = %v, want ErrNotReady", err)
	}

	conflicted := []syntheticEvidence{
		syntheticBinding(t, 4, streamID(0x50), lineageID(0x20), 0x31),
		syntheticBinding(t, 4, streamID(0x50), lineageID(0x40), 0x32),
		syntheticComponentReservation(t, 4, streamID(0x50), 2, 0x33, "channel_state_pts", 0, 4),
	}
	if _, err := syntheticComponentDelta(conflicted, completeSyntheticProof(),
		[]erasureledger.ComponentKey{missingKey}, nil); !errors.Is(err, erasureledger.ErrNotReady) {
		t.Errorf("conflicting stream ownership readiness = %v, want ErrNotReady", err)
	}

	badBaseline := []syntheticEvidence{
		syntheticBinding(t, 4, streamID(0x50), lineageID(0x20), 0x34),
		syntheticComponentReservation(t, 4, streamID(0x50), 2, 0x35, "channel_state_pts", 0, 4),
		syntheticComponentReservation(t, 4, streamID(0x50), 3, 0x36, "channel_state_pts", 1, 4),
	}
	if _, err := syntheticComponentDelta(badBaseline, completeSyntheticProof(),
		[]erasureledger.ComponentKey{missingKey}, nil); err == nil {
		t.Error("conflicting baseline ownership opened readiness")
	}
	nonzeroSnapshotBaseline, err := erasureledger.NewComponentBaseline(missingKey, 12, erasureledger.ComponentInherited)
	if err != nil {
		t.Fatalf("nonzero snapshot baseline: %v", err)
	}
	conflictingHistoricalBaselines := []syntheticEvidence{
		syntheticBinding(t, 4, streamID(0x50), lineageID(0x20), 0x37),
		syntheticComponentReservation(t, 4, streamID(0x50), 2, 0x38, "channel_state_pts", 10, 20),
		syntheticComponentReservation(t, 4, streamID(0x50), 3, 0x39, "channel_state_pts", 11, 20),
	}
	if _, err := syntheticComponentDelta(conflictingHistoricalBaselines, completeSyntheticProof(),
		[]erasureledger.ComponentKey{missingKey}, map[erasureledger.ComponentKey]erasureledger.ComponentBaseline{
			missingKey: nonzeroSnapshotBaseline,
		}); !errors.Is(err, errSyntheticRecoveryNotReady) {
		t.Errorf("same-key evidence baselines 10 and 11 with snapshot 12 = %v, want not-ready", err)
	}

	if _, err := erasureledger.Decode(frameOf(erasureledger.KindComponentReservation, join(
		bytesField(1, []byte("channel_state_pts")), varintField(2, 5), varintField(3, 4),
	))); !errors.Is(err, erasureledger.ErrRejected) {
		t.Errorf("reservation with a negative/inconsistent delta decoded: %v", err)
	}

	lineage := lineageID(0x60)
	streamA, streamB := streamID(0x70), streamID(0x80)
	overflowEvidence := []syntheticEvidence{
		syntheticBinding(t, 4, streamA, lineage, 0x40),
		syntheticBinding(t, 4, streamB, lineage, 0x41),
		syntheticComponentReservation(t, 4, streamA, 2, 0x42, "channel_state_pts", 0, math.MaxInt64),
		syntheticComponentReservation(t, 4, streamB, 2, 0x43, "channel_state_pts", 0, math.MaxInt64),
	}
	overflowKeys := []erasureledger.ComponentKey{
		syntheticComponentKey(t, lineage, streamA, "channel_state_pts"),
		syntheticComponentKey(t, lineage, streamB, "channel_state_pts"),
	}
	if _, err := syntheticComponentDelta(overflowEvidence, completeSyntheticProof(), overflowKeys, nil); err == nil {
		t.Error("component-sum arithmetic overflow opened readiness")
	}
}

func TestInterruptedAndOlderRestoreLineages(t *testing.T) {
	t.Parallel()
	const (
		originalEpoch = int64(4)
		sharedEpoch   = int64(5)
	)
	originalLineage := lineageID(0x10)
	l1, l2 := lineageID(0x20), lineageID(0x30)
	originalStream := streamID(0x40)
	l1A, l1B, l2Stream := streamID(0x60), streamID(0x70), streamID(0x80)

	originalKey := syntheticComponentKey(t, originalLineage, originalStream, "update_state_pts")
	s0Baseline, err := erasureledger.NewComponentBaseline(originalKey, 10, erasureledger.ComponentInherited)
	if err != nil {
		t.Fatalf("original S0 snapshot baseline: %v", err)
	}
	l1SnapshotBaseline, err := erasureledger.NewComponentBaseline(originalKey, 12, erasureledger.ComponentInherited)
	if err != nil {
		t.Fatalf("L1 snapshot baseline: %v", err)
	}
	l1Snapshot := map[erasureledger.ComponentKey]erasureledger.ComponentBaseline{
		originalKey: l1SnapshotBaseline,
	}
	l2Snapshot := map[erasureledger.ComponentKey]erasureledger.ComponentBaseline{
		originalKey: s0Baseline,
	}
	originalEvidence := []syntheticEvidence{
		syntheticBinding(t, originalEpoch, originalStream, originalLineage, 0x10),
		syntheticComponentReservation(t, originalEpoch, originalStream, 2, 0x12, "update_state_pts", 10, 20),
	}

	l1Keys := []erasureledger.ComponentKey{
		syntheticComponentKey(t, l1, l1A, "update_state_next_local_id"),
		syntheticComponentKey(t, l1, l1B, "update_state_next_local_id"),
		syntheticComponentKey(t, l1, l1A, "update_state_pts"),
		syntheticComponentKey(t, l1, l1B, "update_state_pts"),
	}
	l1Evidence := []syntheticEvidence{
		syntheticBinding(t, sharedEpoch, l1A, l1, 0x20),
		syntheticBinding(t, sharedEpoch, l1B, l1, 0x21),
		syntheticComponentReservation(t, sharedEpoch, l1A, 2, 0x22, "update_state_next_local_id", 0, 4),
		syntheticComponentReservation(t, sharedEpoch, l1B, 2, 0x23, "update_state_next_local_id", 0, 3),
		syntheticComponentReservation(t, sharedEpoch, l1A, 3, 0x24, "update_state_pts", 0, 5),
		syntheticComponentReservation(t, sharedEpoch, l1B, 3, 0x25, "update_state_pts", 0, 2),
	}
	l1Expected := append([]erasureledger.ComponentKey{originalKey}, l1Keys...)
	l1Partial := append(append([]syntheticEvidence{}, originalEvidence...), l1Evidence[:4]...)
	if _, err := syntheticComponentDelta(l1Partial,
		completeSyntheticProof(), l1Keys, nil); err == nil {
		t.Fatal("L1 with 2 of 4 fresh components was ready")
	}
	if _, err := syntheticComponentDelta(l1Partial,
		completeSyntheticProof(), l1Expected, l1Snapshot); !errors.Is(err, errSyntheticRecoveryNotReady) {
		t.Fatalf("partial L1 historical and fresh component readiness = %v, want not-ready", err)
	}

	freshComponents := make([]struct {
		key   erasureledger.ComponentKey
		delta int64
	}, len(l1Keys))
	for i, key := range l1Keys {
		delta, err := syntheticComponentDelta(l1Evidence, completeSyntheticProof(),
			[]erasureledger.ComponentKey{key}, nil)
		if err != nil {
			t.Fatalf("fresh L1 component %v: %v", key, err)
		}
		freshComponents[i] = struct {
			key   erasureledger.ComponentKey
			delta int64
		}{key: key, delta: delta}
	}
	for _, component := range freshComponents[:2] {
		partialDelta, err := syntheticComponentDelta(l1Partial, completeSyntheticProof(),
			[]erasureledger.ComponentKey{component.key}, nil)
		if err != nil || partialDelta != component.delta {
			t.Fatalf("partial L1 component %v = %d, %v; want %d", component.key, partialDelta, err, component.delta)
		}
	}
	uninterrupted := newSyntheticComponentProgress(12)
	for _, component := range freshComponents {
		if err := uninterrupted.apply(component.key, component.delta); err != nil {
			t.Fatalf("uninterrupted L1 apply %v: %v", component.key, err)
		}
	}
	interrupted := newSyntheticComponentProgress(12)
	for _, component := range freshComponents[:2] {
		if err := interrupted.apply(component.key, component.delta); err != nil {
			t.Fatalf("partial L1 apply %v: %v", component.key, err)
		}
	}
	if interrupted.value != 19 || len(interrupted.applied) != 2 {
		t.Fatalf("partial L1 snapshot value=%d applied=%d, want 19 and 2", interrupted.value, len(interrupted.applied))
	}
	resumed := interrupted.persist()
	if resumed.snapshotBaseline != 12 || resumed.value != interrupted.value || len(resumed.applied) != 2 {
		t.Fatalf("persisted L1 snapshot/progress = (%d, %d, %d), want (12, 19, 2)",
			resumed.snapshotBaseline, resumed.value, len(resumed.applied))
	}

	resumedEvidence := append(append([]syntheticEvidence{}, originalEvidence...), l1Evidence...)
	l1HistoricalDelta, err := syntheticComponentDelta(originalEvidence, completeSyntheticProof(),
		[]erasureledger.ComponentKey{originalKey}, l1Snapshot)
	if err != nil || l1HistoricalDelta != 8 {
		t.Fatalf("L1 historical bound = %d, %v; want 20-12=8", l1HistoricalDelta, err)
	}
	l1FreshDelta, err := syntheticComponentDelta(l1Evidence, completeSyntheticProof(), l1Keys, nil)
	if err != nil || l1FreshDelta != 14 {
		t.Fatalf("fresh L1 bound = %d, %v; want 14", l1FreshDelta, err)
	}
	l1Capacity, err := syntheticComponentDelta(resumedEvidence, completeSyntheticProof(), l1Expected, l1Snapshot)
	if err != nil {
		t.Fatalf("resumed L1 historical and fresh component sum: %v", err)
	}
	if l1Capacity != 22 {
		t.Fatalf("resumed L1 capacity = %d, want 22 (8 historical + 14 fresh)", l1Capacity)
	}
	for _, component := range freshComponents[2:] {
		if err := resumed.apply(component.key, component.delta); err != nil {
			t.Fatalf("resumed L1 apply %v: %v", component.key, err)
		}
	}
	// Resume can replay the persisted prefix; it must not charge those components
	// a second time or advance from a new snapshot baseline.
	for _, component := range freshComponents[:2] {
		if err := resumed.apply(component.key, component.delta); err != nil {
			t.Fatalf("replayed L1 prefix %v: %v", component.key, err)
		}
	}
	if !reflect.DeepEqual(resumed, uninterrupted) {
		t.Errorf("resumed L1 state = %+v, uninterrupted state = %+v", resumed, uninterrupted)
	}

	if l1SnapshotBaseline.CanReserve() {
		t.Fatal("L1 reopened the inherited historical component for reservations")
	}
	for _, key := range l1Keys {
		if _, inherited := l1Snapshot[key]; inherited {
			t.Errorf("fresh L1 component %v was incorrectly inherited", key)
		}
	}
	// The historical 8-unit anti-reuse bound is closed to L1 reservations; the
	// counter can spend only the 14 fresh units confirmed in L1.
	l1State := syntheticCounter{value: 12, capacity: l1FreshDelta, wireMax: math.MaxInt64,
		confirmed: true, providerStalled: true}
	if err := l1State.advanceRecovery(8); err != nil {
		t.Fatalf("L1 recovery advancement by 8: %v", err)
	}
	if err := l1State.issue(6); err != nil {
		t.Fatalf("L1 issue by 6: %v", err)
	}
	if l1State.value != 26 || l1State.spent != 14 {
		t.Errorf("L1 state=%d capacity-spent=%d, want state 26 and 14 charged units", l1State.value, l1State.spent)
	}
	requireRefusal(t, l1State.issue(1))
	if l1State.providerWaits != 0 {
		t.Errorf("L1 exhausted recovery attempted %d provider waits", l1State.providerWaits)
	}

	l2Expected := append([]erasureledger.ComponentKey{originalKey}, l1Keys...)
	l2HistoricalDelta, err := syntheticComponentDelta(originalEvidence, completeSyntheticProof(),
		[]erasureledger.ComponentKey{originalKey}, l2Snapshot)
	if err != nil || l2HistoricalDelta != 10 {
		t.Fatalf("L2 historical bound = %d, %v; want 20-10=10", l2HistoricalDelta, err)
	}
	l2Delta, err := syntheticComponentDelta(resumedEvidence, completeSyntheticProof(), l2Expected, l2Snapshot)
	if err != nil {
		t.Fatalf("L2 historical and fresh L1 reservation sum: %v", err)
	}
	if l2Delta < 16 || l2Delta == 10 {
		t.Errorf("L2 loss-window bound D = %d, want at least 16 and never 10", l2Delta)
	}
	if l2Delta != 24 {
		t.Errorf("L2 bound = %d, want 24 (10 historical + 14 fresh L1)", l2Delta)
	}
	if l2Expected[0] == l1Keys[0] {
		t.Error("historical and fresh L1 reservations collapsed to one component identity")
	}

	// A shared epoch number is not lineage evidence: L1's component record
	// cannot cover a key attributed to L2.
	l2OnL1Stream := syntheticComponentKey(t, l2, l1A, "update_state_next_local_id")
	if _, err := syntheticComponentDelta(l1Evidence, completeSyntheticProof(),
		[]erasureledger.ComponentKey{l2OnL1Stream}, nil); !errors.Is(err, errSyntheticRecoveryNotReady) {
		t.Errorf("L1 evidence counted as L2 progress = %v, want not-ready", err)
	}

	bindings := erasureledger.NewStreamBindings()
	for _, item := range append(append([]syntheticEvidence{}, originalEvidence...), l1Evidence...) {
		record, err := erasureledger.Decode(item.frame)
		if err != nil {
			t.Fatalf("Decode lineage evidence: %v", err)
		}
		if record.Kind == erasureledger.KindStreamBinding {
			if err := bindings.AddConfirmed(record); err != nil {
				t.Fatalf("AddConfirmed lineage binding: %v", err)
			}
		}
	}
	l1CompletionBody, err := erasureledger.NewEpoch(sharedEpoch, l1, erasureledger.EpochCompleted)
	if err != nil {
		t.Fatalf("NewEpoch L1 completion: %v", err)
	}
	l1Completion := syntheticRecord(t, erasureledger.KindEpoch, sharedEpoch, l1A, 4, 0x30, l1CompletionBody)
	if got, err := bindings.LineageFor(l1Completion); err != nil || got != l1 {
		t.Errorf("L1 completion attribution = %x, %v; want L1", got, err)
	}
	l2CompletionBody, err := erasureledger.NewEpoch(sharedEpoch, l2, erasureledger.EpochCompleted)
	if err != nil {
		t.Fatalf("NewEpoch L2 completion: %v", err)
	}
	wrongStreamCompletion := syntheticRecord(t, erasureledger.KindEpoch, sharedEpoch, l1A, 5, 0x31, l2CompletionBody)
	if _, err := bindings.LineageFor(wrongStreamCompletion); !errors.Is(err, erasureledger.ErrNotReady) {
		t.Errorf("L1 completion counted for L2 = %v, want refusal", err)
	}
	if _, err := bindings.LineageFor(syntheticRecord(t, erasureledger.KindComponentReservation,
		sharedEpoch, l2Stream, 2, 0x32, mustComponentReservation(t, "update_state_pts", 0, 1))); !errors.Is(err, erasureledger.ErrNotReady) {
		t.Errorf("unbound L2 stream readiness = %v, want refusal", err)
	}
	if err := bindings.AddConfirmed(mustBindingRecord(t, sharedEpoch, l2Stream, 1, l2, 0x33)); err != nil {
		t.Fatalf("AddConfirmed L2 stream: %v", err)
	}
	validL2Completion := syntheticRecord(t, erasureledger.KindEpoch, sharedEpoch, l2Stream, 2, 0x34, l2CompletionBody)
	if got, err := bindings.LineageFor(validL2Completion); err != nil || got != l2 {
		t.Errorf("L2 completion attribution = %x, %v; want L2", got, err)
	}
}

func mustComponentReservation(t *testing.T, allocator string, baseline, ceiling int64) erasureledger.ComponentReservation {
	t.Helper()
	body, err := erasureledger.NewComponentReservation(mustAllocator(t, allocator), baseline, ceiling)
	if err != nil {
		t.Fatalf("NewComponentReservation: %v", err)
	}
	return body
}
