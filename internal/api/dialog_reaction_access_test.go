package api_test

import (
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/api"
)

func TestDialogReactionsRequireCurrentGroupMembership(t *testing.T) {
	t.Parallel()
	f := newMessageReactionsListFixture(t)
	reactInMessageListChat(t, f, f.a, "👍")
	memberMessage := chatMessageCopy(t, f.s, f.b.ID, f.chat.ID, f.message.FanoutID)

	read := func(t *testing.T, viewerID int64, method string) []tg.MessageClass {
		t.Helper()
		var result bin.Encoder
		var err error
		switch method {
		case "getDialogs":
			result, err = api.GetDialogsForTest(f.s, viewerID)
		case "getPeerDialogs":
			result, err = api.GetPeerDialogsForTest(f.s, viewerID, &tg.MessagesGetPeerDialogsRequest{
				Peers: []tg.InputDialogPeerClass{&tg.InputDialogPeer{Peer: api.InputPeerChat(viewerID, f.chat.ID)}},
			})
		default:
			t.Fatalf("unknown method %q", method)
		}
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		return messagesFromDialogResponse(t, result)
	}
	find := func(t *testing.T, messages []tg.MessageClass, localID int64) *tg.Message {
		t.Helper()
		for _, raw := range messages {
			if message, ok := raw.(*tg.Message); ok && int64(message.ID) == localID {
				return message
			}
		}
		t.Fatalf("dialog omitted message %d", localID)
		return nil
	}

	for _, method := range []string{"getDialogs", "getPeerDialogs"} {
		message := find(t, read(t, f.b.ID, method), memberMessage.LocalID)
		if reactions, ok := message.GetReactions(); !ok || len(reactions.Results) != 1 {
			t.Fatalf("%s current member reactions = %#v, present=%t; want one reaction", method, reactions, ok)
		}
		if messages := read(t, f.outsider.ID, method); len(messages) != 0 {
			t.Fatalf("%s outsider messages = %#v, want none", method, messages)
		}
	}

	// Model a removal without an announcement: the old top message and its
	// owner-scoped reactions remain even though membership has ended.
	execReactionListSQL(t, f.dsn, `DELETE FROM chat_participants WHERE chat_id = $1 AND user_id = $2`, f.chat.ID, f.b.ID)
	reactInMessageListChat(t, f, f.a, "❤")
	for _, method := range []string{"getDialogs", "getPeerDialogs"} {
		t.Run(method, func(t *testing.T) {
			message := find(t, read(t, f.b.ID, method), memberMessage.LocalID)
			if reactions, ok := message.GetReactions(); ok {
				t.Errorf("removed member received reactions: %#v", reactions)
			}
			currentMessage := chatMessageCopy(t, f.s, f.a.ID, f.chat.ID, f.message.FanoutID)
			current := find(t, read(t, f.a.ID, method), currentMessage.LocalID)
			reactions, ok := current.GetReactions()
			if !ok || len(reactions.Results) != 1 {
				t.Fatalf("current member reactions = %#v, present=%t; want one heart", reactions, ok)
			}
			heart, ok := reactions.Results[0].Reaction.(*tg.ReactionEmoji)
			if !ok || heart.Emoticon != "❤" {
				t.Errorf("current member reaction = %#v, want heart", reactions.Results[0].Reaction)
			}
		})
	}
}
