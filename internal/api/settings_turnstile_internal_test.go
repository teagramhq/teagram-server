package api

import (
	"context"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/mtproto"
)

func TestGetAppConfigPublishesTurnstileOnlyWhenEnforced(t *testing.T) {
	for _, tc := range []struct {
		name        string
		verifier    SignUpTurnstileVerifier
		signIn      bool
		wantSiteKey bool
	}{
		{name: "disabled"},
		{name: "enforced", verifier: &replayTurnstileVerifier{seen: make(map[string]bool)}, wantSiteKey: true},
		{name: "signin enabled without action advertisement", verifier: &replayTurnstileVerifier{seen: make(map[string]bool)}, signIn: true, wantSiteKey: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &handlers{turnstileSiteKey: "public-site-key", turnstileVerifier: tc.verifier, turnstileSignInEnabled: tc.signIn}
			var request bin.Buffer
			if err := (&tg.HelpGetAppConfigRequest{}).Encode(&request); err != nil {
				t.Fatal(err)
			}
			got, err := h.handleGetAppConfig(&mtproto.Request{Ctx: context.Background(), Buf: &request})
			if err != nil {
				t.Fatalf("help.getAppConfig: %v", err)
			}
			response, ok := got.(*tg.HelpAppConfig)
			if !ok {
				t.Fatalf("response = %T, want *tg.HelpAppConfig", got)
			}
			object, ok := response.Config.(*tg.JSONObject)
			if !ok {
				t.Fatalf("config = %T, want *tg.JSONObject", response.Config)
			}
			var turnstile *tg.JSONObject
			for _, value := range object.Value {
				if value.Key == "turnstile" {
					turnstile, _ = value.Value.(*tg.JSONObject)
				}
			}
			if !tc.wantSiteKey {
				if turnstile != nil {
					t.Fatalf("disabled app config advertises Turnstile: %+v", turnstile)
				}
				return
			}
			if turnstile == nil || len(turnstile.Value) != 1 || turnstile.Value[0].Key != "site_key" {
				t.Fatalf("Turnstile config = %+v, want site_key", turnstile)
			}
			key, ok := turnstile.Value[0].Value.(*tg.JSONString)
			if !ok || key.Value != "public-site-key" {
				t.Fatalf("Turnstile site key = %v, want public-site-key", turnstile.Value[0].Value)
			}
		})
	}
}
