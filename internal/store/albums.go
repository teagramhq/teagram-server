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

// MaxAlbumItems is the largest number of items one album send may carry.
const MaxAlbumItems = 10

// AlbumItem is one album entry as the client named it: a reference to a file
// the caller stored with uploadMedia, the constructor kind it was presented
// as, and the item's own dedup token and caption.
type AlbumItem struct {
	FileID     int64
	AccessHash int64
	Photo      bool
	RandomID   int64
	Text       string
}

// AlbumSend is one album send. GroupedID is drawn by the caller, never taken
// from the client. GrantTTL is the uploadMedia grant lifetime.
type AlbumSend struct {
	FromID    int64
	Items     []AlbumItem
	GroupedID int64
	GrantTTL  time.Duration
}

// AlbumResult is the sender's view of an album send or of its replay.
// Messages and Pts are in item order. PerOwner carries each owner's final pts
// for a new send and is nil for a replay.
type AlbumResult struct {
	Messages  []Message
	Pts       []int
	PerOwner  map[int64]int
	Duplicate bool
}

// ChannelAlbumResult is AlbumResult for a channel post.
type ChannelAlbumResult struct {
	Messages  []ChannelMessage
	Pts       []int
	Duplicate bool
}

// UserAlbumRetry is the read-only dedup an album send runs before it charges
// the caller. It answers a replay of a whole album the caller already sent to
// this peer, ErrMediaInvalid for any token that names a deleted row or only
// part of an album, and a zero result when no token is known. The send
// transaction repeats the same decision under its locks.
func (s *Store) UserAlbumRetry(ctx context.Context, ownerID int64, peerType PeerType, peerID int64, randomIDs []int64) (AlbumResult, error) {
	rows, dup, err := userAlbumDedup(ctx, s.q, ownerID, peerType, peerID, randomIDs)
	if err != nil || !dup {
		return AlbumResult{}, err
	}
	return userAlbumReplay(ctx, s.q, ownerID, rows)
}

// userAlbumDedup classifies an album's dedup tokens against the owner's rows.
// Every token must be absent (a new send) or every token must name a live row
// of one album to this peer that has exactly these rows (a replay). A deleted
// row answers ErrMediaInvalid like sendMedia's deleted-original refusal, so a
// resend cannot say which items a recipient deleted, and any other mix is
// refused whole rather than sending the remainder.
func userAlbumDedup(ctx context.Context, q *db.Queries, ownerID int64, peerType PeerType, peerID int64, randomIDs []int64) ([]db.Message, bool, error) {
	rows, err := q.MessagesByRandomIDs(ctx, db.MessagesByRandomIDsParams{OwnerID: ownerID, RandomIds: randomIDs})
	if err != nil {
		return nil, false, fmt.Errorf("album random_id lookup: %w", err)
	}
	if len(rows) == 0 {
		return nil, false, nil
	}
	if len(rows) != len(randomIDs) {
		return nil, false, ErrMediaInvalid
	}
	first := rows[0]
	if first.GroupedID == nil {
		return nil, false, ErrMediaInvalid
	}
	for _, row := range rows {
		if row.Deleted || !row.Out || row.GroupedID == nil || *row.GroupedID != *first.GroupedID ||
			PeerType(row.PeerType) != peerType || row.PeerID != peerID {
			return nil, false, ErrMediaInvalid
		}
	}
	n, err := q.CountGroupedMessagesInRange(ctx, db.CountGroupedMessagesInRangeParams{
		OwnerID:    ownerID,
		MinLocalID: rows[0].LocalID - MaxAlbumItems,
		MaxLocalID: rows[len(rows)-1].LocalID + MaxAlbumItems,
		GroupedID:  *first.GroupedID,
	})
	if err != nil {
		return nil, false, fmt.Errorf("album size: %w", err)
	}
	if n != int64(len(rows)) {
		return nil, false, ErrMediaInvalid
	}
	return rows, true, nil
}

