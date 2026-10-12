//nolint:testpackage // These unit tests exercise the private TL settings normalizer.
package api

import (
	"testing"

	"github.com/gotd/td/tg"
)

func TestNormalizeNotifySettingsStoredFields(t *testing.T) {
	t.Parallel()
	var input tg.InputPeerNotifySettings
	input.SetShowPreviews(false)
	input.SetSilent(true)
	input.SetMuteUntil(-5)
	input.SetSound(&tg.NotificationSoundNone{})
	input.SetStoriesMuted(true)
	input.SetStoriesHideSender(true)
	input.SetStoriesSound(&tg.NotificationSoundDefault{})

	got, err := normalizeNotifySettings(input)
	if err != nil {
		t.Fatalf("normalize notification settings: %v", err)
	}
	if got.ShowPreviews == nil || *got.ShowPreviews {
		t.Fatalf("show_previews = %v, want explicit false", got.ShowPreviews)
	}
	if got.Silent == nil || !*got.Silent {
		t.Fatalf("silent = %v, want explicit true", got.Silent)
	}
	if got.MuteUntil == nil || *got.MuteUntil != 0 {
		t.Fatalf("mute_until = %v, want explicit 0", got.MuteUntil)
	}
	if got.Sound == nil || *got.Sound != 1 {
		t.Fatalf("sound = %v, want stored none value 1", got.Sound)
	}
}

func TestNormalizeNotifySettingsKeepsExplicitZeroDistinctFromUnset(t *testing.T) {
	t.Parallel()
	var explicit tg.InputPeerNotifySettings
	explicit.SetMuteUntil(0)
	got, err := normalizeNotifySettings(explicit)
	if err != nil {
		t.Fatalf("normalize explicit zero mute: %v", err)
	}
	if got.MuteUntil == nil || *got.MuteUntil != 0 {
		t.Fatalf("explicit mute_until = %v, want non-nil zero", got.MuteUntil)
	}

	empty, err := normalizeNotifySettings(tg.InputPeerNotifySettings{})
	if err != nil {
		t.Fatalf("normalize unset fields: %v", err)
	}
	if empty.MuteUntil != nil {
		t.Fatalf("unset mute_until = %v, want nil", empty.MuteUntil)
	}
}

func TestNormalizeNotifySettingsRefusesCustomSounds(t *testing.T) {
	t.Parallel()
	for name, sound := range map[string]tg.NotificationSoundClass{
		"local":    &tg.NotificationSoundLocal{},
		"ringtone": &tg.NotificationSoundRingtone{},
	} {
		t.Run(name, func(t *testing.T) {
			var input tg.InputPeerNotifySettings
			input.SetMuteUntil(123)
			input.SetSound(sound)
			if _, err := normalizeNotifySettings(input); err == nil {
				t.Fatal("custom sound was accepted")
			}
		})
	}
}
