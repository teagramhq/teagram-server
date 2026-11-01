package api_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/mt"
	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestCreateStickerSetRejectsEmptyStickerList(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	owner, err := s.CreateUser(ctx, "+15551819001")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}

	body := dispatchChatMethod(t, fullChannelDispatcher(s), owner.ID, "stickers.createStickerSet", &tg.StickersCreateStickerSetRequest{
		UserID:    &tg.InputUserSelf{},
		Title:     "Cats",
		ShortName: "cats_a",
	})
	if got := stickerSetRPCMessage(t, body); got != "STICKERS_EMPTY" {
		t.Fatalf("createStickerSet with no stickers = %q, want STICKERS_EMPTY", got)
	}
}

func TestCreateStickerSetPreflightsAllFilesBeforeReadingBytes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	blobs := &countingDownloadBlobStore{Store: testBlobs(t)}
	s := openStickerStore(t, blobs)
	owner, err := s.CreateUser(ctx, "+15551819021")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	other, err := s.CreateUser(ctx, "+15551819022")
	if err != nil {
		t.Fatalf("create other account: %v", err)
	}
	ownedFile := storedStickerDocument(t, s, blobs, owner.ID, validStickerTGS(t))
	foreignFile := storedStickerDocument(t, s, blobs, other.ID, validStickerTGS(t))
	request := createStickerRequest("foreign_last", ownedFile, false, false)
	request.Stickers = append(request.Stickers, tg.InputStickerSetItem{
		Document: &tg.InputDocument{ID: foreignFile.ID, AccessHash: foreignFile.AccessHash},
		Emoji:    "⭐",
	})
	blobs.reads.Store(0)

	got := stickerRPCError(t, dispatchChatMethod(t, fullChannelDispatcher(s), owner.ID, "stickers.createStickerSet", request))
	if got != "STICKER_DOCUMENT_INVALID" {
		t.Fatalf("foreign last document create = %q, want STICKER_DOCUMENT_INVALID", got)
	}
	if reads := blobs.reads.Load(); reads != 0 {
		t.Fatalf("foreign last document caused %d blob reads before rejection, want zero", reads)
	}
	createStickerSet(t, fullChannelDispatcher(s), owner.ID, createStickerRequest("first_set", ownedFile, false, false))
	blobs.reads.Store(0)
	got = stickerRPCError(t, dispatchChatMethod(t, fullChannelDispatcher(s), owner.ID, "stickers.createStickerSet", createStickerRequest("reused_file", ownedFile, false, false)))
	if got != "STICKER_DOCUMENT_INVALID" {
		t.Fatalf("already-used document create = %q, want STICKER_DOCUMENT_INVALID", got)
	}
	if reads := blobs.reads.Load(); reads != 0 {
		t.Fatalf("already-used document caused %d blob reads before rejection, want zero", reads)
	}
}

func TestCreateStickerSetHasIndependentBudgetAndChargesFailures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	owner, err := s.CreateUser(ctx, "+15551819023")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	h := fullChannelDispatcherWithRateLimits(s, config.DefaultRateLimits())
	for call := 1; call <= 60; call++ {
		body := dispatchChatMethod(t, h, owner.ID, "stickers.checkShortName", &tg.StickersCheckShortNameRequest{ShortName: fmt.Sprintf("candidate_%d", call)})
		var available tg.BoolTrue
		if err := available.Decode(&bin.Buffer{Buf: body}); err != nil {
			t.Fatalf("checkShortName call %d: %v", call, err)
		}
	}
	request := &tg.StickersCreateStickerSetRequest{UserID: &tg.InputUserSelf{}, Title: "Cats", ShortName: "empty_set"}
	for call := 1; call <= 3; call++ {
		if got := stickerRPCError(t, dispatchChatMethod(t, h, owner.ID, "stickers.createStickerSet", request)); got != "STICKERS_EMPTY" {
			t.Fatalf("failed create call %d after lookup budget exhaustion = %q, want STICKERS_EMPTY", call, got)
		}
	}
	if got := stickerRPCError(t, dispatchChatMethod(t, h, owner.ID, "stickers.createStickerSet", request)); !strings.HasPrefix(got, "FLOOD_WAIT_") {
		t.Fatalf("fourth failed create = %q, want FLOOD_WAIT after charging the first three failures", got)
	}
}

func TestCreateStickerSetBoundsEmojiAndKeywords(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	blobs := testBlobs(t)
	dsn := pgtest.DSN(t)
	s := openStickerStore(t, blobs, dsn)
	owner, err := s.CreateUser(ctx, "+15551819024")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	h := fullChannelDispatcher(s)
	keyword := strings.Repeat("k", 64)
	tooManyTerms := make([]string, 21)
	for i := range tooManyTerms {
		tooManyTerms[i] = keyword
	}
	for _, test := range []struct {
		name   string
		mutate func(*tg.InputStickerSetItem)
		want   string
	}{
		{name: "empty emoji", mutate: func(item *tg.InputStickerSetItem) { item.Emoji = "" }, want: "STICKER_EMOJI_INVALID"},
		{name: "emoji", mutate: func(item *tg.InputStickerSetItem) { item.Emoji = strings.Repeat("x", 21) }, want: "STICKER_EMOJI_INVALID"},
		{name: "keyword bytes", mutate: func(item *tg.InputStickerSetItem) { item.SetKeywords(strings.Repeat("k", 65)) }, want: "STICKERSET_INVALID"},
		{name: "keyword count", mutate: func(item *tg.InputStickerSetItem) { item.SetKeywords(strings.Join(tooManyTerms, ",")) }, want: "STICKERSET_INVALID"},
	} {
		t.Run(test.name, func(t *testing.T) {
			file := storedStickerDocument(t, s, blobs, owner.ID, validStickerTGS(t))
			request := createStickerRequest("too_long_"+strings.ReplaceAll(test.name, " ", "_"), file, false, false)
			test.mutate(&request.Stickers[0])
			if got := stickerRPCError(t, dispatchChatMethod(t, h, owner.ID, "stickers.createStickerSet", request)); got != test.want {
				t.Fatalf("invalid %s = %q, want %s", test.name, got, test.want)
			}
		})
	}
	file := storedStickerDocument(t, s, blobs, owner.ID, validStickerTGS(t))
	keywords := make([]string, 20)
	for i := range keywords {
		keywords[i] = keyword
	}
	valid := createStickerRequest("boundary_text", file, false, false)
	valid.Stickers[0].Emoji = strings.Repeat("⭐", 20)
	valid.Stickers[0].SetKeywords(strings.Join(keywords, ","))
	created := createStickerSet(t, h, owner.ID, valid)
	if created.Set.Count != 1 {
		t.Fatalf("boundary text set count = %d, want 1", created.Set.Count)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect for text constraint checks: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close text constraint connection: %v", err)
		}
	})
	for i, test := range []struct {
		name       string
		emoji      string
		keywords   string
		constraint string
	}{
		{name: "empty emoji", emoji: "", constraint: "user_created_sticker_set_items_emoji_limit"},
		{name: "emoji", emoji: strings.Repeat("x", 21), constraint: "user_created_sticker_set_items_emoji_limit"},
		{name: "keyword bytes", emoji: "⭐", keywords: strings.Repeat("k", 65), constraint: "user_created_sticker_set_items_keywords_limit"},
		{name: "keyword count", emoji: "⭐", keywords: strings.Join(tooManyTerms, ","), constraint: "user_created_sticker_set_items_keywords_limit"},
	} {
		file := storedStickerDocument(t, s, blobs, owner.ID, validStickerTGS(t))
		if _, err := conn.Exec(ctx, `
			INSERT INTO user_created_sticker_set_items
			    (set_id, position, file_id, emoji, keywords, format, width, height)
			VALUES ($1, $2, $3, $4, $5, 'TGS', 1, 1)`,
			created.Set.ID, int32(i+1), file.ID, test.emoji, test.keywords); err == nil {
			t.Errorf("database accepted invalid %s", test.name)
		} else {
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.ConstraintName != test.constraint {
				t.Errorf("database rejected invalid %s with %v, want constraint %s", test.name, err, test.constraint)
			}
		}
	}
}

func TestAddStickerToSetRateLimitAppliesToInvalidSet(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	owner, err := s.CreateUser(ctx, "+15551819009")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	h := fullChannelDispatcher(s)
	req := &tg.StickersAddStickerToSetRequest{
		Stickerset: &tg.InputStickerSetShortName{ShortName: "cats"},
		Sticker:    tg.InputStickerSetItem{Document: &tg.InputDocument{ID: 1, AccessHash: 1}, Emoji: "🐈"},
	}
	for call := 1; call <= 60; call++ {
		if got := stickerRPCError(t, dispatchChatMethod(t, h, owner.ID, "stickers.addStickerToSet", req)); got != "STICKERSET_INVALID" {
			t.Fatalf("addStickerToSet call %d = %q, want STICKERSET_INVALID", call, got)
		}
	}
	if got := stickerRPCError(t, dispatchChatMethod(t, h, owner.ID, "stickers.addStickerToSet", req)); !strings.HasPrefix(got, "FLOOD_WAIT_") {
		t.Fatalf("addStickerToSet over limit = %q, want FLOOD_WAIT", got)
	}
}

