package store_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestSaveCloudDraftUnmeteredPersistsAfterMeteredLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	owner := mustUser(t, s, "+1555"+pgtest.RandomHex())
	peer := mustUser(t, s, "+1555"+pgtest.RandomHex())
	send(t, s, peer, owner, "visible dialog", time.Now().UnixNano())
	key := store.PeerDialogKey{PeerType: store.PeerTypeUser, PeerID: peer.ID}
	since := time.Now().Add(-time.Hour)

	for i := range 120 {
		if _, _, err := s.SaveCloudDraft(ctx, owner.ID, key.PeerType, key.PeerID, fmt.Sprintf("metered-%d", i), false, 0); err != nil {
			t.Fatalf("metered cloud draft %d: %v", i+1, err)
		}
	}
	if changed, _, err := s.SaveCloudDraftUnmetered(ctx, owner.ID, key.PeerType, key.PeerID, "tailnet draft", true, 0); err != nil || !changed {
		t.Fatalf("unmetered cloud draft: changed=%v err=%v", changed, err)
	}
	if _, _, err := s.SaveCloudDraft(ctx, owner.ID, key.PeerType, key.PeerID, "denied draft", false, 0); !errors.Is(err, store.ErrCloudDraftRateLimited) {
		t.Fatalf("metered cloud draft after limit = %v, want ErrCloudDraftRateLimited", err)
	}
	state, found, err := s.CloudDraftStateForPeer(ctx, owner.ID, key)
	if err != nil || !found || !state.HasDraft || state.Draft.Message != "tailnet draft" || !state.Draft.NoWebpage {
		t.Fatalf("persisted cloud draft = (%+v, %v, %v), want unmetered value", state, found, err)
	}
	changes, err := s.CloudDraftChangesForOwnerSince(ctx, owner.ID, since)
	if err != nil {
		t.Fatalf("read cloud draft recovery changes: %v", err)
	}
	if len(changes) != 1 || changes[0].Draft.Message != "tailnet draft" || !changes[0].HasDraft {
		t.Fatalf("cloud draft recovery changes = %+v, want the committed unmetered value", changes)
	}
}

func TestMarkDialogUnreadUnmeteredPersistsAfterMeteredLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	owner := mustUser(t, s, "+1555"+pgtest.RandomHex())
	peer := mustUser(t, s, "+1555"+pgtest.RandomHex())
	send(t, s, peer, owner, "visible dialog", time.Now().UnixNano())
	key := store.PeerDialogKey{PeerType: store.PeerTypeUser, PeerID: peer.ID}
	since := time.Now().Add(-time.Hour)

	for range 60 {
		if _, err := s.MarkDialogUnread(ctx, owner.ID, key, true); err != nil {
			t.Fatalf("metered unread mark: %v", err)
		}
	}
	if changed, err := s.MarkDialogUnreadUnmetered(ctx, owner.ID, key, false); err != nil || !changed {
		t.Fatalf("unmetered unread mark: changed=%v err=%v", changed, err)
	}
	if _, err := s.MarkDialogUnread(ctx, owner.ID, key, true); !errors.Is(err, store.ErrDialogUnreadMarkRateLimited) {
		t.Fatalf("metered unread mark after limit = %v, want ErrDialogUnreadMarkRateLimited", err)
	}
	state, found, err := s.DialogUnreadMarkStateForPeer(ctx, owner.ID, key)
	if err != nil || !found || state.Unread {
		t.Fatalf("persisted unread mark = (%+v, %v, %v), want false", state, found, err)
	}
	changes, err := s.DialogUnreadMarkChangesForOwnerSince(ctx, owner.ID, since)
	if err != nil || len(changes) != 1 || changes[0].Unread {
		t.Fatalf("unread mark recovery changes = (%+v, %v), want committed false state", changes, err)
	}
}

