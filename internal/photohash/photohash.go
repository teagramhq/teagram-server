// Package photohash derives the viewer-bound profile-photo access hash a client
// carries back to name one gallery photo: a keyed MAC over a versioned label
// and the (viewer, owner, file) triple, truncated to 64 bits.
//
// It is a separate capability domain from internal/peerhash, not a new peer
// kind. Its own HKDF expansion of the master key material makes cross-protocol
// substitution explicit and independently testable: a peer access_hash, a
// message photo's raw files.access_hash and a photo hash issued to another
// viewer all fail verification here. Binding the owner as well as the caller is
// what makes a copied gallery entry inert — the hash a viewer was shown names
// that owner's file for that viewer only.
//
// The derivation is stateless: nothing is stored, and any process holding the
// same key material reproduces it. Only the subkey produced by Subkey may reach
// RPC code; the master key's reach stays what it is today, which is
// storage. Subkey returns the derived subkey; Deriver.Format redacts the key
// material held by a Deriver when it is rendered.
//
// # Key rotation constraint
//
// Like the peer hash, this derivation carries no key epoch, so a change of
// master key material invalidates every photo hash every live client holds. That
// is free today and only today: rotating the master is already a total
// re-authentication event, so photo capabilities expire in that same event and
// clients refetch. There is deliberately no independent photo-subkey rotation
// and no accept-previous window.
//
// THE CONSTRAINT THIS PLACES ON WHOEVER IMPLEMENTS KEY ROTATION: the day
// keycrypt gains dual-key rotation — any scheme where a live session survives a
// master key change — the profile-photo hash must gain an epoch or an
// accept-previous window in that same change, together with the peer hash.
// Accepting either key is only ever sound with a fresh live authorization check.
package photohash

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
)

const (
	// SubkeyLen is the frozen 32-byte length of the derived subkey.
	SubkeyLen = 32
	// masterLen is the master key length Subkey requires. It matches
	// keycrypt.KeyLen, but is stated here rather than imported: this package
	// must not pull the storage cipher into the RPC layer's dependency graph.
	masterLen = 32
	// macLen is how many MAC bytes become the wire access_hash.
	macLen = 8
	// subkeyInfo domain-separates the profile-photo subkey from the peer-hash
	// subkey and from every other use of the master key material. Changing it
	// invalidates every issued photo hash.
	subkeyInfo = "telegram-server/profile-photo-access-hash/subkey/v1"
	// label prefixes the MAC input, versioning the construction itself. The
	// fields after it are all fixed-width, so the encoding is unambiguous
	// without a length prefix.
	label = "tg-profile-photo-access-hash-v1"
)

// Subkey derives the profile-photo subkey from the master key material
// at process start. Only the result may travel to the RPC layer.
func Subkey(master []byte) ([]byte, error) {
	if len(master) != masterLen {
		return nil, fmt.Errorf("photohash: master key must be %d bytes, got %d", masterLen, len(master))
	}
	// No salt: the master key is already full-entropy 32-byte key material, so
	// this is a domain-separating expansion, not a randomness extraction.
	sub, err := hkdf.Key(sha256.New, master, nil, subkeyInfo, SubkeyLen)
	if err != nil {
		return nil, fmt.Errorf("photohash: derive subkey: %w", err)
	}
	return sub, nil
}

// Deriver issues profile-photo access hashes under one subkey. It is immutable
// after construction and safe for concurrent use.
type Deriver struct {
	key []byte
}

// New builds a Deriver from a subkey produced by Subkey. It fails fast on a
// wrong-length key so a misconfigured server never starts issuing hashes under
// weak key material, and it copies the subkey so the caller's buffer cannot be
// reused underneath a live Deriver.
func New(subkey []byte) (*Deriver, error) {
	if len(subkey) != SubkeyLen {
		return nil, fmt.Errorf("photohash: subkey must be %d bytes, got %d", SubkeyLen, len(subkey))
	}
	return &Deriver{key: append([]byte(nil), subkey...)}, nil
}

// Derive returns the access_hash the viewer carries for this gallery
// photo. It is the single issuing entry point: a hash issued through it
// verifies through Verify, and no other construction of a profile-photo
// access_hash may exist in the server.
func (d Deriver) Derive(viewerID, ownerID, fileID int64) int64 {
	return d.mac(viewerID, ownerID, fileID)
}

// Verify reports whether candidate is the hash this Deriver issues for the
// (viewer, owner, file) triple. It is the single checking entry point, and a
// caller must still recheck live ownership and the viewer block policy
// separately: the MAC proves authorized issuance to this viewer, it never waives
// a fresh authorization. The comparison is one constant-time compare over all
// eight MAC bytes, so no byte position is short-circuited.
func (d Deriver) Verify(viewerID, ownerID, fileID, candidate int64) bool {
	var want, have [macLen]byte
	putInt64(want[:], d.mac(viewerID, ownerID, fileID))
	putInt64(have[:], candidate)
	return hmac.Equal(want[:], have[:])
}

// mac is the frozen construction: the label, then viewer, owner and file as
// fixed-width big-endian int64, keyed by the subkey, truncated to the leading
// macLen bytes.
func (d Deriver) mac(viewerID, ownerID, fileID int64) int64 {
	var msg [len(label) + 8 + 8 + 8]byte
	n := copy(msg[:], label)
	putInt64(msg[n:], viewerID)
	putInt64(msg[n+8:], ownerID)
	putInt64(msg[n+16:], fileID)

	mac := hmac.New(sha256.New, d.key)
	// hash.Hash never returns an error from Write.
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	// Truncate to 64 bits by taking the leading eight bytes. access_hash is an
	// int64 on the wire and every bit pattern is a legal value, so the whole
	// range is used rather than masked into a positive half.
	return int64(binary.BigEndian.Uint64(sum[:macLen])) //nolint:gosec // reinterpreting all 64 bits, not narrowing
}

// Format keeps key material out of every rendering of a Deriver, including %#v,
// which ignores String. The Deriver is what travels to the RPC layer, where a
// stray %v in a log line must not disclose the subkey it holds.
func (d Deriver) Format(s fmt.State, verb rune) {
	// fmt.State reports write errors that a Formatter cannot act on: the only
	// writer here is fmt's own buffer.
	_, _ = io.WriteString(s, "photohash.Deriver{subkey: redacted}") //nolint:errcheck // fmt.State writes have no recovery path
}

// putInt64 writes v big-endian into b. The unsigned conversion reinterprets the
// two's-complement bits rather than narrowing them, so no value is lost and no
// two ids share an encoding — which is what the derivation needs.
func putInt64(b []byte, v int64) {
	binary.BigEndian.PutUint64(b, uint64(v)) //nolint:gosec // reinterpreting all 64 bits, not narrowing
}
