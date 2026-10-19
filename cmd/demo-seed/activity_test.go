package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

func TestParseActivityMode(t *testing.T) {
	if _, err := parseOptions([]string{"--activity"}); err != nil {
		t.Fatalf("parse activity mode: %v", err)
	}
}

func TestParseActivityConfiguration(t *testing.T) {
	for _, target := range []string{"river", "garden", "workshop"} {
		args := []string{"--activity", "--target", target, "--max-actions-per-hour", "4", "--min-interval", "10m"}
		options, err := parseOptions(args)
		if err != nil {
			t.Fatalf("parse valid activity configuration %q: %v", args, err)
		}
		if options.activityTarget != target || options.maxActionsHour != 4 || options.minActivityGap != 10*time.Minute {
			t.Errorf("parsed activity options = %+v, want target=%q, max=4, interval=10m", options, target)
		}
	}
}

func TestActivityConfigurationCannotExceedSafetyBounds(t *testing.T) {
	for _, args := range [][]string{
		{"--activity", "--target", "welcome"},
		{"--activity", "--target", "teagramdemoriver"},
		{"--activity", "--max-actions-per-hour", "7"},
		{"--activity", "--max-actions-per-hour", "0"},
		{"--activity", "--min-interval", "4m59s"},
	} {
		if _, err := parseOptions(args); err == nil {
			t.Errorf("parseOptions(%q) succeeded; want safety-bound rejection", args)
		}
	}
}

func TestResolveActivityTargetUsesOnlySeededMegagroups(t *testing.T) {
	channel := &tg.Channel{ID: 71, AccessHash: 93, Username: "teagramdemoriver", Title: "Teagram Demo River Walkers", Megagroup: true}
	target, err := resolveActivityTargetFromDialogs("river", &tg.MessagesDialogs{Chats: []tg.ChatClass{channel}})
	if err != nil {
		t.Fatalf("resolve seeded river group: %v", err)
	}
	if target.channelID != channel.ID || target.accessHash != channel.AccessHash || target.username != channel.Username {
		t.Fatalf("resolved target = %+v, want channel %d/%d", target, channel.ID, channel.AccessHash)
	}

	for _, test := range []struct {
		name    string
		slug    string
		channel *tg.Channel
	}{
		{name: "welcome is not an allowed group", slug: "welcome", channel: channel},
		{name: "broadcast channel", slug: "river", channel: &tg.Channel{ID: 71, AccessHash: 93, Username: "teagramdemoriver", Title: "Teagram Demo River Walkers", Broadcast: true}},
		{name: "wrong seeded title", slug: "river", channel: &tg.Channel{ID: 71, AccessHash: 93, Username: "teagramdemoriver", Title: "A stranger's group", Megagroup: true}},
		{name: "target not in joined dialogs", slug: "river", channel: &tg.Channel{ID: 72, AccessHash: 93, Username: "teagramdemogarden", Title: "Teagram Demo Garden Circle", Megagroup: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := resolveActivityTargetFromDialogs(test.slug, &tg.MessagesDialogs{Chats: []tg.ChatClass{test.channel}}); err == nil {
				t.Fatal("resolveActivityTargetFromDialogs succeeded; want rejection")
			}
		})
	}
}

func TestActivitySendRefusesUserPeerBeforeNetworkCall(t *testing.T) {
	api := &fakeActivitySender{}
	target := activityTarget{slug: "river", username: "teagramdemoriver", title: "Teagram Demo River Walkers", channelID: 71, accessHash: 93, megagroup: true}
	peer := &tg.InputPeerUser{UserID: 9, AccessHash: 10}
	if err := sendActivityMessage(context.Background(), api, target, peer, activityCorpus[0], 123, activityRateLimiter{}); err == nil {
		t.Fatal("sendActivityMessage accepted a user peer")
	}
	if api.calls != 0 {
		t.Fatalf("MessagesSendMessage calls = %d, want 0", api.calls)
	}
}

func TestActivitySendUsesOnlyFixedUnmentionedNewPosts(t *testing.T) {
	api := &fakeActivitySender{}
	target := activityTarget{slug: "river", username: "teagramdemoriver", title: "Teagram Demo River Walkers", channelID: 71, accessHash: 93, megagroup: true}
	if err := sendActivityMessage(context.Background(), api, target, target.peer(), activityCorpus[0], 123, activityRateLimiter{}); err != nil {
		t.Fatalf("send fixed activity post: %v", err)
	}
	if api.calls != 1 || api.request == nil {
		t.Fatalf("send calls = %d, request = %+v", api.calls, api.request)
	}
	if _, ok := api.request.Peer.(*tg.InputPeerChannel); !ok {
		t.Fatalf("request peer = %T, want InputPeerChannel", api.request.Peer)
	}
	if api.request.ReplyTo != nil {
		t.Fatal("activity post unexpectedly replies to a message")
	}
	if strings.Contains(api.request.Message, "@") {
		t.Fatalf("activity post contains a mention: %q", api.request.Message)
	}
	if err := sendActivityMessage(context.Background(), api, target, target.peer(), "echo user text", 124, activityRateLimiter{}); err == nil {
		t.Fatal("sendActivityMessage accepted text outside its bundled corpus")
	}
	if api.calls != 1 {
		t.Fatalf("untrusted text caused a network call; calls = %d", api.calls)
	}
}