func TestCreateStickerSetUsesStoredBytesAndReturnsOwnerView(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	blobs := testBlobs(t)
	dsn := pgtest.DSN(t)
	s := openStickerStore(t, blobs, dsn)
	owner, err := s.CreateUser(ctx, "+15551819002")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	other, err := s.CreateUser(ctx, "+15551819003")
	if err != nil {
		t.Fatalf("create other account: %v", err)
	}
	data := validStickerTGS(t)
	file := storedStickerDocument(t, s, blobs, owner.ID, data)
	h := fullChannelDispatcher(s)
	request := &tg.StickersCreateStickerSetRequest{
		UserID:    &tg.InputUserSelf{},
		Title:     "Cats",
		ShortName: "cats_a",
		Stickers: []tg.InputStickerSetItem{{
			Document: &tg.InputDocument{ID: file.ID, AccessHash: file.AccessHash},
			Emoji:    "🐈",
		}},
	}
	request.SetSoftware("web-import")
	body := dispatchChatMethod(t, h, owner.ID, "stickers.createStickerSet", request)
	var rpc mt.RPCError
	if err := rpc.Decode(&bin.Buffer{Buf: body}); err == nil {
		t.Fatalf("createStickerSet RPC error = %q, want messages.stickerSet", rpc.ErrorMessage)
	}
	var box tg.MessagesStickerSetBox
	if err := box.Decode(&bin.Buffer{Buf: body}); err != nil {
		t.Fatalf("decode createStickerSet: %v", err)
	}
	created, ok := box.StickerSet.(*tg.MessagesStickerSet)
	if !ok {
		t.Fatalf("createStickerSet response = %T, want *tg.MessagesStickerSet", box.StickerSet)
	}
	if created.Set.Title != "Cats" || created.Set.ShortName != "cats_a" || created.Set.Count != 1 || !created.Set.Creator {
		t.Fatalf("owner set = %+v, want Cats/cats_a, one sticker, creator", created.Set)
	}
	storedSet, err := s.UserStickerSetByID(ctx, created.Set.ID, created.Set.AccessHash)
	if err != nil || storedSet.Software != "web-import" {
		t.Fatalf("stored software = %q, err = %v, want web-import", storedSet.Software, err)
	}
	if len(created.Documents) != 1 {
		t.Fatalf("created documents = %d, want one", len(created.Documents))
	}
	document, ok := created.Documents[0].(*tg.Document)
	if !ok {
		t.Fatalf("created document = %T, want *tg.Document", created.Documents[0])
	}
	if document.ID != file.ID || document.AccessHash != file.AccessHash || document.MimeType != "application/x-tgsticker" {
		t.Fatalf("created document identity/MIME = %d/%d/%q", document.ID, document.AccessHash, document.MimeType)
	}
	setRef, foundStickerAttribute := false, false
	for _, attribute := range document.Attributes {
		if sticker, ok := attribute.(*tg.DocumentAttributeSticker); ok {
			setID, ok := sticker.Stickerset.(*tg.InputStickerSetID)
			setRef = ok && setID.ID == created.Set.ID && setID.AccessHash == created.Set.AccessHash && sticker.Alt == "🐈"
		}
		if _, ok := attribute.(*tg.DocumentAttributeAnimated); ok {
			foundStickerAttribute = true
		}
	}
	if !setRef || !foundStickerAttribute {
		t.Fatalf("created document attributes = %+v, want sticker set reference and animated marker", document.Attributes)
	}
	otherSet := getStickerSet(t, h, other.ID, &tg.InputStickerSetShortName{ShortName: "CATS_A"}, 0).Set
	if otherSet.ID != created.Set.ID || otherSet.Creator {
		t.Fatalf("other account set = %+v, want same set without creator flag", otherSet)
	}
}

func TestCreateStickerSetRejectsForeignFilesAndSharesShortNameNamespace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	blobs := testBlobs(t)
	s := openStickerStore(t, blobs)
	owner, err := s.CreateUser(ctx, "+15551819004")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	other, err := s.CreateUser(ctx, "+15551819005")
	if err != nil {
		t.Fatalf("create other account: %v", err)
	}
	ownedFile := storedStickerDocument(t, s, blobs, owner.ID, validStickerTGS(t))
	otherDispatcher := fullChannelDispatcher(s)
	foreignRequest := createStickerRequest("foreign_set", ownedFile, false, false)
	if got := stickerRPCError(t, dispatchChatMethod(t, otherDispatcher, other.ID, "stickers.createStickerSet", foreignRequest)); got != "STICKER_DOCUMENT_INVALID" {
		t.Fatalf("foreign document create = %q, want STICKER_DOCUMENT_INVALID", got)
	}

	created := createStickerSet(t, otherDispatcher, owner.ID, createStickerRequest("Cats_A", ownedFile, false, false))
	if created.Set.ShortName != "Cats_A" || !created.Set.Creator {
		t.Fatalf("created set metadata = %+v", created.Set)
	}
	if getStickerSet(t, otherDispatcher, other.ID, &tg.InputStickerSetShortName{ShortName: "cATS_a"}, 0).Set.ID != created.Set.ID {
		t.Fatal("case-insensitive short-name lookup returned another set")
	}
	if got := stickerRPCError(t, dispatchChatMethod(t, otherDispatcher, owner.ID, "stickers.createStickerSet", createStickerRequest("cats_a", ownedFile, false, false))); got != "SHORT_NAME_OCCUPIED" {
		t.Fatalf("case-insensitive duplicate name = %q, want SHORT_NAME_OCCUPIED", got)
	}
	if got := stickerRPCError(t, dispatchChatMethod(t, otherDispatcher, owner.ID, "stickers.createStickerSet", createStickerRequest("twemoji_reactions", ownedFile, false, false))); got != "SHORT_NAME_OCCUPIED" {
		t.Fatalf("catalogue namespace collision = %q, want SHORT_NAME_OCCUPIED", got)
	}
	tooMany := createStickerRequest("too_many", ownedFile, false, false)
	tooMany.Stickers = make([]tg.InputStickerSetItem, store.MaxUserStickerCount+1)
	for i := range tooMany.Stickers {
		tooMany.Stickers[i] = tg.InputStickerSetItem{
			Document: &tg.InputDocument{ID: ownedFile.ID, AccessHash: ownedFile.AccessHash}, Emoji: "⭐",
		}
	}
	if got := stickerRPCError(t, dispatchChatMethod(t, otherDispatcher, owner.ID, "stickers.createStickerSet", tooMany)); got != "STICKERS_TOO_MUCH" {
		t.Fatalf("oversized set = %q, want STICKERS_TOO_MUCH", got)
	}

	checkName := func(name string) []byte {
		return dispatchChatMethod(t, otherDispatcher, owner.ID, "stickers.checkShortName", &tg.StickersCheckShortNameRequest{ShortName: name})
	}
	var occupied tg.BoolFalse
	if err := occupied.Decode(&bin.Buffer{Buf: checkName("CATS_A")}); err != nil {
		t.Fatalf("decode occupied checkShortName: %v", err)
	}
	var available tg.BoolTrue
	if err := available.Decode(&bin.Buffer{Buf: checkName("wild_cats")}); err != nil {
		t.Fatalf("decode available checkShortName: %v", err)
	}
	var consecutiveUnderscores tg.BoolTrue
	if err := consecutiveUnderscores.Decode(&bin.Buffer{Buf: checkName("wild__cats")}); err != nil {
		t.Fatalf("decode checkShortName with consecutive underscores: %v", err)
	}
	if got := stickerRPCError(t, checkName("1invalid")); got != "SHORT_NAME_INVALID" {
		t.Fatalf("invalid checkShortName = %q, want SHORT_NAME_INVALID", got)
	}
	tooLongTitle := createStickerRequest("long_title", ownedFile, false, false)
	tooLongTitle.Title = strings.Repeat("a", 65)
	if got := stickerRPCError(t, dispatchChatMethod(t, otherDispatcher, owner.ID, "stickers.createStickerSet", tooLongTitle)); got != "TITLE_INVALID" {
		t.Fatalf("oversized title = %q, want TITLE_INVALID", got)
	}
	suggestedBody := dispatchChatMethod(t, otherDispatcher, owner.ID, "stickers.suggestShortName", &tg.StickersSuggestShortNameRequest{Title: "Wild Cats!"})
	var suggested tg.StickersSuggestedShortName
	if err := suggested.Decode(&bin.Buffer{Buf: suggestedBody}); err != nil {
		t.Fatalf("decode suggestShortName: %v", err)
	}
	if !validSuggestedName(suggested.ShortName) {
		t.Fatalf("suggestShortName = %q, want a valid name", suggested.ShortName)
	}
	var suggestedAvailable tg.BoolTrue
	if err := suggestedAvailable.Decode(&bin.Buffer{Buf: checkName(suggested.ShortName)}); err != nil {
		t.Fatalf("suggested name is not available: %q: %v", suggested.ShortName, err)
	}
	badSoftware := createStickerRequest("bot_token", ownedFile, false, false)
	badSoftware.SetSoftware("123456:ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmno")
	if got := stickerRPCError(t, dispatchChatMethod(t, otherDispatcher, owner.ID, "stickers.createStickerSet", badSoftware)); got != "STICKERSET_INVALID" {
		t.Fatalf("bot-token software value = %q, want STICKERSET_INVALID", got)
	}
	longSoftware := createStickerRequest("long_software", ownedFile, false, false)
	longSoftware.SetSoftware(strings.Repeat("x", 257))
	if got := stickerRPCError(t, dispatchChatMethod(t, otherDispatcher, owner.ID, "stickers.createStickerSet", longSoftware)); got != "STICKERSET_INVALID" {
		t.Fatalf("oversized software = %q, want STICKERSET_INVALID", got)
	}

	badFile := storedStickerDocument(t, s, blobs, owner.ID, []byte("not a sticker"))
	if got := stickerRPCError(t, dispatchChatMethod(t, otherDispatcher, owner.ID, "stickers.createStickerSet", createStickerRequest("bad_bytes", badFile, false, false))); got != "STICKER_FILE_INVALID" {
		t.Fatalf("invalid stored bytes = %q, want STICKER_FILE_INVALID", got)
	}
	oversized := storedStickerDocument(t, s, blobs, owner.ID, bytes.Repeat([]byte{0}, store.MaxStoredStickerBytes+1))
	if got := stickerRPCError(t, dispatchChatMethod(t, otherDispatcher, owner.ID, "stickers.createStickerSet", createStickerRequest("oversized_file", oversized, false, false))); got != "STICKER_FILE_SIZE" {
		t.Fatalf("oversized stored file = %q, want STICKER_FILE_SIZE", got)
	}
	if got := stickerRPCError(t, dispatchChatMethod(t, otherDispatcher, owner.ID, "stickers.createStickerSet", createStickerRequest("reused_file", ownedFile, false, false))); got != "STICKER_DOCUMENT_INVALID" {
		t.Fatalf("second-set file use = %q, want STICKER_DOCUMENT_INVALID", got)
	}
	if _, err := s.UserStickerSetByShortName(ctx, "bad_bytes"); !errors.Is(err, store.ErrStickerSetNotFound) {
		t.Fatalf("invalid-byte create left a set: %v", err)
	}
}

