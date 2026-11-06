package main

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/blobmigration"
)

func TestEncryptedDestinationWritesUnderEncryptedNamespace(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	remote, err := blob.NewLocal(dir)
	if err != nil {
		t.Fatalf("open remote store: %v", err)
	}
	ring, err := blob.NewKeyRing(1, map[uint32][]byte{1: bytes.Repeat([]byte{1}, blob.MediaKeyLen)})
	if err != nil {
		t.Fatalf("create key ring: %v", err)
	}
	encrypted, tree, err := encryptedDestination(remote, ring)
	if err != nil {
		t.Fatalf("construct encrypted destination: %v", err)
	}
	key, err := blob.NewPartKey()
	if err != nil {
		t.Fatalf("create part key: %v", err)
	}
	if _, err := encrypted.Put(context.Background(), key, strings.NewReader("payload")); err != nil {
		t.Fatalf("write encrypted object: %v", err)
	}

	// An envelope at the plaintext key would replace the accepted object the
	// serving store does not read.
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(key))); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("encrypted write landed at plaintext key %q: stat err = %v", key, err)
	}
	if _, err := os.Stat(filepath.Join(dir, blob.EncryptedObjectsPrefix, filepath.FromSlash(key))); err != nil {
		t.Fatalf("encrypted write missing from encrypted namespace: %v", err)
	}
	var walked []string
	if err := tree.Walk(context.Background(), func(entry blob.Entry) error {
		if entry.Regular && !entry.Dir {
			walked = append(walked, entry.Key)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk encrypted namespace: %v", err)
	}
	if len(walked) != 1 || walked[0] != key {
		t.Fatalf("encrypted namespace walk = %v, want [%q]", walked, key)
	}
}

func TestHasPrivateManifestOutput(t *testing.T) {
	t.Parallel()

	for name, args := range map[string][]string{
		"separate flag":  {"--direction", "s3-census", "--manifest", "stdout"},
		"equals flag":    {"--manifest=stdout", "--direction", "local-census"},
		"discard flag":   {"--direction", "local-to-s3", "--manifest", "discard", "--encrypt"},
		"discard equals": {"--direction", "local-to-s3", "--manifest=discard", "--encrypt"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if !hasPrivateManifestOutput(args) {
				t.Fatal("private manifest output was not detected")
			}
		})
	}

	if hasPrivateManifestOutput([]string{"--manifest", "stderr"}) {
		t.Fatal("non-private output was marked private")
	}
}

func TestEncryptedMigrationRequiresManifestMode(t *testing.T) {
	t.Parallel()

	err := run([]string{"--direction", "local-to-s3", "--encrypt"}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "--encrypt requires --manifest") {
		t.Fatalf("encrypted migration without a manifest mode error = %v", err)
	}
}

func TestLogFailureRedactsDiscardManifestErrors(t *testing.T) {
	t.Parallel()

	var stderr bytes.Buffer
	logFailure(
		[]string{"--direction", "local-to-s3", "--manifest", "discard", "--encrypt"},
		&blobmigration.StageError{Stage: blobmigration.StageReadBack, Err: errors.New("secret object key and backend response")},
		&stderr,
	)
	if strings.Contains(stderr.String(), "secret object key") || strings.Contains(stderr.String(), "backend response") {
		t.Fatalf("private migration error was logged: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "blob-migrate private evidence operation failed") {
		t.Fatalf("private migration failure was not identified: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "stage=read-back") {
		t.Fatalf("private migration stage was not identified: %q", stderr.String())
	}
}
