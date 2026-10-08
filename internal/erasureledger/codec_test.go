package erasureledger_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"reflect"
	"testing"

	"github.com/teagramhq/teagram-server/internal/erasureledger"
)

// opKey and streamID are fixed test identities. They are opaque values, so a
// fixed one is as good as a drawn one for every assertion here.
func opKey(seed byte) erasureledger.OperationKey {
	var k erasureledger.OperationKey
	for i := range k {
		k[i] = seed + byte(i)
	}
	return k
}

func streamID(seed byte) erasureledger.StreamID {
	var s erasureledger.StreamID
	for i := range s {
		s[i] = seed + byte(i)
	}
	return s
}

func lineageID(seed byte) erasureledger.LineageID {
	var l erasureledger.LineageID
	for i := range l {
		l[i] = seed + byte(i)
	}
	return l
}

// record builds a valid record of the given kind and payload.
// streamSlice, opKeySlice and lineageSlice hand out the byte form of an
// opaque identity, which is what a hand-built frame needs.
func streamSlice(seed byte) []byte  { s := streamID(seed); return s[:] }
func opKeySlice(seed byte) []byte   { k := opKey(seed); return k[:] }
func lineageSlice(seed byte) []byte { l := lineageID(seed); return l[:] }

func record(t *testing.T, kind erasureledger.Kind, epoch, seq int64, payload erasureledger.Payload) erasureledger.Record {
	t.Helper()
	rec, err := erasureledger.NewRecord(kind, epoch, streamID(0x50), seq, opKey(0x10), payload)
	if err != nil {
		t.Fatalf("NewRecord kind=%v: %v", kind, err)
	}
	return rec
}

// roundTrips is one canonical payload per accepted kind, which is the
// vocabulary's round-trip floor: each kind must survive encode and decode
// with identical identifiers and an identical scope.
func roundTrips(t *testing.T) []struct {
	kind    erasureledger.Kind
	payload erasureledger.Payload
} {
	t.Helper()
	account, err := erasureledger.NewAccount(42)
	if err != nil {
		t.Fatalf("NewAccount: %v", err)
	}
	file, err := erasureledger.NewFile(9007199254740993)
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	copies, err := erasureledger.NewMessageCopies(
		erasureledger.MessageCopy{OwnerID: 1, LocalID: 7},
		erasureledger.MessageCopy{OwnerID: 2, LocalID: 7},
		erasureledger.MessageCopy{OwnerID: 2, LocalID: 8},
	)
	if err != nil {
		t.Fatalf("NewMessageCopies: %v", err)
	}
	posts, err := erasureledger.NewChannelPostCopies(
		erasureledger.ChannelPost{ChannelID: 3000000001, LocalID: 1},
		erasureledger.ChannelPost{ChannelID: 3000000001, LocalID: 41},
	)
	if err != nil {
		t.Fatalf("NewChannelPostCopies: %v", err)
	}
	excl, err := erasureledger.NewRandomExclusion(erasureledger.RandomClassPoll, 11, 22, 997852516352)
	if err != nil {
		t.Fatalf("NewRandomExclusion: %v", err)
	}
	resv, err := erasureledger.NewReservation(mustAllocator(t, "users_id_seq"), 1000000)
	if err != nil {
		t.Fatalf("NewReservation: %v", err)
	}
	epoch, err := erasureledger.NewEpoch(3, lineageID(0x70), erasureledger.EpochCompleted)
	if err != nil {
		t.Fatalf("NewEpoch: %v", err)
	}
	gallery, err := erasureledger.NewGalleryDelete(5, 9,
		erasureledger.GalleryEntry{FileID: 203, ClientFileID: 88},
		erasureledger.GalleryEntry{FileID: 204, ClientFileID: 89},
	)
	if err != nil {
		t.Fatalf("NewGalleryDelete: %v", err)
	}
	receipt, err := erasureledger.NewReceiptTerminal(5, 88, erasureledger.ReceiptDeleted, 203)
	if err != nil {
		t.Fatalf("NewReceiptTerminal: %v", err)
	}
	return []struct {
		kind    erasureledger.Kind
		payload erasureledger.Payload
	}{
		{erasureledger.KindAccount, account},
		{erasureledger.KindFile, file},
		{erasureledger.KindMessageCopies, copies},
		{erasureledger.KindChannelPosts, posts},
		{erasureledger.KindRandomExclusion, excl},
		{erasureledger.KindReservation, resv},
		{erasureledger.KindEpoch, epoch},
		{erasureledger.KindGalleryDelete, gallery},
		{erasureledger.KindReceiptTerminal, receipt},
	}
}

