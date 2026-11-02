// Package stickerbundle validates generated sticker catalogue bundles.
package stickerbundle

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const (
	manifestName       = "manifest.json"
	manifestMaxBytes   = 8 << 20
	maxStickerCount    = 120
	maxShortNameBytes  = 64
	maxEmojiBytes      = 64
	maxTitleCharacters = 64
	maxTGSSize         = 64 << 10
)

// PackKindStickers and PackKindEmojis identify the set type in a bundle manifest.
const (
	PackKindStickers = "stickers"
	PackKindEmojis   = "emojis"
)

var allowedLicenses = map[string]struct{}{
	"CC0-1.0":      {},
	"CC-BY-4.0":    {},
	"CC-BY-SA-4.0": {},
	"Apache-2.0":   {},
	"MIT":          {},
	"OFL-1.1":      {},
}

// Bundle is a validated manifest and a canonical root for its artwork files.
// Artwork is described by path and checksum and is not retained in memory.
type Bundle struct {
	Root  string
	Packs []Pack
}

// Pack is one installable sticker or custom emoji set from a generated bundle.
type Pack struct {
	ShortName   string
	Title       string
	Source      string
	License     string
	Attribution string
	Kind        string
	Format      string
	Stickers    []Sticker
}

// Sticker describes one validated file in the bundle.
type Sticker struct {
	Emoji    string
	File     string
	Path     string
	Size     int64
	SHA256   [sha256.Size]byte
	Format   string
	MimeType string
	Animated bool
}

type manifest struct {
	Version int            `json:"version"`
	Packs   []manifestPack `json:"packs"`
}

type manifestPack struct {
	ShortName   string            `json:"short_name"`
	Source      string            `json:"source"`
	License     string            `json:"license"`
	Attribution string            `json:"attribution"`
	Kind        string            `json:"kind,omitempty"`
	Format      string            `json:"format,omitempty"`
	Stickers    []manifestSticker `json:"stickers"`
}

