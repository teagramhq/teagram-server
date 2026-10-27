package api

import (
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/mtproto"
)

func TestAppConfigAdvertisesStickerImportOff(t *testing.T) {
	h := testHandlers(nil)
	h.stickerImportMode = config.StickerImportOff
	var request bin.Buffer
	if err := (&tg.HelpGetAppConfigRequest{}).Encode(&request); err != nil {
		t.Fatalf("encode help.getAppConfig: %v", err)
	}
	response, err := h.handleGetAppConfig(&mtproto.Request{Buf: &request})
	if err != nil {
		t.Fatalf("help.getAppConfig: %v", err)
	}
	got, ok := response.(*tg.HelpAppConfig)
	if !ok {
		t.Fatalf("response = %T, want *tg.HelpAppConfig", response)
	}
	object, ok := got.Config.(*tg.JSONObject)
	if !ok {
		t.Fatalf("config = %T, want *tg.JSONObject", got.Config)
	}
	for _, value := range object.Value {
		if value.Key != "sticker_import_mode" {
			continue
		}
		mode, ok := value.Value.(*tg.JSONString)
		if !ok || mode.Value != string(config.StickerImportOff) {
			t.Fatalf("sticker_import_mode = %v, want %q", value.Value, config.StickerImportOff)
		}
		return
	}
	t.Fatal("help.appConfig omitted sticker_import_mode")
}
