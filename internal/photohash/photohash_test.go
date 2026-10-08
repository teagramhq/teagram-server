package photohash_test

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"

	"github.com/teagramhq/teagram-server/internal/peerhash"
	"github.com/teagramhq/teagram-server/internal/photohash"
)

// master keys used across the table. They are test material only.
var (
	masterA = []byte("0123456789abcdef0123456789abcdef")
	masterB = []byte("fedcba9876543210fedcba9876543210")
)

func subkeyOf(t *testing.T, master []byte) []byte {
	t.Helper()
	sub, err := photohash.Subkey(master)
	if err != nil {
		t.Fatalf("Subkey: %v", err)
	}
	return sub
}

func deriver(t *testing.T, master []byte) *photohash.Deriver {
	t.Helper()
	d, err := photohash.New(subkeyOf(t, master))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d
}

// TestSubkeyVector pins the derived subkey to a fixed value. It is the domain
// label and the salt choice frozen in one number: changing the HKDF info string,
// adding a salt or changing the output length moves it, which is exactly the
// point — a photo hash issued by one process must verify in the next one.
func TestSubkeyVector(t *testing.T) {
	t.Parallel()

	const want = "775be6f3071b996227695a023b0c71835c997912a761b811b157ec7f4df4893e"

	if got := hex.EncodeToString(subkeyOf(t, masterA)); got != want {
		t.Fatalf("Subkey(masterA) = %s, want %s", got, want)
	}
}

// TestSubkeySeparatedFromPeerSubkey is the key half of domain separation: the
// profile-photo subkey is its own HKDF expansion of the same master, so no
// process holding one subkey can be handed the other by accident.
func TestSubkeySeparatedFromPeerSubkey(t *testing.T) {
	t.Parallel()

	peerSub, err := peerhash.Subkey(masterA)
	if err != nil {
		t.Fatalf("peerhash.Subkey: %v", err)
	}
	photoSub := subkeyOf(t, masterA)
	if string(peerSub) == string(photoSub) {
		t.Fatal("profile-photo subkey and peer subkey are the same key")
	}
}

// TestSubkeyNotMaster checks the subkey is not the master key handed on under
// another name: only the subkey may reach the RPC layer, so a caller holding it
// must not be holding the storage key.
func TestSubkeyNotMaster(t *testing.T) {
	t.Parallel()

	sub := subkeyOf(t, masterA)
	if string(sub) == string(masterA) {
		t.Fatal("Subkey returned the master key unchanged")
	}
	if string(sub) != string(subkeyOf(t, masterA)) {
		t.Fatal("Subkey is not deterministic")
	}
}

func TestSubkeyRejectsShortMaster(t *testing.T) {
	t.Parallel()

	if _, err := photohash.Subkey(masterA[:31]); err == nil {
		t.Fatal("Subkey accepted a 31-byte master key")
	}
	if _, err := photohash.Subkey(append(masterA, 0)); err == nil {
		t.Fatal("Subkey accepted a 33-byte master key")
	}
}

func TestNewRejectsShortSubkey(t *testing.T) {
	t.Parallel()

	if _, err := photohash.New(make([]byte, photohash.SubkeyLen-1)); err == nil {
		t.Fatal("New accepted a short subkey")
	}
}

// TestNewCopiesSubkey keeps the master's reach where it is: the Deriver holds
// its own copy, so a caller that zeroes or reuses its subkey buffer cannot
// silently change the hashes an already-built Deriver issues.
func TestNewCopiesSubkey(t *testing.T) {
	t.Parallel()

	const want = 1411330975392603422

	sub := subkeyOf(t, masterA)
	d, err := photohash.New(sub)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	clear(sub)
	if got := d.Derive(1, 2, 3); got != want {
		t.Fatalf("Derive after the caller zeroed its subkey = %d, want %d", got, want)
	}
}

// TestDeriveStable pins the construction to a fixed vector for the synthetic
// triple (viewer=1, owner=2, file=3). A change to the label, the field order,
// the field widths or the truncation moves this number.
func TestDeriveStable(t *testing.T) {
	t.Parallel()

	const want = 1411330975392603422

	d := deriver(t, masterA)
	if got := d.Derive(1, 2, 3); got != want {
		t.Fatalf("Derive(1, 2, 3) = %d, want %d", got, want)
	}
	// Same inputs through an independently constructed Deriver: the value is a
	// function of the key material alone, with no per-process state.
	if got := deriver(t, masterA).Derive(1, 2, 3); got != want {
		t.Fatalf("second Deriver = %d, want %d", got, want)
	}
}

