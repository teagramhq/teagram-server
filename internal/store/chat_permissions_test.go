//nolint:testpackage // These assertions exercise the unexported default-rights policy.
package store

import (
	"errors"
	"testing"
)

func TestSendDocsBanDoesNotAllowStickersOutsideDemoGeneral(t *testing.T) {
	err := checkDefaultMessageRestriction([]string{"send_docs"}, false, true, []string{"send_stickers"})
	if !errors.Is(err, ErrChatWriteForbidden) {
		t.Fatalf("sticker under non-demo send_docs ban = %v, want ErrChatWriteForbidden", err)
	}
}

func TestDemoGeneralSendDocsBanAllowsStickers(t *testing.T) {
	if err := checkDemoGeneralChatStickerRestriction([]string{"send_docs"}, false, true, []string{"send_stickers"}); err != nil {
		t.Fatalf("demo General sticker under send_docs ban = %v, want success", err)
	}
}

func TestSendDocsBanRejectsGenericDocument(t *testing.T) {
	err := checkDefaultMessageRestriction([]string{"send_docs"}, false, true, nil)
	if !errors.Is(err, ErrChatWriteForbidden) {
		t.Fatalf("generic document under send_docs ban = %v, want ErrChatWriteForbidden", err)
	}
}
