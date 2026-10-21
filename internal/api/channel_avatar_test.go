package api_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestChannelEditPhotoUsesSharedChannelHistoryForBothKinds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		megagroup bool
	}{
		{name: "broadcast"},
		{name: "supergroup", megagroup: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			s := openStore(t)
			creator := mustUser(t, s, "+15551228901")
			member := mustUser(t, s, "+15551228902")
			joiner := mustUser(t, s, "+15551228903")
			outsider := mustUser(t, s, "+15551228904")
			channel, err := s.CreateChannel(ctx, creator.ID, "Avatar "+tc.name, "", tc.megagroup)
			if err != nil {
				t.Fatalf("create channel: %v", err)
			}
			joinChannelByInvite(t, s, channel, member.ID)
			blobs := newBlobs(t)

			beforePts, err := s.ChannelState(ctx, channel.ID)
			if err != nil {
				t.Fatalf("initial channel state: %v", err)
			}
			beforeVersion := channel.Version
			if _, err := api.EditChannelPhotoForTest(s, creator.ID, blobs, api.TestMaxUserStorageBytes,
				&tg.ChannelsEditPhotoRequest{Channel: api.InputChannel(creator.ID, channel.ID), Photo: &tg.InputChatPhotoEmpty{}}); err != nil {
				t.Fatalf("creator already-empty clear: %v", err)
			}
			unchanged, ok, err := s.ChannelByID(ctx, channel.ID)
			if err != nil || !ok || unchanged.Version != beforeVersion {
				t.Fatalf("already-empty clear changed channel: ok=%v version=%d err=%v", ok, unchanged.Version, err)
			}
			if pts, err := s.ChannelState(ctx, channel.ID); err != nil || pts != beforePts {
				t.Fatalf("already-empty clear pts=%d err=%v, want %d", pts, err, beforePts)
			}

			bodyA := jpegPhotoPayload(t, 640, 480)
			const uploadA = int64(228901)
			saveParts(t, s, creator.ID, uploadA, bodyA)
			resultA, err := api.EditChannelPhotoForTest(s, creator.ID, blobs, api.TestMaxUserStorageBytes,
				&tg.ChannelsEditPhotoRequest{Channel: api.InputChannel(creator.ID, channel.ID), Photo: uploadedGroupPhoto(uploadA, bodyA)})
			if err != nil {
				t.Fatalf("creator select photo A: %v", err)
			}
			serviceA, photoA := channelAvatarEditFromResult(t, resultA)
			if serviceA.ID != beforePts+1 || photoA.ID <= 0 {
				t.Fatalf("photo A service id/photo = %d/%d, want local id %d and a file", serviceA.ID, photoA.ID, beforePts+1)
			}
			selected, ok, err := s.ChannelByID(ctx, channel.ID)
			if err != nil || !ok || selected.CurrentPhotoFileID == nil || *selected.CurrentPhotoFileID != photoA.ID || selected.CurrentPhotoUploaderID == nil || *selected.CurrentPhotoUploaderID != creator.ID {
				t.Fatalf("selected channel photo = %v/%v, ok=%v err=%v", selected.CurrentPhotoFileID, selected.CurrentPhotoUploaderID, ok, err)
			}
			assertChannelAvatarMetadata(t, s, member.ID, channel.ID, photoA.ID)
			assertChannelAvatarHidden(t, s, blobs, outsider.ID, channel.ID, photoA.ID, photoA.AccessHash)
			assertChannelAvatarBytes(t, s, blobs, member.ID, channel.ID, photoA.ID, photoA.AccessHash, bodyA, true)
			assertChannelAvatarPeerBytes(t, s, blobs, member.ID, channel.ID, photoA.ID, bodyA, true)

			bodyB := jpegPhotoPayload(t, 480, 640)
			const uploadB = int64(228902)
			saveParts(t, s, member.ID, uploadB, bodyB)
			resultB, err := api.EditChannelPhotoForTest(s, member.ID, blobs, api.TestMaxUserStorageBytes,
				&tg.ChannelsEditPhotoRequest{Channel: api.InputChannel(member.ID, channel.ID), Photo: uploadedGroupPhoto(uploadB, bodyB)})
			if tc.megagroup {
				if err != nil {
					t.Fatalf("unrestricted supergroup member select photo B: %v", err)
				}
			} else {
				if rpcMessage(t, err) != "CHAT_ADMIN_REQUIRED" {
					t.Fatalf("broadcast member select photo B = %v, want CHAT_ADMIN_REQUIRED", err)
				}
				if parts, _, _, partErr := s.UploadPartsSummary(ctx, member.ID, uploadB); partErr != nil || parts != 1 {
					t.Fatalf("unauthorized edit consumed member parts: parts=%d err=%v", parts, partErr)
				}
				if err := s.SetChannelRoleWithRights(ctx, channel.ID, creator.ID, member.ID, 1, true); err != nil {
					t.Fatalf("grant broadcast admin change_info: %v", err)
				}
				resultB, err = api.EditChannelPhotoForTest(s, member.ID, blobs, api.TestMaxUserStorageBytes,
					&tg.ChannelsEditPhotoRequest{Channel: api.InputChannel(member.ID, channel.ID), Photo: uploadedGroupPhoto(uploadB, bodyB)})
				if err != nil {
					t.Fatalf("authorized broadcast admin select photo B: %v", err)
				}
			}
			serviceB, photoB := channelAvatarEditFromResult(t, resultB)
			if serviceB.ID != serviceA.ID+1 || photoB.ID <= 0 || photoB.ID == photoA.ID {
				t.Fatalf("photo B service id/photo = %d/%d, want new action after %d with a fresh id", serviceB.ID, photoB.ID, serviceA.ID)
			}
			assertChannelAvatarBytes(t, s, blobs, member.ID, channel.ID, photoA.ID, photoA.AccessHash, bodyA, false)
			assertChannelAvatarBytes(t, s, blobs, member.ID, channel.ID, photoB.ID, photoB.AccessHash, bodyB, true)
			assertChannelAvatarPeerBytes(t, s, blobs, member.ID, channel.ID, photoA.ID, bodyA, false)
			assertChannelAvatarPeerBytes(t, s, blobs, member.ID, channel.ID, photoB.ID, bodyB, true)

			if tc.megagroup {
				if _, _, changed, err := s.SetChannelPreHistoryHidden(ctx, channel.ID, creator.ID, true); err != nil || !changed {
					t.Fatalf("enable supergroup prehistory hiding: changed=%v err=%v", changed, err)
				}
			}
			joinChannelByInvite(t, s, channel, joiner.ID)
			joinerMember, joined, err := s.ChannelMemberOf(ctx, channel.ID, joiner.ID)
			if err != nil || !joined {
				t.Fatalf("load new member: joined=%v err=%v", joined, err)
			}
			if tc.megagroup && joinerMember.AvailableMinID < int64(serviceB.ID) {
				t.Fatalf("new supergroup member history floor=%d, want at least replaced photo id %d", joinerMember.AvailableMinID, serviceB.ID)
			}
			if _, err := s.CurrentChannelPhotoForDownload(ctx, photoB.ID, photoB.AccessHash, joiner.ID); err != nil {
				t.Fatalf("new member current photo access: %v", err)
			}
			assertChannelAvatarPeerBytes(t, s, blobs, joiner.ID, channel.ID, photoB.ID, bodyB, true)

			resultClear, err := api.EditChannelPhotoForTest(s, creator.ID, blobs, api.TestMaxUserStorageBytes,
				&tg.ChannelsEditPhotoRequest{Channel: api.InputChannel(creator.ID, channel.ID), Photo: &tg.InputChatPhotoEmpty{}})
			if err != nil {
				t.Fatalf("creator clear current photo: %v", err)
			}
			clearService := channelAvatarDeleteFromResult(t, resultClear)
			if clearService.ID != serviceB.ID+1 {
				t.Fatalf("clear service id=%d, want %d", clearService.ID, serviceB.ID+1)
			}
			cleared, ok, err := s.ChannelByID(ctx, channel.ID)
			if err != nil || !ok || cleared.CurrentPhotoFileID != nil || cleared.CurrentPhotoUploaderID != nil {
				t.Fatalf("current photo after clear = %v/%v, ok=%v err=%v", cleared.CurrentPhotoFileID, cleared.CurrentPhotoUploaderID, ok, err)
			}
			if _, err := s.CurrentChannelPhotoForDownload(ctx, photoB.ID, photoB.AccessHash, member.ID); !errors.Is(err, store.ErrFileNotFound) {
				t.Fatalf("member retained current-photo entitlement after clear: %v", err)
			}
			assertChannelAvatarPeerBytes(t, s, blobs, member.ID, channel.ID, photoB.ID, bodyB, false)
			if _, err := s.FileForDownload(ctx, photoB.ID, photoB.AccessHash, member.ID); err != nil {
				t.Fatalf("member lost retained photo service history: %v", err)
			}
			_, joinerHistoryErr := s.FileForDownload(ctx, photoB.ID, photoB.AccessHash, joiner.ID)
			if tc.megagroup {
				if !errors.Is(joinerHistoryErr, store.ErrFileNotFound) {
					t.Fatalf("new hidden-history member gained below-floor photo history after clear: floor=%d replaced photo id=%d err=%v", joinerMember.AvailableMinID, serviceB.ID, joinerHistoryErr)
				}
			} else if joinerHistoryErr != nil {
				t.Fatalf("new broadcast subscriber lost full-history photo access after clear: %v", joinerHistoryErr)
			}
		})
	}
}

