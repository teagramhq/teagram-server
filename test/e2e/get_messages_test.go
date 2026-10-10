package e2e_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/store"
)

func TestGetMessages(t *testing.T) {
	t.Parallel()
	f := newSmokeFixture(t)
	const phoneA, phoneB = "+15551372001", "+15551372002"
	seedPhoneUsers(t, f.ctx, f.store, phoneA, phoneB)
	a := newSmokeClient(t, f, "getMessages sender", phoneA)
	b := newSmokeClient(t, f, "getMessages peer", phoneB)

	// Give B local id 12 while A has no message with that id. A's lookup must
	// stay in A's own numeric ID space.
	if err := b.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		for i := 1; i <= 12; i++ {
			if _, err := client.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
				Peer: &tg.InputPeerSelf{}, Message: fmt.Sprintf("foreign local id %d", i), RandomID: int64(1372000 + i),
			}); err != nil {
				return fmt.Errorf("seed peer message %d: %w", i, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed peer ID space: %v", err)
	}
	if _, ok, err := f.store.MessageByOwnerLocal(f.ctx, b.id, 12); err != nil || !ok {
		t.Fatalf("peer local id 12: ok=%v err=%v, want existing row", ok, err)
	}
	if _, ok, err := f.store.MessageByOwnerLocal(f.ctx, a.id, 12); err != nil || ok {
		t.Fatalf("caller local id 12: ok=%v err=%v, want no row", ok, err)
	}

	const clientFileID = 1372201
	var targetID, sourceID int
	if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		ok, err := client.UploadSaveFilePart(ctx, &tg.UploadSaveFilePartRequest{
			FileID: clientFileID, FilePart: 0, Bytes: []byte("quoted document body"),
		})
		if err != nil {
			return fmt.Errorf("upload quoted document: %w", err)
		}
		if !ok {
			return errors.New("upload quoted document returned false")
		}
		mediaResult, err := client.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
			Peer: peerUser(a.id, b.id),
			Media: &tg.InputMediaUploadedDocument{
				File: &tg.InputFile{ID: clientFileID, Parts: 1, Name: "quoted.txt"}, MimeType: "text/plain",
			},
			Message: "quoted media target", RandomID: 1372202,
		})
		if err != nil {
			return fmt.Errorf("send quoted document: %w", err)
		}
		target, err := outgoingGetMessagesMessage(mediaResult, "quoted media target")
		if err != nil {
			return err
		}
		targetID = target.ID

		reply := &tg.MessagesSendMessageRequest{Peer: peerUser(a.id, b.id), Message: "reply source", RandomID: 1372203}
		reply.SetReplyTo(&tg.InputReplyToMessage{ReplyToMsgID: targetID})
		replyResult, err := client.MessagesSendMessage(ctx, reply)
		if err != nil {
			return fmt.Errorf("send reply source: %w", err)
		}
		source, err := outgoingGetMessagesMessage(replyResult, "reply source")
		if err != nil {
			return err
		}
		sourceID = source.ID
		return nil
	}); err != nil {
		t.Fatalf("seed quote and media messages: %v", err)
	}

	var collision *tg.MessagesMessages
	if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		result, err := client.MessagesGetMessages(ctx, []tg.InputMessageClass{&tg.InputMessageID{ID: 12}})
		if err != nil {
			return err
		}
		var ok bool
		collision, ok = result.(*tg.MessagesMessages)
		if !ok {
			return fmt.Errorf("collision lookup result = %T, want *tg.MessagesMessages", result)
		}
		return nil
	}); err != nil {
		t.Fatalf("look up another owner's id: %v", err)
	}
	if len(collision.Messages) != 1 || len(collision.Users) != 0 || len(collision.Chats) != 0 {
		t.Fatalf("owner collision hydration = messages:%d users:%d chats:%d, want one empty and no entities", len(collision.Messages), len(collision.Users), len(collision.Chats))
	}
	if empty, ok := collision.Messages[0].(*tg.MessageEmpty); !ok || empty.ID != 12 {
		t.Fatalf("owner collision result = %#v, want messageEmpty{12}", collision.Messages[0])
	}

	var stateBefore *tg.UpdatesState
	if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		var err error
		stateBefore, err = client.UpdatesGetState(ctx)
		return err
	}); err != nil {
		t.Fatalf("read update state before getMessages: %v", err)
	}

	var response *tg.MessagesMessages
	if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		result, err := client.MessagesGetMessages(ctx, []tg.InputMessageClass{
			&tg.InputMessageReplyTo{ID: sourceID},
			&tg.InputMessageID{ID: sourceID},
			&tg.InputMessageID{ID: targetID},
			&tg.InputMessageReplyTo{ID: sourceID},
			&tg.InputMessageID{ID: 999999},
			&tg.InputMessageID{ID: -7},
		})
		if err != nil {
			return err
		}
		var ok bool
		response, ok = result.(*tg.MessagesMessages)
		if !ok {
			return fmt.Errorf("getMessages result = %T, want *tg.MessagesMessages", result)
		}
		return nil
	}); err != nil {
		t.Fatalf("get quote and media by id: %v", err)
	}
	if len(response.Messages) != 5 {
		t.Fatalf("getMessages count = %d, want 5", len(response.Messages))
	}
	if len(response.Users) != 2 || len(response.Chats) != 0 {
		t.Fatalf("authorized getMessages hydration = users:%d chats:%d, want two users and no chats", len(response.Users), len(response.Chats))
	}
	assertGetMessagesText(t, response.Messages[0], targetID, "quoted media target")
	assertGetMessagesDocument(t, response.Messages[0])
	assertGetMessagesText(t, response.Messages[1], sourceID, "reply source")
	assertGetMessagesText(t, response.Messages[2], targetID, "quoted media target")
	assertGetMessagesDocument(t, response.Messages[2])
	for index, id := range []int{999999, -7} {
		message, ok := response.Messages[index+3].(*tg.MessageEmpty)
		if !ok || message.ID != id {
			t.Errorf("getMessages entry %d = %#v, want messageEmpty{%d}", index+3, response.Messages[index+3], id)
		}
	}

	if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		stateAfter, err := client.UpdatesGetState(ctx)
		if err != nil {
			return err
		}
		if stateAfter.Pts != stateBefore.Pts || stateAfter.Seq != stateBefore.Seq || stateAfter.Qts != stateBefore.Qts || stateAfter.UnreadCount != stateBefore.UnreadCount {
			return fmt.Errorf("getMessages changed update state: before=%+v after=%+v", stateBefore, stateAfter)
		}
		return nil
	}); err != nil {
		t.Fatalf("verify read-only update state: %v", err)
	}

	if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		result, err := client.MessagesGetMessages(ctx, nil)
		if err != nil {
			return err
		}
		response, ok := result.(*tg.MessagesMessages)
		if !ok || len(response.Messages) != 0 {
			return fmt.Errorf("empty getMessages result = %T, want empty *tg.MessagesMessages", result)
		}
		return nil
	}); err != nil {
		t.Fatalf("empty getMessages: %v", err)
	}

	if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		_, err := client.MessagesGetMessages(ctx, []tg.InputMessageClass{
			&tg.InputMessageID{ID: targetID}, &tg.InputMessagePinned{},
		})
		if !tgerr.Is(err, "INPUT_METHOD_INVALID") {
			return fmt.Errorf("unsupported variant error = %w, want INPUT_METHOD_INVALID", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("reject pinned input message: %v", err)
	}
	tooMany := make([]tg.InputMessageClass, 101)
	for i := range tooMany {
		tooMany[i] = &tg.InputMessageID{ID: targetID}
	}
	if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		_, err := client.MessagesGetMessages(ctx, tooMany)
		if !tgerr.Is(err, "LIMIT_INVALID") {
			return fmt.Errorf("101 raw inputs error = %w, want LIMIT_INVALID", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("reject oversized raw input: %v", err)
	}

	group, err := f.store.CreateChat(f.ctx, a.id, "getMessages removal", []int64{b.id})
	if err != nil {
		t.Fatalf("create basic group: %v", err)
	}
	groupMessage, _, _, err := f.store.SendChatMessage(f.ctx, store.FanOut{ChatID: group.ID, FromID: a.id, Text: "retained group copy", RandomID: 1372301})
	if err != nil {
		t.Fatalf("send basic group message: %v", err)
	}
	conn, err := pgx.Connect(f.ctx, f.dsn)
	if err != nil {
		t.Fatalf("connect for group copy: %v", err)
	}
	defer func() {
		if err := conn.Close(f.ctx); err != nil {
			t.Errorf("close database connection: %v", err)
		}
	}()
	var memberLocalID int
	if err := conn.QueryRow(f.ctx, `SELECT local_id FROM messages WHERE owner_id = $1 AND fanout_id = $2`, b.id, groupMessage.FanoutID).Scan(&memberLocalID); err != nil {
		t.Fatalf("select retained group copy: %v", err)
	}
	if _, _, _, err := f.store.RemoveChatUser(f.ctx, group.ID, b.id, a.id); err != nil {
		t.Fatalf("commit member removal: %v", err)
	}
	var removedResult *tg.MessagesMessages
	if err := b.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		result, err := client.MessagesGetMessages(ctx, []tg.InputMessageClass{&tg.InputMessageID{ID: memberLocalID}})
		if err != nil {
			return err
		}
		var ok bool
		removedResult, ok = result.(*tg.MessagesMessages)
		if !ok {
			return fmt.Errorf("removed group getMessages result = %T, want *tg.MessagesMessages", result)
		}
		return nil
	}); err != nil {
		t.Fatalf("getMessages after committed removal: %v", err)
	}
	if len(removedResult.Messages) != 1 || len(removedResult.Users) != 0 || len(removedResult.Chats) != 0 {
		t.Fatalf("removed member hydration = messages:%d users:%d chats:%d, want one empty and no entities", len(removedResult.Messages), len(removedResult.Users), len(removedResult.Chats))
	}
	if empty, ok := removedResult.Messages[0].(*tg.MessageEmpty); !ok || empty.ID != memberLocalID {
		t.Fatalf("removed member result = %#v, want messageEmpty{%d}", removedResult.Messages[0], memberLocalID)
	}
}

