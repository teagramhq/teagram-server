package blob

import (
	"bytes"
	"context"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
)

// RotationPlan is a per-object rotation intent. Its salt is public envelope
// metadata; keeping the plan tied to the storage key and target version lets
// an operator journal it before replacing the object and resume after a stop.
type RotationPlan struct {
	key           string
	targetVersion uint32
	rewrite       bool
	salt          [envelopeSaltLen]byte
	used          *atomic.Bool
}

// SaltID returns the canonical journal-safe identifier for this plan's salt.
func (p RotationPlan) SaltID() string { return hex.EncodeToString(p.salt[:]) }

// PrepareRotation creates a fresh, per-object wrapping salt for the current
// key version. A separate plan is required for every object because the salt
// derives the one-shot key that wraps that object's data key.
func (e *Encrypted) PrepareRotation(key string, rewrite bool) (RotationPlan, error) {
	if err := ValidateKey(key); err != nil {
		return RotationPlan{}, err
	}
	if e == nil || e.backend == nil || e.ring == nil {
		return RotationPlan{}, errors.New("envelope rotation: encrypted store is required")
	}
	if _, ok := e.ring.key(e.ring.Current()); !ok || e.ring.Current() == 0 {
		return RotationPlan{}, errors.New("envelope rotation: key ring has no current key")
	}
	plan := RotationPlan{key: key, targetVersion: e.ring.Current(), rewrite: rewrite, used: new(atomic.Bool)}
	if _, err := rand.Read(plan.salt[:]); err != nil {
		return RotationPlan{}, fmt.Errorf("envelope rotation salt: %w", err)
	}
	return plan, nil
}

// Rotate replaces one envelope under the ring's current version. Rewrap mode
// writes a new authenticated header and copies each ciphertext chunk verbatim;
// it never opens chunk payloads. Rewrite mode decrypts and seals each chunk
// under a fresh per-object data key. Both modes stream one chunk at a time.
// Store.Put replaces the object as one write, so readers see the intact old or
// new envelope while the pass runs.
func (e *Encrypted) Rotate(ctx context.Context, plan RotationPlan, physicalBytes int64) error {
	if e == nil || e.backend == nil || e.ring == nil {
		return errors.New("envelope rotation: encrypted store is required")
	}
	if err := ValidateKey(plan.key); err != nil {
		return err
	}
	if plan.targetVersion == 0 || plan.targetVersion != e.ring.Current() {
		return errors.New("envelope rotation: plan target is not the current key version")
	}
	if plan.used == nil || !plan.used.CompareAndSwap(false, true) {
		return errors.New("envelope rotation: rotation plan has already been used")
	}
	if physicalBytes < envelopeHeaderLen {
		return fmt.Errorf("%w: object shorter than the header", ErrEnvelope)
	}
	raw, err := e.backend.ReadAt(ctx, plan.key, 0, envelopeHeaderLen)
	if err != nil {
		return fmt.Errorf("envelope rotation: read source header: %w", err)
	}
	source, err := parseHeader(raw)
	if err != nil {
		return err
	}
	if source.objectLen() != physicalBytes {
		return fmt.Errorf("%w: physical size does not match header", ErrEnvelope)
	}
	if ParsePartKey(plan.key) && source.plainLen > MaxPartBytes {
		return fmt.Errorf("%w: part plaintext length %d exceeds maximum %d", ErrEnvelope, source.plainLen, MaxPartBytes)
	}
	if source.keyVersion == plan.targetVersion && (!plan.rewrite || sourceHasSalt(source, plan.salt)) {
		return nil
	}
	oldDEK, err := e.unwrapDataKey(&source, plan.key)
	if err != nil {
		return err
	}
	defer clear(oldDEK)

	newDEK := oldDEK
	if plan.rewrite {
		newDEK = make([]byte, dataKeyLen)
		if _, err := rand.Read(newDEK); err != nil {
			clear(newDEK)
			return fmt.Errorf("envelope rotation data key: %w", err)
		}
		defer clear(newDEK)
	}
	target, err := makeRotationHeader(e.ring, source, plan.key, plan.targetVersion, plan.salt, newDEK)
	if err != nil {
		return err
	}
	var sourceAEAD, targetAEAD cipher.AEAD
	if plan.rewrite {
		sourceAEAD, err = newGCM(oldDEK)
		if err != nil {
			return err
		}
		targetAEAD, err = newGCM(newDEK)
		if err != nil {
			return err
		}
	}
	body := &rotationBody{
		ctx: ctx, backend: e.backend, key: plan.key,
		source: source, target: target,
		sourceAEAD: sourceAEAD, targetAEAD: targetAEAD,
		rewrite: plan.rewrite,
	}
	var replacement io.Reader = body
	if ParsePartKey(plan.key) {
		replacement, err = boundedPartRotationBody(body)
		if err != nil {
			return err
		}
	}
	written, err := e.backend.Put(ctx, plan.key, replacement)
	if err != nil {
		return fmt.Errorf("envelope rotation: write replacement: %w", err)
	}
	if written != physicalBytes {
		return fmt.Errorf("envelope rotation: backend wrote %d bytes, want %d", written, physicalBytes)
	}
	return nil
}

