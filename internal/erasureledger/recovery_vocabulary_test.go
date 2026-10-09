package erasureledger_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/teagramhq/teagram-server/internal/erasureledger"
)

// These raw frames pin the new wire forms. A reader must preserve their
// ownership and component inputs exactly rather than normalizing away a field.
func TestRecoveryReservationRecordsRoundTripCanonically(t *testing.T) {
	t.Parallel()

	const (
		kindStreamBinding        erasureledger.Kind = 10
		kindComponentReservation erasureledger.Kind = 11
	)
	cases := []struct {
		name string
		kind erasureledger.Kind
		body []byte
	}{
		{
			name: "stream binding",
			kind: kindStreamBinding,
			body: bytesField(1, lineageSlice(0x70)),
		},
		{
			name: "component reservation with restored baseline",
			kind: kindComponentReservation,
			body: join(
				bytesField(1, []byte("update_state_pts")),
				varintField(2, 10),
				varintField(3, 14),
			),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := frameOf(tc.kind, tc.body)
			decoded, err := erasureledger.Decode(input)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			encoded, err := erasureledger.Encode(decoded)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			if !bytes.Equal(encoded, input) {
				t.Fatalf("canonical round trip changed bytes:\n got %x\nwant %x", encoded, input)
			}
		})
	}
}

func TestComponentReservationEncodesZeroBaselineExplicitly(t *testing.T) {
	t.Parallel()
	allocator := mustAllocator(t, "update_state_pts")
	component, err := erasureledger.NewComponentReservation(allocator, 0, 4)
	if err != nil {
		t.Fatalf("NewComponentReservation: %v", err)
	}
	rec, err := erasureledger.NewRecord(erasureledger.KindComponentReservation, 5, streamID(0x50), 2, opKey(0x10), component)
	if err != nil {
		t.Fatalf("NewRecord: %v", err)
	}
	data, err := erasureledger.Encode(rec)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	body := join(
		bytesField(1, []byte("update_state_pts")),
		varintField(2, 0),
		varintField(3, 4),
	)
	want := frameBytes(1,
		varintField(1, uint64(erasureledger.KindComponentReservation)),
		varintField(2, 5),
		bytesField(3, streamSlice(0x50)),
		varintField(4, 2),
		bytesField(5, opKeySlice(0x10)),
		bytesField(6, body),
	)
	if !bytes.Equal(data, want) {
		t.Fatalf("zero baseline encoding = %x, want %x", data, want)
	}
}

func TestComponentReservationUnknownAllocatorIsNotReady(t *testing.T) {
	t.Parallel()
	input := frameOf(erasureledger.KindComponentReservation, join(
		bytesField(1, []byte("unclassified_allocator")),
		varintField(2, 10),
		varintField(3, 14),
	))
	if _, err := erasureledger.Decode(input); !errors.Is(err, erasureledger.ErrNotReady) {
		t.Fatalf("Decode with an unclassified allocator = %v, want ErrNotReady", err)
	} else if info, ok := erasureledger.NotReady(err); !ok || info.Cause != erasureledger.CauseUnknownAllocator || info.Allocator != "unclassified_allocator" {
		t.Errorf("NotReady info = %+v, ok=%v", info, ok)
	}
}

// The scalar reservation form is sufficient for an absolute sequence ceiling,
// but it cannot prove per-stream component-sum capacity or an unknown name.
func TestScalarReservationRejectsComponentSumAndUnknownAllocators(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"update_state_pts", "unclassified_allocator"} {
		t.Run(name, func(t *testing.T) {
			allocator, err := erasureledger.ValidateAllocator(name)
			if err != nil {
				t.Fatalf("ValidateAllocator(%q): %v", name, err)
			}
			_, err = erasureledger.NewReservation(allocator, 14)
			if !errors.Is(err, erasureledger.ErrNotReady) {
				t.Fatalf("NewReservation(%q) succeeded without an absolute-max classification", name)
			}
			info, ok := erasureledger.NotReady(err)
			if !ok || info.Allocator != name {
				t.Errorf("NotReady info = %+v, ok=%v, want allocator %q", info, ok, name)
			}
		})
	}
}

