package blob

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
	"time"
)

// ErrEnvelope is returned by [Encrypted.ReadAt] for an object that is not an
// intact envelope written for the key it is read under: a plaintext object, an
// unknown format or key version, a damaged header or chunk, a truncated or
// extended object, or a valid object moved to another key. It is distinct
// from [ErrNotFound]: an object that is there but does not authenticate must
// never read as missing.
var ErrEnvelope = errors.New("blob envelope invalid")

// ErrEnvelopeKeyUnavailable means a structurally valid envelope header could
// not be authenticated because no media key ring was supplied. An inventory
// must fail in this case rather than report an encrypted object as plaintext.
var ErrEnvelopeKeyUnavailable = errors.New("blob envelope key unavailable")

// Envelope format v1. The header is fixed-size and big-endian:
//
//	offset size field
//	0      4    magic "TGME"
//	4      2    format version
//	6      4    key version (ring version of the key-encryption key)
//	10     4    chunk size C
//	14     8    plaintext length P
//	22     32   wrap salt, random per write
//	54     48   wrapped data key (AES-256-GCM ciphertext and tag)
//
// The object holds n = max(1, ceil(P/C)) chunks after the header. Chunk i
// seals C plaintext bytes, the last one the remainder, plus a 16-byte tag.
const (
	envelopeMagic      = "TGME"
	envelopeVersion    = 1
	envelopeBoundLen   = 54
	envelopeHeaderLen  = envelopeBoundLen + dataKeyLen + gcmTagLen
	envelopeSaltLen    = 32
	gcmTagLen          = 16
	gcmNonceLen        = 12
	dataKeyLen         = 32
	envelopeMaxPlain   = int64(1) << 60
	envelopeWrapInfo   = "teagram media envelope v1 data key wrap"
	envelopeChunkAADPf = 4 + 2 + 4 + 8 + 8 + 1

	// MinChunkSize and MaxChunkSize bound the chunk size an envelope header
	// may record. The size is a per-object format parameter, so changing the
	// writer default needs no format bump.
	MinChunkSize = 4 << 10
	MaxChunkSize = 4 << 20
	// DefaultChunkSize is the measured size new objects are written with.
	DefaultChunkSize = 16 << 10
)

// Encrypted is a [Store] decorator that writes every object as an
// authenticated, chunked envelope and reads it back by plaintext offset.
//
// Each Put draws a fresh random data key, seals the object's chunks under it,
// and wraps it under the ring's current key-encryption key. The header, the
// object key, each chunk's index and the last-chunk marker are all bound into
// the authentication, so a chunk moved, reordered, dropped or appended, an
// object truncated or copied to another key, and a header bit flipped all fail
// closed with [ErrEnvelope].
//
// Key validation and the window rules are the [Store] contract's, checked here
// before any backend work. Remove passes through. WalkPrefix passes through
// too, so the sizes it reports are the backend's physical sizes, not plaintext
// lengths.
type Encrypted struct {
	backend   Store
	ring      *KeyRing
	chunkSize int
}

// NewEncrypted wraps backend so every object it stores is sealed under ring.
// chunkSize is the size new objects are written with; objects already stored
// read back at whatever size their own header records.
func NewEncrypted(backend Store, ring *KeyRing, chunkSize int) (*Encrypted, error) {
	if backend == nil || ring == nil {
		return nil, errors.New("encrypted store: backend and key ring are required")
	}
	// A ring not built by NewKeyRing has no current key, and wrapping under
	// it would derive the wrap key from an empty secret.
	if kek, ok := ring.key(ring.Current()); !ok || len(kek) != MediaKeyLen {
		return nil, errors.New("encrypted store: key ring has no current key")
	}
	if chunkSize < MinChunkSize || chunkSize > MaxChunkSize {
		return nil, fmt.Errorf("encrypted store: chunk size %d outside [%d, %d]", chunkSize, MinChunkSize, MaxChunkSize)
	}
	return &Encrypted{backend: backend, ring: ring, chunkSize: chunkSize}, nil
}

