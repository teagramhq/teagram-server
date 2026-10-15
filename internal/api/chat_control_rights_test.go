package api_test

import (
	"context"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/mt"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/store"
)

// These cases catch creator-only handler checks, missing store authorization,
// admin exemptions, and authorization after the no-op comparison.
func TestBasicChatControlRights(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"rename", "pin", "unpin", "unpinAll", "noopPin", "noopUnpin", "noopUnpinAll"} {
		for _, role := range []string{"member", "admin", "creator", "outsider"} {
			for _, restricted := range []bool{false, true} {
				name := operation + "/" + role + "/allowed"
				if restricted {
					name = operation + "/" + role + "/restricted"
				}
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					ctx := context.Background()
					s := openStore(t)
					creator, member, outsider := chatUser(t, s, 8901), chatUser(t, s, 8902), chatUser(t, s, 8903)
					chat, err := s.CreateChat(ctx, creator.ID, "Before", []int64{member.ID})
					if err != nil {
						t.Fatal(err)
					}
					h := fullChannelDispatcher(s)
					if role == "admin" {
						if result, rpc := editChatAdmin(t, h, creator.ID, member.ID, chat.ID, true); result == nil || rpc != nil {
							t.Fatalf("promote: %v", rpc)
						}
					}
					msg, _, _, err := s.SendChatMessage(ctx, store.FanOut{ChatID: chat.ID, FromID: creator.ID, Text: "chosen", RandomID: 89001})
					if err != nil {
						t.Fatal(err)
					}
					id := int32(msg.LocalID) //nolint:gosec // test message id
					if operation == "unpin" || operation == "unpinAll" || operation == "noopPin" {
						if _, _, err := s.SetChatPinnedMessage(ctx, chat.ID, creator.ID, &id); err != nil {
							t.Fatal(err)
						}
					}
					right := "pin_messages"
					if operation == "rename" {
						right = "change_info"
					}
					rights := []string{"send_messages"} // Service announcements must bypass send restrictions.
					if restricted {
						rights = append(rights, right)
					}
					if _, _, err := s.SetChatDefaultBannedRights(ctx, chat.ID, creator.ID, rights); err != nil {
						t.Fatal(err)
					}
					caller := member.ID
					if role == "creator" {
						caller = creator.ID
					}
					if role == "outsider" {
						caller = outsider.ID
					}
					before := map[int64]int{creator.ID: apiPts(t, s, creator.ID), member.ID: apiPts(t, s, member.ID)}
					var req bin.Encoder
					method := "messages.updatePinnedMessage"
					switch operation {
					case "rename":
						method = "messages.editChatTitle"
						req = &tg.MessagesEditChatTitleRequest{ChatID: chat.ID, Title: "After"}
					case "unpinAll", "noopUnpinAll":
						method = "messages.unpinAllMessages"
						req = &tg.MessagesUnpinAllMessagesRequest{Peer: &tg.InputPeerChat{ChatID: chat.ID}}
					default:
						req = &tg.MessagesUpdatePinnedMessageRequest{Peer: &tg.InputPeerChat{ChatID: chat.ID}, ID: int(id), Unpin: operation == "unpin" || operation == "noopUnpin"}
					}
					body := dispatchChatMethod(t, h, caller, method, req)
					var rpc mt.RPCError
					rpcErr := rpc.Decode(&bin.Buffer{Buf: body})
					want := ""
					if restricted && role != "creator" {
						want = "CHAT_ADMIN_REQUIRED"
					}
					if role == "outsider" {
						want = "PEER_ID_INVALID"
					}
					if want != "" {
						if rpcErr != nil || rpc.ErrorMessage != want {
							t.Fatalf("RPC = %v (%v), want %s", rpc, rpcErr, want)
						}
					} else if rpcErr == nil {
						t.Fatalf("unexpected RPC: %s", rpc.ErrorMessage)
					}
					changed := want == "" && operation != "noopPin" && operation != "noopUnpin" && operation != "noopUnpinAll"
					for owner, pts := range before {
						delta := 0
						if changed {
							delta = 1
						}
						if got := apiPts(t, s, owner); got != pts+delta {
							t.Fatalf("owner %d pts=%d want %d", owner, got, pts+delta)
						}
					}
					stored, _, err := s.ChatByID(ctx, chat.ID)
					if err != nil {
						t.Fatal(err)
					}
					if operation == "rename" {
						title := "Before"
						if changed {
							title = "After"
						}
						if stored.Title != title {
							t.Fatalf("title=%q want %q", stored.Title, title)
						}
						if changed {
							for owner, pts := range before {
								diff, err := api.GetDifferenceForTest(s, owner, &tg.UpdatesGetDifferenceRequest{Pts: pts})
								if err != nil {
									t.Fatal(err)
								}
								d, ok := diff.(*tg.UpdatesDifference)
								if !ok || len(d.NewMessages) != 1 {
									t.Fatalf("rename difference=%#v", diff)
								}
								service, ok := d.NewMessages[0].(*tg.MessageService)
								if !ok {
									t.Fatalf("rename message=%T", d.NewMessages[0])
								}
								action, ok := service.Action.(*tg.MessageActionChatEditTitle)
								if !ok || action.Title != "After" {
									t.Fatalf("rename action=%#v", service.Action)
								}
							}
						}
					} else {
						pinned := operation == "noopPin" || operation == "unpin" || operation == "unpinAll"
						if changed {
							pinned = operation == "pin"
						}
						if (stored.PinnedMessageID != nil) != pinned {
							t.Fatalf("pin=%v want pinned %t", stored.PinnedMessageID, pinned)
						}
					}
				})
			}
		}
	}
}