func TestUserStickerSetCreationAndInstallLimits(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	blobs := testBlobs(t)
	dsn := pgtest.DSN(t)
	s := openStickerStore(t, blobs, dsn)
	owner, err := s.CreateUser(ctx, "+15551819010")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	creator, err := s.CreateUser(ctx, "+15551819011")
	if err != nil {
		t.Fatalf("create second account: %v", err)
	}
	ownerFile := storedStickerDocument(t, s, blobs, owner.ID, validStickerTGS(t))
	creatorFile := storedStickerDocument(t, s, blobs, creator.ID, validStickerTGS(t))
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect limit fixture: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close limit fixture connection: %v", err)
		}
	})
	if _, err := conn.Exec(ctx, `
		INSERT INTO user_created_sticker_sets (access_hash, creator_id, title, short_name, kind, item_count)
		SELECT 7000000000000 + n, $1, 'Seeded', 'seed_' || n::text, 'regular', 1
		FROM generate_series(1, $2) n`, owner.ID, store.MaxUserStickerSetsPerAccount); err != nil {
		t.Fatalf("seed created-set limit: %v", err)
	}
	h := fullChannelDispatcher(s)
	if got := stickerRPCError(t, dispatchChatMethod(t, h, owner.ID, "stickers.createStickerSet", createStickerRequest("one_too_many", ownerFile, false, false))); got != "STICKERS_TOO_MUCH" {
		t.Fatalf("101st user-created set = %q, want STICKERS_TOO_MUCH", got)
	}
	if _, err := s.UserStickerSetByShortName(ctx, "one_too_many"); !errors.Is(err, store.ErrStickerSetNotFound) {
		t.Fatalf("over-limit create left a set: %v", err)
	}
	set := createStickerSet(t, h, creator.ID, createStickerRequest("install_limit_set", creatorFile, false, false))
	if _, err := conn.Exec(ctx, `
		INSERT INTO user_sticker_sets (user_id, set_id)
		SELECT $1, -800000 - n
		FROM generate_series(1, $2) n`, owner.ID, store.MaxInstalledStickerSets); err != nil {
		t.Fatalf("seed installed-set limit: %v", err)
	}
	if got := installStickerSet(t, h, owner.ID, &tg.InputStickerSetID{ID: set.Set.ID, AccessHash: set.Set.AccessHash}); got != "STICKERS_TOO_MUCH" {
		t.Fatalf("201st installed set = %q, want STICKERS_TOO_MUCH", got)
	}
}

type stickerQueryCounter struct{ n atomic.Int64 }

func (c *stickerQueryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.n.Add(1)
	return ctx
}

func (*stickerQueryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestSuggestStickerSetChecksCandidatesInOneQuery(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	tracer := &stickerQueryCounter{}
	s, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithoutBlobStore(), store.WithQueryTracer(tracer))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	owner, err := s.CreateUser(ctx, "+15551819999")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to seed occupied names: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close name seed connection: %v", err)
		}
	})
	if _, err := conn.Exec(ctx, `
		INSERT INTO user_created_sticker_sets (access_hash, creator_id, title, short_name, kind, item_count)
		SELECT 7000000000000 + n, $1, 'Occupied',
		       upper('wild_cats' || CASE WHEN n = 0 THEN '' ELSE '_' || n::text END),
		       'regular', 1
		FROM generate_series(0, 998) AS n`, owner.ID); err != nil {
		t.Fatalf("seed occupied names: %v", err)
	}

	h := fullChannelDispatcher(s)
	tracer.n.Store(0)
	body := dispatchChatMethod(t, h, owner.ID, "stickers.suggestShortName", &tg.StickersSuggestShortNameRequest{Title: "Wild Cats!"})
	var suggested tg.StickersSuggestedShortName
	if err := suggested.Decode(&bin.Buffer{Buf: body}); err != nil {
		t.Fatalf("decode suggestShortName: %v", err)
	}
	if suggested.ShortName != "wild_cats_999" {
		t.Fatalf("suggestShortName = %q, want first free candidate wild_cats_999", suggested.ShortName)
	}
	if n := tracer.n.Load(); n != 2 {
		t.Fatalf("suggestShortName made %d database queries with 999 occupied candidates, want 2 (catalogue and batched candidate lookup)", n)
	}
}

func TestInstalledUserStickerSetGettersUseBoundedQueries(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	tracer := &stickerQueryCounter{}
	s, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithoutBlobStore(), store.WithQueryTracer(tracer))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	owner, err := s.CreateUser(ctx, "+15551819012")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to seed sticker sets: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close sticker seed connection: %v", err)
		}
	})
	if _, err := conn.Exec(ctx, `
		WITH seeded AS (
			INSERT INTO user_created_sticker_sets (access_hash, creator_id, title, short_name, kind, item_count, set_hash)
			SELECT 7000000000000 + n, $1, 'Batch set', 'batch_' || n::text,
			       CASE WHEN n <= $2 / 2 THEN 'regular' ELSE 'emojis' END, 12, n::int
			FROM generate_series(1, $2) AS n
			RETURNING id
		)
		INSERT INTO user_sticker_sets (user_id, set_id)
		SELECT $1, id FROM seeded`, owner.ID, store.MaxInstalledStickerSets); err != nil {
		t.Fatalf("seed installed sticker sets: %v", err)
	}
	h := fullChannelDispatcher(s)
	for _, test := range []struct {
		name string
		get  func() *tg.MessagesAllStickers
		want int
	}{
		{name: "regular", get: func() *tg.MessagesAllStickers { return getAllStickerSets(t, h, owner.ID, 0) }, want: store.MaxInstalledStickerSets / 2},
		{name: "emoji", get: func() *tg.MessagesAllStickers { return getEmojiStickerSets(t, h, owner.ID, 0) }, want: store.MaxInstalledStickerSets / 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			tracer.n.Store(0)
			sets := test.get()
			if len(sets.Sets) != test.want {
				t.Fatalf("returned %d sets, want %d", len(sets.Sets), test.want)
			}
			for _, set := range sets.Sets {
				if set.Count != 12 || set.Hash != int(set.ID) {
					t.Fatalf("summary for set %d has count %d and hash %d, want count 12 and stored hash %d", set.ID, set.Count, set.Hash, set.ID)
				}
			}
			if n := tracer.n.Load(); n != 2 {
				t.Fatalf("getter made %d database queries at the %d-set install cap, want 2", n, store.MaxInstalledStickerSets)
			}
		})
	}
}