// EnvelopePhysicalSize returns the byte length written for a plaintext object
// of plainBytes using chunkSize. It lets a writer authenticate a newly
// written envelope before a subsequent full-tree inventory.
func EnvelopePhysicalSize(plainBytes int64, chunkSize int) (int64, error) {
	if plainBytes < 0 || plainBytes > envelopeMaxPlain {
		return 0, fmt.Errorf("envelope size: plaintext length %d out of range", plainBytes)
	}
	if chunkSize < MinChunkSize || chunkSize > MaxChunkSize {
		return 0, fmt.Errorf("envelope size: chunk size %d outside [%d, %d]", chunkSize, MinChunkSize, MaxChunkSize)
	}
	chunks := max(int64(1), (plainBytes+int64(chunkSize)-1)/int64(chunkSize))
	if chunks > (math.MaxInt64-envelopeHeaderLen-plainBytes)/gcmTagLen {
		return 0, errors.New("envelope size: physical length overflows int64")
	}
	return envelopeHeaderLen + plainBytes + chunks*gcmTagLen, nil
}

// OperationTimeout forwards a backend's per-operation bound through the
// decorator. Stores without a backend timeout keep the caller's default.
func (e *Encrypted) OperationTimeout() time.Duration {
	if bounded, ok := e.backend.(OperationTimeoutProvider); ok {
		return bounded.OperationTimeout()
	}
	return 0
}

// envelopeHeader is a parsed, not yet authenticated, header.
type envelopeHeader struct {
	keyVersion uint32
	chunkSize  int64
	plainLen   int64
	raw        [envelopeHeaderLen]byte
}

// EnvelopeHeaderInfo is the portion of an envelope that can be established
// without reading any encrypted chunks. HasKeyVersion can be true even when
// inspection returns ErrEnvelope: the header may claim a version but fail
// structural or authentication checks, and a retirement inventory must treat
// that claim conservatively.
type EnvelopeHeaderInfo struct {
	KeyVersion       uint32
	LogicalBytes     int64
	WrapSalt         [envelopeSaltLen]byte `json:"-"`
	HasEnvelopeMagic bool
	HasKeyVersion    bool
	HasLogicalBytes  bool
}

func (h *envelopeHeader) chunks() int64 {
	return max(1, (h.plainLen+h.chunkSize-1)/h.chunkSize)
}

// chunkLen is the plaintext length of chunk i.
func (h *envelopeHeader) chunkLen(i int64) int64 {
	if i == h.chunks()-1 {
		return h.plainLen - i*h.chunkSize
	}
	return h.chunkSize
}

func (h *envelopeHeader) objectLen() int64 {
	return envelopeHeaderLen + h.plainLen + h.chunks()*gcmTagLen
}

func (h *envelopeHeader) chunkOffset(i int64) int64 {
	return envelopeHeaderLen + i*(h.chunkSize+gcmTagLen)
}

func encodeHeader(keyVersion uint32, chunkSize, plainLen int64, salt []byte) envelopeHeader {
	h := envelopeHeader{keyVersion: keyVersion, chunkSize: chunkSize, plainLen: plainLen}
	copy(h.raw[0:4], envelopeMagic)
	binary.BigEndian.PutUint16(h.raw[4:6], envelopeVersion)
	binary.BigEndian.PutUint32(h.raw[6:10], keyVersion)
	binary.BigEndian.PutUint32(h.raw[10:14], uint32(chunkSize)) //nolint:gosec // bounded by MaxChunkSize
	binary.BigEndian.PutUint64(h.raw[14:22], uint64(plainLen))  //nolint:gosec // bounded by envelopeMaxPlain
	copy(h.raw[22:envelopeBoundLen], salt)
	return h
}

func parseHeader(raw []byte) (envelopeHeader, error) {
	var h envelopeHeader
	if len(raw) < envelopeHeaderLen {
		return h, fmt.Errorf("%w: object shorter than the header", ErrEnvelope)
	}
	copy(h.raw[:], raw)
	if string(h.raw[0:4]) != envelopeMagic {
		return h, fmt.Errorf("%w: not an envelope", ErrEnvelope)
	}
	if v := binary.BigEndian.Uint16(h.raw[4:6]); v != envelopeVersion {
		return h, fmt.Errorf("%w: unknown format version %d", ErrEnvelope, v)
	}
	h.keyVersion = binary.BigEndian.Uint32(h.raw[6:10])
	h.chunkSize = int64(binary.BigEndian.Uint32(h.raw[10:14]))
	if h.chunkSize < MinChunkSize || h.chunkSize > MaxChunkSize {
		return h, fmt.Errorf("%w: chunk size %d out of range", ErrEnvelope, h.chunkSize)
	}
	plain := binary.BigEndian.Uint64(h.raw[14:22])
	if plain > uint64(envelopeMaxPlain) {
		return h, fmt.Errorf("%w: plaintext length out of range", ErrEnvelope)
	}
	h.plainLen = int64(plain)
	return h, nil
}

