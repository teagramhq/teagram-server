package api

import (
	"strings"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestAppConfigInputPoliciesMatchValidationConstants(t *testing.T) {
	h := testHandlers(nil)
	var request bin.Buffer
	if err := (&tg.HelpGetAppConfigRequest{}).Encode(&request); err != nil {
		t.Fatalf("encode help.getAppConfig: %v", err)
	}
	response, err := h.handleGetAppConfig(&mtproto.Request{Buf: &request})
	if err != nil {
		t.Fatalf("help.getAppConfig: %v", err)
	}
	appConfig, ok := response.(*tg.HelpAppConfig)
	if !ok {
		t.Fatalf("response = %T, want *tg.HelpAppConfig", response)
	}
	object, ok := appConfig.Config.(*tg.JSONObject)
	if !ok {
		t.Fatalf("config = %T, want *tg.JSONObject", appConfig.Config)
	}
	values := make(map[string]any, len(object.Value))
	for _, value := range object.Value {
		values[value.Key] = value.Value
	}
	for key, want := range map[string]float64{
		"username_min_length":      usernameMinLength,
		"username_max_length":      usernameMaxLength,
		"display_name_max_length":  maxSignUpNameCodePoints,
		"bio_max_length":           maxProfileAboutRunes,
		"group_title_min_length":   minChatTitle,
		"group_title_max_length":   maxChatTitle,
		"group_about_max_length":   maxChatAbout,
		"channel_title_min_length": minChatTitle,
		"channel_title_max_length": maxChatTitle,
		"channel_about_max_length": maxChannelAbout,
		"message_max_length":       messageLengthMax,
		"caption_max_length":       captionLengthMax,
		"upload_max_part_bytes":    store.MaxPartBytes,
	} {
		got, ok := values[key].(*tg.JSONNumber)
		if !ok || got.Value != want {
			t.Errorf("%s = %v, want %g", key, values[key], want)
		}
	}
	pattern, ok := values["username_pattern"].(*tg.JSONString)
	if !ok || pattern.Value != usernamePattern || usernameRE.String() != usernamePattern {
		t.Errorf("username_pattern = %v, validator pattern = %q, want %q", values["username_pattern"], usernameRE.String(), usernamePattern)
	}
	if !validateUsername(strings.Repeat("a", usernameMinLength)) || !validateUsername(strings.Repeat("a", usernameMaxLength)) {
		t.Fatal("username validator rejected a published length boundary")
	}
	if validateUsername(strings.Repeat("a", usernameMinLength-1)) || validateUsername(strings.Repeat("a", usernameMaxLength+1)) {
		t.Fatal("username validator accepted a value outside the published length bounds")
	}
	if config := DefaultConfig(1, "127.0.0.1", 443); config.MessageLengthMax != messageLengthMax || config.CaptionLengthMax != captionLengthMax {
		t.Fatalf("help.getConfig message/caption limits = %d/%d, want %d/%d", config.MessageLengthMax, config.CaptionLengthMax, messageLengthMax, captionLengthMax)
	}
}