// TestDeriveFieldOrderFrozen pins the MAC input order viewer, owner, file. Each
// permutation of the same three ids is a distinct frozen value, so a caller that
// swaps two arguments produces a wrong hash rather than the right one.
func TestDeriveFieldOrderFrozen(t *testing.T) {
	t.Parallel()

	d := deriver(t, masterA)
	for _, tc := range []struct {
		viewer, owner, file int64
		want                int64
	}{
		{1, 2, 3, 1411330975392603422},
		{2, 1, 3, -6589232073781708695},
		{1, 3, 2, -1034726634562230817},
		{3, 2, 1, -7405676070799531886},
	} {
		if got := d.Derive(tc.viewer, tc.owner, tc.file); got != tc.want {
			t.Fatalf("Derive(%d, %d, %d) = %d, want %d", tc.viewer, tc.owner, tc.file, got, tc.want)
		}
	}
}

// TestDeriveFixedWidthEncoding pins the pair that a length-free variable-width
// encoding would collide: (1,23,0) and (12,3,0) both concatenate to the same
// digit string. Fixed-width big-endian fields keep them
// apart, and both values are frozen.
func TestDeriveFixedWidthEncoding(t *testing.T) {
	t.Parallel()

	d := deriver(t, masterA)
	ambiguous := []struct {
		viewer, owner, file int64
		want                int64
	}{
		{1, 23, 0, 6744891419605748551},
		{12, 3, 0, 7404751503129697295},
	}
	seen := make(map[int64]string, len(ambiguous))
	for _, tc := range ambiguous {
		got := d.Derive(tc.viewer, tc.owner, tc.file)
		if got != tc.want {
			t.Fatalf("Derive(%d, %d, %d) = %d, want %d", tc.viewer, tc.owner, tc.file, got, tc.want)
		}
		if prev, dup := seen[got]; dup {
			t.Fatalf("%s and (%d,%d,%d) both derived %d", prev, tc.viewer, tc.owner, tc.file, got)
		}
		seen[got] = fmt.Sprintf("(%d,%d,%d)", tc.viewer, tc.owner, tc.file)
	}
}

// TestDeriveIDBoundaries pins the encodings a signed or saturating
// conversion would lose: zero, -1 and both int64 extremes are distinct frozen
// values, so no id collapses onto another.
func TestDeriveIDBoundaries(t *testing.T) {
	t.Parallel()

	d := deriver(t, masterA)
	for _, tc := range []struct {
		viewer, owner, file int64
		want                int64
	}{
		{0, 0, 0, -3599100594644421700},
		{-1, -1, -1, 9154841438189190163},
		{math.MinInt64, math.MinInt64, math.MinInt64, -9101892917434188891},
		{math.MaxInt64, math.MaxInt64, math.MaxInt64, 8961419703841341104},
		{math.MinInt64, 2, 3, 8570382911632661554},
		{math.MaxInt64, 2, 3, 8933735497204470906},
		{1, 2, math.MinInt64, 8818455464707680865},
		{1, 2, math.MaxInt64, 5468522439881205622},
	} {
		if got := d.Derive(tc.viewer, tc.owner, tc.file); got != tc.want {
			t.Fatalf("Derive(%d, %d, %d) = %d, want %d", tc.viewer, tc.owner, tc.file, got, tc.want)
		}
	}
}

// TestDeriveSignedWireBits covers the truncation rule: the leading 64 bits are
// reinterpreted as the signed wire access_hash, not masked into the positive
// half. A masking implementation keeps every pinned negative vector positive.
func TestDeriveSignedWireBits(t *testing.T) {
	t.Parallel()

	d := deriver(t, masterA)
	const want = -1792590408147344533

	if got := d.Derive(1, 2, 2); got != want {
		t.Fatalf("Derive(1, 2, 2) = %d, want %d", got, want)
	}
	if got := d.Derive(1, 2, 2); got >= 0 {
		t.Fatalf("Derive(1, 2, 2) = %d; the high bit was masked away", got)
	}
	// Both halves of the int64 range are reachable, so the wire value is not
	// confined to a positive subset that would shrink the collision space.
	var neg, pos int
	for file := range int64(200) {
		if v := d.Derive(1, 2, file); v < 0 {
			neg++
		} else {
			pos++
		}
	}
	if neg == 0 || pos == 0 {
		t.Fatalf("200 derivations produced %d negative and %d positive values", neg, pos)
	}
}