func TestMemberPinUsesChosenMessageCopies(t *testing.T) {
	t.Parallel()
	for _, deletedCreator := range []bool{false, true} {
		name := "live"
		if deletedCreator {
			name = "deletedCreator"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			s := openStore(t)
			a, b := chatUser(t, s, 8911), chatUser(t, s, 8912)
			if _, _, _, _, err := s.SendMessage(ctx, b.ID, b.ID, "id seed", 89101, 0, 0); err != nil {
				t.Fatal(err)
			}
			ch, err := s.CreateChat(ctx, a.ID, "Copies", []int64{b.ID})
			if err != nil {
				t.Fatal(err)
			}
			sender, _, _, err := s.SendChatMessage(ctx, store.FanOut{ChatID: ch.ID, FromID: b.ID, Text: "chosen", RandomID: 89102})
			if err != nil {
				t.Fatal(err)
			}
			history, err := s.History(ctx, a.ID, store.PeerTypeChat, ch.ID, 0, 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(history) != 1 {
				t.Fatalf("history=%v", history)
			}
			creatorID := history[0].LocalID
			if creatorID == sender.LocalID {
				t.Fatal("fixture must use different local ids")
			}
			// The creator's copy at Bob's chosen ID is a different message.
			if _, _, _, err := s.SendChatMessage(ctx, store.FanOut{ChatID: ch.ID, FromID: a.ID, Text: "decoy", RandomID: 89103}); err != nil {
				t.Fatal(err)
			}
			if deletedCreator {
				if _, err := s.DeleteMessages(ctx, a.ID, []int64{creatorID}, false); err != nil {
					t.Fatal(err)
				}
			}
			enc, err := api.UpdatePinnedMessageForTest(s, b.ID, &tg.MessagesUpdatePinnedMessageRequest{Peer: &tg.InputPeerChat{ChatID: ch.ID}, ID: int(sender.LocalID)})
			if deletedCreator {
				wantRPC(t, err, "MESSAGE_ID_INVALID")
				if id, err := s.ChatPinnedMessage(ctx, ch.ID); err != nil || id != nil {
					t.Fatalf("rejected pin changed state: %v/%v", id, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			ups, ok := enc.(*tg.Updates)
			if !ok || len(ups.Updates) != 1 {
				t.Fatalf("pin reply=%#v", enc)
			}
			pin, ok := ups.Updates[0].(*tg.UpdatePinnedMessages)
			if !ok || len(pin.Messages) != 1 || pin.Messages[0] != int(sender.LocalID) {
				t.Fatalf("caller pin=%#v", ups.Updates)
			}
			for owner, want := range map[int64]int64{a.ID: creatorID, b.ID: sender.LocalID} {
				got, found, err := s.ChatPinnedMessageForOwner(ctx, ch.ID, owner)
				if err != nil || !found || got != want {
					t.Fatalf("owner %d pin=%d/%t/%v want %d", owner, got, found, err, want)
				}
				diff, err := api.GetDifferenceForTest(s, owner, &tg.UpdatesGetDifferenceRequest{})
				if err != nil {
					t.Fatal(err)
				}
				d, ok := diff.(*tg.UpdatesDifference)
				if !ok {
					t.Fatalf("difference=%T", diff)
				}
				foundPin := false
				for _, u := range d.OtherUpdates {
					if p, ok := u.(*tg.UpdatePinnedMessages); ok {
						foundPin = true
						if len(p.Messages) != 1 || p.Messages[0] != int(want) {
							t.Fatalf("owner %d replay=%#v", owner, p)
						}
					}
				}
				if !foundPin {
					t.Fatalf("owner %d missing pin replay", owner)
				}
			}
			before := apiPts(t, s, b.ID)
			if _, err := api.UpdatePinnedMessageForTest(s, b.ID, &tg.MessagesUpdatePinnedMessageRequest{Peer: &tg.InputPeerChat{ChatID: ch.ID}, ID: int(sender.LocalID)}); err != nil {
				t.Fatal(err)
			}
			if got := apiPts(t, s, b.ID); got != before {
				t.Fatalf("translated repeat pin spent pts: %d want %d", got, before)
			}
			enc, err = api.UpdatePinnedMessageForTest(s, b.ID, &tg.MessagesUpdatePinnedMessageRequest{Peer: &tg.InputPeerChat{ChatID: ch.ID}, Unpin: true})
			if err != nil {
				t.Fatal(err)
			}
			ups, ok = enc.(*tg.Updates)
			if !ok || len(ups.Updates) != 1 {
				t.Fatalf("unpin reply=%#v", enc)
			}
			pin, ok = ups.Updates[0].(*tg.UpdatePinnedMessages)
			if !ok {
				t.Fatalf("unpin update=%T", ups.Updates[0])
			}
			if len(pin.Messages) != 1 || pin.Messages[0] != int(sender.LocalID) {
				t.Fatalf("caller unpin=%#v", pin)
			}
		})
	}
}