func TestChannelPhotoShowsOnCurrentMemberResponses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	creator := mustUser(t, s, "+15551228911")
	member := mustUser(t, s, "+15551228912")
	participant := mustUser(t, s, "+15551228913")
	joiner := mustUser(t, s, "+15551228914")
	blobs := newBlobs(t)

	channel, err := s.CreateChannel(ctx, creator.ID, "AvatarResponseGroup", "", true)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	if err := s.ClaimChannelUsername(ctx, channel.ID, "avatarresponsegroup"); err != nil {
		t.Fatalf("make channel public: %v", err)
	}
	joinChannelByInvite(t, s, channel, member.ID)
	joinChannelByInvite(t, s, channel, participant.ID)
	fileID := selectChannelAvatarForTest(t, s, blobs, creator.ID, channel.ID, 228911)

	joinResult, err := api.JoinChannelForTest(s, joiner.ID, &tg.ChannelsJoinChannelRequest{
		Channel: api.InputChannel(joiner.ID, channel.ID),
	})
	if err != nil {
		t.Fatalf("join public channel: %v", err)
	}
	join, ok := joinResult.(*tg.MessagesChatInviteJoinResultOk)
	if !ok || join.Updates == nil {
		t.Fatalf("join result = %T, want successful channel join", joinResult)
	}
	joinUpdates, ok := join.Updates.(*tg.Updates)
	if !ok {
		t.Fatalf("join updates = %T, want *tg.Updates", join.Updates)
	}
	assertChannelAvatarInChats(t, "channels.joinChannel", joinUpdates.Chats, channel.ID, fileID)

	searchResult, err := api.ContactsSearchForTest(s, member.ID, &tg.ContactsSearchRequest{Q: "AvatarResponseGroup", Limit: 10})
	if err != nil {
		t.Fatalf("contacts.search: %v", err)
	}
	search, ok := searchResult.(*tg.ContactsFound)
	if !ok {
		t.Fatalf("contacts.search result = %T, want *tg.ContactsFound", searchResult)
	}
	assertChannelAvatarInChats(t, "contacts.search", search.Chats, channel.ID, fileID)

	inputChannel := api.InputChannel(creator.ID, channel.ID)
	participantsResult, err := api.GetChannelParticipantsForTest(s, creator.ID, &tg.ChannelsGetParticipantsRequest{
		Channel: inputChannel,
		Filter:  &tg.ChannelParticipantsRecent{},
		Limit:   10,
	})
	if err != nil {
		t.Fatalf("channels.getParticipants: %v", err)
	}
	participantsPage, ok := participantsResult.(*tg.ChannelsChannelParticipants)
	if !ok {
		t.Fatalf("channels.getParticipants result = %T, want *tg.ChannelsChannelParticipants", participantsResult)
	}
	assertChannelAvatarInChats(t, "channels.getParticipants", participantsPage.Chats, channel.ID, fileID)

	participantResult, err := api.GetChannelParticipantForTest(s, creator.ID, &tg.ChannelsGetParticipantRequest{
		Channel:     inputChannel,
		Participant: api.InputPeerUser(creator.ID, member.ID),
	})
	if err != nil {
		t.Fatalf("channels.getParticipant: %v", err)
	}
	participantInfo, ok := participantResult.(*tg.ChannelsChannelParticipant)
	if !ok {
		t.Fatalf("channels.getParticipant result = %T, want *tg.ChannelsChannelParticipant", participantResult)
	}
	assertChannelAvatarInChats(t, "channels.getParticipant", participantInfo.Chats, channel.ID, fileID)

	peerSettingsResult, err := api.GetPeerSettingsForTest(s, member.ID, &tg.MessagesGetPeerSettingsRequest{
		Peer: api.InputPeerChannel(member.ID, channel.ID),
	})
	if err != nil {
		t.Fatalf("messages.getPeerSettings: %v", err)
	}
	peerSettings, ok := peerSettingsResult.(*tg.MessagesPeerSettings)
	if !ok {
		t.Fatalf("messages.getPeerSettings result = %T, want *tg.MessagesPeerSettings", peerSettingsResult)
	}
	assertChannelAvatarInChats(t, "messages.getPeerSettings", peerSettings.Chats, channel.ID, fileID)

	commonChatsResult, err := api.GetCommonChatsForTest(s, member.ID, &tg.MessagesGetCommonChatsRequest{
		UserID: api.InputUser(member.ID, participant.ID),
		Limit:  10,
	})
	if err != nil {
		t.Fatalf("messages.getCommonChats: %v", err)
	}
	commonChats, ok := commonChatsResult.(*tg.MessagesChatsSlice)
	if !ok {
		t.Fatalf("messages.getCommonChats result = %T, want *tg.MessagesChatsSlice", commonChatsResult)
	}
	assertChannelAvatarInChats(t, "messages.getCommonChats", commonChats.Chats, channel.ID, fileID)

	dialogsResult, err := api.GetDialogsForTest(s, member.ID)
	if err != nil {
		t.Fatalf("messages.getDialogs: %v", err)
	}
	dialogs, ok := dialogsResult.(*tg.MessagesDialogs)
	if !ok {
		t.Fatalf("messages.getDialogs result = %T, want *tg.MessagesDialogs", dialogsResult)
	}
	assertChannelAvatarInChats(t, "messages.getDialogs", dialogs.Chats, channel.ID, fileID)

	privateChannel, err := s.CreateChannel(ctx, creator.ID, "PrivateAvatarToggle", "", true)
	if err != nil {
		t.Fatalf("create private channel: %v", err)
	}
	privateFileID := selectChannelAvatarForTest(t, s, blobs, creator.ID, privateChannel.ID, 228912)
	toggleResult, err := api.TogglePreHistoryHiddenForTest(s, creator.ID, &tg.ChannelsTogglePreHistoryHiddenRequest{
		Channel: api.InputChannel(creator.ID, privateChannel.ID), Enabled: true,
	})
	if err != nil {
		t.Fatalf("channels.togglePreHistoryHidden: %v", err)
	}
	toggle, ok := toggleResult.(*tg.Updates)
	if !ok {
		t.Fatalf("channels.togglePreHistoryHidden result = %T, want *tg.Updates", toggleResult)
	}
	assertChannelAvatarInChats(t, "channels.togglePreHistoryHidden", toggle.Chats, privateChannel.ID, privateFileID)

	broadcast, err := s.CreateChannel(ctx, creator.ID, "SignatureAvatarToggle", "", false)
	if err != nil {
		t.Fatalf("create broadcast: %v", err)
	}
	broadcastFileID := selectChannelAvatarForTest(t, s, blobs, creator.ID, broadcast.ID, 228913)
	signaturesResult, err := api.ToggleSignaturesForTest(s, creator.ID, &tg.ChannelsToggleSignaturesRequest{
		Channel: api.InputChannel(creator.ID, broadcast.ID), SignaturesEnabled: true,
	})
	if err != nil {
		t.Fatalf("channels.toggleSignatures: %v", err)
	}
	signatures, ok := signaturesResult.(*tg.Updates)
	if !ok {
		t.Fatalf("channels.toggleSignatures result = %T, want *tg.Updates", signaturesResult)
	}
	assertChannelAvatarInChats(t, "channels.toggleSignatures", signatures.Chats, broadcast.ID, broadcastFileID)
}