func TestUpdateNotifySettingsUnmeteredPersistsAfterMeteredLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	owner := mustUser(t, s, "+1555"+pgtest.RandomHex())
	key := store.NotifySettingsKey{Scope: store.NotifySettingsScopeUsers}
	since := time.Now().Add(-time.Hour)
	show := true
	for range 300 {
		if _, err := s.UpdateNotifySettings(ctx, owner.ID, key, store.NotifySettingsValue{ShowPreviews: &show}); err != nil {
			t.Fatalf("metered notification settings update: %v", err)
		}
	}
	show = false
	if changed, err := s.UpdateNotifySettingsUnmetered(ctx, owner.ID, key, store.NotifySettingsValue{ShowPreviews: &show}); err != nil || !changed {
		t.Fatalf("unmetered notification settings update: changed=%v err=%v", changed, err)
	}
	show = true
	if _, err := s.UpdateNotifySettings(ctx, owner.ID, key, store.NotifySettingsValue{ShowPreviews: &show}); !errors.Is(err, store.ErrNotifySettingsRateLimited) {
		t.Fatalf("metered notification settings update after limit = %v, want ErrNotifySettingsRateLimited", err)
	}
	state, found, err := s.GetNotifySettings(ctx, owner.ID, key)
	if err != nil || !found || state.ShowPreviews == nil || *state.ShowPreviews {
		t.Fatalf("persisted notification settings = (%+v, %v, %v), want show_previews=false", state, found, err)
	}
	changes, err := s.NotifySettingsChangesForOwnerSince(ctx, owner.ID, since, nil)
	if err != nil || len(changes) != 1 || changes[0].Value.ShowPreviews == nil || *changes[0].Value.ShowPreviews {
		t.Fatalf("notification settings recovery changes = (%+v, %v), want committed false value", changes, err)
	}
}

func TestCreateUserStickerSetReportsUnmeteredPersistsAfterMeteredLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	owner := mustUser(t, s, "+1555"+pgtest.RandomHex())
	reporter := mustUser(t, s, "+1555"+pgtest.RandomHex())
	setID, file := seedReportableSet(t, ctx, s, owner, "unmetered_"+pgtest.RandomHex()[:8])
	newReport := func(n int) int64 {
		t.Helper()
		return sendReportableSticker(t, ctx, s, owner, reporter, file, time.Now().UnixNano()+int64(n))
	}
	for i := range store.UserStickerSetReportsPerHour {
		messageID := newReport(i)
		if visible, err := s.CreateUserStickerSetReports(ctx, reporter.ID, store.PeerTypeUser, owner.ID, []int64{messageID}, "copyright", "metered"); err != nil || !visible {
			t.Fatalf("metered report %d: visible=%v err=%v", i+1, visible, err)
		}
	}
	unmeteredMessage := newReport(100)
	if visible, err := s.CreateUserStickerSetReportsUnmetered(ctx, reporter.ID, store.PeerTypeUser, owner.ID, []int64{unmeteredMessage}, "copyright", "tailnet"); err != nil || !visible {
		t.Fatalf("unmetered report: visible=%v err=%v", visible, err)
	}
	deniedMessage := newReport(101)
	if _, err := s.CreateUserStickerSetReports(ctx, reporter.ID, store.PeerTypeUser, owner.ID, []int64{deniedMessage}, "copyright", "metered after limit"); !errors.Is(err, store.ErrUserStickerSetReportRateLimited) {
		t.Fatalf("metered report after limit = %v, want ErrUserStickerSetReportRateLimited", err)
	}
	reports, err := s.AdminOpenUserStickerSetReportsForSets(ctx, []int64{setID}, setID, 0, 50)
	if err != nil {
		t.Fatalf("read persisted reports: %v", err)
	}
	found := false
	for _, report := range reports {
		if report.ReporterID == reporter.ID && report.MessageID == unmeteredMessage && report.Comment == "tailnet" {
			found = true
		}
	}
	if !found {
		t.Fatalf("unmetered report for message %d was not persisted in %+v", unmeteredMessage, reports)
	}
}

func TestUpdateUsernameUnmeteredPersistsAfterMeteredLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	user := mustUser(t, s, "+1555"+pgtest.RandomHex())
	base := "user" + pgtest.RandomHex()[:8]
	for _, username := range []string{base + "a", base + "b"} {
		if err := s.UpdateUsername(ctx, user.ID, username); err != nil {
			t.Fatalf("metered username update %q: %v", username, err)
		}
	}
	want := base + "c"
	if err := s.UpdateUsernameUnmetered(ctx, user.ID, want); err != nil {
		t.Fatalf("unmetered username update: %v", err)
	}
	if err := s.UpdateUsername(ctx, user.ID, base+"d"); !errors.Is(err, store.ErrUsernameFloodWait) {
		t.Fatalf("metered username update after limit = %v, want ErrUsernameFloodWait", err)
	}
	got, found, err := s.UserByID(ctx, user.ID)
	if err != nil || !found || got.Username == nil || *got.Username != want {
		t.Fatalf("persisted username = (%+v, %v, %v), want %q", got.Username, found, err, want)
	}
}
