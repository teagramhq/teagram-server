package blob

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"slices"
)

// MediaKeyLen is the byte length of one media key-encryption key.
const MediaKeyLen = 32

// KeyRing holds the media key-encryption keys [Encrypted] wraps per-object
// data keys under, by version. Every version in the ring reads; only the
// current one writes, so a new version can be present on every replica before
// any of them starts writing under it, and an old one keeps reading until it
// is removed.
//
// The ring never prints its key bytes: String, GoString, MarshalJSON and
// LogValue all report versions only, so a ring that reaches a log line, an
// error or a JSON dump of the configuration carries no key material.
type KeyRing struct {
	current uint32
	keys    map[uint32][]byte
}

// NewKeyRing returns a ring over keys with current as the write version. It
// copies every key, so the caller may wipe its own buffers afterwards. Version
// zero is reserved, every key must be [MediaKeyLen] bytes, current must be in
// the ring, and no two versions may hold the same bytes: a duplicate means a
// rotation that did not rotate.
func NewKeyRing(current uint32, keys map[uint32][]byte) (*KeyRing, error) {
	if len(keys) == 0 {
		return nil, errors.New("media key ring: no keys")
	}
	ring := &KeyRing{current: current, keys: make(map[uint32][]byte, len(keys))}
	for _, version := range sortedVersions(keys) {
		key := keys[version]
		if version == 0 {
			return nil, errors.New("media key ring: version 0 is reserved")
		}
		if len(key) != MediaKeyLen {
			return nil, fmt.Errorf("media key ring: version %d is %d bytes, want %d", version, len(key), MediaKeyLen)
		}
		for other, seen := range ring.keys {
			if subtle.ConstantTimeCompare(seen, key) == 1 {
				return nil, fmt.Errorf("media key ring: versions %d and %d hold the same key", other, version)
			}
		}
		ring.keys[version] = slices.Clone(key)
	}
	if _, ok := ring.keys[current]; !ok {
		return nil, fmt.Errorf("media key ring: current version %d is not in the ring", current)
	}
	return ring, nil
}

// Current reports the version new objects are written under.
func (r *KeyRing) Current() uint32 { return r.current }

// Versions reports every version the ring can read, ascending.
func (r *KeyRing) Versions() []uint32 { return sortedVersions(r.keys) }

// Contains reports whether any version in the ring holds key, in constant time
// per version. It exists so a caller can refuse a media key that is byte-equal
// to another secret without the ring handing its bytes out.
func (r *KeyRing) Contains(key []byte) (uint32, bool) {
	for _, version := range r.Versions() {
		if subtle.ConstantTimeCompare(r.keys[version], key) == 1 {
			return version, true
		}
	}
	return 0, false
}

func (r *KeyRing) key(version uint32) ([]byte, bool) {
	key, ok := r.keys[version]
	return key, ok
}

// String reports the ring's versions and never its key bytes.
func (r *KeyRing) String() string {
	if r == nil {
		return "blob.KeyRing(nil)"
	}
	return fmt.Sprintf("blob.KeyRing{current: %d, versions: %v}", r.current, r.Versions())
}

// GoString keeps %#v from printing the key map.
func (r *KeyRing) GoString() string { return r.String() }

// MarshalJSON reports versions only, so a JSON dump of a value holding the
// ring carries no key material.
func (r *KeyRing) MarshalJSON() ([]byte, error) {
	if r == nil {
		return []byte("null"), nil
	}
	versions := r.Versions()
	out := fmt.Appendf(nil, `{"current":%d,"versions":[`, r.current)
	for i, v := range versions {
		if i > 0 {
			out = append(out, ',')
		}
		out = fmt.Appendf(out, "%d", v)
	}
	return append(out, "]}"...), nil
}

// LogValue reports versions only.
func (r *KeyRing) LogValue() slog.Value {
	if r == nil {
		return slog.StringValue("<nil>")
	}
	return slog.GroupValue(
		slog.Any("current", r.current),
		slog.Any("versions", r.Versions()),
	)
}

func sortedVersions(keys map[uint32][]byte) []uint32 {
	versions := make([]uint32, 0, len(keys))
	for v := range keys {
		versions = append(versions, v)
	}
	slices.Sort(versions)
	return versions
}
