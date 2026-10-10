package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/teagramhq/teagram-server/internal/store/db"
)

// ChannelEvent is one entry from the per-channel ordered event log. It mirrors
// Event: channel_events.type uses the same 1 new / 2 edit / 3 delete codes as
// message_events, so EventType carries both.
type ChannelEvent struct {
	Pts     int
	Type    EventType
	LocalID int64
}

// ErrChannelMessageDeleteForbidden is returned when the caller is a member but
// the channel role or requested live post does not permit deletion.
var ErrChannelMessageDeleteForbidden = errors.New("channel message delete forbidden")

// ErrChannelStateExhausted reports that a channel pts or post id cannot
// advance without exceeding Telegram's signed int32 wire range.
var ErrChannelStateExhausted = errors.New("channel update state exhausted")

func requireChannelStateCapacity(channelID, pts, nextLocalID, ptsAdvances, localIDAdvances int64) error {
	maxWireValue := int64(math.MaxInt32)
	if ptsAdvances > 0 && (pts < 0 || pts > maxWireValue || ptsAdvances > maxWireValue-pts) {
		return fmt.Errorf("%w: channel %d pts %d", ErrChannelStateExhausted, channelID, pts)
	}
	if localIDAdvances > 0 && (nextLocalID < 1 || nextLocalID > maxWireValue || localIDAdvances > maxWireValue-nextLocalID+1) {
		return fmt.Errorf("%w: channel %d next_local_id %d", ErrChannelStateExhausted, channelID, nextLocalID)
	}
	return nil
}

func bumpChannelState(ctx context.Context, q *db.Queries, channelID int64) (db.BumpChannelStateRow, error) {
	row, err := q.BumpChannelState(ctx, channelID)
	if err == nil {
		return row, nil
	}
	return row, classifyChannelStateBumpError(ctx, q, channelID, err, true)
}

func bumpChannelPtsOnly(ctx context.Context, q *db.Queries, channelID int64) (int64, error) {
	pts, err := q.BumpChannelPtsOnly(ctx, channelID)
	if err == nil {
		return pts, nil
	}
	return pts, classifyChannelStateBumpError(ctx, q, channelID, err, false)
}

func classifyChannelStateBumpError(ctx context.Context, q *db.Queries, channelID int64, cause error, needsLocalID bool) error {
	if !errors.Is(cause, pgx.ErrNoRows) {
		return cause
	}
	state, err := q.GetChannelState(ctx, channelID)
	if errors.Is(err, pgx.ErrNoRows) {
		return cause
	}
	if err != nil {
		return fmt.Errorf("read channel state after refused bump: %w", err)
	}
	var localIDAdvances int64
	if needsLocalID {
		localIDAdvances = 1
	}
	if capacityErr := requireChannelStateCapacity(channelID, state.Pts, state.NextLocalID, 1, localIDAdvances); capacityErr != nil {
		return capacityErr
	}
	return cause
}

// SlowModeWaitError reports how many seconds an ordinary megagroup member must
// wait before their next distinct post.
type SlowModeWaitError struct {
	Seconds int
}

func (e *SlowModeWaitError) Error() string {
	return fmt.Sprintf("SLOWMODE_WAIT_%d", e.Seconds)
}

// ChannelMessage is a persisted channel message. Unlike Message there is one row
// per channel rather than one per member, so it carries no owner and no Out.
// FileID is nil for "no media" — channel_messages.file_id is a nullable FK,
// where the older messages.file_id uses 0 as that sentinel.
//
// Field order is load-bearing: channelMessageFromFields constructs this type
// with a positional literal, so the field sequence here is the column mapping
// contract. Do not reorder fields without updating that function.
type ChannelMessage struct {
	ChannelID int64
	LocalID   int64
	FromID    int64
	Date      time.Time
	Message   string
	EditDate  *time.Time
	Deleted   bool
	RandomID  int64
	FileID    *int64
	// ReplyToMsgID is the local_id of the post this post replies to; 0 = no reply.
	ReplyToMsgID int32
	Action       ChannelMessageAction
	Poll         *Poll
}

// ChannelMessageAction identifies a service message stored in a channel's
// shared message stream. Zero is a regular post.
type ChannelMessageAction int16

const (
	ChannelMessageActionNone ChannelMessageAction = iota
	ChannelMessageActionCreate
)

// channelMsgFields is a layout-identical copy of the sqlc channel-message
// row types. Any of them converts to this type via a plain type conversion, so
// the single channelMessageFromFields function below is the only place that maps
// database columns to ChannelMessage fields.
type channelMsgFields struct {
	ChannelID    int64
	LocalID      int64
	FromID       int64
	Date         pgtype.Timestamptz
	Message      string
	EditDate     pgtype.Timestamptz
	Deleted      bool
	RandomID     int64
	FileID       *int64
	ReplyToMsgID *int32
	ActionType   int16
}

// channelMessageFromFields is the sole row-to-struct converter for channel
// messages. The positional ChannelMessage literal is intentional: it causes a
// compile error if a new field is added to ChannelMessage without being wired
// in here, rather than silently leaving it at its zero value.
func channelMessageFromFields(r channelMsgFields) ChannelMessage {
	var editDate *time.Time
	if r.EditDate.Valid {
		t := r.EditDate.Time
		editDate = &t
	}
	var replyToMsgID int32
	if r.ReplyToMsgID != nil {
		replyToMsgID = *r.ReplyToMsgID
	}
	message := ChannelMessage{
		r.ChannelID,
		r.LocalID,
		r.FromID,
		r.Date.Time,
		r.Message,
		editDate,
		r.Deleted,
		r.RandomID,
		r.FileID,
		replyToMsgID,
		ChannelMessageAction(r.ActionType),
		nil,
	}
	if message.Deleted {
		message.FromID = 0
		message.Date = time.Time{}
		message.Message = ""
		message.EditDate = nil
		message.RandomID = 0
		message.FileID = nil
		message.ReplyToMsgID = 0
		message.Action = ChannelMessageActionNone
	}
	return message
}

