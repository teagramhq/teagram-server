package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
)

func TestSeedBroadcastChannelIsIdempotentWithNewAndLegacyHistory(t *testing.T) {
	for _, spec := range publicSpaces {
		if spec.slug == "welcome" || spec.megagroup {
			continue
		}
		for _, legacy := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/legacy=%t", spec.slug, legacy), func(t *testing.T) {
				seeded := seededMessagesForSpace(spec)
				rpc := &statefulSeedRPC{}
				if legacy {
					for index, message := range seeded {
						id := index + 1
						rpc.messages = append(rpc.messages, &tg.Message{
							ID:      id,
							Message: message.marker + " " + message.text,
						})
						rpc.nextID = id
					}
				}

				api := tg.NewClient(rpc)
				remote := remoteSpace{spec: spec, channelID: 71, accessHash: 93}
				for run := 1; run <= 2; run++ {
					sendsBefore := rpc.sentMessages
					if err := seedBroadcastChannel(context.Background(), api, remote); err != nil {
						t.Fatalf("seed run %d: %v", run, err)
					}

					if got, want := len(rpc.messages), expectedSeedMarkerCount(spec); got != want {
						t.Fatalf("run %d: message count = %d, want %d", run, got, want)
					}
					wantNewSends := 0
					if !legacy && run == 1 {
						wantNewSends = len(seeded)
					}
					if got := rpc.sentMessages - sendsBefore; got != wantNewSends {
						t.Fatalf("run %d: new sends = %d, want %d", run, got, wantNewSends)
					}

					markers, err := historyMarkers(context.Background(), api, remote.peer(), seeded...)
					if err != nil {
						t.Fatalf("run %d: read seeded messages: %v", run, err)
					}
					if got, want := countSeedMarkers(markers, spec), expectedSeedMarkerCount(spec); got != want {
						t.Fatalf("run %d: verified content count = %d, want %d", run, got, want)
					}

					root := channelPostSeedMessage(spec, 0)
					if err := messageIsPinned(context.Background(), api, remote.peer(), root.marker, root); err != nil {
						t.Fatalf("run %d: verify pinned introduction: %v", run, err)
					}
					if id, ok := markers[root.marker]; !ok || rpc.pinnedID != int(id) {
						t.Fatalf("run %d: pinned message ID = %d, expected root %d (found=%t)", run, rpc.pinnedID, id, ok)
					}
				}

				if !legacy {
					for _, candidate := range rpc.messages {
						message, ok := candidate.(*tg.Message)
						if !ok {
							t.Fatalf("unexpected seeded message %T", candidate)
						}
						if strings.Contains(message.Message, "[[teagram-demo:") {
							t.Errorf("seeded message contains an internal marker: %q", message.Message)
						}
					}
				}
			})
		}
	}
}

func TestSeedGroupConversationIsIdempotentWithNewAndLegacyHistory(t *testing.T) {
	for _, spec := range publicSpaces {
		if !spec.megagroup {
			continue
		}
		for _, legacy := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/legacy=%t", spec.slug, legacy), func(t *testing.T) {
				seeded := seededMessagesForSpace(spec)
				rpc := &statefulSeedRPC{}
				if legacy {
					for index, message := range seeded {
						id := index + 1
						rpc.messages = append(rpc.messages, &tg.Message{
							ID:      id,
							Message: message.marker + " " + message.text,
						})
						rpc.nextID = id
					}
				}

				api := tg.NewClient(rpc)
				remote := remoteSpace{spec: spec, channelID: 71, accessHash: 93}
				groups := []remoteSpace{remote}
				for run := 1; run <= 2; run++ {
					current := make(map[string]int64)
					if legacy || run == 2 {
						var err error
						current, err = historyMarkers(context.Background(), api, remote.peer(), seeded...)
						if err != nil {
							t.Fatalf("run %d: read seeded messages: %v", run, err)
						}
					}

					sendsBefore := rpc.sentMessages
					for turn := range conversationLines {
						if err := seedGroupConversationTurn(context.Background(), api, groups, current, turn); err != nil {
							t.Fatalf("run %d turn %d: %v", run, turn, err)
						}
					}
					if got, want := len(rpc.messages), expectedSeedMarkerCount(spec); got != want {
						t.Fatalf("run %d: message count = %d, want %d", run, got, want)
					}
					wantNewSends := 0
					if !legacy && run == 1 {
						wantNewSends = len(seeded)
					}
					if got := rpc.sentMessages - sendsBefore; got != wantNewSends {
						t.Fatalf("run %d: new sends = %d, want %d", run, got, wantNewSends)
					}
					markers, err := historyMarkers(context.Background(), api, remote.peer(), seeded...)
					if err != nil {
						t.Fatalf("run %d: verify seeded history: %v", run, err)
					}
					if got, want := countSeedMarkers(markers, spec), expectedSeedMarkerCount(spec); got != want {
						t.Fatalf("run %d: verified content count = %d, want %d", run, got, want)
					}

					if err := pinGroupConversations(context.Background(), api, groups); err != nil {
						t.Fatalf("run %d: pin turn 1: %v", run, err)
					}
					turnOne := conversationSeedMessage(spec, 1)
					if err := messageIsPinned(context.Background(), api, remote.peer(), turnOne.marker, turnOne); err != nil {
						t.Fatalf("run %d: verify pinned turn 1: %v", run, err)
					}
				}

				if !legacy {
					if got, want := len(rpc.replyParents), len(conversationLines); got != want {
						t.Fatalf("reply parent records = %d, want %d", got, want)
					}
					if rpc.replyParents[0] != 0 {
						t.Fatalf("turn 0 reply parent = %d, want no parent", rpc.replyParents[0])
					}
					for turn := 1; turn < len(conversationLines); turn++ {
						if got, want := rpc.replyParents[turn], rpc.sentIDs[turn-1]; got != want {
							t.Errorf("turn %d reply parent = %d, want previous message %d", turn, got, want)
						}
					}
				}
			})
		}
	}
}

