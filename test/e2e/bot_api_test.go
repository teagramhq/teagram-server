package e2e_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/botapi"
	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestBotAPIGetMe(t *testing.T) {
	t.Parallel()
	testSmokeBotAPIGetMe(t)
}

func TestBotAPISendMessage(t *testing.T) {
	t.Parallel()
	testSmokeBotAPISendMessage(t)
}

func TestBotAPIGetUpdates(t *testing.T) {
	t.Parallel()
	f := newSmokeFixtureWithSetup(t, config.RegistrationClosed, func(f *smokeFixture) {
		f.photoThumbs = buildSmokePhotoThumbSupervisor(t)
	})
	botA, tokenA, err := f.store.CreateBot(f.ctx, "smoke_updates_a_bot", "Weather A")
	if err != nil {
		t.Fatalf("create bot A: %v", err)
	}
	botB, tokenB, err := f.store.CreateBot(f.ctx, "smoke_updates_b_bot", "Weather B")
	if err != nil {
		t.Fatalf("create bot B: %v", err)
	}
	const phoneA, phoneB = "+15551049321", "+15551049322"
	seedSmokeUsers(t, f, phoneA, phoneB)
	humanA := newSmokeClient(t, f, "Bot API update user A", phoneA)
	humanB := newSmokeClient(t, f, "Bot API update user B", phoneB)
	observedStore := &observedBotUpdateStore{Store: f.store}
	handler := botapi.NewHandler(observedStore, slog.New(slog.DiscardHandler), store.RateLimitConfig{})
	serverURL := startSmokeBotAPIServerWithHandler(t, f, handler)
	startSmokeBotAPINotifyListener(t, f, handler)
	client := &http.Client{Timeout: 15 * time.Second}

	if err := humanA.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		_, err := api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: peerUser(humanA.id, botA.ID), Message: "/start", RandomID: 2819001,
		})
		return err
	}); err != nil {
		t.Fatalf("human A starts bot A: %v", err)
	}
	first := getBotAPIUpdates(t, f.ctx, client, serverURL, tokenA, 0, 0)
	firstUpdates := decodeBotAPIUpdates(t, first)
	if first.Code != http.StatusOK || len(firstUpdates) != 1 || firstUpdates[0].Message.Text != "/start" {
		t.Fatalf("bot A initial updates = (%d, %+v), want only /start", first.Code, firstUpdates)
	}

	if err := humanB.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		_, err := api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: peerUser(humanB.id, botB.ID), Message: "bot B message", RandomID: 2819002,
		})
		return err
	}); err != nil {
		t.Fatalf("human B sends to bot B: %v", err)
	}
	firstNextOffset := firstUpdates[0].UpdateID + 1
	noBotBUpdate := getBotAPIUpdates(t, f.ctx, client, serverURL, tokenA, firstNextOffset, 0)
	if noBotBUpdate.Code != http.StatusOK || string(noBotBUpdate.Body) != "{\"ok\":true,\"result\":[]}\n" {
		t.Fatalf("bot A cross-bot response = (%d, %q), want empty", noBotBUpdate.Code, noBotBUpdate.Body)
	}
	botBUpdates := decodeBotAPIUpdates(t, getBotAPIUpdates(t, f.ctx, client, serverURL, tokenB, 0, 0))
	if len(botBUpdates) != 1 || botBUpdates[0].Message.Text != "bot B message" {
		t.Fatalf("bot B updates = %+v, want its own text", botBUpdates)
	}
	botBOffset := botBUpdates[0].UpdateID
	if offset, err := f.store.BotUpdateOffset(f.ctx, botA.ID, tokenA); err != nil || offset != firstNextOffset {
		t.Fatalf("bot A offset = %d, err %v, want %d", offset, err, firstNextOffset)
	}
	if offset, err := f.store.BotUpdateOffset(f.ctx, botB.ID, tokenB); err != nil || offset != botBOffset {
		t.Fatalf("bot B offset = %d, err %v, want its returned update offset %d", offset, err, botBOffset)
	}

	botReply := sendBotAPIMessage(t, f.ctx, client, serverURL, tokenA, humanA.id, "bot reply")
	if botReply.Code != http.StatusOK {
		t.Fatalf("bot A reply status = %d, want 200; body %q", botReply.Code, botReply.Body)
	}
	receivedReply := recvOrCtx(t, f.ctx, humanA.push.newMsg, "Bot API reply update")
	replyFrom, replyFromOK := receivedReply.FromID.(*tg.PeerUser)
	if !replyFromOK || replyFrom.UserID != botA.ID || receivedReply.Out || receivedReply.Message != "bot reply" {
		t.Fatalf("human A received reply = %+v, want incoming bot A reply", receivedReply)
	}
	var replyHistory tg.MessagesMessagesClass
	if err := humanA.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		replyHistory, err = api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
			Peer: peerUser(humanA.id, botA.ID), Limit: 10,
		})
		return err
	}); err != nil {
		t.Fatalf("human A reads bot reply history: %v", err)
	}
	var historyMessages []tg.MessageClass
	switch history := replyHistory.(type) {
	case *tg.MessagesMessages:
		historyMessages = history.Messages
	case *tg.MessagesMessagesSlice:
		historyMessages = history.Messages
	default:
		t.Fatalf("human A message history = %T, want messages", replyHistory)
	}
	readReply := false
	for _, class := range historyMessages {
		message, ok := class.(*tg.Message)
		if ok && message.ID == receivedReply.ID && message.Message == "bot reply" {
			readReply = true
			break
		}
	}
	if !readReply {
		t.Fatalf("human A history did not contain bot reply %d: %+v", receivedReply.ID, historyMessages)
	}

	if err := sendBotAPIProofPhoto(t, f, humanA, botA.ID); err != nil {
		t.Fatalf("human A sends photo: %v", err)
	}
	observedStore.emptyQuery = make(chan struct{})
	longPoll := make(chan asyncBotAPIResponse, 1)
	go func() {
		response, err := doBotAPIGetUpdates(f.ctx, client, serverURL, tokenA, firstNextOffset, 5)
		longPoll <- asyncBotAPIResponse{response: response, err: err}
	}()
	select {
	case <-observedStore.emptyQuery:
	case <-time.After(3 * time.Second):
		t.Fatal("getUpdates did not reach its empty-log wait after the photo")
	}
	if err := humanA.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		_, err := api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: peerUser(humanA.id, botA.ID), Message: "after photo", RandomID: 2819004,
		})
		return err
	}); err != nil {
		t.Fatalf("human A sends text after photo: %v", err)
	}
	var afterPhoto botAPIHTTPResponse
	select {
	case result := <-longPoll:
		if result.err != nil {
			t.Fatalf("long-poll request failed: %v", result.err)
		}
		afterPhoto = result.response
	case <-time.After(4 * time.Second):
		t.Fatal("getUpdates did not wake from the existing update notification")
	}
	afterPhotoUpdates := decodeBotAPIUpdates(t, afterPhoto)
	if afterPhoto.Code != http.StatusOK || len(afterPhotoUpdates) != 1 || afterPhotoUpdates[0].Message.Text != "after photo" {
		t.Fatalf("bot A updates after photo = (%d, %+v), want one text and no media or bot reply", afterPhoto.Code, afterPhotoUpdates)
	}
	endOffset := afterPhotoUpdates[0].UpdateID + 1
	replayed := getBotAPIUpdates(t, f.ctx, client, serverURL, tokenA, endOffset, 0)
	if replayed.Code != http.StatusOK || string(replayed.Body) != "{\"ok\":true,\"result\":[]}\n" {
		t.Fatalf("bot A replay response = (%d, %q), want empty", replayed.Code, replayed.Body)
	}
	if offset, err := f.store.BotUpdateOffset(f.ctx, botB.ID, tokenB); err != nil || offset != botBOffset {
		t.Fatalf("bot B offset after bot A polls = %d, err %v, want unchanged %d", offset, err, botBOffset)
	}

	const phoneC = "+15551049323"
	seedSmokeUsers(t, f, phoneC)
	humanC := newSmokeClient(t, f, "Bot API stock update client", phoneC)
	runBotAPIStockGetUpdatesSequence(t, f, serverURL, tokenA, botA.ID, humanC)
}

