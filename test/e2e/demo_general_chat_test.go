package e2e_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/gotd/td/session"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/config"
)

func TestDemoGeneralChatSignupAutoJoinSmoke(t *testing.T) {
	f := newSmokeFixtureWithSetup(t, config.RegistrationOpen, func(f *smokeFixture) {
		f.demoGeneralChatEnabled = true
	})
	owner, err := f.store.CreateUser(f.ctx, "+15550007001")
	if err != nil {
		t.Fatalf("create General owner: %v", err)
	}
	channel, _, _, err := f.store.CreateChannelWithServiceMessage(f.ctx, owner.ID, "General", "", true)
	if err != nil {
		t.Fatalf("create General channel: %v", err)
	}
	if err := f.store.ClaimChannelUsername(f.ctx, channel.ID, "teagramdemogeneral"); err != nil {
		t.Fatalf("claim General username: %v", err)
	}

	const username = "general_smoke_guest"
	client := newUsernameClient(f.port, f.key, f.dcID, &session.StorageMemory{})
	if err := client.Run(f.ctx, func(ctx context.Context) error {
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
		if _, err := smokeChannelDialog(ctx, api, channel.ID); err != nil {
			return fmt.Errorf("General dialog after signup: %w", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("General signup smoke: %v", err)
	}
}
