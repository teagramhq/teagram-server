package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/mt"
	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

const (
	createUserInviteRequestID uint32 = 0xbf4f8b0e
	listUserInvitesRequestID  uint32 = 0x2ad32052
	revokeUserInviteRequestID uint32 = 0xa483022c
	userInviteLinkTypeID      uint32 = 0x0078b3b0
	userInviteTypeID          uint32 = 0xff81dc6c
	userInvitesTypeID         uint32 = 0xb5cfec46
)

type rawUserInviteRequest struct {
	id       uint32
	inviteID int64
	hasID    bool
}

func (r rawUserInviteRequest) Encode(b *bin.Buffer) error {
	b.PutID(r.id)
	if r.hasID {
		b.PutLong(r.inviteID)
	}
	return nil
}

func TestUserInviteConstructorsDoNotCollideWithTelegram(t *testing.T) {
	for _, id := range []uint32{
		createUserInviteRequestID,
		listUserInvitesRequestID,
		revokeUserInviteRequestID,
		userInviteLinkTypeID,
		userInviteTypeID,
		userInvitesTypeID,
	} {
		if name, exists := tg.TypesMap()[id]; exists {
			t.Errorf("custom constructor %#08x collides with Telegram type %q", id, name)
		}
	}
}

func TestCreateUserInviteRequiresListenerQuotaAndAuthorizedSession(t *testing.T) {
	for _, tc := range []struct {
		name        string
		userID      int64
		provisional bool
		mode        config.RegistrationMode
		quota       int
		code        int
		message     string
	}{
		{name: "unauthenticated", mode: config.RegistrationInvite, quota: 1, code: 401, message: "AUTH_KEY_UNREGISTERED"},
		{name: "provisional", userID: 1, provisional: true, mode: config.RegistrationInvite, quota: 1, code: 401, message: "AUTH_KEY_UNREGISTERED"},
		{name: "quota disabled", userID: 1, mode: config.RegistrationInvite, code: 400, message: "USER_INVITES_DISABLED"},
		{name: "closed listener", userID: 1, mode: config.RegistrationClosed, quota: 1, code: 400, message: "USER_INVITE_REGISTRATION_CLOSED"},
		{name: "open listener", userID: 1, mode: config.RegistrationOpen, quota: 1, code: 400, message: "USER_INVITE_REGISTRATION_CLOSED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newUserInviteTestHandler(nil, tc.mode, tc.quota, 0, slog.New(slog.DiscardHandler))
			body := dispatchRawUserInviteRequest(t, h, rawUserInviteRequest{id: createUserInviteRequestID}, tc.userID, tc.provisional)
			assertUserInviteRPCError(t, body, tc.code, tc.message)
		})
	}
}