// ChannelState returns the channel's current pts. A channel that has never been
// created before channel-create service messages were added may have pts 0, so
// a difference against them works from the moment the channel exists.
//
// A channel id that does not exist reports 0 as well: this is not an existence
// check and cannot be used as one. Callers gate on the channel and on the
// caller's membership of it before they get here, as everywhere else in this
// package.
func (s *Store) ChannelState(ctx context.Context, channelID int64) (int, error) {
	row, err := s.q.GetChannelState(ctx, channelID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("get channel state: %w", err)
	}
	return int(row.Pts), nil
}

// DeleteChannelMessages tombstones the authorized live posts in one channel.
// Missing and already-deleted ids are skipped, so retries append no duplicate
// events. The transaction locks channel state, then the caller's current
// participant row, then requested posts in ascending local-id order.
func (s *Store) DeleteChannelMessages(ctx context.Context, channelID, userID int64, localIDs []int64) (int, int, error) {
	if channelID == 0 || userID == 0 {
		return 0, 0, ErrNotMember
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("begin channel message delete: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)

	state, err := qtx.LockChannelState(ctx, channelID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, 0, ErrNotMember
	case err != nil:
		return 0, 0, fmt.Errorf("lock channel state for delete: %w", err)
	}

	participant, err := qtx.ChannelParticipantForDelete(ctx, db.ChannelParticipantForDeleteParams{
		ChannelID: channelID,
		UserID:    userID,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, 0, ErrNotMember
	case err != nil:
		return 0, 0, fmt.Errorf("lock channel participant for delete: %w", err)
	}

	megagroup, err := qtx.ChannelMegagroup(ctx, channelID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, 0, ErrNotMember
	case err != nil:
		return 0, 0, fmt.Errorf("read channel kind for delete: %w", err)
	}
	if !megagroup && participant.Role < channelRoleAdmin {
		return 0, 0, ErrChannelMessageDeleteForbidden
	}

	rows, err := qtx.ChannelMessagesForDelete(ctx, db.ChannelMessagesForDeleteParams{
		ChannelID: channelID,
		LocalIds:  localIDs,
	})
	if err != nil {
		return 0, 0, fmt.Errorf("lock channel messages for delete: %w", err)
	}
	for _, row := range rows {
		if row.Deleted {
			continue
		}
		if row.ActionType != int16(ChannelMessageActionNone) ||
			megagroup && participant.Role == channelRoleMember && row.FromID != userID {
			return 0, 0, ErrChannelMessageDeleteForbidden
		}
	}
	var deletes int64
	for _, row := range rows {
		if !row.Deleted {
			deletes++
		}
	}
	if err = requireChannelStateCapacity(channelID, state.Pts, state.NextLocalID, deletes, 0); err != nil {
		return 0, 0, err
	}

	pts := int(state.Pts)
	count := 0
	for _, row := range rows {
		if row.Deleted {
			continue
		}
		updated, e := qtx.TombstoneChannelMessage(ctx, db.TombstoneChannelMessageParams{
			ChannelID: channelID,
			LocalID:   row.LocalID,
		})
		if e != nil {
			return 0, 0, fmt.Errorf("tombstone channel message %d: %w", row.LocalID, e)
		}
		if updated != 1 {
			return 0, 0, fmt.Errorf("tombstone channel message %d: updated %d rows, want 1", row.LocalID, updated)
		}
		newPts, e := bumpChannelPtsOnly(ctx, qtx, channelID)
		if e != nil {
			return 0, 0, fmt.Errorf("bump channel pts for delete %d: %w", row.LocalID, e)
		}
		if e = qtx.InsertChannelEvent(ctx, db.InsertChannelEventParams{
			ChannelID: channelID,
			Pts:       newPts,
			Type:      int16(EventDelete),
			LocalID:   row.LocalID,
		}); e != nil {
			return 0, 0, fmt.Errorf("insert channel delete event %d: %w", row.LocalID, e)
		}
		pts = int(newPts)
		count++
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, 0, fmt.Errorf("commit channel message delete: %w", err)
	}
	return pts, count, nil
}

// newChannelPostPts returns the pts at which the channel's localID entered the
// log. Same contract as newMessagePts, including ErrPtsUnknown.
func newChannelPostPts(ctx context.Context, q *db.Queries, channelID, localID int64) (int, error) {
	pts, err := q.NewChannelPostPts(ctx, db.NewChannelPostPtsParams{ChannelID: channelID, LocalID: localID})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, fmt.Errorf("%w: channel %d post %d", ErrPtsUnknown, channelID, localID)
	case err != nil:
		return 0, fmt.Errorf("new channel post pts: %w", err)
	}
	return int(pts), nil
}

// ChannelPostPts returns the pts at which localID entered the channel's log —
// the only pts a resend of that post may report. ErrPtsUnknown when the log has
// no such event.
func (s *Store) ChannelPostPts(ctx context.Context, channelID, localID int64) (int, error) {
	return newChannelPostPts(ctx, s.q, channelID, localID)
}

// PostChannelMessage appends one post to the channel: the channel_state bump
// that allocates its local_id and pts, the channel_messages row and the
// channel_events row, all in one transaction. Keeping the event and the bump in
// one transaction is what makes the log's contract hold — an event row present
// implies channel_state.pts is at least that event's pts.
//
// A repeat carrying the same non-zero randomID returns the already-stored post
// with dup == true, writing nothing and leaving pts where it was.
//
// replyToMsgID is the local_id of the post this post replies to; 0 = no reply.
// A non-zero replyToMsgID that names a deleted or absent post returns ErrMessageInvalid.
//
// This layer trusts its caller: it does not check that fromID may post here.
//
// Locking: no Go-level lock and no advisory lock, only the channel_state row
// lock, taken by the LockChannelState read ahead of the dedup lookup and held to
// commit. The channel_messages foreign key checks channel_id with KEY SHARE on
// channels; that is compatible with LockChannel's NO KEY UPDATE lock, allowing
// the post to commit while an invite waits for channel_state.
func (s *Store) PostChannelMessage(
	ctx context.Context, channelID, fromID int64, text string, randomID int64, fileID *int64, replyToMsgID int64,
) (ChannelMessage, int, bool, error) {
	return s.postChannelMessage(ctx, channelID, fromID, text, randomID, fileID, replyToMsgID, false, nil, nil, false)
}

// PostChannelMessageAs is PostChannelMessage with the post-rights check
// performed inside the same transaction, under the channel_state row lock. It is
// the entry point a handler uses; PostChannelMessage stays the unchecked
// primitive underneath it.
//
// The rule, and it fails closed on anything not listed: a broadcast channel
// (megagroup = false) admits role >= 1 only, a megagroup admits any participant
// row, and a member banned at now() is admitted by neither. No participant row
// is a rejection. Every rejection is ErrNotMember, the same error the chat
// fan-out uses, because a distinct "you are banned" tells an outsider the
// channel exists.
//
// Locking and ordering, which is the point of this function existing rather than
// the check living in a handler: the channel row and the caller's participant
// row are read AFTER LockChannelState and before any write, inside the
// transaction that does the insert. The same state lock serializes slow-mode
// checks and marker updates with posts; the marker row is updated only after
// authorization, deduplication, restrictions, reply validation and event
// creation succeed. Role, ban and leave mutations can hold channels before a
// participant row, but do not then acquire channel_state; admission takes
// channel_state before participant insertion. A handler-level check runs in its
// own transaction, so a member banned concurrently would still land a post.
// That ordering is what every future channel write inherits.
func (s *Store) PostChannelMessageAs(
	ctx context.Context, channelID, fromID int64, text string, randomID int64, fileID *int64, replyToMsgID int64,
) (ChannelMessage, int, bool, error) {
	return s.postChannelMessage(ctx, channelID, fromID, text, randomID, fileID, replyToMsgID, true, nil, nil, false)
}

// ChannelPhotoRetryAs resolves a photo-send retry only after the caller's
// current channel posting rights have been checked under the channel
// state lock. It follows ChannelPollRetryAs rather than ChannelTextMessageRetryAs
// because a photo retry names a media post: the stored row must be the caller's
// own live ordinary post carrying a persisted photo, and anything else is a
// refusal rather than a replay. Slow mode and default restrictions do not
// apply to a committed retry, and a new random id creates no post here.
func (s *Store) ChannelPhotoRetryAs(
	ctx context.Context, channelID, fromID, randomID int64,
) (ChannelMessage, int, bool, error) {
	if channelID == 0 || fromID == 0 {
		return ChannelMessage{}, 0, false, ErrMessageInvalid
	}
	if randomID == 0 {
		return ChannelMessage{}, 0, false, nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ChannelMessage{}, 0, false, fmt.Errorf("begin channel photo retry: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)

	// Same shape as the post transaction: an early reject that decides nothing
	// but keeps an outsider from probing random ids at all, then the
	// authoritative check under the state lock and the participant lock.
	if _, err = checkPostRights(ctx, qtx, channelID, fromID); err != nil {
		return ChannelMessage{}, 0, false, err
	}
	if err = qtx.EnsureChannelState(ctx, channelID); err != nil {
		return ChannelMessage{}, 0, false, fmt.Errorf("ensure channel state for photo retry: %w", err)
	}
	if _, err = qtx.LockChannelState(ctx, channelID); err != nil {
		return ChannelMessage{}, 0, false, fmt.Errorf("lock channel state for photo retry: %w", err)
	}
	if _, err = checkChannelMediaPostRights(ctx, qtx, channelID, fromID); err != nil {
		return ChannelMessage{}, 0, false, err
	}

	message, pts, duplicate, err := channelMessageRetry(ctx, qtx, channelID, fromID, randomID, nil, false, true)
	if err != nil {
		return ChannelMessage{}, 0, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return ChannelMessage{}, 0, false, fmt.Errorf("commit channel photo retry: %w", err)
	}
	if !duplicate && s.channelPhotoRetryAfterCommitHook != nil {
		// The transaction has committed, so this test-only rendezvous holds no
		// transaction or channel-state lock while the caller waits.
		if err = s.channelPhotoRetryAfterCommitHook(ctx, channelID, fromID, randomID); err != nil {
			return ChannelMessage{}, 0, false, fmt.Errorf("after channel photo retry commit hook: %w", err)
		}
	}
	return message, pts, duplicate, nil
}

// SetChannelPhotoRetryAfterCommitHook installs the test-only synchronization
// seam used by API concurrency tests. Production callers leave it nil.
func SetChannelPhotoRetryAfterCommitHook(s *Store, fn func(context.Context, int64, int64, int64) error) {
	s.channelPhotoRetryAfterCommitHook = fn
}

// CheckChannelPhotoPostPermission is the read-only gate a channel photo send
// runs before it assembles an upload. Assembly is the expensive part of
// a send — a blob Put of up to the photo cap per attempt — so a sender who may
// not post media here must be turned away before it, exactly as
// CheckChatWritePermission does for a basic group.
//
// It takes no lock and decides nothing: the authoritative admission, restriction
// and slow-mode decision is the one PostChannelPhotoAs makes under the
// channel_state row lock. Holding that lock across a blob Put would park every
// other poster in the channel for the length of the Put, which is why this
// precheck exists at all rather than the authoritative check moving earlier.
func (s *Store) CheckChannelPhotoPostPermission(ctx context.Context, channelID, callerID int64, mediaRights []string) error {
	row, err := s.q.ChannelParticipantByUser(ctx, db.ChannelParticipantByUserParams{ChannelID: channelID, UserID: callerID})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ErrNotMember
	case err != nil:
		return fmt.Errorf("channel photo precheck participant: %w", err)
	}
	member := channelMemberFromRow(row)
	if member.Banned(time.Now()) {
		return ErrNotMember
	}
	channel, err := s.q.ChannelPostDefaults(ctx, channelID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ErrNotMember
	case err != nil:
		return fmt.Errorf("channel photo precheck defaults: %w", err)
	}
	if !channel.Megagroup {
		// Broadcast posting is an admin right and default banned rights do not
		// apply to it, same as for text.
		if member.Role < channelRoleAdmin {
			return ErrNotMember
		}
		return nil
	}
	if err = checkDefaultMessageRestriction(channel.DefaultBannedRights, member.Role >= channelRoleAdmin, true, mediaRights); err != nil {
		return err
	}
	if member.Role != channelRoleMember {
		return nil
	}
	state, err := s.q.ChannelSlowModePostState(ctx, db.ChannelSlowModePostStateParams{
		ChannelID: channelID,
		UserID:    callerID,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ErrNotMember
	case err != nil:
		return fmt.Errorf("channel photo precheck slow mode: %w", err)
	}
	if state.SlowmodeSeconds > 0 && state.LastPostAt.Valid {
		remaining := time.Duration(state.SlowmodeSeconds)*time.Second - state.CheckedAt.Time.Sub(state.LastPostAt.Time)
		if remaining > 0 {
			return &SlowModeWaitError{Seconds: int((remaining + time.Second - 1) / time.Second)}
		}
	}
	return nil
}

// PostChannelPhotoAs admits a photo as a channel post, and the file it names
// as a protected reference, in one transaction.
//
// The file id is the one the caller's own send just allocated (or the one
// already on the caller's deduplicated post); it is never a client-named id.
// The rules this entry point enforces under the channel_state row lock are the
// ones PostChannelMessageAs documents for text, plus three that only media
// needs: the megagroup restriction is judged on the file's persisted
// subtype_rights rather than on what the request restates, the file row takes
// the shared reference lock immediately before the post insert so a send
// racing the eraser fails closed instead of surfacing the RESTRICT foreign key
// as an internal error, and a random_id replay must name a post whose stored
// media really is a photo.
func (s *Store) PostChannelPhotoAs(
	ctx context.Context, channelID, fromID, randomID int64, text string, fileID int64, replyToMsgID int64,
) (ChannelMessage, int, bool, error) {
	if fileID == 0 {
		return ChannelMessage{}, 0, false, ErrFileMissing
	}
	id := fileID
	return s.postChannelMessage(ctx, channelID, fromID, text, randomID, &id, replyToMsgID, true, nil, nil, true)
}

// ChannelTextMessageRetryAs resolves a text-send retry only after checking the
// caller's current channel posting rights under the channel state lock. It does
// not apply slow mode or create a post when the random id is new.
func (s *Store) ChannelTextMessageRetryAs(
	ctx context.Context, channelID, fromID, randomID int64,
) (ChannelMessage, int, bool, error) {
	if channelID == 0 || fromID == 0 {
		return ChannelMessage{}, 0, false, ErrMessageInvalid
	}
	if randomID == 0 {
		return ChannelMessage{}, 0, false, nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ChannelMessage{}, 0, false, fmt.Errorf("begin channel text retry: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)

	// Reject before EnsureChannelState so an absent or unauthorized channel
	// cannot be distinguished by probing a random id.
	if _, err = checkPostRights(ctx, qtx, channelID, fromID); err != nil {
		return ChannelMessage{}, 0, false, err
	}
	if err = qtx.EnsureChannelState(ctx, channelID); err != nil {
		return ChannelMessage{}, 0, false, fmt.Errorf("ensure channel state for text retry: %w", err)
	}
	if _, err = qtx.LockChannelState(ctx, channelID); err != nil {
		return ChannelMessage{}, 0, false, fmt.Errorf("lock channel state for text retry: %w", err)
	}
	if _, err = checkPostRights(ctx, qtx, channelID, fromID); err != nil {
		return ChannelMessage{}, 0, false, err
	}

	message, pts, duplicate, err := channelMessageRetry(ctx, qtx, channelID, fromID, randomID, nil, true, false)
	if err != nil {
		return ChannelMessage{}, 0, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return ChannelMessage{}, 0, false, fmt.Errorf("commit channel text retry: %w", err)
	}
	return message, pts, duplicate, nil
}

// PostChannelPollAs atomically admits a poll as a channel post, stores its
// canonical poll and links it to the shared channel message before commit.
func (s *Store) PostChannelPollAs(
	ctx context.Context, channelID, fromID, randomID int64, text string, draft PollDraft,
) (ChannelMessage, Poll, int, bool, error) {
	var poll Poll
	message, pts, duplicate, err := s.postChannelMessage(ctx, channelID, fromID, text, randomID, nil, 0, true, &draft, &poll, false)
	return message, poll, pts, duplicate, err
}

// ChannelPollRetryAs resolves an existing poll resend only after the caller's
// current channel posting rights have been checked under the channel state
// lock. It does not apply slow mode or create a post when the random id is new.
func (s *Store) ChannelPollRetryAs(
	ctx context.Context, channelID, fromID, randomID int64,
) (ChannelMessage, Poll, int, bool, error) {
	var poll Poll
	if channelID == 0 || fromID == 0 {
		return ChannelMessage{}, poll, 0, false, ErrMessageInvalid
	}
	if randomID == 0 {
		return ChannelMessage{}, poll, 0, false, nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ChannelMessage{}, poll, 0, false, fmt.Errorf("begin channel poll retry: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)

	// Match PostChannelPollAs: reject outsiders before EnsureChannelState, then
	// repeat the authorization check under the channel state lock before reading
	// the random id. A retry must not turn that id into a membership oracle.
	if _, err = checkPostRights(ctx, qtx, channelID, fromID); err != nil {
		return ChannelMessage{}, poll, 0, false, err
	}
	if err = qtx.EnsureChannelState(ctx, channelID); err != nil {
		return ChannelMessage{}, poll, 0, false, fmt.Errorf("ensure channel state for poll retry: %w", err)
	}
	if _, err = qtx.LockChannelState(ctx, channelID); err != nil {
		return ChannelMessage{}, poll, 0, false, fmt.Errorf("lock channel state for poll retry: %w", err)
	}
	if _, err = checkChannelMediaPostRights(ctx, qtx, channelID, fromID); err != nil {
		return ChannelMessage{}, poll, 0, false, err
	}

	message, pts, duplicate, err := channelMessageRetry(ctx, qtx, channelID, fromID, randomID, &poll, false, false)
	if err != nil {
		return ChannelMessage{}, poll, 0, false, err
	}
	return message, poll, pts, duplicate, nil
}

func (s *Store) postChannelMessage(
	ctx context.Context, channelID, fromID int64, text string, randomID int64, fileID *int64, replyToMsgID int64, checkRights bool,
	pollDraft *PollDraft, pollResult *Poll, photoMedia bool,
) (ChannelMessage, int, bool, error) {
	if channelID == 0 || fromID == 0 {
		return ChannelMessage{}, 0, false, ErrMessageInvalid
	}
	if pollDraft != nil {
		if _, err := normalizePollDraftShape(*pollDraft); err != nil {
			return ChannelMessage{}, 0, false, err
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ChannelMessage{}, 0, false, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)

	// Early reject, ahead of EnsureChannelState. It decides nothing — the
	// authoritative check is the identical call under the row lock below — but
	// channel_state.channel_id REFERENCES channels (id), so letting a caller with
	// no rights reach the insert turns a channel that does not exist into an FK
	// error instead of ErrNotMember, and that distinct error is exactly the
	// existence oracle the one-error rule closes. Same shape as the fan-out's
	// pre-lock IsChatMember reject in fanout.go.
	if checkRights {
		if _, err = checkPostRights(ctx, qtx, channelID, fromID); err != nil {
			return ChannelMessage{}, 0, false, err
		}
	}

	if err = qtx.EnsureChannelState(ctx, channelID); err != nil {
		return ChannelMessage{}, 0, false, fmt.Errorf("ensure channel state: %w", err)
	}
	channelState, lockErr := qtx.LockChannelState(ctx, channelID)
	if lockErr != nil {
		return ChannelMessage{}, 0, false, fmt.Errorf("lock channel state: %w", lockErr)
	}

	// The authoritative check: under the row lock taken above, before the dedup
	// read and before any write, so a ban committing concurrently is seen. A
	// caller with no right to post here must not be able to probe random_ids or
	// slow-mode state either.
	var role int
	if checkRights {
		if pollDraft != nil || photoMedia {
			role, err = checkChannelMediaPostRights(ctx, qtx, channelID, fromID)
		} else {
			role, err = checkPostRights(ctx, qtx, channelID, fromID)
		}
		if err != nil {
			return ChannelMessage{}, 0, false, err
		}
	}

	// Idempotency: a resend with the same random_id returns the original post,
	// with no row, no event and no pts movement — same rule as the chat and 1:1
	// send paths, read under the row lock so two concurrent resends agree.
	//
	// The pts reported is the one that post occupies, not the channel's current
	// pts: a subscriber applies updateNewChannelMessage by pts, so naming a
	// newer slot for an old post is how it skips whatever really sits there.
	if randomID != 0 {
		message, pts, duplicate, retryErr := channelMessageRetry(
			ctx, qtx, channelID, fromID, randomID, pollResult, fileID == nil && pollDraft == nil, photoMedia,
		)
		if retryErr != nil {
			return ChannelMessage{}, 0, false, retryErr
		}
		if duplicate {
			return message, pts, true, nil
		}
	}
	var megagroup bool
	if checkRights {
		channel, e := qtx.ChannelPostDefaults(ctx, channelID)
		switch {
		case errors.Is(e, pgx.ErrNoRows):
			return ChannelMessage{}, 0, false, ErrNotMember
		case e != nil:
			return ChannelMessage{}, 0, false, fmt.Errorf("channel post defaults: %w", e)
		}
		megagroup = channel.Megagroup
		if pollDraft != nil && !channel.Megagroup && pollDraft.PublicVoters {
			return ChannelMessage{}, 0, false, ErrBroadcastPublicVotersForbidden
		}
		if channel.Megagroup {
			var mediaRights []string
			if pollDraft != nil {
				mediaRights = []string{"send_polls"}
			}
			if fileID != nil {
				media, ferr := qtx.FileMediaRightsForPost(ctx, *fileID)
				switch {
				case errors.Is(ferr, pgx.ErrNoRows):
					return ChannelMessage{}, 0, false, ErrFileMissing
				case ferr != nil:
					return ChannelMessage{}, 0, false, fmt.Errorf("channel post file media rights: %w", ferr)
				}
				mediaRights = media.SubtypeRights
			}
			if err = checkDefaultMessageRestriction(channel.DefaultBannedRights, role >= channelRoleAdmin, fileID != nil || pollDraft != nil, mediaRights); err != nil {
				return ChannelMessage{}, 0, false, err
			}
		}
	}

	if checkRights && megagroup && role == channelRoleMember {
		state, e := qtx.ChannelSlowModePostState(ctx, db.ChannelSlowModePostStateParams{
			ChannelID: channelID,
			UserID:    fromID,
		})
		switch {
		case errors.Is(e, pgx.ErrNoRows):
			return ChannelMessage{}, 0, false, ErrNotMember
		case e != nil:
			return ChannelMessage{}, 0, false, fmt.Errorf("channel slow-mode post state: %w", e)
		}
		if state.SlowmodeSeconds > 0 && state.LastPostAt.Valid {
			remaining := time.Duration(state.SlowmodeSeconds)*time.Second - state.CheckedAt.Time.Sub(state.LastPostAt.Time)
			if remaining > 0 {
				waitSeconds := int((remaining + time.Second - 1) / time.Second)
				return ChannelMessage{}, 0, false, &SlowModeWaitError{Seconds: waitSeconds}
			}
		}
	}

	// Validate reply_to_msg_id under the lock, before any write. A deleted or
	// absent post returns ErrMessageInvalid without distinguishing the two cases,
	// so the channel's post ids are not an existence oracle for another channel.
	var replyTo *int32
	if replyToMsgID > 0 {
		_, verr := qtx.ChannelPostExistsActive(ctx, db.ChannelPostExistsActiveParams{
			ChannelID: channelID, LocalID: replyToMsgID,
		})
		if errors.Is(verr, pgx.ErrNoRows) {
			return ChannelMessage{}, 0, false, ErrMessageInvalid
		}
		if verr != nil {
			return ChannelMessage{}, 0, false, fmt.Errorf("reply_to_msg_id check: %w", verr)
		}
		v := int32(replyToMsgID) //nolint:gosec // G115: local_id fits int32 wire space
		replyTo = &v
	}

	// The file-reference interlock, taken after channel_state, rights, dedup,
	// restriction and reply validation, and before the post row exists: this is
	// the only thing that makes a send and an eraser agree about a file. Without
	// it the eraser can delete the row between the rights read above and the
	// insert, and the RESTRICT foreign key on channel_messages.file_id surfaces as
	// an internal error plus a dangling reference attempt rather than the clean
	// MEDIA_INVALID this returns. files is this transaction's last lock class, so
	// nothing taken below it — the post row's key-share on channels, the
	// participant marker — can reverse the order against the eraser.
	var refFileID int64
	if fileID != nil {
		refFileID = *fileID
	}
	if err = lockFileRefs(ctx, qtx, refFileID); err != nil {
		return ChannelMessage{}, 0, false, err
	}

	if err = requireChannelStateCapacity(channelID, channelState.Pts, channelState.NextLocalID, 1, 1); err != nil {
		return ChannelMessage{}, 0, false, err
	}
	b, err := bumpChannelState(ctx, qtx, channelID)
	if err != nil {
		return ChannelMessage{}, 0, false, fmt.Errorf("bump channel state: %w", err)
	}
	if err = qtx.InsertChannelMessage(ctx, db.InsertChannelMessageParams{
		ChannelID: channelID, LocalID: b.LocalID, FromID: fromID,
		Message: text, RandomID: randomID, FileID: fileID, ReplyToMsgID: replyTo,
	}); err != nil {
		return ChannelMessage{}, 0, false, fmt.Errorf("insert channel message: %w", err)
	}
	if err = qtx.InsertChannelEvent(ctx, db.InsertChannelEventParams{
		ChannelID: channelID, Pts: b.Pts, Type: int16(EventNewMessage), LocalID: b.LocalID,
	}); err != nil {
		return ChannelMessage{}, 0, false, fmt.Errorf("insert channel event: %w", err)
	}
	if pollDraft != nil {
		poll, e := createChannelPollTx(ctx, qtx, channelID, fromID, randomID, b.LocalID, *pollDraft, s.now(), s.newPollID)
		if e != nil {
			return ChannelMessage{}, 0, false, e
		}
		*pollResult = poll
	}
	if checkRights {
		n, e := qtx.UpdateChannelPostMarker(ctx, db.UpdateChannelPostMarkerParams{
			ChannelID: channelID,
			UserID:    fromID,
		})
		if e != nil {
			return ChannelMessage{}, 0, false, fmt.Errorf("update channel post marker: %w", e)
		}
		if n != 1 {
			return ChannelMessage{}, 0, false, ErrNotMember
		}
	}

	stored, err := qtx.ChannelMessageByLocal(ctx, db.ChannelMessageByLocalParams{
		ChannelID: channelID, LocalID: b.LocalID,
	})
	if err != nil {
		return ChannelMessage{}, 0, false, fmt.Errorf("reload channel message: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return ChannelMessage{}, 0, false, fmt.Errorf("commit: %w", err)
	}
	message := channelMessageFromFields(channelMsgFields(stored))
	if pollResult != nil {
		message.Poll = pollResult
	}
	return message, int(b.Pts), false, nil
}

func channelMessageRetry(
	ctx context.Context, qtx *db.Queries, channelID, fromID, randomID int64, pollResult *Poll, textOnly, photoRetry bool,
) (ChannelMessage, int, bool, error) {
	existing, err := qtx.ChannelMessageByRandomID(ctx, db.ChannelMessageByRandomIDParams{
		ChannelID: channelID, RandomID: randomID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ChannelMessage{}, 0, false, nil
	}
	if err != nil {
		return ChannelMessage{}, 0, false, fmt.Errorf("random_id lookup: %w", err)
	}
	if existing.Deleted || existing.FromID != fromID || existing.ActionType != int16(ChannelMessageActionNone) {
		return ChannelMessage{}, 0, false, ErrRandomIDDuplicate
	}
	var retryPoll *Poll
	if pollResult != nil {
		pollRow, pollErr := qtx.PollByChannelMessage(ctx, db.PollByChannelMessageParams{
			ChannelID: channelID,
			LocalID:   existing.LocalID,
		})
		if errors.Is(pollErr, pgx.ErrNoRows) {
			return ChannelMessage{}, 0, false, ErrPollInvalid
		}
		if pollErr != nil {
			return ChannelMessage{}, 0, false, fmt.Errorf("poll retry lookup: %w", pollErr)
		}
		poll, pollErr := pollView(ctx, qtx, pollRow, fromID)
		if pollErr != nil {
			return ChannelMessage{}, 0, false, pollErr
		}
		retryPoll = &poll
	}
	if textOnly {
		if existing.FileID != nil {
			return ChannelMessage{}, 0, false, ErrRandomIDDuplicate
		}
		_, pollErr := qtx.PollByChannelMessage(ctx, db.PollByChannelMessageParams{
			ChannelID: channelID,
			LocalID:   existing.LocalID,
		})
		if pollErr == nil {
			return ChannelMessage{}, 0, false, ErrRandomIDDuplicate
		}
		if !errors.Is(pollErr, pgx.ErrNoRows) {
			return ChannelMessage{}, 0, false, fmt.Errorf("check text retry poll kind: %w", pollErr)
		}
	}
	if photoRetry {
		// The persisted kind, not the request: a photo resend must land on the
		// photo post that send created, after a restart and after any later
		// restriction, and a random id naming the caller's text post, poll or
		// document post is a different send rather than a replay of this one.
		if existing.FileID == nil {
			return ChannelMessage{}, 0, false, ErrMediaInvalid
		}
		media, ferr := qtx.FileMediaRightsForPost(ctx, *existing.FileID)
		if ferr != nil && !errors.Is(ferr, pgx.ErrNoRows) {
			return ChannelMessage{}, 0, false, fmt.Errorf("check photo retry media kind: %w", ferr)
		}
		if errors.Is(ferr, pgx.ErrNoRows) || media.MediaKind != string(FileKindPhoto) {
			return ChannelMessage{}, 0, false, ErrMediaInvalid
		}
	}
	pts, err := newChannelPostPts(ctx, qtx, channelID, existing.LocalID)
	if err != nil {
		return ChannelMessage{}, 0, false, err
	}
	message := channelMessageFromFields(channelMsgFields(existing))
	if retryPoll != nil {
		*pollResult = *retryPoll
		message.Poll = retryPoll
	}
	return message, pts, true, nil
}

// checkChannelMediaPostRights is checkPostRights with the caller's participant
// row locked FOR UPDATE. Media sends take it because the expensive part of the
// request happens before the transaction that admits it: a ban, leave or
// demotion that commits while an upload is being assembled must be seen by the
// authoritative check, so the check cannot settle for a snapshot read.
// Polls already took this lock; photos inherit it.
func checkChannelMediaPostRights(ctx context.Context, qtx *db.Queries, channelID, fromID int64) (int, error) {
	row, err := qtx.ChannelPollParticipantForUpdate(ctx, db.ChannelPollParticipantForUpdateParams{
		ChannelID: channelID,
		UserID:    fromID,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, ErrNotMember
	case err != nil:
		return 0, fmt.Errorf("lock channel media participant: %w", err)
	}
	member := channelMemberFromRow(row)
	if member.Banned(time.Now()) {
		return 0, ErrNotMember
	}
	megagroup, err := qtx.ChannelMegagroup(ctx, channelID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, ErrNotMember
	case err != nil:
		return 0, fmt.Errorf("channel media kind: %w", err)
	}
	if !megagroup && member.Role < channelRoleAdmin {
		return 0, ErrNotMember
	}
	return member.Role, nil
}

// checkPostRights answers whether fromID is a current, unbanned channel poster.
// No slow-mode comparison occurs until post authorization and default
// restrictions have passed.
// Default message restrictions are checked after the dedup read so a committed
// retry remains idempotent. Every rejection here is ErrNotMember; see
// PostChannelMessageAs for why they are not distinguishable.
func checkPostRights(ctx context.Context, qtx *db.Queries, channelID, fromID int64) (int, error) {
	row, err := qtx.ChannelParticipantByUser(ctx, db.ChannelParticipantByUserParams{
		ChannelID: channelID, UserID: fromID,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, ErrNotMember
	case err != nil:
		return 0, fmt.Errorf("channel participant: %w", err)
	}

	member := channelMemberFromRow(row)
	if member.Banned(time.Now()) {
		return 0, ErrNotMember
	}

	megagroup, err := qtx.ChannelMegagroup(ctx, channelID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, ErrNotMember
	case err != nil:
		return 0, fmt.Errorf("channel kind: %w", err)
	}
	// Broadcast: posting is an admin right. Megagroup: any unbanned participant.
	if !megagroup && member.Role < 1 {
		return 0, ErrNotMember
	}
	return member.Role, nil
}

// ChannelEventsWindow returns the channel's events in (fromPts, toPts] ordered
// ascending, at most limit of them. Bounding by toPts (a previously-read state
// pts) keeps the difference from advertising a pts past an event it did not
// return.
func (s *Store) ChannelEventsWindow(ctx context.Context, channelID int64, fromPts, toPts, limit int) ([]ChannelEvent, error) {
	rows, err := s.q.ChannelEventsWindow(ctx, db.ChannelEventsWindowParams{
		ChannelID: channelID,
		FromPts:   int64(fromPts),
		ToPts:     int64(toPts),
		Lim:       int32(limit), //nolint:gosec // limit is a small server-set cap
	})
	if err != nil {
		return nil, fmt.Errorf("channel events window: %w", err)
	}
	events := make([]ChannelEvent, len(rows))
	for i, r := range rows {
		events[i] = ChannelEvent{Pts: int(r.Pts), Type: EventType(r.Type), LocalID: r.LocalID}
	}
	return events, nil
}

// ChannelMessages returns the requested posts keyed by local_id, with deleted
// rows retained as payload-free tombstones so an event can still name the post
// it removed. Unlike ChannelHistory, it includes those tombstone markers. Ids
// with no row are simply absent from the map.
func (s *Store) ChannelMessages(ctx context.Context, channelID int64, localIDs []int64) (map[int64]ChannelMessage, error) {
	if len(localIDs) == 0 {
		return map[int64]ChannelMessage{}, nil
	}
	rows, err := s.q.ChannelMessagesByLocalIDs(ctx, db.ChannelMessagesByLocalIDsParams{
		ChannelID: channelID, LocalIds: localIDs,
	})
	if err != nil {
		return nil, fmt.Errorf("channel messages: %w", err)
	}
	tombstones, err := s.q.ChannelMessageTombstonesByLocalIDs(ctx, db.ChannelMessageTombstonesByLocalIDsParams{
		ChannelID: channelID, LocalIds: localIDs,
	})
	if err != nil {
		return nil, fmt.Errorf("channel message tombstones: %w", err)
	}
	out := make(map[int64]ChannelMessage, len(rows))
	for _, r := range rows {
		out[r.LocalID] = channelMessageFromFields(channelMsgFields(r))
	}
	for _, row := range tombstones {
		out[row.LocalID] = ChannelMessage{ChannelID: row.ChannelID, LocalID: row.LocalID, Deleted: true}
	}
	return out, nil
}

// ChannelHistory returns the channel's posts newest-first, excluding deleted.
// offsetID > 0 pages strictly older than that local_id (0 = from the newest),
// the same convention History uses for user peers.
func (s *Store) ChannelHistory(ctx context.Context, channelID int64, offsetID int64, limit int) ([]ChannelMessage, error) {
	rows, err := s.q.ChannelHistoryPage(ctx, db.ChannelHistoryPageParams{
		ChannelID: channelID,
		OffsetID:  offsetID,
		Lim:       int32(limit), //nolint:gosec // limit is a small validated page size
	})
	if err != nil {
		return nil, fmt.Errorf("channel history page: %w", err)
	}
	msgs := make([]ChannelMessage, len(rows))
	for i, r := range rows {
		msgs[i] = channelMessageFromFields(channelMsgFields(r))
	}
	return msgs, nil
}

// ChannelHistoryWithOffset returns one Telegram-style history page and the
// channel's total number of non-deleted history entries. Offset position is
// computed against the unfiltered history; maxID and minID filter the slice.
func (s *Store) ChannelHistoryWithOffset(
	ctx context.Context,
	channelID, offsetID, addOffset, maxID, minID int64,
	limit int,
) ([]ChannelMessage, int, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("begin channel history snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)

	count, err := qtx.CountChannelHistory(ctx, channelID)
	if err != nil {
		return nil, 0, fmt.Errorf("count channel history: %w", err)
	}
	var msgs []ChannelMessage
	if offsetID > 0 && addOffset < 0 {
		rows, e := qtx.ChannelHistoryPageAround(ctx, db.ChannelHistoryPageAroundParams{
			ChannelID: channelID,
			OffsetID:  offsetID,
			AddOffset: addOffset,
			MaxID:     maxID,
			MinID:     minID,
			Lim:       int32(limit), //nolint:gosec // limit is validated and capped by the API
		})
		if e != nil {
			return nil, 0, fmt.Errorf("channel history page: %w", e)
		}
		msgs = make([]ChannelMessage, len(rows))
		for i, row := range rows {
			msgs[i] = channelMessageFromFields(channelMsgFields(row))
		}
	} else {
		rows, e := qtx.ChannelHistoryPageWithOffset(ctx, db.ChannelHistoryPageWithOffsetParams{
			ChannelID: channelID,
			OffsetID:  offsetID,
			AddOffset: addOffset,
			MaxID:     maxID,
			MinID:     minID,
			Lim:       int32(limit), //nolint:gosec // limit is validated and capped by the API
		})
		if e != nil {
			return nil, 0, fmt.Errorf("channel history page: %w", e)
		}
		msgs = make([]ChannelMessage, len(rows))
		for i, row := range rows {
			msgs[i] = channelMessageFromFields(channelMsgFields(row))
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, 0, fmt.Errorf("commit channel history snapshot: %w", err)
	}
	return msgs, int(count), nil
}

// SearchChannelPosts returns the channel's posts matching query, newest-first
// and excluding deleted, paged by offsetID exactly as ChannelHistory pages.
//
// It takes no caller id on purpose. A channel post is one shared row rather
// than a per-owner copy, so there is nothing here to scope to the reader:
// whether this caller may read the channel at all is decided by the membership
// check the handler runs before calling, and nothing below narrows it further.
func (s *Store) SearchChannelPosts(ctx context.Context, channelID int64, query string, offsetID int64, limit int) ([]ChannelMessage, error) {
	rows, err := s.q.SearchChannelPostsPage(ctx, db.SearchChannelPostsPageParams{
		ChannelID: channelID,
		Query:     query,
		OffsetID:  offsetID,
		Lim:       int32(limit), //nolint:gosec // limit is a small validated page size
	})
	if err != nil {
		return nil, fmt.Errorf("search channel posts page: %w", err)
	}
	msgs := make([]ChannelMessage, len(rows))
	for i, r := range rows {
		msgs[i] = channelMessageFromFields(channelMsgFields(r))
	}
	return msgs, nil
}

// SearchFilteredChannelPosts returns one authorized channel member's matching
// media page and the exact count from the same database snapshot. Membership is
// checked here and included in both queries; limit zero requests only the count.
func (s *Store) SearchFilteredChannelPosts(
	ctx context.Context,
	ownerID, channelID int64,
	query string,
	filter MediaSearchFilter,
	offsetID int64,
	limit int,
) ([]ChannelMessage, int, error) {
	switch filter {
	case MediaSearchFilterDocument, MediaSearchFilterPhoto, MediaSearchFilterURL,
		MediaSearchFilterVideo, MediaSearchFilterGif, MediaSearchFilterPoll,
		MediaSearchFilterRoundVoice, MediaSearchFilterMusic, MediaSearchFilterPhotoVideo:
	default:
		return nil, 0, fmt.Errorf("unsupported channel media search filter %d", filter)
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("begin filtered channel search snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)
	if hook := s.filteredChannelSearchSnapshotHook; hook != nil {
		hook()
	}

	participant, err := qtx.ChannelParticipantByUser(ctx, db.ChannelParticipantByUserParams{
		ChannelID: channelID,
		UserID:    ownerID,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, 0, ErrNotMember
	case err != nil:
		return nil, 0, fmt.Errorf("check filtered channel search membership: %w", err)
	}
	if channelMemberFromRow(participant).Banned(time.Now()) {
		return nil, 0, ErrNotMember
	}

	// A channel poll has no per-viewer poll copy, so the queries below answer
	// that filter with nothing and the store does not read posts for it.
	if filter == MediaSearchFilterPoll {
		if err := tx.Commit(ctx); err != nil {
			return nil, 0, fmt.Errorf("commit empty channel media search: %w", err)
		}
		return []ChannelMessage{}, 0, nil
	}

	count, err := qtx.CountFilteredChannelPosts(ctx, db.CountFilteredChannelPostsParams{
		ChannelID: channelID,
		OwnerID:   ownerID,
		Filter:    int16(filter),
		Query:     query,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("count filtered channel posts: %w", err)
	}

	var rows []db.SearchFilteredChannelPostsPageRow
	if limit > 0 {
		rows, err = qtx.SearchFilteredChannelPostsPage(ctx, db.SearchFilteredChannelPostsPageParams{
			ChannelID: channelID,
			OwnerID:   ownerID,
			Filter:    int16(filter),
			Query:     query,
			OffsetID:  offsetID,
			Lim:       int32(limit), //nolint:gosec // caller caps this to maxHistoryLimit
		})
		if err != nil {
			return nil, 0, fmt.Errorf("page filtered channel posts: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, 0, fmt.Errorf("commit filtered channel search snapshot: %w", err)
	}
	msgs := make([]ChannelMessage, len(rows))
	for i, row := range rows {
		msgs[i] = channelMessageFromFields(channelMsgFields(row))
	}
	return msgs, int(count), nil
}

// SetFilteredChannelSearchSnapshotHook installs the test-only synchronization
// seam used to cover a membership change after handler admission and before the
// search snapshot reads membership. Production callers leave it nil.
func SetFilteredChannelSearchSnapshotHook(s *Store, fn func()) {
	s.filteredChannelSearchSnapshotHook = fn
}

// SearchPinnedChannelPost returns the active pinned post for channelID while
// ownerID still has an unbanned participant row. The channel membership join
// and post lookup happen in one statement so a departed or newly banned viewer
// cannot receive channel content through the pin filter.
func (s *Store) SearchPinnedChannelPost(ctx context.Context, ownerID, channelID int64, query string, offsetID int64, limit int) ([]ChannelMessage, error) {
	rows, err := s.q.SearchPinnedChannelPostForMember(ctx, db.SearchPinnedChannelPostForMemberParams{
		OwnerID:   ownerID,
		ChannelID: channelID,
		Query:     query,
		OffsetID:  offsetID,
		Lim:       int32(limit), //nolint:gosec // limit is a small validated page size
	})
	if err != nil {
		return nil, fmt.Errorf("search pinned channel post: %w", err)
	}
	msgs := make([]ChannelMessage, len(rows))
	for i, row := range rows {
		msgs[i] = channelMessageFromFields(channelMsgFields(row))
	}
	return msgs, nil
}