func mustAllocator(t *testing.T, name string) erasureledger.AllocatorName {
	t.Helper()
	a, err := erasureledger.ValidateAllocator(name)
	if err != nil {
		t.Fatalf("ValidateAllocator(%q): %v", name, err)
	}
	return a
}

// TestCodecRoundTripEveryKind encodes and decodes one canonical record per
// accepted kind and requires the decoded record to be identical, so no
// kind widens an identifier or drifts a scope on the way through the wire.
func TestCodecRoundTripEveryKind(t *testing.T) {
	for _, tc := range roundTrips(t) {
		t.Run(tc.kind.String(), func(t *testing.T) {
			in := record(t, tc.kind, 1, 1, tc.payload)
			data, err := erasureledger.Encode(in)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			out, err := erasureledger.Decode(data)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if out.Kind != in.Kind {
				t.Errorf("kind = %v, want %v", out.Kind, in.Kind)
			}
			if out.Epoch != in.Epoch || out.Seq != in.Seq {
				t.Errorf("ordering = (%d,%d), want (%d,%d)", out.Epoch, out.Seq, in.Epoch, in.Seq)
			}
			if out.Stream != in.Stream {
				t.Errorf("stream = %v, want %v", out.Stream, in.Stream)
			}
			if out.OpKey != in.OpKey {
				t.Errorf("op key = %v, want %v", out.OpKey, in.OpKey)
			}
			if !reflect.DeepEqual(out.Payload, in.Payload) {
				t.Errorf("payload = %#v, want %#v", out.Payload, in.Payload)
			}
			// Canonical form is a fixed point: re-encoding the decoded
			// record reproduces the same bytes.
			again, err := erasureledger.Encode(out)
			if err != nil {
				t.Fatalf("Encode(decoded): %v", err)
			}
			if !bytes.Equal(again, data) {
				t.Errorf("re-encode is not byte-stable:\n got %x\nwant %x", again, data)
			}
		})
	}
}

// TestCodecCoversEveryKind asserts the round-trip table is the whole
// vocabulary, so a kind added to the registry cannot arrive without coverage.
func TestCodecCoversEveryKind(t *testing.T) {
	seen := map[erasureledger.Kind]bool{}
	for _, tc := range roundTrips(t) {
		seen[tc.kind] = true
	}
	for _, spec := range erasureledger.KindSpecs() {
		if !seen[spec.Kind] {
			t.Errorf("kind %v has no round-trip case", spec.Kind)
		}
	}
	if got, want := len(seen), len(erasureledger.KindSpecs()); got != want {
		t.Errorf("round-trip table has %d kinds, registry has %d", got, want)
	}
}

// TestEncodeDeterministic asserts the encoder is deterministic, which is what
// lets a future replay test compare records by bytes.
func TestEncodeDeterministic(t *testing.T) {
	for _, tc := range roundTrips(t) {
		in := record(t, tc.kind, 2, 9, tc.payload)
		first, err := erasureledger.Encode(in)
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		for range 8 {
			next, err := erasureledger.Encode(in)
			if err != nil {
				t.Fatalf("Encode repeat: %v", err)
			}
			if !bytes.Equal(first, next) {
				t.Fatalf("kind %v encodes non-deterministically", tc.kind)
			}
		}
	}
}

