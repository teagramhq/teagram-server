package erasureledger_test

import (
	"errors"
	"testing"

	"github.com/teagramhq/teagram-server/internal/erasureledger"
)

// futureKinds are kind numbers a newer binary would write and this binary has
// no payload type, validation, or replay meaning for. They stand in for the
// record a newer release adds: exactly the input MAIN-1436 requires a
// replayer to fail closed on, so it never skips an erasure it cannot read.
var futureKinds = []erasureledger.Kind{10, 11, 12, 100, 255, 65535}

// TestUnknownKindIsNotReady asserts a structurally sound record of an unknown
// kind reports ErrNotReady naming the kind, and hands back no record at all:
// not a zero payload, not a default body, not a partially read envelope.
func TestUnknownKindIsNotReady(t *testing.T) {
	t.Parallel()
	body := varintField(1, 42)
	for _, kind := range futureKinds {
		t.Run(kind.String(), func(t *testing.T) {
			got, err := erasureledger.Decode(frameOf(kind, body))
			if err == nil {
				t.Fatalf("Decode accepted an unknown kind: %#v", got)
			}
			if !errors.Is(err, erasureledger.ErrNotReady) {
				t.Fatalf("err = %v, want ErrNotReady", err)
			}
			if errors.Is(err, erasureledger.ErrRejected) {
				t.Fatalf("unknown kind reported as a rejection: %v", err)
			}
			info, ok := erasureledger.NotReady(err)
			if !ok {
				t.Fatalf("NotReady(err) = false for %v", err)
			}
			if info.Cause != erasureledger.CauseUnknownKind {
				t.Errorf("cause = %v, want an unknown kind", info.Cause)
			}
			if info.Kind != kind {
				t.Errorf("reported kind = %v, want %v", info.Kind, kind)
			}
			if got != (erasureledger.Record{}) {
				t.Errorf("Decode returned %#v alongside a not-ready error", got)
			}
		})
	}
}

// TestUnknownKindWithBrokenEnvelopeIsRejected keeps the two families honest:
// input that cannot be read at all is a rejection, so a corrupt frame is
// never dressed up as a newer binary's record, and a complete frame of an
// unknown kind is never dismissed as corruption.
func TestUnknownKindWithBrokenEnvelopeIsRejected(t *testing.T) {
	t.Parallel()
	// An unknown kind whose envelope is missing the operation key.
	data := frameBytes(1,
		varintField(1, uint64(futureKinds[0])),
		varintField(2, 1),
		bytesField(3, streamSlice(1)),
		varintField(4, 1),
		bytesField(6, varintField(1, 42)),
	)
	_, err := erasureledger.Decode(data)
	if !errors.Is(err, erasureledger.ErrRejected) {
		t.Fatalf("err = %v, want a rejection for the missing field", err)
	}
	if errors.Is(err, erasureledger.ErrNotReady) {
		t.Fatalf("incomplete frame reported as not-ready: %v", err)
	}
	info, ok := erasureledger.Rejection(err)
	if !ok {
		t.Fatalf("Rejection(err) = false for %v", err)
	}
	if info.Reason != erasureledger.ReasonMissingField {
		t.Errorf("reason = %q, want a missing field", info.Reason)
	}
}

// TestNewerCodecVersionIsNotReady covers the other way a newer binary outruns
// this one: the framing itself. No field in a frame with an unreadable version
// can be trusted, so the whole frame is not-ready.
func TestNewerCodecVersionIsNotReady(t *testing.T) {
	t.Parallel()
	body := varintField(1, 42)
	for _, version := range []uint16{erasureledger.CodecVersion + 1, 2, 3, 65535} {
		data := frameBytes(version, envelopeFields(erasureledger.KindAccount, body)...)
		got, err := erasureledger.Decode(data)
		if !errors.Is(err, erasureledger.ErrNotReady) {
			t.Fatalf("version %d: err = %v, want ErrNotReady", version, err)
		}
		info, ok := erasureledger.NotReady(err)
		if !ok {
			t.Fatalf("version %d: NotReady(err) = false", version)
		}
		if info.Cause != erasureledger.CauseCodecVersion {
			t.Errorf("version %d: cause = %v, want a codec version", version, info.Cause)
		}
		if info.CodecVersion != int(version) {
			t.Errorf("reported version = %d, want %d", info.CodecVersion, version)
		}
		if got != (erasureledger.Record{}) {
			t.Errorf("version %d: Decode returned %#v alongside a not-ready error", version, got)
		}
	}
}

// TestBatchWithUnknownKindYieldsNoRecords is the enumeration contract: a
// listing that contains one record this build cannot read
// yields no records at all. A partial replay is the failure MAIN-1360 and
// MAIN-1436 both forbid, so a caller cannot accidentally admit a subset.
func TestBatchWithUnknownKindYieldsNoRecords(t *testing.T) {
	t.Parallel()
	known, err := erasureledger.Encode(record(t, erasureledger.KindAccount, 1, 1, mustAccount(t)))
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	unknown := frameOf(futureKinds[0], varintField(1, 42))
	batch := frameBytes(1, bytesField(2, known), bytesField(2, unknown), bytesField(2, known))

	records, err := erasureledger.DecodeBatch(batch)
	if !errors.Is(err, erasureledger.ErrNotReady) {
		t.Fatalf("err = %v, want ErrNotReady", err)
	}
	if records != nil {
		t.Errorf("DecodeBatch returned %d records with a not-ready error", len(records))
	}
	info, ok := erasureledger.NotReady(err)
	if !ok || info.Kind != futureKinds[0] {
		t.Errorf("NotReady info = %+v, ok=%v, want the unknown kind %v", info, ok, futureKinds[0])
	}
}