func TestUserInviteRPCCreateListAndRevoke(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	owner, err := s.CreateUser(ctx, "+15559911001")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	other, err := s.CreateUser(ctx, "+15559911002")
	if err != nil {
		t.Fatalf("create other: %v", err)
	}

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	h := newUserInviteTestHandler(s, config.RegistrationInvite, 3, 0, logger)
	first := dispatchRawUserInviteRequest(t, h, rawUserInviteRequest{id: createUserInviteRequestID}, owner.ID, false)
	firstLink := decodeUserInviteLinkResponse(t, first)
	second := dispatchRawUserInviteRequest(t, h, rawUserInviteRequest{id: createUserInviteRequestID}, owner.ID, false)
	secondLink := decodeUserInviteLinkResponse(t, second)
	otherResponse := dispatchRawUserInviteRequest(t, h, rawUserInviteRequest{id: createUserInviteRequestID}, other.ID, false)
	otherLink := decodeUserInviteLinkResponse(t, otherResponse)

	secretPattern := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for _, invite := range []userInviteLinkResult{firstLink, secondLink, otherLink} {
		if !strings.HasPrefix(invite.Link, "https://links.example.test/invite#") {
			t.Errorf("invite link = %q, want fixed /invite fragment URL", invite.Link)
		}
		secret := strings.TrimPrefix(invite.Link, "https://links.example.test/invite#")
		if !secretPattern.MatchString(secret) {
			t.Errorf("invite link secret = %q, want 64 lowercase hex characters", secret)
		}
		if delta := time.Until(time.Unix(invite.ExpiresAt, 0)); delta < 7*24*time.Hour-2*time.Second || delta > 7*24*time.Hour+2*time.Second {
			t.Errorf("invite expiry is %s from now, want seven days", delta)
		}
	}
	if secondLink.ID <= firstLink.ID {
		t.Fatalf("second invite id %d is not newer than first id %d", secondLink.ID, firstLink.ID)
	}

	listRaw := dispatchRawUserInviteRequest(t, h, rawUserInviteRequest{id: listUserInvitesRequestID}, owner.ID, false)
	listed := decodeUserInvitesResponse(t, listRaw)
	if listed.Quota != 3 || listed.WeeklyCount != 2 || listed.NextAvailableAt == nil {
		t.Fatalf("list summary = quota %d, count %d, next %v; want 3, 2, set", listed.Quota, listed.WeeklyCount, listed.NextAvailableAt)
	}
	if len(listed.Invites) != 2 {
		t.Fatalf("listed %d invites, want 2", len(listed.Invites))
	}
	if want := int64(listed.Invites[1].CreatedAt) + int64(7*24*time.Hour/time.Second); *listed.NextAvailableAt != want {
		t.Errorf("next available at = %d, want oldest invite creation plus seven days (%d)", *listed.NextAvailableAt, want)
	}
	if len(listed.Invites) != 2 || listed.Invites[0].ID != secondLink.ID || listed.Invites[1].ID != firstLink.ID {
		t.Fatalf("listed invites = %+v, want owner's two invites newest first", listed.Invites)
	}
	for _, invite := range listed.Invites {
		if invite.State != "pending" || invite.CreatedAt <= 0 || invite.ExpiresAt <= invite.CreatedAt {
			t.Errorf("listed metadata = %+v, want pending with valid dates", invite)
		}
	}
	for _, response := range []struct {
		body []byte
		link userInviteLinkResult
	}{{listRaw, firstLink}, {listRaw, secondLink}} {
		secret := strings.TrimPrefix(response.link.Link, "https://links.example.test/invite#")
		digest := sha256.Sum256([]byte(secret))
		if bytes.Contains(response.body, []byte(secret)) || bytes.Contains(response.body, digest[:]) || bytes.Contains(response.body, []byte(hex.EncodeToString(digest[:]))) {
			t.Fatal("list response included an invite secret or digest")
		}
	}

	foreignRevoke := dispatchRawUserInviteRequest(t, h, rawUserInviteRequest{id: revokeUserInviteRequestID, inviteID: otherLink.ID, hasID: true}, owner.ID, false)
	assertUserInviteBoolTrue(t, foreignRevoke)
	absentRevoke := dispatchRawUserInviteRequest(t, h, rawUserInviteRequest{id: revokeUserInviteRequestID, inviteID: int64(1 << 61), hasID: true}, owner.ID, false)
	assertUserInviteBoolTrue(t, absentRevoke)
	if !bytes.Equal(foreignRevoke, absentRevoke) {
		t.Fatalf("foreign revoke response %x differs from absent-id response %x", foreignRevoke, absentRevoke)
	}
	otherListRaw := dispatchRawUserInviteRequest(t, h, rawUserInviteRequest{id: listUserInvitesRequestID}, other.ID, false)
	otherListed := decodeUserInvitesResponse(t, otherListRaw)
	if len(otherListed.Invites) != 1 || otherListed.Invites[0].State != "pending" {
		t.Fatalf("foreign revoke changed other user's invite: %+v", otherListed.Invites)
	}

	ownRevoke := dispatchRawUserInviteRequest(t, h, rawUserInviteRequest{id: revokeUserInviteRequestID, inviteID: firstLink.ID, hasID: true}, owner.ID, false)
	assertUserInviteBoolTrue(t, ownRevoke)
	ownerAfterRevoke := decodeUserInvitesResponse(t, dispatchRawUserInviteRequest(t, h, rawUserInviteRequest{id: listUserInvitesRequestID}, owner.ID, false))
	if ownerAfterRevoke.WeeklyCount != 2 || len(ownerAfterRevoke.Invites) != 2 || ownerAfterRevoke.Invites[1].State != "revoked" {
		t.Fatalf("revoked invite summary = count %d invites %+v", ownerAfterRevoke.WeeklyCount, ownerAfterRevoke.Invites)
	}

	for _, invite := range []userInviteLinkResult{firstLink, secondLink, otherLink} {
		secret := strings.TrimPrefix(invite.Link, "https://links.example.test/invite#")
		digest := sha256.Sum256([]byte(secret))
		if bytes.Contains(logs.Bytes(), []byte(secret)) || bytes.Contains(logs.Bytes(), digest[:]) || bytes.Contains(logs.Bytes(), []byte(hex.EncodeToString(digest[:]))) {
			t.Fatal("create path logged an invite secret or digest")
		}
	}
}