func userAlbumReplay(ctx context.Context, q *db.Queries, ownerID int64, rows []db.Message) (AlbumResult, error) {
	result := AlbumResult{Duplicate: true, Messages: make([]Message, len(rows)), Pts: make([]int, len(rows))}
	for i, row := range rows {
		pts, err := newMessagePts(ctx, q, ownerID, row.LocalID)
		if err != nil {
			return AlbumResult{}, err
		}
		result.Messages[i] = messageFromRow(row)
		result.Pts[i] = pts
	}
	return result, nil
}

// consumeAlbumGrants resolves every album reference through an upload grant
// and consumes it. Only a grant the caller owns, inside its lifetime, on a
// stored file whose access hash matches and whose kind matches the constructor
// the client presented resolves; anything else is ErrMediaInvalid, and the
// caller's rollback restores every grant this consumed.
//
// Ownership is filtered before the files-row interlock, so a reference to
// another account's file never takes a lock on that account's row. The
// interlock is then taken on the caller's own files in the shared ascending
// order, before the grant rows: the eraser takes the file row first and the
// grant row only through its cascade, so the two cannot wait on each other.
// The grant rows are then locked in ascending file_id order before the delete,
// so two albums naming the same grants in any item order queue on the lowest.
// Returns each item's stored subtype rights in item order.
func consumeAlbumGrants(ctx context.Context, qtx *db.Queries, ownerID int64, items []AlbumItem, ttl time.Duration) ([][]string, error) {
	if ttl <= 0 {
		return nil, errors.New("upload grant TTL must be positive")
	}
	fileIDs := make([]int64, len(items))
	hashes := make([]int64, len(items))
	for i, item := range items {
		fileIDs[i], hashes[i] = item.FileID, item.AccessHash
	}
	owned, err := qtx.OwnedUploadGrantFileIDs(ctx, db.OwnedUploadGrantFileIDsParams{OwnerID: ownerID, FileIds: fileIDs})
	if err != nil {
		return nil, fmt.Errorf("album grants: %w", err)
	}
	if len(owned) != len(items) {
		return nil, ErrMediaInvalid
	}
	if err = lockFileRefs(ctx, qtx, fileIDs...); err != nil {
		if errors.Is(err, ErrFileMissing) {
			return nil, ErrMediaInvalid
		}
		return nil, err
	}
	locked, err := qtx.LockUploadGrants(ctx, db.LockUploadGrantsParams{OwnerID: ownerID, FileIds: fileIDs})
	if err != nil {
		return nil, fmt.Errorf("lock album grants: %w", err)
	}
	if len(locked) != len(items) {
		return nil, ErrMediaInvalid
	}
	consumed, err := qtx.ConsumeUploadGrants(ctx, db.ConsumeUploadGrantsParams{
		FileIds:      fileIDs,
		AccessHashes: hashes,
		OwnerID:      ownerID,
		NotBefore:    pgtype.Timestamptz{Time: time.Now().Add(-ttl), Valid: true},
	})
	if err != nil {
		return nil, fmt.Errorf("consume album grants: %w", err)
	}
	if len(consumed) != len(items) {
		return nil, ErrMediaInvalid
	}
	byID := make(map[int64]db.ConsumeUploadGrantsRow, len(consumed))
	for _, row := range consumed {
		byID[row.ID] = row
	}
	rights := make([][]string, len(items))
	for i, item := range items {
		row, ok := byID[item.FileID]
		if !ok || (row.MediaKind == string(FileKindPhoto)) != item.Photo {
			return nil, ErrMediaInvalid
		}
		rights[i] = row.SubtypeRights
		if item.Photo {
			rights[i] = []string{"send_photos"}
		}
	}
	return rights, nil
}

