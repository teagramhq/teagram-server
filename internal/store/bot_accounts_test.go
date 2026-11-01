package store_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestBotAccountSchemaProtectsKindsAndAuthKeyBindings(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	st, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithoutBlobStore())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open inspection pool: %v", err)
	}
	t.Cleanup(pool.Close)

	human, err := st.CreateUser(ctx, "15551234567")
	if err != nil {
		t.Fatalf("create legacy human: %v", err)
	}
	humanID := human.ID
	var kind string
	if err := pool.QueryRow(ctx, `SELECT kind FROM users WHERE id = $1`, humanID).Scan(&kind); err != nil {
		t.Fatalf("read default user kind: %v", err)
	}
	if kind != "human" {
		t.Fatalf("legacy user's kind = %q, want human", kind)
	}

	var botID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO users (phone, login_mode, first_name, kind)
		VALUES (NULL, 'username', 'Weather', 'bot')
		RETURNING id
	`).Scan(&botID); err != nil {
		t.Fatalf("insert bot: %v", err)
	}

	if _, err := pool.Exec(ctx, `UPDATE users SET kind = 'human' WHERE id = $1`, botID); !isCheckViolation(err) {
		t.Fatalf("change bot kind error = %v, want check-constraint error", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO bot_tokens (user_id, token_hash)
		VALUES ($1, decode(repeat('00', 32), 'hex'))
	`, humanID); !isForeignKeyViolation(err) {
		t.Fatalf("issue bot token for human error = %v, want foreign-key constraint error", err)
	}

	for _, tc := range []struct {
		name  string
		query string
	}{
		{name: "bound user", query: `UPDATE auth_keys SET user_id = $2 WHERE id = $1`},
		{name: "pending user", query: `UPDATE auth_keys SET pending_user_id = $2 WHERE id = $1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const keyID = int64(0x12345678)
			if err := st.SaveAuthKey(ctx, keyID, []byte("auth key")); err != nil {
				t.Fatalf("save auth key: %v", err)
			}
			if _, err := pool.Exec(ctx, tc.query, keyID, botID); !isForeignKeyViolation(err) {
				t.Fatalf("bind bot error = %v, want foreign-key constraint error", err)
			}
		})
	}
}

func TestBotUpdatesReadIncomingTextAndKeepConfirmedOffsetMonotonic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := store.Open(ctx, pgtest.DSN(t), pgtest.EncKey(), store.WithoutBlobStore())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	bot, token, err := st.CreateBot(ctx, "updates_weather_bot", "Weather")
	if err != nil {
		t.Fatalf("create bot: %v", err)
	}
	human, err := st.CreateUser(ctx, "15551234568")
	if err != nil {
		t.Fatalf("create human: %v", err)
	}
	_, _, incomingPts, duplicate, err := st.SendMessage(ctx, human.ID, bot.ID, "hello", 93001, 0, 0)
	if err != nil {
		t.Fatalf("send incoming text: %v", err)
	}
	if duplicate {
		t.Fatal("incoming text send unexpectedly deduplicated")
	}
	if _, _, _, err := st.SendBotMessage(ctx, bot.ID, human.ID, "reply", 93002); err != nil {
		t.Fatalf("send bot reply: %v", err)
	}
	_, _, afterGapPts, duplicate, err := st.SendMessage(ctx, human.ID, bot.ID, "after gap", 93003, 0, 0)
	if err != nil {
		t.Fatalf("send text after bot event gap: %v", err)
	}
	if duplicate {
		t.Fatal("text after bot event gap unexpectedly deduplicated")
	}
	page, err := st.BotMessageEvents(ctx, bot.ID, 0, 100)
	if err != nil {
		t.Fatalf("read bot message events: %v", err)
	}
	events := page.Events
	if len(events) != 2 || events[0].Pts != int64(incomingPts) || events[1].Pts != int64(afterGapPts) || events[0].Type != store.EventNewMessage || events[1].Type != store.EventNewMessage {
		t.Fatalf("bot events = %+v, want incoming texts at pts %d and %d around the bot-send gap", events, incomingPts, afterGapPts)
	}
	if page.NextOffset != int64(afterGapPts+1) {
		t.Fatalf("bot event scan offset = %d, want %d after the last event", page.NextOffset, afterGapPts+1)
	}
	if got := events[0]; got.Message.OwnerID != bot.ID || got.Message.LocalID != events[0].LocalID || got.Message.FromID != human.ID || got.Message.Text != "hello" || got.Sender.ID != human.ID || got.Sender.Kind != store.UserKindHuman || got.Sender.FirstName != human.FirstName {
		t.Fatalf("bot event projection = %+v, want message and sender hydrated in the event query", got)
	}

	poll, err := st.StartBotUpdatePoll(ctx, bot.ID, token)
	if err != nil {
		t.Fatalf("start bot update poll: %v", err)
	}
	confirmed := int64(incomingPts + 1)
	if _, err := st.FinishBotUpdatePoll(ctx, bot.ID, token, poll.Generation, confirmed); err != nil {
		t.Fatalf("advance bot offset: %v", err)
	}
	if _, err := st.FinishBotUpdatePoll(ctx, bot.ID, token, poll.Generation, int64(incomingPts)); err != nil {
		t.Fatalf("replay older bot offset: %v", err)
	}
	offset, err := st.BotUpdateOffset(ctx, bot.ID, token)
	if err != nil {
		t.Fatalf("read confirmed bot offset: %v", err)
	}
	if offset != confirmed {
		t.Fatalf("confirmed offset = %d, want monotonic %d", offset, confirmed)
	}
	page, err = st.BotMessageEvents(ctx, bot.ID, offset, 100)
	if err != nil {
		t.Fatalf("re-read bot message events: %v", err)
	}
	events = page.Events
	if len(events) != 1 || events[0].Pts != int64(afterGapPts) {
		t.Fatalf("events after offset inside bot-send gap = %+v, want pts %d without replaying pts %d", events, afterGapPts, incomingPts)
	}
	confirmed = int64(afterGapPts + 1)
	poll, err = st.StartBotUpdatePoll(ctx, bot.ID, token)
	if err != nil {
		t.Fatalf("start final bot update poll: %v", err)
	}
	if _, err := st.FinishBotUpdatePoll(ctx, bot.ID, token, poll.Generation, confirmed); err != nil {
		t.Fatalf("advance bot offset over final text: %v", err)
	}
	if _, err := st.FinishBotUpdatePoll(ctx, bot.ID, token, poll.Generation, int64(incomingPts+1)); err != nil {
		t.Fatalf("replay first bot offset: %v", err)
	}
	offset, err = st.BotUpdateOffset(ctx, bot.ID, token)
	if err != nil {
		t.Fatalf("read final confirmed bot offset: %v", err)
	}
	if offset != confirmed {
		t.Fatalf("confirmed offset after older replay = %d, want %d", offset, confirmed)
	}
	page, err = st.BotMessageEvents(ctx, bot.ID, offset, 100)
	if err != nil {
		t.Fatalf("read events after final offset: %v", err)
	}
	events = page.Events
	if len(events) != 0 {
		t.Fatalf("events after final offset = %+v, want none", events)
	}

	rotatedToken, err := st.RotateBotToken(ctx, bot.ID)
	if err != nil {
		t.Fatalf("rotate bot token: %v", err)
	}
	if _, err := st.BotUpdateOffset(ctx, bot.ID, token); !errors.Is(err, store.ErrBotTokenNotFound) {
		t.Fatalf("old token offset lookup error = %v, want token not found", err)
	}
	rotatedOffset, err := st.BotUpdateOffset(ctx, bot.ID, rotatedToken)
	if err != nil {
		t.Fatalf("read offset after token rotation: %v", err)
	}
	if rotatedOffset != confirmed {
		t.Fatalf("offset after token rotation = %d, want %d", rotatedOffset, confirmed)
	}
}

func TestBotMessageEventsSkipsExpiredAndPollCopyText(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	st, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithoutBlobStore())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open inspection pool: %v", err)
	}
	t.Cleanup(pool.Close)

	bot, _, err := st.CreateBot(ctx, "updates_filter_bot", "Filter")
	if err != nil {
		t.Fatalf("create bot: %v", err)
	}
	human, err := st.CreateUser(ctx, "15551234569")
	if err != nil {
		t.Fatalf("create human: %v", err)
	}

	expiredMessage, _, expiredPts, duplicate, err := st.SendMessage(ctx, human.ID, bot.ID, "expired text", 93101, 0, 0)
	if err != nil || duplicate {
		t.Fatalf("send expired text: duplicate=%v err=%v", duplicate, err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE messages
		SET expires_at = statement_timestamp() - interval '1 second'
		WHERE owner_id = $1 AND local_id = $2
	`, bot.ID, expiredMessage.PeerLocalID); err != nil {
		t.Fatalf("expire bot message copy: %v", err)
	}

	pollCopyMessage, _, pollCopyPts, duplicate, err := st.SendMessage(ctx, human.ID, bot.ID, "poll copy text", 93102, 0, 0)
	if err != nil || duplicate {
		t.Fatalf("send poll-copy text: duplicate=%v err=%v", duplicate, err)
	}
	const pollID int64 = 93102
	if _, err := pool.Exec(ctx, `
		INSERT INTO polls (id, creator_id, source_local_id, question)
		VALUES ($1, $2, $3, $4)
	`, pollID, human.ID, pollCopyMessage.LocalID, []byte("poll question")); err != nil {
		t.Fatalf("insert poll: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO poll_message_copies (owner_id, local_id, poll_id)
		VALUES ($1, $2, $3)
	`, bot.ID, pollCopyMessage.PeerLocalID, pollID); err != nil {
		t.Fatalf("insert poll message copy: %v", err)
	}

	page, err := st.BotMessageEvents(ctx, bot.ID, 0, 100)
	if err != nil {
		t.Fatalf("read bot message events: %v", err)
	}
	if len(page.Events) != 0 {
		t.Fatalf("bot events = %+v, want expired and poll-copy text excluded", page.Events)
	}
	wantOffset := int64(max(expiredPts, pollCopyPts) + 1)
	if page.NextOffset != wantOffset {
		t.Fatalf("scan offset = %d, want %d after both excluded events", page.NextOffset, wantOffset)
	}
}

func TestBotUpdatePhotoHistoryAdvancesScanOffset(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	bot, token, err := s.CreateBot(ctx, "photo_gap_updates_bot", "Photo gaps")
	if err != nil {
		t.Fatalf("create bot: %v", err)
	}
	human, err := s.CreateUser(ctx, "15551234569")
	if err != nil {
		t.Fatalf("create human: %v", err)
	}
	photo, err := s.AllocateFile(ctx, human.ID, 1, "image/jpeg", "photo.jpg", bigQuota)
	if err != nil {
		t.Fatalf("allocate photo: %v", err)
	}
	const photoCount = 21
	var lastPts int
	for i := range photoCount {
		_, _, recipientPts, duplicate, err := s.SendMessage(ctx, human.ID, bot.ID, "photo", 95000+int64(i), photo.ID, 0)
		if err != nil || duplicate {
			t.Fatalf("send photo %d: duplicate=%v err=%v", i, duplicate, err)
		}
		lastPts = recipientPts
	}

	poll, err := s.StartBotUpdatePoll(ctx, bot.ID, token)
	if err != nil {
		t.Fatalf("start bot update poll: %v", err)
	}
	const scanLimit = 10
	clientOffset := int64(0)
	storedOffset := poll.Offset
	for batch := range 3 {
		fromPts := max(storedOffset, clientOffset)
		page, err := s.BotMessageEvents(ctx, bot.ID, fromPts, scanLimit)
		if err != nil {
			t.Fatalf("read photo-only scan page %d: %v", batch, err)
		}
		if len(page.Events) != 0 {
			t.Fatalf("photo-only scan page %d returned updates: %+v", batch, page.Events)
		}
		if page.NextOffset <= fromPts {
			t.Fatalf("photo-only scan page %d next offset = %d, want progress beyond %d", batch, page.NextOffset, fromPts)
		}
		if batch < 2 && page.NextOffset >= int64(lastPts+1) {
			t.Fatalf("photo-only scan page %d advanced to %d, past the bounded page before final pts %d", batch, page.NextOffset, lastPts)
		}
		if _, err := s.FinishBotUpdatePoll(ctx, bot.ID, token, poll.Generation, page.NextOffset); err != nil {
			t.Fatalf("store photo-only scan offset %d: %v", page.NextOffset, err)
		}
		storedOffset, err = s.BotUpdateOffset(ctx, bot.ID, token)
		if err != nil || storedOffset != page.NextOffset {
			t.Fatalf("stored scan offset = %d, err %v; want %d", storedOffset, err, page.NextOffset)
		}
	}
	if storedOffset != int64(lastPts+1) {
		t.Fatalf("offset after scanning photo-only history = %d, want %d", storedOffset, lastPts+1)
	}
	page, err := s.BotMessageEvents(ctx, bot.ID, storedOffset, scanLimit)
	if err != nil {
		t.Fatalf("repeat photo-only scan after unchanged client offset: %v", err)
	}
	if len(page.Events) != 0 || page.NextOffset != storedOffset {
		t.Fatalf("repeated photo-only scan = %+v, next offset %d; want no updates and unchanged offset %d", page.Events, page.NextOffset, storedOffset)
	}
}

func TestBotUpdatePollGenerationFencesOffsetWrites(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := store.Open(ctx, pgtest.DSN(t), pgtest.EncKey(), store.WithoutBlobStore())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	bot, token, err := st.CreateBot(ctx, "generation_weather_bot", "Weather")
	if err != nil {
		t.Fatalf("create bot: %v", err)
	}

	first, err := st.StartBotUpdatePoll(ctx, bot.ID, token)
	if err != nil {
		t.Fatalf("start first poll: %v", err)
	}
	second, err := st.StartBotUpdatePoll(ctx, bot.ID, token)
	if err != nil {
		t.Fatalf("start second poll: %v", err)
	}
	if second.Generation != first.Generation+1 || second.Offset != first.Offset {
		t.Fatalf("poll generations = %+v then %+v, want next generation with same starting offset", first, second)
	}
	if err := st.CheckBotUpdatePoll(ctx, bot.ID, token, first.Generation); !errors.Is(err, store.ErrBotPollSuperseded) {
		t.Fatalf("first poll ownership error = %v, want superseded", err)
	}
	if _, err := st.FinishBotUpdatePoll(ctx, bot.ID, token, first.Generation, 5); !errors.Is(err, store.ErrBotPollSuperseded) {
		t.Fatalf("superseded first poll offset write error = %v, want superseded", err)
	}
	if offset, err := st.BotUpdateOffset(ctx, bot.ID, token); err != nil || offset != 0 {
		t.Fatalf("offset after superseded write = %d, err %v, want unchanged 0", offset, err)
	}
	if offset, err := st.FinishBotUpdatePoll(ctx, bot.ID, token, second.Generation, 3); err != nil || offset != 3 {
		t.Fatalf("winning poll offset write = %d, err %v, want 3", offset, err)
	}
	if offset, err := st.FinishBotUpdatePoll(ctx, bot.ID, token, second.Generation, 2); err != nil || offset != 3 {
		t.Fatalf("winning poll replayed older offset = %d, err %v, want monotonic 3", offset, err)
	}
}

func isCheckViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514"
}

func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

func TestBotTokenLifecycleStoresOnlyHashAndNormalizesFailures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	st, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithoutBlobStore())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open inspection pool: %v", err)
	}
	t.Cleanup(pool.Close)

	bot, token, err := st.CreateBot(ctx, "Weather_Bot", "Weather")
	if err != nil {
		t.Fatalf("create bot: %v", err)
	}
	if bot.ID <= 0 || bot.Username != "weather_bot" || bot.FirstName != "Weather" {
		t.Fatalf("created bot = %+v, want normalized username and supplied profile", bot)
	}
	idText, secretText, ok := strings.Cut(token, ":")
	if !ok || idText != strconv.FormatInt(bot.ID, 10) {
		t.Fatalf("token = %q, want <bot id>:<secret>", token)
	}
	secret, err := base64.RawURLEncoding.DecodeString(secretText)
	if err != nil || len(secret) != 32 {
		t.Fatalf("token secret decoded length = %d, error = %v; want 32 random bytes", len(secret), err)
	}
	secretHash := sha256.Sum256(secret)
	var storedHash []byte
	if err := pool.QueryRow(ctx, `SELECT token_hash FROM bot_tokens WHERE user_id = $1 AND revoked_at IS NULL`, bot.ID).Scan(&storedHash); err != nil {
		t.Fatalf("read stored token hash: %v", err)
	}
	if !bytes.Equal(storedHash, secretHash[:]) {
		t.Fatalf("stored token hash = %x, want SHA-256 %x", storedHash, secretHash)
	}

	resolvedID, err := st.BotIDByToken(ctx, token)
	if err != nil || resolvedID != bot.ID {
		t.Fatalf("lookup created token = (%d, %v), want (%d, nil)", resolvedID, err, bot.ID)
	}
	for _, candidate := range []string{
		"malformed",
		"0:" + secretText,
		fmt.Sprintf("%d:%s", bot.ID+1000000, secretText),
		fmt.Sprintf("%d:%s", bot.ID, "short"),
	} {
		if _, err := st.BotIDByToken(ctx, candidate); !errors.Is(err, store.ErrBotTokenNotFound) {
			t.Errorf("lookup %q error = %v, want the shared not-found result", candidate, err)
		}
	}

	rotated, err := st.RotateBotToken(ctx, bot.ID)
	if err != nil {
		t.Fatalf("rotate bot token: %v", err)
	}
	if rotated == token {
		t.Fatal("rotated token equals old token")
	}
	if _, err := st.BotIDByToken(ctx, token); !errors.Is(err, store.ErrBotTokenNotFound) {
		t.Fatalf("lookup old token after rotate error = %v, want the shared not-found result", err)
	}
	resolvedID, err = st.BotIDByToken(ctx, rotated)
	if err != nil || resolvedID != bot.ID {
		t.Fatalf("lookup rotated token = (%d, %v), want (%d, nil)", resolvedID, err, bot.ID)
	}

	if err := st.RevokeBotToken(ctx, bot.ID); err != nil {
		t.Fatalf("revoke bot token: %v", err)
	}
	if _, err := st.BotIDByToken(ctx, rotated); !errors.Is(err, store.ErrBotTokenNotFound) {
		t.Fatalf("lookup revoked token error = %v, want the shared not-found result", err)
	}
	if err := pool.QueryRow(ctx, `SELECT token_hash FROM bot_tokens WHERE user_id = $1`, bot.ID).Scan(&storedHash); err != nil {
		t.Fatalf("read revoked token row: %v", err)
	}
	if storedHash != nil {
		t.Fatalf("revoked token hash = %x, want NULL", storedHash)
	}
}
