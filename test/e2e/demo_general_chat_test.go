package e2e_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gotd/td/session"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/demo"
	"github.com/teagramhq/teagram-server/internal/mtproto"
)

func testSmokeDemoGeneralChat(t *testing.T) {
	t.Helper()
	f := newSmokeFixtureWithSetup(t, config.RegistrationOpen, func(f *smokeFixture) {
		f.demoGeneralChatEnabled = true
	})
	owner, err := f.store.CreateUser(f.ctx, "+15550007001")
	if err != nil {
		t.Fatalf("create General owner: %v", err)
	}
	if err := f.store.ClaimUsername(f.ctx, owner.ID, demo.SeedOwnerUsername); err != nil {
		t.Fatalf("claim General seed owner username: %v", err)
	}
	channel, _, _, err := f.store.CreateChannelWithServiceMessage(f.ctx, owner.ID, "General", "", true)
	if err != nil {
		t.Fatalf("create General channel: %v", err)
	}
	if err := f.store.ClaimChannelUsername(f.ctx, channel.ID, "teagramdemogeneral"); err != nil {
		t.Fatalf("claim General username: %v", err)
	}
	if _, changed, err := f.store.SetChannelSlowMode(f.ctx, channel.ID, owner.ID, 10); err != nil || !changed {
		t.Fatalf("set General slow mode changed=%v err=%v", changed, err)
	}

	signUpGuest := func(username string, afterSignUp func(context.Context, *tg.Client, int64) error) error {
		sessionStorage := &session.StorageMemory{}
		client := newUsernameClient(f.port, f.key, f.dcID, sessionStorage)
		return client.Run(f.ctx, func(ctx context.Context) error {
			api := client.API()
			codeHash, err := sendCodeUsername(ctx, api, username)
			if err != nil {
				return fmt.Errorf("send signup code: %w", err)
			}
			code, err := f.codes.wait(ctx, username)
			if err != nil {
				return fmt.Errorf("wait for signup code: %w", err)
			}
			response, err := signInUsername(ctx, api, username, codeHash, code)
			if err != nil && !isSignUpRequired(err) {
				return fmt.Errorf("sign in before signup: %w", err)
			}
			if err == nil {
				if _, ok := response.(*tg.AuthAuthorizationSignUpRequired); !ok {
					return fmt.Errorf("sign in response = %T, want signup required", response)
				}
			}
			response, err = signUpUsername(ctx, api, username, codeHash, "General", "Guest")
			if err != nil {
				return fmt.Errorf("sign up guest: %w", err)
			}
			authorization, ok := response.(*tg.AuthAuthorization)
			if !ok {
				return fmt.Errorf("sign up response = %T, want authorization", response)
			}
			user, ok := authorization.User.(*tg.User)
			if !ok || user.ID <= 0 {
				return fmt.Errorf("sign up user = %T, want persisted user", authorization.User)
			}
			member, found, err := f.store.ChannelMemberOf(ctx, channel.ID, user.ID)
			if err != nil {
				return fmt.Errorf("read new guest General membership: %w", err)
			}
			if !found || member.Role != 0 {
				return fmt.Errorf("new guest General membership = %+v found=%v, want ordinary member", member, found)
			}
			sessionData, err := loadSmokeSession(ctx, sessionStorage)
			if err != nil {
				return fmt.Errorf("load guest session after signup: %w", err)
			}
			if len(sessionData.AuthKeyID) != 8 {
				return fmt.Errorf("guest auth key ID length = %d, want 8", len(sessionData.AuthKeyID))
			}
			var authKeyID [8]byte
			copy(authKeyID[:], sessionData.AuthKeyID)
			passwordState, err := api.AccountGetPassword(ctx)
			if err != nil {
				return fmt.Errorf("read password setup after signup: %w", err)
			}
			if passwordState.HasPassword {
				return errors.New("new guest unexpectedly already has a password")
			}
			verifier, salt1, salt2, err := testComputeSRPVerifier([]byte("demo-general-smoke-password"))
			if err != nil {
				return fmt.Errorf("prepare guest password: %w", err)
			}
			_, err = api.AccountUpdatePasswordSettings(ctx, &tg.AccountUpdatePasswordSettingsRequest{
				Password: &tg.InputCheckPasswordEmpty{},
				NewSettings: tg.AccountPasswordInputSettings{
					NewAlgo: &tg.PasswordKdfAlgoSHA256SHA256PBKDF2HMACSHA512iter100000SHA256ModPow{
						Salt1: salt1,
						Salt2: salt2,
					},
					NewPasswordHash: verifier,
				},
			})
			if err != nil {
				return fmt.Errorf("set guest password after signup: %w", err)
			}
			storedKey, found, err := f.store.AuthKeyByID(ctx, mtproto.AuthKeyIDInt64(authKeyID))
			if err != nil {
				return fmt.Errorf("read guest auth key after signup: %w", err)
			}
			if !found || storedKey.UserID != user.ID || storedKey.Provisional {
				return fmt.Errorf("guest auth key = {user:%d provisional:%v} found=%v, want bound and non-provisional user %d", storedKey.UserID, storedKey.Provisional, found, user.ID)
			}
			if _, err := smokeChannelDialog(ctx, api, channel.ID); err != nil {
				return fmt.Errorf("General dialog after signup: %w", err)
			}
			return afterSignUp(ctx, api, user.ID)
		})
	}

	const postedText = "hello from the first General guest"
	err = signUpGuest("general_smoke_guest", func(ctx context.Context, api *tg.Client, userID int64) error {
		result, err := api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: peerChannel(userID, channel.ID), Message: postedText, RandomID: 7707001,
		})
		if err != nil {
			return fmt.Errorf("send General post: %w", err)
		}
		updates, ok := result.(*tg.Updates)
		if !ok {
			return fmt.Errorf("General send result = %T, want *tg.Updates", result)
		}
		var foundPost bool
		for _, update := range updates.Updates {
			newPost, ok := update.(*tg.UpdateNewChannelMessage)
			if !ok {
				continue
			}
			message, ok := newPost.Message.(*tg.Message)
			if !ok {
				return fmt.Errorf("General send message = %T, want *tg.Message", newPost.Message)
			}
			if message.Message != postedText {
				return fmt.Errorf("General send message = %q, want %q", message.Message, postedText)
			}
			foundPost = true
		}
		if !foundPost {
			return fmt.Errorf("General send result omitted posted text %q", postedText)
		}

		_, err = api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: peerChannel(userID, channel.ID), Message: "second send during slow mode", RandomID: 7707002,
		})
		var rpcError *tgerr.Error
		if err == nil {
			return errors.New("second General send during slow mode succeeded, want SLOWMODE_WAIT")
		}
		if !errors.As(err, &rpcError) || !strings.HasPrefix(rpcError.Message, "SLOWMODE_WAIT_") {
			return fmt.Errorf("second General send during slow mode = %w, want SLOWMODE_WAIT", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("first General guest smoke: %v", err)
	}

	err = signUpGuest("general_smoke_guest2", func(ctx context.Context, api *tg.Client, userID int64) error {
		result, err := api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
			Peer: peerChannel(userID, channel.ID), Limit: 20,
		})
		if err != nil {
			return fmt.Errorf("read General history as second guest: %w", err)
		}
		history, ok := result.(*tg.MessagesChannelMessages)
		if !ok {
			return fmt.Errorf("General history result = %T, want *tg.MessagesChannelMessages", result)
		}
		matches := 0
		for _, class := range history.Messages {
			message, ok := class.(*tg.Message)
			if ok && message.Message == postedText {
				matches++
			}
		}
		if matches != 1 {
			return fmt.Errorf("second General guest history contains posted text %d times, want once", matches)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("second General guest smoke: %v", err)
	}
}