// boundedPartRotationBody materializes only part envelopes, whose plaintext is
// capped by MaxPartBytes. The local orphan reclaim pass relies on every writer
// under parts/ finishing within that bounded input size.
func boundedPartRotationBody(body *rotationBody) (io.Reader, error) {
	maxChunks := (int64(MaxPartBytes) + int64(MinChunkSize) - 1) / int64(MinChunkSize)
	maxBytes := int64(envelopeHeaderLen) + int64(MaxPartBytes) + maxChunks*gcmTagLen
	if body.Size() > maxBytes {
		return nil, fmt.Errorf("envelope rotation: part envelope size %d exceeds maximum %d", body.Size(), maxBytes)
	}
	data, err := io.ReadAll(io.LimitReader(body, maxBytes+1))
	if err != nil {
		clear(data)
		return nil, fmt.Errorf("envelope rotation: buffer bounded part: %w", err)
	}
	if int64(len(data)) != body.Size() {
		clear(data)
		return nil, fmt.Errorf("envelope rotation: buffered part has %d bytes, want %d", len(data), body.Size())
	}
	return bytes.NewReader(data), nil
}

func sourceHasSalt(h envelopeHeader, salt [envelopeSaltLen]byte) bool {
	for i, b := range salt {
		if h.raw[22+i] != b {
			return false
		}
	}
	return true
}

func makeRotationHeader(ring *KeyRing, source envelopeHeader, key string, targetVersion uint32, salt [envelopeSaltLen]byte, dek []byte) (envelopeHeader, error) {
	kek, ok := ring.key(targetVersion)
	if !ok || len(kek) != MediaKeyLen {
		return envelopeHeader{}, errors.New("envelope rotation: key ring has no current key")
	}
	target := encodeHeader(targetVersion, source.chunkSize, source.plainLen, salt[:])
	wrap, err := wrapAEAD(kek, &target)
	if err != nil {
		return envelopeHeader{}, err
	}
	var nonce [gcmNonceLen]byte
	wrap.Seal(target.raw[envelopeBoundLen:envelopeBoundLen], nonce[:], dek, wrapAAD(&target, key))
	return target, nil
}

type rotationBody struct {
	ctx        context.Context
	backend    Store
	key        string
	source     envelopeHeader
	target     envelopeHeader
	sourceAEAD cipher.AEAD
	targetAEAD cipher.AEAD
	rewrite    bool
	position   int64
	nextChunk  int64
	pending    []byte
	atEnd      bool
}

// Size gives S3 a known Content-Length. The stateful stream deliberately does
// not implement io.Seeker, because a retry could rewind while an earlier HTTP
// attempt is still reading it.
func (r *rotationBody) Size() int64 { return r.target.objectLen() }

func (r *rotationBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.atEnd {
		return 0, io.EOF
	}
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if r.position < envelopeHeaderLen {
		n := copy(p, r.target.raw[r.position:])
		r.position += int64(n)
		return n, nil
	}
	for len(r.pending) == 0 {
		if r.nextChunk == r.source.chunks() {
			return 0, io.EOF
		}
		if err := r.readChunk(); err != nil {
			return 0, err
		}
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	r.position += int64(n)
	return n, nil
}

func (r *rotationBody) readChunk() error {
	i := r.nextChunk
	chunkBytes := r.source.chunkLen(i) + gcmTagLen
	sealed, err := r.backend.ReadAt(r.ctx, r.key, r.source.chunkOffset(i), chunkBytes)
	if err != nil {
		return fmt.Errorf("envelope rotation: read source chunk %d: %w", i, err)
	}
	if int64(len(sealed)) != chunkBytes {
		return fmt.Errorf("envelope rotation: source chunk %d has %d bytes, want %d", i, len(sealed), chunkBytes)
	}
	if !r.rewrite {
		r.pending = sealed
		r.nextChunk++
		return nil
	}
	var nonce [gcmNonceLen]byte
	plain, err := r.sourceAEAD.Open(nil, chunkNonce(&nonce, i), sealed, chunkAAD(nil, &r.source, r.key, i))
	if err != nil {
		return fmt.Errorf("%w: source chunk %d does not authenticate", ErrEnvelope, i)
	}
	r.pending = r.targetAEAD.Seal(nil, chunkNonce(&nonce, i), plain, chunkAAD(nil, &r.target, r.key, i))
	clear(plain)
	r.nextChunk++
	return nil
}

var _ interface{ Size() int64 } = (*rotationBody)(nil)
