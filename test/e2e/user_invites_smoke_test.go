package e2e_test

import (
	"fmt"
	"regexp"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/config"
)

const (
	smokeCreateUserInviteRequestID uint32 = 0xbf4f8b0e
	smokeListUserInvitesRequestID  uint32 = 0x2ad32052
	smokeRevokeUserInviteRequestID uint32 = 0xa483022c
	smokeUserInviteLinkTypeID      uint32 = 0x0078b3b0
	smokeUserInviteTypeID          uint32 = 0xff81dc6c
	smokeUserInvitesTypeID         uint32 = 0xb5cfec46
)

type smokeUserInviteRequest struct {
	constructor uint32
	id          int64
	hasID       bool
}

func (r smokeUserInviteRequest) TypeID() uint32 { return r.constructor }

func (r smokeUserInviteRequest) Encode(b *bin.Buffer) error {
	b.PutID(r.constructor)
	if r.hasID {
		b.PutLong(r.id)
	}
	return nil
}

type smokeUserInviteLink struct {
	ID        int64
	Link      string
	ExpiresAt int
}

func (r *smokeUserInviteLink) Decode(b *bin.Buffer) error {
	constructor, err := b.ID()
	if err != nil {
		return err
	}
	if constructor != smokeUserInviteLinkTypeID {
		return fmt.Errorf("user invite link constructor %#08x", constructor)
	}
	if r.ID, err = b.Long(); err != nil {
		return err
	}
	if r.Link, err = b.String(); err != nil {
		return err
	}
	if r.ExpiresAt, err = b.Int(); err != nil {
		return err
	}
	if b.Len() != 0 {
		return fmt.Errorf("user invite link has %d trailing bytes", b.Len())
	}
	return nil
}

type smokeUserInviteItem struct {
	ID        int64
	State     string
	CreatedAt int
	ExpiresAt int
}

type smokeUserInvites struct {
	Items           []smokeUserInviteItem
	Quota           int64
	WeeklyCount     int64
	NextAvailableAt *int64
}

func (r *smokeUserInvites) Decode(b *bin.Buffer) error {
	constructor, err := b.ID()
	if err != nil {
		return err
	}
	if constructor != smokeUserInvitesTypeID {
		return fmt.Errorf("user invites constructor %#08x", constructor)
	}
	vector, err := b.ID()
	if err != nil {
		return err
	}
	if vector != bin.TypeVector {
		return fmt.Errorf("user invites vector constructor %#08x", vector)
	}
	count, err := b.Int()
	if err != nil {
		return err
	}
	if count < 0 || count > 100 {
		return fmt.Errorf("user invites count %d is outside [0, 100]", count)
	}
	r.Items = make([]smokeUserInviteItem, count)
	for i := range r.Items {
		itemConstructor, err := b.ID()
		if err != nil {
			return err
		}
		if itemConstructor != smokeUserInviteTypeID {
			return fmt.Errorf("user invite %d constructor %#08x", i, itemConstructor)
		}
		item := &r.Items[i]
		if item.ID, err = b.Long(); err != nil {
			return err
		}
		if item.State, err = b.String(); err != nil {
			return err
		}
		if item.CreatedAt, err = b.Int(); err != nil {
			return err
		}
		if item.ExpiresAt, err = b.Int(); err != nil {
			return err
		}
	}
	if r.Quota, err = b.Long(); err != nil {
		return err
	}
	if r.WeeklyCount, err = b.Long(); err != nil {
		return err
	}
	flags, err := b.Int()
	if err != nil {
		return err
	}
	if flags&^1 != 0 {
		return fmt.Errorf("user invites flags %d contain unknown bits", flags)
	}
	if flags&1 != 0 {
		next, err := b.Long()
		if err != nil {
			return err
		}
		r.NextAvailableAt = &next
	}
	if b.Len() != 0 {
		return fmt.Errorf("user invites has %d trailing bytes", b.Len())
	}
	return nil
}

func testSmokeUserInvites(t *testing.T) {
	t.Helper()
	f := newSmokeFixtureWithSetup(t, config.RegistrationInvite, func(f *smokeFixture) {
		f.userInviteQuota = 1
		f.userInviteMinAccountAge = 0
	})
	const phone = "+15551028371"
	seedSmokeUsers(t, f, phone)
	client := newSmokeClient(t, f, "user invite smoke", phone)

	var created smokeUserInviteLink
	if err := client.client.Invoke(f.ctx, smokeUserInviteRequest{constructor: smokeCreateUserInviteRequestID}, &created); err != nil {
		t.Fatalf("teagram.createUserInvite: %v", err)
	}
	secretPattern := regexp.MustCompile(`^` + regexp.QuoteMeta(testPublicLinkPrefix+"invite#") + `[0-9a-f]{64}$`)
	if !secretPattern.MatchString(created.Link) {
		t.Fatalf("create invite link = %q, want fixed invite path and lowercase fragment secret", created.Link)
	}
	if delta := time.Until(time.Unix(int64(created.ExpiresAt), 0)); delta < 7*24*time.Hour-2*time.Second || delta > 7*24*time.Hour+2*time.Second {
		t.Fatalf("invite expiry is %s from now, want seven days", delta)
	}

	var listed smokeUserInvites
	if err := client.client.Invoke(f.ctx, smokeUserInviteRequest{constructor: smokeListUserInvitesRequestID}, &listed); err != nil {
		t.Fatalf("teagram.listUserInvites: %v", err)
	}
	if listed.Quota != 1 || listed.WeeklyCount != 1 || len(listed.Items) != 1 || listed.NextAvailableAt == nil {
		t.Fatalf("list summary = quota %d, count %d, items %+v, next %v", listed.Quota, listed.WeeklyCount, listed.Items, listed.NextAvailableAt)
	}
	if listed.Items[0].ID != created.ID || listed.Items[0].State != "pending" || listed.Items[0].CreatedAt <= 0 || listed.Items[0].ExpiresAt != created.ExpiresAt {
		t.Fatalf("listed invite = %+v, want metadata for created invite %d", listed.Items[0], created.ID)
	}
	if want := int64(listed.Items[0].CreatedAt) + int64(7*24*time.Hour/time.Second); *listed.NextAvailableAt != want {
		t.Fatalf("next available at = %d, want oldest creation plus seven days (%d)", *listed.NextAvailableAt, want)
	}

	var revoked tg.BoolTrue
	if err := client.client.Invoke(f.ctx, smokeUserInviteRequest{constructor: smokeRevokeUserInviteRequestID, id: created.ID, hasID: true}, &revoked); err != nil {
		t.Fatalf("teagram.revokeUserInvite: %v", err)
	}
	var afterRevoke smokeUserInvites
	if err := client.client.Invoke(f.ctx, smokeUserInviteRequest{constructor: smokeListUserInvitesRequestID}, &afterRevoke); err != nil {
		t.Fatalf("teagram.listUserInvites after revoke: %v", err)
	}
	if afterRevoke.WeeklyCount != 1 || len(afterRevoke.Items) != 1 || afterRevoke.Items[0].State != "revoked" {
		t.Fatalf("list after revoke = count %d items %+v, want one revoked invite still counting", afterRevoke.WeeklyCount, afterRevoke.Items)
	}
}