// bumpStateBy allocates n consecutive local ids and pts values for one owner
// and returns the first of each.
func bumpStateBy(ctx context.Context, q *db.Queries, ownerID int64, n int) (firstLocalID, firstPts int64, err error) {
	row, err := q.BumpStateBy(ctx, db.BumpStateByParams{N: int64(n), UserID: ownerID})
	if err == nil {
		return row.LocalID - int64(n) + 1, row.Pts - int64(n) + 1, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, err
	}
	state, stateErr := q.GetState(ctx, ownerID)
	if errors.Is(stateErr, pgx.ErrNoRows) {
		return 0, 0, err
	}
	if stateErr != nil {
		return 0, 0, fmt.Errorf("read owner state after refused bump: %w", stateErr)
	}
	if state.Pts > math.MaxInt32-int64(n) || state.NextLocalID > math.MaxInt32-int64(n)+1 {
		return 0, 0, fmt.Errorf("%w: owner %d", ErrOwnerStateExhausted, ownerID)
	}
	return 0, 0, err
}

// albumRows accumulates the parallel arrays of one batched album write.
type albumRows struct {
	owners, localIDs, peerIDs, randomIDs, peerLocalIDs, fanoutIDs, fileIDs, pts []int64
	texts                                                                       []string
	outs                                                                        []bool
}

func (r *albumRows) add(owner, localID, peerID, randomID, peerLocalID, fanoutID, fileID, pts int64, text string, out bool) {
	r.owners = append(r.owners, owner)
	r.localIDs = append(r.localIDs, localID)
	r.peerIDs = append(r.peerIDs, peerID)
	r.randomIDs = append(r.randomIDs, randomID)
	r.peerLocalIDs = append(r.peerLocalIDs, peerLocalID)
	r.fanoutIDs = append(r.fanoutIDs, fanoutID)
	r.fileIDs = append(r.fileIDs, fileID)
	r.pts = append(r.pts, pts)
	r.texts = append(r.texts, text)
	r.outs = append(r.outs, out)
}

func (r *albumRows) write(ctx context.Context, qtx *db.Queries, peerType PeerType, fromID, groupedID int64) error {
	if err := qtx.InsertAlbumMessages(ctx, db.InsertAlbumMessagesParams{
		OwnerIds: r.owners, LocalIds: r.localIDs, PeerType: int16(peerType), PeerIds: r.peerIDs, FromID: fromID,
		Texts: r.texts, Outs: r.outs, RandomIds: r.randomIDs, PeerLocalIds: r.peerLocalIDs,
		FanoutIds: r.fanoutIDs, FileIds: r.fileIDs, GroupedID: groupedID,
	}); err != nil {
		return fmt.Errorf("insert album messages: %w", err)
	}
	if err := qtx.InsertNewMessageEvents(ctx, db.InsertNewMessageEventsParams{
		OwnerIds: r.owners, Pts: r.pts, Type: int16(EventNewMessage), LocalIds: r.localIDs,
	}); err != nil {
		return fmt.Errorf("insert album events: %w", err)
	}
	return nil
}

func albumRandomIDs(items []AlbumItem) []int64 {
	ids := make([]int64, len(items))
	for i, item := range items {
		ids[i] = item.RandomID
	}
	return ids
}

func validAlbum(a AlbumSend) error {
	if a.FromID == 0 || a.GroupedID == 0 || len(a.Items) < 2 || len(a.Items) > MaxAlbumItems {
		return ErrMediaInvalid
	}
	return nil
}