func TestUserStickerSetListsDownloadsSendsAndForwards(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	blobs := testBlobs(t)
	dsn := pgtest.DSN(t)
	s := openStickerStore(t, blobs, dsn)
	owner, err := s.CreateUser(ctx, "+15551819006")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	installer, err := s.CreateUser(ctx, "+15551819007")
	if err != nil {
		t.Fatalf("create installer: %v", err)
	}
	receiver, err := s.CreateUser(ctx, "+15551819008")
	if err != nil {
		t.Fatalf("create receiver: %v", err)
	}
	regularBytes := validStickerTGS(t)
	emojiBytes := validStickerTGSSize(t, 100, 100)
	regularFile := storedStickerDocument(t, s, blobs, owner.ID, regularBytes)
	emojiFile := storedStickerDocument(t, s, blobs, owner.ID, emojiBytes)
	h := fullChannelDispatcher(s)
	regular := createStickerSet(t, h, owner.ID, createStickerRequest("regular_cats", regularFile, false, false))
	emojiRequest := createStickerRequest("emoji_cats", emojiFile, false, true)
	emojiRequest.TextColor = true
	emoji := createStickerSet(t, h, owner.ID, emojiRequest)
	emojiDoc, ok := emoji.Documents[0].(*tg.Document)
	if !ok {
		t.Fatalf("custom emoji document = %T, want *tg.Document", emoji.Documents[0])
	}
	var customEmoji *tg.DocumentAttributeCustomEmoji
	for _, attribute := range emojiDoc.Attributes {
		if value, ok := attribute.(*tg.DocumentAttributeCustomEmoji); ok {
			customEmoji = value
		}
	}
	if customEmoji == nil || !customEmoji.TextColor || customEmoji.Alt != "⭐" {
		t.Fatalf("custom emoji attributes = %+v, want text-color custom emoji metadata", emojiDoc.Attributes)
	}

	initialAll := getAllStickerSets(t, h, installer.ID, 0)
	if len(initialAll.Sets) != 0 {
		t.Fatalf("initial getAllStickers = %+v, want empty", initialAll.Sets)
	}
	initialEmoji := getEmojiStickerSets(t, h, installer.ID, 0)
	if len(initialEmoji.Sets) != 0 {
		t.Fatalf("initial getEmojiStickers = %+v, want empty", initialEmoji.Sets)
	}
	wrongHashRef := &tg.InputStickerSetID{ID: regular.Set.ID, AccessHash: regular.Set.AccessHash + 1}
	if got := stickerRPCError(t, dispatchChatMethod(t, h, installer.ID, "messages.getStickerSet", &tg.MessagesGetStickerSetRequest{Stickerset: wrongHashRef})); got != "STICKERSET_INVALID" {
		t.Fatalf("getStickerSet with correct ID and wrong hash = %q, want STICKERSET_INVALID", got)
	}
	if got := installStickerSet(t, h, installer.ID, wrongHashRef); got != "STICKERSET_INVALID" {
		t.Fatalf("installStickerSet with correct ID and wrong hash = %q, want STICKERSET_INVALID", got)
	}
	if got := installStickerSet(t, h, installer.ID, &tg.InputStickerSetID{ID: regular.Set.ID, AccessHash: regular.Set.AccessHash}); got != "success" {
		t.Fatalf("install regular user sticker set = %q", got)
	}
	if got := installStickerSet(t, h, installer.ID, &tg.InputStickerSetShortName{ShortName: "EMOJI_CATS"}); got != "success" {
		t.Fatalf("install custom emoji set = %q", got)
	}
	all := getAllStickerSets(t, h, installer.ID, initialAll.Hash)
	if len(all.Sets) != 1 || all.Sets[0].ID != regular.Set.ID || all.Hash == initialAll.Hash {
		t.Fatalf("getAllStickers after install = %+v, want regular set and changed hash", all)
	}
	if all.Sets[0].Count != regular.Set.Count || all.Sets[0].Hash != regular.Set.Hash {
		t.Fatalf("getAllStickers summary = count %d hash %d, want created set count %d hash %d", all.Sets[0].Count, all.Sets[0].Hash, regular.Set.Count, regular.Set.Hash)
	}
	emojis := getEmojiStickerSets(t, h, installer.ID, initialEmoji.Hash)
	if len(emojis.Sets) != 1 || emojis.Sets[0].ID != emoji.Set.ID || emojis.Hash == initialEmoji.Hash {
		t.Fatalf("getEmojiStickers after install = %+v, want custom emoji set and changed hash", emojis)
	}
	if emojis.Sets[0].Count != emoji.Set.Count || emojis.Sets[0].Hash != emoji.Set.Hash {
		t.Fatalf("getEmojiStickers summary = count %d hash %d, want created set count %d hash %d", emojis.Sets[0].Count, emojis.Sets[0].Hash, emoji.Set.Count, emoji.Set.Hash)
	}
	if got := getStickerSet(t, h, installer.ID, &tg.InputStickerSetID{ID: regular.Set.ID, AccessHash: regular.Set.AccessHash}, 0).Set.Creator; got {
		t.Fatal("non-owner getStickerSet set the creator flag")
	}

	regularDoc, ok := regular.Documents[0].(*tg.Document)
	if !ok {
		t.Fatalf("regular sticker document = %T, want *tg.Document", regular.Documents[0])
	}
	downloaded, err := api.GetFileForTest(s, installer.ID, blobs, &tg.UploadGetFileRequest{
		Location: &tg.InputDocumentFileLocation{ID: regularDoc.ID, AccessHash: regularDoc.AccessHash, FileReference: regularDoc.FileReference},
		Limit:    1024,
	})
	if err != nil {
		t.Fatalf("download live user sticker: %v", err)
	}
	file, ok := downloaded.(*tg.UploadFile)
	if !ok {
		t.Fatalf("downloaded sticker = %T, want *tg.UploadFile", downloaded)
	}
	if !bytes.Equal(file.Bytes, regularBytes) {
		t.Fatalf("downloaded sticker bytes = %d, want original %d bytes", len(file.Bytes), len(regularBytes))
	}
	_, wrongHashErr := api.GetFileForTest(s, installer.ID, blobs, &tg.UploadGetFileRequest{
		Location: &tg.InputDocumentFileLocation{ID: regularDoc.ID, AccessHash: regularDoc.AccessHash + 1}, Limit: 1024,
	})
	if got := rpcMessage(t, wrongHashErr); got != "LOCATION_INVALID" {
		t.Fatalf("download with wrong sticker hash = %q, want LOCATION_INVALID", got)
	}

	_, err = api.SendMediaForTest(s, installer.ID, blobs, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer:    api.InputPeerUser(installer.ID, owner.ID),
		Media:   &tg.InputMediaDocument{ID: &tg.InputDocument{ID: regularDoc.ID, AccessHash: regularDoc.AccessHash}},
		Message: "user sticker", RandomID: 5518162401,
	})
	if err != nil {
		t.Fatalf("send user sticker: %v", err)
	}
	ownerHistory, err := api.GetHistoryForTest(s, owner.ID, &tg.MessagesGetHistoryRequest{
		Peer: api.InputPeerUser(owner.ID, installer.ID), Limit: 10,
	})
	if err != nil {
		t.Fatalf("read user sticker message: %v", err)
	}
	messages, ok := ownerHistory.(*tg.MessagesMessages)
	if !ok || len(messages.Messages) == 0 {
		t.Fatalf("recipient history = %T/%v, want delivered sticker message", ownerHistory, ownerHistory)
	}
	sent, ok := messages.Messages[0].(*tg.Message)
	if !ok {
		t.Fatalf("delivered message = %T, want *tg.Message", messages.Messages[0])
	}
	media, ok := sent.Media.(*tg.MessageMediaDocument)
	if !ok {
		t.Fatalf("delivered media = %T, want sticker document", sent.Media)
	}
	messageDoc, ok := media.Document.(*tg.Document)
	if !ok || messageDoc.ID != regularDoc.ID || messageDoc.MimeType != "application/x-tgsticker" {
		t.Fatalf("delivered user sticker document = %#v", media.Document)
	}
	recipientDownload, err := api.GetFileForTest(s, owner.ID, blobs, &tg.UploadGetFileRequest{
		Location: &tg.InputDocumentFileLocation{ID: messageDoc.ID, AccessHash: messageDoc.AccessHash}, Limit: 1024,
	})
	if err != nil {
		t.Fatalf("message recipient downloads user sticker: %v", err)
	}
	recipientFile, ok := recipientDownload.(*tg.UploadFile)
	if !ok || !bytes.Equal(recipientFile.Bytes, regularBytes) {
		t.Fatalf("recipient sticker download = %T, want original %d-byte upload", recipientDownload, len(regularBytes))
	}

	senderHistory, err := s.History(ctx, installer.ID, store.PeerTypeUser, owner.ID, 0, 10)
	if err != nil || len(senderHistory) == 0 {
		t.Fatalf("read sender sticker history: %v, %v", senderHistory, err)
	}
	_, err = api.ForwardMessagesForTest(s, installer.ID, &tg.MessagesForwardMessagesRequest{
		FromPeer: api.InputPeerUser(installer.ID, owner.ID), ID: []int{int(senderHistory[0].LocalID)},
		ToPeer: api.InputPeerUser(installer.ID, receiver.ID), RandomID: []int64{5518162402},
	})
	if err != nil {
		t.Fatalf("forward user sticker: %v", err)
	}
	receiverHistory, err := api.GetHistoryForTest(s, receiver.ID, &tg.MessagesGetHistoryRequest{
		Peer: api.InputPeerUser(receiver.ID, installer.ID), Limit: 10,
	})
	if err != nil {
		t.Fatalf("read forwarded sticker: %v", err)
	}
	forwardedMessages, ok := receiverHistory.(*tg.MessagesMessages)
	if !ok || len(forwardedMessages.Messages) == 0 {
		t.Fatalf("forward recipient history = %T/%v", receiverHistory, receiverHistory)
	}
	forwarded, ok := forwardedMessages.Messages[0].(*tg.Message)
	if !ok {
		t.Fatalf("forwarded message = %T", forwardedMessages.Messages[0])
	}
	forwardMedia, ok := forwarded.Media.(*tg.MessageMediaDocument)
	if !ok {
		t.Fatalf("forwarded media = %T", forwarded.Media)
	}
	forwardedDocument, ok := forwardMedia.Document.(*tg.Document)
	if !ok {
		t.Fatalf("forwarded document = %T, want *tg.Document", forwardMedia.Document)
	}
	if got := forwardedDocument.ID; got != regularDoc.ID {
		t.Fatalf("forwarded sticker id = %d, want %d", got, regularDoc.ID)
	}
	if !uninstallStickerSet(t, h, installer.ID, &tg.InputStickerSetID{ID: regular.Set.ID, AccessHash: regular.Set.AccessHash}) {
		t.Fatal("uninstall regular user sticker set returned false")
	}
	if !uninstallStickerSet(t, h, installer.ID, &tg.InputStickerSetShortName{ShortName: "emoji_cats"}) {
		t.Fatal("uninstall custom emoji set returned false")
	}
	clearedAll := getAllStickerSets(t, h, installer.ID, all.Hash)
	if len(clearedAll.Sets) != 0 || clearedAll.Hash != initialAll.Hash {
		t.Fatalf("getAllStickers after uninstall = %+v, want empty with initial hash %d", clearedAll, initialAll.Hash)
	}
	clearedEmojis := getEmojiStickerSets(t, h, installer.ID, emojis.Hash)
	if len(clearedEmojis.Sets) != 0 || clearedEmojis.Hash != initialEmoji.Hash {
		t.Fatalf("getEmojiStickers after uninstall = %+v, want empty with initial hash %d", clearedEmojis, initialEmoji.Hash)
	}

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect for set liveness change: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close set liveness connection: %v", err)
		}
	})
	if _, err = conn.Exec(ctx, `UPDATE user_created_sticker_sets SET is_live = false WHERE id = $1`, regular.Set.ID); err != nil {
		t.Fatalf("disable user set: %v", err)
	}
	inactiveSetDownload, inactiveSetErr := api.GetFileForTest(s, installer.ID, blobs, &tg.UploadGetFileRequest{
		Location: &tg.InputDocumentFileLocation{ID: regularDoc.ID, AccessHash: regularDoc.AccessHash}, Limit: 1024,
	})
	if inactiveSetErr != nil {
		t.Fatalf("download sticker from prior message after set deactivation: %v", inactiveSetErr)
	}
	inactiveSetFile, ok := inactiveSetDownload.(*tg.UploadFile)
	if !ok || !bytes.Equal(inactiveSetFile.Bytes, validStickerTGS(t)) {
		t.Fatalf("download prior message sticker after set deactivation = %T, want original bytes", inactiveSetDownload)
	}
	_, inactiveSendErr := api.SendMediaForTest(s, installer.ID, blobs, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerUser(installer.ID, receiver.ID),
		Media: &tg.InputMediaDocument{ID: &tg.InputDocument{
			ID: regularDoc.ID, AccessHash: regularDoc.AccessHash,
		}},
		Message: "inactive sticker", RandomID: 5518162403,
	})
	if got := rpcMessage(t, inactiveSendErr); got != "MEDIA_INVALID" {
		t.Fatalf("send inactive user sticker = %q, want MEDIA_INVALID", got)
	}
	_, inactiveForwardErr := api.ForwardMessagesForTest(s, installer.ID, &tg.MessagesForwardMessagesRequest{
		FromPeer: api.InputPeerUser(installer.ID, owner.ID), ID: []int{int(senderHistory[0].LocalID)},
		ToPeer: api.InputPeerUser(installer.ID, receiver.ID), RandomID: []int64{5518162404},
	})
	if inactiveForwardErr == nil {
		t.Fatal("forwarding an inactive user sticker succeeded")
	}
}