func TestAllocatorReservationClassificationIsClosed(t *testing.T) {
	t.Parallel()

	absolute := []string{
		"users_id_seq", "files_id_seq", "chats_id_seq", "secret_chats_id_seq",
		"message_fanout_seq", "chat_admin_events_id_seq", "phone_codes_id_seq",
		"registration_invites_id_seq", "language_catalog_publication_audit_id_seq",
	}
	component := []string{
		"update_state_next_local_id", "update_state_pts",
		"channel_state_next_local_id", "channel_state_pts",
		"profile_photo_state_mutation_revision",
	}
	for _, name := range absolute {
		allocator, err := erasureledger.ValidateAllocator(name)
		if err != nil {
			t.Fatalf("ValidateAllocator(%q): %v", name, err)
		}
		got, ok := erasureledger.ClassifyAllocator(allocator)
		if !ok || got != erasureledger.AllocatorAbsoluteMax {
			t.Errorf("ClassifyAllocator(%q) = %v, %v, want absolute-max", name, got, ok)
		}
		if _, err := erasureledger.NewReservation(allocator, 14); err != nil {
			t.Errorf("NewReservation(%q): %v", name, err)
		}
	}
	for _, name := range component {
		allocator, err := erasureledger.ValidateAllocator(name)
		if err != nil {
			t.Fatalf("ValidateAllocator(%q): %v", name, err)
		}
		got, ok := erasureledger.ClassifyAllocator(allocator)
		if !ok || got != erasureledger.AllocatorComponentSum {
			t.Errorf("ClassifyAllocator(%q) = %v, %v, want component-sum", name, got, ok)
		}
		if _, err := erasureledger.NewComponentReservation(allocator, 10, 14); err != nil {
			t.Errorf("NewComponentReservation(%q): %v", name, err)
		}
	}
	for _, name := range []string{"channels_id_seq", "polls_id", "unclassified_allocator"} {
		allocator, err := erasureledger.ValidateAllocator(name)
		if err != nil {
			t.Fatalf("ValidateAllocator(%q): %v", name, err)
		}
		if _, ok := erasureledger.ClassifyAllocator(allocator); ok {
			t.Errorf("ClassifyAllocator(%q) accepted a random or unknown allocator", name)
		}
		if _, err := erasureledger.NewReservation(allocator, 14); !errors.Is(err, erasureledger.ErrNotReady) {
			t.Errorf("NewReservation(%q) = %v, want fail-closed ErrNotReady", name, err)
		}
		if _, err := erasureledger.NewComponentReservation(allocator, 10, 14); !errors.Is(err, erasureledger.ErrNotReady) {
			t.Errorf("NewComponentReservation(%q) = %v, want fail-closed ErrNotReady", name, err)
		}
	}
}