// SendUserAlbum writes a 1:1 album: every grant consumed and every copy of
// every item written in one transaction, or nothing. Lock order is the one
// SendMessage takes: both owners' advisory locks, then the files rows.
func (s *Store) SendUserAlbum(ctx context.Context, toID int64, a AlbumSend) (AlbumResult, error) {
	if err := validAlbum(a); err != nil {
		return AlbumResult{}, err
	}
	fromID, n := a.FromID, len(a.Items)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AlbumResult{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)

	if err = lockOwners(ctx, tx, fromID, toID); err != nil {
		return AlbumResult{}, err
	}
	randomIDs := albumRandomIDs(a.Items)
	if existing, dup, dedupErr := userAlbumDedup(ctx, qtx, fromID, PeerTypeUser, toID, randomIDs); dedupErr != nil {
		return AlbumResult{}, dedupErr
	} else if dup {
		return userAlbumReplay(ctx, qtx, fromID, existing)
	}
	if _, err = consumeAlbumGrants(ctx, qtx, fromID, a.Items, a.GrantTTL); err != nil {
		return AlbumResult{}, err
	}

	owners := []int64{fromID}
	if toID != fromID {
		owners = append(owners, toID)
	}
	firstLocal := make(map[int64]int64, len(owners))
	firstPts := make(map[int64]int64, len(owners))
	for _, owner := range owners {
		if err = qtx.EnsureUpdateState(ctx, owner); err != nil {
			return AlbumResult{}, fmt.Errorf("ensure state %d: %w", owner, err)
		}
		if firstLocal[owner], firstPts[owner], err = bumpStateBy(ctx, qtx, owner, n); err != nil {
			return AlbumResult{}, fmt.Errorf("bump %d: %w", owner, err)
		}
	}

	var rows albumRows
	for i, item := range a.Items {
		offset := int64(i)
		senderLocal := firstLocal[fromID] + offset
		if toID == fromID {
			rows.add(fromID, senderLocal, fromID, item.RandomID, 0, 0, item.FileID, firstPts[fromID]+offset, item.Text, true)
			continue
		}
		recipientLocal := firstLocal[toID] + offset
		rows.add(fromID, senderLocal, toID, item.RandomID, recipientLocal, 0, item.FileID, firstPts[fromID]+offset, item.Text, true)
		rows.add(toID, recipientLocal, fromID, 0, senderLocal, 0, item.FileID, firstPts[toID]+offset, item.Text, false)
	}
	if err = rows.write(ctx, qtx, PeerTypeUser, fromID, a.GroupedID); err != nil {
		return AlbumResult{}, err
	}
	dialogOwners, dialogPeers, tops, unread := []int64{fromID}, []int64{toID}, []int64{firstLocal[fromID] + int64(n) - 1}, []int32{0}
	if toID != fromID {
		dialogOwners = append(dialogOwners, toID)
		dialogPeers = append(dialogPeers, fromID)
		tops = append(tops, firstLocal[toID]+int64(n)-1)
		unread = append(unread, int32(n)) //nolint:gosec // G115: n is at most MaxAlbumItems
	}
	if err = qtx.UpsertDialogs(ctx, db.UpsertDialogsParams{
		OwnerIds: dialogOwners, PeerType: int16(PeerTypeUser), PeerIds: dialogPeers, TopMessages: tops, UnreadCounts: unread,
	}); err != nil {
		return AlbumResult{}, fmt.Errorf("album dialogs: %w", err)
	}

	result, err := senderAlbumResult(ctx, qtx, fromID, randomIDs, firstPts[fromID])
	if err != nil {
		return AlbumResult{}, err
	}
	result.PerOwner = make(map[int64]int, len(owners))
	for _, owner := range owners {
		result.PerOwner[owner] = int(firstPts[owner]) + n - 1
	}
	if err = tx.Commit(ctx); err != nil {
		return AlbumResult{}, fmt.Errorf("commit: %w", err)
	}
	return result, nil
}

