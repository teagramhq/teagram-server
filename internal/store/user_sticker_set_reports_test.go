package store_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/teagramhq/teagram-server/internal/store"
)

func TestUserStickerSetReportQuotaSerializesConcurrentReports(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	reporter := mustUser(t, s, "+15559421001")
	setIDs := make([]int64, store.UserStickerSetReportsPerHour+1)
	for i := range setIDs {
		setIDs[i] = seedReportSet(t, ctx, s, reporter.ID, fmt.Sprintf("quota_set_%02d", i+1), int64(i+1))
	}

	errCh := make(chan error, len(setIDs))
	var wg sync.WaitGroup
	for _, setID := range setIDs {
		wg.Add(1)
		go func(setID int64) {
			defer wg.Done()
			errCh <- s.CreateUserStickerSetReportsForMessage(
				ctx, reporter.ID, []int64{setID}, store.PeerTypeUser, reporter.ID, 1, "copyright", "",
			)
		}(setID)
	}
	wg.Wait()
	close(errCh)
	rateLimited := 0
	for err := range errCh {
		switch {
		case err == nil:
		case errors.Is(err, store.ErrUserStickerSetReportRateLimited):
			rateLimited++
		default:
			t.Fatalf("concurrent report returned unexpected error: %v", err)
		}
	}
	if rateLimited != 1 {
		t.Fatalf("concurrent reports rate-limited %d calls, want one", rateLimited)
	}
	var count int
	if err := store.StorePool(s).QueryRow(ctx, `SELECT count(*) FROM user_sticker_set_reports WHERE reporter_id = $1`, reporter.ID).Scan(&count); err != nil {
		t.Fatalf("count reports after concurrent requests: %v", err)
	}
	if count != store.UserStickerSetReportsPerHour {
		t.Fatalf("concurrent report rows = %d, want %d", count, store.UserStickerSetReportsPerHour)
	}
}