func testSmokeBotAPIGetUpdatesStockClient(t *testing.T) {
	t.Helper()
	f := newSmokeFixtureWithSetup(t, config.RegistrationClosed, func(f *smokeFixture) {
		f.photoThumbs = buildSmokePhotoThumbSupervisor(t)
	})
	bot, token, err := f.store.CreateBot(f.ctx, "smoke_updates_stock_bot", "Weather Stock")
	if err != nil {
		t.Fatalf("create stock-client bot: %v", err)
	}
	const phone = "+15551049324"
	seedSmokeUsers(t, f, phone)
	human := newSmokeClient(t, f, "Bot API stock update smoke", phone)
	handler := botapi.NewHandler(f.store, slog.New(slog.DiscardHandler), store.RateLimitConfig{})
	serverURL := startSmokeBotAPIServerWithHandler(t, f, handler)
	startSmokeBotAPINotifyListener(t, f, handler)
	runBotAPIStockGetUpdatesSequence(t, f, serverURL, token, bot.ID, human)
}

func runBotAPIStockGetUpdatesSequence(t *testing.T, f *smokeFixture, serverURL, token string, botID int64, human *smokeClient) {
	t.Helper()
	stockClient, err := tgbotapi.NewBotAPIWithClient(token, serverURL+"/bot%s/%s", &http.Client{Timeout: 15 * time.Second})
	if err != nil {
		t.Fatalf("create stock Bot API client: %v", err)
	}
	config := tgbotapi.NewUpdate(0)
	config.Limit = 100
	config.Timeout = 1
	updates := stockClient.GetUpdatesChan(config)
	t.Cleanup(func() {
		stockClient.StopReceivingUpdates()
		deadline := time.NewTimer(5 * time.Second)
		defer deadline.Stop()
		for {
			select {
			case _, ok := <-updates:
				if !ok {
					return
				}
			case <-deadline.C:
				t.Error("stock Bot API update client did not stop")
				return
			}
		}
	})

	const firstText = "stock client start"
	if err := human.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		_, err := api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: peerUser(human.id, botID), Message: firstText, RandomID: 2819005,
		})
		return err
	}); err != nil {
		t.Fatalf("human sends stock-client start: %v", err)
	}
	var first tgbotapi.Update
	select {
	case first = <-updates:
	case <-time.After(5 * time.Second):
		t.Fatal("stock Bot API client did not receive the first human text")
	}
	if first.Message == nil || first.Message.Text != firstText {
		t.Fatalf("stock client first update = %+v, want %q", first, firstText)
	}

	const replyText = "stock client bot reply"
	reply := sendBotAPIMessage(t, f.ctx, &http.Client{Timeout: 15 * time.Second}, serverURL, token, human.id, replyText)
	if reply.Code != http.StatusOK {
		t.Fatalf("bot reply status = %d, want 200; body %q", reply.Code, reply.Body)
	}
	receivedReply := recvOrCtx(t, f.ctx, human.push.newMsg, "stock Bot API reply")
	replyFrom, replyFromOK := receivedReply.FromID.(*tg.PeerUser)
	if !replyFromOK || replyFrom.UserID != botID || receivedReply.Out || receivedReply.Message != replyText {
		t.Fatalf("human received stock Bot API reply = %+v, want incoming bot reply", receivedReply)
	}
	if err := sendBotAPIProofPhoto(t, f, human, botID); err != nil {
		t.Fatalf("human sends photo between stock-client updates: %v", err)
	}

	const secondText = "stock client text after photo"
	if err := human.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		_, err := api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: peerUser(human.id, botID), Message: secondText, RandomID: 2819006,
		})
		return err
	}); err != nil {
		t.Fatalf("human sends text after photo: %v", err)
	}
	var second tgbotapi.Update
	select {
	case second = <-updates:
	case <-time.After(5 * time.Second):
		t.Fatal("stock Bot API client did not receive the human text after the photo")
	}
	if second.Message == nil || second.Message.Text != secondText || second.UpdateID <= first.UpdateID {
		t.Fatalf("stock client second update = %+v, want %q after update %d", second, secondText, first.UpdateID)
	}
	select {
	case extra := <-updates:
		t.Fatalf("stock Bot API client received an extra or duplicate update: %+v", extra)
	case <-time.After(1500 * time.Millisecond):
	}
}