func TestActivitySendDoesNotRetryFloodWait(t *testing.T) {
	api := &fakeActivitySender{err: tgerr.New(420, "FLOOD_WAIT_1")}
	target := activityTarget{slug: "river", username: "teagramdemoriver", title: "Teagram Demo River Walkers", channelID: 71, accessHash: 93, megagroup: true}
	if err := sendActivityMessage(context.Background(), api, target, target.peer(), activityCorpus[0], 123, newActivityTestRateLimiter(t)); err == nil {
		t.Fatal("sendActivityMessage ignored FLOOD_WAIT")
	}
	if api.calls != 1 {
		t.Fatalf("MessagesSendMessage calls = %d, want one call with no retry", api.calls)
	}
}

func TestActivitySendMarksInvalidSessionForOneRecovery(t *testing.T) {
	api := &fakeActivitySender{err: tgerr.New(401, "AUTH_KEY_UNREGISTERED")}
	target := activityTarget{slug: "river", username: "teagramdemoriver", title: "Teagram Demo River Walkers", channelID: 71, accessHash: 93, megagroup: true}
	if err := sendActivityMessage(context.Background(), api, target, target.peer(), activityCorpus[0], 123, activityRateLimiter{}); !errors.Is(err, errActivitySessionInvalid) {
		t.Fatalf("sendActivityMessage error = %v, want invalid session sentinel", err)
	}
	if api.calls != 1 {
		t.Fatalf("MessagesSendMessage calls = %d, want one call before recovery", api.calls)
	}
}

func TestActivityFloodWaitUsesReportedDuration(t *testing.T) {
	wait, handled := activityFloodWait(tgerr.New(420, "FLOOD_WAIT_73"))
	if !handled || wait != 73*time.Second {
		t.Fatalf("FLOOD_WAIT duration = %s, handled=%t; want 73s, true", wait, handled)
	}
}

