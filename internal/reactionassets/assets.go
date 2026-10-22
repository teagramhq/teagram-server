package reactionassets

import (
	"embed"
	"encoding/json"
)

// Files embeds the pinned reaction artwork in the server binary.
//
//go:embed manifest.json static/*.webp tgs/*.tgs
var Files embed.FS

// Entry maps one standard reaction to its Twemoji source asset name.
type Entry struct {
	Emoji string `json:"emoji"`
	SVG   string `json:"svg"`
}

// Manifest returns the ordered mapping used to build the reaction catalog.
func Manifest() ([]Entry, error) {
	data, err := Files.ReadFile("manifest.json")
	if err != nil {
		return nil, err
	}
	var entries []Entry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}
