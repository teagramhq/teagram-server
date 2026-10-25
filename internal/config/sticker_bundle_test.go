package config_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/teagramhq/teagram-server/internal/config"
)

func TestLoadValidatesConfiguredStickerBundle(t *testing.T) {
	dir := writeConfigStickerBundle(t, "MIT")
	t.Setenv("TG_POSTGRES_DSN", "postgres://localhost/tg")
	t.Setenv("TG_AUTHKEY_ENC_KEY", validEncKey)
	t.Setenv("TG_STICKER_CATALOG_BUNDLE", dir)

	cfg, err := config.Load(discardLog())
	if err != nil {
		t.Fatalf("load valid sticker bundle config: %v", err)
	}
	if cfg.StickerBundle == nil || cfg.StickerBundle.Root != dir {
		t.Fatalf("configured sticker bundle = %+v, want validated bundle rooted at %q", cfg.StickerBundle, dir)
	}
}

func TestLoadRefusesInvalidConfiguredStickerBundle(t *testing.T) {
	dir := writeConfigStickerBundle(t, "GPL-3.0-only")
	t.Setenv("TG_POSTGRES_DSN", "postgres://localhost/tg")
	t.Setenv("TG_AUTHKEY_ENC_KEY", validEncKey)
	t.Setenv("TG_STICKER_CATALOG_BUNDLE", dir)

	_, err := config.Load(discardLog())
	if err == nil || !strings.Contains(err.Error(), "sample_pack") {
		t.Fatalf("load invalid sticker bundle error = %v, want pack name", err)
	}
}

func writeConfigStickerBundle(t *testing.T, license string) string {
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
				"emoji":  "🐈",
				"file":   "cat.webp",
				"size":   len(art),
				"sha256": hex.EncodeToString(checksum[:]),
			}},
		}},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("encode sticker bundle manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o600); err != nil {
		t.Fatalf("write sticker bundle manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cat.webp"), art, 0o600); err != nil {
		t.Fatalf("write sticker bundle art: %v", err)
	}
	return dir
}