// SendChatAlbum writes a basic-group album to every member. Lock order and
// membership rules are fanOut's: the chats row, the member set read under it,
// the members' advisory locks ascending, then the files rows. Each item is its
// own fan-out with its own fanout_id; every copy of every item shares the
// album's grouped_id. Default restrictions are judged item by item on each
// file's stored rights, so the album is refused when any item would be.
func (s *Store) SendChatAlbum(ctx context.Context, chatID int64, a AlbumSend) (AlbumResult, error) {
	if err := validAlbum(a); err != nil {
		return AlbumResult{}, err
	}
	if chatID == 0 {
		return AlbumResult{}, ErrNotMember
	}
	fromID, n := a.FromID, len(a.Items)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AlbumResult{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)

	member, err := qtx.IsChatMember(ctx, db.IsChatMemberParams{ChatID: chatID, UserID: fromID})
	if err != nil {
		return AlbumResult{}, fmt.Errorf("is chat member: %w", err)
	}
	if !member {
		return AlbumResult{}, ErrNotMember
	}
	chat, err := qtx.ChatByIDForUpdate(ctx, chatID)
	if errors.Is(err, pgx.ErrNoRows) {
		return AlbumResult{}, ErrNotMember
	} else if err != nil {
		return AlbumResult{}, fmt.Errorf("lock chat: %w", err)
	}
	parts, err := qtx.ChatParticipants(ctx, chatID)
	if err != nil {
		return AlbumResult{}, fmt.Errorf("chat participants: %w", err)
	}
	owners := make([]int64, 0, len(parts))
	isMember := false
	for _, p := range parts {
		owners = append(owners, p.UserID)
		isMember = isMember || p.UserID == fromID
	}
	if !isMember {
		return AlbumResult{}, ErrNotMember
	}
	if len(owners) > maxChatParticipants {
		return AlbumResult{}, ErrChatFull
	}
	if err = lockOwners(ctx, tx, owners...); err != nil {
		return AlbumResult{}, err
	}

	randomIDs := albumRandomIDs(a.Items)
	if existing, dup, dedupErr := userAlbumDedup(ctx, qtx, fromID, PeerTypeChat, chatID, randomIDs); dedupErr != nil {
		return AlbumResult{}, dedupErr
	} else if dup {
		return userAlbumReplay(ctx, qtx, fromID, existing)
	}
	rights, err := consumeAlbumGrants(ctx, qtx, fromID, a.Items, a.GrantTTL)
	if err != nil {
		return AlbumResult{}, err
	}
	for _, itemRights := range rights {
		if err = checkDefaultMessageRestriction(chat.DefaultBannedRights, fromID == chat.CreatorID, true, itemRights); err != nil {
			return AlbumResult{}, err
		}
	}

	fanoutIDs := make([]int64, n)
	for i := range fanoutIDs {
		if fanoutIDs[i], err = qtx.NextFanoutID(ctx); err != nil {
			return AlbumResult{}, fmt.Errorf("next fanout id: %w", err)
		}
	}
	firstLocal := make(map[int64]int64, len(owners))
	firstPts := make(map[int64]int64, len(owners))
	for _, owner := range owners {
		if err = qtx.EnsureUpdateState(ctx, owner); err != nil {
			return AlbumResult{}, fmt.Errorf("ensure state %d: %w", owner, err)
		}
		if firstLocal[owner], firstPts[owner], err = bumpStateBy(ctx, qtx, owner, n); err != nil {
			return AlbumResult{}, fmt.Errorf("bump %d: %w", owner, err)
		}
	}

	var rows albumRows
	dialogPeers := make([]int64, len(owners))
	tops := make([]int64, len(owners))
	unread := make([]int32, len(owners))
	for j, owner := range owners {
		out := owner == fromID
		for i, item := range a.Items {
			randomID := int64(0)
			if out {
				randomID = item.RandomID
			}
			offset := int64(i)
			rows.add(owner, firstLocal[owner]+offset, chatID, randomID, 0, fanoutIDs[i], item.FileID, firstPts[owner]+offset, item.Text, out)
		}
		dialogPeers[j] = chatID
		tops[j] = firstLocal[owner] + int64(n) - 1
		if !out {
			unread[j] = int32(n) //nolint:gosec // G115: n is at most MaxAlbumItems
		}
	}
	if err = rows.write(ctx, qtx, PeerTypeChat, fromID, a.GroupedID); err != nil {
		return AlbumResult{}, err
	}
	if err = qtx.UpsertDialogs(ctx, db.UpsertDialogsParams{
		OwnerIds: owners, PeerType: int16(PeerTypeChat), PeerIds: dialogPeers, TopMessages: tops, UnreadCounts: unread,
	}); err != nil {
		return AlbumResult{}, fmt.Errorf("album dialogs: %w", err)
	}

	result, err := senderAlbumResult(ctx, qtx, fromID, randomIDs, firstPts[fromID])
	if err != nil {
		return AlbumResult{}, err
	}
	result.PerOwner = make(map[int64]int, len(owners))
	for _, owner := range owners {
		result.PerOwner[owner] = int(firstPts[owner]) + n - 1
	}
	if err = tx.Commit(ctx); err != nil {
		return AlbumResult{}, fmt.Errorf("commit: %w", err)
	}
	return result, nil
}