func TestStreamBindingsKeepSharedEpochsAndComponentsDistinct(t *testing.T) {
	t.Parallel()

	const epoch = int64(5)
	lineage1, lineage2 := lineageID(0x20), lineageID(0x40)
	streamA, streamB := streamID(0x50), streamID(0x60)
	bindings := erasureledger.NewStreamBindings()
	for _, tc := range []struct {
		stream  erasureledger.StreamID
		lineage erasureledger.LineageID
	}{
		{streamA, lineage1},
		{streamB, lineage2},
	} {
		rec := mustBindingRecord(t, epoch, tc.stream, 1, tc.lineage, tc.lineage[0])
		data, err := erasureledger.Encode(rec)
		if err != nil {
			t.Fatalf("Encode binding: %v", err)
		}
		decoded, err := erasureledger.Decode(data)
		if err != nil {
			t.Fatalf("Decode binding: %v", err)
		}
		if err := bindings.AddConfirmed(decoded); err != nil {
			t.Fatalf("AddConfirmed: %v", err)
		}
	}

	allocator := mustAllocator(t, "update_state_pts")
	components := []struct {
		stream  erasureledger.StreamID
		lineage erasureledger.LineageID
		base    int64
		ceiling int64
	}{
		{streamA, lineage1, 10, 14},
		{streamB, lineage2, 7, 12},
	}
	keys := make(map[erasureledger.ComponentKey]erasureledger.ComponentReservation)
	for i, tc := range components {
		payload, err := erasureledger.NewComponentReservation(allocator, tc.base, tc.ceiling)
		if err != nil {
			t.Fatalf("NewComponentReservation: %v", err)
		}
		rec, err := erasureledger.NewRecord(erasureledger.KindComponentReservation, epoch, tc.stream, 2, opKey(byte(0x70+i)), payload)
		if err != nil {
			t.Fatalf("NewRecord component: %v", err)
		}
		data, err := erasureledger.Encode(rec)
		if err != nil {
			t.Fatalf("Encode component: %v", err)
		}
		decoded, err := erasureledger.Decode(data)
		if err != nil {
			t.Fatalf("Decode component: %v", err)
		}
		key, err := bindings.ComponentKeyFor(decoded)
		if err != nil {
			t.Fatalf("ComponentKeyFor: %v", err)
		}
		if key.Lineage != tc.lineage || key.Stream != tc.stream || key.Allocator != allocator {
			t.Errorf("component key = %+v, want lineage %x stream %x allocator %s", key, tc.lineage, tc.stream, allocator)
		}
		keys[key] = payloadAs[erasureledger.ComponentReservation](t, decoded.Payload)
	}
	if len(keys) != 2 {
		t.Fatalf("same-name components collapsed to %d key(s), want two", len(keys))
	}

	completion := func(stream erasureledger.StreamID, lineage erasureledger.LineageID) erasureledger.Record {
		t.Helper()
		body, err := erasureledger.NewEpoch(epoch, lineage, erasureledger.EpochCompleted)
		if err != nil {
			t.Fatalf("NewEpoch: %v", err)
		}
		rec, err := erasureledger.NewRecord(erasureledger.KindEpoch, epoch, stream, 3, opKey(0x30), body)
		if err != nil {
			t.Fatalf("NewRecord epoch: %v", err)
		}
		return rec
	}
	if got, err := bindings.LineageFor(completion(streamA, lineage1)); err != nil || got != lineage1 {
		t.Errorf("L1 completion attribution = %x, %v; want L1", got, err)
	}
	if got, err := bindings.LineageFor(completion(streamB, lineage2)); err != nil || got != lineage2 {
		t.Errorf("L2 completion attribution = %x, %v; want L2", got, err)
	}
	if _, err := bindings.LineageFor(completion(streamA, lineage2)); !errors.Is(err, erasureledger.ErrNotReady) {
		t.Errorf("L2 completion on L1's bound stream = %v, want refusal", err)
	}
	if keysFound := len(keys); keysFound != 2 {
		t.Errorf("historical L1 component evidence was lost while attributing L2: got %d keys", keysFound)
	}
}