// InspectEnvelopeHeader reads and authenticates only key's fixed-size header.
// It checks the envelope's declared physical length and, when the header is
// structurally valid, authenticates its wrapped data key against key and ring.
// It does not read or authenticate any encrypted chunk. A nil ring is accepted
// for plaintext stores, but a structurally valid envelope then returns
// ErrEnvelopeKeyUnavailable so a caller cannot mistake it for plaintext.
//
// physicalBytes comes from the backend's listing and lets inspection reject a
// truncated or extended object without reading its body.
func InspectEnvelopeHeader(
	ctx context.Context,
	backend Store,
	ring *KeyRing,
	key string,
	physicalBytes int64,
) (EnvelopeHeaderInfo, error) {
	var info EnvelopeHeaderInfo
	if backend == nil {
		return info, errors.New("envelope inspect: backend is required")
	}
	if err := ValidateKey(key); err != nil {
		return info, err
	}
	if physicalBytes < 0 {
		return info, fmt.Errorf("envelope inspect: negative physical size %d", physicalBytes)
	}
	var raw []byte
	if physicalBytes > 0 {
		// Limit the request to the available prefix. A truncated envelope may
		// still carry a readable key-version claim that must block retirement.
		readRaw, readErr := backend.ReadAt(ctx, key, 0, min(physicalBytes, int64(envelopeHeaderLen)))
		if readErr != nil {
			return info, fmt.Errorf("envelope inspect: read header: %w", readErr)
		}
		raw = readRaw
	}
	if len(raw) >= 4 && string(raw[:4]) == envelopeMagic {
		info.HasEnvelopeMagic = true
	}
	if len(raw) >= 10 && info.HasEnvelopeMagic && binary.BigEndian.Uint16(raw[4:6]) == envelopeVersion {
		info.KeyVersion = binary.BigEndian.Uint32(raw[6:10])
		info.HasKeyVersion = info.KeyVersion != 0
	}
	h, err := parseHeader(raw)
	if err != nil {
		return info, err
	}
	info.KeyVersion = h.keyVersion
	info.HasKeyVersion = h.keyVersion != 0
	info.LogicalBytes = h.plainLen
	copy(info.WrapSalt[:], h.raw[22:envelopeBoundLen])
	info.HasLogicalBytes = true
	if h.objectLen() != physicalBytes {
		return info, fmt.Errorf("%w: physical size does not match header", ErrEnvelope)
	}
	if ring == nil {
		return info, ErrEnvelopeKeyUnavailable
	}
	if _, ok := ring.key(h.keyVersion); !ok {
		return info, fmt.Errorf("%w: key version %d not in key ring", ErrEnvelope, h.keyVersion)
	}
	if _, err := (&Encrypted{ring: ring}).openDataKey(&h, key); err != nil {
		return info, err
	}
	return info, nil
}

// wrapAEAD returns the one-shot cipher that wraps this object's data key. The
// wrap key is derived from the key-encryption key and the per-write salt, so
// each derived key seals exactly one data key and the all-zero nonce is never
// reused under it.
func wrapAEAD(kek []byte, h *envelopeHeader) (cipher.AEAD, error) {
	kw, err := hkdf.Key(sha256.New, kek, h.raw[22:envelopeBoundLen], envelopeWrapInfo, dataKeyLen)
	if err != nil {
		return nil, fmt.Errorf("envelope wrap key: %w", err)
	}
	return newGCM(kw)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("envelope cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("envelope cipher: %w", err)
	}
	return aead, nil
}

// wrapAAD binds the header's fixed fields and salt to the object key.
func wrapAAD(h *envelopeHeader, key string) []byte {
	aad := make([]byte, 0, envelopeBoundLen+len(key))
	aad = append(aad, h.raw[:envelopeBoundLen]...)
	return append(aad, key...)
}

