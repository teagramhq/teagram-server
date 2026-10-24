package config_test

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/teagramhq/teagram-server/internal/config"
)

const (
	mediaKeyOne = "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"
	mediaKeyTwo = "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
)

func setMediaKeyBase(t *testing.T) {
	t.Helper()
	clearBlobS3Env(t)
	t.Setenv("TG_POSTGRES_DSN", "postgres://localhost/tg")
	t.Setenv("TG_AUTHKEY_ENC_KEY", validEncKey)
	t.Setenv("TG_MEDIA_KEY_DIR", "")
	t.Setenv("TG_MEDIA_KEY_CURRENT", "")
}

func mediaKeyDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "media-keys")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o400); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLoadMediaKeysUnsetLeavesRingNil(t *testing.T) {
	setMediaKeyBase(t)
	cfg, err := config.Load(discardLog())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MediaKeys != nil {
		t.Fatalf("MediaKeys = %v, want nil", cfg.MediaKeys)
	}
}

func TestLoadMediaKeysReadsEveryVersion(t *testing.T) {
	setMediaKeyBase(t)
	dir := mediaKeyDir(t, map[string]string{
		"1.key": mediaKeyOne + "\n",
		"2.key": "  " + mediaKeyTwo + "\r\n",
	})
	// Kubernetes secret volume bookkeeping: dot-entries are skipped.
	if err := os.Mkdir(filepath.Join(dir, "..data"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TG_MEDIA_KEY_DIR", dir)
	t.Setenv("TG_MEDIA_KEY_CURRENT", "1")
	cfg, err := config.Load(discardLog())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MediaKeys == nil || cfg.MediaKeys.Current() != 1 {
		t.Fatalf("MediaKeys = %v, want current 1", cfg.MediaKeys)
	}
	if got := cfg.MediaKeys.Versions(); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("versions = %v, want [1 2]", got)
	}
	two, err := hex.DecodeString(mediaKeyTwo)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := cfg.MediaKeys.Contains(two); !ok || v != 2 {
		t.Fatalf("version 2 = %d, %v; want the 2.key bytes", v, ok)
	}
}

func TestLoadMediaKeysFollowsSymlinks(t *testing.T) {
	setMediaKeyBase(t)
	dir := mediaKeyDir(t, nil)
	target := filepath.Join(dir, "..2026_10_24")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "1.key"), []byte(mediaKeyOne), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..2026_10_24", "1.key"), filepath.Join(dir, "1.key")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TG_MEDIA_KEY_DIR", dir)
	t.Setenv("TG_MEDIA_KEY_CURRENT", "1")
	cfg, err := config.Load(discardLog())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MediaKeys.Current() != 1 {
		t.Fatalf("MediaKeys = %v", cfg.MediaKeys)
	}
}

