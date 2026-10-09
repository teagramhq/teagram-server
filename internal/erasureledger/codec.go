package erasureledger

import (
	"encoding/binary"
	"math"
	"slices"
)

// Wire shape. Every field is a key of (field number << 3 | wire type) followed
// by a varint or a length-delimited run, so the format is self-describing
// enough to tell "this binary has no idea" from "these bytes are corrupt". The
// decoder is strict on purpose: fields must appear in ascending order, at most
// once unless the vocabulary declares them repeated, every field a kind
// requires must be present, integers must be minimally encoded, sets must be
// sorted and de-duplicated, and no byte may trail the last field. Canonical bytes are what makes "the same record
// again" a byte comparison, and strictness is what makes an unexpected field
// impossible to ignore.
const (
	wireVarint uint64 = 0
	wireBytes  uint64 = 2
)

// magic prefixes every frame. It is not compression: a frame that does not
// start with it is not a ledger record, and reading on would be guesswork.
const magic = "TGEL"

// CodecVersion is the framing version this binary writes and the highest it
// can read. A frame claiming more is not-ready, not best-effort: a field
// number introduced by a future framing has no meaning here, so trusting any
// part of it is unsound.
const CodecVersion = 1

// Envelope field numbers.
const (
	fieldKind         = 1
	fieldEpoch        = 2
	fieldStream       = 3
	fieldSeq          = 4
	fieldOperationKey = 5
	fieldPayload      = 6
)

// Batch field numbers.
const (
	fieldBatchRecord = 2
)

// Reserved tags are the fields the accepted privacy contract
// forbids off-alpha, spelled as numbers so a decoder can name them. They
// are in both the envelope and the body namespaces, they are never written,
// and they are always rejected. The generic unknown-field rule already
// refuses them; this reserved range makes the refusal explicit, so a
// record that carries a transport identity cannot be mistaken for a future
// format addition that a newer reader would understand.
const (
	tagAuthKeyID     = 17
	tagSessionID     = 18
	tagMsgID         = 19
	tagPhone         = 20
	tagText          = 21
	tagAccessHash    = 22
	tagBlobKey       = 23
	tagPayloadDigest = 24
)

// ReasonReservedField marks a frame that named a forbidden transport or
// content field.
const ReasonReservedField Reason = "reserved transport or content field"

// reservedTag reports whether field is a forbidden identity or content tag.
func reservedTag(field uint64) bool {
	return field >= tagAuthKeyID && field <= tagPayloadDigest
}

// Body field numbers. Numbering is local to each kind's namespace (and, for a
// set member, to the nested member namespace), which is what lets one
// kind grow without renumbering another. The name records who owns the slot.
const (
	fUserID    = 1 // Account
	fFileID    = 1 // File, GalleryEntry
	fOwnerID   = 1 // MessageCopy, GalleryDelete, ReceiptTerminal
	fClass     = 1 // RandomExclusion
	fAlloc     = 1 // Reservation
	fEpochNum  = 1 // Epoch
	fCopy      = 1 // MessageCopies, repeated
	fPost      = 1 // ChannelPostCopies, repeated
	fPostChann = 1 // ChannelPost

	fLocalID    = 2 // MessageCopy, ChannelPost
	fExclID     = 2 // RandomExclusion, repeated
	fCeiling    = 2 // Reservation
	fLineage    = 2 // Epoch
	fClientID   = 2 // GalleryEntry, ReceiptTerminal
	fEntry      = 2 // GalleryDelete, repeated
	fReceiptCli = 2 // ReceiptTerminal

	fLevel             = 3 // Epoch
	fRevision          = 3 // GalleryDelete
	fReceiptSt         = 3 // ReceiptTerminal
	fComponentCeiling  = 3 // ComponentReservation
	fReceiptFil        = 4 // ReceiptTerminal
	fBindingLineage    = 1 // StreamBinding
	fComponentBaseline = 2 // ComponentReservation
)