// TestBatchRoundTrip encodes a batch of one record per kind and reads it
// back in order, which is the shape a paginated enumeration hands a reader.
func TestBatchRoundTrip(t *testing.T) {
	rt := roundTrips(t)
	records := make([]erasureledger.Record, 0, len(rt))
	for i, tc := range rt {
		records = append(records, record(t, tc.kind, int64(i+1), int64(i+1), tc.payload))
	}
	data, err := erasureledger.EncodeBatch(records)
	if err != nil {
		t.Fatalf("EncodeBatch: %v", err)
	}
	if len(data) > erasureledger.MaxBatchBytes {
		t.Fatalf("batch of %d records is %d bytes, over the bound", len(records), len(data))
	}
	out, err := erasureledger.DecodeBatch(data)
	if err != nil {
		t.Fatalf("DecodeBatch: %v", err)
	}
	if len(out) != len(records) {
		t.Fatalf("decoded %d records, want %d", len(out), len(records))
	}
	for i := range records {
		if !reflect.DeepEqual(out[i], records[i]) {
			t.Errorf("record %d = %#v, want %#v", i, out[i], records[i])
		}
	}
}

// TestBatchEmptyIsValidOnItsOwn encodes the empty batch, which a reader must
// be able to distinguish from "unreadable": a complete enumeration with no
// records is an answer, not a parse failure.
func TestBatchEmptyIsValidOnItsOwn(t *testing.T) {
	data, err := erasureledger.EncodeBatch(nil)
	if err != nil {
		t.Fatalf("EncodeBatch(nil): %v", err)
	}
	out, err := erasureledger.DecodeBatch(data)
	if err != nil {
		t.Fatalf("DecodeBatch: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("decoded %d records, want 0", len(out))
	}
}

// frameBytes composes a frame from raw fields, so the tests can hand a
// decoder exactly the malformed input a hostile or buggy writer would emit.
func frameBytes(version uint16, fields ...[]byte) []byte {
	out := binary.BigEndian.AppendUint16([]byte("TGEL"), version)
	for _, f := range fields {
		out = append(out, f...)
	}
	return out
}

func key(field, wire uint64) []byte {
	return binary.AppendUvarint(nil, field<<3|wire)
}

func varintField(field uint64, v uint64) []byte {
	return append(key(field, 0), binary.AppendUvarint(nil, v)...)
}

func bytesField(field uint64, v []byte) []byte {
	out := append(key(field, 2), binary.AppendUvarint(nil, uint64(len(v)))...)
	return append(out, v...)
}

// envelopeFields is the field sequence a valid record's envelope carries,
// with the caller's body.
func envelopeFields(kind erasureledger.Kind, body []byte) [][]byte {
	return [][]byte{
		varintField(1, uint64(kind)),
		varintField(2, 1),
		bytesField(3, streamSlice(0x50)),
		varintField(4, 1),
		bytesField(5, opKeySlice(0x10)),
		bytesField(6, body),
	}
}

// TestDecodeRejectsMalformedInput is the fail-closed table: every one of
// these inputs must come back as ErrRejected, and must come back with no
// record attached.
func TestDecodeRejectsMalformedInput(t *testing.T) {
	valid, err := erasureledger.Encode(record(t, erasureledger.KindAccount, 1, 1, mustAccount(t)))
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	// accountBody is the one-field body of a KindAccount record: user id 42.
	accountBody := varintField(1, 42)
	body := accountBody

	cases := []struct {
		name  string
		data  []byte
		cause erasureledger.Reason
	}{
		{
			name:  "bad magic",
			data:  append([]byte("XXXX"), valid[len("TGEL"):]...),
			cause: "frame is not an erasure ledger record",
		},
		{
			name:  "truncated frame",
			data:  valid[:len(valid)-3],
			cause: erasureledger.ReasonTruncated,
		},
		{
			name:  "trailing byte",
			data:  append(append([]byte{}, valid...), 0x00),
			cause: erasureledger.ReasonMalformed,
		},
		{
			name:  "zero codec version",
			data:  frameBytes(0, varintField(1, 1), varintField(2, 1), bytesField(3, streamSlice(1)), varintField(4, 1), bytesField(5, opKeySlice(1)), bytesField(6, nil)),
			cause: erasureledger.ReasonOutOfRange,
		},
		{
			name:  "missing kind",
			data:  frameBytes(1, envelopeFields(0, body)[1:]...),
			cause: erasureledger.ReasonMissingField,
		},
		{
			name:  "missing stream",
			data:  frameBytes(1, varintField(1, 1), varintField(2, 1), varintField(4, 1), bytesField(5, opKeySlice(1)), bytesField(6, body)),
			cause: erasureledger.ReasonMissingField,
		},
		{
			name:  "missing payload",
			data:  frameBytes(1, varintField(1, 1), varintField(2, 1), bytesField(3, streamSlice(1)), varintField(4, 1), bytesField(5, opKeySlice(1))),
			cause: erasureledger.ReasonMissingField,
		},
		{
			name:  "duplicate epoch",
			data:  frameBytes(1, varintField(1, 1), varintField(2, 1), varintField(2, 2), bytesField(3, streamSlice(1)), varintField(4, 1), bytesField(5, opKeySlice(1)), bytesField(6, body)),
			cause: erasureledger.ReasonDuplicateField,
		},
		{
			name:  "unknown envelope field",
			data:  frameBytes(1, append(envelopeFields(erasureledger.KindAccount, body), varintField(40, 7))...),
			cause: erasureledger.ReasonUnknownField,
		},
		{
			name:  "fields out of order",
			data:  frameBytes(1, varintField(2, 1), varintField(1, 1), bytesField(3, streamSlice(1)), varintField(4, 1), bytesField(5, opKeySlice(1)), bytesField(6, body)),
			cause: erasureledger.ReasonFieldOrder,
		},
		{
			name:  "stream as varint",
			data:  frameBytes(1, varintField(1, 1), varintField(2, 1), varintField(3, 7), varintField(4, 1), bytesField(5, opKeySlice(1)), bytesField(6, body)),
			cause: erasureledger.ReasonWireType,
		},
		{
			name:  "stream too short",
			data:  frameBytes(1, varintField(1, 1), varintField(2, 1), bytesField(3, streamSlice(1)[:9]), varintField(4, 1), bytesField(5, opKeySlice(1)), bytesField(6, body)),
			cause: erasureledger.ReasonOutOfRange,
		},
		{
			name:  "operation key too long",
			data:  frameBytes(1, varintField(1, 1), varintField(2, 1), bytesField(3, streamSlice(1)), varintField(4, 1), bytesField(5, append(opKeySlice(1), 0)), bytesField(6, body)),
			cause: erasureledger.ReasonTooLarge,
		},
		{
			name:  "non-canonical varint",
			data:  frameBytes(1, varintField(1, 1), append(key(2, 0), 0x80, 0x00), bytesField(3, streamSlice(1)), varintField(4, 1), bytesField(5, opKeySlice(1)), bytesField(6, body)),
			cause: erasureledger.ReasonNotCanonical,
		},
		{
			name:  "kind zero",
			data:  frameBytes(1, varintField(1, 0), varintField(2, 1), bytesField(3, streamSlice(1)), varintField(4, 1), bytesField(5, opKeySlice(1)), bytesField(6, body)),
			cause: erasureledger.ReasonOutOfRange,
		},
		{
			name:  "declared length past the frame",
			data:  frameBytes(1, append(key(6, 2), 0xff, 0xff, 0x7f)),
			cause: erasureledger.ReasonTooLarge,
		},
		{
			name:  "epoch zero",
			data:  frameBytes(1, varintField(1, 1), varintField(2, 0), bytesField(3, streamSlice(1)), varintField(4, 1), bytesField(5, opKeySlice(1)), bytesField(6, body)),
			cause: erasureledger.ReasonOutOfRange,
		},
		{
			name:  "sequence zero",
			data:  frameBytes(1, varintField(1, 1), varintField(2, 1), bytesField(3, streamSlice(1)), varintField(4, 0), bytesField(5, opKeySlice(1)), bytesField(6, body)),
			cause: erasureledger.ReasonOutOfRange,
		},
		{
			name:  "all-zero operation key",
			data:  frameBytes(1, varintField(1, 1), varintField(2, 1), bytesField(3, streamSlice(1)), varintField(4, 1), bytesField(5, make([]byte, 16)), bytesField(6, body)),
			cause: erasureledger.ReasonAllZero,
		},
		{
			name:  "account body with a second field",
			data:  frameOf(erasureledger.KindAccount, append(accountBody, varintField(9, 1)...)),
			cause: erasureledger.ReasonUnknownField,
		},
		{
			name:  "account body missing its id",
			data:  frameOf(erasureledger.KindAccount, nil),
			cause: erasureledger.ReasonOutOfRange,
		},
		{
			name:  "message copy set empty",
			data:  frameOf(erasureledger.KindMessageCopies, nil),
			cause: erasureledger.ReasonNotCanonical,
		},
		{
			name:  "message copy local id past the wire width",
			data:  frameOf(erasureledger.KindMessageCopies, bytesField(1, append(varintField(1, 1), varintField(2, uint64(1)<<31)...))),
			cause: erasureledger.ReasonOutOfRange,
		},
		{
			name:  "message copies not canonical",
			data:  frameOf(erasureledger.KindMessageCopies, join(copyBytes(2, 7), copyBytes(1, 7))),
			cause: erasureledger.ReasonNotCanonical,
		},
		{
			name:  "message copies duplicated",
			data:  frameOf(erasureledger.KindMessageCopies, join(copyBytes(1, 7), copyBytes(1, 7))),
			cause: erasureledger.ReasonDuplicate,
		},
		{
			name:  "receipt state pending",
			data:  frameOf(erasureledger.KindReceiptTerminal, join(varintField(1, 5), varintField(2, 88), varintField(3, 0))),
			cause: "receipt state is not terminal",
		},
		{
			name:  "receipt carries a byte-valued field",
			data:  frameOf(erasureledger.KindReceiptTerminal, join(varintField(1, 5), varintField(2, 88), varintField(3, 2), bytesField(4, []byte("blob-key-bytes")))),
			cause: erasureledger.ReasonBytesForbidden,
		},
		{
			name:  "epoch level unknown",
			data:  frameOf(erasureledger.KindEpoch, join(varintField(1, 3), bytesField(2, lineageSlice(1)), varintField(3, 9))),
			cause: "epoch level is unknown",
		},
		{
			name:  "epoch lineage all zero",
			data:  frameOf(erasureledger.KindEpoch, join(varintField(1, 3), bytesField(2, make([]byte, 16)), varintField(3, 1))),
			cause: erasureledger.ReasonAllZero,
		},
		{
			name:  "reservation allocator is not a schema identifier",
			data:  frameOf(erasureledger.KindReservation, append(bytesField(1, []byte("Users Id Seq")), varintField(2, 100)...)),
			cause: "allocator name is not a schema identifier",
		},
		{
			name:  "random exclusion class unknown",
			data:  frameOf(erasureledger.KindRandomExclusion, append(varintField(1, 9), varintField(2, 5)...)),
			cause: "random allocator class is unknown",
		},
		{
			name:  "gallery revision negative",
			data:  frameOf(erasureledger.KindGalleryDelete, join(varintField(1, 5), bytesField(2, join(varintField(1, 203), varintField(2, 88))), varintField(3, ^uint64(0)))),
			cause: erasureledger.ReasonOutOfRange,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := erasureledger.Decode(tc.data)
			if err == nil {
				t.Fatalf("Decode accepted malformed input: %#v", got)
			}
			if !errors.Is(err, erasureledger.ErrRejected) {
				t.Fatalf("err = %v, want ErrRejected", err)
			}
			if errors.Is(err, erasureledger.ErrNotReady) {
				t.Fatalf("malformed input reported as not-ready: %v", err)
			}
			if got != (erasureledger.Record{}) {
				t.Errorf("Decode returned a record with an error: %#v", got)
			}
			info, ok := erasureledger.Rejection(err)
			if !ok {
				t.Fatalf("Rejection(err) = false for %v", err)
			}
			if tc.cause != "" && info.Reason != tc.cause {
				t.Errorf("reason = %q, want %q (field %q)", info.Reason, tc.cause, info.Field)
			}
		})
	}
}

