package api

import (
	"context"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/mtproto"
)

func TestGetAppConfigAdvertisesUploadPartLimits(t *testing.T) {
	h := testHandlers(nil)
	h.maxFileBytes = 512000
	var request bin.Buffer
	if err := (&tg.HelpGetAppConfigRequest{}).Encode(&request); err != nil {
		t.Fatal(err)
	}
	result, err := h.handleGetAppConfig(&mtproto.Request{Ctx: context.Background(), Buf: &request})
	if err != nil {
		t.Fatalf("help.getAppConfig: %v", err)
	}
	appConfig, ok := result.(*tg.HelpAppConfig)
	if !ok {
		t.Fatalf("result = %T, want *tg.HelpAppConfig", result)
	}
	object, ok := appConfig.Config.(*tg.JSONObject)
	if !ok {
		t.Fatalf("config = %T, want *tg.JSONObject", appConfig.Config)
	}
	want := map[string]float64{
		"upload_max_fileparts_default": 16,
		"upload_max_fileparts_premium": 16,
	}
	for _, value := range object.Value {
		limit, found := want[value.Key]
		if !found {
			continue
		}
		number, ok := value.Value.(*tg.JSONNumber)
		if !ok || number.Value != limit {
			t.Errorf("%s = %#v, want JSON number %g", value.Key, value.Value, limit)
		}
		delete(want, value.Key)
	}
	if len(want) != 0 {
		t.Errorf("help.getAppConfig omitted upload limits: %v", want)
	}
}