func TestHistoryKeepsNormalFileMediaSeparateFromStickerMedia(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	blobs := newBlobs(t)
	s := openStickerStore(t, blobs)
	owner, err := s.CreateUser(ctx, "+15551819029")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	recipient, err := s.CreateUser(ctx, "+15551819030")
	if err != nil {
		t.Fatalf("create recipient: %v", err)
	}

	const uploadID = int64(902901)
	saveParts(t, s, owner.ID, uploadID, validStickerTGS(t))
	normalSend, err := api.SendMediaForTest(s, owner.ID, blobs, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer:    api.InputPeerUser(owner.ID, recipient.ID),
		Media:   uploadedDocument(uploadID, 1, "same-file.tgs", "application/octet-stream"),
		Message: "normal document", RandomID: 902902,
	})
	if err != nil {
		t.Fatalf("send normal document: %v", err)
	}
	normalDocument := documentOf(t, normalSend)
	files, err := s.FilesByIDs(ctx, []int64{normalDocument.ID})
	if err != nil {
		t.Fatalf("load uploaded file: %v", err)
	}
	file, ok := files[normalDocument.ID]
	if !ok {
		t.Fatalf("uploaded file %d is missing", normalDocument.ID)
	}
	createStickerSet(t, fullChannelDispatcher(s), owner.ID, createStickerRequest("same_file", file, false, false))

	_, err = api.SendMediaForTest(s, owner.ID, blobs, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer:    api.InputPeerUser(owner.ID, recipient.ID),
		Media:   &tg.InputMediaDocument{ID: &tg.InputDocument{ID: file.ID, AccessHash: file.AccessHash}},
		Message: "sticker message", RandomID: 902903,
	})
	if err != nil {
		t.Fatalf("send sticker message: %v", err)
	}

	history, err := api.GetHistoryForTest(s, recipient.ID, &tg.MessagesGetHistoryRequest{
		Peer: api.InputPeerUser(recipient.ID, owner.ID), Limit: 10,
	})
	if err != nil {
		t.Fatalf("get history: %v", err)
	}
	result, ok := history.(*tg.MessagesMessages)
	if !ok {
		t.Fatalf("history = %T, want *tg.MessagesMessages", history)
	}
	mediaByText := make(map[string]*tg.Document, len(result.Messages))
	for _, class := range result.Messages {
		message, ok := class.(*tg.Message)
		if !ok {
			continue
		}
		media, ok := message.Media.(*tg.MessageMediaDocument)
		if !ok {
			t.Fatalf("%q media = %T, want *tg.MessageMediaDocument", message.Message, message.Media)
		}
		document, ok := media.Document.(*tg.Document)
		if !ok {
			t.Fatalf("%q document = %T, want *tg.Document", message.Message, media.Document)
		}
		mediaByText[message.Message] = document
	}

	if got := mediaByText["normal document"]; got == nil || got.MimeType != "application/octet-stream" {
		t.Fatalf("normal document media = %+v, want original generic document", got)
	}
	sticker := mediaByText["sticker message"]
	if sticker == nil || sticker.MimeType != "application/x-tgsticker" {
		t.Fatalf("sticker media = %+v, want TGS sticker document", sticker)
	}
	hasStickerAttribute := false
	for _, attribute := range sticker.Attributes {
		if _, ok := attribute.(*tg.DocumentAttributeSticker); ok {
			hasStickerAttribute = true
			break
		}
	}
	if !hasStickerAttribute {
		t.Fatalf("sticker attributes = %+v, want sticker attribute", sticker.Attributes)
	}

	assertStickerMessage := func(path string, class tg.MessageClass) {
		t.Helper()
		message, ok := class.(*tg.Message)
		if !ok {
			t.Fatalf("%s message = %T, want *tg.Message", path, class)
		}
		media, ok := message.Media.(*tg.MessageMediaDocument)
		if !ok {
			t.Fatalf("%s media = %T, want *tg.MessageMediaDocument", path, message.Media)
		}
		document, ok := media.Document.(*tg.Document)
		if !ok || document.MimeType != "application/x-tgsticker" {
			t.Fatalf("%s document = %#v, want TGS sticker document", path, media.Document)
		}
		for _, attribute := range document.Attributes {
			if _, ok := attribute.(*tg.DocumentAttributeSticker); ok {
				return
			}
		}
		t.Fatalf("%s document attributes = %+v, want sticker attribute", path, document.Attributes)
	}

	recipientHistory, err := s.History(ctx, recipient.ID, store.PeerTypeUser, owner.ID, 0, 10)
	if err != nil {
		t.Fatalf("read recipient history: %v", err)
	}
	var stickerLocalID int64
	for _, message := range recipientHistory {
		if message.Text == "sticker message" {
			stickerLocalID = message.LocalID
			break
		}
	}
	if stickerLocalID == 0 {
		t.Fatal("sticker message missing from recipient history")
	}

	byID, err := api.GetMessagesForTest(s, recipient.ID, &tg.MessagesGetMessagesRequest{
		ID: []tg.InputMessageClass{&tg.InputMessageID{ID: int(stickerLocalID)}},
	})
	if err != nil {
		t.Fatalf("get sticker message by id: %v", err)
	}
	byIDResult, ok := byID.(*tg.MessagesMessages)
	if !ok {
		t.Fatalf("getMessages result = %T, want *tg.MessagesMessages", byID)
	}
	if len(byIDResult.Messages) != 1 {
		t.Fatalf("getMessages returned %d messages, want one", len(byIDResult.Messages))
	}
	assertStickerMessage("getMessages", byIDResult.Messages[0])

	peerDialogs, err := api.GetPeerDialogsForTest(s, recipient.ID, &tg.MessagesGetPeerDialogsRequest{
		Peers: []tg.InputDialogPeerClass{&tg.InputDialogPeer{Peer: api.InputPeerUser(recipient.ID, owner.ID)}},
	})
	if err != nil {
		t.Fatalf("get peer dialogs with sticker message: %v", err)
	}
	peerDialogsResult, ok := peerDialogs.(*tg.MessagesPeerDialogs)
	if !ok {
		t.Fatalf("getPeerDialogs result = %T, want *tg.MessagesPeerDialogs", peerDialogs)
	}
	if len(peerDialogsResult.Messages) != 1 {
		t.Fatalf("getPeerDialogs returned %d messages, want one", len(peerDialogsResult.Messages))
	}
	assertStickerMessage("getPeerDialogs", peerDialogsResult.Messages[0])
}

func TestGetPeerDialogsHydratesUserStickerChannelPost(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	blobs := newBlobs(t)
	s := openStickerStore(t, blobs)
	owner, err := s.CreateUser(ctx, "+15551819031")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	channel, err := s.CreateChannel(ctx, owner.ID, "Sticker channel", "", true)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}

	file := storedStickerDocument(t, s, blobs, owner.ID, validStickerTGS(t))
	createStickerSet(t, fullChannelDispatcher(s), owner.ID, createStickerRequest("channel_stickers", file, false, false))
	_, err = api.SendMediaForTest(s, owner.ID, blobs, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer:    api.InputPeerChannel(owner.ID, channel.ID),
		Media:   &tg.InputMediaDocument{ID: &tg.InputDocument{ID: file.ID, AccessHash: file.AccessHash}},
		Message: "channel user sticker", RandomID: 902904,
	})
	if err != nil {
		t.Fatalf("send user sticker to channel: %v", err)
	}
	posts, err := s.ChannelHistory(ctx, channel.ID, 0, 10)
	if err != nil {
		t.Fatalf("read channel history: %v", err)
	}
	var foundStickerPost bool
	for _, post := range posts {
		if post.Message == "channel user sticker" && post.StickerDocumentID != nil && *post.StickerDocumentID == file.ID {
			foundStickerPost = true
			break
		}
	}
	if !foundStickerPost {
		t.Fatalf("channel history = %+v, want sticker document %d", posts, file.ID)
	}

	result, err := api.GetPeerDialogsForTest(s, owner.ID, &tg.MessagesGetPeerDialogsRequest{
		Peers: []tg.InputDialogPeerClass{&tg.InputDialogPeer{Peer: api.InputPeerChannel(owner.ID, channel.ID)}},
	})
	if err != nil {
		t.Fatalf("get channel peer dialog: %v", err)
	}
	peerDialogs, ok := result.(*tg.MessagesPeerDialogs)
	if !ok {
		t.Fatalf("getPeerDialogs result = %T, want *tg.MessagesPeerDialogs", result)
	}
	if len(peerDialogs.Messages) != 1 {
		t.Fatalf("getPeerDialogs returned %d messages, want one", len(peerDialogs.Messages))
	}
	message, ok := peerDialogs.Messages[0].(*tg.Message)
	if !ok {
		t.Fatalf("channel top message = %T, want *tg.Message", peerDialogs.Messages[0])
	}
	media, ok := message.Media.(*tg.MessageMediaDocument)
	if !ok {
		t.Fatalf("channel top message media = %T, want *tg.MessageMediaDocument", message.Media)
	}
	document, ok := media.Document.(*tg.Document)
	if !ok || document.ID != file.ID || document.MimeType != "application/x-tgsticker" {
		t.Fatalf("channel top message document = %#v, want user TGS sticker %d", media.Document, file.ID)
	}
	for _, attribute := range document.Attributes {
		if _, ok := attribute.(*tg.DocumentAttributeSticker); ok {
			return
		}
	}
	t.Fatalf("channel top message document attributes = %+v, want sticker attribute", document.Attributes)
}

func createStickerRequest(shortName string, file store.File, masks, emojis bool) *tg.StickersCreateStickerSetRequest {
	return &tg.StickersCreateStickerSetRequest{
		UserID: &tg.InputUserSelf{}, Title: "Generated stickers", ShortName: shortName,
		Masks: masks, Emojis: emojis,
		Stickers: []tg.InputStickerSetItem{{Document: &tg.InputDocument{ID: file.ID, AccessHash: file.AccessHash}, Emoji: "⭐"}},
	}
}

func createStickerSet(t *testing.T, h mtproto.Handler, userID int64, req *tg.StickersCreateStickerSetRequest) *tg.MessagesStickerSet {
	t.Helper()
	body := dispatchChatMethod(t, h, userID, "stickers.createStickerSet", req)
	var rpc mt.RPCError
	if err := rpc.Decode(&bin.Buffer{Buf: body}); err == nil {
		t.Fatalf("createStickerSet RPC error = %q", rpc.ErrorMessage)
	}
	var box tg.MessagesStickerSetBox
	if err := box.Decode(&bin.Buffer{Buf: body}); err != nil {
		t.Fatalf("decode createStickerSet: %v", err)
	}
	set, ok := box.StickerSet.(*tg.MessagesStickerSet)
	if !ok {
		t.Fatalf("createStickerSet = %T, want *tg.MessagesStickerSet", box.StickerSet)
	}
	return set
}