func mustAccount(t *testing.T) erasureledger.Account {
	t.Helper()
	a, err := erasureledger.NewAccount(42)
	if err != nil {
		t.Fatalf("NewAccount: %v", err)
	}
	return a
}

func copyBytes(owner, local uint64) []byte {
	return bytesField(1, join(varintField(1, owner), varintField(2, local)))
}

// payloadAs asserts a decoded payload's type, so a test that reads a body
// cannot silently act on the wrong kind's fields.
func payloadAs[T erasureledger.Payload](t *testing.T, p erasureledger.Payload) T {
	t.Helper()
	v, ok := p.(T)
	if !ok {
		t.Fatalf("payload type = %T, want %T", p, v)
	}
	return v
}

// frameOf builds a record frame with the canonical envelope and the given
// body, so a case only has to describe the part it is testing.
func frameOf(kind erasureledger.Kind, body []byte) []byte {
	return frameBytes(1, envelopeFields(kind, body)...)
}

// join concatenates encoded fields into one body.
func join(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// TestConstructorValidation covers the same rules the decoder enforces,
// on the Go side, so a writer cannot build an out-of-scope record in the first
// place.
func TestConstructorValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
	}{
		{"account zero id", func() error { _, err := erasureledger.NewAccount(0); return err }()},
		{"file zero id", func() error { _, err := erasureledger.NewFile(0); return err }()},
		{"self-delete zero owner", func() error { _, err := erasureledger.NewSelfDelete(0, 7); return err }()},
		{"empty copy set", func() error { _, err := erasureledger.NewMessageCopies(); return err }()},
		{"empty post set", func() error { _, err := erasureledger.NewChannelPostCopies(); return err }()},
		{"empty exclusion set", func() error {
			_, err := erasureledger.NewRandomExclusion(erasureledger.RandomClassChannel)
			return err
		}()},
		{"exclusion id zero", func() error {
			_, err := erasureledger.NewRandomExclusion(erasureledger.RandomClassChannel, 0)
			return err
		}()},
		{"reservation ceiling zero", func() error {
			_, err := erasureledger.NewReservation(mustAllocator(t, "users_id_seq"), 0)
			return err
		}()},
		{"allocator empty", func() error { _, err := erasureledger.ValidateAllocator(""); return err }()},
		{"allocator uppercase", func() error { _, err := erasureledger.ValidateAllocator("Users_id_seq"); return err }()},
		{"allocator digit leading", func() error { _, err := erasureledger.ValidateAllocator("1users_id_seq"); return err }()},
		{"allocator too long", func() error {
			_, err := erasureledger.ValidateAllocator(string(make([]byte, erasureledger.MaxAllocatorNameLen+1)))
			return err
		}()},
		{"epoch number zero", func() error {
			_, err := erasureledger.NewEpoch(0, lineageID(1), erasureledger.EpochEstablished)
			return err
		}()},
		{"epoch level zero", func() error {
			_, err := erasureledger.NewEpoch(1, lineageID(1), erasureledger.EpochLevel(0))
			return err
		}()},
		{"gallery entry zero", func() error {
			_, err := erasureledger.NewGalleryDelete(5, 1, erasureledger.GalleryEntry{FileID: 0, ClientFileID: 88})
			return err
		}()},
		{"gallery revision negative", func() error {
			_, err := erasureledger.NewGalleryDelete(5, -1, erasureledger.GalleryEntry{FileID: 1, ClientFileID: 1})
			return err
		}()},
		{"receipt non-terminal state", func() error {
			_, err := erasureledger.NewReceiptTerminal(5, 88, erasureledger.ReceiptState(0), 203)
			return err
		}()},
		{"receipt zero client id", func() error {
			_, err := erasureledger.NewReceiptTerminal(5, 0, erasureledger.ReceiptDeleted, 203)
			return err
		}()},
		{"record payload kind mismatch", func() error {
			_, err := erasureledger.NewRecord(erasureledger.KindFile, 1, streamID(1), 1, opKey(1), mustAccount(t))
			return err
		}()},
		{"record unknown kind", func() error {
			_, err := erasureledger.NewRecord(erasureledger.Kind(77), 1, streamID(1), 1, opKey(1), mustAccount(t))
			return err
		}()},
		{"record zero stream", func() error {
			_, err := erasureledger.NewRecord(erasureledger.KindAccount, 1, erasureledger.StreamID{}, 1, opKey(1), mustAccount(t))
			return err
		}()},
		{"record nil payload", func() error {
			_, err := erasureledger.NewRecord(erasureledger.KindAccount, 1, streamID(1), 1, opKey(1), nil)
			return err
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.err == nil {
				t.Fatal("constructor accepted an invalid input")
			}
			if !errors.Is(tc.err, erasureledger.ErrRejected) {
				t.Fatalf("err = %v, want ErrRejected", tc.err)
			}
		})
	}
}