func selectChannelAvatarForTest(t *testing.T, s *store.Store, blobs blob.Store, userID, channelID, uploadID int64) int64 {
	t.Helper()
	body := jpegPhotoPayload(t, 320, 240)
	saveParts(t, s, userID, uploadID, body)
	result, err := api.EditChannelPhotoForTest(s, userID, blobs, api.TestMaxUserStorageBytes,
		&tg.ChannelsEditPhotoRequest{
			Channel: api.InputChannel(userID, channelID),
			Photo:   uploadedGroupPhoto(uploadID, body),
		})
	if err != nil {
		t.Fatalf("select channel photo: %v", err)
	}
	_, photo := channelAvatarEditFromResult(t, result)
	return photo.ID
}

func assertChannelAvatarInChats(t *testing.T, source string, chats []tg.ChatClass, channelID, fileID int64) {
	t.Helper()
	for _, candidate := range chats {
		channel, ok := candidate.(*tg.Channel)
		if ok && channel.ID == channelID {
			assertChannelAvatarChatPhoto(t, source, channel.Photo, fileID)
			return
		}
	}
	t.Fatalf("%s chats did not contain channel %d: %#v", source, channelID, chats)
}

func assertChannelAvatarMetadata(t *testing.T, s *store.Store, viewerID, channelID, fileID int64) {
	t.Helper()
	channelsResult, err := api.GetChannelsForTest(s, viewerID, &tg.ChannelsGetChannelsRequest{ID: []tg.InputChannelClass{api.InputChannel(viewerID, channelID)}})
	if err != nil {
		t.Fatalf("get channel metadata: %v", err)
	}
	channels, ok := channelsResult.(*tg.MessagesChats)
	if !ok || len(channels.Chats) != 1 {
		t.Fatalf("getChannels result = %T with %d chats, want one", channelsResult, len(channels.Chats))
	}
	channel, ok := channels.Chats[0].(*tg.Channel)
	if !ok {
		t.Fatalf("getChannels chat = %T, want *tg.Channel", channels.Chats[0])
	}
	assertChannelAvatarChatPhoto(t, "channels.getChannels", channel.Photo, fileID)

	fullResult, err := api.GetFullChannelForTest(s, viewerID, &tg.ChannelsGetFullChannelRequest{Channel: api.InputChannel(viewerID, channelID)})
	if err != nil {
		t.Fatalf("get full channel: %v", err)
	}
	full, ok := fullResult.(*tg.MessagesChatFull)
	if !ok {
		t.Fatalf("getFullChannel result = %T, want *tg.MessagesChatFull", fullResult)
	}
	channelFull, ok := full.FullChat.(*tg.ChannelFull)
	if !ok {
		t.Fatalf("getFullChannel full chat = %T, want *tg.ChannelFull", full.FullChat)
	}
	assertChannelAvatarPhoto(t, "channels.getFullChannel", channelFull.ChatPhoto, fileID)

	peerDialogsResult, err := api.GetPeerDialogsForTest(s, viewerID, &tg.MessagesGetPeerDialogsRequest{
		Peers: []tg.InputDialogPeerClass{&tg.InputDialogPeer{Peer: api.InputPeerChannel(viewerID, channelID)}},
	})
	if err != nil {
		t.Fatalf("get peer dialogs: %v", err)
	}
	peerDialogs, ok := peerDialogsResult.(*tg.MessagesPeerDialogs)
	if !ok || len(peerDialogs.Chats) != 1 {
		t.Fatalf("getPeerDialogs result = %T with %d chats, want one", peerDialogsResult, len(peerDialogs.Chats))
	}
	peerChannel, ok := peerDialogs.Chats[0].(*tg.Channel)
	if !ok {
		t.Fatalf("getPeerDialogs chat = %T, want *tg.Channel", peerDialogs.Chats[0])
	}
	assertChannelAvatarChatPhoto(t, "messages.getPeerDialogs", peerChannel.Photo, fileID)
}