func senderAlbumResult(ctx context.Context, qtx *db.Queries, fromID int64, randomIDs []int64, firstPts int64) (AlbumResult, error) {
	stored, err := qtx.MessagesByRandomIDs(ctx, db.MessagesByRandomIDsParams{OwnerID: fromID, RandomIds: randomIDs})
	if err != nil {
		return AlbumResult{}, fmt.Errorf("reload album: %w", err)
	}
	if len(stored) != len(randomIDs) {
		return AlbumResult{}, fmt.Errorf("reload album: %d rows for %d items", len(stored), len(randomIDs))
	}
	result := AlbumResult{Messages: make([]Message, len(stored)), Pts: make([]int, len(stored))}
	for i, row := range stored {
		result.Messages[i] = messageFromRow(row)
		result.Pts[i] = int(firstPts) + i
	}
	return result, nil
}

// ChannelAlbumRetryAs is ChannelPhotoRetryAs for an album: it checks the
// caller's current posting rights under the channel state lock before it reads
// any dedup token, then answers a replay, a refusal, or a zero result for a new
// send. It never creates a post.
func (s *Store) ChannelAlbumRetryAs(ctx context.Context, channelID, fromID int64, randomIDs []int64) (ChannelAlbumResult, error) {
	if channelID == 0 || fromID == 0 {
		return ChannelAlbumResult{}, ErrMessageInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ChannelAlbumResult{}, fmt.Errorf("begin channel album retry: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)

	if _, err = checkPostRights(ctx, qtx, channelID, fromID); err != nil {
		return ChannelAlbumResult{}, err
	}
	if err = qtx.EnsureChannelState(ctx, channelID); err != nil {
		return ChannelAlbumResult{}, fmt.Errorf("ensure channel state for album retry: %w", err)
	}
	if _, err = qtx.LockChannelState(ctx, channelID); err != nil {
		return ChannelAlbumResult{}, fmt.Errorf("lock channel state for album retry: %w", err)
	}
	if _, err = checkChannelMediaPostRights(ctx, qtx, channelID, fromID); err != nil {
		return ChannelAlbumResult{}, err
	}
	result, err := channelAlbumDedup(ctx, qtx, channelID, fromID, randomIDs)
	if err != nil {
		return ChannelAlbumResult{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return ChannelAlbumResult{}, fmt.Errorf("commit channel album retry: %w", err)
	}
	return result, nil
}

// channelAlbumDedup is userAlbumDedup in a channel's own (channel_id,
// random_id) space. A replayable post is the caller's own live photo post of
// one album that has exactly these posts.
func channelAlbumDedup(ctx context.Context, qtx *db.Queries, channelID, fromID int64, randomIDs []int64) (ChannelAlbumResult, error) {
	rows, err := qtx.ChannelMessagesByRandomIDs(ctx, db.ChannelMessagesByRandomIDsParams{ChannelID: channelID, RandomIds: randomIDs})
	if err != nil {
		return ChannelAlbumResult{}, fmt.Errorf("channel album random_id lookup: %w", err)
	}
	if len(rows) == 0 {
		return ChannelAlbumResult{}, nil
	}
	if len(rows) != len(randomIDs) || rows[0].GroupedID == nil {
		return ChannelAlbumResult{}, ErrMediaInvalid
	}
	groupedID := *rows[0].GroupedID
	for _, row := range rows {
		if row.Deleted || row.FromID != fromID || row.ActionType != int16(ChannelMessageActionNone) ||
			row.FileID == nil || row.GroupedID == nil || *row.GroupedID != groupedID {
			return ChannelAlbumResult{}, ErrMediaInvalid
		}
	}
	n, err := qtx.CountGroupedChannelMessagesInRange(ctx, db.CountGroupedChannelMessagesInRangeParams{
		ChannelID:  channelID,
		MinLocalID: rows[0].LocalID - MaxAlbumItems,
		MaxLocalID: rows[len(rows)-1].LocalID + MaxAlbumItems,
		GroupedID:  groupedID,
	})
	if err != nil {
		return ChannelAlbumResult{}, fmt.Errorf("channel album size: %w", err)
	}
	if n != int64(len(rows)) {
		return ChannelAlbumResult{}, ErrMediaInvalid
	}
	result := ChannelAlbumResult{Duplicate: true, Messages: make([]ChannelMessage, len(rows)), Pts: make([]int, len(rows))}
	for i, row := range rows {
		pts, ptsErr := newChannelPostPts(ctx, qtx, channelID, row.LocalID)
		if ptsErr != nil {
			return ChannelAlbumResult{}, ptsErr
		}
		result.Messages[i] = channelMessageFromFields(channelMsgFields(row))
		result.Pts[i] = pts
	}
	return result, nil
}

// PostChannelAlbumAs admits a photo album as consecutive channel posts. Every
// rule PostChannelPhotoAs enforces under the channel_state row lock applies
// here once for the album: posting rights, default restrictions judged on each
// file's stored rights, slow mode (the album counts as one post) and reply
// validation. Grants are consumed after the files-row interlock, which is
// taken after channel_state and the participant row as in the single-photo
// path.
func (s *Store) PostChannelAlbumAs(ctx context.Context, channelID, replyToMsgID int64, a AlbumSend) (ChannelAlbumResult, error) {
	if err := validAlbum(a); err != nil {
		return ChannelAlbumResult{}, err
	}
	if channelID == 0 {
		return ChannelAlbumResult{}, ErrMessageInvalid
	}
	for _, item := range a.Items {
		if !item.Photo {
			return ChannelAlbumResult{}, ErrMediaInvalid
		}
	}
	fromID, n := a.FromID, len(a.Items)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ChannelAlbumResult{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)

	if _, err = checkPostRights(ctx, qtx, channelID, fromID); err != nil {
		return ChannelAlbumResult{}, err
	}
	if err = qtx.EnsureChannelState(ctx, channelID); err != nil {
		return ChannelAlbumResult{}, fmt.Errorf("ensure channel state: %w", err)
	}
	channelState, err := qtx.LockChannelState(ctx, channelID)
	if err != nil {
		return ChannelAlbumResult{}, fmt.Errorf("lock channel state: %w", err)
	}
	role, err := checkChannelMediaPostRights(ctx, qtx, channelID, fromID)
	if err != nil {
		return ChannelAlbumResult{}, err
	}
	randomIDs := albumRandomIDs(a.Items)
	if replay, dedupErr := channelAlbumDedup(ctx, qtx, channelID, fromID, randomIDs); dedupErr != nil || replay.Duplicate {
		return replay, dedupErr
	}

	rights, err := consumeAlbumGrants(ctx, qtx, fromID, a.Items, a.GrantTTL)
	if err != nil {
		return ChannelAlbumResult{}, err
	}
	channel, err := qtx.ChannelPostDefaults(ctx, channelID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ChannelAlbumResult{}, ErrNotMember
	case err != nil:
		return ChannelAlbumResult{}, fmt.Errorf("channel post defaults: %w", err)
	}
	if channel.Megagroup {
		for _, itemRights := range rights {
			if err = checkDefaultMessageRestriction(channel.DefaultBannedRights, role >= channelRoleAdmin, true, itemRights); err != nil {
				return ChannelAlbumResult{}, err
			}
		}
		if role == channelRoleMember {
			state, stateErr := qtx.ChannelSlowModePostState(ctx, db.ChannelSlowModePostStateParams{ChannelID: channelID, UserID: fromID})
			switch {
			case errors.Is(stateErr, pgx.ErrNoRows):
				return ChannelAlbumResult{}, ErrNotMember
			case stateErr != nil:
				return ChannelAlbumResult{}, fmt.Errorf("channel slow-mode post state: %w", stateErr)
			}
			if state.SlowmodeSeconds > 0 && state.LastPostAt.Valid {
				remaining := time.Duration(state.SlowmodeSeconds)*time.Second - state.CheckedAt.Time.Sub(state.LastPostAt.Time)
				if remaining > 0 {
					return ChannelAlbumResult{}, &SlowModeWaitError{Seconds: int((remaining + time.Second - 1) / time.Second)}
				}
			}
		}
	}

	var replyTo *int32
	if replyToMsgID > 0 {
		_, verr := qtx.ChannelPostExistsActive(ctx, db.ChannelPostExistsActiveParams{ChannelID: channelID, LocalID: replyToMsgID})
		if errors.Is(verr, pgx.ErrNoRows) {
			return ChannelAlbumResult{}, ErrMessageInvalid
		}
		if verr != nil {
			return ChannelAlbumResult{}, fmt.Errorf("reply_to_msg_id check: %w", verr)
		}
		v := int32(replyToMsgID) //nolint:gosec // G115: local_id fits int32 wire space
		replyTo = &v
	}

	if err = requireChannelStateCapacity(channelID, channelState.Pts, channelState.NextLocalID, int64(n), int64(n)); err != nil {
		return ChannelAlbumResult{}, err
	}
	bumped, err := qtx.BumpChannelStateBy(ctx, db.BumpChannelStateByParams{N: int64(n), ChannelID: channelID})
	if err != nil {
		return ChannelAlbumResult{}, fmt.Errorf("bump channel state: %w", err)
	}
	firstLocal, firstPts := bumped.LocalID-int64(n)+1, bumped.Pts-int64(n)+1
	localIDs := make([]int64, n)
	pts := make([]int64, n)
	texts := make([]string, n)
	fileIDs := make([]int64, n)
	for i, item := range a.Items {
		localIDs[i], pts[i] = firstLocal+int64(i), firstPts+int64(i)
		texts[i], fileIDs[i] = item.Text, item.FileID
	}
	if err = qtx.InsertChannelAlbumMessages(ctx, db.InsertChannelAlbumMessagesParams{
		ChannelID: channelID, LocalIds: localIDs, FromID: fromID, Texts: texts, RandomIds: randomIDs,
		FileIds: fileIDs, ReplyToMsgID: replyTo, GroupedID: a.GroupedID,
	}); err != nil {
		return ChannelAlbumResult{}, fmt.Errorf("insert channel album: %w", err)
	}
	if err = qtx.InsertChannelNewMessageEvents(ctx, db.InsertChannelNewMessageEventsParams{
		ChannelID: channelID, Pts: pts, Type: int16(EventNewMessage), LocalIds: localIDs,
	}); err != nil {
		return ChannelAlbumResult{}, fmt.Errorf("insert channel album events: %w", err)
	}
	marked, err := qtx.UpdateChannelPostMarker(ctx, db.UpdateChannelPostMarkerParams{ChannelID: channelID, UserID: fromID})
	if err != nil {
		return ChannelAlbumResult{}, fmt.Errorf("update channel post marker: %w", err)
	}
	if marked != 1 {
		return ChannelAlbumResult{}, ErrNotMember
	}

	stored, err := qtx.ChannelMessagesByRandomIDs(ctx, db.ChannelMessagesByRandomIDsParams{ChannelID: channelID, RandomIds: randomIDs})
	if err != nil {
		return ChannelAlbumResult{}, fmt.Errorf("reload channel album: %w", err)
	}
	if len(stored) != n {
		return ChannelAlbumResult{}, fmt.Errorf("reload channel album: %d rows for %d items", len(stored), n)
	}
	result := ChannelAlbumResult{Messages: make([]ChannelMessage, n), Pts: make([]int, n)}
	for i, row := range stored {
		result.Messages[i] = channelMessageFromFields(channelMsgFields(row))
		result.Pts[i] = int(pts[i])
	}
	if err = tx.Commit(ctx); err != nil {
		return ChannelAlbumResult{}, fmt.Errorf("commit: %w", err)
	}
	return result, nil
}
