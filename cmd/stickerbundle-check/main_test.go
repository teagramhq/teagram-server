package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunChecksBundle(t *testing.T) {
	dir := writeCommandBundle(t, "MIT")
	var output strings.Builder
	if err := run([]string{dir}, &output); err != nil {
		t.Fatalf("check valid bundle: %v", err)
	}
	if output.String() != "validated 1 sticker packs\n" {
		t.Fatalf("check output = %q", output.String())
	}
}

func TestRunNamesPackRejectedByLicenseGate(t *testing.T) {
	dir := writeCommandBundle(t, "GPL-3.0-only")
	var output strings.Builder
	err := run([]string{dir}, &output)
	if err == nil || !strings.Contains(err.Error(), "sample_pack") {
		t.Fatalf("check invalid bundle error = %v, want pack name", err)
	}
}

func writeCommandBundle(t *testing.T, license string) string {
	t.Helper()
	dir := t.TempDir()
	art := []byte("RIFF\x04\x00\x00\x00WEBP")
	checksum := sha256.Sum256(art)
	manifest := map[string]any{
		"version": 1,
		"packs": []any{map[string]any{
			"short_name":  "sample_pack",
			"source":      "Sample Art",
			"license":     license,
			"attribution": "Sample attribution",
			"stickers": []any{map[string]any{
				"emoji": "🐈", "file": "cat.webp", "size": len(art), "sha256": hex.EncodeToString(checksum[:]),
			}},
		}},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("encode bundle manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o600); err != nil {
		t.Fatalf("write bundle manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cat.webp"), art, 0o600); err != nil {
		t.Fatalf("write bundle art: %v", err)
	}
	return dir
}