func assertChannelAvatarChatPhoto(t *testing.T, source string, photo tg.ChatPhotoClass, fileID int64) {
	t.Helper()
	chatPhoto, ok := photo.(*tg.ChatPhoto)
	if !ok || chatPhoto.PhotoID != fileID {
		t.Fatalf("%s photo = %#v, want current photo %d", source, photo, fileID)
	}
}

func assertChannelAvatarPhoto(t *testing.T, source string, photo tg.PhotoClass, fileID int64) {
	t.Helper()
	got, ok := photo.(*tg.Photo)
	if !ok || got.ID != fileID {
		t.Fatalf("%s photo = %#v, want current photo %d", source, photo, fileID)
	}
}

func assertChannelAvatarHidden(t *testing.T, s *store.Store, blobs blob.Store, viewerID, channelID, fileID, accessHash int64) {
	t.Helper()
	channelsResult, err := api.GetChannelsForTest(s, viewerID, &tg.ChannelsGetChannelsRequest{ID: []tg.InputChannelClass{api.InputChannel(viewerID, channelID)}})
	if err != nil {
		t.Fatalf("get outsider channel metadata: %v", err)
	}
	channels, ok := channelsResult.(*tg.MessagesChats)
	if !ok || len(channels.Chats) != 1 {
		t.Fatalf("outsider getChannels result = %T with %d chats, want one", channelsResult, len(channels.Chats))
	}
	if forbidden, ok := channels.Chats[0].(*tg.ChannelForbidden); !ok || forbidden.Title != "" {
		t.Fatalf("outsider channel = %#v, want metadata-free *tg.ChannelForbidden", channels.Chats[0])
	}
	for name, location := range map[string]tg.InputFileLocationClass{
		"photo": &tg.InputPhotoFileLocation{ID: fileID, AccessHash: accessHash, ThumbSize: "x"},
		"peer photo": &tg.InputPeerPhotoFileLocation{
			Peer: api.InputPeerChannel(viewerID, channelID), PhotoID: fileID, Big: true,
		},
	} {
		if _, err := api.GetFileForTest(s, viewerID, blobs, &tg.UploadGetFileRequest{Location: location, Limit: 1024}); rpcMessage(t, err) != "LOCATION_INVALID" {
			t.Fatalf("outsider %s download = %v, want LOCATION_INVALID", name, err)
		}
	}
}

