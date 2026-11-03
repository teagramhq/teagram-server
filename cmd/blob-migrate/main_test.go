package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

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
		errors.New("secret object key and backend response"),
		&stderr,
	)
	if strings.Contains(stderr.String(), "secret object key") || strings.Contains(stderr.String(), "backend response") {
		t.Fatalf("private migration error was logged: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "blob-migrate private evidence operation failed") {
		t.Fatalf("private migration failure was not identified: %q", stderr.String())
	}
}