// TestDeriveRejectsPeerSubkey shows the subkey domain label is load-bearing: a
// Deriver holding the peer subkey issues different values, so a photo hash and
// a peer hash never share a key even under one master.
func TestDeriveRejectsPeerSubkey(t *testing.T) {
	t.Parallel()

	peerSub, err := peerhash.Subkey(masterA)
	if err != nil {
		t.Fatalf("peerhash.Subkey: %v", err)
	}
	underPeer, err := photohash.New(peerSub)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got, want := underPeer.Derive(1, 2, 3), deriver(t, masterA).Derive(1, 2, 3); got == want {
		t.Fatalf("peer subkey produced the profile-photo hash %d", got)
	}
}

// TestVerifyAcceptsIssuedHash is the round trip the gallery lane depends on:
// the hash Derive issues for a triple verifies for that same triple.
func TestVerifyAcceptsIssuedHash(t *testing.T) {
	t.Parallel()

	d := deriver(t, masterA)
	if !d.Verify(1, 2, 3, d.Derive(1, 2, 3)) {
		t.Fatal("Verify rejected the hash Derive issued for (1, 2, 3)")
	}
	// A second Deriver over the same master key material accepts it too: the
	// credential is stateless, so any replica holding the master reproduces it.
	if !deriver(t, masterA).Verify(1, 2, 3, d.Derive(1, 2, 3)) {
		t.Fatal("an independent Deriver rejected the issued hash")
	}
}

// TestVerifyRejectsIdentityChange is the security property: the hash is bound to
// the (viewer, owner, file) triple, so a copied gallery entry, a re-uploaded
// file id or a different viewer never carries an existing hash over.
func TestVerifyRejectsIdentityChange(t *testing.T) {
	t.Parallel()

	d := deriver(t, masterA)
	issued := d.Derive(1, 2, 3)
	for _, tc := range []struct {
		name              string
		viewer, owner, fi int64
	}{
		{"viewer", 9, 2, 3},
		{"owner", 1, 9, 3},
		{"file", 1, 2, 9},
		{"viewer zero", 0, 2, 3},
		{"owner zero", 1, 0, 3},
		{"file zero", 1, 2, 0},
		{"all swapped", 2, 1, 3},
	} {
		if d.Verify(tc.viewer, tc.owner, tc.fi, issued) {
			t.Fatalf("Verify accepted the hash for (1,2,3) under %s = (%d,%d,%d)", tc.name, tc.viewer, tc.owner, tc.fi)
		}
	}
}

// TestVerifyRejectsOtherMaster pins the rotation contract: photo hashes are a
// pure function of the subkey, so a master rotation invalidates every issued
// hash at once. There is no accept-previous window to introduce by accident.
func TestVerifyRejectsOtherMaster(t *testing.T) {
	t.Parallel()

	before := deriver(t, masterA)
	after := deriver(t, masterB)
	if after.Verify(1, 2, 3, before.Derive(1, 2, 3)) {
		t.Fatal("a Deriver under a different master accepted the old hash")
	}
	if before.Verify(1, 2, 3, after.Derive(1, 2, 3)) {
		t.Fatal("a Deriver under the old master accepted the new hash")
	}
}

// TestVerifyRejectsPeerHashSubstitution is the cross-lane case: a peer access
// hash derived from the same master, for any kind and for any pairing of the
// same three ids, is not a profile-photo credential.
func TestVerifyRejectsPeerHashSubstitution(t *testing.T) {
	t.Parallel()

	photo := deriver(t, masterA)
	peers := func(viewer int64, kind peerhash.Kind, peer int64) int64 {
		sub, err := peerhash.Subkey(masterA)
		if err != nil {
			t.Fatalf("peerhash.Subkey: %v", err)
		}
		d, err := peerhash.New(sub)
		if err != nil {
			t.Fatalf("peerhash.New: %v", err)
		}
		return d.Derive(viewer, kind, peer)
	}
	kinds := []peerhash.Kind{peerhash.KindUser, peerhash.KindChat, peerhash.KindChannel, peerhash.KindSecret}
	triples := [][3]int64{{1, 2, 3}, {2, 1, 3}, {1, 3, 2}, {3, 2, 1}}
	for _, kind := range kinds {
		for _, tr := range triples {
			for _, pair := range [][2]int64{{tr[0], tr[1]}, {tr[1], tr[2]}, {tr[0], tr[2]}} {
				h := peers(pair[0], kind, pair[1])
				if photo.Verify(tr[0], tr[1], tr[2], h) {
					t.Fatalf("peer hash kind %d for (%d,%d) verified as a photo hash for (%d,%d,%d)",
						kind, pair[0], pair[1], tr[0], tr[1], tr[2])
				}
			}
		}
	}
}