func TestFloodWaitPersistsUntilRetryTime(t *testing.T) {
	start := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	now := start
	limiter := activityRateLimiter{
		path:        filepath.Join(t.TempDir(), "rate.json"),
		maxPerHour:  6,
		minInterval: 5 * time.Minute,
		now:         func() time.Time { return now },
	}
	if allowed, err := limiter.reserve(); err != nil || !allowed {
		t.Fatalf("initial reservation = %t, %v; want allowed", allowed, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var observedWait time.Duration
	if err, handled := waitActivityFloodWithSleep(ctx, tgerr.New(420, "FLOOD_WAIT_360"), limiter, func(waitCtx context.Context, wait time.Duration) error {
		if waitCtx.Err() != nil {
			t.Fatalf("flood sleep context is canceled: %v", waitCtx.Err())
		}
		observedWait = wait
		return nil
	}); !handled || err == nil {
		t.Fatalf("waitActivityFlood = %v, handled=%t; want handled flood wait", err, handled)
	}
	if observedWait != 360*time.Second {
		t.Fatalf("wait duration = %s, want 6m", observedWait)
	}

	now = start.Add(359 * time.Second)
	if allowed, err := limiter.reserve(); err != nil || allowed {
		t.Fatalf("reservation before flood wait expires = %t, %v; want denied", allowed, err)
	}
	now = start.Add(360 * time.Second)
	if allowed, err := limiter.reserve(); err != nil || !allowed {
		t.Fatalf("reservation after flood wait expires = %t, %v; want allowed", allowed, err)
	}
}

func TestActivityRateLimitPersistsMinimumIntervalAndHourlyCeiling(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rate.json")
	start := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	now := start
	limiter := activityRateLimiter{path: path, maxPerHour: 6, minInterval: 5 * time.Minute, now: func() time.Time { return now }}
	if allowed, err := limiter.reserve(); err != nil || !allowed {
		t.Fatalf("first reservation = %t, %v; want allowed", allowed, err)
	}
	now = start.Add(4*time.Minute + 59*time.Second)
	limiter = activityRateLimiter{path: path, maxPerHour: 6, minInterval: 5 * time.Minute, now: func() time.Time { return now }}
	if allowed, err := limiter.reserve(); err != nil || allowed {
		t.Fatalf("reservation inside minimum interval = %t, %v; want denied", allowed, err)
	}

	for action := 1; action < 6; action++ {
		now = start.Add(time.Duration(action) * 10 * time.Minute)
		limiter = activityRateLimiter{path: path, maxPerHour: 6, minInterval: 5 * time.Minute, now: func() time.Time { return now }}
		if allowed, err := limiter.reserve(); err != nil || !allowed {
			t.Fatalf("reservation %d = %t, %v; want allowed", action+1, allowed, err)
		}
	}
	now = start.Add(59 * time.Minute)
	limiter = activityRateLimiter{path: path, maxPerHour: 6, minInterval: 5 * time.Minute, now: func() time.Time { return now }}
	if allowed, err := limiter.reserve(); err != nil || allowed {
		t.Fatalf("seventh reservation in rolling hour = %t, %v; want denied", allowed, err)
	}
	now = start.Add(time.Hour)
	limiter = activityRateLimiter{path: path, maxPerHour: 6, minInterval: 5 * time.Minute, now: func() time.Time { return now }}
	if allowed, err := limiter.reserve(); err != nil || !allowed {
		t.Fatalf("reservation after oldest action expires = %t, %v; want allowed", allowed, err)
	}
}

func TestSeedActivityAccountIsDedicatedAndOrdinaryGroupMember(t *testing.T) {
	var activity accountSpec
	count := 0
	for _, account := range seedAccounts {
		if account.activity {
			activity = account
			count++
		}
	}
	if count != 1 || activity.owner || activity.username != "demo_tavi" {
		t.Fatalf("activity account = %+v, count=%d; want one non-owner demo_tavi", activity, count)
	}
	for _, group := range groupMemberAccounts() {
		if group.username == activity.username {
			return
		}
	}
	t.Fatalf("activity account %q is not a normal seeded group member", activity.username)
}

func TestActivityCredentialRequiresPrivateInventoryAndSeedAccount(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "inventory.json")
	data := []byte(`{"demo_tavi":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}`)
	if err := os.WriteFile(path, data, 0400); err != nil {
		t.Fatal(err)
	}
	account := accountSpec{loginID: "demo_tavi", activity: true}
	password, err := readActivityPasswordAt(path, account)
	if err != nil {
		t.Fatalf("read activity credential: %v", err)
	}
	if password != "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" {
		t.Fatal("readActivityPasswordAt returned a different credential")
	}
	if err := os.Chmod(path, 0644); err != nil { // #nosec G302 -- deliberately weaken permissions to test rejection.
		t.Fatal(err)
	}
	if _, err := readActivityPasswordAt(path, account); err == nil {
		t.Fatal("readActivityPasswordAt accepted a group/world-readable credential")
	}
	if err := os.WriteFile(path, []byte(`{"demo_iona":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}`), 0400); err != nil {
		t.Fatal(err)
	}
	if _, err := readActivityPasswordAt(path, account); err == nil {
		t.Fatal("readActivityPasswordAt accepted an inventory without the dedicated account")
	}
}

func TestActivitySessionMustBelongToDedicatedSeedAccount(t *testing.T) {
	account := accountSpec{username: "demo_tavi", activity: true}
	if !isActivitySessionUser([]tg.UserClass{&tg.User{Username: "demo_tavi"}}, account) {
		t.Fatal("isActivitySessionUser rejected the dedicated seed account")
	}
	if isActivitySessionUser([]tg.UserClass{&tg.User{Username: "demo_ren"}}, account) {
		t.Fatal("isActivitySessionUser accepted a different demo account")
	}
	if isActivitySessionUser([]tg.UserClass{&tg.User{Username: "demo_tavi"}}, accountSpec{username: "demo_tavi"}) {
		t.Fatal("isActivitySessionUser accepted an account without the activity marker")
	}
}

type fakeActivitySender struct {
	calls   int
	request *tg.MessagesSendMessageRequest
	err     error
}

func newActivityTestRateLimiter(t *testing.T) activityRateLimiter {
	t.Helper()
	return activityRateLimiter{
		path:        filepath.Join(t.TempDir(), "rate.json"),
		maxPerHour:  6,
		minInterval: 5 * time.Minute,
		now:         time.Now,
	}
}

func (api *fakeActivitySender) MessagesSendMessage(_ context.Context, request *tg.MessagesSendMessageRequest) (tg.UpdatesClass, error) {
	api.calls++
	api.request = request
	return &tg.Updates{}, api.err
}