// Encode serializes one record. The result is canonical: the same semantic
// record always produces the same bytes, which is what a replay that must be
// idempotent and order-independent can be tested against.
func Encode(r Record) ([]byte, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}
	body, err := encodeBody(r.Payload)
	if err != nil {
		return nil, err
	}
	out := frame(CodecVersion)
	out = appendVarintField(out, fieldKind, int64(r.Kind))
	out = appendVarintField(out, fieldEpoch, r.Epoch)
	out = appendBytesField(out, fieldStream, r.Stream[:])
	out = appendVarintField(out, fieldSeq, r.Seq)
	out = appendBytesField(out, fieldOperationKey, r.OpKey[:])
	out = appendNested(out, fieldPayload, body)
	if len(out) > MaxRecordBytes {
		return nil, newRejected(ReasonTooLarge, "record")
	}
	return out, nil
}

// Decode parses one record. A frame past MaxRecordBytes is rejected before it
// is read: the bound is on the whole record, and a reader that accepted a frame
// the writer cannot produce would hand back a record that cannot be
// re-encoded. On any error the returned Record is the zero value: a partially
// read record is never handed back, so a caller cannot act on a body that was
// half understood.
func Decode(data []byte) (Record, error) {
	if len(data) > MaxRecordBytes {
		return Record{}, newRejected(ReasonTooLarge, "record")
	}
	version, rest, err := splitFrame(data)
	if err != nil {
		return Record{}, err
	}
	if err := checkVersion(version); err != nil {
		return Record{}, err
	}
	rec, err := decodeEnvelope(rest, version)
	if err != nil {
		return Record{}, err
	}
	return rec, nil
}

// EncodeBatch serializes a batch of records as one frame, each record nested
// as a complete frame of its own. A batch is the shape an enumeration hands a
// reader, so the not-ready contract is a batch contract: one record a reader
// cannot understand fails the whole batch, and a batch is never a place where
// a record can go missing.
func EncodeBatch(records []Record) ([]byte, error) {
	if len(records) > MaxBatchRecords {
		return nil, newRejected(ReasonTooLarge, "batch")
	}
	out := frame(CodecVersion)
	for i, rec := range records {
		body, err := Encode(rec)
		if err != nil {
			return nil, wrapRejected(Reason("record "+itoa(uint64(i))+" invalid"), "batch", err)
		}
		out = appendNested(out, fieldBatchRecord, body)
	}
	if len(out) > MaxBatchBytes {
		return nil, newRejected(ReasonTooLarge, "batch")
	}
	return out, nil
}

// DecodeBatch parses a batch. It returns no records on failure: a reader that
// cannot understand one record of a listing must not admit a subset of it,
// which is the completeness rule for replay. Each nested record is read as a
// full frame, so a batch cannot smuggle a record past the framing check.
func DecodeBatch(data []byte) ([]Record, error) {
	version, rest, err := splitFrame(data)
	if err != nil {
		return nil, err
	}
	if err := checkVersion(version); err != nil {
		return nil, err
	}
	if len(data) > MaxBatchBytes {
		return nil, newRejected(ReasonTooLarge, "batch")
	}
	var out []Record
	fs := newFields(rest)
	for !fs.done() {
		field, wire, err := fs.next(fieldBatchRecord)
		if err != nil {
			return nil, err
		}
		if reservedTag(field) {
			return nil, newRejected(ReasonReservedField, "batch")
		}
		if field != fieldBatchRecord {
			return nil, newRejected(ReasonUnknownField, "batch")
		}
		if wire != wireBytes {
			return nil, newRejected(ReasonWireType, "batch")
		}
		body, err := fs.rd.delimited(MaxRecordBytes)
		if err != nil {
			return nil, err
		}
		rec, err := Decode(body)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
		if len(out) > MaxBatchRecords {
			return nil, newRejected(ReasonTooLarge, "batch")
		}
	}
	return out, nil
}

