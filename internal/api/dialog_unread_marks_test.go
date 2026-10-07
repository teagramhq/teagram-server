//nolint:testpackage // This test verifies the unbound guard runs before request decoding.
package api

import (
	"context"
	"errors"
	"testing"

	"github.com/gotd/td/bin"

	"github.com/teagramhq/teagram-server/internal/mtproto"
)

func TestMarkDialogUnreadRejectsUnboundBeforeDecodeOrStorage(t *testing.T) {
	t.Parallel()
	_, err := testHandlers(nil).handleMarkDialogUnread(&mtproto.Request{
		Ctx:    context.Background(),
		UserID: 0,
		Buf:    &bin.Buffer{},
	})
	if !errors.Is(err, errAuthKeyUnreg) {
		t.Fatalf("unbound messages.markDialogUnread error = %v, want AUTH_KEY_UNREGISTERED", err)
	}
}
