package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
)

type welcomeRPC struct {
	messages []tg.MessageClass
	nextID   int
	pinnedID int
	deleted  []int
}

func (rpc *welcomeRPC) Invoke(_ context.Context, input bin.Encoder, output bin.Decoder) error {
	switch request := input.(type) {
	case *tg.MessagesGetHistoryRequest:
		box, ok := output.(*tg.MessagesMessagesBox)
		if !ok {
			return fmt.Errorf("unexpected output %T", output)
		}
		box.Messages = &tg.MessagesMessages{Messages: rpc.messages}
	case *tg.MessagesSendMessageRequest:
		if request.RandomID == randomID(welcomeMarker()) {
			return errors.New("reused legacy random ID")
		}
		rpc.nextID++
		rpc.messages = append(rpc.messages, &tg.Message{ID: rpc.nextID, Message: request.Message, Entities: request.Entities})
		box, ok := output.(*tg.UpdatesBox)
		if !ok {
			return fmt.Errorf("unexpected output %T", output)
		}
		box.Updates = &tg.Updates{Updates: []tg.UpdateClass{&tg.UpdateMessageID{ID: rpc.nextID, RandomID: request.RandomID}}}
	case *tg.ChannelsDeleteMessagesRequest:
		rpc.deleted = append(rpc.deleted, request.ID...)
		var kept []tg.MessageClass
		for _, candidate := range rpc.messages {
			message, ok := candidate.(*tg.Message)
			if !ok {
				return fmt.Errorf("unexpected message %T", candidate)
			}
			if message.ID != request.ID[0] {
				kept = append(kept, message)
			}
		}
		rpc.messages = kept
		affected, ok := output.(*tg.MessagesAffectedMessages)
		if !ok {
			return fmt.Errorf("unexpected output %T", output)
		}
		*affected = tg.MessagesAffectedMessages{PtsCount: len(request.ID)}
	case *tg.ChannelsGetFullChannelRequest:
		channel := &tg.ChannelFull{}
		channel.SetPinnedMsgID(rpc.pinnedID)
		full, ok := output.(*tg.MessagesChatFull)
		if !ok {
			return fmt.Errorf("unexpected output %T", output)
		}
		full.FullChat = channel
	case *tg.MessagesUpdatePinnedMessageRequest:
		rpc.pinnedID = request.ID
		box, ok := output.(*tg.UpdatesBox)
		if !ok {
			return fmt.Errorf("unexpected output %T", output)
		}
		box.Updates = &tg.Updates{}
	default:
		return fmt.Errorf("unexpected request %T", input)
	}
	return nil
}

const expectedWelcomeText = "This fictional demo is refreshed by a weekly data wipe. Explore public notes and groups, then come back after the next reset."

func TestWelcomeSeedMessageUsesCleanRetryKey(t *testing.T) {
	seeded := welcomeSeedMessage()
	if seeded.marker != welcomeMarker() || seeded.text != expectedWelcomeText {
		t.Fatalf("welcome seed message = %+v", seeded)
	}
	if got, want := seeded.randomKey, welcomeMarker()+":clean"; got != want {
		t.Fatalf("welcome random key = %q, want %q", got, want)
	}
}

func TestSeedWelcomeIsMarkerFreeAndIdempotent(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			rpc := &welcomeRPC{}
			unrelated := &tg.Message{ID: 2, Message: "unrelated"}
			if legacy {
				rpc.messages = []tg.MessageClass{&tg.Message{ID: 1, Message: welcomeMarker() + " " + expectedWelcomeText}, unrelated}
				rpc.nextID = 2
			}
			api := tg.NewClient(rpc)
			remote := remoteSpace{channelID: 1, accessHash: 2}
			seeded := welcomeSeedMessage()
			for iteration := range 3 {
				if iteration == 2 {
					rpc.messages = nil
				}
				if err := seedWelcome(context.Background(), api, remote); err != nil {
					t.Fatal(err)
				}
				if legacy && iteration == 0 {
					if len(rpc.deleted) != 1 || rpc.deleted[0] != 1 || len(rpc.messages) != 2 || rpc.messages[0] != unrelated {
						t.Fatalf("legacy deletion affected wrong messages: %+v; %v", rpc.messages, rpc.deleted)
					}
				}
				var notes []*tg.Message
				for _, candidate := range rpc.messages {
					message, ok := candidate.(*tg.Message)
					if !ok {
						t.Fatalf("unexpected message %T", candidate)
					}
					if message.Message != "unrelated" {
						notes = append(notes, message)
					}
				}
				if len(notes) != 1 {
					t.Fatalf("iteration %d: got %d welcome messages, want 1", iteration, len(notes))
				}
				message := notes[0]
				if rpc.pinnedID != message.ID {
					t.Fatalf("pinned ID %d, want %d", rpc.pinnedID, message.ID)
				}
				if message.Message != expectedWelcomeText || strings.Contains(fmt.Sprint(message.Entities), "[[teagram-demo:") {
					t.Fatalf("welcome message contains unexpected text or entities: %+v", message)
				}
				if err := messageIsPinned(context.Background(), api, remote.peer(), seeded.marker, seeded); err != nil {
					t.Fatalf("verify pinned welcome: %v", err)
				}
				markers, err := historyMarkers(context.Background(), api, remote.peer(), seeded)
				if err != nil || markers[seeded.marker] != int64(message.ID) {
					t.Fatalf("welcome lookup = %v, %v", markers, err)
				}
			}
		})
	}
}