// decodeEnvelope reads the six envelope fields and then the body for a kind
// this binary knows. An unknown kind is reported not-ready only once the
// envelope has been read in full and its fields validated, so structurally
// sound input from a newer binary is never downgraded to a parse error, and
// structurally broken input is never promoted to not-ready. The envelope
// belongs to every kind, known or not: a zero epoch, sequence, stream, or
// operation key is a write every binary rejects, so it must never be reported
// to admission as "wait for a newer binary".
func decodeEnvelope(data []byte, version uint16) (Record, error) {
	var (
		rec   Record
		body  []byte
		seen  uint64
		known = true
	)
	fs := newFields(data)
	for !fs.done() {
		field, wire, err := fs.next()
		if err != nil {
			return Record{}, err
		}
		if reservedTag(field) {
			return Record{}, newRejected(ReasonReservedField, "record")
		}
		switch field {
		case fieldKind:
			if wire != wireVarint {
				return Record{}, newRejected(ReasonWireType, "kind")
			}
			v, err := fs.rd.varint("kind")
			if err != nil {
				return Record{}, err
			}
			if v < 1 || v > math.MaxUint16 {
				return Record{}, newRejected(ReasonOutOfRange, "kind")
			}
			rec.Kind = Kind(v)
			_, known = LookupKind(rec.Kind)
		case fieldEpoch:
			if wire != wireVarint {
				return Record{}, newRejected(ReasonWireType, "epoch")
			}
			rec.Epoch, err = fs.rd.varint("epoch")
			if err != nil {
				return Record{}, err
			}
		case fieldStream:
			if wire != wireBytes {
				return Record{}, newRejected(ReasonWireType, "stream")
			}
			b, err := fs.rd.fixed(StreamIDLen, "stream")
			if err != nil {
				return Record{}, err
			}
			copy(rec.Stream[:], b)
		case fieldSeq:
			if wire != wireVarint {
				return Record{}, newRejected(ReasonWireType, "seq")
			}
			rec.Seq, err = fs.rd.varint("seq")
			if err != nil {
				return Record{}, err
			}
		case fieldOperationKey:
			if wire != wireBytes {
				return Record{}, newRejected(ReasonWireType, "operation_key")
			}
			b, err := fs.rd.fixed(OperationKeyLen, "operation_key")
			if err != nil {
				return Record{}, err
			}
			copy(rec.OpKey[:], b)
		case fieldPayload:
			if wire != wireBytes {
				return Record{}, newRejected(ReasonWireType, "payload")
			}
			body, err = fs.rd.delimited(MaxRecordBytes)
			if err != nil {
				return Record{}, err
			}
		default:
			return Record{}, newRejected(ReasonUnknownField, "record")
		}
		seen |= 1 << field
	}
	for _, f := range []struct {
		field int
		name  string
	}{{fieldKind, "kind"}, {fieldEpoch, "epoch"}, {fieldStream, "stream"},
		{fieldSeq, "seq"}, {fieldOperationKey, "operation_key"}, {fieldPayload, "payload"}} {
		if seen&(1<<f.field) == 0 {
			return Record{}, newRejected(ReasonMissingField, f.name)
		}
	}
	if err := rec.validateEnvelope(); err != nil {
		return Record{}, err
	}
	if !known {
		// The frame is structurally sound and asks for a body this binary
		// has no meaning for: that is a newer binary's record, and the only
		// safe reading is "not ready".
		return Record{}, newNotReady(CauseUnknownKind, rec.Kind, int(version))
	}
	payload, err := decodeBody(rec.Kind, body)
	if err != nil {
		return Record{}, err
	}
	rec.Payload = payload
	if err := rec.validate(); err != nil {
		return Record{}, err
	}
	return rec, nil
}