func stickerRPCError(t *testing.T, body []byte) string {
	t.Helper()
	var rpc mt.RPCError
	if err := rpc.Decode(&bin.Buffer{Buf: body}); err != nil {
		t.Fatalf("decode sticker RPC error: %v", err)
	}
	return rpc.ErrorMessage
}

func validSuggestedName(name string) bool {
	if len(name) == 0 || len(name) > 64 || name[0] < 'a' || name[0] > 'z' {
		return false
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}

func getEmojiStickerSets(t *testing.T, h mtproto.Handler, userID, hash int64) *tg.MessagesAllStickers {
	t.Helper()
	body := dispatchChatMethod(t, h, userID, "messages.getEmojiStickers", &tg.MessagesGetEmojiStickersRequest{Hash: hash})
	var result tg.MessagesAllStickers
	if err := result.Decode(&bin.Buffer{Buf: body}); err != nil {
		t.Fatalf("decode getEmojiStickers: %v", err)
	}
	return &result
}

func openStickerStore(t *testing.T, blobs blob.Store, dsns ...string) *store.Store {
	t.Helper()
	dsn := pgtest.DSN(t)
	if len(dsns) > 0 {
		dsn = dsns[0]
	}
	s, err := store.Open(context.Background(), dsn, pgtest.EncKey(), store.WithBlobStore(blobs))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return s
}

func storedStickerDocument(t *testing.T, s *store.Store, blobs blob.Store, ownerID int64, data []byte) store.File {
	t.Helper()
	file, err := s.AllocateAndCompleteFile(context.Background(), ownerID, int64(len(data)), "text/plain", "upload.txt", api.TestMaxUserStorageBytes, []string{}, func(file store.File) error {
		written, err := blobs.Put(context.Background(), blob.Key(file.ID), bytes.NewReader(data))
		if err != nil {
			return err
		}
		if written != int64(len(data)) {
			t.Fatalf("stored bytes = %d, want %d", written, len(data))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("store sticker document: %v", err)
	}
	return file
}

func validStickerTGS(t *testing.T) []byte {
	t.Helper()
	return validStickerTGSSize(t, 512, 512)
}

func validStickerTGSSize(t *testing.T, width, height int) []byte {
	t.Helper()
	var body bytes.Buffer
	writer := gzip.NewWriter(&body)
	if _, err := io.WriteString(writer, fmt.Sprintf(`{"tgs":1,"w":%d,"h":%d,"fr":30,"ip":0,"op":60}`, width, height)); err != nil {
		t.Fatalf("write TGS fixture: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close TGS fixture: %v", err)
	}
	return body.Bytes()
}

func TestAddStickerToSetEnforcesCreationCountLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	blobs := testBlobs(t)
	s := openStickerStore(t, blobs)
	owner, err := s.CreateUser(ctx, "+15551819121")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	h := fullChannelDispatcher(s)
	files := make([]store.File, store.MaxUserStickerCount+1)
	for i := range files {
		files[i] = storedStickerDocument(t, s, blobs, owner.ID, validStickerTGS(t))
	}
	request := createStickerRequest("item_limit_set", files[0], false, false)
	for _, file := range files[1:store.MaxUserStickerCount] {
		request.Stickers = append(request.Stickers, tg.InputStickerSetItem{
			Document: &tg.InputDocument{ID: file.ID, AccessHash: file.AccessHash}, Emoji: "⭐",
		})
	}
	created := createStickerSet(t, h, owner.ID, request)
	if created.Set.Count != store.MaxUserStickerCount {
		t.Fatalf("created set count = %d, want %d", created.Set.Count, store.MaxUserStickerCount)
	}
	ref := &tg.InputStickerSetID{ID: created.Set.ID, AccessHash: created.Set.AccessHash}
	if got := stickerRPCError(t, dispatchChatMethod(t, h, owner.ID, "stickers.addStickerToSet", &tg.StickersAddStickerToSetRequest{
		Stickerset: ref,
		Sticker: tg.InputStickerSetItem{
			Document: &tg.InputDocument{ID: files[store.MaxUserStickerCount].ID, AccessHash: files[store.MaxUserStickerCount].AccessHash},
			Emoji:    "⭐",
		},
	})); got != "STICKERS_TOO_MUCH" {
		t.Fatalf("add beyond creation count limit = %q, want STICKERS_TOO_MUCH", got)
	}
	after, err := s.UserStickerSetByID(ctx, created.Set.ID, created.Set.AccessHash)
	if err != nil || after.ItemCount != store.MaxUserStickerCount || after.Hash != created.Set.Hash {
		t.Fatalf("set after over-limit add = count %d hash %d, err %v; want unchanged count/hash", after.ItemCount, after.Hash, err)
	}
}

func TestChangeStickerUpdatesMaskCoordsOnlyForMaskSets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	blobs := testBlobs(t)
	s := openStickerStore(t, blobs)
	owner, err := s.CreateUser(ctx, "+15551819122")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	h := fullChannelDispatcher(s)
	regularFile := storedStickerDocument(t, s, blobs, owner.ID, validStickerTGS(t))
	regular := createStickerSet(t, h, owner.ID, createStickerRequest("coords_regular", regularFile, false, false))
	invalidSticker := storedStickerDocument(t, s, blobs, owner.ID, validStickerTGSSize(t, 100, 100))
	if got := stickerRPCError(t, dispatchChatMethod(t, h, owner.ID, "stickers.addStickerToSet", &tg.StickersAddStickerToSetRequest{
		Stickerset: &tg.InputStickerSetID{ID: regular.Set.ID, AccessHash: regular.Set.AccessHash},
		Sticker: tg.InputStickerSetItem{
			Document: &tg.InputDocument{ID: invalidSticker.ID, AccessHash: invalidSticker.AccessHash}, Emoji: "⭐",
		},
	})); got != "STICKER_PNG_DIMENSIONS" {
		t.Fatalf("wrong-sized sticker add = %q, want STICKER_PNG_DIMENSIONS", got)
	}
	invalidThumb := storedStickerDocument(t, s, blobs, owner.ID, validStickerTGSSize(t, 99, 100))
	thumbRequest := &tg.StickersSetStickerSetThumbRequest{Stickerset: &tg.InputStickerSetID{ID: regular.Set.ID, AccessHash: regular.Set.AccessHash}}
	thumbRequest.SetThumb(&tg.InputDocument{ID: invalidThumb.ID, AccessHash: invalidThumb.AccessHash})
	if got := stickerRPCError(t, dispatchChatMethod(t, h, owner.ID, "stickers.setStickerSetThumb", thumbRequest)); got != "STICKER_PNG_DIMENSIONS" {
		t.Fatalf("wrong-sized set thumbnail = %q, want STICKER_PNG_DIMENSIONS", got)
	}
	regularAfterInvalidInputs, err := s.UserStickerSetByID(ctx, regular.Set.ID, regular.Set.AccessHash)
	if err != nil || regularAfterInvalidInputs.ItemCount != 1 || regularAfterInvalidInputs.Hash != regular.Set.Hash || regularAfterInvalidInputs.Thumbnail != nil {
		t.Fatalf("regular set after invalid file dimensions = %+v, err %v; want unchanged", regularAfterInvalidInputs, err)
	}
	coordinates := tg.MaskCoords{N: 2, X: 0.5, Y: 0.25, Zoom: 1.5}
	invalid := &tg.StickersChangeStickerRequest{Sticker: &tg.InputDocument{ID: regularFile.ID, AccessHash: regularFile.AccessHash}}
	invalid.SetMaskCoords(coordinates)
	if got := stickerRPCError(t, dispatchChatMethod(t, h, owner.ID, "stickers.changeSticker", invalid)); got != "STICKERSET_INVALID" {
		t.Fatalf("mask coordinates on regular set = %q, want STICKERSET_INVALID", got)
	}
	regularAfter, err := s.UserStickerSetByID(ctx, regular.Set.ID, regular.Set.AccessHash)
	if err != nil || regularAfter.ItemCount != 1 || regularAfter.Hash != regular.Set.Hash || len(regularAfter.Items) != 1 || regularAfter.Items[0].MaskCoords != "" {
		t.Fatalf("regular set after invalid coordinates = %+v, err %v; want unchanged", regularAfter, err)
	}

	maskFile := storedStickerDocument(t, s, blobs, owner.ID, validStickerTGS(t))
	createStickerSet(t, h, owner.ID, createStickerRequest("coords_mask", maskFile, true, false))
	change := &tg.StickersChangeStickerRequest{Sticker: &tg.InputDocument{ID: maskFile.ID, AccessHash: maskFile.AccessHash}}
	change.SetMaskCoords(coordinates)
	updated := stickerSetMutation(t, h, owner.ID, "stickers.changeSticker", change)
	if len(updated.Documents) != 1 {
		t.Fatalf("mask change returned %d documents, want one", len(updated.Documents))
	}
	document, ok := updated.Documents[0].(*tg.Document)
	if !ok {
		t.Fatalf("changed mask document = %T, want *tg.Document", updated.Documents[0])
	}
	for _, attribute := range document.Attributes {
		sticker, ok := attribute.(*tg.DocumentAttributeSticker)
		if !ok {
			continue
		}
		got, hasCoords := sticker.GetMaskCoords()
		if hasCoords && got == coordinates {
			return
		}
	}
	t.Fatalf("changed mask attributes = %+v, want coordinates %+v", document.Attributes, coordinates)
}

func TestOwnerCanEditAndDeleteUserStickerSet(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	blobs := testBlobs(t)
	dsn := pgtest.DSN(t)
	s := openStickerStore(t, blobs, dsn)
	owner, err := s.CreateUser(ctx, "+15551819101")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	installer, err := s.CreateUser(ctx, "+15551819102")
	if err != nil {
		t.Fatalf("create installer: %v", err)
	}
	other, err := s.CreateUser(ctx, "+15551819103")
	if err != nil {
		t.Fatalf("create other account: %v", err)
	}

	first := storedStickerDocument(t, s, blobs, owner.ID, validStickerTGS(t))
	second := storedStickerDocument(t, s, blobs, owner.ID, validStickerTGS(t))
	added := storedStickerDocument(t, s, blobs, owner.ID, validStickerTGS(t))
	request := createStickerRequest("editable_set", first, false, false)
	request.Stickers = append(request.Stickers, tg.InputStickerSetItem{
		Document: &tg.InputDocument{ID: second.ID, AccessHash: second.AccessHash}, Emoji: "⭐",
	})
	h := fullChannelDispatcher(s)
	created := createStickerSet(t, h, owner.ID, request)
	ref := &tg.InputStickerSetID{ID: created.Set.ID, AccessHash: created.Set.AccessHash}
	if got := installStickerSet(t, h, installer.ID, ref); got != "success" {
		t.Fatalf("install user sticker set = %q, want success", got)
	}
	initialList := getAllStickerSets(t, h, installer.ID, 0)
	if len(initialList.Sets) != 1 || initialList.Sets[0].ID != created.Set.ID {
		t.Fatalf("initial installer set list = %+v, want set %d", initialList.Sets, created.Set.ID)
	}
	listHash := initialList.Hash

	add := tg.InputStickerSetItem{
		Document: &tg.InputDocument{ID: added.ID, AccessHash: added.AccessHash}, Emoji: "⭐",
	}
	add.SetKeywords("star,shine")
	updated := stickerSetMutation(t, h, owner.ID, "stickers.addStickerToSet", &tg.StickersAddStickerToSetRequest{
		Stickerset: ref, Sticker: add,
	})
	if updated.Set.Count != 3 || len(updated.Documents) != 3 {
		t.Fatalf("addStickerToSet returned count %d and %d documents, want 3", updated.Set.Count, len(updated.Documents))
	}
	list := getAllStickerSets(t, h, installer.ID, listHash)
	if list.Hash == listHash || len(list.Sets) != 1 || list.Sets[0].Count != 3 {
		t.Fatalf("installer list after add = %+v, want changed hash and 3 stickers", list)
	}
	listHash = list.Hash

	updated = stickerSetMutation(t, h, owner.ID, "stickers.changeStickerPosition", &tg.StickersChangeStickerPositionRequest{
		Sticker: &tg.InputDocument{ID: added.ID, AccessHash: added.AccessHash}, Position: 0,
	})
	firstDocument, ok := updated.Documents[0].(*tg.Document)
	if updated.Set.Count != 3 || !ok || firstDocument.ID != added.ID {
		t.Fatalf("changeStickerPosition result = count %d, first document %#v", updated.Set.Count, updated.Documents[0])
	}
	list = getAllStickerSets(t, h, installer.ID, listHash)
	if list.Hash == listHash {
		t.Fatalf("installer list hash stayed %d after reordering stickers", listHash)
	}
	listHash = list.Hash

	change := &tg.StickersChangeStickerRequest{Sticker: &tg.InputDocument{ID: added.ID, AccessHash: added.AccessHash}}
	change.SetEmoji("🌟")
	change.SetKeywords("star,glow")
	updated = stickerSetMutation(t, h, owner.ID, "stickers.changeSticker", change)
	if len(updated.Keywords) != 1 || updated.Keywords[0].DocumentID != added.ID || strings.Join(updated.Keywords[0].Keyword, ",") != "star,glow" {
		t.Fatalf("changeSticker keywords = %+v, want star,glow for %d", updated.Keywords, added.ID)
	}
	if list = getAllStickerSets(t, h, installer.ID, listHash); list.Hash == listHash {
		t.Fatalf("installer list hash stayed %d after changing sticker metadata", listHash)
	}
	listHash = list.Hash

	thumb := storedStickerDocument(t, s, blobs, owner.ID, validStickerTGSSize(t, 100, 100))
	setThumb := &tg.StickersSetStickerSetThumbRequest{Stickerset: ref}
	setThumb.SetThumb(&tg.InputDocument{ID: thumb.ID, AccessHash: thumb.AccessHash})
	updated = stickerSetMutation(t, h, owner.ID, "stickers.setStickerSetThumb", setThumb)
	if len(updated.Set.Thumbs) != 1 || updated.Set.ThumbVersion == 0 || updated.Set.ThumbDCID != 2 {
		t.Fatalf("setStickerSetThumb summary = %+v, want one downloadable 100x100 thumbnail", updated.Set)
	}
	if list = getAllStickerSets(t, h, installer.ID, listHash); list.Hash == listHash {
		t.Fatalf("installer list hash stayed %d after changing the thumbnail", listHash)
	}
	listHash = list.Hash
	erasureCounts, err := s.MediaErasureSummary(ctx, time.Now().Add(time.Hour), 100)
	if err != nil || erasureCounts.SkippedStickerSetRef != 4 {
		t.Fatalf("erasure summary with three set items and one thumbnail = %+v, err %v; want 4 sticker-set references", erasureCounts, err)
	}
	thumbnail, err := api.GetFileForTest(s, installer.ID, blobs, &tg.UploadGetFileRequest{
		Location: &tg.InputStickerSetThumb{Stickerset: ref, ThumbVersion: updated.Set.ThumbVersion}, Limit: 1024,
	})
	if err != nil {
		t.Fatalf("download user sticker set thumbnail: %v", err)
	}
	thumbnailFile, ok := thumbnail.(*tg.UploadFile)
	if !ok || !bytes.Equal(thumbnailFile.Bytes, validStickerTGSSize(t, 100, 100)) {
		t.Fatalf("downloaded sticker set thumbnail = %T, want original TGS bytes", thumbnail)
	}

	updated = stickerSetMutation(t, h, owner.ID, "stickers.removeStickerFromSet", &tg.StickersRemoveStickerFromSetRequest{
		Sticker: &tg.InputDocument{ID: second.ID, AccessHash: second.AccessHash},
	})
	if updated.Set.Count != 2 || len(updated.Documents) != 2 {
		t.Fatalf("removeStickerFromSet returned count %d and %d documents, want 2", updated.Set.Count, len(updated.Documents))
	}
	if list = getAllStickerSets(t, h, installer.ID, listHash); list.Hash == listHash || len(list.Sets) != 1 || list.Sets[0].Count != 2 {
		t.Fatalf("installer list after remove = %+v, want changed hash and 2 stickers", list)
	}
	listHash = list.Hash

	updated = stickerSetMutation(t, h, owner.ID, "stickers.renameStickerSet", &tg.StickersRenameStickerSetRequest{
		Stickerset: ref, Title: "Renamed set",
	})
	if updated.Set.Title != "Renamed set" {
		t.Fatalf("renameStickerSet title = %q, want Renamed set", updated.Set.Title)
	}
	if list = getAllStickerSets(t, h, installer.ID, listHash); list.Hash == listHash {
		t.Fatalf("installer list hash stayed %d after renaming the set", listHash)
	}
	listHash = list.Hash

	_, err = api.SendMediaForTest(s, installer.ID, blobs, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer:    api.InputPeerUser(installer.ID, owner.ID),
		Media:   &tg.InputMediaDocument{ID: &tg.InputDocument{ID: added.ID, AccessHash: added.AccessHash}},
		Message: "sticker sent before set deletion", RandomID: 5518191001,
	})
	if err != nil {
		t.Fatalf("send sticker before deleting set: %v", err)
	}

	for _, unauthorized := range []struct {
		name    string
		method  string
		request bin.Encoder
	}{
		{name: "add", method: "stickers.addStickerToSet", request: &tg.StickersAddStickerToSetRequest{Stickerset: ref, Sticker: tg.InputStickerSetItem{Document: &tg.InputDocument{ID: first.ID, AccessHash: first.AccessHash}, Emoji: "⭐"}}},
		{name: "remove", method: "stickers.removeStickerFromSet", request: &tg.StickersRemoveStickerFromSetRequest{Sticker: &tg.InputDocument{ID: added.ID, AccessHash: added.AccessHash}}},
		{name: "position", method: "stickers.changeStickerPosition", request: &tg.StickersChangeStickerPositionRequest{Sticker: &tg.InputDocument{ID: added.ID, AccessHash: added.AccessHash}, Position: 1}},
		{name: "change", method: "stickers.changeSticker", request: change},
		{name: "thumb", method: "stickers.setStickerSetThumb", request: setThumb},
		{name: "rename", method: "stickers.renameStickerSet", request: &tg.StickersRenameStickerSetRequest{Stickerset: ref, Title: "Attacker title"}},
		{name: "delete", method: "stickers.deleteStickerSet", request: &tg.StickersDeleteStickerSetRequest{Stickerset: ref}},
	} {
		if got := stickerRPCError(t, dispatchChatMethod(t, h, other.ID, unauthorized.method, unauthorized.request)); got == "" {
			t.Fatalf("non-owner %s mutation returned an empty error", unauthorized.name)
		}
	}
	unchanged, err := s.UserStickerSetByID(ctx, created.Set.ID, created.Set.AccessHash)
	if err != nil || unchanged.Title != "Renamed set" || unchanged.ItemCount != 2 {
		t.Fatalf("non-owner changed set = %+v, err %v; want title Renamed set and 2 stickers", unchanged, err)
	}
	if got := stickerRPCError(t, dispatchChatMethod(t, h, owner.ID, "stickers.deleteStickerSet", &tg.StickersDeleteStickerSetRequest{
		Stickerset: &tg.InputStickerSetShortName{ShortName: "twemoji_reactions"},
	})); got != "STICKERSET_INVALID" {
		t.Fatalf("delete catalogue sticker set = %q, want STICKERSET_INVALID", got)
	}

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to seed legacy sticker positions: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close legacy sticker position connection: %v", err)
		}
	})
	if result, updateErr := conn.Exec(ctx, `
		UPDATE user_created_sticker_set_items SET position = 250
		WHERE set_id = $1 AND file_id = $2 AND is_active = false`, created.Set.ID, second.ID); updateErr != nil || result.RowsAffected() != 1 {
		t.Fatalf("seed legacy archived sticker position: result %v, err %v", result, updateErr)
	}
	archivedBefore, err := s.UserStickerSetsByFileIDs(ctx, []int64{second.ID})
	if err != nil {
		t.Fatalf("load removed sticker reference before more edits: %v", err)
	}
	archivedPosition := archivedBefore[second.ID].Item.Position
	if archivedPosition != 250 {
		t.Fatalf("legacy archived sticker position = %d, want 250", archivedPosition)
	}
	extra := storedStickerDocument(t, s, blobs, owner.ID, validStickerTGS(t))
	extraItem := tg.InputStickerSetItem{
		Document: &tg.InputDocument{ID: extra.ID, AccessHash: extra.AccessHash}, Emoji: "⭐",
	}
	if got := stickerSetMutation(t, h, owner.ID, "stickers.addStickerToSet", &tg.StickersAddStickerToSetRequest{
		Stickerset: ref, Sticker: extraItem,
	}).Set.Count; got != 3 {
		t.Fatalf("add after removal returned %d stickers, want 3", got)
	}
	archivedAfterAdd, err := s.UserStickerSetsByFileIDs(ctx, []int64{second.ID})
	if err != nil {
		t.Fatalf("reload legacy sticker reference after add: %v", err)
	}
	if got := archivedAfterAdd[second.ID].Item.Position; got < 401 {
		t.Fatalf("legacy archived sticker position = %d after add, want it moved out of the active/temporary range", got)
	}
	if got := stickerSetMutation(t, h, owner.ID, "stickers.removeStickerFromSet", &tg.StickersRemoveStickerFromSetRequest{
		Sticker: &tg.InputDocument{ID: extra.ID, AccessHash: extra.AccessHash},
	}).Set.Count; got != 2 {
		t.Fatalf("remove after add returned %d stickers, want 2", got)
	}
	archivedAfter, err := s.UserStickerSetsByFileIDs(ctx, []int64{second.ID})
	if err != nil {
		t.Fatalf("reload removed sticker reference after more edits: %v", err)
	}
	if got := archivedAfter[second.ID].Item.Position; got != archivedAfterAdd[second.ID].Item.Position {
		t.Fatalf("removed sticker history position changed from %d to %d after active-set edits", archivedAfterAdd[second.ID].Item.Position, got)
	}

	deleted := dispatchChatMethod(t, h, owner.ID, "stickers.deleteStickerSet", &tg.StickersDeleteStickerSetRequest{Stickerset: ref})
	var deletedResult tg.BoolTrue
	if err := deletedResult.Decode(&bin.Buffer{Buf: deleted}); err != nil {
		t.Fatalf("decode deleteStickerSet result: %v", err)
	}
	erasureCounts, err = s.MediaErasureSummary(ctx, time.Now().Add(time.Hour), 100)
	if err != nil || erasureCounts.SkippedStickerSetRef != 4 || erasureCounts.SkippedMessageRef != 0 {
		t.Fatalf("erasure summary after delete = %+v, err %v; want 4 retained item refs and no thumbnail ref", erasureCounts, err)
	}
	if _, err := s.UserStickerSetByID(ctx, created.Set.ID, created.Set.AccessHash); !errors.Is(err, store.ErrStickerSetNotFound) {
		t.Fatalf("deleted set lookup error = %v, want ErrStickerSetNotFound", err)
	}
	if got := stickerRPCError(t, dispatchChatMethod(t, h, installer.ID, "messages.getStickerSet", &tg.MessagesGetStickerSetRequest{Stickerset: ref})); got != "STICKERSET_INVALID" {
		t.Fatalf("get deleted set by ID = %q, want STICKERSET_INVALID", got)
	}
	if got := stickerRPCError(t, dispatchChatMethod(t, h, installer.ID, "messages.getStickerSet", &tg.MessagesGetStickerSetRequest{
		Stickerset: &tg.InputStickerSetShortName{ShortName: "editable_set"},
	})); got != "STICKERSET_INVALID" {
		t.Fatalf("get deleted set by short name = %q, want STICKERSET_INVALID", got)
	}
	deletedList := getAllStickerSets(t, h, installer.ID, listHash)
	if len(deletedList.Sets) != 0 || deletedList.Hash == listHash {
		t.Fatalf("installer list after delete = %+v, want empty with changed hash", deletedList)
	}
	ownerHistory, err := api.GetHistoryForTest(s, owner.ID, &tg.MessagesGetHistoryRequest{
		Peer: api.InputPeerUser(owner.ID, installer.ID), Limit: 10,
	})
	if err != nil {
		t.Fatalf("read message sent before set deletion: %v", err)
	}
	messageList, ok := ownerHistory.(*tg.MessagesMessages)
	if !ok || len(messageList.Messages) == 0 {
		t.Fatalf("history after set deletion = %T, want sent sticker message", ownerHistory)
	}
	sentMessage, ok := messageList.Messages[0].(*tg.Message)
	if !ok {
		t.Fatalf("sent message after set deletion = %T, want *tg.Message", messageList.Messages[0])
	}
	media, ok := sentMessage.Media.(*tg.MessageMediaDocument)
	if !ok {
		t.Fatalf("sent media after set deletion = %T, want sticker document", sentMessage.Media)
	}
	sentDocument, ok := media.Document.(*tg.Document)
	if !ok || sentDocument.ID != added.ID {
		t.Fatalf("sent document after set deletion = %#v, want document %d", media.Document, added.ID)
	}
	oldSticker, err := api.GetFileForTest(s, owner.ID, blobs, &tg.UploadGetFileRequest{
		Location: &tg.InputDocumentFileLocation{ID: sentDocument.ID, AccessHash: sentDocument.AccessHash}, Limit: 1024,
	})
	if err != nil {
		t.Fatalf("download sticker from message after set deletion: %v", err)
	}
	oldStickerFile, ok := oldSticker.(*tg.UploadFile)
	if !ok || !bytes.Equal(oldStickerFile.Bytes, validStickerTGS(t)) {
		t.Fatalf("message sticker after set deletion = %T, want original sticker bytes", oldSticker)
	}
}

