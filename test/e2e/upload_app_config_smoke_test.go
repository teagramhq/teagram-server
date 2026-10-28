package e2e_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/config"
)

func testSmokeUploadAppConfigByteLimit(t *testing.T) {
	t.Helper()
	f := newSmokeFixtureWithSetup(t, config.RegistrationClosed, func(f *smokeFixture) {
		f.uploadMaxFileBytes = 512000
	})
	const phone = "+15551048461"
	seedSmokeUsers(t, f, phone)
	client := newSmokeClient(t, f, "upload app config", phone)

	var appConfig *tg.HelpAppConfig
	if err := client.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		result, err := api.HelpGetAppConfig(ctx, 6)
		if err != nil {
			return err
		}
		var ok bool
		appConfig, ok = result.(*tg.HelpAppConfig)
		if !ok {
			return fmt.Errorf("help.getAppConfig = %T, want *tg.HelpAppConfig", result)
		}
		return nil
	}); err != nil {
		t.Fatalf("help.getAppConfig with cached hash 6: %v", err)
	}
	if appConfig.Hash == 6 {
		t.Errorf("help.getAppConfig hash = %d, want changed hash for configured upload limit", appConfig.Hash)
	}
	object, ok := appConfig.Config.(*tg.JSONObject)
	if !ok {
		t.Fatalf("help.getAppConfig config = %T, want *tg.JSONObject", appConfig.Config)
	}
	found := false
	for _, value := range object.Value {
		if value.Key != "upload_max_file_bytes" {
			continue
		}
		number, ok := value.Value.(*tg.JSONNumber)
		if !ok {
			t.Errorf("upload_max_file_bytes = %T, want *tg.JSONNumber", value.Value)
			continue
		}
		if number.Value != 512000 {
			t.Errorf("upload_max_file_bytes = %g, want 512000", number.Value)
		}
		found = true
	}
	if !found {
		t.Error("help.getAppConfig omitted upload_max_file_bytes")
	}
	wantPolicies := map[string]float64{
		"username_min_length":      2,
		"username_max_length":      32,
		"display_name_max_length":  64,
		"bio_max_length":           70,
		"group_title_min_length":   1,
		"group_title_max_length":   255,
		"group_about_max_length":   255,
		"channel_title_min_length": 1,
		"channel_title_max_length": 255,
		"channel_about_max_length": 255,
		"message_max_length":       4096,
		"caption_max_length":       1024,
		"upload_max_part_bytes":    512 * 1024,
	}
	patternFound := false
	for _, value := range object.Value {
		if value.Key == "username_pattern" {
			pattern, ok := value.Value.(*tg.JSONString)
			if !ok || pattern.Value != `^[a-zA-Z][a-zA-Z0-9_]{1,31}$` {
				t.Errorf("username_pattern = %#v, want the server username pattern", value.Value)
			}
			patternFound = true
			continue
		}
		want, exists := wantPolicies[value.Key]
		if !exists {
			continue
		}
		number, ok := value.Value.(*tg.JSONNumber)
		if !ok || number.Value != want {
			t.Errorf("%s = %#v, want %g", value.Key, value.Value, want)
		}
		delete(wantPolicies, value.Key)
	}
	if len(wantPolicies) != 0 {
		t.Errorf("help.getAppConfig omitted input policies: %v", wantPolicies)
	}
	if !patternFound {
		t.Error("help.getAppConfig omitted username_pattern")
	}
}