// encodeBody serializes a payload in its kind's field namespace.
func encodeBody(p Payload) ([]byte, error) {
	var out []byte
	switch v := p.(type) {
	case Account:
		out = appendVarintField(out, fUserID, v.UserID)
	case File:
		out = appendVarintField(out, fFileID, v.FileID)
	case MessageCopies:
		for _, c := range v.Copies {
			inner := appendVarintField(nil, fOwnerID, c.OwnerID)
			inner = appendVarintField(inner, fLocalID, int64(c.LocalID))
			out = appendNested(out, fCopy, inner)
		}
	case ChannelPostCopies:
		for _, p := range v.Posts {
			inner := appendVarintField(nil, fPostChann, p.ChannelID)
			inner = appendVarintField(inner, fLocalID, int64(p.LocalID))
			out = appendNested(out, fPost, inner)
		}
	case RandomExclusion:
		out = appendVarintField(out, fClass, int64(v.Class))
		for _, id := range v.IDs {
			out = appendVarintField(out, fExclID, id)
		}
	case Reservation:
		out = appendBytesField(out, fAlloc, []byte(v.Allocator))
		out = appendVarintField(out, fCeiling, v.Ceiling)
	case StreamBinding:
		out = appendBytesField(out, fBindingLineage, v.Lineage[:])
	case ComponentReservation:
		out = appendBytesField(out, fAlloc, []byte(v.Allocator))
		out = appendVarintField(out, fComponentBaseline, v.Baseline)
		out = appendVarintField(out, fComponentCeiling, v.Ceiling)
	case Epoch:
		out = appendVarintField(out, fEpochNum, v.Number)
		out = appendBytesField(out, fLineage, v.Lineage[:])
		out = appendVarintField(out, fLevel, int64(v.Level))
	case GalleryDelete:
		out = appendVarintField(out, fOwnerID, v.OwnerID)
		for _, e := range v.Entries {
			inner := appendVarintField(nil, fFileID, e.FileID)
			inner = appendVarintField(inner, fClientID, e.ClientFileID)
			out = appendNested(out, fEntry, inner)
		}
		out = appendVarintField(out, fRevision, v.Revision)
	case ReceiptTerminal:
		out = appendVarintField(out, fOwnerID, v.OwnerID)
		out = appendVarintField(out, fReceiptCli, v.ClientFileID)
		out = appendVarintField(out, fReceiptSt, int64(v.State))
		if v.FileID != NoFileID {
			out = appendVarintField(out, fReceiptFil, v.FileID)
		}
	default:
		return nil, newRejected("payload type has no encoder arm", "payload")
	}
	return out, nil
}

