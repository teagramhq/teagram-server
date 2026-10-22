package api_test

import (
	"bytes"
	"context"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestRecoveryEmailOffDiscardsAndNeverAdvertises(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	user, err := s.CreateUser(ctx, "+15551297551")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	candidate := newFirstPasswordCandidate(t, "recovery", "recovery password", 0x91)
	var updateBuf bin.Buffer
	if err := firstPasswordRPCRequest(candidate).Encode(&updateBuf); err != nil {
		t.Fatalf("encode first password: %v", err)
	}
	if _, err := api.UpdatePasswordSettingsWithProofLimits(s, user.ID, [8]byte{0x91}, store.RateLimitConfig{}, &updateBuf); err != nil {
		t.Fatalf("set password with recovery email: %v", err)
	}
	stored, ok, err := s.PasswordByUser(ctx, user.ID)
	if err != nil || !ok {
		t.Fatalf("load password: ok=%t err=%v", ok, err)
	}
	if stored.RecoveryEmail != "" || stored.HasRecovery {
		t.Fatalf("off mode stored recovery settings: email=%q has_recovery=%t", stored.RecoveryEmail, stored.HasRecovery)
	}

	var authKeyID [8]byte
	authKeyID[7] = 0x91
	if err := s.SaveAuthKey(ctx, mtproto.AuthKeyIDInt64(authKeyID), make([]byte, 256)); err != nil {
		t.Fatalf("save auth key: %v", err)
	}
	if err := s.BindAuthKeyUser(ctx, mtproto.AuthKeyIDInt64(authKeyID), user.ID); err != nil {
		t.Fatalf("bind auth key: %v", err)
	}
	h := api.SharedHandlersForTest(s)
	var getPasswordBuf bin.Buffer
	if err := (&tg.AccountGetPasswordRequest{}).Encode(&getPasswordBuf); err != nil {
		t.Fatalf("encode getPassword: %v", err)
	}
	passwordResult, err := api.HandleGetPassword(h, &mtproto.Request{
		Ctx: ctx, UserID: user.ID, AuthKeyID: authKeyID, Buf: &getPasswordBuf,
	})
	if err != nil {
		t.Fatalf("getPassword: %v", err)
	}
	passwordState, ok := passwordResult.(*tg.AccountPassword)
	if !ok || !passwordState.HasPassword {
		t.Fatalf("getPassword result = %T, want a password challenge", passwordResult)
	}
	if passwordState.HasRecovery {
		t.Fatal("getPassword advertised recovery in off mode")
	}
	srpB, ok := passwordState.GetSRPB()
	if !ok {
		t.Fatal("getPassword omitted SRP challenge")
	}
	proof, err := auth.PasswordHash([]byte(candidate.password), passwordState.SRPID, srpB, passwordState.SecureRandom, passwordState.CurrentAlgo)
	if err != nil {
		t.Fatalf("compute SRP proof: %v", err)
	}
	var settingsBuf bin.Buffer
	if err := (&tg.AccountGetPasswordSettingsRequest{Password: proof}).Encode(&settingsBuf); err != nil {
		t.Fatalf("encode getPasswordSettings: %v", err)
	}
	settingsResult, err := api.HandleGetPasswordSettings(h, &mtproto.Request{
		Ctx: ctx, UserID: user.ID, AuthKeyID: authKeyID, Buf: &settingsBuf,
	})
	if err != nil {
		t.Fatalf("proved getPasswordSettings: %v", err)
	}
	settings, ok := settingsResult.(*tg.AccountPasswordSettings)
	if !ok {
		t.Fatalf("getPasswordSettings result = %T, want password settings", settingsResult)
	}
	if email, present := settings.GetEmail(); present || email != "" {
		t.Fatalf("off mode returned recovery email %q (present=%t)", email, present)
	}
}

type recoveryEmailRow struct {
	salt1       []byte
	salt2       []byte
	verifier    []byte
	hint        string
	email       *string
	hasRecovery bool
	updatedAt   int64
}

func TestRecoveryEmailContactProofAndModeTransition(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	user, err := s.CreateUser(ctx, "+15551297552")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	var authKeyID [8]byte
	authKeyID[7] = 0x92
	if err := s.SaveAuthKey(ctx, mtproto.AuthKeyIDInt64(authKeyID), make([]byte, 256)); err != nil {
		t.Fatalf("save auth key: %v", err)
	}
	if err := s.BindAuthKeyUser(ctx, mtproto.AuthKeyIDInt64(authKeyID), user.ID); err != nil {
		t.Fatalf("bind auth key: %v", err)
	}

	candidate := newFirstPasswordCandidate(t, "contact", "contact password", 0xa1)
	h := api.SharedHandlersForTest(s)
	api.SetRecoveryEmailModeForTest(h, config.RecoveryEmailContact)
	var logs bytes.Buffer
	api.SetLoggerForTest(h, slog.New(slog.NewTextHandler(&logs, nil)))
	update := func(req *tg.AccountUpdatePasswordSettingsRequest) error {
		t.Helper()
		var buf bin.Buffer
		if err := req.Encode(&buf); err != nil {
			t.Fatalf("encode updatePasswordSettings: %v", err)
		}
		_, err := api.HandleUpdatePasswordSettings(h, &mtproto.Request{
			Ctx: ctx, UserID: user.ID, AuthKeyID: authKeyID, Buf: &buf,
		})
		return err
	}
	issueProof := func(password string) *tg.InputCheckPasswordSRP {
		t.Helper()
		var buf bin.Buffer
		if err := (&tg.AccountGetPasswordRequest{}).Encode(&buf); err != nil {
			t.Fatalf("encode getPassword: %v", err)
		}
		result, err := api.HandleGetPassword(h, &mtproto.Request{
			Ctx: ctx, UserID: user.ID, AuthKeyID: authKeyID, Buf: &buf,
		})
		if err != nil {
			t.Fatalf("getPassword: %v", err)
		}
		state, ok := result.(*tg.AccountPassword)
		if !ok || !state.HasPassword {
			t.Fatalf("getPassword result = %T, want password challenge", result)
		}
		if state.HasRecovery {
			t.Fatal("getPassword advertised recovery in contact mode")
		}
		srpB, ok := state.GetSRPB()
		if !ok {
			t.Fatal("getPassword omitted SRP challenge")
		}
		proof, err := auth.PasswordHash([]byte(password), state.SRPID, srpB, state.SecureRandom, state.CurrentAlgo)
		if err != nil {
			t.Fatalf("compute SRP proof: %v", err)
		}
		return proof
	}
	settings := func(proof *tg.InputCheckPasswordSRP) (*tg.AccountPasswordSettings, error) {
		t.Helper()
		var buf bin.Buffer
		if err := (&tg.AccountGetPasswordSettingsRequest{Password: proof}).Encode(&buf); err != nil {
			t.Fatalf("encode getPasswordSettings: %v", err)
		}
		result, err := api.HandleGetPasswordSettings(h, &mtproto.Request{
			Ctx: ctx, UserID: user.ID, AuthKeyID: authKeyID, Buf: &buf,
		})
		if err != nil {
			return nil, err
		}
		got, ok := result.(*tg.AccountPasswordSettings)
		if !ok {
			t.Fatalf("getPasswordSettings result = %T, want password settings", result)
		}
		return got, nil
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to inspect password row: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close password row connection: %v", err)
		}
	})
	readRow := func() recoveryEmailRow {
		t.Helper()
		var row recoveryEmailRow
		if err := conn.QueryRow(ctx, `
			SELECT salt1, salt2, verifier, hint, recovery_email, has_recovery,
			       (extract(epoch FROM updated_at) * 1000000)::bigint
			FROM user_passwords WHERE user_id = $1`, user.ID).Scan(
			&row.salt1, &row.salt2, &row.verifier, &row.hint, &row.email,
			&row.hasRecovery, &row.updatedAt,
		); err != nil {
			t.Fatalf("read password row: %v", err)
		}
		return row
	}

	if err := update(firstPasswordRPCRequest(candidate)); err != nil {
		t.Fatalf("set password with contact email: %v", err)
	}
	stored, ok, err := s.PasswordByUser(ctx, user.ID)
	if err != nil || !ok {
		t.Fatalf("load stored password: ok=%t err=%v", ok, err)
	}
	if stored.RecoveryEmail != candidate.email || stored.HasRecovery {
		t.Fatalf("contact mode stored password = {email:%q has_recovery:%t}, want email and false recovery flag", stored.RecoveryEmail, stored.HasRecovery)
	}

	// Model a legacy or restored row. The server must ignore has_recovery while
	// keeping the proved contact email available in contact mode.
	if _, err := conn.Exec(ctx, "UPDATE user_passwords SET has_recovery = true WHERE user_id = $1", user.ID); err != nil {
		t.Fatalf("seed legacy recovery flag: %v", err)
	}
	contactRow := readRow()
	wrongProof := issueProof("wrong password")
	if _, err := settings(wrongProof); !tgerr.Is(err, "PASSWORD_HASH_INVALID") {
		t.Fatalf("invalid proof error = %v, want PASSWORD_HASH_INVALID", err)
	}
	if _, err := settings(wrongProof); !tgerr.Is(err, "SRP_ID_INVALID") {
		t.Fatalf("replayed proof error = %v, want SRP_ID_INVALID", err)
	}
	contactSettings, err := settings(issueProof(candidate.password))
	if err != nil {
		t.Fatalf("proved contact getPasswordSettings: %v", err)
	}
	if email, present := contactSettings.GetEmail(); !present || email != candidate.email {
		t.Fatalf("contact mode email = %q (present=%t), want %q", email, present, candidate.email)
	}

	// Email-only updates verify the current password but keep the entire row,
	// including its timestamp and verifier, byte-identical.
	if err := update(&tg.AccountUpdatePasswordSettingsRequest{
		Password: issueProof(candidate.password),
		NewSettings: tg.AccountPasswordInputSettings{
			Email: "replacement@example.invalid",
		},
	}); err != nil {
		t.Fatalf("email-only update in contact mode: %v", err)
	}
	if got := readRow(); !reflect.DeepEqual(got, contactRow) {
		t.Fatalf("email-only update changed password row: before=%+v after=%+v", contactRow, got)
	}
	if strings.Contains(logs.String(), candidate.email) {
		t.Fatalf("application logs contain recovery email: %s", logs.String())
	}

	api.SetRecoveryEmailModeForTest(h, config.RecoveryEmailOff)
	if got := readRow(); !reflect.DeepEqual(got, contactRow) {
		t.Fatalf("switch to off rewrote password row: before=%+v after=%+v", contactRow, got)
	}
	offSettings, err := settings(issueProof(candidate.password))
	if err != nil {
		t.Fatalf("proved getPasswordSettings in off mode: %v", err)
	}
	if email, present := offSettings.GetEmail(); present || email != "" {
		t.Fatalf("off mode returned contact email %q (present=%t)", email, present)
	}

	replacement := newFirstPasswordCandidate(t, "off", "replacement password", 0xb1)
	req := firstPasswordRPCRequest(replacement)
	req.Password = issueProof(candidate.password)
	if err := update(req); err != nil {
		t.Fatalf("change password in off mode: %v", err)
	}
	stored, ok, err = s.PasswordByUser(ctx, user.ID)
	if err != nil || !ok {
		t.Fatalf("load changed password: ok=%t err=%v", ok, err)
	}
	if stored.RecoveryEmail != "" || stored.HasRecovery {
		t.Fatalf("off mode password change retained recovery data: email=%q has_recovery=%t", stored.RecoveryEmail, stored.HasRecovery)
	}
	if got := readRow(); got.email != nil {
		t.Fatalf("off mode changed row email = %q, want SQL NULL", *got.email)
	}

	api.SetRecoveryEmailModeForTest(h, config.RecoveryEmailContact)
	resumedSettings, err := settings(issueProof(replacement.password))
	if err != nil {
		t.Fatalf("proved getPasswordSettings after switching back: %v", err)
	}
	if email, present := resumedSettings.GetEmail(); present || email != "" {
		t.Fatalf("switching back restored no-backfill email %q (present=%t)", email, present)
	}
}