func TestBotAPIGetUpdatesSupersedesPollAcrossHandlerInstances(t *testing.T) {
	t.Parallel()
	f := newSmokeFixture(t)
	bot, token, err := f.store.CreateBot(f.ctx, "smoke_replica_poll_bot", "Replica")
	if err != nil {
		t.Fatalf("create bot: %v", err)
	}
	const phone = "+15551049330"
	seedSmokeUsers(t, f, phone)
	human := newSmokeClient(t, f, "Bot API replica poll", phone)
	firstStore := &observedBotUpdateStore{Store: f.store, emptyQuery: make(chan struct{})}
	secondStore := &observedBotUpdateStore{Store: f.store, emptyQuery: make(chan struct{})}
	firstHandler := botapi.NewHandler(firstStore, slog.New(slog.DiscardHandler), store.RateLimitConfig{})
	secondHandler := botapi.NewHandler(secondStore, slog.New(slog.DiscardHandler), store.RateLimitConfig{})
	firstURL := startSmokeBotAPIServerWithHandler(t, f, firstHandler)
	secondURL := startSmokeBotAPIServerWithHandler(t, f, secondHandler)
	startSmokeBotAPINotifyListener(t, f, firstHandler)
	startSmokeBotAPINotifyListener(t, f, secondHandler)
	client := &http.Client{Timeout: 15 * time.Second}

	if err := human.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		_, err := api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: peerUser(human.id, bot.ID), Message: "first update", RandomID: 2819030,
		})
		return err
	}); err != nil {
		t.Fatalf("human sends first message: %v", err)
	}
	first := getBotAPIUpdates(t, f.ctx, client, firstURL, token, 0, 0)
	firstUpdates := decodeBotAPIUpdates(t, first)
	if first.Code != http.StatusOK || len(firstUpdates) != 1 || firstUpdates[0].Message.Text != "first update" {
		t.Fatalf("initial updates = (%d, %+v), want first update", first.Code, firstUpdates)
	}
	confirmedOffset := firstUpdates[0].UpdateID + 1

	firstPoll := make(chan asyncBotAPIResponse, 1)
	go func() {
		response, err := doBotAPIGetUpdates(f.ctx, client, firstURL, token, confirmedOffset+1, 5)
		firstPoll <- asyncBotAPIResponse{response: response, err: err}
	}()
	select {
	case <-firstStore.emptyQuery:
	case <-time.After(3 * time.Second):
		t.Fatal("first replica did not enter the empty-log wait")
	}

	secondPoll := make(chan asyncBotAPIResponse, 1)
	go func() {
		response, err := doBotAPIGetUpdates(f.ctx, client, secondURL, token, confirmedOffset, 5)
		secondPoll <- asyncBotAPIResponse{response: response, err: err}
	}()
	select {
	case <-secondStore.emptyQuery:
	case <-time.After(3 * time.Second):
		t.Fatal("second replica did not enter the empty-log wait")
	}

	var superseded botAPIHTTPResponse
	select {
	case result := <-firstPoll:
		if result.err != nil {
			t.Fatalf("superseded poll request failed: %v", result.err)
		}
		superseded = result.response
	case <-time.After(4 * time.Second):
		t.Fatal("first replica was not woken by the newer poll generation")
	}
	if superseded.Code != http.StatusConflict {
		t.Fatalf("first replica response = (%d, %q), want 409", superseded.Code, superseded.Body)
	}

	if err := human.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		_, err := api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: peerUser(human.id, bot.ID), Message: "second update", RandomID: 2819031,
		})
		return err
	}); err != nil {
		t.Fatalf("human sends second message: %v", err)
	}
	var current botAPIHTTPResponse
	select {
	case result := <-secondPoll:
		if result.err != nil {
			t.Fatalf("current poll request failed: %v", result.err)
		}
		current = result.response
	case <-time.After(4 * time.Second):
		t.Fatal("second replica did not return the update")
	}
	updates := decodeBotAPIUpdates(t, current)
	if current.Code != http.StatusOK || len(updates) != 1 || updates[0].Message.Text != "second update" {
		t.Fatalf("second replica response = (%d, %+v), want only second update", current.Code, updates)
	}
	if offset, err := f.store.BotUpdateOffset(f.ctx, bot.ID, token); err != nil || offset != confirmedOffset {
		t.Fatalf("confirmed offset = %d, err %v, want winning poll's %d", offset, err, confirmedOffset)
	}
}