// decodeBody parses a payload in its kind's field namespace, then validates
// it with the same rules the constructor enforces.
func decodeBody(kind Kind, data []byte) (Payload, error) {
	switch kind {
	case KindAccount:
		var v Account
		fs := newFields(data)
		for !fs.done() {
			field, wire, err := fs.next()
			if err != nil {
				return nil, err
			}
			if field != fUserID || wire != wireVarint {
				return nil, bodyFieldError(field, wire, "user_id")
			}
			if v.UserID, err = fs.rd.varint("user_id"); err != nil {
				return nil, err
			}
		}
		return v, v.validate()
	case KindFile:
		var v File
		fs := newFields(data)
		for !fs.done() {
			field, wire, err := fs.next()
			if err != nil {
				return nil, err
			}
			if field != fFileID || wire != wireVarint {
				return nil, bodyFieldError(field, wire, "file_id")
			}
			if v.FileID, err = fs.rd.varint("file_id"); err != nil {
				return nil, err
			}
		}
		return v, v.validate()
	case KindMessageCopies:
		var v MessageCopies
		fs := newFields(data)
		for !fs.done() {
			field, wire, err := fs.next(fCopy)
			if err != nil {
				return nil, err
			}
			if field != fCopy || wire != wireBytes {
				return nil, bodyFieldError(field, wire, "copies")
			}
			inner, err := fs.rd.delimited(MaxRecordBytes)
			if err != nil {
				return nil, err
			}
			c, err := decodeMessageCopy(inner)
			if err != nil {
				return nil, err
			}
			v.Copies = append(v.Copies, c)
		}
		return v, v.validate()
	case KindChannelPosts:
		var v ChannelPostCopies
		fs := newFields(data)
		for !fs.done() {
			field, wire, err := fs.next(fPost)
			if err != nil {
				return nil, err
			}
			if field != fPost || wire != wireBytes {
				return nil, bodyFieldError(field, wire, "posts")
			}
			inner, err := fs.rd.delimited(MaxRecordBytes)
			if err != nil {
				return nil, err
			}
			p, err := decodeChannelPost(inner)
			if err != nil {
				return nil, err
			}
			v.Posts = append(v.Posts, p)
		}
		return v, v.validate()
	case KindRandomExclusion:
		var v RandomExclusion
		fs := newFields(data)
		for !fs.done() {
			// Only the id set repeats. The class is a scalar, and a second one
			// would make one record exclude ids from two allocators.
			field, wire, err := fs.next(fExclID)
			if err != nil {
				return nil, err
			}
			switch {
			case field == fClass && wire == wireVarint:
				c, err := fs.rd.varint("class")
				if err != nil {
					return nil, err
				}
				n, err := smallEnum(c, "class")
				if err != nil {
					return nil, err
				}
				v.Class = RandomClass(n)
			case field == fExclID && wire == wireVarint:
				id, err := fs.rd.varint("id")
				if err != nil {
					return nil, err
				}
				v.IDs = append(v.IDs, id)
			default:
				return nil, bodyFieldError(field, wire, "ids")
			}
		}
		return v, v.validate()
	case KindReservation:
		var v Reservation
		fs := newFields(data)
		for !fs.done() {
			field, wire, err := fs.next()
			if err != nil {
				return nil, err
			}
			switch {
			case field == fAlloc && wire == wireBytes:
				name, err := fs.rd.delimited(MaxAllocatorNameLen)
				if err != nil {
					return nil, err
				}
				v.Allocator = AllocatorName(name)
			case field == fCeiling && wire == wireVarint:
				v.Ceiling, err = fs.rd.varint("ceiling")
				if err != nil {
					return nil, err
				}
			default:
				return nil, bodyFieldError(field, wire, "allocator")
			}
		}
		return v, v.validate()
	case KindStreamBinding:
		var v StreamBinding
		fs := newFields(data)
		for !fs.done() {
			field, wire, err := fs.next()
			if err != nil {
				return nil, err
			}
			if field != fBindingLineage || wire != wireBytes {
				return nil, bodyFieldError(field, wire, "lineage")
			}
			b, err := fs.rd.fixed(LineageIDLen, "lineage")
			if err != nil {
				return nil, err
			}
			copy(v.Lineage[:], b)
		}
		return v, v.validate()
	case KindComponentReservation:
		var v ComponentReservation
		var baselineSeen bool
		fs := newFields(data)
		for !fs.done() {
			field, wire, err := fs.next()
			if err != nil {
				return nil, err
			}
			switch {
			case field == fAlloc && wire == wireBytes:
				name, err := fs.rd.delimited(MaxAllocatorNameLen)
				if err != nil {
					return nil, err
				}
				v.Allocator = AllocatorName(name)
			case field == fComponentBaseline && wire == wireVarint:
				if v.Baseline, err = fs.rd.varint("baseline"); err != nil {
					return nil, err
				}
				baselineSeen = true
			case field == fComponentCeiling && wire == wireVarint:
				if v.Ceiling, err = fs.rd.varint("ceiling"); err != nil {
					return nil, err
				}
			default:
				return nil, bodyFieldError(field, wire, "component_reservation")
			}
		}
		if !baselineSeen {
			return nil, newRejected(ReasonMissingField, "baseline")
		}
		return v, v.validate()
	case KindEpoch:
		var v Epoch
		fs := newFields(data)
		for !fs.done() {
			field, wire, err := fs.next()
			if err != nil {
				return nil, err
			}
			switch {
			case field == fEpochNum && wire == wireVarint:
				if v.Number, err = fs.rd.varint("epoch_number"); err != nil {
					return nil, err
				}
			case field == fLineage && wire == wireBytes:
				b, err := fs.rd.fixed(LineageIDLen, "lineage")
				if err != nil {
					return nil, err
				}
				copy(v.Lineage[:], b)
			case field == fLevel && wire == wireVarint:
				l, err := fs.rd.varint("level")
				if err != nil {
					return nil, err
				}
				n, err := smallEnum(l, "level")
				if err != nil {
					return nil, err
				}
				v.Level = EpochLevel(n)
			default:
				return nil, bodyFieldError(field, wire, "epoch")
			}
		}
		return v, v.validate()
	case KindGalleryDelete:
		var v GalleryDelete
		// Revision is tracked by presence, not by value: zero is an encodable
		// revision, so a body that omits the field cannot be told from a clear
		// that cleared at zero, and reading the omission as zero invents clear
		// evidence at a revision the writer never named.
		var revisionSeen bool
		fs := newFields(data)
		for !fs.done() {
			// Only the deleted entries repeat. The owner and the revision are
			// scalars: a second one is a second meaning for one clear.
			field, wire, err := fs.next(fEntry)
			if err != nil {
				return nil, err
			}
			switch {
			case field == fOwnerID && wire == wireVarint:
				if v.OwnerID, err = fs.rd.varint("owner_id"); err != nil {
					return nil, err
				}
			case field == fEntry && wire == wireBytes:
				inner, err := fs.rd.delimited(MaxRecordBytes)
				if err != nil {
					return nil, err
				}
				e, err := decodeGalleryEntry(inner)
				if err != nil {
					return nil, err
				}
				v.Entries = append(v.Entries, e)
			case field == fRevision && wire == wireVarint:
				rev, err := fs.rd.varint("revision")
				if err != nil {
					return nil, err
				}
				v.Revision = rev
				revisionSeen = true
			default:
				return nil, bodyFieldError(field, wire, "entries")
			}
		}
		if !revisionSeen {
			return nil, newRejected(ReasonMissingField, "revision")
		}
		return v, v.validate()
	case KindReceiptTerminal:
		var v ReceiptTerminal
		fs := newFields(data)
		for !fs.done() {
			field, wire, err := fs.next()
			if err != nil {
				return nil, err
			}
			switch {
			case field == fOwnerID && wire == wireVarint:
				if v.OwnerID, err = fs.rd.varint("owner_id"); err != nil {
					return nil, err
				}
			case field == fReceiptCli && wire == wireVarint:
				if v.ClientFileID, err = fs.rd.varint("client_file_id"); err != nil {
					return nil, err
				}
			case field == fReceiptSt && wire == wireVarint:
				st, err := fs.rd.varint("state")
				if err != nil {
					return nil, err
				}
				n, err := smallEnum(st, "state")
				if err != nil {
					return nil, err
				}
				v.State = ReceiptState(n)
			case field == fReceiptFil && wire == wireVarint:
				fid, err := fs.rd.varint("file_id")
				if err != nil {
					return nil, err
				}
				// Encode omits this field when the receipt names no file, so a
				// frame writing zero is a second encoding of the same receipt,
				// which is what a byte-equality replay check cannot tolerate.
				if fid == NoFileID {
					return nil, newRejected(ReasonNotCanonical, "file_id")
				}
				v.FileID = fid
			default:
				return nil, bodyFieldError(field, wire, "receipt")
			}
		}
		return v, v.validate()
	default:
		return nil, newNotReady(CauseUnknownKind, kind, CodecVersion)
	}
}