// TestVerifyRejectsRawFileHash covers the other two substitutes. A file's
// own files.access_hash is 64 random bits drawn per row, and a message photo
// carries that same raw value, so neither is derivable or caller-bound.
func TestVerifyRejectsRawFileHash(t *testing.T) {
	t.Parallel()

	d := deriver(t, masterA)
	for _, raw := range []int64{0, -1, 1, 2, 3, math.MaxInt64, math.MinInt64, 6921093098080204358, -6589232073781708695} {
		if d.Verify(1, 2, 3, raw) {
			t.Fatalf("Verify accepted raw file access_hash %d for (1,2,3)", raw)
		}
	}
	if got := d.Derive(1, 2, 3); got == 3 {
		t.Fatal("the photo hash for (1,2,3) is the file id itself")
	}
}

// TestVerifyRejectsEverySingleBytePerturbation pins that all eight MAC bytes
// take part in the decision. Verify compares the full eight-byte MAC in one
// constant-time comparison, so a mismatch in any position rejects, and no
// position is short-circuited.
func TestVerifyRejectsEverySingleBytePerturbation(t *testing.T) {
	t.Parallel()

	d := deriver(t, masterA)
	want := d.Derive(1, 2, 3)

	var base [8]byte
	putBE(base[:], want)
	for i := range base {
		perturbed := base
		perturbed[i] ^= 0xff
		got := int64(binary.BigEndian.Uint64(perturbed[:])) //nolint:gosec // reinterpreting all 64 bits, not narrowing
		if got == want {
			t.Fatalf("perturbing byte %d of %d did not change the value", i, want)
		}
		if d.Verify(1, 2, 3, got) {
			t.Fatalf("Verify accepted a hash differing from %d in byte %d: %d", want, i, got)
		}
	}
}

// TestDeriverRendersNoKeyMaterial keeps the subkey out of logs: the Deriver is
// what travels to the RPC layer, and a stray %v, %s, %+v or %#v of it must not
// print the key it holds.
func TestDeriverRendersNoKeyMaterial(t *testing.T) {
	t.Parallel()

	sub := subkeyOf(t, masterA)
	d, err := photohash.New(sub)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rendered := fmt.Sprintf("%v|%s|%+v|%#v|%q|%d", d, d, d, d, d, 0)
	for _, form := range []string{
		string(sub),
		hex.EncodeToString(sub),
		fmt.Sprintf("%v", sub),
		strings.Join(strings.Fields(fmt.Sprintf("%v", sub)), ","),
	} {
		if strings.Contains(rendered, form) {
			t.Fatalf("rendered Deriver carries subkey material as %q: %s", form, rendered)
		}
	}
	for _, form := range []string{string(masterA), hex.EncodeToString(masterA), fmt.Sprintf("%v", masterA)} {
		if strings.Contains(rendered, form) {
			t.Fatalf("rendered Deriver carries master key material as %q: %s", form, rendered)
		}
	}
}

// TestDeriveConcurrentUse backs the immutability claim: one Deriver is shared
// across the RPC layer, so concurrent derivations must agree and stay race-free
// under -race.
func TestDeriveConcurrentUse(t *testing.T) {
	t.Parallel()

	const want = 1411330975392603422

	d := deriver(t, masterA)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 64 {
				if got := d.Derive(1, 2, 3); got != want {
					t.Errorf("concurrent Derive = %d, want %d", got, want)
					return
				}
			}
		})
	}
	wg.Wait()
}

// putBE writes v big-endian into b, reinterpreting the signed bits rather than
// narrowing them, the same way the production derivation does.
func putBE(b []byte, v int64) {
	binary.BigEndian.PutUint64(b, uint64(v)) //nolint:gosec // reinterpreting all 64 bits, not narrowing
}