func TestBotAPIGetUpdatesReReadsAfterMissedNotify(t *testing.T) {
	t.Parallel()
	f := newSmokeFixture(t)
	bot, token, err := f.store.CreateBot(f.ctx, "smoke_timeout_bot", "Timeout")
	if err != nil {
		t.Fatalf("create bot: %v", err)
	}
	const phone = "+15551049323"
	seedSmokeUsers(t, f, phone)
	human := newSmokeClient(t, f, "Bot API missed notify", phone)
	observedStore := &observedBotUpdateStore{Store: f.store, emptyQuery: make(chan struct{})}
	handler := botapi.NewHandler(observedStore, slog.New(slog.DiscardHandler), store.RateLimitConfig{})
	serverURL := startSmokeBotAPIServerWithHandler(t, f, handler)
	client := &http.Client{Timeout: 12 * time.Second}
	const timeout = 5
	started := time.Now()
	result := make(chan asyncBotAPIResponse, 1)
	go func() {
		response, err := doBotAPIGetUpdates(f.ctx, client, serverURL, token, 0, timeout)
		result <- asyncBotAPIResponse{response: response, err: err}
	}()
	select {
	case <-observedStore.emptyQuery:
	case <-time.After(3 * time.Second):
		t.Fatal("getUpdates did not read the empty log")
	}
	if err := human.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		_, err := api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: peerUser(human.id, bot.ID), Message: "arrives after missed notify", RandomID: 2819005,
		})
		return err
	}); err != nil {
		t.Fatalf("human sends during long poll: %v", err)
	}
	var response botAPIHTTPResponse
	select {
	case asyncResult := <-result:
		if asyncResult.err != nil {
			t.Fatalf("missed-notify poll request failed: %v", asyncResult.err)
		}
		response = asyncResult.response
	case <-time.After(12 * time.Second):
		t.Fatal("getUpdates did not return after timeout re-read")
	}
	updates := decodeBotAPIUpdates(t, response)
	if response.Code != http.StatusOK || len(updates) != 1 || updates[0].Message.Text != "arrives after missed notify" {
		t.Fatalf("missed-notify response = (%d, %+v), want the committed text", response.Code, updates)
	}
	if elapsed := time.Since(started); elapsed < time.Duration(timeout)*time.Second-200*time.Millisecond {
		t.Fatalf("missed-notify poll returned after %v, want it to wait for timeout", elapsed)
	}
}