func decodeMessageCopy(data []byte) (MessageCopy, error) {
	var v MessageCopy
	fs := newFields(data)
	for !fs.done() {
		field, wire, err := fs.next()
		if err != nil {
			return MessageCopy{}, err
		}
		switch {
		case field == fOwnerID && wire == wireVarint:
			if v.OwnerID, err = fs.rd.varint("owner_id"); err != nil {
				return MessageCopy{}, err
			}
		case field == fLocalID && wire == wireVarint:
			n, err := fs.rd.varint("local_id")
			if err != nil {
				return MessageCopy{}, err
			}
			if n > int64(maxLocalID) {
				return MessageCopy{}, newRejected(ReasonOutOfRange, "local_id")
			}
			v.LocalID = int32(n) //nolint:gosec // G115: bounded to MaxInt32 above.
		default:
			return MessageCopy{}, bodyFieldError(field, wire, "copy")
		}
	}
	return v, nil
}

func decodeChannelPost(data []byte) (ChannelPost, error) {
	var v ChannelPost
	fs := newFields(data)
	for !fs.done() {
		field, wire, err := fs.next()
		if err != nil {
			return ChannelPost{}, err
		}
		switch {
		case field == fPostChann && wire == wireVarint:
			if v.ChannelID, err = fs.rd.varint("channel_id"); err != nil {
				return ChannelPost{}, err
			}
		case field == fLocalID && wire == wireVarint:
			n, err := fs.rd.varint("local_id")
			if err != nil {
				return ChannelPost{}, err
			}
			if n > int64(maxLocalID) {
				return ChannelPost{}, newRejected(ReasonOutOfRange, "local_id")
			}
			v.LocalID = int32(n) //nolint:gosec // G115: bounded to MaxInt32 above.
		default:
			return ChannelPost{}, bodyFieldError(field, wire, "post")
		}
	}
	return v, nil
}