// chunkAAD writes chunk i's associated data into buf and returns it. The key
// version is left out on purpose: it is authenticated through the wrap, and
// leaving it out is what lets a rotation rewrap the data key without
// resealing a single chunk.
func chunkAAD(buf []byte, h *envelopeHeader, key string, i int64) []byte {
	buf = append(buf[:0], h.raw[0:6]...)
	buf = append(buf, h.raw[10:22]...)
	buf = binary.BigEndian.AppendUint64(buf, uint64(i)) //nolint:gosec // chunk index is never negative
	last := byte(0)
	if i == h.chunks()-1 {
		last = 1
	}
	buf = append(buf, last)
	return append(buf, key...)
}

func chunkNonce(nonce *[gcmNonceLen]byte, i int64) []byte {
	binary.BigEndian.PutUint64(nonce[4:], uint64(i)) //nolint:gosec // chunk index is never negative
	return nonce[:]
}

// openDataKey authenticates h against key and returns the data key's cipher.
func (e *Encrypted) openDataKey(h *envelopeHeader, key string) (cipher.AEAD, error) {
	dek, err := e.unwrapDataKey(h, key)
	if err != nil {
		return nil, err
	}
	defer clear(dek)
	return newGCM(dek)
}

func (e *Encrypted) unwrapDataKey(h *envelopeHeader, key string) ([]byte, error) {
	kek, ok := e.ring.key(h.keyVersion)
	if !ok {
		return nil, fmt.Errorf("%w: key version %d not in the ring", ErrEnvelope, h.keyVersion)
	}
	wrap, err := wrapAEAD(kek, h)
	if err != nil {
		return nil, err
	}
	var nonce [gcmNonceLen]byte
	dek, err := wrap.Open(nil, nonce[:], h.raw[envelopeBoundLen:], wrapAAD(h, key))
	if err != nil {
		return nil, fmt.Errorf("%w: header does not authenticate", ErrEnvelope)
	}
	return dek, nil
}

// Put seals r under key and returns the plaintext length stored. r must have a
// known length, the same as [S3.Put] requires: an [io.Seeker], or a Size or
// Len method. The sealed body handed to the backend has a known length too, so
// it is streamed a chunk at a time and never buffered, and a seekable source
// makes it replayable for a backend that retries. A source that turns out
// shorter or longer than it declared fails the Put.
func (e *Encrypted) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	if err := ValidateKey(key); err != nil {
		return 0, err
	}
	sr, err := e.newSealReader(key, r)
	if err != nil {
		return 0, err
	}
	var body io.Reader = sr
	if sr.seeker != nil {
		body = &seekableSealReader{sr}
	}
	n, err := e.backend.Put(ctx, key, body)
	if err != nil {
		return 0, err
	}
	if n != sr.total {
		return 0, fmt.Errorf("envelope put: backend wrote %d bytes, want %d", n, sr.total)
	}
	return sr.plainLen, nil
}

// sealReader produces an envelope from a source of known length, one chunk
// at a time. Its own lock is a leaf: nothing is called with it held except
// the source and the cipher. It is there because an HTTP transport may still
// be reading a body from an attempt it has given up on while the retry
// rewinds it, and an unserialised read racing the rewind could seal two
// plaintexts at one chunk index under one data key.
type sealReader struct {
	mu       sync.Mutex
	e        *Encrypted
	key      string
	src      io.Reader
	seeker   io.Seeker
	srcStart int64
	plainLen int64
	total    int64

	header  envelopeHeader
	aead    cipher.AEAD
	next    int64 // index of the next chunk to seal; chunks() once the source is checked
	pos     int64 // bytes of the envelope produced so far
	atEnd   bool  // a Seek to the end with no rewind since
	pending []byte
	plain   []byte
	sealed  []byte
	aad     []byte
	nonce   [gcmNonceLen]byte
	err     error
}

func (e *Encrypted) newSealReader(key string, r io.Reader) (*sealReader, error) {
	sr := &sealReader{e: e, key: key, src: r}
	switch src := r.(type) {
	case nil:
		return nil, errors.New("envelope put: nil source")
	case io.Seeker:
		start, err := src.Seek(0, io.SeekCurrent)
		if err != nil {
			return nil, fmt.Errorf("envelope put: source seek: %w", err)
		}
		end, err := src.Seek(0, io.SeekEnd)
		if err != nil {
			return nil, fmt.Errorf("envelope put: source seek: %w", err)
		}
		if _, err := src.Seek(start, io.SeekStart); err != nil {
			return nil, fmt.Errorf("envelope put: source seek: %w", err)
		}
		sr.seeker, sr.srcStart, sr.plainLen = src, start, end-start
	case interface{ Size() int64 }:
		sr.plainLen = src.Size()
	case interface{ Len() int }:
		sr.plainLen = int64(src.Len())
	default:
		return nil, errors.New("envelope put: source length is unknown")
	}
	if sr.plainLen < 0 || sr.plainLen > envelopeMaxPlain {
		return nil, fmt.Errorf("envelope put: source length %d out of range", sr.plainLen)
	}
	sr.plain = make([]byte, e.chunkSize)
	sr.sealed = make([]byte, 0, e.chunkSize+gcmTagLen)
	sr.aad = make([]byte, 0, envelopeChunkAADPf+len(key))
	if err := sr.start(); err != nil {
		return nil, err
	}
	sr.total = sr.header.objectLen()
	return sr, nil
}

