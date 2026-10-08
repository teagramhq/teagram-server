package erasureledger_test

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/teagramhq/teagram-server/internal/erasureledger"
)

// forbiddenNameParts are the field names a payload type must never grow.
// They are the transport identities, content, and key material the accepted
// privacy contract keeps on-alpha: an auth key, a
// session, an MTProto message id, a phone, text, a peer access hash, a blob
// key, an upload payload digest. A receipt's request metadata is in
// the same list: a size, part count and content digest are enough to fingerprint
// a deleted photo, and a replayer does not need them.
var forbiddenNameParts = []string{
	"authkey", "auth", "session", "msg", "msgid", "messageid",
	"phone", "msisdn", "text", "caption", "content", "body", "mime",
	"filename", "accesshash", "access hash", "hash", "blob", "key",
	"digest", "secret", "password", "token", "cookie", "device",
	"ipaddr", "clientip", "remoteip", "address", "latitude", "longitude",
	"useragent", "time", "stamp", "created", "updated", "size", "part",
	"raw", "bytes", "data", "value", "string", "payload",
}

// payloadTypes is one value per accepted kind, which is the set the
// structural checks walk: a vocabulary's privacy property is a property of
// every kind, not of one convenient kind.
func payloadTypes(t *testing.T) []erasureledger.Payload {
	t.Helper()
	rt := roundTrips(t)
	out := make([]erasureledger.Payload, 0, len(rt))
	for _, tc := range rt {
		out = append(out, tc.payload)
	}
	return out
}

// TestPayloadTypesHaveNoForbiddenFields walks every payload type's field
// graph, including nested set members, and refuses any field whose name names
// forbidden material. This is the structural half of the identifier-only rule:
// a future field that carries a phone, a text, a hash, a key, a digest,
// or a timestamp fails here, in the vocabulary's own test, before any writer
// can put it on the wire.
func TestPayloadTypesHaveNoForbiddenFields(t *testing.T) {
	t.Parallel()
	for _, p := range payloadTypes(t) {
		t.Run(reflect.TypeOf(p).Name(), func(t *testing.T) {
			walkFields(t, reflect.TypeOf(p), reflect.TypeOf(p).Name(), map[reflect.Type]bool{})
		})
	}
}

// walkFields visits every field type reachable from a payload type.
func walkFields(t *testing.T, typ reflect.Type, path string, seen map[reflect.Type]bool) {
	t.Helper()
	if seen[typ] {
		return
	}
	seen[typ] = true

	name := typ.Name()
	lower := strings.ToLower(name)
	for _, part := range forbiddenNameParts {
		if strings.Contains(lower, part) {
			t.Errorf("type %s is named %q, matching forbidden %q", path, name, part)
		}
	}
	switch typ.Kind() {
	case reflect.Struct:
		for f := range typ.Fields() {
			fname := strings.ToLower(f.Name)
			for _, part := range forbiddenNameParts {
				if strings.Contains(fname, part) {
					t.Errorf("%s.%s is a forbidden field name (matches %q)", path, f.Name, part)
				}
			}
			walkFields(t, f.Type, path+"."+f.Name, seen)
		}
	case reflect.Slice, reflect.Array:
		// A byte-valued field is the shape text, a digest, and a blob key
		// take on the wire. The vocabulary's only byte-carrying values are
		// the fixed-length opaque identities, which are arrays of a named
		// type, and the schema allocator name, which is a named string type.
		elem := typ.Elem()
		if elem.Kind() == reflect.Uint8 && typ.Kind() == reflect.Slice && elem.PkgPath() == "" {
			t.Errorf("%s is a bare byte slice: a body field cannot carry bytes", path)
		}
		walkFields(t, elem, path+"[]", seen)
	case reflect.String:
		if typ.PkgPath() == "" {
			t.Errorf("%s is a bare string: a body field cannot carry text", path)
		}
	case reflect.Bool, reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128,
		reflect.Pointer, reflect.Map, reflect.Chan, reflect.Func, reflect.Interface:
		t.Errorf("%s has kind %v: the vocabulary carries identifiers, enumerated values, and fixed-width integers only", path, typ.Kind())
	}
}

