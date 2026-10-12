package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/teagramhq/teagram-server/internal/store"
)

func TestEvaluateLastSeenPrivacyPrecedence(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	owner := mustUser(t, s, "+15551917001")
	blocked := mustUser(t, s, "+15551917002")
	disallowed := mustUser(t, s, "+15551917003")
	allowed := mustUser(t, s, "+15551917004")
	contact := mustUser(t, s, "+15551917005")
	nonContact := mustUser(t, s, "+15551917006")

	setLastSeenRule(t, s, owner.ID, "contacts")
	setLastSeenEntry(t, s, owner.ID, blocked.ID, "allow")
	setLastSeenEntry(t, s, owner.ID, disallowed.ID, "allow")
	setLastSeenEntry(t, s, owner.ID, disallowed.ID, "disallow")
	setLastSeenEntry(t, s, owner.ID, allowed.ID, "allow")
	if changed, err := s.AddContact(ctx, owner.ID, contact.ID); err != nil || !changed {
		t.Fatalf("add owner's contact: changed=%v err=%v", changed, err)
	}
	if changed, err := s.BlockUser(ctx, owner.ID, blocked.ID); err != nil || !changed {
		t.Fatalf("block viewer: changed=%v err=%v", changed, err)
	}

	seenAt := time.Now().Add(-2 * 24 * time.Hour)
	owner.IsOnline = false
	owner.LastSeenAt = &seenAt
	got, err := s.EvaluateLastSeenPrivacy(ctx, owner, []int64{
		owner.ID,
		blocked.ID,
		disallowed.ID,
		allowed.ID,
		contact.ID,
		nonContact.ID,
	})
	if err != nil {
		t.Fatalf("evaluate privacy: %v", err)
	}

	assertLastSeenVisibility(t, got[owner.ID], store.LastSeenAccessCoarse, store.LastSeenBucketRecently, false)
	assertLastSeenVisibility(t, got[blocked.ID], store.LastSeenAccessHidden, store.LastSeenBucketEmpty, false)
	assertLastSeenVisibility(t, got[disallowed.ID], store.LastSeenAccessCoarse, store.LastSeenBucketRecently, false)
	assertLastSeenVisibility(t, got[allowed.ID], store.LastSeenAccessExact, store.LastSeenBucketEmpty, false)
	assertLastSeenVisibility(t, got[contact.ID], store.LastSeenAccessExact, store.LastSeenBucketEmpty, false)
	assertLastSeenVisibility(t, got[nonContact.ID], store.LastSeenAccessCoarse, store.LastSeenBucketRecently, false)
}

func TestEvaluateLastSeenPrivacyContactsUseOwnerDirectedEdge(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	owner := mustUser(t, s, "+15551917101")
	viewer := mustUser(t, s, "+15551917102")
	setLastSeenRule(t, s, owner.ID, "contacts")
	now := time.Date(2026, time.October, 12, 12, 0, 0, 0, time.UTC)
	store.SetNowFunc(s, func() time.Time { return now })
	twoDaysAgo := now.Add(-2 * 24 * time.Hour)
	owner.LastSeenAt = &twoDaysAgo

	if changed, err := s.AddContact(ctx, viewer.ID, owner.ID); err != nil || !changed {
		t.Fatalf("add owner to viewer's contacts: changed=%v err=%v", changed, err)
	}
	got, err := s.EvaluateLastSeenPrivacy(ctx, owner, []int64{viewer.ID})
	if err != nil {
		t.Fatalf("evaluate one-way contact: %v", err)
	}
	assertLastSeenVisibility(t, got[viewer.ID], store.LastSeenAccessCoarse, store.LastSeenBucketRecently, false)
	twoDayVisibility := got[viewer.ID]

	owner.IsOnline = true
	onlineNow := now
	owner.LastSeenAt = &onlineNow
	got, err = s.EvaluateLastSeenPrivacy(ctx, owner, []int64{viewer.ID})
	if err != nil {
		t.Fatalf("evaluate online one-way contact: %v", err)
	}
	if got[viewer.ID] != twoDayVisibility {
		t.Fatalf("online visibility = %+v, two-day visibility = %+v", got[viewer.ID], twoDayVisibility)
	}

	if changed, err := s.AddContact(ctx, owner.ID, viewer.ID); err != nil || !changed {
		t.Fatalf("add viewer to owner's contacts: changed=%v err=%v", changed, err)
	}
	got, err = s.EvaluateLastSeenPrivacy(ctx, owner, []int64{viewer.ID})
	if err != nil {
		t.Fatalf("evaluate owner-directed contact: %v", err)
	}
	assertLastSeenVisibility(t, got[viewer.ID], store.LastSeenAccessExact, store.LastSeenBucketEmpty, false)
}

