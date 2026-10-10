package api_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/netip"
	"reflect"
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/store"
)

type phoneCodeRecord struct {
	hash      string
	code      string
	attempts  int32
	consumed  bool
	createdAt time.Time
	expiresAt time.Time
}

func phoneCodeRecords(t *testing.T, dsn, phone string) []phoneCodeRecord {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal("connect to inspect code state")
	}
	defer pool.Close()
	rows, err := pool.Query(context.Background(), `
		SELECT code_hash, code, attempts, consumed_at IS NOT NULL, created_at, expires_at
		FROM phone_codes WHERE phone = $1 ORDER BY created_at, id`, phone)
	if err != nil {
		t.Fatal("read code state")
	}
	defer rows.Close()
	var records []phoneCodeRecord
	for rows.Next() {
		var record phoneCodeRecord
		if err := rows.Scan(&record.hash, &record.code, &record.attempts, &record.consumed, &record.createdAt, &record.expiresAt); err != nil {
			t.Fatal("scan code state")
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		t.Fatal("iterate code state")
	}
	return records
}

func TestSignInPhoneRefusesLegacyUsersWithoutChangingState(t *testing.T) {
	t.Parallel()
	for _, hasPassword := range []bool{false, true} {
		name := "without password"
		if hasPassword {
			name = "with password verifier"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s, dsn := openStoreDSN(t)
			const phone = "+15551298003"
			user, err := s.CreateUser(ctx, phone)
			if err != nil {
				t.Fatal("create synthetic legacy account")
			}
			if hasPassword {
				if err := s.UpsertPassword(ctx, store.UserPassword{
					UserID: user.ID, Salt1: []byte("salt-one"), Salt2: []byte("salt-two"), Verifier: []byte("synthetic-verifier"),
				}); err != nil {
					t.Fatal("seed password verifier")
				}
			}
			hash, code, err := s.IssueCode(ctx, phone)
			if err != nil {
				t.Fatal("seed valid code")
			}
			const keyID = int64(0x2)
			if err := s.SaveAuthKey(ctx, keyID, make([]byte, 256)); err != nil {
				t.Fatal("save unbound auth key")
			}

			codeBefore := phoneCodeRecords(t, dsn, store.NormalizePhone(phone))
			userBefore, found, err := s.UserByPhone(ctx, store.NormalizePhone(phone))
			if err != nil || !found {
				t.Fatal("read synthetic legacy account")
			}
			keyBefore, found, err := s.AuthKeyByID(ctx, keyID)
			if err != nil || !found {
				t.Fatal("read unbound auth key")
			}
			usersBefore := countUsers(t, dsn)
			logs := &captureHandler{}
			_, err = api.SignInForTestWithLimitsAndLogger(s, [8]byte{2}, netip.MustParseAddr("10.0.0.2"), store.RateLimitConfig{Limit: 1, Window: time.Hour}, &tg.AuthSignInRequest{
				PhoneNumber: phone, PhoneCodeHash: hash, PhoneCode: code,
			}, slog.New(logs))
			if !isPhoneNumberInvalid(err) {
				t.Fatalf("phone signIn error = %v, want PHONE_NUMBER_INVALID", err)
			}
			if after := phoneCodeRecords(t, dsn, store.NormalizePhone(phone)); !reflect.DeepEqual(after, codeBefore) {
				t.Fatal("phone signIn changed stored code state")
			}
			userAfter, found, err := s.UserByPhone(ctx, store.NormalizePhone(phone))
			if err != nil || !found || !reflect.DeepEqual(userAfter, userBefore) {
				t.Fatal("phone signIn changed account state")
			}
			if after := countUsers(t, dsn); after != usersBefore {
				t.Fatal("phone signIn changed user count")
			}
			keyAfter, found, err := s.AuthKeyByID(ctx, keyID)
			if err != nil || !found || !sameAuthKeyState(keyAfter, keyBefore) {
				t.Fatal("phone signIn changed auth-key session state")
			}
			if len(logs.records) != 0 {
				t.Fatal("phone signIn wrote a log record")
			}
		})
	}
}

func TestSignInPhoneRefusalLeavesOtherBoundSessionUsable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	const phone = "+15551298005"
	user, err := s.CreateUser(ctx, phone)
	if err != nil {
		t.Fatal("create synthetic legacy account")
	}
	hash, code, err := s.IssueCode(ctx, phone)
	if err != nil {
		t.Fatal("seed valid code")
	}
	const boundKeyID, requestKeyID = int64(0x11), int64(0x12)
	for _, keyID := range []int64{boundKeyID, requestKeyID} {
		if err := s.SaveAuthKey(ctx, keyID, make([]byte, 256)); err != nil {
			t.Fatal("save auth key")
		}
	}
	if err := s.BindAuthKeyUser(ctx, boundKeyID, user.ID); err != nil {
		t.Fatal("bind existing session")
	}
	codeBefore := phoneCodeRecords(t, dsn, store.NormalizePhone(phone))

	_, err = api.SignInForTestWithLimits(s, [8]byte{0x12}, netip.MustParseAddr("10.0.0.12"), store.RateLimitConfig{}, &tg.AuthSignInRequest{
		PhoneNumber: phone, PhoneCodeHash: hash, PhoneCode: code,
	})
	if !isPhoneNumberInvalid(err) {
		t.Fatalf("phone signIn error = %v, want PHONE_NUMBER_INVALID", err)
	}
	if after := phoneCodeRecords(t, dsn, store.NormalizePhone(phone)); !reflect.DeepEqual(after, codeBefore) {
		t.Fatal("phone signIn changed stored code state")
	}
	boundKey, found, err := s.AuthKeyByID(ctx, boundKeyID)
	if err != nil || !found || boundKey.UserID != user.ID || boundKey.PendingUserID != 0 {
		t.Fatal("phone signIn changed the already-bound session")
	}
	requestKey, found, err := s.AuthKeyByID(ctx, requestKeyID)
	if err != nil || !found || requestKey.UserID != 0 || requestKey.PendingUserID != 0 {
		t.Fatal("phone signIn changed the requesting session")
	}
	res, err := api.GetAuthorizationsForTest(s, boundKey.UserID, [8]byte{0x11})
	if err != nil {
		t.Fatal("authenticated account.getAuthorizations")
	}
	auths, ok := res.(*tg.AccountAuthorizations)
	if !ok || len(auths.Authorizations) != 1 || !auths.Authorizations[0].Current || auths.Authorizations[0].Hash != boundKeyID {
		t.Fatal("bound session could not complete authenticated account.getAuthorizations")
	}
}

func sameAuthKeyState(a, b store.AuthKey) bool {
	return a.ID == b.ID && bytes.Equal(a.Value, b.Value) && a.UserID == b.UserID &&
		a.PendingUserID == b.PendingUserID && a.PendingStartedAt.Equal(b.PendingStartedAt) &&
		a.CreatedAt.Equal(b.CreatedAt) && a.LastSeenAt.Equal(b.LastSeenAt)
}