func TestUserInviteRPCLogsStoreFailuresWithoutLoggingCredential(t *testing.T) {
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	owner, err := s.CreateUser(ctx, "+15559911003")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}

	conn, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open test connection: %v", err)
	}
	t.Cleanup(conn.Close)
	if _, err := conn.Exec(ctx, `
CREATE FUNCTION fail_user_invite_insert() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'user invite insert rejected digest=%', encode(NEW.secret_digest, 'hex');
END;
$$`); err != nil {
		t.Fatalf("create invite failure trigger function: %v", err)
	}
	if _, err := conn.Exec(ctx, `
CREATE TRIGGER fail_user_invite_insert
BEFORE INSERT ON registration_invites
FOR EACH ROW EXECUTE FUNCTION fail_user_invite_insert()`); err != nil {
		t.Fatalf("create invite failure trigger: %v", err)
	}

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	h := newUserInviteTestHandler(s, config.RegistrationInvite, 3, 0, logger)
	create := dispatchRawUserInviteRequest(t, h, rawUserInviteRequest{id: createUserInviteRequestID}, owner.ID, false)
	assertUserInviteRPCError(t, create, 500, "INTERNAL")
	credentialPattern := regexp.MustCompile(`(?i)[0-9a-f]{64}`)
	if credentialPattern.MatchString(logs.String()) {
		t.Fatalf("create failure log contains a 64-character invite credential: %s", logs.String())
	}

	if err := s.Close(); err != nil {
		t.Fatalf("close store to simulate outage: %v", err)
	}
	list := dispatchRawUserInviteRequest(t, h, rawUserInviteRequest{id: listUserInvitesRequestID}, owner.ID, false)
	assertUserInviteRPCError(t, list, 500, "INTERNAL")
	revoke := dispatchRawUserInviteRequest(t, h, rawUserInviteRequest{id: revokeUserInviteRequestID, inviteID: 1, hasID: true}, owner.ID, false)
	assertUserInviteRPCError(t, revoke, 500, "INTERNAL")

	logOutput := logs.String()
	for _, operation := range []string{"create", "list", "revoke"} {
		if !strings.Contains(logOutput, "operation="+operation) {
			t.Errorf("store failure log missing operation %q: %s", operation, logOutput)
		}
	}
	if !strings.Contains(logOutput, "user_id="+strconv.FormatInt(owner.ID, 10)) {
		t.Errorf("store failure log missing user id %d: %s", owner.ID, logOutput)
	}
	if !strings.Contains(logOutput, "err=") || !strings.Contains(logOutput, "user invite insert rejected digest=[redacted]") {
		t.Errorf("store failure logs do not include a redacted error cause: %s", logOutput)
	}
}