func TestDemoGeneralModerationSmokeSignupSetsPasswordBeforeProfileLookup(t *testing.T) {
	t.Parallel()
	f := newSmokeFixtureWithRegistration(t, config.RegistrationOpen)
	client := newUsernameClient(f.port, f.key, f.dcID, &session.StorageMemory{})
	const username = "ab"
	const password = "demo-moderation-smoke-password"

	if err := client.Run(f.ctx, func(ctx context.Context) error {
		apiClient := client.API()
		codeHash, err := sendCodeUsername(ctx, apiClient, username)
		if err != nil {
			return fmt.Errorf("send smoke signup code: %w", err)
		}
		code, err := f.codes.wait(ctx, username)
		if err != nil {
			return fmt.Errorf("wait for smoke signup code: %w", err)
		}
		response, err := signInUsername(ctx, apiClient, username, codeHash, code)
		if err != nil && !isSignUpRequired(err) {
			return fmt.Errorf("sign in before smoke signup: %w", err)
		}
		if err == nil {
			if _, ok := response.(*tg.AuthAuthorizationSignUpRequired); !ok {
				return fmt.Errorf("sign in response = %T, want signup required", response)
			}
		}

		response, err = signUpUsername(ctx, apiClient, username, codeHash, "Moderation", "Smoke")
		if err != nil {
			return fmt.Errorf("sign up smoke guest: %w", err)
		}
		authorization, ok := response.(*tg.AuthAuthorization)
		if !ok {
			return fmt.Errorf("signup response = %T, want authorization", response)
		}
		user, ok := authorization.User.(*tg.User)
		if !ok || user.ID <= 0 {
			return fmt.Errorf("signup user = %T, want persisted user", authorization.User)
		}

		if _, err := apiClient.UsersGetFullUser(ctx, &tg.InputUserSelf{}); err == nil {
			return errors.New("profile lookup before password setup succeeded, want AUTH_KEY_UNREGISTERED")
		} else if !tgerr.Is(err, "AUTH_KEY_UNREGISTERED") {
			return fmt.Errorf("profile lookup before password setup = %w, want AUTH_KEY_UNREGISTERED", err)
		}
		passwordState, err := apiClient.AccountGetPassword(ctx)
		if err != nil {
			return fmt.Errorf("read smoke guest password settings: %w", err)
		}
		if passwordState.HasPassword {
			return errors.New("new smoke guest unexpectedly has a password")
		}
		verifier, salt1, salt2, err := testComputeSRPVerifier([]byte(password))
		if err != nil {
			return fmt.Errorf("prepare smoke guest password: %w", err)
		}
		_, err = apiClient.AccountUpdatePasswordSettings(ctx, &tg.AccountUpdatePasswordSettingsRequest{
			Password: &tg.InputCheckPasswordEmpty{},
			NewSettings: tg.AccountPasswordInputSettings{
				NewAlgo: &tg.PasswordKdfAlgoSHA256SHA256PBKDF2HMACSHA512iter100000SHA256ModPow{
					Salt1: salt1,
					Salt2: salt2,
				},
				NewPasswordHash: verifier,
			},
		})
		if err != nil {
			return fmt.Errorf("set smoke guest password: %w", err)
		}

		full, err := apiClient.UsersGetFullUser(ctx, &tg.InputUserSelf{})
		if err != nil {
			return fmt.Errorf("inspect smoke guest after password setup: %w", err)
		}
		if len(full.Users) != 1 {
			return fmt.Errorf("smoke guest profile count = %d, want 1", len(full.Users))
		}
		profile, ok := full.Users[0].(*tg.User)
		if !ok || profile.ID != user.ID || profile.Bot || !strings.EqualFold(profile.Username, username) {
			return fmt.Errorf("smoke guest profile = %#v, want ordinary user %d with username %q", full.Users[0], user.ID, username)
		}
		return nil
	}); err != nil {
		t.Fatalf("real MTProto moderation smoke signup: %v", err)
	}
}