func TestEvaluateLastSeenPrivacyReciprocity(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	owner := mustUser(t, s, "+15551917201")
	viewer := mustUser(t, s, "+15551917202")
	setLastSeenRule(t, s, owner.ID, "everybody")
	setLastSeenRule(t, s, viewer.ID, "nobody")

	got, err := s.EvaluateLastSeenPrivacy(ctx, owner, []int64{viewer.ID})
	if err != nil {
		t.Fatalf("evaluate reciprocal restriction: %v", err)
	}
	assertLastSeenVisibility(t, got[viewer.ID], store.LastSeenAccessCoarse, store.LastSeenBucketEmpty, true)

	setLastSeenEntry(t, s, viewer.ID, owner.ID, "allow")
	got, err = s.EvaluateLastSeenPrivacy(ctx, owner, []int64{viewer.ID})
	if err != nil {
		t.Fatalf("evaluate reciprocal allow: %v", err)
	}
	assertLastSeenVisibility(t, got[viewer.ID], store.LastSeenAccessExact, store.LastSeenBucketEmpty, false)

	ownerRestrictedViewer := mustUser(t, s, "+15551917203")
	setLastSeenRule(t, s, owner.ID, "contacts")
	setLastSeenRule(t, s, ownerRestrictedViewer.ID, "nobody")
	got, err = s.EvaluateLastSeenPrivacy(ctx, owner, []int64{ownerRestrictedViewer.ID})
	if err != nil {
		t.Fatalf("evaluate mutually restricted status: %v", err)
	}
	assertLastSeenVisibility(t, got[ownerRestrictedViewer.ID], store.LastSeenAccessCoarse, store.LastSeenBucketEmpty, true)
}

func TestEvaluateLastSeenPrivacyBuckets(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	owner := mustUser(t, s, "+15551917301")
	viewer := mustUser(t, s, "+15551917302")
	setLastSeenRule(t, s, owner.ID, "nobody")

	now := time.Date(2026, time.October, 12, 12, 0, 0, 0, time.UTC)
	store.SetNowFunc(s, func() time.Time { return now })
	for _, test := range []struct {
		name     string
		online   bool
		lastSeen *time.Time
		want     store.LastSeenBucket
	}{
		{name: "online", online: true, lastSeen: new(now.Add(-2 * 24 * time.Hour)), want: store.LastSeenBucketRecently},
		{name: "two days ago", lastSeen: new(now.Add(-2 * 24 * time.Hour)), want: store.LastSeenBucketRecently},
		{name: "at three days", lastSeen: new(now.Add(-3 * 24 * time.Hour)), want: store.LastSeenBucketRecently},
		{name: "after three days", lastSeen: new(now.Add(-3*24*time.Hour - time.Nanosecond)), want: store.LastSeenBucketLastWeek},
		{name: "at seven days", lastSeen: new(now.Add(-7 * 24 * time.Hour)), want: store.LastSeenBucketLastWeek},
		{name: "after seven days", lastSeen: new(now.Add(-7*24*time.Hour - time.Nanosecond)), want: store.LastSeenBucketLastMonth},
		{name: "at thirty days", lastSeen: new(now.Add(-30 * 24 * time.Hour)), want: store.LastSeenBucketLastMonth},
		{name: "after thirty days", lastSeen: new(now.Add(-30*24*time.Hour - time.Nanosecond)), want: store.LastSeenBucketEmpty},
		{name: "never seen", lastSeen: nil, want: store.LastSeenBucketEmpty},
	} {
		t.Run(test.name, func(t *testing.T) {
			owner.IsOnline = test.online
			owner.LastSeenAt = test.lastSeen
			got, err := s.EvaluateLastSeenPrivacy(ctx, owner, []int64{viewer.ID})
			if err != nil {
				t.Fatalf("evaluate bucket: %v", err)
			}
			assertLastSeenVisibility(t, got[viewer.ID], store.LastSeenAccessCoarse, test.want, false)
		})
	}

	owner.IsOnline = true
	owner.LastSeenAt = new(now)
	online, err := s.EvaluateLastSeenPrivacy(ctx, owner, []int64{viewer.ID})
	if err != nil {
		t.Fatalf("evaluate online status: %v", err)
	}
	owner.IsOnline = false
	owner.LastSeenAt = new(now.Add(-2 * 24 * time.Hour))
	twoDaysAgo, err := s.EvaluateLastSeenPrivacy(ctx, owner, []int64{viewer.ID})
	if err != nil {
		t.Fatalf("evaluate two-day status: %v", err)
	}
	if online[viewer.ID] != twoDaysAgo[viewer.ID] {
		t.Fatalf("online visibility = %+v, two-day visibility = %+v", online[viewer.ID], twoDaysAgo[viewer.ID])
	}
}

