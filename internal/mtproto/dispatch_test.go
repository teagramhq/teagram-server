package mtproto_test

import (
	"errors"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/mtproto"
)

// encodeReq encodes obj into a Request buffer positioned at its constructor ID.
func encodeReq(t *testing.T, obj bin.Encoder) *mtproto.Request {
	t.Helper()
	var b bin.Buffer
	if err := obj.Encode(&b); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return &mtproto.Request{Buf: &b}
}

func TestUnpackInvokeWithAfterMsgAndReCaptchaPassesSupportedAuthTokens(t *testing.T) {
	const token = "challenge-value"
	tests := []struct {
		name  string
		query bin.Object
		id    uint32
	}{
		{name: "auth.sendCode", query: &tg.AuthSendCodeRequest{}, id: tg.AuthSendCodeRequestTypeID},
		{name: "auth.checkPassword", query: &tg.AuthCheckPasswordRequest{Password: &tg.InputCheckPasswordSRP{}}, id: tg.AuthCheckPasswordRequestTypeID},
		{name: "auth.signUp", query: &tg.AuthSignUpRequest{}, id: tg.AuthSignUpRequestTypeID},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wrapped := &tg.InvokeWithReCaptchaRequest{Token: token, Query: tc.query}
			var gotID uint32
			var gotToken string
			next := mtproto.HandlerFunc(func(_ *mtproto.Conn, req *mtproto.Request) error {
				id, err := req.Buf.PeekID()
				if err != nil {
					return err
				}
				gotID = id
				gotToken = req.ReCaptchaToken
				return nil
			})

			req := encodeReq(t, wrapped)
			err := mtproto.UnpackInvokeWithAfterMsgAndReCaptcha(next, nil).OnMessage(nil, req)
			if err != nil {
				t.Fatalf("unwrap challenge: %v", err)
			}
			if gotID != tc.id || gotToken != token {
				t.Fatalf("inner request = %#x with token %q, want %#x with original token", gotID, gotToken, tc.id)
			}
			if req.ReCaptchaToken != "" {
				t.Fatal("request retained the challenge token after dispatch")
			}
		})
	}
}

func TestUnpackInvokeWithAfterMsgAndReCaptchaAcceptsLayerEnvelope(t *testing.T) {
	challenge := func() *tg.InvokeWithReCaptchaRequest {
		return &tg.InvokeWithReCaptchaRequest{Token: "challenge-value", Query: &tg.AuthSignUpRequest{}}
	}
	initConnection := func(query bin.Object) *tg.InitConnectionRequest {
		return &tg.InitConnectionRequest{
			APIID: 1, DeviceModel: "test", SystemVersion: "1.0", AppVersion: "1.0",
			SystemLangCode: "en", LangCode: "en", Query: query,
		}
	}
	tests := []struct {
		name  string
		query bin.Object
	}{
		{name: "invokeWithLayer", query: &tg.InvokeWithLayerRequest{Layer: 100, Query: challenge()}},
		{name: "initConnection", query: initConnection(challenge())},
		{name: "invokeWithoutUpdates", query: &tg.InvokeWithoutUpdatesRequest{Query: challenge()}},
		{name: "all supported outer envelopes", query: &tg.InvokeWithLayerRequest{Layer: 100, Query: initConnection(&tg.InvokeWithoutUpdatesRequest{Query: challenge()})}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotID uint32
			var gotToken string
			next := mtproto.HandlerFunc(func(_ *mtproto.Conn, req *mtproto.Request) error {
				id, err := req.Buf.PeekID()
				if err != nil {
					return err
				}
				gotID = id
				gotToken = req.ReCaptchaToken
				return nil
			})
			req := encodeReq(t, tc.query)
			if err := mtproto.UnpackInvokeWithAfterMsgAndReCaptcha(next, nil).OnMessage(nil, req); err != nil {
				t.Fatalf("unwrap outer envelope and challenge: %v", err)
			}
			if gotID != tg.AuthSignUpRequestTypeID || gotToken != "challenge-value" {
				t.Fatalf("inner request = %#x with token %q, want auth.signUp with original token", gotID, gotToken)
			}
			if req.ReCaptchaToken != "" {
				t.Fatal("request retained the challenge token after dispatch")
			}
		})
	}
}

