package api_test

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/api"
)

func TestSendMessageDetectsURLsAndPreservesEntitiesAcrossReadPaths(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	a, err := s.CreateUser(ctx, "+15551293001")
	if err != nil {
		t.Fatalf("user a: %v", err)
	}
	b, err := s.CreateUser(ctx, "+15551293002")
	if err != nil {
		t.Fatalf("user b: %v", err)
	}

	text := "😀 bold https://teagram.tailaa4918.ts.net/+abc and t.me/+def then tg://join?invite=ghi wrapped t.me/+skip link"
	wrappedStart := utf16LengthBeforeForTest(t, text, "t.me/+skip")
	linkStart := utf16LengthBeforeForTest(t, text, "link")
	urlStart := utf16LengthBeforeForTest(t, text, "https://")
	urlLength := utf16LengthForTest("https://teagram.tailaa4918.ts.net/+abc")
	clientEntities := []tg.MessageEntityClass{
		&tg.MessageEntityBold{Offset: 3, Length: 4},
		&tg.MessageEntityBold{Offset: urlStart, Length: urlLength},
		&tg.MessageEntityTextURL{Offset: linkStart, Length: 4, URL: "https://example.org/target"},
		&tg.MessageEntityCode{Offset: wrappedStart, Length: utf16LengthForTest("t.me/+skip")},
	}
	const randomID = 423001
	result, err := api.SendMessageForTest(s, a.ID, &tg.MessagesSendMessageRequest{
		Peer: api.InputPeerUser(a.ID, b.ID), Message: text, Entities: clientEntities, RandomID: randomID,
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	want := []string{
		entitySpan("bold", 3, 4),
		entitySpan("bold", urlStart, urlLength),
		entitySpan("text_url", linkStart, 4),
		entitySpan("code", wrappedStart, utf16LengthForTest("t.me/+skip")),
		entitySpan("url", utf16LengthBeforeForTest(t, text, "t.me/+def"), utf16LengthForTest("t.me/+def")),
		entitySpan("url", utf16LengthBeforeForTest(t, text, "tg://"), utf16LengthForTest("tg://join?invite=ghi")),
	}
	sort.Strings(want)
	assertEntities := func(label string, raw tg.MessageClass) {
		t.Helper()
		message, ok := raw.(*tg.Message)
		if !ok {
			t.Fatalf("%s message = %T, want *tg.Message", label, raw)
		}
		entities, present := message.GetEntities()
		if !present || len(entities) != len(want) {
			t.Fatalf("%s entities = %#v present=%v, want %d entities", label, entities, present, len(want))
		}
		got := make([]string, 0, len(entities))
		for _, entity := range entities {
			switch entity := entity.(type) {
			case *tg.MessageEntityBold:
				got = append(got, entitySpan("bold", entity.Offset, entity.Length))
			case *tg.MessageEntityTextURL:
				if entity.URL != "https://example.org/target" {
					t.Errorf("%s text_url target = %q", label, entity.URL)
				}
				got = append(got, entitySpan("text_url", entity.Offset, entity.Length))
			case *tg.MessageEntityCode:
				got = append(got, entitySpan("code", entity.Offset, entity.Length))
			case *tg.MessageEntityURL:
				got = append(got, entitySpan("url", entity.Offset, entity.Length))
			default:
				t.Errorf("%s unexpected entity type %T", label, entity)
			}
		}
		sort.Strings(got)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s entities = %v, want %v", label, got, want)
		}
	}

	updates, ok := result.(*tg.Updates)
	if !ok {
		t.Fatalf("send result = %T, want *tg.Updates", result)
	}
	var sent tg.MessageClass
	for _, update := range updates.Updates {
		if newMessage, ok := update.(*tg.UpdateNewMessage); ok {
			sent = newMessage.Message
		}
	}
	assertEntities("send response", sent)

	history, err := api.GetHistoryForTest(s, a.ID, &tg.MessagesGetHistoryRequest{
		Peer: api.InputPeerUser(a.ID, b.ID), Limit: 10,
	})
	if err != nil {
		t.Fatalf("sender history: %v", err)
	}
	assertEntities("sender history", firstHistoryMessage(t, history))

	difference, err := api.GetDifferenceForTest(s, b.ID, &tg.UpdatesGetDifferenceRequest{Pts: 0})
	if err != nil {
		t.Fatalf("recipient difference: %v", err)
	}
	diff, ok := difference.(*tg.UpdatesDifference)
	if !ok {
		t.Fatalf("recipient difference = %T, want *tg.UpdatesDifference", difference)
	}
	var received tg.MessageClass
	if len(diff.NewMessages) > 0 {
		received = diff.NewMessages[0]
	}
	assertEntities("recipient update", received)

	recipientHistory, err := api.GetHistoryForTest(s, b.ID, &tg.MessagesGetHistoryRequest{
		Peer: api.InputPeerUser(b.ID, a.ID), Limit: 10,
	})
	if err != nil {
		t.Fatalf("recipient history: %v", err)
	}
	assertEntities("recipient history", firstHistoryMessage(t, recipientHistory))

	original, ok := sent.(*tg.Message)
	if !ok {
		t.Fatalf("sent message = %T, want *tg.Message", sent)
	}
	editedText := "edited 😀 https://t.me/+updated"
	editResult, err := api.EditMessageForTest(s, a.ID, &tg.MessagesEditMessageRequest{
		Peer: api.InputPeerUser(a.ID, b.ID), ID: original.ID, Message: editedText,
	})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	edits, ok := editResult.(*tg.Updates)
	if !ok {
		t.Fatalf("edit result = %T, want *tg.Updates", editResult)
	}
	var edited tg.MessageClass
	for _, update := range edits.Updates {
		if edit, ok := update.(*tg.UpdateEditMessage); ok {
			edited = edit.Message
		}
	}
	assertSingleURL(t, "edit response", edited, editedText, "https://t.me/+updated")

	for _, tc := range []struct{ ownerID, peerID int64 }{{a.ID, b.ID}, {b.ID, a.ID}} {
		history, historyErr := api.GetHistoryForTest(s, tc.ownerID, &tg.MessagesGetHistoryRequest{
			Peer: api.InputPeerUser(tc.ownerID, tc.peerID), Limit: 10,
		})
		if historyErr != nil {
			t.Fatalf("owner %d history after edit: %v", tc.ownerID, historyErr)
		}
		assertSingleURL(t, "edited history", firstHistoryMessage(t, history), editedText, "https://t.me/+updated")
	}

	editDifference, err := api.GetDifferenceForTest(s, b.ID, &tg.UpdatesGetDifferenceRequest{Pts: 1})
	if err != nil {
		t.Fatalf("recipient difference after edit: %v", err)
	}
	editDiff, ok := editDifference.(*tg.UpdatesDifference)
	if !ok {
		t.Fatalf("recipient edit difference = %T, want *tg.UpdatesDifference", editDifference)
	}
	var differenceEdit tg.MessageClass
	for _, update := range editDiff.OtherUpdates {
		if edit, ok := update.(*tg.UpdateEditMessage); ok {
			differenceEdit = edit.Message
			break
		}
	}
	assertSingleURL(t, "recipient edit difference", differenceEdit, editedText, "https://t.me/+updated")
}

func TestSendMessageWithoutURLsAddsNoEntities(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	a, err := s.CreateUser(ctx, "+15551293003")
	if err != nil {
		t.Fatalf("user a: %v", err)
	}
	b, err := s.CreateUser(ctx, "+15551293004")
	if err != nil {
		t.Fatalf("user b: %v", err)
	}
	result, err := api.SendMessageForTest(s, a.ID, &tg.MessagesSendMessageRequest{
		Peer: api.InputPeerUser(a.ID, b.ID), Message: "plain text 😀", RandomID: 423002,
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	updates, ok := result.(*tg.Updates)
	if !ok {
		t.Fatalf("send result = %T, want *tg.Updates", result)
	}
	message := firstNewMessage(t, updates.Updates)
	if entities, present := message.GetEntities(); present && len(entities) != 0 {
		t.Fatalf("plain message entities = %#v, want none", entities)
	}
}

func TestSendMediaCaptionDetectsURLs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	a, err := s.CreateUser(ctx, "+15551293005")
	if err != nil {
		t.Fatalf("user a: %v", err)
	}
	b, err := s.CreateUser(ctx, "+15551293006")
	if err != nil {
		t.Fatalf("user b: %v", err)
	}
	const fileID = 423003
	saveParts(t, s, a.ID, fileID, []byte("document payload"))
	caption := "caption 😀 t.me/+caption"
	result, err := api.SendMediaForTest(s, a.ID, testBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerUser(a.ID, b.ID),
		Media: &tg.InputMediaUploadedDocument{
			File: &tg.InputFile{ID: fileID, Parts: 1, Name: "caption.txt"}, MimeType: "text/plain",
		},
		Message: caption, RandomID: 423004,
	})
	if err != nil {
		t.Fatalf("send media: %v", err)
	}
	updates, ok := result.(*tg.Updates)
	if !ok {
		t.Fatalf("send media result = %T, want *tg.Updates", result)
	}
	assertSingleURL(t, "media caption", firstNewMessage(t, updates.Updates), caption, "t.me/+caption")
}

func TestChannelHistoryReturnsDetectedURL(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	a, err := s.CreateUser(ctx, "+15551293007")
	if err != nil {
		t.Fatalf("user a: %v", err)
	}
	channel := createChannel(t, s, a.ID, &tg.ChannelsCreateChannelRequest{Broadcast: true, Title: "Link channel"})
	text := "channel post 😀 https://t.me/+channel"
	result, err := api.SendMessageForTest(s, a.ID, &tg.MessagesSendMessageRequest{
		Peer: api.InputPeerChannel(a.ID, channel.ID), Message: text, RandomID: 423005,
	})
	if err != nil {
		t.Fatalf("send channel message: %v", err)
	}
	updates, ok := result.(*tg.Updates)
	if !ok {
		t.Fatalf("send channel result = %T, want *tg.Updates", result)
	}
	var sent tg.MessageClass
	for _, update := range updates.Updates {
		if newMessage, ok := update.(*tg.UpdateNewChannelMessage); ok {
			sent = newMessage.Message
		}
	}
	assertSingleURL(t, "channel send response", sent, text, "https://t.me/+channel")

	history, err := api.GetHistoryForTest(s, a.ID, &tg.MessagesGetHistoryRequest{
		Peer: api.InputPeerChannel(a.ID, channel.ID), Limit: 10,
	})
	if err != nil {
		t.Fatalf("channel history: %v", err)
	}
	assertSingleURL(t, "channel history", firstHistoryMessage(t, history), text, "https://t.me/+channel")
}

func firstHistoryMessage(t *testing.T, result any) tg.MessageClass {
	t.Helper()
	var messages []tg.MessageClass
	switch history := result.(type) {
	case *tg.MessagesMessages:
		messages = history.Messages
	case *tg.MessagesChannelMessages:
		messages = history.Messages
	default:
		t.Fatalf("history result = %T, want a messages history", result)
	}
	if len(messages) == 0 {
		t.Fatal("history contains no messages")
	}
	return messages[0]
}

func firstNewMessage(t *testing.T, updates []tg.UpdateClass) *tg.Message {
	t.Helper()
	for _, update := range updates {
		if newMessage, ok := update.(*tg.UpdateNewMessage); ok {
			message, ok := newMessage.Message.(*tg.Message)
			if !ok {
				t.Fatalf("new message = %T, want *tg.Message", newMessage.Message)
			}
			return message
		}
	}
	t.Fatal("updates contain no UpdateNewMessage")
	return nil
}

func assertSingleURL(t *testing.T, label string, raw tg.MessageClass, text, link string) {
	t.Helper()
	message, ok := raw.(*tg.Message)
	if !ok {
		t.Fatalf("%s message = %T, want *tg.Message", label, raw)
	}
	if message.Message != text {
		t.Fatalf("%s text = %q, want %q", label, message.Message, text)
	}
	entities, present := message.GetEntities()
	if !present || len(entities) != 1 {
		t.Fatalf("%s entities = %#v present=%v, want one URL", label, entities, present)
	}
	urlEntity, ok := entities[0].(*tg.MessageEntityURL)
	if !ok {
		t.Fatalf("%s entity = %T, want MessageEntityUrl", label, entities[0])
	}
	wantOffset := utf16LengthBeforeForTest(t, text, link)
	wantLength := utf16LengthForTest(link)
	if urlEntity.Offset != wantOffset || urlEntity.Length != wantLength {
		t.Errorf("%s URL span = (%d,%d), want (%d,%d)", label, urlEntity.Offset, urlEntity.Length, wantOffset, wantLength)
	}
}

func utf16LengthForTest(text string) int {
	return len(utf16.Encode([]rune(text)))
}

func utf16LengthBeforeForTest(t *testing.T, text, marker string) int {
	t.Helper()
	prefix, _, ok := strings.Cut(text, marker)
	if !ok {
		t.Fatalf("text %q does not contain %q", text, marker)
	}
	return utf16LengthForTest(prefix)
}

func entitySpan(kind string, offset, length int) string {
	return kind + ":" + strconv.Itoa(offset) + ":" + strconv.Itoa(length)
}
