package e2e_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestSearchResultsCalendarKeepsDateJumpSessionAndHistory(t *testing.T) {
	f := newSmokeFixtureWithSetup(t, config.RegistrationClosed, func(f *smokeFixture) {
		f.photoThumbs = buildSmokePhotoThumbSupervisor(t)
	})
	const phoneA, phoneB = "+15552888001", "+15552888002"
	seedSmokeUsers(t, f, phoneA, phoneB)
	a := newSmokeClient(t, f, "calendar sender", phoneA)
	b := newSmokeClient(t, f, "calendar peer", phoneB)
	peer := peerUser(a.id, b.id)

	texts := []string{
		"calendar history before one",
		"calendar history before two",
		"calendar history before three",
		"calendar history after",
	}
	dates := []time.Time{
		time.Date(2024, time.May, 1, 12, 0, 0, 0, time.UTC),
		time.Date(2024, time.May, 2, 12, 0, 0, 0, time.UTC),
		time.Date(2024, time.May, 3, 10, 0, 0, 0, time.UTC),
		time.Date(2024, time.May, 3, 14, 0, 0, 0, time.UTC),
	}
	for i, message := range texts {
		if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
			_, err := client.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
				Peer: peer, Message: message, RandomID: int64(2888001 + i),
			})
			return err
		}); err != nil {
			t.Fatalf("send history message %q: %v", message, err)
		}
	}

	photoBody := smokeJPEGAtUploadSize(t, smokePhotoPartCount)
	const photoFileID = int64(2888011)
	photoParts := uploadSmokeMediaParts(t, f.ctx, a, photoFileID, photoBody, smokePhotoPartSize)
	if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		_, err := client.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
			Peer: peer, Message: "calendar photo", RandomID: 2888012,
			Media: &tg.InputMediaUploadedPhoto{File: &tg.InputFile{
				ID: photoFileID, Parts: photoParts, Name: "calendar.jpg", MD5Checksum: smokePhotoMD5(photoBody),
			}},
		})
		return err
	}); err != nil {
		t.Fatalf("send calendar photo: %v", err)
	}

	videoBody := []byte("stored calendar video payload")
	const videoFileID = int64(2888021)
	videoParts := uploadSmokeMediaParts(t, f.ctx, a, videoFileID, videoBody, len(videoBody))
	if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		_, err := client.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
			Peer: peer, Message: "calendar video", RandomID: 2888022,
			Media: &tg.InputMediaUploadedDocument{
				File:     &tg.InputFile{ID: videoFileID, Parts: videoParts, Name: "calendar-video.mp4"},
				MimeType: "video/mp4",
				Attributes: []tg.DocumentAttributeClass{
					&tg.DocumentAttributeFilename{FileName: "calendar-video.mp4"},
					&tg.DocumentAttributeVideo{Duration: 1, W: 320, H: 240, SupportsStreaming: true},
				},
			},
		})
		return err
	}); err != nil {
		t.Fatalf("send calendar video: %v", err)
	}

	conn, err := pgx.Connect(f.ctx, f.dsn)
	if err != nil {
		t.Fatalf("connect to seed history dates: %v", err)
	}
	defer func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close date seed connection: %v", err)
		}
	}()
	for i, message := range texts {
		tag, err := conn.Exec(f.ctx, `
			UPDATE messages SET date = $1
			WHERE owner_id = $2 AND peer_type = $3 AND peer_id = $4 AND message = $5
		`, dates[i], a.id, int16(store.PeerTypeUser), b.id, message)
		if err != nil {
			t.Fatalf("seed date for %q: %v", message, err)
		}
		if tag.RowsAffected() != 1 {
			t.Fatalf("seed date for %q changed %d rows, want 1", message, tag.RowsAffected())
		}
	}
	seedMessageDate := func(message string, date time.Time) int64 {
		t.Helper()
		var localID int64
		if err := conn.QueryRow(f.ctx, `
			UPDATE messages SET date = $1
			WHERE owner_id = $2 AND peer_type = $3 AND peer_id = $4 AND message = $5
			RETURNING local_id
		`, date, a.id, int16(store.PeerTypeUser), b.id, message).Scan(&localID); err != nil {
			t.Fatalf("seed date for %q: %v", message, err)
		}
		return localID
	}
	photoDate := time.Date(2024, time.April, 2, 15, 0, 0, 0, time.UTC)
	videoDate := time.Date(2024, time.April, 3, 11, 0, 0, 0, time.UTC)
	photoID := seedMessageDate("calendar photo", photoDate)
	videoID := seedMessageDate("calendar video", videoDate)

	calendarAt := func(offsetID, offsetDate int) (*tg.MessagesSearchResultsCalendar, error) {
		var calendar *tg.MessagesSearchResultsCalendar
		err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
			var err error
			calendar, err = client.MessagesGetSearchResultsCalendar(ctx, &tg.MessagesGetSearchResultsCalendarRequest{
				Peer: peer, Filter: &tg.InputMessagesFilterPhotoVideo{}, OffsetID: offsetID, OffsetDate: offsetDate,
			})
			return err
		})
		return calendar, err
	}
	monthEnd := int(time.Date(2024, time.April, 30, 23, 59, 59, 0, time.UTC).Unix())
	calendar, err := calendarAt(0, monthEnd)
	if err != nil {
		t.Fatalf("messages.getSearchResultsCalendar: %v", err)
	}
	if calendar.Count != 2 || len(calendar.Periods) != 2 || len(calendar.Messages) != 2 {
		t.Fatalf("photo/video calendar = count %d, periods %d, messages %d; want 2/2/2", calendar.Count, len(calendar.Periods), len(calendar.Messages))
	}
	wantPeriods := []tg.SearchResultsCalendarPeriod{
		{Date: int(time.Date(2024, time.April, 3, 0, 0, 0, 0, time.UTC).Unix()), MinMsgID: int(videoID), MaxMsgID: int(videoID), Count: 1},
		{Date: int(time.Date(2024, time.April, 2, 0, 0, 0, 0, time.UTC).Unix()), MinMsgID: int(photoID), MaxMsgID: int(photoID), Count: 1},
	}
	for i, want := range wantPeriods {
		if calendar.Periods[i] != want {
			t.Fatalf("calendar period %d = %+v, want %+v", i, calendar.Periods[i], want)
		}
	}
	videoMessage, ok := calendar.Messages[0].(*tg.Message)
	if !ok || videoMessage.Message != "calendar video" {
		t.Fatalf("calendar video message = %T/%+v, want video caption", calendar.Messages[0], calendar.Messages[0])
	}
	videoMedia, ok := videoMessage.Media.(*tg.MessageMediaDocument)
	if !ok {
		t.Fatalf("calendar video media = %T, want *tg.MessageMediaDocument", videoMessage.Media)
	}
	videoDocument, ok := videoMedia.Document.(*tg.Document)
	if !ok || videoDocument.MimeType != "video/mp4" || videoDocument.Size != int64(len(videoBody)) {
		t.Fatalf("calendar video document = %T/%+v, want stored video/mp4 payload", videoMedia.Document, videoMedia.Document)
	}
	photoMessage, ok := calendar.Messages[1].(*tg.Message)
	if !ok || photoMessage.Message != "calendar photo" {
		t.Fatalf("calendar photo message = %T/%+v, want photo caption", calendar.Messages[1], calendar.Messages[1])
	}
	photoMedia, ok := photoMessage.Media.(*tg.MessageMediaPhoto)
	if !ok {
		t.Fatalf("calendar photo media = %T, want *tg.MessageMediaPhoto", photoMessage.Media)
	}
	photo, ok := photoMedia.Photo.(*tg.Photo)
	if !ok || photo.ID <= 0 || len(photo.Sizes) == 0 {
		t.Fatalf("calendar photo payload = %T/%+v, want stored photo with sizes", photoMedia.Photo, photoMedia.Photo)
	}

	nextPage, err := calendarAt(int(videoID), monthEnd)
	if err != nil {
		t.Fatalf("messages.getSearchResultsCalendar next page: %v", err)
	}
	if nextPage.Count != 2 || len(nextPage.Periods) != 1 || nextPage.Periods[0] != wantPeriods[1] || len(nextPage.Messages) != 1 {
		t.Fatalf("next calendar page = count %d, periods %+v, messages %d; want total count 2 and only photo result", nextPage.Count, nextPage.Periods, len(nextPage.Messages))
	}
	earlyPage, err := calendarAt(0, int(time.Date(2024, time.April, 2, 23, 59, 59, 0, time.UTC).Unix()))
	if err != nil {
		t.Fatalf("messages.getSearchResultsCalendar earlier date page: %v", err)
	}
	if earlyPage.Count != 2 || len(earlyPage.Periods) != 1 || earlyPage.Periods[0] != wantPeriods[1] || len(earlyPage.Messages) != 1 {
		t.Fatalf("earlier date page = count %d, periods %+v, messages %d; want total count 2 and only photo result", earlyPage.Count, earlyPage.Periods, len(earlyPage.Messages))
	}

	var history tg.MessagesMessagesClass
	date := time.Date(2024, time.May, 3, 12, 0, 0, 0, time.UTC)
	if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		var err error
		history, err = client.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
			Peer: peer, OffsetDate: int(date.Unix()), AddOffset: -2, Limit: 4,
		})
		return err
	}); err != nil {
		t.Fatalf("messages.getHistory around chosen date: %v", err)
	}
	page, ok := history.(*tg.MessagesMessages)
	if !ok {
		t.Fatalf("date history result = %T, want *tg.MessagesMessages", history)
	}
	if len(page.Messages) != len(texts) {
		t.Fatalf("date history page = %d messages, want %d", len(page.Messages), len(texts))
	}
	for i, want := range []string{texts[3], texts[2], texts[1], texts[0]} {
		message, ok := page.Messages[i].(*tg.Message)
		if !ok || message.Message != want || message.Date != int(dates[len(texts)-i-1].Unix()) {
			t.Fatalf("date history message %d = %T/%+v, want %q at %s", i, page.Messages[i], page.Messages[i], want, dates[len(texts)-i-1].Format(time.RFC3339))
		}
	}
	first, ok := page.Messages[0].(*tg.Message)
	if !ok {
		t.Fatalf("newest date history message = %T, want *tg.Message", page.Messages[0])
	}
	last, ok := page.Messages[len(page.Messages)-1].(*tg.Message)
	if !ok {
		t.Fatalf("oldest date history message = %T, want *tg.Message", page.Messages[len(page.Messages)-1])
	}
	if first.Date <= int(date.Unix()) || last.Date >= int(date.Unix()) {
		t.Fatalf("date history page does not straddle %s: %s", date.Format(time.RFC3339), fmt.Sprint(page.Messages))
	}
}
