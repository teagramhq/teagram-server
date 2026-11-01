package e2e_test

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestUploadsDisabledEndToEnd(t *testing.T) {
	t.Parallel()

	t.Run("disabled RPCs return the same error", func(t *testing.T) {
		f := newSmokeFixtureWithSetup(t, config.RegistrationOpen, func(f *smokeFixture) {
			f.uploadsDisabled = true
		})
		const phoneA, phoneB = "+15551029241", "+15551029242"
		seedSmokeUsers(t, f, phoneA, phoneB)
		a, b := newSmokeClient(t, f, "uploads-disabled sender", phoneA), newSmokeClient(t, f, "uploads-disabled peer", phoneB)

		refused := []struct {
			name string
			run  func() error
		}{
			{name: "upload.saveFilePart", run: func() error {
				return a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
					_, err := client.UploadSaveFilePart(ctx, &tg.UploadSaveFilePartRequest{FileID: 292401, FilePart: 0, Bytes: []byte("part")})
					return err
				})
			}},
			{name: "upload.saveBigFilePart", run: func() error {
				return a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
					_, err := client.UploadSaveBigFilePart(ctx, &tg.UploadSaveBigFilePartRequest{FileID: 292402, FilePart: 0, FileTotalParts: 1, Bytes: []byte("part")})
					return err
				})
			}},
			{name: "photos.uploadProfilePhoto", run: func() error {
				return a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
					request := &tg.PhotosUploadProfilePhotoRequest{}
					request.SetFile(&tg.InputFile{ID: 292403, Parts: 1, Name: "profile.jpg", MD5Checksum: "00000000000000000000000000000000"})
					_, err := client.PhotosUploadProfilePhoto(ctx, request)
					return err
				})
			}},
			{name: "messages.uploadMedia", run: func() error {
				return a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
					_, err := client.MessagesUploadMedia(ctx, &tg.MessagesUploadMediaRequest{
						Peer: peerUser(a.id, b.id),
						Media: &tg.InputMediaUploadedDocument{
							File: &tg.InputFile{ID: 292404, Parts: 1, Name: "blocked.txt"}, MimeType: "text/plain",
						},
					})
					return err
				})
			}},
			{name: "messages.sendMedia", run: func() error {
				return a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
					_, err := client.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
						Peer: peerUser(a.id, b.id), Message: "blocked upload", RandomID: 292405,
						Media: &tg.InputMediaUploadedDocument{
							File: &tg.InputFile{ID: 292405, Parts: 1, Name: "blocked.txt"}, MimeType: "text/plain",
						},
					})
					return err
				})
			}},
			{name: "messages.editChatPhoto", run: func() error {
				return a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
					_, err := client.MessagesEditChatPhoto(ctx, &tg.MessagesEditChatPhotoRequest{
						ChatID: 1, Photo: uploadedSmokeGroupPhoto(292406, 1, []byte("not assembled")),
					})
					return err
				})
			}},
			{name: "channels.editPhoto", run: func() error {
				return a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
					_, err := client.ChannelsEditPhoto(ctx, &tg.ChannelsEditPhotoRequest{
						Channel: inputChannel(a.id, 1), Photo: uploadedSmokeGroupPhoto(292407, 1, []byte("not assembled")),
					})
					return err
				})
			}},
		}
		for _, test := range refused {
			t.Run(test.name, func(t *testing.T) {
				assertRPCError(t, test.run(), "UPLOADS_DISABLED")
			})
		}
		for _, fileID := range []int64{292401, 292402, 292403, 292404, 292405, 292406, 292407} {
			parts, _, _, err := f.store.UploadPartsSummary(f.ctx, a.id, fileID)
			if err != nil {
				t.Fatalf("read upload part count for %d: %v", fileID, err)
			}
			if parts != 0 {
				t.Fatalf("disabled upload %d stored %d parts", fileID, parts)
			}
		}

		if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
			_, err := client.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
				Peer: peerUser(a.id, b.id), Message: "text still works", RandomID: time.Now().UnixNano(),
			})
			return err
		}); err != nil {
			t.Fatalf("messages.sendMessage with uploads disabled: %v", err)
		}

		guest := newUsernameClient(f.port, f.key, f.dcID, nil)
		if err := guest.Run(f.ctx, func(ctx context.Context) error {
			apiClient := guest.API()
			const username = "uploads_disabled_guest"
			hash, err := sendCodeUsername(ctx, apiClient, username)
			if err != nil {
				return fmt.Errorf("auth.sendCode: %w", err)
			}
			response, err := signInUsername(ctx, apiClient, username, hash, "")
			if err != nil {
				if !isSignUpRequired(err) {
					return fmt.Errorf("auth.signIn: expected SIGN_UP_REQUIRED, got %w", err)
				}
			} else if _, ok := response.(*tg.AuthAuthorizationSignUpRequired); !ok {
				return fmt.Errorf("auth.signIn response = %T, want AuthAuthorizationSignUpRequired", response)
			}
			response, err = signUpUsername(ctx, apiClient, username, hash, "Guest", "")
			if err != nil {
				return fmt.Errorf("auth.signUp with uploads disabled: %w", err)
			}
			if authz, ok := response.(*tg.AuthAuthorization); !ok || authz.User == nil {
				return fmt.Errorf("auth.signUp response = %T, want authorization with a user", response)
			}
			return nil
		}); err != nil {
			t.Fatalf("guest registration with uploads disabled: %v", err)
		}

		const existingMediaBody = "already stored media remains readable"
		existingFile, err := f.store.AllocateAndCompleteFile(
			f.ctx, a.id, int64(len(existingMediaBody)), "text/plain", "existing.txt", 2<<30, nil,
			func(file store.File) error {
				_, err := f.blobs.Put(f.ctx, blob.Key(file.ID), bytes.NewReader([]byte(existingMediaBody)))
				return err
			},
		)
		if err != nil {
			t.Fatalf("seed previously stored media: %v", err)
		}
		if _, _, _, _, err := f.store.SendMessage(f.ctx, a.id, b.id, "existing media", 292408, existingFile.ID, 0); err != nil {
			t.Fatalf("seed existing media message: %v", err)
		}
		if err := b.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
			history, err := client.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: peerUser(b.id, a.id), Limit: 20})
			if err != nil {
				return err
			}
			messages, ok := history.(*tg.MessagesMessages)
			if !ok {
				return fmt.Errorf("messages.getHistory = %T, want *tg.MessagesMessages", history)
			}
			for _, message := range messages.Messages {
				item, ok := message.(*tg.Message)
				if !ok {
					continue
				}
				media, ok := item.Media.(*tg.MessageMediaDocument)
				if !ok {
					continue
				}
				document, ok := media.Document.(*tg.Document)
				if !ok || document.ID != existingFile.ID {
					continue
				}
				result, err := client.UploadGetFile(ctx, &tg.UploadGetFileRequest{
					Location: &tg.InputDocumentFileLocation{ID: document.ID, AccessHash: document.AccessHash, FileReference: document.FileReference},
					Offset:   0, Limit: len(existingMediaBody),
				})
				if err != nil {
					return err
				}
				file, ok := result.(*tg.UploadFile)
				if !ok || !bytes.Equal(file.Bytes, []byte(existingMediaBody)) {
					return fmt.Errorf("upload.getFile = %#v, want stored document bytes", result)
				}
				return nil
			}
			return fmt.Errorf("messages.getHistory omitted stored file %d", existingFile.ID)
		}); err != nil {
			t.Fatalf("read previously stored media with uploads disabled: %v", err)
		}
	})

	t.Run("uploads remain available by default", func(t *testing.T) {
		f := newSmokeFixtureWithSetup(t, config.RegistrationClosed, func(f *smokeFixture) {
			f.photoThumbs = buildSmokePhotoThumbSupervisor(t)
		})
		const phoneA, phoneB = "+15551029243", "+15551029244"
		seedSmokeUsers(t, f, phoneA, phoneB)
		a, b := newSmokeClient(t, f, "uploads-enabled sender", phoneA), newSmokeClient(t, f, "uploads-enabled peer", phoneB)

		if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
			_, err := client.UploadSaveFilePart(ctx, &tg.UploadSaveFilePartRequest{FileID: 292410, FilePart: 0, Bytes: []byte("part")})
			return err
		}); err != nil {
			t.Fatalf("upload.saveFilePart with uploads enabled: %v", err)
		}
		if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
			_, err := client.UploadSaveBigFilePart(ctx, &tg.UploadSaveBigFilePartRequest{FileID: 292411, FilePart: 0, FileTotalParts: 1, Bytes: []byte("part")})
			return err
		}); err != nil {
			t.Fatalf("upload.saveBigFilePart with uploads enabled: %v", err)
		}

		documentBody := []byte("uploadMedia enabled document")
		const documentFileID = int64(292412)
		documentParts := uploadSmokeMediaParts(t, f.ctx, a, documentFileID, documentBody, len(documentBody))
		if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
			_, err := client.MessagesUploadMedia(ctx, &tg.MessagesUploadMediaRequest{
				Peer: peerUser(a.id, b.id),
				Media: &tg.InputMediaUploadedDocument{
					File: &tg.InputFile{ID: documentFileID, Parts: documentParts, Name: "enabled.txt"}, MimeType: "text/plain",
				},
			})
			return err
		}); err != nil {
			t.Fatalf("messages.uploadMedia with uploads enabled: %v", err)
		}

		photoBody := smokeJPEGAtUploadSize(t, smokePhotoPartCount)
		const profileFileID = int64(292413)
		profileParts := uploadSmokeMediaParts(t, f.ctx, a, profileFileID, photoBody, smokePhotoPartSize)
		if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
			request := &tg.PhotosUploadProfilePhotoRequest{}
			request.SetFile(&tg.InputFile{ID: profileFileID, Parts: profileParts, Name: "enabled.jpg", MD5Checksum: smokePhotoMD5(photoBody)})
			_, err := client.PhotosUploadProfilePhoto(ctx, request)
			return err
		}); err != nil {
			t.Fatalf("photos.uploadProfilePhoto with uploads enabled: %v", err)
		}

		var chatID int64
		if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
			created, err := client.MessagesCreateChat(ctx, &tg.MessagesCreateChatRequest{
				Title: "uploads enabled", Users: []tg.InputUserClass{inputUser(a.id, b.id)},
			})
			if err != nil {
				return err
			}
			updates, ok := created.Updates.(*tg.Updates)
			if !ok || len(updates.Chats) != 1 {
				return fmt.Errorf("createChat result = %T with %d chats", created.Updates, len(updates.Chats))
			}
			chat, ok := updates.Chats[0].(*tg.Chat)
			if !ok {
				return fmt.Errorf("created chat = %T, want *tg.Chat", updates.Chats[0])
			}
			chatID = chat.ID
			return nil
		}); err != nil {
			t.Fatalf("create chat with uploads enabled: %v", err)
		}
		_ = uploadSmokeGroupPhoto(t, a, chatID, 292414, photoBody)

		channelID := createBroadcastChannel(t, f.ctx, a.cmds, "uploads enabled")
		if err := uploadChannelAvatar(f, a, channelID, 292415, photoBody); err != nil {
			t.Fatalf("channels.editPhoto with uploads enabled: %v", err)
		}

		const sendFileID = int64(292416)
		sendParts := uploadSmokeMediaParts(t, f.ctx, a, sendFileID, photoBody, smokePhotoPartSize)
		if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
			_, err := client.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
				Peer: peerUser(a.id, b.id), Message: "enabled photo", RandomID: time.Now().UnixNano(),
				Media: &tg.InputMediaUploadedPhoto{File: &tg.InputFile{
					ID: sendFileID, Parts: sendParts, Name: "enabled.jpg", MD5Checksum: smokePhotoMD5(photoBody),
				}},
			})
			return err
		}); err != nil {
			t.Fatalf("messages.sendMedia with uploads enabled: %v", err)
		}

		for _, fileID := range []int64{292410, 292411, documentFileID, profileFileID, 292414, 292415, sendFileID} {
			parts, _, _, err := f.store.UploadPartsSummary(f.ctx, a.id, fileID)
			if err != nil {
				t.Fatalf("read upload part count for %d: %v", fileID, err)
			}
			want := int64(0)
			if fileID == 292410 || fileID == 292411 {
				want = 1
			}
			if parts != want {
				t.Fatalf("accepted upload %d retained %d parts, want %d", fileID, parts, want)
			}
		}
	})
}

func testSmokeUploadsDisabled(t *testing.T) {
	t.Helper()
	f := newSmokeFixtureWithSetup(t, config.RegistrationClosed, func(f *smokeFixture) {
		f.uploadsDisabled = true
	})
	const phoneA, phoneB = "+15551029245", "+15551029246"
	seedSmokeUsers(t, f, phoneA, phoneB)
	a, b := newSmokeClient(t, f, "uploads-disabled smoke sender", phoneA), newSmokeClient(t, f, "uploads-disabled smoke peer", phoneB)

	err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		_, err := client.UploadSaveFilePart(ctx, &tg.UploadSaveFilePartRequest{FileID: 292417, FilePart: 0, Bytes: []byte("part")})
		return err
	})
	assertRPCError(t, err, "UPLOADS_DISABLED")

	if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		_, err := client.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: peerUser(a.id, b.id), Message: "text still works", RandomID: time.Now().UnixNano(),
		})
		return err
	}); err != nil {
		t.Fatalf("messages.sendMessage with uploads disabled: %v", err)
	}
}