func channelAvatarEditFromResult(t *testing.T, enc any) (*tg.MessageService, *tg.Photo) {
	t.Helper()
	updates, ok := enc.(*tg.Updates)
	if !ok {
		t.Fatalf("channel photo result = %T, want *tg.Updates", enc)
	}
	for _, update := range updates.Updates {
		created, ok := update.(*tg.UpdateNewChannelMessage)
		if !ok {
			continue
		}
		service, ok := created.Message.(*tg.MessageService)
		if !ok {
			continue
		}
		action, ok := service.Action.(*tg.MessageActionChatEditPhoto)
		if !ok {
			continue
		}
		photo, ok := action.Photo.(*tg.Photo)
		if !ok {
			t.Fatalf("channel edit photo action photo = %T, want *tg.Photo", action.Photo)
		}
		return service, photo
	}
	t.Fatal("channel photo result omitted edit-photo service action")
	return nil, nil
}

func channelAvatarDeleteFromResult(t *testing.T, enc any) *tg.MessageService {
	t.Helper()
	updates, ok := enc.(*tg.Updates)
	if !ok {
		t.Fatalf("channel photo clear result = %T, want *tg.Updates", enc)
	}
	for _, update := range updates.Updates {
		created, ok := update.(*tg.UpdateNewChannelMessage)
		if !ok {
			continue
		}
		service, ok := created.Message.(*tg.MessageService)
		if ok {
			if _, ok := service.Action.(*tg.MessageActionChatDeletePhoto); ok {
				return service
			}
		}
	}
	t.Fatal("channel photo clear result omitted delete-photo service action")
	return nil
}

func assertChannelAvatarBytes(t *testing.T, s *store.Store, blobs blob.Store, viewerID, channelID, fileID, accessHash int64, want []byte, current bool) {
	t.Helper()
	if current {
		if _, err := s.CurrentChannelPeerPhotoForDownload(context.Background(), channelID, fileID, viewerID); err != nil {
			t.Fatalf("viewer %d current peer photo %d: %v", viewerID, fileID, err)
		}
	} else if _, err := s.CurrentChannelPeerPhotoForDownload(context.Background(), channelID, fileID, viewerID); !errors.Is(err, store.ErrFileNotFound) {
		t.Fatalf("viewer %d stale peer photo %d = %v, want hidden", viewerID, fileID, err)
	}
	result, err := api.GetFileForTest(s, viewerID, blobs, &tg.UploadGetFileRequest{
		Location: &tg.InputPhotoFileLocation{ID: fileID, AccessHash: accessHash, ThumbSize: "x"},
		Limit:    len(want),
	})
	if err != nil {
		t.Fatalf("viewer %d photo %d download: %v", viewerID, fileID, err)
	}
	file, ok := result.(*tg.UploadFile)
	if !ok || !bytes.Equal(file.Bytes, want) {
		t.Fatalf("viewer %d photo %d bytes = %#v, want %d bytes", viewerID, fileID, result, len(want))
	}
}

func assertChannelAvatarPeerBytes(t *testing.T, s *store.Store, blobs blob.Store, viewerID, channelID, fileID int64, want []byte, current bool) {
	t.Helper()
	result, err := api.GetFileForTest(s, viewerID, blobs, &tg.UploadGetFileRequest{
		Location: &tg.InputPeerPhotoFileLocation{Peer: api.InputPeerChannel(viewerID, channelID), PhotoID: fileID, Big: true},
		Limit:    len(want),
	})
	if !current {
		if rpcMessage(t, err) != "LOCATION_INVALID" {
			t.Fatalf("viewer %d stale channel peer photo %d = %v, want LOCATION_INVALID", viewerID, fileID, err)
		}
		return
	}
	if err != nil {
		t.Fatalf("viewer %d channel peer photo %d download: %v", viewerID, fileID, err)
	}
	file, ok := result.(*tg.UploadFile)
	if !ok || !bytes.Equal(file.Bytes, want) {
		t.Fatalf("viewer %d channel peer photo %d bytes = %#v, want %d bytes", viewerID, fileID, result, len(want))
	}
}

func channelAvatarHistoryCount(t *testing.T, s *store.Store, channelID int64) int {
	t.Helper()
	rows, err := s.ChannelHistory(context.Background(), channelID, 0, 50)
	if err != nil {
		t.Fatalf("channel history: %v", err)
	}
	return len(rows)
}