type manifestSticker struct {
	Emoji  string `json:"emoji"`
	File   string `json:"file"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Load reads and validates manifest.json and streams each artwork file through
// its size and SHA-256 checks. It does not retain file bytes.
func Load(root string) (*Bundle, error) {
	if root == "" {
		return nil, errors.New("sticker bundle: directory is empty")
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("sticker bundle: resolve directory: %w", err)
	}
	canonicalRoot, err := filepath.EvalSymlinks(absoluteRoot)
	if err != nil {
		return nil, fmt.Errorf("sticker bundle: resolve directory: %w", err)
	}
	rootInfo, err := os.Stat(canonicalRoot)
	if err != nil || !rootInfo.IsDir() {
		if err != nil {
			return nil, fmt.Errorf("sticker bundle: inspect directory: %w", err)
		}
		return nil, errors.New("sticker bundle: path is not a directory")
	}

	manifestPath := filepath.Join(canonicalRoot, manifestName)
	canonicalManifest, err := filepath.EvalSymlinks(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("sticker bundle: resolve manifest: %w", err)
	}
	if !withinRoot(canonicalRoot, canonicalManifest) {
		return nil, errors.New("sticker bundle: manifest is outside bundle directory")
	}
	manifestInfo, err := os.Stat(canonicalManifest)
	if err != nil {
		return nil, fmt.Errorf("sticker bundle: inspect manifest: %w", err)
	}
	if !manifestInfo.Mode().IsRegular() || manifestInfo.Size() > manifestMaxBytes {
		return nil, fmt.Errorf("sticker bundle: manifest must be a regular file no larger than %d bytes", manifestMaxBytes)
	}
	manifestFile, err := os.Open(canonicalManifest)
	if err != nil {
		return nil, fmt.Errorf("sticker bundle: open manifest: %w", err)
	}
	manifestBytes, readErr := io.ReadAll(io.LimitReader(manifestFile, manifestMaxBytes+1))
	closeErr := manifestFile.Close()
	if readErr != nil {
		return nil, errors.Join(fmt.Errorf("sticker bundle: read manifest: %w", readErr), wrapCloseError("sticker bundle: close manifest", closeErr))
	}
	if closeErr != nil {
		return nil, fmt.Errorf("sticker bundle: close manifest: %w", closeErr)
	}
	if len(manifestBytes) > manifestMaxBytes {
		return nil, fmt.Errorf("sticker bundle: manifest must be no larger than %d bytes", manifestMaxBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(manifestBytes))
	decoder.DisallowUnknownFields()
	var input manifest
	if err := decoder.Decode(&input); err != nil {
		return nil, fmt.Errorf("sticker bundle: decode manifest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("sticker bundle: manifest has trailing JSON")
		}
		return nil, fmt.Errorf("sticker bundle: decode manifest trailer: %w", err)
	}
	if input.Version != 1 {
		return nil, fmt.Errorf("sticker bundle: manifest version %d is unsupported", input.Version)
	}
	if len(input.Packs) == 0 {
		return nil, errors.New("sticker bundle: manifest has no packs")
	}

	bundle := &Bundle{Root: canonicalRoot, Packs: make([]Pack, 0, len(input.Packs))}
	seenNames := make(map[string]struct{}, len(input.Packs))
	for _, rawPack := range input.Packs {
		pack, err := loadPack(canonicalRoot, rawPack)
		if err != nil {
			return nil, err
		}
		if _, exists := seenNames[pack.ShortName]; exists {
			return nil, fmt.Errorf("sticker bundle pack %q duplicates a short name", pack.ShortName)
		}
		seenNames[pack.ShortName] = struct{}{}
		bundle.Packs = append(bundle.Packs, pack)
	}
	return bundle, nil
}

func loadPack(root string, raw manifestPack) (Pack, error) {
	pack := Pack{
		ShortName:   raw.ShortName,
		Source:      raw.Source,
		License:     raw.License,
		Attribution: raw.Attribution,
		Kind:        raw.Kind,
		Format:      raw.Format,
	}
	if pack.Kind == "" {
		pack.Kind = PackKindStickers
	}
	if pack.Kind != PackKindStickers && pack.Kind != PackKindEmojis {
		return Pack{}, fmt.Errorf("sticker bundle pack %q has unsupported kind %q", pack.ShortName, pack.Kind)
	}
	if pack.Format == "" {
		pack.Format = "webp"
	}
	if pack.Format != "webp" && pack.Format != "tgs" {
		return Pack{}, fmt.Errorf("sticker bundle pack %q has unsupported format %q", pack.ShortName, pack.Format)
	}
	if !validShortName(pack.ShortName) {
		return Pack{}, fmt.Errorf("sticker bundle pack %q has an invalid short name", pack.ShortName)
	}
	if pack.ShortName == "twemoji_reactions" {
		return Pack{}, fmt.Errorf("sticker bundle pack %q collides with the built-in sticker set", pack.ShortName)
	}
	if !validText(pack.Source) || strings.TrimSpace(pack.Source) != pack.Source {
		return Pack{}, fmt.Errorf("sticker bundle pack %q has an invalid source", pack.ShortName)
	}
	if _, ok := allowedLicenses[pack.License]; !ok {
		return Pack{}, fmt.Errorf("sticker bundle pack %q has missing or unsupported license %q", pack.ShortName, pack.License)
	}
	if !validText(pack.Attribution) || strings.TrimSpace(pack.Attribution) != pack.Attribution {
		return Pack{}, fmt.Errorf("sticker bundle pack %q has an invalid attribution", pack.ShortName)
	}
	pack.Title = pack.Source + " (" + displayLicense(pack.License) + ")"
	if utf8.RuneCountInString(pack.Title) > maxTitleCharacters {
		return Pack{}, fmt.Errorf("sticker bundle pack %q title exceeds Telegram's %d-character limit", pack.ShortName, maxTitleCharacters)
	}
	if len(raw.Stickers) == 0 || len(raw.Stickers) > maxStickerCount {
		return Pack{}, fmt.Errorf("sticker bundle pack %q has %d stickers; want between 1 and %d", pack.ShortName, len(raw.Stickers), maxStickerCount)
	}
	pack.Stickers = make([]Sticker, 0, len(raw.Stickers))
	for _, rawSticker := range raw.Stickers {
		sticker, err := loadSticker(root, pack.ShortName, pack.Format, rawSticker)
		if err != nil {
			return Pack{}, err
		}
		pack.Stickers = append(pack.Stickers, sticker)
	}
	return pack, nil
}

func displayLicense(license string) string {
	switch license {
	case "CC0-1.0":
		return "CC0 1.0"
	case "CC-BY-4.0":
		return "CC BY 4.0"
	case "CC-BY-SA-4.0":
		return "CC BY-SA 4.0"
	default:
		return license
	}
}

func loadSticker(root, shortName, format string, raw manifestSticker) (Sticker, error) {
	if !validText(raw.Emoji) || strings.TrimSpace(raw.Emoji) != raw.Emoji || len(raw.Emoji) > maxEmojiBytes {
		return Sticker{}, fmt.Errorf("sticker bundle pack %q has an invalid sticker emoji", shortName)
	}
	if !fs.ValidPath(raw.File) || strings.Contains(raw.File, "\\") || !filepath.IsLocal(filepath.FromSlash(raw.File)) {
		return Sticker{}, fmt.Errorf("sticker bundle pack %q sticker %q has an unsafe file path", shortName, raw.Emoji)
	}
	if filepath.Ext(raw.File) != "."+format {
		return Sticker{}, fmt.Errorf("sticker bundle pack %q sticker %q file extension does not match %s format", shortName, raw.Emoji, format)
	}
	if raw.Size <= 0 {
		return Sticker{}, fmt.Errorf("sticker bundle pack %q sticker %q has invalid size %d", shortName, raw.Emoji, raw.Size)
	}
	if format == "tgs" && raw.Size > maxTGSSize {
		return Sticker{}, fmt.Errorf("sticker bundle pack %q sticker %q TGS size %d exceeds %d bytes", shortName, raw.Emoji, raw.Size, maxTGSSize)
	}
	checksum, err := hex.DecodeString(raw.SHA256)
	if err != nil || len(checksum) != sha256.Size {
		return Sticker{}, fmt.Errorf("sticker bundle pack %q sticker %q has an invalid SHA-256 checksum", shortName, raw.Emoji)
	}
	var expected [sha256.Size]byte
	copy(expected[:], checksum)
	path := filepath.Join(root, filepath.FromSlash(raw.File))
	canonicalPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return Sticker{}, fmt.Errorf("sticker bundle pack %q sticker %q file %q: %w", shortName, raw.Emoji, raw.File, err)
	}
	if !withinRoot(root, canonicalPath) {
		return Sticker{}, fmt.Errorf("sticker bundle pack %q sticker %q file %q escapes the bundle directory", shortName, raw.Emoji, raw.File)
	}
	file, err := os.Open(canonicalPath)
	if err != nil {
		return Sticker{}, fmt.Errorf("sticker bundle pack %q sticker %q open file: %w", shortName, raw.Emoji, err)
	}
	sticker, validateErr := validateStickerFile(file, canonicalPath, shortName, format, raw, expected)
	closeErr := file.Close()
	if validateErr != nil || closeErr != nil {
		return Sticker{}, errors.Join(validateErr, wrapCloseError(fmt.Sprintf("sticker bundle pack %q sticker %q close file", shortName, raw.Emoji), closeErr))
	}
	return sticker, nil
}

func wrapCloseError(context string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", context, err)
}

func validateStickerFile(file *os.File, canonicalPath, shortName, format string, raw manifestSticker, expected [sha256.Size]byte) (Sticker, error) {
	info, err := file.Stat()
	if err != nil {
		return Sticker{}, fmt.Errorf("sticker bundle pack %q sticker %q inspect file: %w", shortName, raw.Emoji, err)
	}
	if !info.Mode().IsRegular() {
		return Sticker{}, fmt.Errorf("sticker bundle pack %q sticker %q is not a regular file", shortName, raw.Emoji)
	}
	if info.Size() != raw.Size {
		return Sticker{}, fmt.Errorf("sticker bundle pack %q sticker %q size is %d, manifest says %d", shortName, raw.Emoji, info.Size(), raw.Size)
	}
	var header [12]byte
	headerSize := 12
	if format == "tgs" {
		headerSize = 2
	}
	if _, err := io.ReadFull(file, header[:headerSize]); err != nil {
		return Sticker{}, fmt.Errorf("sticker bundle pack %q sticker %q read %s header: %w", shortName, raw.Emoji, format, err)
	}
	switch format {
	case "webp":
		if string(header[:4]) != "RIFF" || string(header[8:]) != "WEBP" {
			return Sticker{}, fmt.Errorf("sticker bundle pack %q sticker %q is not a WebP file", shortName, raw.Emoji)
		}
	case "tgs":
		if header[0] != 0x1f || header[1] != 0x8b {
			return Sticker{}, fmt.Errorf("sticker bundle pack %q sticker %q is not a gzip-compressed TGS file", shortName, raw.Emoji)
		}
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return Sticker{}, fmt.Errorf("sticker bundle pack %q sticker %q rewind file: %w", shortName, raw.Emoji, err)
	}
	digest := sha256.New()
	readBytes, err := io.Copy(digest, file)
	if err != nil {
		return Sticker{}, fmt.Errorf("sticker bundle pack %q sticker %q checksum file: %w", shortName, raw.Emoji, err)
	}
	if readBytes != raw.Size {
		return Sticker{}, fmt.Errorf("sticker bundle pack %q sticker %q changed size while validating: read %d, want %d", shortName, raw.Emoji, readBytes, raw.Size)
	}
	if !equalChecksum(digest.Sum(nil), expected[:]) {
		return Sticker{}, fmt.Errorf("sticker bundle pack %q sticker %q checksum does not match manifest", shortName, raw.Emoji)
	}
	mimeType := "image/webp"
	animated := false
	if format == "tgs" {
		mimeType = "application/x-tgsticker"
		animated = true
	}
	return Sticker{
		Emoji: raw.Emoji, File: raw.File, Path: canonicalPath, Size: raw.Size, SHA256: expected,
		Format: format, MimeType: mimeType, Animated: animated,
	}, nil
}

func validShortName(value string) bool {
	if len(value) == 0 || len(value) > maxShortNameBytes {
		return false
	}
	for _, char := range value {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '_' {
			return false
		}
	}
	return true
}

func validText(value string) bool {
	return value != "" && utf8.ValidString(value) && !strings.ContainsRune(value, '\x00')
}

func withinRoot(root, target string) bool {
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return false
	}
	return relative != "."
}

func equalChecksum(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	var diff byte
	for i := range left {
		diff |= left[i] ^ right[i]
	}
	return diff == 0
}