func TestUnpackInvokeWithAfterMsgRefusesOtherOrNestedReCaptcha(t *testing.T) {
	tests := []struct {
		name  string
		query bin.Object
	}{
		{name: "other method", query: &tg.AuthSignInRequest{}},
		{name: "nested challenge", query: &tg.InvokeWithReCaptchaRequest{Token: "inner", Query: &tg.AuthSignUpRequest{}}},
		{name: "layer inside challenge", query: &tg.InvokeWithLayerRequest{Layer: 100, Query: &tg.AuthSignUpRequest{}}},
		{name: "initConnection inside challenge", query: &tg.InitConnectionRequest{Query: &tg.AuthSignUpRequest{}}},
		{name: "invokeWithoutUpdates inside challenge", query: &tg.InvokeWithoutUpdatesRequest{Query: &tg.AuthSignUpRequest{}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var dispatched bool
			next := mtproto.HandlerFunc(func(_ *mtproto.Conn, _ *mtproto.Request) error {
				dispatched = true
				return nil
			})
			var refusedID uint32
			onRefusal := func(_ *mtproto.Conn, _ *mtproto.Request, id uint32, _ mtproto.InvokeAfterMsgRefusal, _ mtproto.UnimplementedVerdict) error {
				refusedID = id
				return nil
			}
			wrapped := &tg.InvokeWithReCaptchaRequest{Token: "outer", Query: tc.query}
			if err := mtproto.UnpackInvokeWithAfterMsgAndReCaptcha(next, onRefusal).OnMessage(nil, encodeReq(t, wrapped)); err != nil {
				t.Fatalf("refuse unsupported challenge: %v", err)
			}
			if dispatched || refusedID == 0 {
				t.Fatalf("dispatched=%v refused id=%#x, want refusal without dispatch", dispatched, refusedID)
			}
		})
	}
}

func TestUnpackInvokeWithAfterMsgDoesNotShareChallengeAcrossContainerSiblings(t *testing.T) {
	var seen []string
	next := mtproto.HandlerFunc(func(_ *mtproto.Conn, req *mtproto.Request) error {
		seen = append(seen, req.ReCaptchaToken)
		return nil
	})
	handler := mtproto.UnpackInvokeWithAfterMsgAndReCaptcha(next, nil)
	first := encodeReq(t, &tg.InvokeWithReCaptchaRequest{Token: "first-sibling-token", Query: &tg.AuthSendCodeRequest{}})
	second := encodeReq(t, &tg.AuthSendCodeRequest{})
	if err := handler.OnMessage(nil, first); err != nil {
		t.Fatalf("dispatch challenged first sibling: %v", err)
	}
	if err := handler.OnMessage(nil, second); err != nil {
		t.Fatalf("dispatch bare second sibling: %v", err)
	}
	if len(seen) != 2 || seen[0] != "first-sibling-token" || seen[1] != "" {
		t.Fatalf("sibling challenge tokens = %q, want [first-sibling-token, empty]", seen)
	}
	if first.ReCaptchaToken != "" {
		t.Fatal("first sibling retained its challenge token after dispatch")
	}
}

func TestUnpackInvokeWithAfterMsgClearsChallengeAfterHandlerError(t *testing.T) {
	wantErr := errors.New("handler rejected request")
	next := mtproto.HandlerFunc(func(_ *mtproto.Conn, req *mtproto.Request) error {
		if req.ReCaptchaToken != "sentinel-token" {
			t.Errorf("handler token = %q, want sentinel-token", req.ReCaptchaToken)
		}
		return wantErr
	})
	req := encodeReq(t, &tg.InvokeWithReCaptchaRequest{Token: "sentinel-token", Query: &tg.AuthSendCodeRequest{}})
	if err := mtproto.UnpackInvokeWithAfterMsgAndReCaptcha(next, nil).OnMessage(nil, req); !errors.Is(err, wantErr) {
		t.Fatalf("dispatch error = %v, want handler error", err)
	}
	if req.ReCaptchaToken != "" {
		t.Fatal("request retained the challenge token after handler error")
	}
}