func TestStickerImportOffBlocksAddingButAllowsRemoval(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	blobs := testBlobs(t)
	s := openStickerStore(t, blobs)
	owner, err := s.CreateUser(ctx, "+15551819111")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	first := storedStickerDocument(t, s, blobs, owner.ID, validStickerTGS(t))
	second := storedStickerDocument(t, s, blobs, owner.ID, validStickerTGS(t))
	third := storedStickerDocument(t, s, blobs, owner.ID, validStickerTGS(t))
	on := fullChannelDispatcher(s)
	initial := createStickerRequest("off_mode_set", first, false, false)
	initial.Stickers = append(initial.Stickers, tg.InputStickerSetItem{
		Document: &tg.InputDocument{ID: third.ID, AccessHash: third.AccessHash}, Emoji: "⭐",
	})
	created := createStickerSet(t, on, owner.ID, initial)
	off := stickerDispatcherWithImportMode(s, config.StickerImportOff)
	add := tg.InputStickerSetItem{Document: &tg.InputDocument{ID: second.ID, AccessHash: second.AccessHash}, Emoji: "⭐"}
	if got := stickerRPCError(t, dispatchChatMethod(t, off, owner.ID, "stickers.addStickerToSet", &tg.StickersAddStickerToSetRequest{
		Stickerset: &tg.InputStickerSetID{ID: created.Set.ID, AccessHash: created.Set.AccessHash}, Sticker: add,
	})); got != "STICKER_IMPORT_DISABLED" {
		t.Fatalf("addStickerToSet with imports off = %q, want STICKER_IMPORT_DISABLED", got)
	}
	other, err := s.CreateUser(ctx, "+15551819112")
	if err != nil {
		t.Fatalf("create non-owner: %v", err)
	}
	if got := stickerRPCError(t, dispatchChatMethod(t, off, other.ID, "stickers.addStickerToSet", &tg.StickersAddStickerToSetRequest{
		Stickerset: &tg.InputStickerSetID{ID: created.Set.ID, AccessHash: created.Set.AccessHash}, Sticker: add,
	})); got != "STICKER_IMPORT_DISABLED" {
		t.Fatalf("non-owner addStickerToSet with imports off = %q, want STICKER_IMPORT_DISABLED", got)
	}
	ref := &tg.InputDocument{ID: first.ID, AccessHash: first.AccessHash}
	updated := stickerSetMutation(t, off, owner.ID, "stickers.removeStickerFromSet", &tg.StickersRemoveStickerFromSetRequest{Sticker: ref})
	if updated.Set.Count != 1 {
		t.Fatalf("removeStickerFromSet with imports off left %d stickers, want 1", updated.Set.Count)
	}
}