// start begins a fresh envelope: a new data key and salt, so a rewound body
// never reseals a chunk index under a data key it has already used, even if
// the source changed between attempts.
func (sr *sealReader) start() error {
	var salt [envelopeSaltLen]byte
	if _, err := rand.Read(salt[:]); err != nil {
		return fmt.Errorf("envelope salt: %w", err)
	}
	dek := make([]byte, dataKeyLen)
	if _, err := rand.Read(dek); err != nil {
		return fmt.Errorf("envelope data key: %w", err)
	}
	current := sr.e.ring.Current()
	kek, ok := sr.e.ring.key(current)
	if !ok || len(kek) != MediaKeyLen {
		return errors.New("envelope put: key ring has no current key")
	}
	h := encodeHeader(current, int64(sr.e.chunkSize), sr.plainLen, salt[:])
	wrap, err := wrapAEAD(kek, &h)
	if err != nil {
		return err
	}
	var nonce [gcmNonceLen]byte
	wrap.Seal(h.raw[envelopeBoundLen:envelopeBoundLen], nonce[:], dek, wrapAAD(&h, sr.key))
	aead, err := newGCM(dek)
	if err != nil {
		return err
	}
	sr.header, sr.aead = h, aead
	sr.next, sr.pos, sr.atEnd, sr.err = 0, 0, false, nil
	sr.pending = sr.header.raw[:]
	return nil
}

// Size reports the sealed length, which is what lets a backend send the body
// with a known Content-Length.
func (sr *sealReader) Size() int64 { return sr.total }

func (sr *sealReader) Read(p []byte) (int, error) {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	if sr.atEnd {
		return 0, io.EOF
	}
	for len(sr.pending) == 0 {
		if sr.err != nil {
			return 0, sr.err
		}
		sr.err = sr.fill()
	}
	n := copy(p, sr.pending)
	sr.pending = sr.pending[n:]
	sr.pos += int64(n)
	return n, nil
}

// fill seals the next chunk into pending, or checks the source is exhausted
// once every chunk is sealed. Its error is sticky.
func (sr *sealReader) fill() error {
	h := &sr.header
	if sr.next == h.chunks() {
		return io.EOF
	}
	if sr.next == h.chunks()-1 {
		if err := sr.sealChunk(); err != nil {
			return err
		}
		// The last chunk is withheld until the source is proven exhausted. A
		// transport stops reading at the declared length, so an error that
		// surfaced only after the last chunk would never reach it, and the
		// backend would store a complete envelope of a truncated source.
		var probe [1]byte
		n, err := io.ReadFull(sr.src, probe[:])
		if n > 0 {
			sr.pending = nil
			return errors.New("envelope put: source longer than its declared length")
		}
		if !errors.Is(err, io.EOF) {
			sr.pending = nil
			return fmt.Errorf("envelope put: source: %w", err)
		}
		return nil
	}
	return sr.sealChunk()
}

func (sr *sealReader) sealChunk() error {
	h := &sr.header
	i := sr.next
	plain := sr.plain[:h.chunkLen(i)]
	if _, err := io.ReadFull(sr.src, plain); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return errors.New("envelope put: source shorter than its declared length")
		}
		return fmt.Errorf("envelope put: source: %w", err)
	}
	sr.sealed = sr.aead.Seal(sr.sealed[:0], chunkNonce(&sr.nonce, i), plain, chunkAAD(sr.aad, h, sr.key, i))
	sr.pending = sr.sealed
	sr.next++
	return nil
}

// seekableSealReader is a sealReader over a seekable source, so a backend
// that retries can rewind it. It supports exactly the seeks a length probe
// and a rewind make: reporting the position, moving to the end, and returning
// to the start, which begins a fresh envelope under a new data key.
type seekableSealReader struct{ *sealReader }