func TestLoadMediaKeysRejects(t *testing.T) {
	tests := []struct {
		name    string
		files   map[string]string
		setup   func(t *testing.T, dir string)
		dir     func(dir string) string
		current string
		want    string
	}{
		{
			name: "dir without current", files: map[string]string{"1.key": mediaKeyOne},
			want: "TG_MEDIA_KEY_DIR and TG_MEDIA_KEY_CURRENT must be set together",
		},
		{
			name: "current without dir", current: "1",
			dir:  func(string) string { return "" },
			want: "TG_MEDIA_KEY_DIR and TG_MEDIA_KEY_CURRENT must be set together",
		},
		{name: "current zero", files: map[string]string{"1.key": mediaKeyOne}, current: "0", want: "TG_MEDIA_KEY_CURRENT must be a key version"},
		{name: "current padded", files: map[string]string{"1.key": mediaKeyOne}, current: "01", want: "TG_MEDIA_KEY_CURRENT must be a key version"},
		{name: "current too large", files: map[string]string{"1.key": mediaKeyOne}, current: "4294967296", want: "TG_MEDIA_KEY_CURRENT must be a key version"},
		{
			name: "missing dir", current: "1",
			dir:  func(dir string) string { return filepath.Join(dir, "absent") },
			want: "TG_MEDIA_KEY_DIR:",
		},
		{
			name: "dir is a file", files: map[string]string{"1.key": mediaKeyOne}, current: "1",
			dir:  func(dir string) string { return filepath.Join(dir, "1.key") },
			want: "is not a directory",
		},
		{
			name: "dir writable by group", files: map[string]string{"1.key": mediaKeyOne}, current: "1",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				// #nosec G302 -- This fixture verifies rejection of a writable key directory.
				if err := os.Chmod(dir, 0o770); err != nil {
					t.Fatal(err)
				}
			},
			want: "is writable by group or others",
		},
		{
			name: "file writable by others", files: map[string]string{"1.key": mediaKeyOne}, current: "1",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				// #nosec G302 -- This fixture verifies rejection of a writable key file.
				if err := os.Chmod(filepath.Join(dir, "1.key"), 0o602); err != nil {
					t.Fatal(err)
				}
			},
			want: "1.key is writable by group or others",
		},
		{
			name: "key entry not a regular file", files: map[string]string{"1.key": mediaKeyOne}, current: "1",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				if err := os.Mkdir(filepath.Join(dir, "2.key"), 0o700); err != nil {
					t.Fatal(err)
				}
			},
			want: "2.key is not a regular file",
		},
		{name: "unexpected entry", files: map[string]string{"1.key": mediaKeyOne, "README": "x"}, current: "1", want: "unexpected entry"},
		{name: "padded version file", files: map[string]string{"1.key": mediaKeyOne, "02.key": mediaKeyTwo}, current: "1", want: "unexpected entry"},
		{name: "version zero file", files: map[string]string{"1.key": mediaKeyOne, "0.key": mediaKeyTwo}, current: "1", want: "unexpected entry"},
		{name: "wrong suffix", files: map[string]string{"1.key": mediaKeyOne, "2.pem": mediaKeyTwo}, current: "1", want: "unexpected entry"},
		{name: "short key", files: map[string]string{"1.key": mediaKeyOne[:62]}, current: "1", want: "1.key must hold 64 hex chars (32 bytes)"},
		{name: "long key", files: map[string]string{"1.key": mediaKeyOne + "a1"}, current: "1", want: "1.key must hold 64 hex chars (32 bytes)"},
		{name: "odd length key", files: map[string]string{"1.key": mediaKeyOne[:63]}, current: "1", want: "1.key must hold 64 hex chars (32 bytes)"},
		{name: "malformed key", files: map[string]string{"1.key": "zz" + mediaKeyOne[2:]}, current: "1", want: "1.key must hold 64 hex chars (32 bytes)"},
		{name: "empty key", files: map[string]string{"1.key": "\n"}, current: "1", want: "1.key must hold 64 hex chars (32 bytes)"},
		{name: "current has no file", files: map[string]string{"1.key": mediaKeyOne}, current: "3", want: "TG_MEDIA_KEY_CURRENT=3 has no 3.key in TG_MEDIA_KEY_DIR"},
		{name: "no keys", current: "1", want: "TG_MEDIA_KEY_CURRENT=1 has no 1.key in TG_MEDIA_KEY_DIR"},
		{
			name: "equals auth-key master", files: map[string]string{"1.key": mediaKeyOne, "2.key": validEncKey}, current: "1",
			want: "TG_MEDIA_KEY_DIR: 2.key equals TG_AUTHKEY_ENC_KEY: the media key must be a separate key",
		},
		{
			name: "equals auth-key master in another case", files: map[string]string{"1.key": strings.ToUpper(validEncKey)}, current: "1",
			want: "1.key equals TG_AUTHKEY_ENC_KEY",
		},
		{name: "two versions share bytes", files: map[string]string{"1.key": mediaKeyOne, "2.key": mediaKeyOne}, current: "1", want: "hold the same key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setMediaKeyBase(t)
			dir := mediaKeyDir(t, tt.files)
			if tt.setup != nil {
				tt.setup(t, dir)
			}
			if tt.dir != nil {
				dir = tt.dir(dir)
			}
			t.Setenv("TG_MEDIA_KEY_DIR", dir)
			t.Setenv("TG_MEDIA_KEY_CURRENT", tt.current)
			_, err := config.Load(discardLog())
			if err == nil {
				t.Fatal("Load accepted the media key configuration")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load error = %q, want it to contain %q", err, tt.want)
			}
			for _, secret := range []string{mediaKeyOne, mediaKeyTwo, validEncKey, mediaKeyOne[:62], mediaKeyOne[2:]} {
				if strings.Contains(strings.ToLower(err.Error()), secret) {
					t.Fatalf("Load error carries key material: %q", err)
				}
			}
		})
	}
}

func TestConfigJSONCarriesNoKeyMaterial(t *testing.T) {
	setMediaKeyBase(t)
	dir := mediaKeyDir(t, map[string]string{"1.key": mediaKeyOne, "2.key": mediaKeyTwo})
	t.Setenv("TG_MEDIA_KEY_DIR", dir)
	t.Setenv("TG_MEDIA_KEY_CURRENT", "2")
	cfg, err := config.Load(discardLog())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	out, err := json.Marshal(cfg) //nolint:musttag // asserts the untagged default encoding a dump would use
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, secret := range []string{mediaKeyOne, mediaKeyTwo, validEncKey} {
		raw, err := hex.DecodeString(secret)
		if err != nil {
			t.Fatal(err)
		}
		for _, form := range []string{secret, base64.StdEncoding.EncodeToString(raw), string(raw)} {
			if strings.Contains(string(out), form) {
				t.Fatalf("Config JSON carries key material: %s", out)
			}
		}
	}
}