func decodeGalleryEntry(data []byte) (GalleryEntry, error) {
	var v GalleryEntry
	fs := newFields(data)
	for !fs.done() {
		field, wire, err := fs.next()
		if err != nil {
			return GalleryEntry{}, err
		}
		switch {
		case field == fFileID && wire == wireVarint:
			if v.FileID, err = fs.rd.varint("file_id"); err != nil {
				return GalleryEntry{}, err
			}
		case field == fClientID && wire == wireVarint:
			if v.ClientFileID, err = fs.rd.varint("client_file_id"); err != nil {
				return GalleryEntry{}, err
			}
		default:
			return GalleryEntry{}, bodyFieldError(field, wire, "entry")
		}
	}
	return v, nil
}

// bodyFieldError rejects a body field the kind does not define, naming the
// set it belongs to. A reserved tag gets the reserved reason, so a body
// carrying a transport identity is reported as a privacy violation and not
// as a format surprise.
func bodyFieldError(field, wire uint64, set string) error {
	if reservedTag(field) {
		return newRejected(ReasonReservedField, set)
	}
	if wire == wireBytes {
		return newRejected(ReasonBytesForbidden, set)
	}
	return newRejected(ReasonUnknownField, set)
}

// ReasonBytesForbidden marks a length-delimited value in a body namespace
// that has no byte-valued field. It is the structural half of the
// identifier-only rule: the only byte-valued body fields in the vocabulary
// are the opaque lineage id and the schema allocator name, so a body that
// presents text, a digest, or a blob key as a value cannot be read.
const ReasonBytesForbidden Reason = "byte-valued field is not part of this kind"

// fields tracks one frame's field stream in ascending order.
type fields struct {
	rd   *reader
	last uint64
}

func newFields(data []byte) *fields {
	return &fields{rd: &reader{buf: data}}
}

func (f *fields) done() bool { return f.rd.pos >= len(f.rd.buf) }

// next reads the next field key. Keys must ascend, and a key may repeat only
// for a field the caller declares as a repeated set member: a second scalar
// with the same number is a second meaning for one identifier, which is a
// rejection, not a last-write-wins.
func (f *fields) next(repeated ...uint64) (field, wire uint64, err error) {
	key, err := f.rd.key()
	if err != nil {
		return 0, 0, err
	}
	field, wire = key>>3, key&0x07
	if field == 0 {
		return 0, 0, newRejected(ReasonMalformed, "field number zero")
	}
	switch {
	case field < f.last:
		return 0, 0, newRejected(ReasonFieldOrder, "record")
	case field == f.last && !slices.Contains(repeated, field):
		return 0, 0, newRejected(ReasonDuplicateField, "record")
	}
	f.last = field
	return field, wire, nil
}

// reader is a strict cursor over one frame.
type reader struct {
	buf []byte
	pos int
}

func (r *reader) key() (uint64, error) {
	return r.uvarint()
}

// varint reads a signed-width value, rejecting an out-of-range
// encoding rather than truncating it. A record whose identifier does not fit
// the class is a bug in the writer, and reading it as a wrapped value is how
// an erasure gets applied to the wrong identity.
func (r *reader) varint(field string) (int64, error) {
	v, err := r.uvarint()
	if err != nil {
		return 0, err
	}
	if v > uint64(math.MaxInt64) {
		return 0, newRejected(ReasonOutOfRange, field)
	}
	return int64(v), nil
}