// TestRecordEnvelopeHasNoForbiddenFields pins the envelope's field set. The
// opaque identities are the envelope's own business; a timestamp is not,
// because ordering comes from the provider's arrival evidence and a writer
// must not be able to order a ledger by its own clock.
func TestRecordEnvelopeHasNoForbiddenFields(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeFor[erasureledger.Record]()
	want := map[string]bool{
		"Kind": true, "Epoch": true, "Stream": true,
		"Seq": true, "OpKey": true, "Payload": true,
	}
	for f := range typ.Fields() {
		if !want[f.Name] {
			t.Errorf("Record has field %s: the envelope is a closed set", f.Name)
		}
		delete(want, f.Name)
	}
	for name := range want {
		t.Errorf("Record is missing envelope field %s", name)
	}
	payloadField, ok := reflect.TypeFor[erasureledger.Record]().FieldByName("Payload")
	if !ok || payloadField.Type.Kind() != reflect.Interface {
		t.Errorf("Payload field type = %v, want the sealed payload interface", payloadField.Type)
	}
}

// reservedTagNames names the reserved tags for test output, so a failure says
// which forbidden field leaked.
var reservedTagNames = map[uint64]string{
	17: "auth_key_id",
	18: "session_id",
	19: "msg_id",
	20: "phone",
	21: "text",
	22: "access_hash",
	23: "blob_key",
	24: "payload_digest",
}

// TestReservedTransportTagsAreRejected appends each forbidden transport or
// content tag to a valid record, in the envelope and in the body, and requires
// a rejection naming the reserved field. The privacy rule is enforced by the
// reader, not by convention: a record that carries a transport identity
// cannot be read at all, so a writer cannot smuggle one into the ledger.
func TestReservedTransportTagsAreRejected(t *testing.T) {
	t.Parallel()
	for _, tc := range roundTrips(t) {
		rec := record(t, tc.kind, 1, 1, tc.payload)
		data, err := erasureledger.Encode(rec)
		if err != nil {
			t.Fatalf("Encode kind=%v: %v", tc.kind, err)
		}
		for tag, label := range reservedTagNames {
			t.Run(tc.kind.String()+"/envelope/"+label, func(t *testing.T) {
				bad := append(append([]byte{}, data...), varintField(tag, 1)...)
				_, err := erasureledger.Decode(bad)
				if !errors.Is(err, erasureledger.ErrRejected) {
					t.Fatalf("Decode accepted a %s tag in the envelope: %v", label, err)
				}
				info, ok := erasureledger.Rejection(err)
				if !ok {
					t.Fatalf("Rejection(err) = false for %v", err)
				}
				if info.Reason != erasureledger.ReasonReservedField {
					t.Errorf("reason = %q, want the reserved-field reason", info.Reason)
				}
			})
			t.Run(tc.kind.String()+"/body/"+label, func(t *testing.T) {
				body := payloadBody(t, data)
				padded := append(append([]byte{}, body...), varintField(tag, 1)...)
				_, err := erasureledger.Decode(frameOf(tc.kind, padded))
				if !errors.Is(err, erasureledger.ErrRejected) {
					t.Fatalf("Decode accepted a %s tag in the %v body: %v", label, tc.kind, err)
				}
			})
		}
	}
}

// payloadBody returns the payload field's bytes from an encoded record.
func payloadBody(t *testing.T, data []byte) []byte {
	t.Helper()
	rest := data[len("TGEL")+2:]
	for len(rest) > 0 {
		key, adv := binary.Uvarint(rest)
		if adv <= 0 {
			t.Fatalf("field key is not a varint")
		}
		rest = rest[adv:]
		field, wire := int(key>>3), uint8(key&0x07)
		if wire == 0 {
			_, adv = binary.Uvarint(rest)
			if adv <= 0 {
				t.Fatalf("varint field is not decodable")
			}
			rest = rest[adv:]
			continue
		}
		n, adv := binary.Uvarint(rest)
		if adv <= 0 {
			t.Fatalf("length is not a varint")
		}
		rest = rest[adv:]
		length := int(n) //nolint:gosec // G115: test frames are bounded by MaxRecordBytes.
		value := rest[:length]
		rest = rest[length:]
		if field == 6 {
			return value
		}
	}
	t.Fatalf("encoded record has no payload field")
	return nil
}