func TestChannelEditPhotoRejectsUnsupportedAndUnavailableInputs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	creator := mustUser(t, s, "+15551228911")
	member := mustUser(t, s, "+15551228912")
	outsider := mustUser(t, s, "+15551228913")
	channel, err := s.CreateChannel(ctx, creator.ID, "Avatar inputs", "", true)
	if err != nil {
		t.Fatalf("create supergroup: %v", err)
	}
	joinChannelByInvite(t, s, channel, member.ID)
	blobs := newBlobs(t)

	video := &tg.InputChatUploadedPhoto{}
	video.SetVideo(&tg.InputFile{ID: 228911, Parts: 1, Name: "video.mp4"})
	unknownFlags := &tg.InputChatUploadedPhoto{}
	unknownFlags.SetFile(&tg.InputFile{ID: 228912, Parts: 1, Name: "photo.jpg"})
	unknownFlags.Flags.Set(5)
	big := &tg.InputChatUploadedPhoto{}
	big.SetFile(&tg.InputFileBig{ID: 228913, Parts: 1, Name: "photo.jpg"})
	for _, tc := range []struct {
		name  string
		photo tg.InputChatPhotoClass
		code  string
	}{
		{name: "existing photo reselection", photo: &tg.InputChatPhoto{ID: &tg.InputPhotoEmpty{}}, code: "INPUT_REQUEST_INVALID"},
		{name: "video", photo: video, code: "INPUT_REQUEST_INVALID"},
		{name: "unknown flags", photo: unknownFlags, code: "INPUT_REQUEST_INVALID"},
		{name: "inputFileBig", photo: big, code: "MEDIA_INVALID"},
		{name: "missing file flag", photo: &tg.InputChatUploadedPhoto{}, code: "INPUT_REQUEST_INVALID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := api.EditChannelPhotoForTest(s, creator.ID, blobs, api.TestMaxUserStorageBytes,
				&tg.ChannelsEditPhotoRequest{Channel: api.InputChannel(creator.ID, channel.ID), Photo: tc.photo})
			if got := rpcMessage(t, err); got != tc.code {
				t.Fatalf("editChannelPhoto error = %s, want %s", got, tc.code)
			}
		})
	}
	if _, err := api.EditChannelPhotoForTest(s, outsider.ID, blobs, api.TestMaxUserStorageBytes,
		&tg.ChannelsEditPhotoRequest{Channel: api.InputChannel(outsider.ID, channel.ID), Photo: &tg.InputChatPhotoEmpty{}}); rpcMessage(t, err) != "PEER_ID_INVALID" {
		t.Fatalf("outsider clear = %v, want PEER_ID_INVALID", err)
	}
	if _, err := api.EditChannelPhotoForTest(s, outsider.ID, blobs, api.TestMaxUserStorageBytes,
		&tg.ChannelsEditPhotoRequest{Channel: api.InputChannel(outsider.ID, channel.ID), Photo: &tg.InputChatPhoto{ID: &tg.InputPhotoEmpty{}}}); rpcMessage(t, err) != "INPUT_REQUEST_INVALID" {
		t.Fatalf("unsupported outsider photo input = %v, want INPUT_REQUEST_INVALID", err)
	}
	if _, err := api.EditChannelPhotoForTest(s, creator.ID, blobs, api.TestMaxUserStorageBytes,
		&tg.ChannelsEditPhotoRequest{Channel: api.InputChannel(outsider.ID, channel.ID), Photo: &tg.InputChatPhotoEmpty{}}); rpcMessage(t, err) != "PEER_ID_INVALID" {
		t.Fatalf("borrowed input-channel hash = %v, want PEER_ID_INVALID", err)
	}

	body := jpegPhotoPayload(t, 640, 480)
	const foreignUpload = int64(228914)
	saveParts(t, s, member.ID, foreignUpload, body)
	_, err = api.EditChannelPhotoForTest(s, creator.ID, blobs, api.TestMaxUserStorageBytes,
		&tg.ChannelsEditPhotoRequest{Channel: api.InputChannel(creator.ID, channel.ID), Photo: uploadedGroupPhoto(foreignUpload, body)})
	if got := rpcMessage(t, err); got != "MEDIA_INVALID" {
		t.Fatalf("foreign upload = %s, want MEDIA_INVALID", got)
	}
	if parts, _, _, partErr := s.UploadPartsSummary(ctx, member.ID, foreignUpload); partErr != nil || parts != 1 {
		t.Fatalf("foreign upload attempt consumed owner parts: parts=%d err=%v", parts, partErr)
	}
	ownBody := jpegPhotoPayload(t, 640, 480)
	saveParts(t, s, creator.ID, foreignUpload, ownBody)
	ownedResult, err := api.EditChannelPhotoForTest(s, creator.ID, blobs, api.TestMaxUserStorageBytes,
		&tg.ChannelsEditPhotoRequest{Channel: api.InputChannel(creator.ID, channel.ID), Photo: uploadedGroupPhoto(foreignUpload, ownBody)})
	if err != nil {
		t.Fatalf("same numeric upload id under caller account: %v", err)
	}
	_, ownedPhoto := channelAvatarEditFromResult(t, ownedResult)
	assertChannelAvatarBytes(t, s, blobs, member.ID, channel.ID, ownedPhoto.ID, ownedPhoto.AccessHash, ownBody, true)
	if parts, _, _, partErr := s.UploadPartsSummary(ctx, member.ID, foreignUpload); partErr != nil || parts != 1 {
		t.Fatalf("same numeric upload id changed foreign owner's parts: parts=%d err=%v", parts, partErr)
	}
	if _, _, err := s.SetChannelDefaultBannedRights(ctx, channel.ID, creator.ID, []string{"change_info"}); err != nil {
		t.Fatalf("restrict supergroup change_info: %v", err)
	}
	if _, err := api.EditChannelPhotoForTest(s, member.ID, blobs, api.TestMaxUserStorageBytes,
		&tg.ChannelsEditPhotoRequest{Channel: api.InputChannel(member.ID, channel.ID), Photo: &tg.InputChatPhotoEmpty{}}); rpcMessage(t, err) != "CHAT_ADMIN_REQUIRED" {
		t.Fatalf("restricted member clear = %v, want CHAT_ADMIN_REQUIRED", err)
	}
	if _, err := api.EditChannelPhotoForTest(s, creator.ID, blobs, api.TestMaxUserStorageBytes,
		&tg.ChannelsEditPhotoRequest{Channel: api.InputChannel(creator.ID, channel.ID), Photo: &tg.InputChatPhotoEmpty{}}); err != nil {
		t.Fatalf("creator already-empty clear under restriction: %v", err)
	}
	if _, _, err := s.SetChannelDefaultBannedRights(ctx, channel.ID, creator.ID, []string{}); err != nil {
		t.Fatalf("restore default rights: %v", err)
	}
	if err := s.SetChannelRoleWithRights(ctx, channel.ID, creator.ID, member.ID, 1, false); err != nil {
		t.Fatalf("promote member to admin without change_info: %v", err)
	}
	if _, _, err := s.SetChannelDefaultBannedRights(ctx, channel.ID, creator.ID, []string{"change_info"}); err != nil {
		t.Fatalf("restrict admin by default rights: %v", err)
	}
	if err := s.CheckChannelPhotoEditPermission(ctx, channel.ID, member.ID); !errors.Is(err, store.ErrChatAdminRequired) {
		t.Fatalf("admin without persisted change_info under restriction = %v, want forbidden", err)
	}
	if err := s.CheckChannelPhotoEditPermission(ctx, channel.ID, creator.ID); err != nil {
		t.Fatalf("creator explicit authority: %v", err)
	}
	if err := s.CheckChannelPhotoEditPermission(ctx, channel.ID+1000, creator.ID); !errors.Is(err, store.ErrNotMember) {
		t.Fatalf("missing channel photo permission = %v, want ErrNotMember", err)
	}
}

