//nolint:testpackage // This test exercises the unexported calendar error mapping.
package api

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/teagramhq/teagram-server/internal/store"
)

func TestCalendarStoreErrorLogsRollbackFailureAlongsideNotMember(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	h := &handlers{log: slog.New(slog.NewTextHandler(&logs, nil))}

	if got := h.calendarStoreError(42, store.ErrNotMember); got == nil || got.Error() != errPeerIDInvalid.Error() {
		t.Fatalf("plain ErrNotMember mapped to %v, want PEER_ID_INVALID", got)
	}
	if logs.Len() != 0 {
		t.Fatalf("plain ErrNotMember logged unexpectedly: %q", logs.String())
	}

	rollbackErr := errors.New("rollback failed")
	err := errors.Join(store.ErrNotMember, fmt.Errorf("rollback search results calendar snapshot: %w", rollbackErr))
	if got := h.calendarStoreError(42, err); got == nil || got.Error() != errPeerIDInvalid.Error() {
		t.Fatalf("joined ErrNotMember mapped to %v, want PEER_ID_INVALID", got)
	}
	if !strings.Contains(logs.String(), "rollback failed") {
		t.Fatalf("joined rollback failure was not logged: %q", logs.String())
	}
	if !strings.Contains(logs.String(), "user_id=42") {
		t.Fatalf("joined rollback failure log lacks user context: %q", logs.String())
	}
}