func TestBotAPIGetUpdatesRevocationReturns401WithoutAdvancingOffset(t *testing.T) {
	t.Parallel()
	f := newSmokeFixture(t)
	bot, token, err := f.store.CreateBot(f.ctx, "smoke_revoke_poll_bot", "Revoked")
	if err != nil {
		t.Fatalf("create bot: %v", err)
	}
	const phone = "+15551049324"
	seedSmokeUsers(t, f, phone)
	human := newSmokeClient(t, f, "Bot API revoked poll", phone)
	observedStore := &observedBotUpdateStore{Store: f.store, emptyQuery: make(chan struct{})}
	handler := botapi.NewHandler(observedStore, slog.New(slog.DiscardHandler), store.RateLimitConfig{})
	serverURL := startSmokeBotAPIServerWithHandler(t, f, handler)
	client := &http.Client{Timeout: 5 * time.Second}
	result := make(chan asyncBotAPIResponse, 1)
	go func() {
		response, err := doBotAPIGetUpdates(f.ctx, client, serverURL, token, 10, 1)
		result <- asyncBotAPIResponse{response: response, err: err}
	}()
	select {
	case <-observedStore.emptyQuery:
	case <-time.After(3 * time.Second):
		t.Fatal("getUpdates did not read the empty log")
	}
	if err := f.store.RevokeBotToken(f.ctx, bot.ID); err != nil {
		t.Fatalf("revoke bot token: %v", err)
	}
	if err := human.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		_, err := api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: peerUser(human.id, bot.ID), Message: "private after revocation", RandomID: 2819006,
		})
		return err
	}); err != nil {
		t.Fatalf("human sends after token revocation: %v", err)
	}
	var response botAPIHTTPResponse
	select {
	case asyncResult := <-result:
		if asyncResult.err != nil {
			t.Fatalf("revoked poll request failed: %v", asyncResult.err)
		}
		response = asyncResult.response
	case <-time.After(4 * time.Second):
		t.Fatal("revoked getUpdates did not end at timeout")
	}
	if response.Code != http.StatusUnauthorized || string(response.Body) != "{\"ok\":false,\"error_code\":401,\"description\":\"Unauthorized\"}\n" {
		t.Fatalf("revoked getUpdates = (%d, %q), want 401 with no updates", response.Code, response.Body)
	}
	rotatedToken, err := f.store.RotateBotToken(f.ctx, bot.ID)
	if err != nil {
		t.Fatalf("reactivate bot with a new token: %v", err)
	}
	if offset, err := f.store.BotUpdateOffset(f.ctx, bot.ID, rotatedToken); err != nil || offset != 0 {
		t.Fatalf("offset after revoked poll = %d, err %v, want unchanged 0", offset, err)
	}
}

func testSmokeBotAPIGetMe(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	bot, token, err := f.store.CreateBot(f.ctx, "smoke_weather_bot", "Weather")
	if err != nil {
		t.Fatalf("create bot: %v", err)
	}
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(f.ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen Bot API: %v", err)
	}
	listener = botapi.LimitConnectionsPerIP(listener, botapi.DefaultMaxConnectionsPerIP)
	server := botapi.NewServer(listener.Addr().String(), f.store, slog.New(slog.DiscardHandler), store.RateLimitConfig{})
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			t.Errorf("Bot API shutdown failed")
		}
		if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Bot API server stopped unexpectedly")
		}
	})

	request, err := http.NewRequestWithContext(f.ctx, http.MethodGet,
		"http://"+listener.Addr().String()+"/bot"+token+"/getMe", nil)
	if err != nil {
		t.Fatal("build Bot API request")
	}
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal("Bot API request failed")
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Errorf("close Bot API response: %v", err)
		}
	}()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("Bot API status = %d, want 200", response.StatusCode)
	}
	var got struct {
		OK     bool `json:"ok"`
		Result struct {
			ID        int64  `json:"id"`
			IsBot     bool   `json:"is_bot"`
			FirstName string `json:"first_name"`
			Username  string `json:"username"`
		} `json:"result"`
	}
	if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
		t.Fatalf("decode Bot API response: %v", err)
	}
	if !got.OK || got.Result.ID != bot.ID || !got.Result.IsBot || got.Result.FirstName != "Weather" || got.Result.Username != "smoke_weather_bot" {
		t.Fatalf("getMe result = %+v, want bot profile id %d", got, bot.ID)
	}
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatalf("read Bot API response: %v", err)
	}
}