// smallEnum decodes an enumerated value at its declared width. An
// out-of-width value is a rejection, so a reader never truncates a value a
// newer binary wrote.
func smallEnum(v int64, field string) (uint8, error) {
	if v > math.MaxUint8 {
		return 0, newRejected(ReasonOutOfRange, field)
	}
	return uint8(v), nil //nolint:gosec // G115: bounded to MaxUint8 above.
}

// delimited reads a length-prefixed run, bounded so a hostile length cannot
// make a reader allocate.
func (r *reader) delimited(limit uint64) ([]byte, error) {
	n, err := r.uvarint()
	if err != nil {
		return nil, err
	}
	if n > limit {
		return nil, newRejected(ReasonTooLarge, "length")
	}
	if int(n) > len(r.buf)-r.pos { //nolint:gosec // G115: n is bounded by limit, at most MaxRecordBytes.
		return nil, newRejected(ReasonTruncated, "length")
	}
	end := r.pos + int(n) //nolint:gosec // G115: n is bounded by the remaining frame length.
	out := r.buf[r.pos:end]
	r.pos = end
	return out, nil
}

// fixed reads exactly want bytes, which is how an opaque identity is
// decodable at all: a stream id, operation key, or lineage id of any other
// length was not written by this vocabulary.
func (r *reader) fixed(want uint64, field string) ([]byte, error) {
	b, err := r.delimited(want)
	if err != nil {
		return nil, err
	}
	if uint64(len(b)) != want {
		return nil, newRejected(ReasonOutOfRange, field)
	}
	return b, nil
}

// uvarint reads a minimally encoded unsigned integer. Non-minimal padding is
// rejected: canonical bytes are the point, and a padded integer is the
// cheapest way for two writers to encode "the same" record differently.
func (r *reader) uvarint() (uint64, error) {
	var v uint64
	for i := range binary.MaxVarintLen64 {
		if r.pos >= len(r.buf) {
			return 0, newRejected(ReasonTruncated, "varint")
		}
		b := r.buf[r.pos]
		r.pos++
		if i == binary.MaxVarintLen64-1 && b > 1 {
			return 0, newRejected(ReasonOutOfRange, "varint")
		}
		v |= uint64(b&0x7f) << (7 * i)
		if b < 0x80 {
			if i > 0 && b == 0 {
				return 0, newRejected(ReasonNotCanonical, "varint")
			}
			return v, nil
		}
	}
	return 0, newRejected(ReasonMalformed, "varint")
}

// frame starts a frame with the magic and framing version.
func frame(version uint16) []byte {
	return binary.BigEndian.AppendUint16([]byte(magic), version)
}

// splitFrame checks the magic and returns the declared version plus the field
// bytes.
func splitFrame(data []byte) (uint16, []byte, error) {
	if len(data) < len(magic)+2 {
		return 0, nil, newRejected(ReasonTruncated, "frame")
	}
	if string(data[:len(magic)]) != magic {
		return 0, nil, newRejected("frame is not an erasure ledger record", "magic")
	}
	return binary.BigEndian.Uint16(data[len(magic):]), data[len(magic)+2:], nil
}

// checkVersion refuses to read a framing this binary predates.
func checkVersion(version uint16) error {
	if version < 1 {
		return newRejected(ReasonOutOfRange, "codec_version")
	}
	if version > CodecVersion {
		return newNotReady(CauseCodecVersion, 0, int(version))
	}
	return nil
}

func appendVarintField(out []byte, field uint64, v int64) []byte {
	out = appendKey(out, field, wireVarint)
	return binary.AppendUvarint(out, uint64(v)) //nolint:gosec // G115: callers validate identifiers as positive.
}

func appendBytesField(out []byte, field uint64, v []byte) []byte {
	out = appendKey(out, field, wireBytes)
	out = binary.AppendUvarint(out, uint64(len(v)))
	return append(out, v...)
}

func appendNested(out []byte, field uint64, inner []byte) []byte {
	return appendBytesField(out, field, inner)
}

func appendKey(out []byte, field, wire uint64) []byte {
	return binary.AppendUvarint(out, field<<3|wire)
}