// TestBatchOfKnownRecordsIsReadable is the control for the case above: the
// same batch with a readable middle record decodes in full, so the not-ready
// result is the unknown kind and not the framing.
func TestBatchOfKnownRecordsIsReadable(t *testing.T) {
	t.Parallel()
	known, err := erasureledger.Encode(record(t, erasureledger.KindAccount, 1, 1, mustAccount(t)))
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	batch := frameBytes(1, bytesField(2, known), bytesField(2, known), bytesField(2, known))
	records, err := erasureledger.DecodeBatch(batch)
	if err != nil {
		t.Fatalf("DecodeBatch: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("decoded %d records, want 3", len(records))
	}
}

// TestBatchRejectsUnknownFieldAndOversizeRecord checks the batch framing's own
// fail-closed rules: an unrecognised batch field, a record frame past
// the bound, and a truncated frame all come back with no records.
func TestBatchRejectsUnknownFieldAndOversizeRecord(t *testing.T) {
	t.Parallel()
	known, err := erasureledger.Encode(record(t, erasureledger.KindAccount, 1, 1, mustAccount(t)))
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	cases := []struct {
		name string
		data []byte
		seen func(error) bool
	}{
		{
			name: "unknown batch field",
			data: frameBytes(1, bytesField(2, known), varintField(9, 1)),
			seen: func(err error) bool {
				info, ok := erasureledger.Rejection(err)
				return ok && info.Reason == erasureledger.ReasonUnknownField
			},
		},
		{
			name: "reserved tag in a batch",
			data: frameBytes(1, bytesField(2, known), varintField(17, 1)),
			seen: func(err error) bool {
				info, ok := erasureledger.Rejection(err)
				return ok && info.Reason == erasureledger.ReasonReservedField
			},
		},
		{
			name: "record frame past the bound",
			data: frameBytes(1, bytesField(2, append(known, make([]byte, erasureledger.MaxRecordBytes)...))),
			seen: func(err error) bool { return errors.Is(err, erasureledger.ErrRejected) },
		},
		{
			name: "truncated record frame",
			data: frameBytes(1, bytesField(2, known[:len(known)-2])),
			seen: func(err error) bool { return errors.Is(err, erasureledger.ErrRejected) },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			records, err := erasureledger.DecodeBatch(tc.data)
			if err == nil {
				t.Fatalf("DecodeBatch accepted %s", tc.name)
			}
			if records != nil {
				t.Errorf("DecodeBatch returned %d records with an error", len(records))
			}
			if !tc.seen(err) {
				t.Errorf("unexpected failure for %s: %v", tc.name, err)
			}
		})
	}
}

// TestNotReadyIsConsumableWithoutGuessing is the admission contract: a caller
// branches on a typed cause and never parses an error string, and the two
// families are mutually exclusive.
func TestNotReadyIsConsumableWithoutGuessing(t *testing.T) {
	t.Parallel()
	notReady, err := erasureledger.Decode(frameOf(futureKinds[3], varintField(1, 42)))
	if err == nil {
		t.Fatalf("Decode accepted an unknown kind: %#v", notReady)
	}
	if _, ok := erasureledger.NotReady(err); !ok {
		t.Fatalf("NotReady(err) = false for %v", err)
	}
	if _, ok := erasureledger.Rejection(err); ok {
		t.Errorf("Rejection(err) = true for a not-ready failure: %v", err)
	}

	rejected, err := erasureledger.Decode([]byte("not a ledger frame at all"))
	if err == nil {
		t.Fatalf("Decode accepted junk: %#v", rejected)
	}
	if _, ok := erasureledger.Rejection(err); !ok {
		t.Fatalf("Rejection(err) = false for %v", err)
	}
	if _, ok := erasureledger.NotReady(err); ok {
		t.Errorf("NotReady(err) = true for a rejection: %v", err)
	}
}

// TestKindStringNamesTheVocabulary keeps error text informative:
// an unknown kind is reported as a number, a known kind by its payload type,
// so a log line says which side of the vocabulary the failure is on.
func TestKindStringNamesTheVocabulary(t *testing.T) {
	t.Parallel()
	for _, spec := range erasureledger.KindSpecs() {
		if got := spec.Kind.String(); got != spec.Name() {
			t.Errorf("known kind prints %q, want %q", got, spec.Name())
		}
	}
	unknown := erasureledger.Kind(4242)
	if got := unknown.String(); got == erasureledger.KindAccount.String() {
		t.Errorf("unknown kind prints as a known one: %q", got)
	}
}