func TestUserInviteRPCAgeAndWeeklyQuotaErrors(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	user, err := s.CreateUser(ctx, "+15559911003")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	tooNew := newUserInviteTestHandler(s, config.RegistrationInvite, 1, 24*time.Hour, slog.New(slog.DiscardHandler))
	body := dispatchRawUserInviteRequest(t, tooNew, rawUserInviteRequest{id: createUserInviteRequestID}, user.ID, false)
	assertUserInviteRPCErrorPrefix(t, body, 420, "USER_INVITE_ACCOUNT_TOO_NEW_WAIT_")

	quotaOne := newUserInviteTestHandler(s, config.RegistrationInvite, 1, 0, slog.New(slog.DiscardHandler))
	created := decodeUserInviteLinkResponse(t, dispatchRawUserInviteRequest(t, quotaOne, rawUserInviteRequest{id: createUserInviteRequestID}, user.ID, false))
	revoked := dispatchRawUserInviteRequest(t, quotaOne, rawUserInviteRequest{id: revokeUserInviteRequestID, inviteID: created.ID, hasID: true}, user.ID, false)
	assertUserInviteBoolTrue(t, revoked)
	limited := dispatchRawUserInviteRequest(t, quotaOne, rawUserInviteRequest{id: createUserInviteRequestID}, user.ID, false)
	assertUserInviteRPCError(t, limited, 400, "USER_INVITE_WEEKLY_LIMIT_REACHED")
}

type userInviteLinkResult struct {
	ID        int64
	Link      string
	ExpiresAt int64
}

func decodeUserInviteLinkResponse(t *testing.T, raw []byte) userInviteLinkResult {
	t.Helper()
	buf := &bin.Buffer{Buf: raw}
	constructor, err := buf.ID()
	if err != nil || constructor != userInviteLinkTypeID {
		t.Fatalf("create response constructor = %#08x, err %v; want %#08x", constructor, err, userInviteLinkTypeID)
	}
	id, err := buf.Long()
	if err != nil {
		t.Fatalf("decode invite id: %v", err)
	}
	link, err := buf.String()
	if err != nil {
		t.Fatalf("decode invite link: %v", err)
	}
	expiresAt, err := buf.Int()
	if err != nil || buf.Len() != 0 {
		t.Fatalf("decode invite expiry: %v, trailing bytes %d", err, buf.Len())
	}
	return userInviteLinkResult{ID: id, Link: link, ExpiresAt: int64(expiresAt)}
}

type userInviteListItem struct {
	ID        int64
	State     string
	CreatedAt int
	ExpiresAt int
}

type userInvitesListResult struct {
	Invites         []userInviteListItem
	Quota           int64
	WeeklyCount     int64
	NextAvailableAt *int64
}

func decodeUserInvitesResponse(t *testing.T, raw []byte) userInvitesListResult {
	t.Helper()
	buf := &bin.Buffer{Buf: raw}
	constructor, err := buf.ID()
	if err != nil || constructor != userInvitesTypeID {
		t.Fatalf("list response constructor = %#08x, err %v; want %#08x", constructor, err, userInvitesTypeID)
	}
	vector, err := buf.ID()
	if err != nil || vector != bin.TypeVector {
		t.Fatalf("list vector constructor = %#08x, err %v; want %#08x", vector, err, bin.TypeVector)
	}
	count, err := buf.Int()
	if err != nil || count < 0 || count > 100 {
		t.Fatalf("list count = %d, err %v", count, err)
	}
	result := userInvitesListResult{Invites: make([]userInviteListItem, count)}
	for i := range result.Invites {
		itemType, err := buf.ID()
		if err != nil || itemType != userInviteTypeID {
			t.Fatalf("list item %d constructor = %#08x, err %v; want %#08x", i, itemType, err, userInviteTypeID)
		}
		item := &result.Invites[i]
		if item.ID, err = buf.Long(); err != nil {
			t.Fatalf("list item %d id: %v", i, err)
		}
		if item.State, err = buf.String(); err != nil {
			t.Fatalf("list item %d state: %v", i, err)
		}
		if item.CreatedAt, err = buf.Int(); err != nil {
			t.Fatalf("list item %d created_at: %v", i, err)
		}
		if item.ExpiresAt, err = buf.Int(); err != nil {
			t.Fatalf("list item %d expires_at: %v", i, err)
		}
	}
	if result.Quota, err = buf.Long(); err != nil {
		t.Fatalf("decode list quota: %v", err)
	}
	if result.WeeklyCount, err = buf.Long(); err != nil {
		t.Fatalf("decode list weekly count: %v", err)
	}
	flags, err := buf.Int()
	if err != nil || flags&^1 != 0 {
		t.Fatalf("list flags = %d, err %v", flags, err)
	}
	if flags&1 != 0 {
		next, err := buf.Long()
		if err != nil {
			t.Fatalf("decode next available time: %v", err)
		}
		result.NextAvailableAt = &next
	}
	if buf.Len() != 0 {
		t.Fatalf("list response has %d trailing bytes", buf.Len())
	}
	return result
}

