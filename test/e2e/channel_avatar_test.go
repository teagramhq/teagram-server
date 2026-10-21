package e2e_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/config"
)

func TestChannelEditPhoto(t *testing.T) {
	t.Parallel()
	testChannelEditPhoto(t)
}

func testChannelEditPhoto(t *testing.T) {
	t.Helper()
	f := newSmokeFixtureWithSetup(t, config.RegistrationClosed, func(f *smokeFixture) {
		f.photoThumbs = buildSmokePhotoThumbSupervisor(t)
	})
	const phoneCreator, phoneMember, phoneOffline = "+15551022941", "+15551022942", "+15551022943"
	seedSmokeUsers(t, f, phoneCreator, phoneMember, phoneOffline)
	creator := newSmokeClient(t, f, "channel photo creator", phoneCreator)
	member := newSmokeClient(t, f, "channel photo member", phoneMember)
	offline := newSmokeClient(t, f, "channel photo offline member", phoneOffline)

	type channelCase struct {
		name        string
		megagroup   bool
		photoMarker byte
	}
	cases := []channelCase{
		{name: "broadcast", photoMarker: 0x31},
		{name: "supergroup", megagroup: true, photoMarker: 0x32},
	}
	type offlineBaseline struct {
		channelID int64
		pts       int
	}
	baselines := make([]offlineBaseline, 0, len(cases))
	photoIDs := make([][2]int64, 0, len(cases))

	for index, tc := range cases {
		if !t.Run(tc.name, func(t *testing.T) {
			var channelID int64
			if err := creator.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
				created, err := client.ChannelsCreateChannel(ctx, &tg.ChannelsCreateChannelRequest{
					Title: tc.name, About: "", Broadcast: !tc.megagroup, Megagroup: tc.megagroup,
				})
				if err != nil {
					return err
				}
				updates, ok := created.(*tg.Updates)
				if !ok {
					return fmt.Errorf("createChannel result = %T, want *tg.Updates", created)
				}
				for _, chat := range updates.Chats {
					if channel, ok := chat.(*tg.Channel); ok {
						channelID = channel.ID
						return nil
					}
				}
				return errors.New("createChannel omitted channel")
			}); err != nil {
				t.Fatalf("create %s: %v", tc.name, err)
			}
			if _, err := creator.seen.waitService(f.ctx, &tg.MessageActionChannelCreate{}); err != nil {
				t.Fatalf("creator create service update: %v", err)
			}

			var inviteHashValue string
			if err := creator.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
				invite, err := client.MessagesExportChatInvite(ctx, &tg.MessagesExportChatInviteRequest{Peer: peerChannel(creator.id, channelID)})
				if err != nil {
					return err
				}
				exported, ok := invite.(*tg.ChatInviteExported)
				if !ok {
					return fmt.Errorf("exportChatInvite result = %T, want *tg.ChatInviteExported", invite)
				}
				inviteHashValue = inviteHash(exported.Link)
				return nil
			}); err != nil {
				t.Fatalf("export %s invite: %v", tc.name, err)
			}
			for _, joiner := range []*smokeClient{member, offline} {
				if err := joiner.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
					_, err := client.MessagesImportChatInvite(ctx, inviteHashValue)
					return err
				}); err != nil {
					t.Fatalf("%s join %s: %v", joiner.label, tc.name, err)
				}
			}
			baseline, err := channelPhotoDifferencePts(f.ctx, offline, channelID, 0)
			if err != nil {
				t.Fatalf("capture %s offline channel pts: %v", tc.name, err)
			}
			baselines = append(baselines, offlineBaseline{channelID: channelID, pts: baseline})

			if err := creator.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
				result, err := client.ChannelsEditPhoto(ctx, &tg.ChannelsEditPhotoRequest{
					Channel: inputChannel(creator.id, channelID), Photo: &tg.InputChatPhotoEmpty{},
				})
				if err != nil {
					return err
				}
				updates, ok := result.(*tg.Updates)
				if !ok || len(updates.Updates) != 0 {
					return fmt.Errorf("already-empty clear = %#v, want empty Updates", result)
				}
				return nil
			}); err != nil {
				t.Fatalf("creator already-empty clear: %v", err)
			}

			bodyA := smokeJPEGAtUploadSize(t, smokePhotoPartCount)
			bodyB := distinctSmokePhotoBody(t, bodyA, tc.photoMarker)
			fileIDA, fileIDB := int64(10229410+index*10), int64(10229411+index*10)
			photoEditor := creator
			if tc.megagroup {
				photoEditor = member
			}
			if err := uploadChannelAvatar(f, photoEditor, channelID, fileIDA, bodyA); err != nil {
				t.Fatalf("%s upload photo A: %v", tc.name, err)
			}
			memberA, err := member.seen.waitService(f.ctx, &tg.MessageActionChatEditPhoto{})
			if err != nil {
				t.Fatalf("member receives %s photo A: %v", tc.name, err)
			}
			photoA := channelAvatarPhotoFromAction(t, memberA.svc.Action, tc.name+" member live photo A")
			assertSmokePhotoAction(t, photoA, bodyA, tc.name+" member live photo A")
			assertChannelAvatarService(t, memberA.svc.Action, photoA.ID, tc.name+" member live photo A")
			creatorA, err := creator.seen.waitService(f.ctx, &tg.MessageActionChatEditPhoto{})
			if err != nil {
				t.Fatalf("creator receives %s photo A: %v", tc.name, err)
			}
			assertChannelAvatarService(t, creatorA.svc.Action, photoA.ID, tc.name+" creator live photo A")
			assertChannelAvatarMetadata(t, f.ctx, member, channelID, photoA.ID)
			assertChannelAvatarDownloads(t, f.ctx, member, channelID, photoA, bodyA)

			if err := uploadChannelAvatar(f, creator, channelID, fileIDB, bodyB); err != nil {
				t.Fatalf("%s replace photo with B: %v", tc.name, err)
			}
			memberB, err := member.seen.waitService(f.ctx, &tg.MessageActionChatEditPhoto{})
			if err != nil {
				t.Fatalf("member receives %s photo B: %v", tc.name, err)
			}
			photoB := channelAvatarPhotoFromAction(t, memberB.svc.Action, tc.name+" member live photo B")
			assertSmokePhotoAction(t, photoB, bodyB, tc.name+" member live photo B")
			assertChannelAvatarService(t, memberB.svc.Action, photoB.ID, tc.name+" member live photo B")
			creatorB, err := creator.seen.waitService(f.ctx, &tg.MessageActionChatEditPhoto{})
			if err != nil {
				t.Fatalf("creator receives %s photo B: %v", tc.name, err)
			}
			assertChannelAvatarService(t, creatorB.svc.Action, photoB.ID, tc.name+" creator live photo B")
			assertChannelAvatarMetadata(t, f.ctx, member, channelID, photoB.ID)
			assertChannelAvatarDownloads(t, f.ctx, member, channelID, photoB, bodyB)
			assertChannelAvatarHistoryDownload(t, f.ctx, member, photoA, bodyA, tc.name+" retained photo A")
			assertSmokeLocationInvalid(t, f.ctx, member, &tg.InputPeerPhotoFileLocation{
				Peer: peerChannel(member.id, channelID), PhotoID: photoA.ID, Big: true,
			}, tc.name+" stale peer photo")

			clearer := creator
			if tc.megagroup {
				clearer = member
			}
			if err := clearer.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
				_, err := client.ChannelsEditPhoto(ctx, &tg.ChannelsEditPhotoRequest{
					Channel: inputChannel(clearer.id, channelID), Photo: &tg.InputChatPhotoEmpty{},
				})
				return err
			}); err != nil {
				t.Fatalf("%s clear current photo: %v", tc.name, err)
			}
			memberClear, err := member.seen.waitService(f.ctx, &tg.MessageActionChatDeletePhoto{})
			if err != nil {
				t.Fatalf("member receives %s clear: %v", tc.name, err)
			}
			assertChannelDeleteAvatarService(t, memberClear.svc.Action, tc.name+" member clear")
			creatorClear, err := creator.seen.waitService(f.ctx, &tg.MessageActionChatDeletePhoto{})
			if err != nil {
				t.Fatalf("creator receives %s clear: %v", tc.name, err)
			}
			assertChannelDeleteAvatarService(t, creatorClear.svc.Action, tc.name+" creator clear")
			assertChannelAvatarMetadata(t, f.ctx, member, channelID, 0)
			assertChannelAvatarHistoryDownload(t, f.ctx, member, photoB, bodyB, tc.name+" retained photo B after clear")
			assertSmokeLocationInvalid(t, f.ctx, member, &tg.InputPeerPhotoFileLocation{
				Peer: peerChannel(member.id, channelID), PhotoID: photoB.ID, Big: true,
			}, tc.name+" cleared peer photo")
			photoIDs = append(photoIDs, [2]int64{photoA.ID, photoB.ID})
		}) {
			return
		}
	}
	offline.stopClient(t)

	reconnected := f.savedSessionClient(offline.session)
	if err := reconnected.Run(f.ctx, func(ctx context.Context) error {
		for i, baseline := range baselines {
			difference, err := reconnected.API().UpdatesGetChannelDifference(ctx, &tg.UpdatesGetChannelDifferenceRequest{
				Channel: inputChannel(offline.id, baseline.channelID),
				Filter:  &tg.ChannelMessagesFilterEmpty{},
				Pts:     baseline.pts,
				Limit:   100,
			})
			if err != nil {
				return fmt.Errorf("get %s offline difference: %w", cases[i].name, err)
			}
			if err := assertChannelPhotoDifference(difference, photoIDs[i][0], photoIDs[i][1]); err != nil {
				return fmt.Errorf("%s offline difference: %w", cases[i].name, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("offline channel photo recovery: %v", err)
	}
}

func uploadChannelAvatar(f *smokeFixture, actor *smokeClient, channelID, fileID int64, body []byte) error {
	if err := uploadSmokeGroupParts(actor, fileID, body, smokePhotoPartCount); err != nil {
		return err
	}
	if err := actor.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		_, err := client.ChannelsEditPhoto(ctx, &tg.ChannelsEditPhotoRequest{
			Channel: inputChannel(actor.id, channelID),
			Photo:   uploadedSmokeGroupPhoto(fileID, smokePhotoPartCount, body),
		})
		return err
	}); err != nil {
		return err
	}
	return nil
}

func channelAvatarPhotoFromAction(t *testing.T, action tg.MessageActionClass, label string) *tg.Photo {
	t.Helper()
	edit, ok := action.(*tg.MessageActionChatEditPhoto)
	if !ok {
		t.Fatalf("%s action = %T, want chatEditPhoto", label, action)
	}
	photo, ok := edit.Photo.(*tg.Photo)
	if !ok {
		t.Fatalf("%s service action photo = %T, want *tg.Photo", label, edit.Photo)
	}
	return photo
}

func assertChannelAvatarService(t *testing.T, action tg.MessageActionClass, fileID int64, label string) {
	t.Helper()
	edit, ok := action.(*tg.MessageActionChatEditPhoto)
	if !ok {
		t.Fatalf("%s action = %T, want chatEditPhoto", label, action)
	}
	photo, ok := edit.Photo.(*tg.Photo)
	if !ok || photo.ID != fileID || photo.AccessHash == 0 || len(photo.FileReference) == 0 {
		t.Fatalf("%s photo = %#v, want identity %d with access hash and file reference", label, edit.Photo, fileID)
	}
}

func assertChannelDeleteAvatarService(t *testing.T, action tg.MessageActionClass, label string) {
	t.Helper()
	if _, ok := action.(*tg.MessageActionChatDeletePhoto); !ok {
		t.Fatalf("%s action = %T, want chatDeletePhoto", label, action)
	}
}

func assertChannelAvatarMetadata(t *testing.T, ctx context.Context, client *smokeClient, channelID, photoID int64) {
	t.Helper()
	if err := client.call(ctx, func(ctx context.Context, api *tg.Client) error {
		channelsResult, err := api.ChannelsGetChannels(ctx, []tg.InputChannelClass{inputChannel(client.id, channelID)})
		if err != nil {
			return err
		}
		channels, ok := channelsResult.(*tg.MessagesChats)
		if !ok || len(channels.Chats) != 1 {
			return fmt.Errorf("channels.getChannels result = %T with %d chats, want one", channelsResult, len(channels.Chats))
		}
		channel, ok := channels.Chats[0].(*tg.Channel)
		if !ok {
			return fmt.Errorf("channels.getChannels chat = %T, want *tg.Channel", channels.Chats[0])
		}
		if err := assertChannelChatPhoto(channel.Photo, photoID, "channels.getChannels"); err != nil {
			return err
		}

		full, err := api.ChannelsGetFullChannel(ctx, inputChannel(client.id, channelID))
		if err != nil {
			return err
		}
		channelFull, ok := full.FullChat.(*tg.ChannelFull)
		if !ok {
			return fmt.Errorf("channels.getFullChannel full chat = %T, want *tg.ChannelFull", full.FullChat)
		}
		if err := assertChannelPhoto(channelFull.ChatPhoto, photoID, "channels.getFullChannel"); err != nil {
			return err
		}

		peerDialogs, err := api.MessagesGetPeerDialogs(ctx, []tg.InputDialogPeerClass{&tg.InputDialogPeer{Peer: peerChannel(client.id, channelID)}})
		if err != nil {
			return err
		}
		if len(peerDialogs.Chats) != 1 {
			return fmt.Errorf("messages.getPeerDialogs returned %d chats, want one", len(peerDialogs.Chats))
		}
		peerChannel, ok := peerDialogs.Chats[0].(*tg.Channel)
		if !ok {
			return fmt.Errorf("messages.getPeerDialogs chat = %T, want *tg.Channel", peerDialogs.Chats[0])
		}
		return assertChannelChatPhoto(peerChannel.Photo, photoID, "messages.getPeerDialogs")
	}); err != nil {
		t.Fatalf("%s channel photo metadata: %v", client.label, err)
	}
}

func assertChannelChatPhoto(photo tg.ChatPhotoClass, photoID int64, source string) error {
	if photoID == 0 {
		if _, ok := photo.(*tg.ChatPhotoEmpty); !ok {
			return fmt.Errorf("%s photo = %T, want chatPhotoEmpty", source, photo)
		}
		return nil
	}
	chatPhoto, ok := photo.(*tg.ChatPhoto)
	if !ok || chatPhoto.PhotoID != photoID {
		return fmt.Errorf("%s photo = %#v, want current photo %d", source, photo, photoID)
	}
	return nil
}

func assertChannelPhoto(photo tg.PhotoClass, photoID int64, source string) error {
	if photoID == 0 {
		if _, ok := photo.(*tg.PhotoEmpty); !ok {
			return fmt.Errorf("%s photo = %T, want photoEmpty", source, photo)
		}
		return nil
	}
	got, ok := photo.(*tg.Photo)
	if !ok || got.ID != photoID {
		return fmt.Errorf("%s photo = %#v, want current photo %d", source, photo, photoID)
	}
	return nil
}

func channelFullPhoto(result *tg.MessagesChatFull, channelID, photoID int64) bool {
	if result == nil {
		return false
	}
	full, ok := result.FullChat.(*tg.ChannelFull)
	if !ok {
		return false
	}
	photo, ok := full.ChatPhoto.(*tg.Photo)
	return ok && photo.ID == photoID
}

func channelFullPhotoEmpty(result *tg.MessagesChatFull, channelID int64) bool {
	if result == nil {
		return false
	}
	full, ok := result.FullChat.(*tg.ChannelFull)
	if !ok {
		return false
	}
	_, ok = full.ChatPhoto.(*tg.PhotoEmpty)
	return ok
}

func assertChannelAvatarDownloads(t *testing.T, ctx context.Context, client *smokeClient, channelID int64, photo *tg.Photo, want []byte) {
	t.Helper()
	for _, big := range []bool{false, true} {
		var result tg.UploadFileClass
		if err := client.call(ctx, func(ctx context.Context, api *tg.Client) error {
			var err error
			result, err = api.UploadGetFile(ctx, &tg.UploadGetFileRequest{
				Location: &tg.InputPeerPhotoFileLocation{Peer: peerChannel(client.id, channelID), PhotoID: photo.ID, Big: big},
				Limit:    len(want),
			})
			return err
		}); err != nil {
			t.Fatalf("%s channel peer photo big=%v download: %v", client.label, big, err)
		}
		file, ok := result.(*tg.UploadFile)
		if !ok || len(file.Bytes) == 0 {
			t.Fatalf("%s channel peer photo big=%v result = %#v, want non-empty UploadFile", client.label, big, result)
		}
		if big && !bytes.Equal(file.Bytes, want) {
			t.Fatalf("%s channel big photo bytes = %d, want original %d bytes", client.label, len(file.Bytes), len(want))
		}
	}
	assertChannelAvatarHistoryDownload(t, ctx, client, photo, want, "current original")
}

func assertChannelAvatarHistoryDownload(t *testing.T, ctx context.Context, client *smokeClient, photo *tg.Photo, want []byte, label string) {
	t.Helper()
	var result tg.UploadFileClass
	if err := client.call(ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		result, err = api.UploadGetFile(ctx, &tg.UploadGetFileRequest{
			Location: &tg.InputPhotoFileLocation{ID: photo.ID, AccessHash: photo.AccessHash, ThumbSize: "w"},
			Limit:    len(want),
		})
		return err
	}); err != nil {
		t.Fatalf("%s %s photo download: %v", client.label, label, err)
	}
	file, ok := result.(*tg.UploadFile)
	if !ok || !bytes.Equal(file.Bytes, want) {
		t.Fatalf("%s %s photo result = %#v, want original %d bytes", client.label, label, result, len(want))
	}
}