func TestChannelEditPhotoPublicNonmembersKeepEmptyPhotoAndPublicMetadata(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	creator := mustUser(t, s, "+15551228951")
	outsider := mustUser(t, s, "+15551228952")
	channel, err := s.CreateChannel(ctx, creator.ID, "Public avatar boundary", "", false)
	if err != nil {
		t.Fatalf("create public broadcast channel: %v", err)
	}
	const username = "publicavatarboundary"
	if err := api.ClaimChannelUsernameForTest(s, channel.ID, username); err != nil {
		t.Fatalf("claim public channel username: %v", err)
	}
	body := jpegPhotoPayload(t, 640, 480)
	const uploadID = int64(228951)
	saveParts(t, s, creator.ID, uploadID, body)
	blobs := newBlobs(t)
	result, err := api.EditChannelPhotoForTest(s, creator.ID, blobs, api.TestMaxUserStorageBytes,
		&tg.ChannelsEditPhotoRequest{Channel: api.InputChannel(creator.ID, channel.ID), Photo: uploadedGroupPhoto(uploadID, body)})
	if err != nil {
		t.Fatalf("creator set public channel photo: %v", err)
	}
	_, photo := channelAvatarEditFromResult(t, result)

	resolvedResult, err := api.ResolveUsernameForTest(s, outsider.ID, &tg.ContactsResolveUsernameRequest{Username: username})
	if err != nil {
		t.Fatalf("resolve public channel for outsider: %v", err)
	}
	resolved, ok := resolvedResult.(*tg.ContactsResolvedPeer)
	if !ok {
		t.Fatalf("resolve public channel result = %T, want *tg.ContactsResolvedPeer", resolvedResult)
	}
	if len(resolved.Chats) != 1 {
		t.Fatalf("resolve public channel returned %d chats, want one", len(resolved.Chats))
	}
	publicChannel, ok := resolved.Chats[0].(*tg.Channel)
	if !ok {
		t.Fatalf("resolved public chat = %T, want *tg.Channel", resolved.Chats[0])
	}
	if publicChannel.Title != channel.Title || publicChannel.Username != username || publicChannel.ParticipantsCount == 0 || !publicChannel.Left {
		t.Fatalf("public outsider metadata = title %q username %q count %d left %v", publicChannel.Title, publicChannel.Username, publicChannel.ParticipantsCount, publicChannel.Left)
	}
	if _, ok := publicChannel.Photo.(*tg.ChatPhotoEmpty); !ok {
		t.Fatalf("resolved public outsider photo = %T, want *tg.ChatPhotoEmpty", publicChannel.Photo)
	}

	fullResult, err := api.GetFullChannelForTest(s, outsider.ID, &tg.ChannelsGetFullChannelRequest{Channel: api.InputChannel(outsider.ID, channel.ID)})
	if err != nil {
		t.Fatalf("get public full channel for outsider: %v", err)
	}
	full, ok := fullResult.(*tg.MessagesChatFull)
	if !ok {
		t.Fatalf("getFullChannel result = %T, want *tg.MessagesChatFull", fullResult)
	}
	channelFull, ok := full.FullChat.(*tg.ChannelFull)
	if !ok {
		t.Fatalf("public full channel = %T, want *tg.ChannelFull", full.FullChat)
	}
	if channelFull.ParticipantsCount == 0 {
		t.Fatal("public full channel has no participants")
	}
	if _, ok := channelFull.ChatPhoto.(*tg.PhotoEmpty); !ok {
		t.Fatalf("public outsider full photo = %T, want *tg.PhotoEmpty", channelFull.ChatPhoto)
	}
	for name, location := range map[string]tg.InputFileLocationClass{
		"photo":      &tg.InputPhotoFileLocation{ID: photo.ID, AccessHash: photo.AccessHash, ThumbSize: "x"},
		"peer photo": &tg.InputPeerPhotoFileLocation{Peer: api.InputPeerChannel(outsider.ID, channel.ID), PhotoID: photo.ID, Big: true},
	} {
		if _, err := api.GetFileForTest(s, outsider.ID, blobs, &tg.UploadGetFileRequest{Location: location, Limit: 1024}); rpcMessage(t, err) != "LOCATION_INVALID" {
			t.Fatalf("public outsider %s download = %v, want LOCATION_INVALID", name, err)
		}
	}
}