func assertUserInviteBoolTrue(t *testing.T, raw []byte) {
	t.Helper()
	buf := &bin.Buffer{Buf: raw}
	var value tg.BoolTrue
	if err := value.Decode(buf); err != nil || buf.Len() != 0 {
		t.Fatalf("revoke response decode: %v, trailing bytes %d", err, buf.Len())
	}
}

func assertUserInviteRPCErrorPrefix(t *testing.T, body []byte, wantCode int, prefix string) {
	t.Helper()
	rpc := &mt.RPCError{}
	if err := rpc.Decode(&bin.Buffer{Buf: body}); err != nil {
		t.Fatalf("decode user-invite RPC error: %v", err)
	}
	if rpc.ErrorCode != wantCode || !strings.HasPrefix(rpc.ErrorMessage, prefix) {
		t.Fatalf("user-invite error = %d %q, want code %d and prefix %q", rpc.ErrorCode, rpc.ErrorMessage, wantCode, prefix)
	}
	seconds, err := strconv.ParseInt(strings.TrimPrefix(rpc.ErrorMessage, prefix), 10, 64)
	if err != nil || seconds < 1 {
		t.Fatalf("user-invite wait error = %q, want a positive remaining wait in seconds", rpc.ErrorMessage)
	}
}

func newUserInviteTestHandler(s *store.Store, mode config.RegistrationMode, quota int, minAge time.Duration, logger *slog.Logger) mtproto.Handler {
	return api.NewWithPhotoThumbsAndDialogFilterSyncAndUploadPartTTLAndTurnstileAndWelcomeChannelAndSignInAndStickerCatalogAndUserInviteQuota(
		s, 2, &tg.Config{MeURLPrefix: "https://links.example.test/"}, logger, false,
		api.TestMaxFileBytes, nil, 1, pgtest.PeerDeriver(), pgtest.PhotoDeriver(),
		config.RateLimitsConfig{}, mode, api.NewDialogFilterSync(), nil, config.DefaultUploadPartTTL,
		"", nil, "", false, config.RecoveryEmailOff, config.DefaultSignUpTermsOfService,
		nil, config.StickerImportOn, quota, minAge, false,
	)
}

func dispatchRawUserInviteRequest(t *testing.T, h mtproto.Handler, request rawUserInviteRequest, userID int64, provisional bool) []byte {
	t.Helper()
	key := testKey()
	transport := &settingsDispatcherTransport{}
	conn := mtproto.NewTestConn(transport, key)
	var body bin.Buffer
	if err := request.Encode(&body); err != nil {
		t.Fatalf("encode user-invite request: %v", err)
	}
	const msgID = int64(1 << 32)
	if err := h.OnMessage(conn, &mtproto.Request{
		AuthKeyID:   key.ID,
		UserID:      userID,
		Provisional: provisional,
		MsgID:       msgID,
		Buf:         &body,
		Ctx:         context.Background(),
	}); err != nil {
		t.Fatalf("dispatch user-invite request: %v", err)
	}
	return transport.result(t, key, msgID)
}

func assertUserInviteRPCError(t *testing.T, body []byte, wantCode int, wantMessage string) {
	t.Helper()
	rpc := &mt.RPCError{}
	if err := rpc.Decode(&bin.Buffer{Buf: body}); err != nil {
		t.Fatalf("decode user-invite RPC error: %v", err)
	}
	if rpc.ErrorCode != wantCode || rpc.ErrorMessage != wantMessage {
		t.Fatalf("user-invite error = %d %q, want %d %q", rpc.ErrorCode, rpc.ErrorMessage, wantCode, wantMessage)
	}
}
