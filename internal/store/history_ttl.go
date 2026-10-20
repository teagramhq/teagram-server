package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/teagramhq/teagram-server/internal/store/db"
)

var historyTTLPeriods = [...]int{0, 86400, 604800, 2592000}

// ValidHistoryTTLPeriod reports whether period is one of the server-supported
// history auto-delete intervals.
func ValidHistoryTTLPeriod(period int) bool {
	for _, allowed := range historyTTLPeriods {
		if period == allowed {
			return true
		}
	}
	return false
}

func historyTTLPeriodValue(period int) int32 {
	return int32(period) //nolint:gosec // G115: callers validate the four-value allow-list.
}

// UserHistoryTTL returns the current prospective TTL for a 1:1 dialog.
func (s *Store) UserHistoryTTL(ctx context.Context, userID, peerID int64) (int, error) {
	if userID <= 0 || peerID <= 0 {
		return 0, ErrHistoryTTLPeerInvalid
	}
	if userID == peerID {
		return 0, nil
	}
	period, err := s.q.UserHistoryTTL(ctx, db.UserHistoryTTLParams{UserID: userID, PeerID: peerID})
	if err != nil {
		return 0, fmt.Errorf("user history TTL: %w", err)
	}
	return int(period), nil
}

// SetUserHistoryTTL changes a direct dialog's future-message TTL and writes its
// service message to both owners in one transaction. Owner locks serialize the
// change with all 1:1 message send paths, and the database trigger stamps the
// following content inserts from the committed dialog setting.
func (s *Store) SetUserHistoryTTL(
	ctx context.Context, callerID, peerID int64, period int, budget RateLimitConfig,
) (sender Message, senderPts, peerPts int, changed bool, err error) {
	if callerID <= 0 || peerID <= 0 || callerID == peerID {
		return Message{}, 0, 0, false, ErrHistoryTTLPeerInvalid
	}
	if !ValidHistoryTTLPeriod(period) {
		return Message{}, 0, 0, false, ErrHistoryTTLPeriodInvalid
	}

	if _, ok, checkErr := s.UserByID(ctx, peerID); checkErr != nil {
		return Message{}, 0, 0, false, fmt.Errorf("validate history TTL peer: %w", checkErr)
	} else if !ok {
		return Message{}, 0, 0, false, ErrHistoryTTLPeerInvalid
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Message{}, 0, 0, false, fmt.Errorf("begin user history TTL: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	if err = lockOwners(ctx, tx, callerID, peerID); err != nil {
		return Message{}, 0, 0, false, err
	}
	qtx := s.q.WithTx(tx)
	callerBlockedPeer, err := qtx.IsBlocked(ctx, db.IsBlockedParams{BlockerID: callerID, BlockedID: peerID})
	if err != nil {
		return Message{}, 0, 0, false, fmt.Errorf("check caller block for history TTL: %w", err)
	}
	if callerBlockedPeer {
		return Message{}, 0, 0, false, ErrYouBlockedUser
	}
	peerBlockedCaller, err := qtx.IsBlocked(ctx, db.IsBlockedParams{BlockerID: peerID, BlockedID: callerID})
	if err != nil {
		return Message{}, 0, 0, false, fmt.Errorf("check peer block for history TTL: %w", err)
	}
	if peerBlockedCaller {
		return Message{}, 0, 0, false, ErrPeerBlockedUser
	}
	current, err := qtx.UserHistoryTTL(ctx, db.UserHistoryTTLParams{UserID: callerID, PeerID: peerID})
	if err != nil {
		return Message{}, 0, 0, false, fmt.Errorf("read current user history TTL: %w", err)
	}
	if int(current) == period {
		return Message{}, 0, 0, false, nil
	}
	if err = s.consumeRateLimitTx(ctx, qtx, callerID,
		fmt.Sprintf("history_ttl_user_%d", peerID), budget, 1); err != nil {
		return Message{}, 0, 0, false, err
	}
	if err = qtx.SetUserHistoryTTL(ctx, db.SetUserHistoryTTLParams{
		UserID: callerID, PeerID: peerID, TtlPeriod: historyTTLPeriodValue(period),
	}); err != nil {
		return Message{}, 0, 0, false, fmt.Errorf("set user history TTL: %w", err)
	}
	if err = qtx.EnsureUpdateState(ctx, callerID); err != nil {
		return Message{}, 0, 0, false, fmt.Errorf("ensure user history TTL caller state: %w", err)
	}
	if err = qtx.EnsureUpdateState(ctx, peerID); err != nil {
		return Message{}, 0, 0, false, fmt.Errorf("ensure user history TTL peer state: %w", err)
	}
	senderBump, err := bumpState(ctx, qtx, callerID)
	if err != nil {
		return Message{}, 0, 0, false, fmt.Errorf("bump caller history TTL state: %w", err)
	}
	peerBump, err := bumpState(ctx, qtx, peerID)
	if err != nil {
		return Message{}, 0, 0, false, fmt.Errorf("bump peer history TTL state: %w", err)
	}
	if err = qtx.InsertMessage(ctx, historyTTLMessageParams(callerID, callerID, peerID, senderBump.LocalID, peerBump.LocalID, true, period)); err != nil {
		return Message{}, 0, 0, false, fmt.Errorf("insert caller history TTL service message: %w", err)
	}
	if err = qtx.InsertMessage(ctx, historyTTLMessageParams(peerID, callerID, callerID, peerBump.LocalID, senderBump.LocalID, false, period)); err != nil {
		return Message{}, 0, 0, false, fmt.Errorf("insert peer history TTL service message: %w", err)
	}
	for _, event := range []db.InsertEventParams{
		{OwnerID: callerID, Pts: senderBump.Pts, Type: int16(EventNewMessage), LocalID: senderBump.LocalID},
		{OwnerID: peerID, Pts: peerBump.Pts, Type: int16(EventNewMessage), LocalID: peerBump.LocalID},
	} {
		if err = qtx.InsertEvent(ctx, event); err != nil {
			return Message{}, 0, 0, false, fmt.Errorf("insert history TTL event: %w", err)
		}
	}
	if err = qtx.UpsertDialog(ctx, db.UpsertDialogParams{
		OwnerID: callerID, PeerType: int16(PeerTypeUser), PeerID: peerID,
		TopMessage: senderBump.LocalID, UnreadCount: 0,
	}); err != nil {
		return Message{}, 0, 0, false, fmt.Errorf("upsert caller history TTL dialog: %w", err)
	}
	if err = qtx.UpsertDialog(ctx, db.UpsertDialogParams{
		OwnerID: peerID, PeerType: int16(PeerTypeUser), PeerID: callerID,
		TopMessage: peerBump.LocalID, UnreadCount: 1,
	}); err != nil {
		return Message{}, 0, 0, false, fmt.Errorf("upsert peer history TTL dialog: %w", err)
	}
	row, err := qtx.MessageByOwnerLocal(ctx, db.MessageByOwnerLocalParams{OwnerID: callerID, LocalID: senderBump.LocalID})
	if err != nil {
		return Message{}, 0, 0, false, fmt.Errorf("reload caller history TTL service message: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return Message{}, 0, 0, false, fmt.Errorf("commit user history TTL: %w", err)
	}
	return messageFromRow(row), int(senderBump.Pts), int(peerBump.Pts), true, nil
}

func historyTTLMessageParams(ownerID, fromID, peerID, localID, peerLocalID int64, out bool, period int) db.InsertMessageParams {
	return db.InsertMessageParams{
		OwnerID: ownerID, LocalID: localID, PeerType: int16(PeerTypeUser), PeerID: peerID,
		FromID: fromID, Message: "", Out: out, RandomID: 0, PeerLocalID: peerLocalID,
		FanoutID: 0, ActionType: int16(ChatActionSetMessagesTTL), ActionUserID: 0, FileID: 0,
		ReplyToMsgID: nil, FwdFromID: nil, FwdDate: pgtype.Timestamptz{},
		FwdChannelID: nil, FwdChannelPost: nil, ReplyToTrusted: false,
		TtlPeriod: historyTTLPeriodValue(period),
	}
}

// SetChatHistoryTTL changes a basic group's future-message TTL with the same
// change-info authority and chat/member locks as a title edit.
func (s *Store) SetChatHistoryTTL(
	ctx context.Context, chatID, callerID int64, period int, budget RateLimitConfig,
) (chat Chat, sender Message, perOwner map[int64]int, changed bool, err error) {
	if !ValidHistoryTTLPeriod(period) {
		return Chat{}, Message{}, nil, false, ErrHistoryTTLPeriodInvalid
	}
	m, err := s.beginChatMutation(ctx, chatID, callerID)
	if err != nil {
		return Chat{}, Message{}, nil, false, err
	}
	defer func() { _ = m.tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	if !m.canChangeInfo(callerID) {
		return Chat{}, Message{}, nil, false, ErrChatAdminRequired
	}
	if int(m.chat.TtlPeriod) == period {
		return chatFromRow(m.chat), Message{}, nil, false, nil
	}
	if err = m.lockOwners(ctx); err != nil {
		return Chat{}, Message{}, nil, false, err
	}
	if err = s.consumeRateLimitTx(ctx, m.qtx, callerID,
		fmt.Sprintf("history_ttl_chat_%d", chatID), budget, 1); err != nil {
		return Chat{}, Message{}, nil, false, err
	}
	row, err := m.qtx.SetChatHistoryTTL(ctx, db.SetChatHistoryTTLParams{ChatID: chatID, TtlPeriod: historyTTLPeriodValue(period)})
	if err != nil {
		return Chat{}, Message{}, nil, false, fmt.Errorf("set chat history TTL: %w", err)
	}
	sender, perOwner, _, err = fanOut(ctx, m.tx, m.qtx, s.log, FanOut{
		ChatID: chatID, FromID: callerID, Action: ChatActionSetMessagesTTL, TTLPeriod: period,
	})
	if err != nil {
		return Chat{}, Message{}, nil, false, err
	}
	if err = m.tx.Commit(ctx); err != nil {
		return Chat{}, Message{}, nil, false, fmt.Errorf("commit chat history TTL: %w", err)
	}
	return chatFromRow(row), sender, perOwner, true, nil
}

// SetChannelHistoryTTL changes a channel's future-post TTL under the same
// change_info authority and channel locks used by title edits.
func (s *Store) SetChannelHistoryTTL(
	ctx context.Context, channelID, callerID int64, period int, budget RateLimitConfig,
) (channel Channel, message ChannelMessage, pts int, changed bool, err error) {
	if !ValidHistoryTTLPeriod(period) {
		return Channel{}, ChannelMessage{}, 0, false, ErrHistoryTTLPeriodInvalid
	}
	tx, qtx, locked, err := s.beginChannelInfoMutation(ctx, channelID, callerID)
	if err != nil {
		return Channel{}, ChannelMessage{}, 0, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	if int(locked.TtlPeriod) == period {
		return channelFromRow(locked), ChannelMessage{}, 0, false, nil
	}
	if err = s.consumeRateLimitTx(ctx, qtx, callerID,
		fmt.Sprintf("history_ttl_channel_%d", channelID), budget, 1); err != nil {
		return Channel{}, ChannelMessage{}, 0, false, err
	}
	if _, err = qtx.LockChannelState(ctx, channelID); err != nil {
		return Channel{}, ChannelMessage{}, 0, false, fmt.Errorf("lock channel state for history TTL: %w", err)
	}
	b, err := bumpChannelState(ctx, qtx, channelID)
	if err != nil {
		return Channel{}, ChannelMessage{}, 0, false, fmt.Errorf("bump channel state for history TTL: %w", err)
	}
	row, err := qtx.SetChannelHistoryTTL(ctx, db.SetChannelHistoryTTLParams{ChannelID: channelID, TtlPeriod: historyTTLPeriodValue(period)})
	if err != nil {
		return Channel{}, ChannelMessage{}, 0, false, fmt.Errorf("set channel history TTL: %w", err)
	}
	if err = qtx.InsertChannelHistoryTTLChangeMessage(ctx, db.InsertChannelHistoryTTLChangeMessageParams{
		ChannelID: channelID, LocalID: b.LocalID, FromID: callerID, TtlPeriod: historyTTLPeriodValue(period),
	}); err != nil {
		return Channel{}, ChannelMessage{}, 0, false, fmt.Errorf("insert channel history TTL service message: %w", err)
	}
	if err = qtx.InsertChannelEvent(ctx, db.InsertChannelEventParams{
		ChannelID: channelID, Pts: b.Pts, Type: int16(EventNewMessage), LocalID: b.LocalID,
	}); err != nil {
		return Channel{}, ChannelMessage{}, 0, false, fmt.Errorf("insert channel history TTL event: %w", err)
	}
	messageRow, err := qtx.ChannelMessageByLocal(ctx, db.ChannelMessageByLocalParams{ChannelID: channelID, LocalID: b.LocalID})
	if err != nil {
		return Channel{}, ChannelMessage{}, 0, false, fmt.Errorf("reload channel history TTL service message: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return Channel{}, ChannelMessage{}, 0, false, fmt.Errorf("commit channel history TTL: %w", err)
	}
	return channelFromRow(row), channelMessageFromFields(channelMsgFields(messageRow)), int(b.Pts), true, nil
}
