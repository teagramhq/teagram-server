package config

import (
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/teagramhq/teagram-server/internal/blob"
)

const mediaKeySuffix = ".key"

// mediaKeysConfigured reports whether either media key setting is present.
// With neither, the media key ring stays nil.
func mediaKeysConfigured() bool {
	return os.Getenv("TG_MEDIA_KEY_DIR") != "" || os.Getenv("TG_MEDIA_KEY_CURRENT") != ""
}

// loadMediaKeys reads the media key ring from TG_MEDIA_KEY_DIR, one
// "<version>.key" file of 64 hex chars per version, with TG_MEDIA_KEY_CURRENT
// naming the version new objects are written under. Every version in the
// directory reads. The current version is named rather than taken as the
// highest file, so a new key can reach every replica before any of them
// writes under it.
//
// Both settings or neither. There is no inline env form, so the key never
// sits in an env file beside the object-store secret. The media key is its own secret, never derived from the auth-key
// master, and one byte-equal to it is refused.
//
// The permission check is for write access only: a key someone else can write
// is a key someone else can choose. Compose secrets reach the container as
// world-readable files under a private host directory, so refusing read access
// would refuse the deployment's own secret layout.
//
// Errors name the setting and the file, never key bytes.
func loadMediaKeys(authKey []byte) (*blob.KeyRing, error) {
	dir := os.Getenv("TG_MEDIA_KEY_DIR")
	rawCurrent := os.Getenv("TG_MEDIA_KEY_CURRENT")
	if dir == "" || rawCurrent == "" {
		return nil, errors.New("TG_MEDIA_KEY_DIR and TG_MEDIA_KEY_CURRENT must be set together")
	}
	current, ok := parseMediaKeyVersion(rawCurrent)
	if !ok {
		return nil, errors.New("TG_MEDIA_KEY_CURRENT must be a key version from 1 to 4294967295")
	}

	info, err := os.Stat(dir) // #nosec G703 -- dir is the operator-configured key directory.
	if err != nil {
		return nil, fmt.Errorf("TG_MEDIA_KEY_DIR: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("TG_MEDIA_KEY_DIR: %s is not a directory", dir)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("TG_MEDIA_KEY_DIR: %s is writable by group or others", dir)
	}
	entries, err := os.ReadDir(dir) // #nosec G703 -- dir is the operator-configured key directory.
	if err != nil {
		return nil, fmt.Errorf("TG_MEDIA_KEY_DIR: %w", err)
	}

	keys := make(map[uint32][]byte, len(entries))
	defer func() {
		for _, key := range keys {
			clear(key)
		}
	}()
	for _, entry := range entries {
		name := entry.Name()
		// Kubernetes secret volumes keep their bookkeeping under dot-names
		// and expose each key as a symlink into it.
		if strings.HasPrefix(name, ".") {
			continue
		}
		path := filepath.Join(dir, name)
		version, ok := parseMediaKeyVersion(strings.TrimSuffix(name, mediaKeySuffix))
		if !ok || !strings.HasSuffix(name, mediaKeySuffix) {
			return nil, fmt.Errorf("TG_MEDIA_KEY_DIR: unexpected entry %s: want only <version>%s files", path, mediaKeySuffix)
		}
		key, err := readMediaKeyFile(path)
		if err != nil {
			return nil, err
		}
		keys[version] = key
		if subtle.ConstantTimeCompare(key, authKey) == 1 {
			return nil, fmt.Errorf("TG_MEDIA_KEY_DIR: %s equals TG_AUTHKEY_ENC_KEY: the media key must be a separate key", name)
		}
	}
	if _, ok := keys[current]; !ok {
		return nil, fmt.Errorf("TG_MEDIA_KEY_CURRENT=%d has no %d%s in TG_MEDIA_KEY_DIR", current, current, mediaKeySuffix)
	}
	ring, err := blob.NewKeyRing(current, keys)
	if err != nil {
		return nil, fmt.Errorf("TG_MEDIA_KEY_DIR: %w", err)
	}
	return ring, nil
}

// parseMediaKeyVersion accepts only the canonical decimal form of a nonzero
// uint32, so "01.key" and "1.key" cannot both name version 1.
func parseMediaKeyVersion(s string) (uint32, bool) {
	v, err := strconv.ParseUint(s, 10, 32)
	if err != nil || v == 0 || strconv.FormatUint(v, 10) != s {
		return 0, false
	}
	return uint32(v), true
}

func readMediaKeyFile(path string) ([]byte, error) {
	info, err := os.Stat(path) // #nosec G703 -- path is under the operator-configured key directory.
	if err != nil {
		return nil, fmt.Errorf("TG_MEDIA_KEY_DIR: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("TG_MEDIA_KEY_DIR: %s is not a regular file", path)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("TG_MEDIA_KEY_DIR: %s is writable by group or others", path)
	}
	raw, err := os.ReadFile(path) // #nosec G304,G703 -- path is under the operator-configured key directory.
	if err != nil {
		return nil, fmt.Errorf("TG_MEDIA_KEY_DIR: %w", err)
	}
	defer clear(raw)
	text := strings.TrimSpace(string(raw))
	// The decoder's own error quotes the offending character, which is key
	// material, so it is replaced rather than wrapped.
	key, err := hex.DecodeString(text)
	if err != nil || len(key) != blob.MediaKeyLen {
		return nil, fmt.Errorf("TG_MEDIA_KEY_DIR: %s must hold 64 hex chars (32 bytes)", path)
	}
	return key, nil
}