func testSmokeBotAPISendMessage(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	bot, token, err := f.store.CreateBot(f.ctx, "smoke_send_bot", "Weather")
	if err != nil {
		t.Fatalf("create bot: %v", err)
	}
	otherBot, _, err := f.store.CreateBot(f.ctx, "smoke_other_bot", "Other")
	if err != nil {
		t.Fatalf("create other bot: %v", err)
	}
	const phoneStarted, phoneNever, phoneBlocked = "+15551049311", "+15551049312", "+15551049313"
	seedSmokeUsers(t, f, phoneStarted, phoneNever, phoneBlocked)
	neverStarted := smokeUserByPhone(t, f, phoneNever)
	started := newSmokeClient(t, f, "Bot API started user", phoneStarted)
	blocked := newSmokeClient(t, f, "Bot API blocked user", phoneBlocked)
	server := startSmokeBotAPIServer(t, f, store.RateLimitConfig{})
	client := &http.Client{Timeout: 5 * time.Second}

	if err := started.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		_, err := api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: peerUser(started.id, bot.ID), Message: "/start", RandomID: 2818001,
		})
		return err
	}); err != nil {
		t.Fatalf("send /start to bot: %v", err)
	}
	if err := blocked.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		_, err := api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: peerUser(blocked.id, bot.ID), Message: "/start", RandomID: 2818002,
		})
		return err
	}); err != nil {
		t.Fatalf("send /start before blocking bot: %v", err)
	}

	response := sendBotAPIMessage(t, f.ctx, client, server, token, started.id, "hi")
	if response.Code != http.StatusOK {
		t.Fatalf("sendMessage status = %d, want 200; body %q", response.Code, response.Body)
	}
	var sent struct {
		OK     bool `json:"ok"`
		Result struct {
			MessageID int `json:"message_id"`
			From      struct {
				ID        int64  `json:"id"`
				IsBot     bool   `json:"is_bot"`
				FirstName string `json:"first_name"`
				Username  string `json:"username"`
			} `json:"from"`
			Chat struct {
				ID   int64  `json:"id"`
				Type string `json:"type"`
			} `json:"chat"`
			Date int    `json:"date"`
			Text string `json:"text"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body, &sent); err != nil {
		t.Fatalf("decode sendMessage response: %v", err)
	}
	if !sent.OK || sent.Result.From.ID != bot.ID || !sent.Result.From.IsBot || sent.Result.From.FirstName != "Weather" || sent.Result.From.Username != "smoke_send_bot" || sent.Result.Chat.ID != started.id || sent.Result.Chat.Type != "private" || sent.Result.Text != "hi" || sent.Result.Date <= 0 {
		t.Fatalf("sendMessage result = %+v, want bot %d speaking to user %d", sent, bot.ID, started.id)
	}
	received := recvOrCtx(t, f.ctx, started.push.newMsg, "Bot API reply update")
	from, fromOK := received.FromID.(*tg.PeerUser)
	peer, peerOK := received.PeerID.(*tg.PeerUser)
	if received.ID != sent.Result.MessageID || !fromOK || from.UserID != bot.ID || received.Out || !peerOK || peer.UserID != bot.ID || received.Message != "hi" {
		t.Fatalf("human received message = %+v, want normal incoming message from bot %d", received, bot.ID)
	}

	forbidden := "{\"ok\":false,\"error_code\":403,\"description\":\"Forbidden: bot can't initiate conversation with a user\"}\n"
	for label, targetID := range map[string]int64{
		"unknown user":  1 << 62,
		"never-started": neverStarted.ID,
		"another bot":   otherBot.ID,
	} {
		result := sendBotAPIMessage(t, f.ctx, client, server, token, targetID, "hi")
		if result.Code != http.StatusForbidden || string(result.Body) != forbidden {
			t.Errorf("%s response = (%d, %q), want identical 403 %q", label, result.Code, result.Body, forbidden)
		}
	}
	neverStartedHistory, err := f.store.History(f.ctx, neverStarted.ID, store.PeerTypeUser, bot.ID, 0, 10)
	if err != nil {
		t.Fatalf("read never-started user's history: %v", err)
	}
	if len(neverStartedHistory) != 0 {
		t.Fatalf("never-started user's history after refused send = %+v, want empty", neverStartedHistory)
	}

	if _, err := f.store.BlockUser(f.ctx, blocked.id, bot.ID); err != nil {
		t.Fatalf("block bot from user: %v", err)
	}
	blockedBefore, err := f.store.History(f.ctx, blocked.id, store.PeerTypeUser, bot.ID, 0, 10)
	if err != nil {
		t.Fatalf("read blocked user's history before send: %v", err)
	}
	blockedResult := sendBotAPIMessage(t, f.ctx, client, server, token, blocked.id, "blocked reply")
	blockedBody := "{\"ok\":false,\"error_code\":403,\"description\":\"Forbidden: bot was blocked by the user\"}\n"
	if blockedResult.Code != http.StatusForbidden || string(blockedResult.Body) != blockedBody {
		t.Fatalf("blocked send response = (%d, %q), want 403 %q", blockedResult.Code, blockedResult.Body, blockedBody)
	}
	blockedAfter, err := f.store.History(f.ctx, blocked.id, store.PeerTypeUser, bot.ID, 0, 10)
	if err != nil {
		t.Fatalf("read blocked user's history after send: %v", err)
	}
	if len(blockedAfter) != len(blockedBefore) {
		t.Fatalf("blocked user's history count = %d after send, want unchanged at %d", len(blockedAfter), len(blockedBefore))
	}
}

func TestBotAPISendMessageRateLimit(t *testing.T) {
	t.Parallel()
	f := newSmokeFixture(t)
	bot, token, err := f.store.CreateBot(f.ctx, "smoke_limit_bot", "Limit")
	if err != nil {
		t.Fatalf("create bot: %v", err)
	}
	const phone = "+15551049314"
	seedSmokeUsers(t, f, phone)
	human := newSmokeClient(t, f, "Bot API limited user", phone)
	if err := human.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		_, err := api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: peerUser(human.id, bot.ID), Message: "/start", RandomID: 2818003,
		})
		return err
	}); err != nil {
		t.Fatalf("send /start to bot: %v", err)
	}
	server := startSmokeBotAPIServer(t, f, store.RateLimitConfig{Limit: 2, Window: time.Hour})
	client := &http.Client{Timeout: 5 * time.Second}
	for range 2 {
		response := sendBotAPIMessage(t, f.ctx, client, server, token, human.id, "limited")
		if response.Code != http.StatusOK {
			t.Fatalf("sendMessage under limit status = %d, want 200; body %q", response.Code, response.Body)
		}
	}
	response := sendBotAPIMessage(t, f.ctx, client, server, token, human.id, "over limit")
	var limited struct {
		OK          bool   `json:"ok"`
		ErrorCode   int    `json:"error_code"`
		Description string `json:"description"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(response.Body, &limited); err != nil {
		t.Fatalf("decode flood-wait response: %v", err)
	}
	wantDescription := "Too Many Requests: retry after " + strconv.Itoa(limited.Parameters.RetryAfter)
	if response.Code != http.StatusTooManyRequests || limited.OK || limited.ErrorCode != http.StatusTooManyRequests || limited.Parameters.RetryAfter < 1 || limited.Description != wantDescription {
		t.Fatalf("over-limit response = (%d, %+v), want a 429 with retry_after", response.Code, limited)
	}
	history, err := f.store.History(f.ctx, human.id, store.PeerTypeUser, bot.ID, 0, 10)
	if err != nil {
		t.Fatalf("read recipient history after flood wait: %v", err)
	}
	if len(history) != 3 || history[0].Text != "limited" || history[1].Text != "limited" || history[2].Text != "/start" {
		t.Fatalf("recipient history after flood wait = %+v, want two bot messages after /start", history)
	}
}

type botAPIHTTPResponse struct {
	Code int
	Body []byte
}

func startSmokeBotAPIServer(t *testing.T, f *smokeFixture, messageSendLimit store.RateLimitConfig) string {
	t.Helper()
	handler := botapi.NewHandler(f.store, slog.New(slog.DiscardHandler), messageSendLimit)
	return startSmokeBotAPIServerWithHandler(t, f, handler)
}

type observedBotUpdateStore struct {
	*store.Store

	emptyQuery chan struct{}
	queryOnce  sync.Once
}

func (s *observedBotUpdateStore) BotMessageEvents(ctx context.Context, botID, fromPts int64, limit int) (store.BotMessageEventPage, error) {
	page, err := s.Store.BotMessageEvents(ctx, botID, fromPts, limit)
	if err == nil && len(page.Events) == 0 && s.emptyQuery != nil {
		s.queryOnce.Do(func() { close(s.emptyQuery) })
	}
	return page, err
}

func startSmokeBotAPIServerWithHandler(t *testing.T, f *smokeFixture, handler http.Handler) string {
	t.Helper()
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(f.ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen Bot API: %v", err)
	}
	server := botapi.NewServerWithHandler(listener.Addr().String(), handler, slog.New(slog.DiscardHandler))
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			t.Errorf("Bot API shutdown failed")
		}
		if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Bot API server stopped unexpectedly")
		}
	})
	return "http://" + listener.Addr().String()
}