// TestOperationKeyIsVerbatimRandomness is the "the opaque server operation
// key never embeds those values" check. NewOperationKey copies the caller's
// bytes and mixes in nothing, so even identifiers built from
// the same byte pattern as the key cannot perturb it, and the key in an
// encoded record is exactly the bytes that were drawn.
func TestOperationKeyIsVerbatimRandomness(t *testing.T) {
	t.Parallel()
	pattern := bytes.Repeat([]byte{0x5c}, erasureledger.OperationKeyLen)
	key, err := erasureledger.NewOperationKey(bytes.NewReader(pattern))
	if err != nil {
		t.Fatalf("NewOperationKey: %v", err)
	}
	if !bytes.Equal(key[:], pattern) {
		t.Fatalf("key = %x, want the drawn bytes %x verbatim", key[:], pattern)
	}

	// Adversarial identifiers: every one of them is spelled from the key's
	// own byte pattern, so any embedding of an identifier into the key would
	// show up as a difference from the drawn bytes.
	owner := int64(0x5c5c5c5c5c5c5c5c)
	file := mustFile(t, owner)
	data, err := erasureledger.Encode(mustRecord(t, erasureledger.KindFile, owner, key, file))
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !bytes.Contains(data, key[:]) {
		t.Fatalf("encoded record does not carry the operation key verbatim: %x", data)
	}
	if bytes.Count(data, key[:]) != 1 {
		t.Errorf("operation key bytes appear %d times; the key must be one opaque field", bytes.Count(data, key[:]))
	}

	// Drawn keys are distinct, which is what makes a retried write
	// findable and a fresh write unguessable in a listing.
	seen := map[erasureledger.OperationKey]bool{}
	for range 64 {
		k, err := erasureledger.NewOperationKey(rand.Reader)
		if err != nil {
			t.Fatalf("NewOperationKey(rand): %v", err)
		}
		if seen[k] {
			t.Fatalf("drawn key %x repeated", k)
		}
		seen[k] = true
	}

	// A short entropy source and an all-zero draw are refusals, not a
	// zero-padded key: a zero key cannot be a retry identity.
	if _, err := erasureledger.NewOperationKey(bytes.NewReader(pattern[:4])); !errors.Is(err, erasureledger.ErrRejected) {
		t.Errorf("short reader: err = %v, want ErrRejected", err)
	}
	if _, err := erasureledger.NewOperationKey(bytes.NewReader(make([]byte, 16))); !errors.Is(err, erasureledger.ErrRejected) {
		t.Errorf("all-zero draw: err = %v, want ErrRejected", err)
	}
	if _, err := erasureledger.NewStreamID(bytes.NewReader(make([]byte, 16))); !errors.Is(err, erasureledger.ErrRejected) {
		t.Errorf("all-zero stream: err = %v, want ErrRejected", err)
	}
	if _, err := erasureledger.NewLineageID(bytes.NewReader(make([]byte, 16))); !errors.Is(err, erasureledger.ErrRejected) {
		t.Errorf("all-zero lineage: err = %v, want ErrRejected", err)
	}
}

// TestEncodedRecordsCarryNoForbiddenLiterals encodes the whole vocabulary and
// scans the frames for the byte patterns of the material the ledger must
// never carry. The structural test is the real gate; this one catches a literal
// that reaches the wire through an encoder change.
func TestEncodedRecordsCarryNoForbiddenLiterals(t *testing.T) {
	t.Parallel()
	forbidden := []string{
		"phone", "+62", "auth_key", "session", "msg_id", "access_hash",
		"blob", "digest", "caption", "secret", "password", "photo.jpg",
		"sha256", "13800138000", "text/plain",
	}
	for _, tc := range roundTrips(t) {
		data, err := erasureledger.Encode(record(t, tc.kind, 1, 1, tc.payload))
		if err != nil {
			t.Fatalf("Encode kind=%v: %v", tc.kind, err)
		}
		for _, lit := range forbidden {
			if bytes.Contains(data, []byte(lit)) {
				t.Errorf("kind %v encodes the forbidden literal %q", tc.kind, lit)
			}
		}
	}
}

// mustFile builds a file tombstone body.
func mustFile(t *testing.T, fileID int64) erasureledger.File {
	t.Helper()
	f, err := erasureledger.NewFile(fileID)
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	return f
}

// mustRecord builds a record with a caller-supplied epoch and key.
func mustRecord(t *testing.T, kind erasureledger.Kind, epoch int64, key erasureledger.OperationKey, p erasureledger.Payload) erasureledger.Record {
	t.Helper()
	rec, err := erasureledger.NewRecord(kind, epoch, streamID(0x50), 1, key, p)
	if err != nil {
		t.Fatalf("NewRecord: %v", err)
	}
	return rec
}