func TestRecordRejectsPointerPayloads(t *testing.T) {
	t.Parallel()
	var nilAccount *erasureledger.Account
	cases := []struct {
		name    string
		payload erasureledger.Payload
	}{
		{name: "typed nil pointer", payload: nilAccount},
		{name: "non-nil pointer", payload: &erasureledger.Account{UserID: 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertRejected := func(operation string, call func() error) {
				t.Helper()
				defer func() {
					if recovered := recover(); recovered != nil {
						t.Errorf("%s panicked: %v", operation, recovered)
					}
				}()
				if err := call(); !errors.Is(err, erasureledger.ErrRejected) {
					t.Errorf("%s error = %v, want ErrRejected", operation, err)
				}
			}

			assertRejected("NewRecord", func() error {
				_, err := erasureledger.NewRecord(erasureledger.KindAccount, 1, streamID(1), 1, opKey(1), tc.payload)
				return err
			})
			assertRejected("Encode", func() error {
				_, err := erasureledger.Encode(erasureledger.Record{
					Kind:    erasureledger.KindAccount,
					Epoch:   1,
					Stream:  streamID(1),
					Seq:     1,
					OpKey:   opKey(1),
					Payload: tc.payload,
				})
				return err
			})
		})
	}
}

// TestEncodeRejectsOversizedSet checks the per-record set bound on the writer
// side, so no record can be built that a reader would have to refuse.
func TestEncodeRejectsOversizedSet(t *testing.T) {
	t.Parallel()
	ids := make([]int64, erasureledger.MaxSetMembers+1)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	if _, err := erasureledger.NewRandomExclusion(erasureledger.RandomClassChannel, ids...); !errors.Is(err, erasureledger.ErrRejected) {
		t.Fatalf("oversized exclusion set: err = %v, want ErrRejected", err)
	}
}
