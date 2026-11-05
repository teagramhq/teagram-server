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
	owner := mustUser(t, s, "+15559421000")
	reporter := mustUser(t, s, "+15559421001")
	messageIDs := make([]int64, store.UserStickerSetReportsPerHour+1)
	for i := range messageIDs {
		_, file := seedReportableSet(t, ctx, s, owner, fmt.Sprintf("quota_set_%02d", i+1))
		messageIDs[i] = sendReportableSticker(t, ctx, s, owner, reporter, file, int64(i+1))
	}

	errCh := make(chan error, len(messageIDs))
	var wg sync.WaitGroup
	for _, messageID := range messageIDs {
		wg.Add(1)
		go func(messageID int64) {
			defer wg.Done()
			_, err := s.CreateUserStickerSetReports(
				ctx, reporter.ID, store.PeerTypeUser, owner.ID, []int64{messageID}, "copyright", "",
			)
			errCh <- err
		}(messageID)
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
	setID, file := seedReportableSet(t, ctx, s, owner, "distinct_reports")
	first := sendReportableSticker(t, ctx, s, owner, reporter, file, 1011)
	second := sendReportableSticker(t, ctx, s, owner, reporter, file, 1012)
	create := func(messageID int64, reason, comment string) {
		t.Helper()
		if visible, err := s.CreateUserStickerSetReports(
			ctx, reporter.ID, store.PeerTypeUser, owner.ID, []int64{messageID}, reason, comment,
		); err != nil || !visible {
			t.Fatalf("create report for message %d (%s): visible %t, err %v", messageID, reason, visible, err)
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

	create(first, "copyright", "same report")
	create(first, "copyright", "same report")
	if got := count(); got != 1 {
		t.Fatalf("identical open reports = %d, want one", got)
	}
	create(second, "copyright", "same report")
	create(first, "abuse", "same report")
	if got := count(); got != 3 {
		t.Fatalf("distinct message or reason reports = %d, want three", got)
	}

	var dismissedID int64
	if err := store.StorePool(s).QueryRow(ctx, `
		SELECT id FROM user_sticker_set_reports
		WHERE reporter_id = $1 AND set_id = $2 AND message_id = $3 AND reason = 'copyright'
	`, reporter.ID, setID, first).Scan(&dismissedID); err != nil {
		t.Fatalf("find report to dismiss: %v", err)
	}
	if err := s.DismissUserStickerSetReport(ctx, dismissedID); err != nil {
		t.Fatalf("dismiss report: %v", err)
	}
	create(first, "copyright", "same report")
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
	setID, file := seedReportableSet(t, ctx, s, owner, "reported_action")
	messageID := sendReportableSticker(t, ctx, s, owner, reporter, file, 1004)
	if visible, err := s.CreateUserStickerSetReports(ctx, reporter.ID, store.PeerTypeUser, owner.ID, []int64{messageID}, "abuse", ""); err != nil || !visible {
		t.Fatalf("create sticker-set report: visible %t, err %v", visible, err)
	}
	ref, err := store.StickerSetAdminRefByID(setID)
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

// A takedown that commits before the report transaction resolves the set leaves
// the reporter with a completed flow and no stored report. That is intended:
// the set is already unavailable, so there is nothing left to action.
func TestCreateUserStickerSetReportsSkipsSetTakenDownBeforeInsert(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	owner := mustUser(t, s, "+15559421021")
	reporter := mustUser(t, s, "+15559421022")
	setID, file := seedReportableSet(t, ctx, s, owner, "taken_down_first")
	messageID := sendReportableSticker(t, ctx, s, owner, reporter, file, 1021)
	visible, err := s.UserStickerSetReportMessagesVisible(ctx, reporter.ID, store.PeerTypeUser, owner.ID, []int64{messageID})
	if err != nil || !visible {
		t.Fatalf("message visibility before takedown = %t, err %v; want visible", visible, err)
	}
	ref, err := store.StickerSetAdminRefByID(setID)
	if err != nil {
		t.Fatalf("build set reference: %v", err)
	}
	if _, err := s.TakedownUserStickerSet(ctx, ref, "taken down first", make([]byte, 32)); err != nil {
		t.Fatalf("take down set: %v", err)
	}

	visible, err = s.CreateUserStickerSetReports(ctx, reporter.ID, store.PeerTypeUser, owner.ID, []int64{messageID}, "copyright", "late")
	if err != nil || !visible {
		t.Fatalf("report after takedown = visible %t, err %v; want a nil error", visible, err)
	}
	var count int
	if err := store.StorePool(s).QueryRow(ctx, `SELECT count(*) FROM user_sticker_set_reports`).Scan(&count); err != nil {
		t.Fatalf("count reports after takedown: %v", err)
	}
	if count != 0 {
		t.Fatalf("reports for a set taken down before the insert = %d, want none", count)
	}
}

func TestCreateUserStickerSetReportsIgnoresMessagesOutsideReporterSpace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	owner := mustUser(t, s, "+15559421031")
	reporter := mustUser(t, s, "+15559421032")
	stranger := mustUser(t, s, "+15559421033")
	_, file := seedReportableSet(t, ctx, s, owner, "outside_space")
	messageID := sendReportableSticker(t, ctx, s, owner, reporter, file, 1031)

	// The same local id in a stranger's own space, or in a dialog the reporter
	// does not have, must not resolve.
	for name, peerID := range map[string]int64{"other dialog": stranger.ID, "other reporter": owner.ID} {
		reporterID := reporter.ID
		if name == "other reporter" {
			reporterID = stranger.ID
		}
		visible, err := s.CreateUserStickerSetReports(ctx, reporterID, store.PeerTypeUser, peerID, []int64{messageID}, "abuse", "")
		if err != nil || visible {
			t.Fatalf("%s: report = visible %t, err %v; want not visible", name, visible, err)
		}
	}
	var count int
	if err := store.StorePool(s).QueryRow(ctx, `SELECT count(*) FROM user_sticker_set_reports`).Scan(&count); err != nil {
		t.Fatalf("count reports: %v", err)
	}
	if count != 0 {
		t.Fatalf("reports for messages outside the reporter's space = %d, want none", count)
	}
}

// seedReportableSet creates a live one-sticker set owned by owner and returns
// its id and the sticker file.
func seedReportableSet(t *testing.T, ctx context.Context, s *store.Store, owner store.User, shortName string) (int64, store.File) {
	t.Helper()
	data := []byte("hello world")
	file := stickerSetFileWithBytes(t, s, owner, data)
	set, err := createOneFileStickerSet(ctx, s, owner, file, "Reported", shortName, data)
	if err != nil {
		t.Fatalf("create sticker set %q: %v", shortName, err)
	}
	return set.ID, file
}

// sendReportableSticker sends the sticker from owner to reporter and returns
// the id of the copy in the reporter's own message space.
func sendReportableSticker(t *testing.T, ctx context.Context, s *store.Store, owner, reporter store.User, file store.File, randomID int64) int64 {
	t.Helper()
	sent, _, _, duplicate, err := s.SendStickerMessageWithEntities(ctx, owner.ID, reporter.ID, "cat", randomID, file.ID, 0, nil)
	if err != nil || duplicate {
		t.Fatalf("send sticker %d: duplicate %t, err %v", randomID, duplicate, err)
	}
	history, err := s.History(ctx, reporter.ID, store.PeerTypeUser, owner.ID, 0, 100)
	if err != nil {
		t.Fatalf("read reporter history: %v", err)
	}
	for _, message := range history {
		if message.PeerLocalID == sent.LocalID {
			return message.LocalID
		}
	}
	t.Fatalf("reporter history has no copy of sticker %d", randomID)
	return 0
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
