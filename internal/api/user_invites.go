package api

import (
	"errors"
	"regexp"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/linklanding"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

const (
	createUserInviteRequestTypeID uint32 = 0xbf4f8b0e
	listUserInvitesRequestTypeID  uint32 = 0x2ad32052
	revokeUserInviteRequestTypeID uint32 = 0xa483022c
	userInviteLinkTypeID          uint32 = 0x0078b3b0
	userInviteTypeID              uint32 = 0xff81dc6c
	userInvitesTypeID             uint32 = 0xb5cfec46

	// TG_PUBLIC_LINK_PREFIX always ends in '/', so appending this reserved path
	// segment produces /invite#<secret>.
	userInviteLandingPath = linklanding.InviteLandingPathSegment
)

var userInviteCredentialPattern = regexp.MustCompile(`(?i)[0-9a-f]{64}`)

type userInviteLinkResponse struct {
	ID        int64
	Link      string
	ExpiresAt time.Time
}

func (r userInviteLinkResponse) Encode(b *bin.Buffer) error {
	b.PutID(userInviteLinkTypeID)
	b.PutLong(r.ID)
	b.PutString(r.Link)
	b.PutInt(int(r.ExpiresAt.Unix()))
	return nil
}

type userInviteResponse struct {
	ID        int64
	State     string
	CreatedAt time.Time
	ExpiresAt time.Time
}

func (r userInviteResponse) encode(b *bin.Buffer) {
	b.PutID(userInviteTypeID)
	b.PutLong(r.ID)
	b.PutString(r.State)
	b.PutInt(int(r.CreatedAt.Unix()))
	b.PutInt(int(r.ExpiresAt.Unix()))
}

type userInvitesResponse struct {
	Invites         []userInviteResponse
	Quota           int64
	WeeklyCount     int64
	NextAvailableAt *time.Time
}

func (r userInvitesResponse) Encode(b *bin.Buffer) error {
	b.PutID(userInvitesTypeID)
	b.PutVectorHeader(len(r.Invites))
	for _, invite := range r.Invites {
		invite.encode(b)
	}
	b.PutLong(r.Quota)
	b.PutLong(r.WeeklyCount)
	if r.NextAvailableAt == nil {
		b.PutInt(0)
	} else {
		b.PutInt(1)
		b.PutLong(r.NextAvailableAt.Unix())
	}
	return nil
}

func (h *handlers) handleCreateUserInvite(r *mtproto.Request) (bin.Encoder, error) {
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	if h.registrationMode != config.RegistrationInvite {
		return nil, errUserInviteRegistrationClosed
	}
	if h.userInviteQuota <= 0 {
		return nil, errUserInvitesDisabled
	}
	if err := decodeUserInviteRequest(r.Buf, createUserInviteRequestTypeID); err != nil {
		return nil, err
	}
	if h.store == nil || h.cfg == nil {
		return nil, errInternal
	}
	invite, secret, err := h.store.CreateUserInvite(r.Ctx, r.UserID, h.userInviteQuota, h.userInviteMinAccountAge)
	if err != nil {
		var tooNew *store.UserInviteAccountTooNewError
		switch {
		case errors.Is(err, store.ErrUserInviteQuotaReached):
			return nil, errUserInviteWeeklyLimitReached
		case errors.As(err, &tooNew):
			return nil, userInviteAccountTooNewError(tooNew.RemainingWaitSeconds)
		default:
			h.logUserInviteStoreFailure("create", r.UserID, err)
			return nil, errInternal
		}
	}
	return userInviteLinkResponse{
		ID:        invite.ID,
		Link:      h.cfg.MeURLPrefix + userInviteLandingPath + "#" + secret,
		ExpiresAt: invite.ExpiresAt,
	}, nil
}

func (h *handlers) handleListUserInvites(r *mtproto.Request) (bin.Encoder, error) {
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	if err := decodeUserInviteRequest(r.Buf, listUserInvitesRequestTypeID); err != nil {
		return nil, err
	}
	if h.store == nil {
		return nil, errInternal
	}
	invites, weeklyCount, nextAvailableAt, err := h.store.ListUserInvites(r.Ctx, r.UserID)
	if err != nil {
		h.logUserInviteStoreFailure("list", r.UserID, err)
		return nil, errInternal
	}
	response := userInvitesResponse{
		Invites:         make([]userInviteResponse, len(invites)),
		Quota:           int64(h.userInviteQuota),
		WeeklyCount:     weeklyCount,
		NextAvailableAt: nextAvailableAt,
	}
	for i, invite := range invites {
		response.Invites[i] = userInviteResponse{
			ID:        invite.ID,
			State:     userInviteStateForClient(invite.State),
			CreatedAt: invite.IssuedAt,
			ExpiresAt: invite.ExpiresAt,
		}
	}
	return response, nil
}

func (h *handlers) handleRevokeUserInvite(r *mtproto.Request) (bin.Encoder, error) {
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	id, err := decodeUserInviteIDRequest(r.Buf, revokeUserInviteRequestTypeID)
	if err != nil {
		return nil, err
	}
	if h.store == nil {
		return nil, errInternal
	}
	if err := h.store.RevokeUserInvite(r.Ctx, r.UserID, id); err != nil {
		h.logUserInviteStoreFailure("revoke", r.UserID, err)
		return nil, errInternal
	}
	return &tg.BoolTrue{}, nil
}

func (h *handlers) logUserInviteStoreFailure(operation string, userID int64, err error) {
	h.log.Error("user invite store failure",
		"operation", operation,
		"user_id", userID,
		"err", userInviteCredentialPattern.ReplaceAllString(err.Error(), "[redacted]"),
	)
}

func decodeUserInviteRequest(b *bin.Buffer, constructor uint32) error {
	id, err := b.ID()
	if err != nil || id != constructor {
		return errMethodNotImpl
	}
	if b.Len() != 0 {
		return errMethodNotImpl
	}
	return nil
}

func decodeUserInviteIDRequest(b *bin.Buffer, constructor uint32) (int64, error) {
	id, err := b.ID()
	if err != nil || id != constructor {
		return 0, errMethodNotImpl
	}
	inviteID, err := b.Long()
	if err != nil || b.Len() != 0 {
		return 0, errMethodNotImpl
	}
	return inviteID, nil
}

func userInviteStateForClient(state store.InviteState) string {
	switch state {
	case store.InviteIssued:
		return "pending"
	case store.InviteConsumed:
		return "used"
	case store.InviteExpired:
		return "expired"
	case store.InviteRevoked:
		return "revoked"
	default:
		return "expired"
	}
}