type statefulSeedRPC struct {
	messages     []tg.MessageClass
	nextID       int
	pinnedID     int
	sentMessages int
	replyParents []int
	sentIDs      []int
}

func (rpc *statefulSeedRPC) Invoke(_ context.Context, input bin.Encoder, output bin.Decoder) error {
	switch request := input.(type) {
	case *tg.MessagesGetHistoryRequest:
		response, ok := output.(*tg.MessagesMessagesBox)
		if !ok {
			return fmt.Errorf("unexpected history output %T", output)
		}
		response.Messages = &tg.MessagesMessages{Messages: rpc.messages}
	case *tg.MessagesSendMessageRequest:
		rpc.nextID++
		rpc.sentMessages++
		parentID := 0
		if reply, ok := request.GetReplyTo(); ok {
			messageReply, ok := reply.(*tg.InputReplyToMessage)
			if !ok {
				return fmt.Errorf("unexpected reply type %T", reply)
			}
			parentID = messageReply.ReplyToMsgID
		}
		rpc.replyParents = append(rpc.replyParents, parentID)
		rpc.sentIDs = append(rpc.sentIDs, rpc.nextID)
		rpc.messages = append(rpc.messages, &tg.Message{ID: rpc.nextID, Message: request.Message})
		response, ok := output.(*tg.UpdatesBox)
		if !ok {
			return fmt.Errorf("unexpected send output %T", output)
		}
		response.Updates = &tg.Updates{Updates: []tg.UpdateClass{&tg.UpdateMessageID{ID: rpc.nextID, RandomID: request.RandomID}}}
	case *tg.MessagesSendMediaRequest:
		rpc.nextID++
		rpc.sentMessages++
		rpc.messages = append(rpc.messages, &tg.Message{ID: rpc.nextID, Message: request.Message})
		response, ok := output.(*tg.UpdatesBox)
		if !ok {
			return fmt.Errorf("unexpected media output %T", output)
		}
		response.Updates = &tg.Updates{}
	case *tg.UploadSaveFilePartRequest:
		response, ok := output.(*tg.BoolBox)
		if !ok {
			return fmt.Errorf("unexpected upload output %T", output)
		}
		response.Bool = &tg.BoolTrue{}
	case *tg.MessagesUpdatePinnedMessageRequest:
		if !rpc.hasMessage(request.ID) {
			return fmt.Errorf("cannot pin missing message %d", request.ID)
		}
		rpc.pinnedID = request.ID
		response, ok := output.(*tg.UpdatesBox)
		if !ok {
			return fmt.Errorf("unexpected pin output %T", output)
		}
		response.Updates = &tg.Updates{}
	case *tg.ChannelsGetFullChannelRequest:
		channel := &tg.ChannelFull{}
		channel.SetPinnedMsgID(rpc.pinnedID)
		response, ok := output.(*tg.MessagesChatFull)
		if !ok {
			return fmt.Errorf("unexpected full-channel output %T", output)
		}
		response.FullChat = channel
	default:
		return fmt.Errorf("unexpected seed request %T", input)
	}
	return nil
}

func (rpc *statefulSeedRPC) hasMessage(id int) bool {
	for _, candidate := range rpc.messages {
		message, ok := candidate.(*tg.Message)
		if ok && message.ID == id {
			return true
		}
	}
	return false
}