func outgoingGetMessagesMessage(result tg.UpdatesClass, text string) (*tg.Message, error) {
	updates, ok := result.(*tg.Updates)
	if !ok {
		return nil, fmt.Errorf("send result = %T, want *tg.Updates", result)
	}
	for _, update := range updates.Updates {
		newMessage, ok := update.(*tg.UpdateNewMessage)
		if !ok {
			continue
		}
		message, ok := newMessage.Message.(*tg.Message)
		if ok && message.Out && message.Message == text {
			return message, nil
		}
	}
	return nil, fmt.Errorf("send result omitted outgoing %q", text)
}

func assertGetMessagesText(t *testing.T, class tg.MessageClass, id int, text string) *tg.Message {
	t.Helper()
	message, ok := class.(*tg.Message)
	if !ok || message.ID != id || message.Message != text {
		t.Fatalf("message = %#v, want id=%d text=%q", class, id, text)
	}
	return message
}

func assertGetMessagesDocument(t *testing.T, class tg.MessageClass) {
	t.Helper()
	message, ok := class.(*tg.Message)
	if !ok {
		t.Fatalf("message = %T, want *tg.Message", class)
	}
	media, ok := message.Media.(*tg.MessageMediaDocument)
	if !ok {
		t.Fatalf("message media = %T, want *tg.MessageMediaDocument", message.Media)
	}
	document, ok := media.Document.(*tg.Document)
	if !ok || document.ID <= 0 || document.AccessHash == 0 || document.DCID != 2 || len(document.FileReference) != 8 {
		t.Fatalf("message document = %#v, want stored document with download metadata", media.Document)
	}
}