func TestUserStickerSetReportsDeduplicateOnlyIdenticalOpenReports(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	owner := mustUser(t, s, "+15559421011")
	reporter := mustUser(t, s, "+15559421012")
	setID := seedReportSet(t, ctx, s, owner.ID, "distinct_reports", 1011)
	create := func(messageID int64, reason, comment string) {
		t.Helper()
		if err := s.CreateUserStickerSetReportsForMessage(
			ctx, reporter.ID, []int64{setID}, store.PeerTypeUser, owner.ID, messageID, reason, comment,
		); err != nil {
			t.Fatalf("create report for message %d (%s): %v", messageID, reason, err)
		}
	}
	count := func() int {
		t.Helper()
		var reports int
		if err := store.StorePool(s).QueryRow(ctx, `
			SELECT count(*) FROM user_sticker_set_reports WHERE reporter_id = $1 AND set_id = $2
		`, reporter.ID, setID).Scan(&reports); err != nil {
			t.Fatalf("count reports: %v", err)
		}
		return reports
	}

	create(11, "copyright", "same report")
	create(11, "copyright", "same report")
	if got := count(); got != 1 {
		t.Fatalf("identical open reports = %d, want one", got)
	}
	create(12, "copyright", "same report")
	create(11, "abuse", "same report")
	if got := count(); got != 3 {
		t.Fatalf("distinct message or reason reports = %d, want three", got)
	}

	var dismissedID int64
	if err := store.StorePool(s).QueryRow(ctx, `
		SELECT id FROM user_sticker_set_reports
		WHERE reporter_id = $1 AND set_id = $2 AND message_id = 11 AND reason = 'copyright'
	`, reporter.ID, setID).Scan(&dismissedID); err != nil {
		t.Fatalf("find report to dismiss: %v", err)
	}
	if err := s.DismissUserStickerSetReport(ctx, dismissedID); err != nil {
		t.Fatalf("dismiss report: %v", err)
	}
	create(11, "copyright", "same report")
	if got := count(); got != 4 {
		t.Fatalf("reports after resubmission of dismissed report = %d, want four", got)
	}
	var openReports, dismissedReports int
	if err := store.StorePool(s).QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE status = 'open'), count(*) FILTER (WHERE status = 'dismissed')
		FROM user_sticker_set_reports WHERE reporter_id = $1 AND set_id = $2
	`, reporter.ID, setID).Scan(&openReports, &dismissedReports); err != nil {
		t.Fatalf("count report states: %v", err)
	}
	if openReports != 3 || dismissedReports != 1 {
		t.Fatalf("open/dismissed reports = %d/%d, want 3/1", openReports, dismissedReports)
	}
}

func TestSweepExpiredUserStickerSetReportsUsesClosedAtAnd180DayRetention(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	creator := mustUser(t, s, "+15559421002")
	reporter := mustUser(t, s, "+15559421003")
	setIDs := []int64{
		seedReportSet(t, ctx, s, creator.ID, "retention_open", 1001),
		seedReportSet(t, ctx, s, creator.ID, "retention_dismissed", 1002),
		seedReportSet(t, ctx, s, creator.ID, "retention_actioned", 1003),
		seedReportSet(t, ctx, s, creator.ID, "retention_recent", 1004),
	}
	for i, setID := range setIDs {
		status := []string{"open", "dismissed", "actioned", "dismissed"}[i]
		closedAt := "NULL"
		switch i {
		case 1, 2:
			closedAt = "statement_timestamp() - interval '181 days'"
		case 3:
			closedAt = "statement_timestamp() - interval '179 days'"
		}
		if _, err := store.StorePool(s).Exec(ctx, fmt.Sprintf(`
			INSERT INTO user_sticker_set_reports (
				reporter_id, set_id, message_peer_type, message_peer_id, message_id, reason, status, created_at, closed_at
			) VALUES ($1, $2, 1, $3, 1, 'copyright', $4, statement_timestamp() - interval '365 days', %s)
		`, closedAt), reporter.ID, setID, reporter.ID, status); err != nil {
			t.Fatalf("seed %s report: %v", status, err)
		}
	}

	deleted, err := s.SweepExpiredUserStickerSetReports(ctx)
	if err != nil {
		t.Fatalf("sweep expired reports: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("expired reports deleted = %d, want two closed reports past 180 days", deleted)
	}
	var remaining int
	if err := store.StorePool(s).QueryRow(ctx, `SELECT count(*) FROM user_sticker_set_reports`).Scan(&remaining); err != nil {
		t.Fatalf("count remaining reports: %v", err)
	}
	if remaining != 2 {
		t.Fatalf("remaining reports = %d, want the open and recently closed reports", remaining)
	}
}

func TestTakedownActionsOpenStickerSetReports(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	owner := mustUser(t, s, "+15559421004")
	reporter := mustUser(t, s, "+15559421005")
	data := []byte("hello world")
	file := stickerSetFileWithBytes(t, s, owner, data)
	set, err := createOneFileStickerSet(ctx, s, owner, file, "Reported", "reported_action", data)
	if err != nil {
		t.Fatalf("create sticker set: %v", err)
	}
	if err := s.CreateUserStickerSetReportsForMessage(ctx, reporter.ID, []int64{set.ID}, store.PeerTypeUser, owner.ID, 1, "abuse", ""); err != nil {
		t.Fatalf("create sticker-set report: %v", err)
	}
	ref, err := store.StickerSetAdminRefByID(set.ID)
	if err != nil {
		t.Fatalf("build set reference: %v", err)
	}
	if _, err := s.TakedownUserStickerSet(ctx, ref, "reviewed report", make([]byte, 32)); err != nil {
		t.Fatalf("take down reported set: %v", err)
	}
	var status string
	if err := store.StorePool(s).QueryRow(ctx, `SELECT status FROM user_sticker_set_reports WHERE reporter_id = $1`, reporter.ID).Scan(&status); err != nil {
		t.Fatalf("read report after takedown: %v", err)
	}
	if status != "actioned" {
		t.Fatalf("report status after takedown = %q, want actioned", status)
	}
}

func seedReportSet(t *testing.T, ctx context.Context, s *store.Store, creatorID int64, shortName string, accessHash int64) int64 {
	t.Helper()
	var setID int64
	err := store.StorePool(s).QueryRow(ctx, `
		INSERT INTO user_created_sticker_sets (access_hash, creator_id, title, short_name, kind, item_count)
		VALUES ($1, $2, 'Report test set', $3, 'regular', 1)
		RETURNING id
	`, accessHash, creatorID, shortName).Scan(&setID)
	if err != nil {
		t.Fatalf("seed report set %q: %v", shortName, err)
	}
	return setID
}