func channelPhotoDifferencePts(ctx context.Context, client *smokeClient, channelID int64, fromPts int) (int, error) {
	var result tg.UpdatesChannelDifferenceClass
	err := client.call(ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		result, err = api.UpdatesGetChannelDifference(ctx, &tg.UpdatesGetChannelDifferenceRequest{
			Channel: inputChannel(client.id, channelID), Filter: &tg.ChannelMessagesFilterEmpty{}, Pts: fromPts, Limit: 100,
		})
		return err
	})
	if err != nil {
		return 0, err
	}
	switch difference := result.(type) {
	case *tg.UpdatesChannelDifference:
		return difference.Pts, nil
	case *tg.UpdatesChannelDifferenceEmpty:
		return difference.Pts, nil
	case *tg.UpdatesChannelDifferenceTooLong:
		return 0, errors.New("channel difference is too long to establish a baseline")
	default:
		return 0, fmt.Errorf("channel difference result = %T", result)
	}
}

func assertChannelPhotoDifference(result tg.UpdatesChannelDifferenceClass, photoA, photoB int64) error {
	difference, ok := result.(*tg.UpdatesChannelDifference)
	if !ok || !difference.Final {
		return fmt.Errorf("result = %T final=%v, want final channel difference", result, ok && difference.Final)
	}
	var gotPhotos []int64
	var clears int
	for _, class := range difference.NewMessages {
		service, ok := class.(*tg.MessageService)
		if !ok {
			continue
		}
		switch action := service.Action.(type) {
		case *tg.MessageActionChatEditPhoto:
			photo, ok := action.Photo.(*tg.Photo)
			if !ok {
				return fmt.Errorf("difference edit action photo = %T", action.Photo)
			}
			gotPhotos = append(gotPhotos, photo.ID)
		case *tg.MessageActionChatDeletePhoto:
			clears++
		}
	}
	if len(gotPhotos) != 2 || gotPhotos[0] != photoA || gotPhotos[1] != photoB || clears != 1 {
		return fmt.Errorf("photo actions = %v with %d clears, want [%d %d] and one clear", gotPhotos, clears, photoA, photoB)
	}
	return nil
}