func startSmokeBotAPINotifyListener(t *testing.T, f *smokeFixture, notifier interface{ NotifyUpdates(int64) }) {
	t.Helper()
	_, stop, err := store.StartListener(
		f.ctx, f.dsn,
		func(_ context.Context, ownerID int64) { notifier.NotifyUpdates(ownerID) },
		func(context.Context, int64, int64) {},
		func(context.Context, int64, int64) {},
		func(context.Context, int64) {},
		func(context.Context, int64, int64) {},
		func(context.Context, int64, bool) {},
		func(context.Context, int64, int) {},
		func(context.Context, int64, int64, int64) {},
		func(context.Context, store.PeerType, int64, int32) {},
		slog.New(slog.DiscardHandler),
	)
	if err != nil {
		t.Fatalf("start Bot API notify listener: %v", err)
	}
	t.Cleanup(func() {
		if err := stop(); err != nil {
			t.Errorf("stop Bot API notify listener: %v", err)
		}
	})
}

type asyncBotAPIResponse struct {
	response botAPIHTTPResponse
	err      error
}

func getBotAPIUpdates(t *testing.T, ctx context.Context, client *http.Client, serverURL, token string, offset int64, timeout int) botAPIHTTPResponse {
	t.Helper()
	response, err := doBotAPIGetUpdates(ctx, client, serverURL, token, offset, timeout)
	if err != nil {
		t.Fatalf("Bot API getUpdates request failed: %v", err)
	}
	return response
}