func stickerSetMutation(t *testing.T, h mtproto.Handler, userID int64, method string, request bin.Encoder) *tg.MessagesStickerSet {
	t.Helper()
	body := dispatchChatMethod(t, h, userID, method, request)
	var rpc mt.RPCError
	if err := rpc.Decode(&bin.Buffer{Buf: body}); err == nil {
		t.Fatalf("%s RPC error = %q", method, rpc.ErrorMessage)
	}
	var box tg.MessagesStickerSetBox
	if err := box.Decode(&bin.Buffer{Buf: body}); err != nil {
		t.Fatalf("decode %s response: %v", method, err)
	}
	set, ok := box.StickerSet.(*tg.MessagesStickerSet)
	if !ok {
		t.Fatalf("%s response = %T, want *tg.MessagesStickerSet", method, box.StickerSet)
	}
	return set
}

func stickerDispatcherWithImportMode(s *store.Store, mode config.StickerImportMode) mtproto.Handler {
	return api.NewWithPhotoThumbsAndDialogFilterSyncAndUploadPartTTLAndTurnstileAndWelcomeChannelAndSignInAndStickerCatalogAndUserInviteQuota(
		s, 2, &tg.Config{MeURLPrefix: testPublicLinkPrefix}, slog.New(slog.DiscardHandler), false,
		api.TestMaxFileBytes, nil, 1, pgtest.PeerDeriver(), pgtest.PhotoDeriver(),
		config.RateLimitsConfig{}, config.RegistrationClosed, api.NewDialogFilterSync(), nil,
		config.DefaultUploadPartTTL, "", nil, "", false, config.RecoveryEmailOff,
		config.DefaultSignUpTermsOfService, nil, mode, 0, config.DefaultUserInviteMinAccountAge, false,
	)
}
