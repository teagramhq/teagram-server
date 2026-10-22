package api_test

import (
	"testing"
	"time"

	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/api"
)

func TestHelpGetConfigAdvertisesDefaultReaction(t *testing.T) {
	getConfig := api.GetConfigSeqForTest(2, "127.0.0.1", 443, func() time.Time { return time.Unix(1_800_000_000, 0) })
	config, err := getConfig()
	if err != nil {
		t.Fatalf("help.getConfig: %v", err)
	}
	value, ok := config.GetReactionsDefault()
	if !ok {
		t.Fatal("help.getConfig omitted reactions_default")
	}
	reaction, ok := value.(*tg.ReactionEmoji)
	if !ok || reaction.Emoticon != "👍" {
		t.Fatalf("reactions_default = %#v, want reactionEmoji(👍)", value)
	}
}

func TestHelpGetAppConfigAdvertisesReactionLimits(t *testing.T) {
	result, err := api.GetAppConfigForTest(0)
	if err != nil {
		t.Fatalf("help.getAppConfig: %v", err)
	}
	config, ok := result.(*tg.HelpAppConfig)
	if !ok {
		t.Fatalf("help.getAppConfig result = %T, want *tg.HelpAppConfig", result)
	}
	object, ok := config.Config.(*tg.JSONObject)
	if !ok {
		t.Fatalf("app config = %T, want *tg.JSONObject", config.Config)
	}
	want := map[string]float64{
		"reactions_uniq_max":         11,
		"reactions_user_max_default": 1,
		"reactions_user_max_premium": 1,
	}
	for _, item := range object.Value {
		limit, found := want[item.Key]
		if !found {
			continue
		}
		number, ok := item.Value.(*tg.JSONNumber)
		if !ok || number.Value != limit {
			t.Errorf("%s = %#v, want JSON number %g", item.Key, item.Value, limit)
		}
		delete(want, item.Key)
	}
	if len(want) != 0 {
		t.Errorf("help.getAppConfig omitted reaction limits: %v", want)
	}
}