func TestUnpackInvokeWithAfterMsgKeepsReCaptchaDisabledByDefault(t *testing.T) {
	var dispatched, refused bool
	next := mtproto.HandlerFunc(func(_ *mtproto.Conn, _ *mtproto.Request) error {
		dispatched = true
		return nil
	})
	onRefusal := func(_ *mtproto.Conn, _ *mtproto.Request, _ uint32, _ mtproto.InvokeAfterMsgRefusal, _ mtproto.UnimplementedVerdict) error {
		refused = true
		return nil
	}
	wrapped := &tg.InvokeWithReCaptchaRequest{Token: "token", Query: &tg.AuthSignUpRequest{}}
	if err := mtproto.UnpackInvokeWithAfterMsg(next, onRefusal).OnMessage(nil, encodeReq(t, wrapped)); err != nil {
		t.Fatalf("refuse disabled challenge: %v", err)
	}
	if dispatched || !refused {
		t.Fatalf("dispatched=%v refused=%v, want existing refusal behavior", dispatched, refused)
	}
}

func TestDispatcherUnknownIDUsesFallback(t *testing.T) {
	d := mtproto.NewDispatcher()

	var handledConfig, handledFallback bool
	d.HandleFunc(tg.HelpGetConfigRequestTypeID, func(_ *mtproto.Conn, _ *mtproto.Request) error {
		handledConfig = true
		return nil
	})
	d.Fallback(mtproto.HandlerFunc(func(_ *mtproto.Conn, _ *mtproto.Request) error {
		handledFallback = true
		return nil
	}))

	// Registered ID goes to its handler.
	if err := d.OnMessage(nil, encodeReq(t, &tg.HelpGetConfigRequest{})); err != nil {
		t.Fatalf("OnMessage config: %v", err)
	}
	if !handledConfig || handledFallback {
		t.Fatalf("registered id: config=%v fallback=%v", handledConfig, handledFallback)
	}

	// Unregistered ID goes to the fallback.
	handledConfig, handledFallback = false, false
	if err := d.OnMessage(nil, encodeReq(t, &tg.UsersGetUsersRequest{})); err != nil {
		t.Fatalf("OnMessage users: %v", err)
	}
	if handledConfig || !handledFallback {
		t.Fatalf("unregistered id: config=%v fallback=%v", handledConfig, handledFallback)
	}
}

func TestDispatcherUnknownIDNoFallbackErrors(t *testing.T) {
	d := mtproto.NewDispatcher()
	if err := d.OnMessage(nil, encodeReq(t, &tg.HelpGetConfigRequest{})); err == nil {
		t.Fatal("expected error for unregistered id without fallback")
	}
}

func TestUnpackInvokePeelsWrappers(t *testing.T) {
	// invokeWithLayer -> initConnection -> invokeWithoutUpdates -> help.getConfig
	wrapped := &tg.InvokeWithLayerRequest{
		Layer: 100,
		Query: &tg.InitConnectionRequest{
			APIID:          1,
			DeviceModel:    "test",
			SystemVersion:  "1.0",
			AppVersion:     "1.0",
			SystemLangCode: "en",
			LangPack:       "",
			LangCode:       "en",
			Query: &tg.InvokeWithoutUpdatesRequest{
				Query: &tg.HelpGetConfigRequest{},
			},
		},
	}

	var gotID uint32
	next := mtproto.HandlerFunc(func(_ *mtproto.Conn, req *mtproto.Request) error {
		id, err := req.Buf.PeekID()
		if err != nil {
			return err
		}
		gotID = id
		return nil
	})

	if err := mtproto.UnpackInvoke(next).OnMessage(nil, encodeReq(t, wrapped)); err != nil {
		t.Fatalf("UnpackInvoke: %v", err)
	}
	if gotID != tg.HelpGetConfigRequestTypeID {
		t.Fatalf("peeled id = %#x, want %#x", gotID, tg.HelpGetConfigRequestTypeID)
	}
}

func TestUnpackInvokePassesThroughPlainRequest(t *testing.T) {
	var gotID uint32
	next := mtproto.HandlerFunc(func(_ *mtproto.Conn, req *mtproto.Request) error {
		id, err := req.Buf.PeekID()
		if err != nil {
			return err
		}
		gotID = id
		return nil
	})

	if err := mtproto.UnpackInvoke(next).OnMessage(nil, encodeReq(t, &tg.HelpGetConfigRequest{})); err != nil {
		t.Fatalf("UnpackInvoke: %v", err)
	}
	if gotID != tg.HelpGetConfigRequestTypeID {
		t.Fatalf("id = %#x, want %#x", gotID, tg.HelpGetConfigRequestTypeID)
	}
}