func TestEvaluateLastSeenPrivacyDefaultsToEverybodyAndFailsClosed(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	owner := mustUser(t, s, "+15551917401")
	viewer := mustUser(t, s, "+15551917402")

	got, err := s.EvaluateLastSeenPrivacy(ctx, owner, []int64{viewer.ID})
	if err != nil {
		t.Fatalf("evaluate missing rule: %v", err)
	}
	assertLastSeenVisibility(t, got[viewer.ID], store.LastSeenAccessExact, store.LastSeenBucketEmpty, false)

	var zero store.LastSeenVisibility
	assertLastSeenVisibility(t, zero, store.LastSeenAccessHidden, store.LastSeenBucketEmpty, false)
	got, err = s.EvaluateLastSeenPrivacy(ctx, owner, []int64{viewer.ID + 1000})
	if err == nil {
		t.Fatal("evaluation for missing viewer succeeded")
	}
	if len(got) != 0 {
		t.Fatalf("missing viewer evaluation returned results: %+v", got)
	}

	if _, err := store.StorePool(s).Exec(ctx, `DROP TABLE user_privacy_rule_users`); err != nil {
		t.Fatalf("drop rule entries table: %v", err)
	}
	got, err = s.EvaluateLastSeenPrivacy(ctx, owner, []int64{viewer.ID})
	if err == nil {
		t.Fatal("evaluation without rule storage succeeded")
	}
	if len(got) != 0 {
		t.Fatalf("failed evaluation returned visibility results: %+v", got)
	}
}

func assertLastSeenVisibility(t *testing.T, got store.LastSeenVisibility, level store.LastSeenAccess, bucket store.LastSeenBucket, byMe bool) {
	t.Helper()
	if got.Level != level || got.Bucket != bucket || got.ByMe != byMe {
		t.Errorf("visibility = %+v, want level=%d bucket=%d byMe=%v", got, level, bucket, byMe)
	}
}

func setLastSeenRule(t *testing.T, s *store.Store, ownerID int64, baseRule string) {
	t.Helper()
	_, err := store.StorePool(s).Exec(context.Background(), `
		INSERT INTO user_privacy_rules (owner_id, privacy_key, base_rule)
		VALUES ($1, 'status_timestamp', $2)
		ON CONFLICT (owner_id, privacy_key) DO UPDATE SET base_rule = EXCLUDED.base_rule`, ownerID, baseRule)
	if err != nil {
		t.Fatalf("set last-seen rule: %v", err)
	}
}

func setLastSeenEntry(t *testing.T, s *store.Store, ownerID, userID int64, entryType string) {
	t.Helper()
	_, err := store.StorePool(s).Exec(context.Background(), `
		INSERT INTO user_privacy_rule_users (owner_id, privacy_key, user_id, entry_type)
		VALUES ($1, 'status_timestamp', $2, $3)`, ownerID, userID, entryType)
	if err != nil {
		t.Fatalf("set last-seen %s entry: %v", entryType, err)
	}
}
