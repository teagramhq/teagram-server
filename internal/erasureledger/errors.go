package erasureledger

import (
	"errors"
	"strconv"
)

// The two failure families a reader reports. They are deliberately distinct,
// because the correct response to each is opposite.
//
// ErrNotReady means the input is well-formed but asks for a meaning this
// binary does not have, because a newer binary wrote it. A replayer must
// close the restore gate, not skip the record: MAIN-1436 names that explicitly
// for the gallery_delete kind, and MAIN-1360 requires that no record be
// silently passed over.
//
// ErrRejected means the input is not a valid record at all: malformed,
// truncated, out of width, non-canonical, or carrying a field the vocabulary
// does not define. There is nothing to admit and nothing to gate on; the
// write is bad and must not be trusted.
var (
	// ErrNotReady is returned for input that only a newer binary can
	// meaningfully read.
	ErrNotReady = errors.New("erasureledger: not readable by this binary")
	// ErrRejected is returned for input that is not a valid record.
	ErrRejected = errors.New("erasureledger: record rejected")
)

// Reason classifies a rejection so a caller can branch on a typed value
// instead of parsing error text. Free-form reasons are allowed for
// descriptive cases; these are the ones a reader can act on.
type Reason string

const (
	// ReasonMalformed marks bytes that do not parse.
	ReasonMalformed Reason = "malformed"
	// ReasonTruncated marks input that ends inside a field.
	ReasonTruncated Reason = "truncated"
	// ReasonTrailing marks input with bytes after the last field.
	ReasonTrailing Reason = "trailing bytes"
	// ReasonUnknownField marks a field the vocabulary does not define outside
	// the reserved transport and content tags. Those tags return
	// ReasonReservedField so consumers can distinguish privacy violations from
	// format surprises.
	ReasonUnknownField Reason = "unknown field"
	// ReasonDuplicateField marks a field that appears twice, which is a
	// second meaning for one identifier.
	ReasonDuplicateField Reason = "duplicate field"
	// ReasonMissingField marks a required field that is absent.
	ReasonMissingField Reason = "missing field"
	// ReasonFieldOrder marks fields that are not in ascending order, which
	// breaks the single canonical encoding a byte-equality replay test needs.
	ReasonFieldOrder Reason = "field order"
	// ReasonWireType marks a field encoded with the wrong wire shape.
	ReasonWireType Reason = "wire type"
	// ReasonOutOfRange marks an identifier outside the width the class
	// allows, including a message id past the signed-32-bit wire width.
	ReasonOutOfRange Reason = "out of range"
	// ReasonNotCanonical marks a set that is not sorted, a set that is
	// empty, or an integer that is not minimally encoded.
	ReasonNotCanonical Reason = "not canonical"
	// ReasonDuplicate marks the same set member twice in one record.
	ReasonDuplicate Reason = "duplicate member"
	// ReasonTooLarge marks input past a declared bound.
	ReasonTooLarge Reason = "too large"
	// ReasonAllZero marks an opaque identifier that is all zero, which is not
	// a drawn value.
	ReasonAllZero Reason = "all-zero identifier"
)

// RejectedError reports why a record was rejected, naming the field.
type RejectedError struct {
	Reason Reason
	Field  string
	cause  error
}

func newRejected(reason Reason, field string) error {
	return &RejectedError{Reason: reason, Field: field}
}

func wrapRejected(reason Reason, field string, cause error) error {
	return &RejectedError{Reason: reason, Field: field, cause: cause}
}

// Error implements error.
func (e *RejectedError) Error() string {
	s := "erasureledger: reject field=" + e.Field + " reason=" + string(e.Reason)
	if e.cause != nil {
		s += ": " + e.cause.Error()
	}
	return s
}

// Unwrap reports the rejection family, so errors.Is(err, ErrRejected) holds.
func (e *RejectedError) Unwrap() error { return ErrRejected }

// NotReadyCause classifies why a record is unreadable by this binary. The two
// causes are the two ways a newer binary's output can outrun this one.
type NotReadyCause uint8

const (
	// CauseUnknownKind means a record carries a kind this binary has no
	// payload type, validation, or replay meaning for.
	CauseUnknownKind NotReadyCause = 1
	// CauseCodecVersion means the framing itself is newer than CodecVersion,
	// so no field in it can be trusted.
	CauseCodecVersion NotReadyCause = 2
)

// String names a not-ready cause.
func (c NotReadyCause) String() string {
	switch c {
	case CauseUnknownKind:
		return "unknown kind"
	case CauseCodecVersion:
		return "codec version"
	default:
		return "cause(" + itoa(uint64(c)) + ")"
	}
}

// NotReadyError reports input a newer binary wrote.
type NotReadyError struct {
	Cause        NotReadyCause
	Kind         Kind
	CodecVersion int
}

func newNotReady(cause NotReadyCause, kind Kind, version int) error {
	return &NotReadyError{Cause: cause, Kind: kind, CodecVersion: version}
}

// Error implements error.
func (e *NotReadyError) Error() string {
	switch e.Cause {
	case CauseUnknownKind:
		return "erasureledger: not ready kind=" + e.Kind.String()
	case CauseCodecVersion:
		return "erasureledger: not ready codec_version=" + strconv.Itoa(e.CodecVersion)
	default:
		return "erasureledger: not ready cause=" + e.Cause.String()
	}
}

// Unwrap reports the not-ready family, so errors.Is(err, ErrNotReady) holds.
func (e *NotReadyError) Unwrap() error { return ErrNotReady }

// NotReadyInfo is the admission-facing view of a not-ready failure. A
// future admission gate consumes this instead of guessing what an error
// string meant: it learns whether the gate must close because a kind is
// unknown to this build, or because the whole framing is newer.
type NotReadyInfo struct {
	Cause        NotReadyCause
	Kind         Kind
	CodecVersion int
}

// NotReady extracts the not-ready detail from err, reporting false for any
// other failure.
func NotReady(err error) (NotReadyInfo, bool) {
	var nre *NotReadyError
	if !errors.As(err, &nre) {
		return NotReadyInfo{}, false
	}
	return NotReadyInfo{Cause: nre.Cause, Kind: nre.Kind, CodecVersion: nre.CodecVersion}, true
}

// RejectionInfo is the admission-facing view of a rejection.
type RejectionInfo struct {
	Reason Reason
	Field  string
}

// Rejection extracts the rejection detail from err, reporting false for any
// other failure.
func Rejection(err error) (RejectionInfo, bool) {
	var re *RejectedError
	if !errors.As(err, &re) {
		return RejectionInfo{}, false
	}
	return RejectionInfo{Reason: re.Reason, Field: re.Field}, true
}

// itoa formats a number for identifiers and error text without pulling
// fmt into these hot paths.
func itoa(v uint64) string { return strconv.FormatUint(v, 10) }