func TestStreamBindingConflictAndMissingOwnershipRefuseReadiness(t *testing.T) {
	t.Parallel()

	stream := streamID(0x50)
	lineage1, lineage2 := lineageID(0x20), lineageID(0x40)
	allocator := mustAllocator(t, "channel_state_pts")
	component, err := erasureledger.NewComponentReservation(allocator, 0, 4)
	if err != nil {
		t.Fatalf("NewComponentReservation: %v", err)
	}
	record, err := erasureledger.NewRecord(erasureledger.KindComponentReservation, 5, stream, 2, opKey(0x10), component)
	if err != nil {
		t.Fatalf("NewRecord component: %v", err)
	}
	bindings := erasureledger.NewStreamBindings()
	_, err = bindings.LineageFor(record)
	if !errors.Is(err, erasureledger.ErrNotReady) {
		t.Fatalf("unbound stream attribution = %v, want ErrNotReady", err)
	}
	if info, ok := erasureledger.NotReady(err); !ok || info.Cause != erasureledger.CauseMissingBinding {
		t.Errorf("unbound stream error detail = %+v, ok=%v", info, ok)
	}
	if err := bindings.AddConfirmed(mustBindingRecord(t, 5, stream, 1, lineage1, 0x20)); err != nil {
		t.Fatalf("AddConfirmed first lineage: %v", err)
	}
	if err := bindings.AddConfirmed(mustBindingRecord(t, 5, stream, 2, lineage1, 0x21)); err != nil {
		t.Errorf("repeated identical binding = %v, want idempotent success", err)
	}
	err = bindings.AddConfirmed(mustBindingRecord(t, 5, stream, 3, lineage2, 0x40))
	if !errors.Is(err, erasureledger.ErrNotReady) {
		t.Fatalf("conflicting binding = %v, want refusal", err)
	}
	if info, ok := erasureledger.NotReady(err); !ok || info.Cause != erasureledger.CauseBindingConflict {
		t.Errorf("conflicting binding detail = %+v, ok=%v", info, ok)
	}
	if got, err := bindings.LineageFor(record); err != nil || got != lineage1 {
		t.Errorf("first accepted lineage = %x, %v; a conflict must not replace it", got, err)
	}

	ordered := erasureledger.NewStreamBindings()
	if err := ordered.AddConfirmed(mustBindingRecord(t, 5, streamID(0x60), 2, lineage2, 0x41)); !errors.Is(err, erasureledger.ErrNotReady) {
		t.Errorf("first binding at sequence 2 = %v, want refusal", err)
	}
}

func TestFreshAndInheritedComponentBaselines(t *testing.T) {
	t.Parallel()
	key, err := erasureledger.NewComponentKey(lineageID(0x20), streamID(0x50), mustAllocator(t, "profile_photo_state_mutation_revision"))
	if err != nil {
		t.Fatalf("NewComponentKey: %v", err)
	}
	fresh, err := erasureledger.NewComponentBaseline(key, 0, erasureledger.ComponentFresh)
	if err != nil {
		t.Fatalf("NewComponentBaseline fresh: %v", err)
	}
	if !fresh.CanReserve() {
		t.Error("fresh zero-baseline component is closed")
	}
	if _, err := erasureledger.NewComponentBaseline(key, 1, erasureledger.ComponentFresh); !errors.Is(err, erasureledger.ErrRejected) {
		t.Errorf("fresh component with nonzero baseline = %v, want rejection", err)
	}
	inherited, err := erasureledger.NewComponentBaseline(key, 30, erasureledger.ComponentInherited)
	if err != nil {
		t.Fatalf("NewComponentBaseline inherited: %v", err)
	}
	if inherited.CanReserve() {
		t.Error("inherited component is open to a new reservation")
	}
	if _, err := erasureledger.NewComponentBaseline(key, 0, erasureledger.ComponentOrigin(0)); !errors.Is(err, erasureledger.ErrRejected) {
		t.Errorf("unknown component origin = %v, want rejection", err)
	}
}

func mustBindingRecord(t *testing.T, epoch int64, stream erasureledger.StreamID, seq int64, lineage erasureledger.LineageID, keySeed byte) erasureledger.Record {
	t.Helper()
	binding, err := erasureledger.NewStreamBinding(lineage)
	if err != nil {
		t.Fatalf("NewStreamBinding: %v", err)
	}
	record, err := erasureledger.NewRecord(erasureledger.KindStreamBinding, epoch, stream, seq, opKey(keySeed), binding)
	if err != nil {
		t.Fatalf("NewRecord binding: %v", err)
	}
	return record
}