func doBotAPIGetUpdates(ctx context.Context, client *http.Client, serverURL, token string, offset int64, timeout int) (botAPIHTTPResponse, error) {
	body, err := json.Marshal(struct {
		Offset  int64 `json:"offset"`
		Timeout int   `json:"timeout"`
	}{Offset: offset, Timeout: timeout})
	if err != nil {
		return botAPIHTTPResponse{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, serverURL+"/bot"+token+"/getUpdates", strings.NewReader(string(body)))
	if err != nil {
		return botAPIHTTPResponse{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return botAPIHTTPResponse{}, err
	}
	responseBody, err := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if err != nil {
		return botAPIHTTPResponse{}, err
	}
	if closeErr != nil {
		return botAPIHTTPResponse{}, closeErr
	}
	return botAPIHTTPResponse{Code: response.StatusCode, Body: responseBody}, nil
}

func decodeBotAPIUpdates(t *testing.T, response botAPIHTTPResponse) []struct {
	UpdateID int64 `json:"update_id"`
	Message  struct {
		MessageID int64  `json:"message_id"`
		Text      string `json:"text"`
		From      struct {
			ID int64 `json:"id"`
		} `json:"from"`
		Chat struct {
			ID   int64  `json:"id"`
			Type string `json:"type"`
		} `json:"chat"`
	} `json:"message"`
} {
	t.Helper()
	var envelope struct {
		OK     bool `json:"ok"`
		Result []struct {
			UpdateID int64 `json:"update_id"`
			Message  struct {
				MessageID int64  `json:"message_id"`
				Text      string `json:"text"`
				From      struct {
					ID int64 `json:"id"`
				} `json:"from"`
				Chat struct {
					ID   int64  `json:"id"`
					Type string `json:"type"`
				} `json:"chat"`
			} `json:"message"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body, &envelope); err != nil {
		t.Fatalf("decode getUpdates response %q: %v", response.Body, err)
	}
	if !envelope.OK {
		t.Fatalf("getUpdates response has ok=false: %q", response.Body)
	}
	return envelope.Result
}

func sendBotAPIProofPhoto(t *testing.T, f *smokeFixture, human *smokeClient, botID int64) error {
	t.Helper()
	body := smokeJPEGAtUploadSize(t, smokePhotoPartCount)
	const fileID, randomID = int64(2819003), int64(2819003)
	return human.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		for part := range smokePhotoPartCount {
			start := part * smokePhotoPartSize
			ok, err := api.UploadSaveFilePart(ctx, &tg.UploadSaveFilePartRequest{
				FileID: fileID, FilePart: part, Bytes: body[start : start+smokePhotoPartSize],
			})
			if err != nil {
				return err
			}
			if !ok {
				return errors.New("photo part upload returned false")
			}
		}
		_, err := api.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
			Peer: peerUser(human.id, botID),
			Media: &tg.InputMediaUploadedPhoto{File: &tg.InputFile{
				ID: fileID, Parts: smokePhotoPartCount, Name: "bot-update.jpg", MD5Checksum: smokePhotoMD5(body),
			}},
			RandomID: randomID,
		})
		return err
	})
}

func sendBotAPIMessage(t *testing.T, ctx context.Context, client *http.Client, serverURL, token string, chatID int64, text string) botAPIHTTPResponse {
	t.Helper()
	body, err := json.Marshal(struct {
		ChatID int64  `json:"chat_id"`
		Text   string `json:"text"`
	}{ChatID: chatID, Text: text})
	if err != nil {
		t.Fatalf("encode Bot API sendMessage: %v", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, serverURL+"/bot"+token+"/sendMessage", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("build Bot API sendMessage request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("Bot API sendMessage request failed: %v", err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Errorf("close Bot API response: %v", err)
		}
	}()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read Bot API response: %v", err)
	}
	return botAPIHTTPResponse{Code: response.StatusCode, Body: responseBody}
}
