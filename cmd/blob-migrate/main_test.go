package main

import "testing"

func TestHasPrivateManifestOutput(t *testing.T) {
	t.Parallel()

	for name, args := range map[string][]string{
		"separate flag": {"--direction", "s3-census", "--manifest", "stdout"},
		"equals flag":   {"--manifest=stdout", "--direction", "local-census"},
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