func TestChannelEditPhotoRechecksRightsAfterPausedPut(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	creator := mustUser(t, s, "+15551228941")
	member := mustUser(t, s, "+15551228942")
	channel, err := s.CreateChannel(ctx, creator.ID, "Avatar rights race", "", true)
	if err != nil {
		t.Fatalf("create supergroup: %v", err)
	}
	joinChannelByInvite(t, s, channel, member.ID)
	blobs := &blockingAssembledPut{Store: newBlobs(t), started: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(blobs.unblock)
	body := jpegPhotoPayload(t, 640, 480)
	const uploadID = int64(228941)
	saveParts(t, s, member.ID, uploadID, body)
	beforeChannel, ok, err := s.ChannelByID(ctx, channel.ID)
	if err != nil || !ok {
		t.Fatalf("load channel before upload: ok=%v err=%v", ok, err)
	}
	beforePts, err := s.ChannelState(ctx, channel.ID)
	if err != nil {
		t.Fatalf("channel state before upload: %v", err)
	}
	beforeHistory := channelAvatarHistoryCount(t, s, channel.ID)
	result := make(chan error, 1)
	go func() {
		_, err := api.EditChannelPhotoForTest(s, member.ID, blobs, api.TestMaxUserStorageBytes,
			&tg.ChannelsEditPhotoRequest{Channel: api.InputChannel(member.ID, channel.ID), Photo: uploadedGroupPhoto(uploadID, body)})
		result <- err
	}()
	select {
	case <-blobs.started:
	case <-time.After(10 * time.Second):
		t.Fatal("photo assembly never reached the paused original-blob write")
	}
	if _, _, err := s.SetChannelDefaultBannedRights(ctx, channel.ID, creator.ID, []string{"change_info"}); err != nil {
		t.Fatalf("restrict supergroup change_info during upload: %v", err)
	}
	afterRestriction, ok, err := s.ChannelByID(ctx, channel.ID)
	if err != nil || !ok {
		t.Fatalf("load channel after restriction: ok=%v err=%v", ok, err)
	}
	blobs.unblock()
	select {
	case err = <-result:
	case <-time.After(10 * time.Second):
		t.Fatal("channel photo edit stayed blocked after releasing blob write")
	}
	if rpcMessage(t, err) != "CHAT_ADMIN_REQUIRED" {
		t.Fatalf("member edit after default-right change = %v, want CHAT_ADMIN_REQUIRED", err)
	}
	after, ok, err := s.ChannelByID(ctx, channel.ID)
	if err != nil || !ok {
		t.Fatalf("load channel after refused edit: ok=%v err=%v", ok, err)
	}
	if after.CurrentPhotoFileID != nil || after.CurrentPhotoUploaderID != nil || after.Version != afterRestriction.Version || after.Version != beforeChannel.Version+1 {
		t.Fatalf("refused edit changed channel photo/version: photo=%v uploader=%v version=%d before=%d after restriction=%d",
			after.CurrentPhotoFileID, after.CurrentPhotoUploaderID, after.Version, beforeChannel.Version, afterRestriction.Version)
	}
	if pts, err := s.ChannelState(ctx, channel.ID); err != nil || pts != beforePts {
		t.Fatalf("refused edit channel pts=%d err=%v, want unchanged %d", pts, err, beforePts)
	}
	if got := channelAvatarHistoryCount(t, s, channel.ID); got != beforeHistory {
		t.Fatalf("refused edit left %d history rows, want %d", got, beforeHistory)
	}
}

func TestChannelEditPhotoServiceMessageIsRecoveredByChannelDifference(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	creator := mustUser(t, s, "+15551228921")
	member := mustUser(t, s, "+15551228922")
	channel, err := s.CreateChannel(ctx, creator.ID, "Avatar difference", "", false)
	if err != nil {
		t.Fatalf("create broadcast channel: %v", err)
	}
	joinChannelByInvite(t, s, channel, member.ID)
	body := jpegPhotoPayload(t, 640, 480)
	const uploadID = int64(228921)
	saveParts(t, s, creator.ID, uploadID, body)
	result, err := api.EditChannelPhotoForTest(s, creator.ID, newBlobs(t), api.TestMaxUserStorageBytes,
		&tg.ChannelsEditPhotoRequest{Channel: api.InputChannel(creator.ID, channel.ID), Photo: uploadedGroupPhoto(uploadID, body)})
	if err != nil {
		t.Fatalf("edit channel photo: %v", err)
	}
	service, photo := channelAvatarEditFromResult(t, result)
	diffEnc, err := api.GetChannelDifferenceForTest(s, member.ID, &tg.UpdatesGetChannelDifferenceRequest{
		Channel: api.InputChannel(member.ID, channel.ID), Filter: &tg.ChannelMessagesFilterEmpty{}, Pts: 0, Limit: 100,
	})
	if err != nil {
		t.Fatalf("get channel difference: %v", err)
	}
	diff, ok := diffEnc.(*tg.UpdatesChannelDifference)
	if !ok {
		t.Fatalf("get channel difference result = %T, want *tg.UpdatesChannelDifference", diffEnc)
	}
	found := false
	for _, message := range diff.NewMessages {
		got, ok := message.(*tg.MessageService)
		if !ok || got.ID != service.ID {
			continue
		}
		action, ok := got.Action.(*tg.MessageActionChatEditPhoto)
		if !ok {
			continue
		}
		gotPhoto, ok := action.Photo.(*tg.Photo)
		if ok && gotPhoto.ID == photo.ID && gotPhoto.AccessHash == photo.AccessHash {
			found = true
		}
	}
	if !found {
		t.Fatalf("channel difference omitted photo identity %d at local id %d: %+v", photo.ID, service.ID, diff.NewMessages)
	}
}

func TestChannelEditPhotoDeletedSelectionDoesNotRetainCurrentPointer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	creator := mustUser(t, s, "+15551228931")
	channel, err := s.CreateChannel(ctx, creator.ID, "Avatar deletion", "", true)
	if err != nil {
		t.Fatalf("create supergroup: %v", err)
	}
	photo, err := s.AllocateAndCompletePhotoFile(ctx, creator.ID, 19, "image/jpeg", "channel-photo.jpg", api.TestMaxUserStorageBytes,
		func(store.File) (store.PhotoDimensions, error) {
			return store.PhotoDimensions{Width: 640, Height: 480}, nil
		},
	)
	if err != nil {
		t.Fatalf("assemble photo: %v", err)
	}
	if _, _, _, _, err := s.SetChannelPhoto(ctx, channel.ID, creator.ID, photo.ID); err != nil {
		t.Fatalf("select channel photo: %v", err)
	}
	if _, err := s.DeleteChannel(ctx, channel.ID, creator.ID); err != nil {
		t.Fatalf("delete channel: %v", err)
	}
	deleted, ok, err := s.ChannelByID(ctx, channel.ID)
	if err != nil || !ok || deleted.DeletedAt == nil || deleted.CurrentPhotoFileID != nil || deleted.CurrentPhotoUploaderID != nil {
		t.Fatalf("deleted channel = %+v, ok=%v err=%v", deleted, ok, err)
	}
}