func (s *seekableSealReader) Seek(offset int64, whence int) (int64, error) {
	sr := s.sealReader
	sr.mu.Lock()
	defer sr.mu.Unlock()
	switch {
	case offset == 0 && whence == io.SeekCurrent:
		if sr.atEnd {
			return sr.total, nil
		}
		return sr.pos, nil
	case offset == 0 && whence == io.SeekEnd:
		sr.atEnd = true
		return sr.total, nil
	case offset == 0 && whence == io.SeekStart:
		if _, err := sr.seeker.Seek(sr.srcStart, io.SeekStart); err != nil {
			return 0, fmt.Errorf("envelope put: source seek: %w", err)
		}
		if err := sr.start(); err != nil {
			return 0, err
		}
		return 0, nil
	default:
		return 0, errors.New("envelope put: unsupported seek")
	}
}

// ReadAt returns at most limit plaintext bytes of key starting at plaintext
// offset. It fetches the header, then exactly the chunks the window overlaps,
// and authenticates every byte it returns at its position. A window reaching
// the last chunk reads one byte past it, so data appended to the object is
// caught. A window over intact chunks of an object damaged elsewhere still
// reads: proving the whole object intact would mean fetching all of it.
func (e *Encrypted) ReadAt(ctx context.Context, key string, offset, limit int64) ([]byte, error) {
	if err := ValidateKey(key); err != nil {
		return nil, err
	}
	if offset < 0 || limit < 0 {
		return nil, fmt.Errorf("blob read: negative window offset=%d limit=%d", offset, limit)
	}
	if limit == 0 {
		return []byte{}, nil
	}
	raw, err := e.backend.ReadAt(ctx, key, 0, envelopeHeaderLen)
	if err != nil {
		return nil, err
	}
	h, err := parseHeader(raw)
	if err != nil {
		return nil, err
	}
	aead, err := e.openDataKey(&h, key)
	if err != nil {
		return nil, err
	}
	var end, first, last int64
	switch {
	case h.plainLen == 0:
		// An empty object's one chunk is a bare tag no window overlaps, so
		// every read authenticates it; otherwise nothing would.
		offset = 0
	case offset >= h.plainLen:
		return []byte{}, nil
	default:
		end = offset + min(limit, h.plainLen-offset)
		first, last = offset/h.chunkSize, (end-1)/h.chunkSize
	}
	from := h.chunkOffset(first)
	want := h.chunkOffset(last) + h.chunkLen(last) + gcmTagLen - from
	fetch := want
	if last == h.chunks()-1 {
		fetch++
	}
	sealed, err := e.backend.ReadAt(ctx, key, from, fetch)
	if err != nil {
		return nil, err
	}
	if int64(len(sealed)) < want {
		return nil, fmt.Errorf("%w: object truncated", ErrEnvelope)
	}
	if int64(len(sealed)) > want {
		return nil, fmt.Errorf("%w: data after the last chunk", ErrEnvelope)
	}

	out := make([]byte, 0, end-offset)
	var nonce [gcmNonceLen]byte
	aad := make([]byte, 0, envelopeChunkAADPf+len(key))
	plain := make([]byte, 0, h.chunkSize)
	for i := first; i <= last; i++ {
		n := h.chunkLen(i) + gcmTagLen
		var err error
		plain, err = aead.Open(plain[:0], chunkNonce(&nonce, i), sealed[:n], chunkAAD(aad, &h, key, i))
		if err != nil {
			return nil, fmt.Errorf("%w: chunk %d does not authenticate", ErrEnvelope, i)
		}
		sealed = sealed[n:]
		lo := max(offset-i*h.chunkSize, 0)
		hi := min(end-i*h.chunkSize, int64(len(plain)))
		out = append(out, plain[lo:hi]...)
	}
	return out, nil
}

// Remove deletes key from the backend.
func (e *Encrypted) Remove(ctx context.Context, key string) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	return e.backend.Remove(ctx, key)
}

// WalkPrefix enumerates the backend under prefix. Entry sizes are physical.
func (e *Encrypted) WalkPrefix(ctx context.Context, prefix string, fn func(Entry) error) error {
	return e.backend.WalkPrefix(ctx, prefix, fn)
}

var _ Store = (*Encrypted)(nil)
