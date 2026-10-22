package reactionassets_test

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/image/webp"
)

type manifestEntry struct {
	Emoji string `json:"emoji"`
	SVG   string `json:"svg"`
}

var standardReactionCodepoints = []string{
	"1f44d", "1f44e", "2764", "1f525", "1f970", "1f44f", "1f601", "1f914",
	"1f92f", "1f631", "1f92c", "1f622", "1f389", "1f929", "1f92e", "1f4a9",
	"1f64f", "1f44c", "1f54a", "1f921", "1f971", "1f974", "1f60d", "1f433",
	"2764-200d-1f525", "1f31a", "1f32d", "1f4af", "1f923", "26a1", "1f34c", "1f3c6",
	"1f494", "1f928", "1f610", "1f353", "1f37e", "1f48b", "1f595", "1f608",
	"1f634", "1f62d", "1f913", "1f47b", "1f468-200d-1f4bb", "1f440", "1f383", "1f648",
	"1f607", "1f628", "1f91d", "270d", "1f917", "1fae1", "1f385", "1f384",
	"2603", "1f485", "1f92a", "1f5ff", "1f192", "1f498", "1f649", "1f984",
	"1f618", "1f48a", "1f64a", "1f60e", "1f47e", "1f937-200d-2642", "1f937",
	"1f937-200d-2640", "1f621",
}

func TestReactionAssets(t *testing.T) {
	manifestBytes, err := os.ReadFile("manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest []manifestEntry
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if len(manifest) != len(standardReactionCodepoints) {
		t.Fatalf("manifest has %d reactions, want %d", len(manifest), len(standardReactionCodepoints))
	}

	staticFiles := make(map[string]struct{}, len(manifest))
	tgsFiles := make(map[string]struct{}, len(manifest))
	emojis := make(map[string]struct{}, len(manifest))
	for i, entry := range manifest {
		if got := codepointSequence(entry.Emoji); got != standardReactionCodepoints[i] {
			t.Fatalf("manifest reaction %d is %q (%s), want %s", i, entry.Emoji, got, standardReactionCodepoints[i])
		}
		if _, exists := emojis[entry.Emoji]; exists {
			t.Fatalf("duplicate reaction %q", entry.Emoji)
		}
		emojis[entry.Emoji] = struct{}{}

		stem := strings.TrimSuffix(entry.SVG, ".svg")
		if stem == entry.SVG || normalizeTwemojiStem(stem) != standardReactionCodepoints[i] {
			t.Fatalf("reaction %q maps to invalid Twemoji source %q", entry.Emoji, entry.SVG)
		}
		if _, exists := staticFiles[stem+".webp"]; exists {
			t.Fatalf("duplicate WebP mapping for %q", entry.Emoji)
		}
		staticFiles[stem+".webp"] = struct{}{}
		tgsFiles[stem+".tgs"] = struct{}{}

		checkWebP(t, filepath.Join("static", stem+".webp"))
		checkTGS(t, filepath.Join("tgs", stem+".tgs"))
	}
	checkDirectory(t, "static", staticFiles)
	checkDirectory(t, "tgs", tgsFiles)
}

func codepointSequence(emoji string) string {
	parts := make([]string, 0, len(emoji))
	for _, r := range emoji {
		parts = append(parts, strconv.FormatInt(int64(r), 16))
	}
	return strings.Join(parts, "-")
}

func normalizeTwemojiStem(stem string) string {
	parts := strings.Split(stem, "-")
	filtered := parts[:0]
	for _, part := range parts {
		if part != "fe0e" && part != "fe0f" {
			filtered = append(filtered, part)
		}
	}
	return strings.Join(filtered, "-")
}

func checkWebP(t *testing.T, path string) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Errorf("close %s: %v", path, err)
		}
	}()

	image, err := webp.Decode(file)
	if err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	if got := image.Bounds().Size(); got.X != 512 || got.Y != 512 {
		t.Fatalf("%s dimensions = %dx%d, want 512x512", path, got.X, got.Y)
	}
}

func checkTGS(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatalf("%s is empty", path)
	}
	if len(data) > 64*1024 {
		t.Fatalf("%s is %d bytes, TGS limit is 65536", path, len(data))
	}
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("open gzip %s: %v", path, err)
	}
	defer func() {
		if err := reader.Close(); err != nil {
			t.Errorf("close gzip %s: %v", path, err)
		}
	}()
	jsonBytes, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("gunzip %s: %v", path, err)
	}
	var composition struct {
		Width  int               `json:"w"`
		Height int               `json:"h"`
		FPS    float64           `json:"fr"`
		TGS    int               `json:"tgs"`
		Layers []json.RawMessage `json:"layers"`
	}
	if err := json.Unmarshal(jsonBytes, &composition); err != nil {
		t.Fatalf("decode Lottie JSON in %s: %v", path, err)
	}
	if composition.Width != 512 || composition.Height != 512 {
		t.Fatalf("%s canvas = %dx%d, want 512x512", path, composition.Width, composition.Height)
	}
	if composition.FPS != 30 && composition.FPS != 60 {
		t.Fatalf("%s framerate = %g, want 30 or 60", path, composition.FPS)
	}
	if composition.TGS != 1 {
		t.Fatalf("%s tgs marker = %d, want 1", path, composition.TGS)
	}
	if len(composition.Layers) == 0 {
		t.Fatalf("%s has no Lottie layers", path)
	}
}

func checkDirectory(t *testing.T, path string, expected map[string]struct{}) {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(expected) {
		t.Fatalf("%s contains %d files, want %d", path, len(entries), len(expected))
	}
	for _, entry := range entries {
		if entry.IsDir() {
			t.Errorf("unexpected directory in %s: %s", path, entry.Name())
			continue
		}
		if _, ok := expected[entry.Name()]; !ok {
			t.Errorf("unexpected file in %s: %s", path, entry.Name())
		}
	}
}

func TestReactionManifestCount(t *testing.T) {
	if got := len(standardReactionCodepoints); got != 73 {
		t.Fatalf("standard reaction set has %d entries, want 73", got)
	}
}
