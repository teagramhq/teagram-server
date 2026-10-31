package api_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"

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

func TestAddStickerToSetRateLimitAppliesWhileEditingIsUnsupported(t *testing.T) {
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
		if got := stickerRPCError(t, dispatchChatMethod(t, h, owner.ID, "stickers.addStickerToSet", req)); got != "INPUT_METHOD_INVALID" {
			t.Fatalf("addStickerToSet call %d = %q, want INPUT_METHOD_INVALID", call, got)
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
		INSERT INTO user_created_sticker_sets (access_hash, creator_id, title, short_name, kind)
		SELECT 7000000000000 + n, $1, 'Seeded', 'seed_' || n::text, 'regular'
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
		INSERT INTO user_created_sticker_sets (access_hash, creator_id, title, short_name, kind)
		SELECT 7000000000000 + n, $1, 'Occupied',
		       upper('wild_cats' || CASE WHEN n = 0 THEN '' ELSE '_' || n::text END),
		       'regular'
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
			INSERT INTO user_created_sticker_sets (access_hash, creator_id, title, short_name, kind)
			SELECT 7000000000000 + n, $1, 'Batch set', 'batch_' || n::text,
			       CASE WHEN n <= $2 / 2 THEN 'regular' ELSE 'emojis' END
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
			if n := tracer.n.Load(); n != 3 {
				t.Fatalf("getter made %d database queries at the %d-set install cap, want 3", n, store.MaxInstalledStickerSets)
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
	emojis := getEmojiStickerSets(t, h, installer.ID, initialEmoji.Hash)
	if len(emojis.Sets) != 1 || emojis.Sets[0].ID != emoji.Set.ID || emojis.Hash == initialEmoji.Hash {
		t.Fatalf("getEmojiStickers after install = %+v, want custom emoji set and changed hash", emojis)
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
	_, inactiveSetErr := api.GetFileForTest(s, installer.ID, blobs, &tg.UploadGetFileRequest{
		Location: &tg.InputDocumentFileLocation{ID: regularDoc.ID, AccessHash: regularDoc.AccessHash}, Limit: 1024,
	})
	if got := rpcMessage(t, inactiveSetErr); got != "LOCATION_INVALID" {
		t.Fatalf("download inactive user sticker = %q, want LOCATION_INVALID", got)
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
