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
	if output.String() != "validated 1 sticker/emoji packs\n" {
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

func TestRunRejectsEmojiPackWithoutLicenseEntry(t *testing.T) {
	dir := writeCommandBundle(t, "", "emojis")
	var output strings.Builder
	err := run([]string{dir}, &output)
	if err == nil || !strings.Contains(err.Error(), "sample_pack") {
		t.Fatalf("check unlicensed emoji bundle error = %v, want pack name", err)
	}
}

func TestRunRejectsEmojiPackNameCollisionWithBuiltInSet(t *testing.T) {
	dir := writeCommandBundleNamed(t, "MIT", "emojis", "twemoji_custom_emojis")
	var output strings.Builder
	err := run([]string{dir}, &output)
	if err == nil || !strings.Contains(err.Error(), "twemoji_custom_emojis") {
		t.Fatalf("check colliding emoji set error = %v, want built-in short name", err)
	}
}

func TestRunLabelsEmojiPacksInValidationResult(t *testing.T) {
	dir := writeCommandBundle(t, "MIT", "emojis")
	var output strings.Builder
	if err := run([]string{dir}, &output); err != nil {
		t.Fatalf("check valid emoji bundle: %v", err)
	}
	if output.String() != "validated 1 sticker/emoji packs\n" {
		t.Fatalf("emoji bundle check output = %q", output.String())
	}
}

func writeCommandBundle(t *testing.T, license string, kind ...string) string {
	t.Helper()
	emojiKind := ""
	if len(kind) != 0 {
		emojiKind = kind[0]
	}
	return writeCommandBundleNamed(t, license, emojiKind, "sample_pack")
}

func writeCommandBundleNamed(t *testing.T, license, kind, shortName string) string {
	t.Helper()
	dir := t.TempDir()
	art := []byte("RIFF\x04\x00\x00\x00WEBP")
	checksum := sha256.Sum256(art)
	pack := map[string]any{
		"short_name":  shortName,
		"source":      "Sample Art",
		"attribution": "Sample attribution",
		"stickers": []any{map[string]any{
			"emoji": "🐈", "file": "cat.webp", "size": len(art), "sha256": hex.EncodeToString(checksum[:]),
		}},
	}
	if license != "" {
		pack["license"] = license
	}
	if kind != "" {
		pack["kind"] = kind
	}
	manifest := map[string]any{
		"version": 1,
		"packs":   []any{pack},
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
