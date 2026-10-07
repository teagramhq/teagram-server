package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/teagramhq/teagram-server/internal/store/db"
)

const (
	maxPollAnswers     = 10
	maxPollOptionBytes = 100
	maxPollTextBytes   = 4096
	maxPollIDAttempts  = 5
)

var (
	ErrPollInvalid                    = errors.New("poll invalid")
	ErrBroadcastPublicVotersForbidden = errors.New("broadcast public voters forbidden")
	ErrPollClosed                     = errors.New("poll closed")
	ErrPollVoteNotAllowed             = errors.New("poll vote change not allowed")
	ErrPollVoteRequired               = errors.New("poll vote required before listing voters")
	ErrPollDenied                     = errors.New("poll operation denied")
)

// PollMessageRef addresses a caller-owned message copy. Poll identity is
// resolved from this message, never from an id supplied as authority.
type PollMessageRef struct {
	PeerType PeerType
	PeerID   int64
	LocalID  int64
}

// PollDraft is fixed-answer poll metadata before canonical normalization.
// OpenAnswers is accepted only so callers can normalize the supported client
// payload; it is always stripped before storage and readback.
type PollDraft struct {
	Question            []byte
	DescriptionEntities []PollDescriptionEntity
	Answers             []PollAnswer
	PublicVoters        bool
	MultipleChoice      bool
	Quiz                bool
	OpenAnswers         bool
	ShuffleAnswers      bool
	RevotingDisabled    bool
	ClosePeriod         int
	CloseDate           *time.Time
	Solution            []byte
}

// PollAnswer contains canonical option data plus the viewer-specific result
// fields returned by PollForMessage and CastPollVote.
type PollAnswer struct {
	Option     []byte
	Text       []byte
	Correct    bool
	VoterCount int64
	Chosen     bool
}

// Poll is one viewer's rendering of the canonical poll and committed results.
// Correct answers and the solution are present only after this viewer votes or
// the poll closes. Voter identities are never returned here.
type Poll struct {
	ID                  int64
	Creator             bool
	Question            []byte
	DescriptionEntities []PollDescriptionEntity
	Answers             []PollAnswer
	PublicVoters        bool
	MultipleChoice      bool
	Quiz                bool
	OpenAnswers         bool
	ShuffleAnswers      bool
	RevotingDisabled    bool
	Closed              bool
	CloseDate           *time.Time
	Solution            []byte
	HasVoted            bool
	VoterCount          int64
}

// PollVoter is one privacy-authorized public voter and their selected options.
type PollVoter struct {
	UserID       int64
	Options      [][]byte
	FirstVotedAt time.Time
}

// PollVoterPage is a bounded public-voter result page.
type PollVoterPage struct {
	Count      int
	Voters     []PollVoter
	NextOffset string
}

const (
	defaultPollVoterPageSize = 10
	maxPollVoterPageSize     = 100
)

// CreatePoll stores one canonical poll for every live copy of the outgoing
// message. The message random_id deduplicates retries; every retry returns the
// originally stored, per-viewer rendering without changing poll state.
func (s *Store) CreatePoll(ctx context.Context, creatorID int64, ref PollMessageRef, draft PollDraft) (poll Poll, duplicate bool, err error) {
	if err = validatePollRef(creatorID, ref); err != nil {
		return Poll{}, false, err
	}
	if _, err = normalizePollDraftShape(draft); err != nil {
		return Poll{}, false, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Poll{}, false, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)
	var lockedChat db.Chat
	if ref.PeerType == PeerTypeChat {
		if err = pollPeerAccess(ctx, qtx, creatorID, ref); err != nil {
			return Poll{}, false, err
		}
		// Match chat fan-out lock order: hold the chat row before taking any
		// per-owner locks, so poll admission observes rights changes in sequence.
		lockedChat, err = qtx.ChatByIDForUpdate(ctx, ref.PeerID)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return Poll{}, false, ErrNotMember
		case err != nil:
			return Poll{}, false, fmt.Errorf("lock poll chat: %w", err)
		}
	}

	msg, copies, err := lockPollMessage(ctx, tx, qtx, creatorID, ref)
	if err != nil {
		return Poll{}, false, err
	}
	if !msg.Out || msg.FromID != creatorID {
		return Poll{}, false, ErrMessageInvalid
	}
	if ref.PeerType == PeerTypeChat {
		if creatorID != lockedChat.CreatorID && hasChatRight(lockedChat.DefaultBannedRights, "send_polls") {
			return Poll{}, false, ErrChatWriteForbidden
		}
	}
	poll, duplicate, err = createPollForMessageTx(ctx, qtx, creatorID, msg, copies, draft, s.now(), false)
	if err != nil {
		return Poll{}, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Poll{}, false, fmt.Errorf("commit poll: %w", err)
	}
	return poll, duplicate, nil
}

// createPollForMessageTx creates a poll while the message row and its owning
// transaction are already held by the caller. Existing-only is used for a
// message random_id retry: a reused id may return its original poll, but must
// never attach a new poll to an unrelated existing message.
func createPollForMessageTx(
	ctx context.Context,
	q *db.Queries,
	creatorID int64,
	msg db.Message,
	copies []db.Message,
	draft PollDraft,
	now time.Time,
	existingOnly bool,
) (Poll, bool, error) {
	if !msg.Out || msg.FromID != creatorID || msg.ActionType != int16(ChatActionNone) || len(copies) == 0 {
		return Poll{}, false, ErrMessageInvalid
	}
	if _, err := normalizePollDraftShape(draft); err != nil {
		return Poll{}, false, err
	}
	if msg.RandomID != 0 {
		prior, err := q.PollByCreatorRandomID(ctx, db.PollByCreatorRandomIDParams{CreatorID: creatorID, RandomID: msg.RandomID})
		switch {
		case err == nil:
			if prior.SourceLocalID != msg.LocalID {
				return Poll{}, false, ErrPollInvalid
			}
			poll, err := pollView(ctx, q, prior, creatorID)
			if err != nil {
				return Poll{}, false, err
			}
			return poll, true, nil
		case !errors.Is(err, pgx.ErrNoRows):
			return Poll{}, false, fmt.Errorf("poll create dedup lookup: %w", err)
		case existingOnly:
			return Poll{}, false, ErrPollInvalid
		}
	}
	canonical, err := normalizePollDraft(draft, now)
	if err != nil {
		return Poll{}, false, err
	}
	descriptionEntities, err := encodePollDescriptionEntities(canonical.DescriptionEntities)
	if err != nil {
		return Poll{}, false, fmt.Errorf("encode poll description entities: %w", err)
	}
	closeDate := pgtype.Timestamptz{}
	if canonical.CloseDate != nil {
		closeDate = pgtype.Timestamptz{Time: *canonical.CloseDate, Valid: true}
	}
	var row db.Poll
	created := false
	for range maxPollIDAttempts {
		id, err := randomPollID()
		if err != nil {
			return Poll{}, false, fmt.Errorf("generate poll id: %w", err)
		}
		row, err = q.InsertPoll(ctx, db.InsertPollParams{
			ID: id, CreatorID: creatorID, RandomID: msg.RandomID, SourceLocalID: msg.LocalID,
			Question: canonical.Question, PublicVoters: canonical.PublicVoters,
			DescriptionEntities: descriptionEntities,
			MultipleChoice:      canonical.MultipleChoice, Quiz: canonical.Quiz,
			ShuffleAnswers: canonical.ShuffleAnswers, RevotingDisabled: canonical.RevotingDisabled,
			CloseDate: closeDate, Solution: canonical.Solution,
		})
		if err == nil {
			created = true
			break
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return Poll{}, false, fmt.Errorf("insert poll: %w", err)
		}
		if msg.RandomID != 0 {
			prior, lookupErr := q.PollByCreatorRandomID(ctx, db.PollByCreatorRandomIDParams{CreatorID: creatorID, RandomID: msg.RandomID})
			if lookupErr == nil {
				if prior.SourceLocalID != msg.LocalID {
					return Poll{}, false, ErrPollInvalid
				}
				poll, viewErr := pollView(ctx, q, prior, creatorID)
				if viewErr != nil {
					return Poll{}, false, viewErr
				}
				return poll, true, nil
			}
			if !errors.Is(lookupErr, pgx.ErrNoRows) {
				return Poll{}, false, fmt.Errorf("poll create dedup lookup: %w", lookupErr)
			}
		}
	}
	if !created {
		return Poll{}, false, errors.New("generate unique poll id: retry limit reached")
	}
	for position, answer := range canonical.Answers {
		if err = q.InsertPollOption(ctx, db.InsertPollOptionParams{
			PollID: row.ID, Option: answer.Option, Text: answer.Text,
			Correct: answer.Correct, Position: int16(position),
		}); err != nil {
			return Poll{}, false, fmt.Errorf("insert poll option %d: %w", position, err)
		}
	}
	for _, copy := range copies {
		if copy.Deleted {
			continue
		}
		n, err := q.InsertPollMessageCopy(ctx, db.InsertPollMessageCopyParams{OwnerID: copy.OwnerID, LocalID: copy.LocalID, PollID: row.ID})
		if err != nil {
			return Poll{}, false, fmt.Errorf("link poll to message copy %d/%d: %w", copy.OwnerID, copy.LocalID, err)
		}
		if n != 1 {
			return Poll{}, false, ErrPollInvalid
		}
	}
	poll, err := pollView(ctx, q, row, creatorID)
	if err != nil {
		return Poll{}, false, err
	}
	return poll, false, nil
}

func createChannelPollTx(
	ctx context.Context,
	q *db.Queries,
	channelID, creatorID, randomID, localID int64,
	draft PollDraft,
	now time.Time,
) (Poll, error) {
	if randomID != 0 {
		_, err := q.PollByCreatorRandomID(ctx, db.PollByCreatorRandomIDParams{CreatorID: creatorID, RandomID: randomID})
		switch {
		case err == nil:
			return Poll{}, ErrPollInvalid
		case !errors.Is(err, pgx.ErrNoRows):
			return Poll{}, fmt.Errorf("channel poll random id lookup: %w", err)
		}
	}
	canonical, err := normalizePollDraft(draft, now)
	if err != nil {
		return Poll{}, err
	}
	descriptionEntities, err := encodePollDescriptionEntities(canonical.DescriptionEntities)
	if err != nil {
		return Poll{}, fmt.Errorf("encode channel poll description entities: %w", err)
	}
	closeDate := pgtype.Timestamptz{}
	if canonical.CloseDate != nil {
		closeDate = pgtype.Timestamptz{Time: *canonical.CloseDate, Valid: true}
	}
	var row db.Poll
	created := false
	for range maxPollIDAttempts {
		id, idErr := randomPollID()
		if idErr != nil {
			return Poll{}, fmt.Errorf("generate channel poll id: %w", idErr)
		}
		row, err = q.InsertPoll(ctx, db.InsertPollParams{
			ID: id, CreatorID: creatorID, RandomID: randomID, SourceLocalID: localID,
			Question: canonical.Question, PublicVoters: canonical.PublicVoters,
			DescriptionEntities: descriptionEntities,
			MultipleChoice:      canonical.MultipleChoice, Quiz: canonical.Quiz,
			ShuffleAnswers: canonical.ShuffleAnswers, RevotingDisabled: canonical.RevotingDisabled,
			CloseDate: closeDate, Solution: canonical.Solution,
		})
		if err == nil {
			created = true
			break
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return Poll{}, fmt.Errorf("insert channel poll: %w", err)
		}
		if randomID != 0 {
			if _, lookupErr := q.PollByCreatorRandomID(ctx, db.PollByCreatorRandomIDParams{CreatorID: creatorID, RandomID: randomID}); lookupErr == nil {
				return Poll{}, ErrPollInvalid
			} else if !errors.Is(lookupErr, pgx.ErrNoRows) {
				return Poll{}, fmt.Errorf("channel poll random id retry lookup: %w", lookupErr)
			}
		}
	}
	if !created {
		return Poll{}, errors.New("generate unique channel poll id: retry limit reached")
	}
	for position, answer := range canonical.Answers {
		if err = q.InsertPollOption(ctx, db.InsertPollOptionParams{
			PollID: row.ID, Option: answer.Option, Text: answer.Text,
			Correct: answer.Correct, Position: int16(position),
		}); err != nil {
			return Poll{}, fmt.Errorf("insert channel poll option %d: %w", position, err)
		}
	}
	inserted, err := q.InsertChannelPollMessage(ctx, db.InsertChannelPollMessageParams{
		ChannelID: channelID, LocalID: localID, PollID: row.ID,
	})
	if err != nil {
		return Poll{}, fmt.Errorf("link poll to channel message: %w", err)
	}
	if inserted != 1 {
		return Poll{}, ErrPollInvalid
	}
	poll, err := pollView(ctx, q, row, creatorID)
	if err != nil {
		return Poll{}, err
	}
	return poll, nil
}

// PollForMessage returns a caller-authorized poll view for one of their message
// copies. The read uses one database snapshot for membership and poll results.
func (s *Store) PollForMessage(ctx context.Context, viewerID int64, ref PollMessageRef) (Poll, error) {
	if err := validatePollRef(viewerID, ref); err != nil {
		return Poll{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Poll{}, fmt.Errorf("begin poll read: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)
	if err = pollPeerAccess(ctx, qtx, viewerID, ref); err != nil {
		return Poll{}, err
	}
	var msg db.Poll
	if ref.PeerType == PeerTypeChannel {
		msg, err = qtx.PollByChannelMessage(ctx, db.PollByChannelMessageParams{ChannelID: ref.PeerID, LocalID: ref.LocalID})
	} else {
		msg, err = qtx.PollByMessage(ctx, db.PollByMessageParams{
			OwnerID:  viewerID,
			LocalID:  ref.LocalID,
			PeerType: int16(ref.PeerType),
			PeerID:   ref.PeerID,
		})
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return Poll{}, ErrMessageInvalid
	}
	if err != nil {
		return Poll{}, fmt.Errorf("poll by message: %w", err)
	}
	poll, err := pollView(ctx, qtx, msg, viewerID)
	if err != nil {
		return Poll{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Poll{}, fmt.Errorf("commit poll read: %w", err)
	}
	return poll, nil
}

// PollMessageCopiesByOwnerLocalIDs returns the owner's local IDs linked to
// polls from the provided batch. Empty input avoids a database round trip.
func (s *Store) PollMessageCopiesByOwnerLocalIDs(ctx context.Context, ownerID int64, localIDs []int64) ([]int64, error) {
	if len(localIDs) == 0 {
		return []int64{}, nil
	}
	rows, err := s.q.PollMessageCopiesByOwnerLocalIDs(ctx, db.PollMessageCopiesByOwnerLocalIDsParams{
		OwnerID:  ownerID,
		LocalIds: localIDs,
	})
	if err != nil {
		return nil, fmt.Errorf("poll message copies by owner local ids: %w", err)
	}
	return rows, nil
}

// ChannelPollMessageLocalIDs returns the subset of channel message ids linked
// to canonical polls, ordered for stable rendering.
func (s *Store) ChannelPollMessageLocalIDs(ctx context.Context, channelID int64, localIDs []int64) ([]int64, error) {
	if len(localIDs) == 0 {
		return []int64{}, nil
	}
	rows, err := s.q.ChannelPollMessageLocalIDs(ctx, db.ChannelPollMessageLocalIDsParams{
		ChannelID: channelID,
		LocalIds:  localIDs,
	})
	if err != nil {
		return nil, fmt.Errorf("channel poll message ids: %w", err)
	}
	return rows, nil
}

// HasPollMessageCopy reports whether an owned message row is linked to a poll.
// It is for paths that already validated ownership but cannot render a poll,
// such as forwarding, where treating the poll as its empty text would lose it.
func (s *Store) HasPollMessageCopy(ctx context.Context, ownerID int64, ref PollMessageRef) (bool, error) {
	if ownerID <= 0 || ref.PeerID <= 0 || ref.LocalID <= 0 || (ref.PeerType != PeerTypeUser && ref.PeerType != PeerTypeChat) {
		return false, ErrMessageInvalid
	}
	_, err := s.q.PollByMessage(ctx, db.PollByMessageParams{
		OwnerID: ownerID, LocalID: ref.LocalID, PeerType: int16(ref.PeerType), PeerID: ref.PeerID,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("poll by message copy: %w", err)
	default:
		return true, nil
	}
}

// PollForViewerByID resolves a recipient-owned message copy and then applies
// the same live membership and viewer-specific result checks as PollForMessage.
func (s *Store) PollForViewerByID(ctx context.Context, viewerID, pollID int64) (Poll, PollMessageRef, error) {
	if viewerID <= 0 || pollID <= 0 {
		return Poll{}, PollMessageRef{}, ErrMessageInvalid
	}
	var ref PollMessageRef
	row, err := s.q.PollMessageForOwner(ctx, db.PollMessageForOwnerParams{OwnerID: viewerID, PollID: pollID})
	switch {
	case err == nil:
		ref = PollMessageRef{PeerType: PeerType(row.PeerType), PeerID: row.PeerID, LocalID: row.LocalID}
	case errors.Is(err, pgx.ErrNoRows):
		channelRow, channelErr := s.q.PollChannelMessageForViewer(ctx, db.PollChannelMessageForViewerParams{PollID: pollID, UserID: viewerID})
		if errors.Is(channelErr, pgx.ErrNoRows) {
			return Poll{}, PollMessageRef{}, ErrMessageInvalid
		}
		if channelErr != nil {
			return Poll{}, PollMessageRef{}, fmt.Errorf("resolve viewer channel poll message: %w", channelErr)
		}
		ref = PollMessageRef{PeerType: PeerTypeChannel, PeerID: channelRow.ChannelID, LocalID: channelRow.LocalID}
	default:
		return Poll{}, PollMessageRef{}, fmt.Errorf("resolve viewer poll message: %w", err)
	}
	poll, err := s.PollForMessage(ctx, viewerID, ref)
	if err != nil {
		return Poll{}, PollMessageRef{}, err
	}
	return poll, ref, nil
}

// CastPollVote atomically replaces one viewer's current selection. The poll row
// lock is acquired after the message-owner advisory locks and membership check,
// following the existing owner-before-poll lock order.
func (s *Store) CastPollVote(ctx context.Context, viewerID int64, ref PollMessageRef, selected [][]byte) (Poll, error) {
	poll, _, _, err := s.CastPollVoteWithUpdates(ctx, viewerID, ref, selected)
	return poll, err
}

// CastPollVoteWithChange reports whether the canonical selection changed. A
// successful identical retry returns changed=false and performs no writes.
func (s *Store) CastPollVoteWithChange(ctx context.Context, viewerID int64, ref PollMessageRef, selected [][]byte) (Poll, bool, error) {
	poll, _, changed, err := s.CastPollVoteWithUpdates(ctx, viewerID, ref, selected)
	return poll, changed, err
}

// CastPollVoteWithUpdates atomically replaces one viewer's selection and, when
// it changes, records a per-owner edit event for each current entitled copy.
// The events carry only the copy's local id; results are rendered for each owner
// when that owner reads or is pushed their pending updates.
func (s *Store) CastPollVoteWithUpdates(ctx context.Context, viewerID int64, ref PollMessageRef, selected [][]byte) (Poll, map[int64]int, bool, error) {
	if err := validatePollRef(viewerID, ref); err != nil {
		return Poll{}, nil, false, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Poll{}, nil, false, fmt.Errorf("begin poll vote: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)
	var copies []db.Message
	var pollRow db.Poll
	if ref.PeerType == PeerTypeChannel {
		pollRow, err = lockChannelPollForMutation(ctx, qtx, viewerID, ref)
	} else {
		_, copies, pollRow, err = lockPollForMutation(ctx, tx, qtx, viewerID, ref)
	}
	if err != nil {
		return Poll{}, nil, false, err
	}
	options, err := normalizePollSelection(selected)
	if err != nil {
		return Poll{}, nil, false, err
	}
	_, voteErr := qtx.PollVoteByVoter(ctx, db.PollVoteByVoterParams{PollID: pollRow.ID, VoterID: viewerID})
	hasVote := voteErr == nil
	if voteErr != nil && !errors.Is(voteErr, pgx.ErrNoRows) {
		return Poll{}, nil, false, fmt.Errorf("poll vote lookup: %w", voteErr)
	}
	previous, err := qtx.PollVoteOptionsByVoter(ctx, db.PollVoteOptionsByVoterParams{
		PollID:  pollRow.ID,
		VoterID: viewerID,
	})
	if err != nil {
		return Poll{}, nil, false, fmt.Errorf("poll vote options: %w", err)
	}
	closed, err := qtx.PollIsClosed(ctx, pollRow.ID)
	if err != nil {
		return Poll{}, nil, false, fmt.Errorf("poll closed state: %w", err)
	}
	if closed {
		return Poll{}, nil, false, ErrPollClosed
	}
	if samePollSelection(previous, options) {
		poll, viewErr := pollView(ctx, qtx, pollRow, viewerID)
		if viewErr != nil {
			return Poll{}, nil, false, viewErr
		}
		if err = tx.Commit(ctx); err != nil {
			return Poll{}, nil, false, fmt.Errorf("commit unchanged poll vote: %w", err)
		}
		return poll, nil, false, nil
	}
	if len(options) > 1 && !pollRow.MultipleChoice {
		return Poll{}, nil, false, ErrPollInvalid
	}
	optionRows, err := qtx.PollOptionResults(ctx, db.PollOptionResultsParams{
		PollID:   pollRow.ID,
		ViewerID: viewerID,
	})
	if err != nil {
		return Poll{}, nil, false, fmt.Errorf("poll options: %w", err)
	}
	allowed := make(map[string]bool, len(optionRows))
	for _, option := range optionRows {
		allowed[string(option.Option)] = true
	}
	for _, option := range options {
		if !allowed[string(option)] {
			return Poll{}, nil, false, ErrPollInvalid
		}
	}
	if pollRow.RevotingDisabled && hasVote {
		return Poll{}, nil, false, ErrPollVoteNotAllowed
	}
	if len(options) == 0 {
		if hasVote {
			if err = qtx.DeletePollVote(ctx, db.DeletePollVoteParams{PollID: pollRow.ID, VoterID: viewerID}); err != nil {
				return Poll{}, nil, false, fmt.Errorf("retract poll vote: %w", err)
			}
		}
	} else {
		if err = qtx.InsertPollVote(ctx, db.InsertPollVoteParams{PollID: pollRow.ID, VoterID: viewerID}); err != nil {
			return Poll{}, nil, false, fmt.Errorf("record poll voter: %w", err)
		}
		if err = qtx.DeletePollVoteOptionsByVoter(ctx, db.DeletePollVoteOptionsByVoterParams{PollID: pollRow.ID, VoterID: viewerID}); err != nil {
			return Poll{}, nil, false, fmt.Errorf("replace poll selections: %w", err)
		}
		for _, option := range options {
			if err = qtx.InsertPollVoteOption(ctx, db.InsertPollVoteOptionParams{
				PollID:  pollRow.ID,
				VoterID: viewerID,
				Option:  option,
			}); err != nil {
				return Poll{}, nil, false, fmt.Errorf("save poll selection: %w", err)
			}
		}
	}

	active := map[int64]bool{viewerID: true}
	if ref.PeerType == PeerTypeChat {
		active, err = chatMembers(ctx, qtx, ref.PeerID)
		if err != nil {
			return Poll{}, nil, false, err
		}
	} else if ref.PeerType == PeerTypeUser && ref.PeerID != viewerID {
		for _, copy := range copies {
			active[copy.OwnerID] = true
		}
	}
	ownerPts := make(map[int64]int)
	for _, copy := range copies {
		if copy.Deleted || !active[copy.OwnerID] {
			continue
		}
		pts, bumpErr := qtx.BumpPtsOnly(ctx, copy.OwnerID)
		if bumpErr != nil {
			return Poll{}, nil, false, fmt.Errorf("bump poll vote pts for %d: %w", copy.OwnerID, bumpErr)
		}
		if err = qtx.InsertEvent(ctx, db.InsertEventParams{
			OwnerID: copy.OwnerID,
			Pts:     pts,
			Type:    int16(EventEdit),
			LocalID: copy.LocalID,
		}); err != nil {
			return Poll{}, nil, false, fmt.Errorf("record poll vote edit for %d/%d: %w", copy.OwnerID, copy.LocalID, err)
		}
		ownerPts[copy.OwnerID] = int(pts)
	}

	poll, err := pollView(ctx, qtx, pollRow, viewerID)
	if err != nil {
		return Poll{}, nil, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Poll{}, nil, false, fmt.Errorf("commit poll vote: %w", err)
	}
	return poll, ownerPts, true, nil
}

// PollVoters returns a bounded public-voter page after the caller has voted or
// the poll has closed. Anonymous polls never disclose voter identities.
func (s *Store) PollVoters(ctx context.Context, viewerID int64, ref PollMessageRef, option []byte, offset string, limit int) (PollVoterPage, error) {
	if err := validatePollRef(viewerID, ref); err != nil {
		return PollVoterPage{}, err
	}
	if limit < 0 {
		return PollVoterPage{}, ErrPollInvalid
	}
	if limit == 0 {
		limit = defaultPollVoterPageSize
	}
	limit = min(limit, maxPollVoterPageSize)
	if len(option) > maxPollOptionBytes {
		return PollVoterPage{}, ErrPollInvalid
	}
	cursorAt, cursorUserID, hasCursor, err := decodePollVoterOffset(offset)
	if err != nil {
		return PollVoterPage{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return PollVoterPage{}, fmt.Errorf("begin poll voter read: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)
	if err = pollPeerAccess(ctx, qtx, viewerID, ref); err != nil {
		return PollVoterPage{}, err
	}
	var pollRow db.Poll
	if ref.PeerType == PeerTypeChannel {
		pollRow, err = qtx.PollByChannelMessage(ctx, db.PollByChannelMessageParams{ChannelID: ref.PeerID, LocalID: ref.LocalID})
	} else {
		pollRow, err = qtx.PollByMessage(ctx, db.PollByMessageParams{
			OwnerID: viewerID, LocalID: ref.LocalID, PeerType: int16(ref.PeerType), PeerID: ref.PeerID,
		})
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return PollVoterPage{}, ErrMessageInvalid
	}
	if err != nil {
		return PollVoterPage{}, fmt.Errorf("load poll for voter list: %w", err)
	}
	view, err := pollView(ctx, qtx, pollRow, viewerID)
	if err != nil {
		return PollVoterPage{}, err
	}
	if !view.PublicVoters {
		return PollVoterPage{}, ErrPollDenied
	}
	if !view.HasVoted && !view.Closed {
		return PollVoterPage{}, ErrPollVoteRequired
	}
	if len(option) != 0 {
		exists, e := qtx.PollOptionExists(ctx, db.PollOptionExistsParams{PollID: pollRow.ID, Option: option})
		if e != nil {
			return PollVoterPage{}, fmt.Errorf("validate poll voter option: %w", e)
		}
		if !exists {
			return PollVoterPage{}, ErrPollInvalid
		}
	}
	queryOption := []byte{}
	if len(option) != 0 {
		queryOption = bytes.Clone(option)
	}
	count, err := qtx.PollVoterCountForOption(ctx, db.PollVoterCountForOptionParams{PollID: pollRow.ID, Option: queryOption})
	if err != nil {
		return PollVoterPage{}, fmt.Errorf("count poll voters: %w", err)
	}
	var cursorTime pgtype.Timestamptz
	if hasCursor {
		cursorTime = pgtype.Timestamptz{Time: cursorAt, Valid: true}
	}
	rows, err := qtx.PollVotersPage(ctx, db.PollVotersPageParams{
		PollID: pollRow.ID, Option: queryOption, HasCursor: hasCursor,
		CursorAt: cursorTime, CursorUserID: cursorUserID, Lim: int32(limit + 1),
	})
	if err != nil {
		return PollVoterPage{}, fmt.Errorf("page poll voters: %w", err)
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	page := PollVoterPage{Count: int(count), Voters: make([]PollVoter, len(rows))}
	for i, row := range rows {
		options, e := decodePollVoterOptions(row.Options)
		if e != nil {
			return PollVoterPage{}, fmt.Errorf("decode poll voter options: %w", e)
		}
		if len(option) != 0 {
			options = [][]byte{bytes.Clone(option)}
		}
		page.Voters[i] = PollVoter{UserID: row.VoterID, Options: options, FirstVotedAt: row.FirstVotedAt.Time.UTC()}
	}
	if hasMore && len(page.Voters) > 0 {
		last := page.Voters[len(page.Voters)-1]
		page.NextOffset = encodePollVoterOffset(last.FirstVotedAt, last.UserID)
	}
	if err = tx.Commit(ctx); err != nil {
		return PollVoterPage{}, fmt.Errorf("commit poll voter read: %w", err)
	}
	return page, nil
}

func encodePollVoterOffset(at time.Time, userID int64) string {
	var cursor [16]byte
	// Voter timestamps and IDs come from validated persisted rows and are positive.
	binary.BigEndian.PutUint64(cursor[:8], uint64(at.UnixMicro()))
	binary.BigEndian.PutUint64(cursor[8:], uint64(userID)) //nolint:gosec // G115: persisted user IDs are positive.
	return base64.RawURLEncoding.EncodeToString(cursor[:])
}

func decodePollVoterOffset(offset string) (time.Time, int64, bool, error) {
	if offset == "" {
		return time.Time{}, 0, false, nil
	}
	cursor, err := base64.RawURLEncoding.DecodeString(offset)
	if err != nil || len(cursor) != 16 || base64.RawURLEncoding.EncodeToString(cursor) != offset {
		return time.Time{}, 0, false, ErrPollInvalid
	}
	microsRaw := binary.BigEndian.Uint64(cursor[:8])
	userIDRaw := binary.BigEndian.Uint64(cursor[8:])
	if microsRaw > math.MaxInt64 || userIDRaw > math.MaxInt64 {
		return time.Time{}, 0, false, ErrPollInvalid
	}
	micros := int64(microsRaw)
	userID := int64(userIDRaw)
	if micros <= 0 || userID <= 0 {
		return time.Time{}, 0, false, ErrPollInvalid
	}
	return time.UnixMicro(micros).UTC(), userID, true, nil
}

func decodePollVoterOptions(encoded any) ([][]byte, error) {
	var value string
	switch raw := encoded.(type) {
	case string:
		value = raw
	case []byte:
		value = string(raw)
	default:
		return nil, fmt.Errorf("unsupported poll voter options value %T", encoded)
	}
	if value == "" {
		return nil, nil
	}
	parts := strings.Split(value, ".")
	options := make([][]byte, 0, len(parts))
	for _, part := range parts {
		option, err := base64.StdEncoding.DecodeString(part)
		if err != nil || len(option) == 0 || len(option) > maxPollOptionBytes {
			return nil, ErrPollInvalid
		}
		options = append(options, option)
	}
	return options, nil
}

// ClosePoll applies only the closed transition. A repeated close is a no-op,
// and no caller-supplied poll metadata can overwrite the canonical record.
func (s *Store) ClosePoll(ctx context.Context, callerID int64, ref PollMessageRef) (bool, error) {
	changed, _, err := s.ClosePollWithUpdates(ctx, callerID, ref)
	return changed, err
}

// ClosePollWithUpdates applies the close transition and returns each current
// recipient's committed pts for the durable edit event.
func (s *Store) ClosePollWithUpdates(ctx context.Context, callerID int64, ref PollMessageRef) (bool, map[int64]int, error) {
	if err := validatePollRef(callerID, ref); err != nil {
		return false, nil, err
	}
	if ref.PeerType == PeerTypeChannel {
		return s.closeChannelPollWithUpdates(ctx, callerID, ref)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, nil, fmt.Errorf("begin poll close: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)
	_, copies, err := lockPollMessage(ctx, tx, qtx, callerID, ref)
	if err != nil {
		return false, nil, err
	}
	pollRow, err := qtx.PollByMessage(ctx, db.PollByMessageParams{
		OwnerID:  callerID,
		LocalID:  ref.LocalID,
		PeerType: int16(ref.PeerType),
		PeerID:   ref.PeerID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil, ErrMessageInvalid
	}
	if err != nil {
		return false, nil, fmt.Errorf("poll by message: %w", err)
	}
	if pollRow.CreatorID != callerID {
		return false, nil, ErrPollDenied
	}
	pollRow, err = qtx.PollByIDForUpdate(ctx, pollRow.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil, ErrMessageInvalid
	}
	if err != nil {
		return false, nil, fmt.Errorf("lock poll: %w", err)
	}
	changed := false
	ownerPts := make(map[int64]int)
	if !pollRow.Closed {
		n, e := qtx.ClosePoll(ctx, pollRow.ID)
		if e != nil {
			return false, nil, fmt.Errorf("close poll: %w", e)
		}
		changed = n == 1
		if changed {
			active := map[int64]bool{callerID: true}
			if ref.PeerType == PeerTypeChat {
				active, err = chatMembers(ctx, qtx, ref.PeerID)
				if err != nil {
					return false, nil, err
				}
			} else if ref.PeerType == PeerTypeUser && ref.PeerID != callerID {
				for _, copy := range copies {
					active[copy.OwnerID] = true
				}
			}
			for _, copy := range copies {
				if copy.Deleted || !active[copy.OwnerID] {
					continue
				}
				if err = qtx.SetEditedText(ctx, db.SetEditedTextParams{
					OwnerID: copy.OwnerID,
					LocalID: copy.LocalID,
					Message: copy.Message,
				}); err != nil {
					return false, nil, fmt.Errorf("edit poll message copy %d/%d: %w", copy.OwnerID, copy.LocalID, err)
				}
				pts, e := qtx.BumpPtsOnly(ctx, copy.OwnerID)
				if e != nil {
					return false, nil, fmt.Errorf("bump poll close pts for %d: %w", copy.OwnerID, e)
				}
				if e = qtx.InsertEvent(ctx, db.InsertEventParams{
					OwnerID: copy.OwnerID,
					Pts:     pts,
					Type:    int16(EventEdit),
					LocalID: copy.LocalID,
				}); e != nil {
					return false, nil, fmt.Errorf("record poll close edit for %d/%d: %w", copy.OwnerID, copy.LocalID, e)
				}
				ownerPts[copy.OwnerID] = int(pts)
			}
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return false, nil, fmt.Errorf("commit poll close: %w", err)
	}
	return changed, ownerPts, nil
}

func (s *Store) closeChannelPollWithUpdates(ctx context.Context, callerID int64, ref PollMessageRef) (bool, map[int64]int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, nil, fmt.Errorf("begin channel poll close: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)
	if err = pollPeerAccess(ctx, qtx, callerID, ref); err != nil {
		return false, nil, err
	}
	if _, err = qtx.LockChannelState(ctx, ref.PeerID); errors.Is(err, pgx.ErrNoRows) {
		return false, nil, ErrNotMember
	} else if err != nil {
		return false, nil, fmt.Errorf("lock channel poll state: %w", err)
	}
	participant, err := qtx.ChannelPollParticipantForUpdate(ctx, db.ChannelPollParticipantForUpdateParams{
		ChannelID: ref.PeerID,
		UserID:    callerID,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil, ErrNotMember
	case err != nil:
		return false, nil, fmt.Errorf("lock channel poll closer membership: %w", err)
	}
	member := channelMemberFromRow(participant)
	if member.Banned(time.Now()) {
		return false, nil, ErrNotMember
	}
	message, err := qtx.ChannelMessageByLocal(ctx, db.ChannelMessageByLocalParams{ChannelID: ref.PeerID, LocalID: ref.LocalID})
	switch {
	case errors.Is(err, pgx.ErrNoRows) || err == nil && (message.Deleted || message.ActionType != int16(ChannelMessageActionNone)):
		return false, nil, ErrMessageInvalid
	case err != nil:
		return false, nil, fmt.Errorf("load channel poll message: %w", err)
	}
	pollRow, err := qtx.PollByChannelMessage(ctx, db.PollByChannelMessageParams{ChannelID: ref.PeerID, LocalID: ref.LocalID})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil, ErrMessageInvalid
	case err != nil:
		return false, nil, fmt.Errorf("load channel poll: %w", err)
	}
	if callerID != message.FromID && member.Role < channelRoleAdmin {
		return false, nil, ErrPollDenied
	}
	pollRow, err = qtx.PollByIDForUpdate(ctx, pollRow.ID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil, ErrMessageInvalid
	case err != nil:
		return false, nil, fmt.Errorf("lock channel poll: %w", err)
	}
	if pollRow.Closed {
		if err = tx.Commit(ctx); err != nil {
			return false, nil, fmt.Errorf("commit unchanged channel poll close: %w", err)
		}
		return false, nil, nil
	}
	if n, e := qtx.ClosePoll(ctx, pollRow.ID); e != nil {
		return false, nil, fmt.Errorf("close channel poll: %w", e)
	} else if n != 1 {
		return false, nil, ErrMessageInvalid
	}
	if n, e := qtx.SetChannelMessageEditDate(ctx, db.SetChannelMessageEditDateParams{ChannelID: ref.PeerID, LocalID: ref.LocalID}); e != nil {
		return false, nil, fmt.Errorf("mark channel poll edited: %w", e)
	} else if n != 1 {
		return false, nil, ErrMessageInvalid
	}
	pts, err := qtx.BumpChannelPtsOnly(ctx, ref.PeerID)
	if err != nil {
		return false, nil, fmt.Errorf("bump channel poll close pts: %w", err)
	}
	if err = qtx.InsertChannelEvent(ctx, db.InsertChannelEventParams{
		ChannelID: ref.PeerID,
		Pts:       pts,
		Type:      int16(EventEdit),
		LocalID:   ref.LocalID,
	}); err != nil {
		return false, nil, fmt.Errorf("record channel poll close edit: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return false, nil, fmt.Errorf("commit channel poll close: %w", err)
	}
	return true, map[int64]int{callerID: int(pts)}, nil
}

func validatePollRef(viewerID int64, ref PollMessageRef) error {
	if viewerID <= 0 || ref.PeerID <= 0 || ref.LocalID <= 0 {
		return ErrMessageInvalid
	}
	switch ref.PeerType {
	case PeerTypeUser:
	case PeerTypeChat, PeerTypeChannel:
	default:
		return ErrMessageInvalid
	}
	return nil
}

func pollPeerAccess(ctx context.Context, q *db.Queries, viewerID int64, ref PollMessageRef) error {
	if err := validatePollRef(viewerID, ref); err != nil {
		return err
	}
	if ref.PeerType == PeerTypeUser {
		return nil
	}
	if ref.PeerType == PeerTypeChannel {
		row, err := q.ChannelParticipantByUser(ctx, db.ChannelParticipantByUserParams{
			ChannelID: ref.PeerID,
			UserID:    viewerID,
		})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return ErrNotMember
		case err != nil:
			return fmt.Errorf("poll channel membership: %w", err)
		}
		if channelMemberFromRow(row).Banned(time.Now()) {
			return ErrNotMember
		}
		return nil
	}
	member, err := q.IsChatMember(ctx, db.IsChatMemberParams{ChatID: ref.PeerID, UserID: viewerID})
	if err != nil {
		return fmt.Errorf("poll chat membership: %w", err)
	}
	if !member {
		return ErrNotMember
	}
	return nil
}

func lockPollMessage(ctx context.Context, tx pgx.Tx, q *db.Queries, viewerID int64, ref PollMessageRef) (db.Message, []db.Message, error) {
	if err := pollPeerAccess(ctx, q, viewerID, ref); err != nil {
		return db.Message{}, nil, err
	}
	pre, err := q.MessageByOwnerLocal(ctx, db.MessageByOwnerLocalParams{OwnerID: viewerID, LocalID: ref.LocalID})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Message{}, nil, ErrMessageInvalid
	}
	if err != nil {
		return db.Message{}, nil, fmt.Errorf("load poll message: %w", err)
	}
	if !pollMessageMatches(pre, ref) {
		return db.Message{}, nil, ErrMessageInvalid
	}
	copies := []db.Message{pre}
	if ref.PeerType == PeerTypeChat {
		copies, err = chatCopies(ctx, q, pre)
		if err != nil {
			return db.Message{}, nil, err
		}
	} else if ref.PeerType == PeerTypeUser && ref.PeerID != viewerID {
		peer, peerErr := q.MessageByOwnerLocal(ctx, db.MessageByOwnerLocalParams{OwnerID: pre.PeerID, LocalID: pre.PeerLocalID})
		if errors.Is(peerErr, pgx.ErrNoRows) {
			return db.Message{}, nil, ErrMessageInvalid
		}
		if peerErr != nil {
			return db.Message{}, nil, fmt.Errorf("load private poll message copy: %w", peerErr)
		}
		if peer.PeerType != int16(PeerTypeUser) || peer.PeerID != viewerID || peer.PeerLocalID != pre.LocalID || peer.FromID != pre.FromID || peer.Out == pre.Out {
			return db.Message{}, nil, ErrMessageInvalid
		}
		if !peer.Deleted {
			copies = append(copies, peer)
		}
	}
	if err = lockOwners(ctx, tx, copyOwners(copies)...); err != nil {
		return db.Message{}, nil, err
	}
	msg, err := q.MessageByOwnerLocal(ctx, db.MessageByOwnerLocalParams{OwnerID: viewerID, LocalID: ref.LocalID})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Message{}, nil, ErrMessageInvalid
	}
	if err != nil {
		return db.Message{}, nil, fmt.Errorf("reload poll message: %w", err)
	}
	if !pollMessageMatches(msg, ref) {
		return db.Message{}, nil, ErrMessageInvalid
	}
	if ref.PeerType == PeerTypeChat {
		members, e := chatMembers(ctx, q, ref.PeerID)
		if e != nil {
			return db.Message{}, nil, e
		}
		if !members[viewerID] {
			return db.Message{}, nil, ErrNotMember
		}
		copies, err = chatCopies(ctx, q, msg)
		if err != nil {
			return db.Message{}, nil, err
		}
	} else if ref.PeerType == PeerTypeUser && ref.PeerID != viewerID {
		peer, peerErr := q.MessageByOwnerLocal(ctx, db.MessageByOwnerLocalParams{OwnerID: msg.PeerID, LocalID: msg.PeerLocalID})
		if errors.Is(peerErr, pgx.ErrNoRows) {
			return db.Message{}, nil, ErrMessageInvalid
		}
		if peerErr != nil {
			return db.Message{}, nil, fmt.Errorf("reload private poll message copy: %w", peerErr)
		}
		if peer.PeerType != int16(PeerTypeUser) || peer.PeerID != viewerID || peer.PeerLocalID != msg.LocalID || peer.FromID != msg.FromID || peer.Out == msg.Out {
			return db.Message{}, nil, ErrMessageInvalid
		}
		copies = []db.Message{msg}
		if !peer.Deleted {
			copies = append(copies, peer)
		}
	}
	return msg, copies, nil
}

func lockPollForMutation(ctx context.Context, tx pgx.Tx, q *db.Queries, viewerID int64, ref PollMessageRef) (db.Message, []db.Message, db.Poll, error) {
	msg, copies, err := lockPollMessage(ctx, tx, q, viewerID, ref)
	if err != nil {
		return db.Message{}, nil, db.Poll{}, err
	}
	row, err := q.PollByMessage(ctx, db.PollByMessageParams{
		OwnerID:  viewerID,
		LocalID:  ref.LocalID,
		PeerType: int16(ref.PeerType),
		PeerID:   ref.PeerID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Message{}, nil, db.Poll{}, ErrMessageInvalid
	}
	if err != nil {
		return db.Message{}, nil, db.Poll{}, fmt.Errorf("poll by message: %w", err)
	}
	locked, err := q.PollByIDForUpdate(ctx, row.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Message{}, nil, db.Poll{}, ErrMessageInvalid
	}
	if err != nil {
		return db.Message{}, nil, db.Poll{}, fmt.Errorf("lock poll: %w", err)
	}
	return msg, copies, locked, nil
}

func lockChannelPollForMutation(ctx context.Context, q *db.Queries, viewerID int64, ref PollMessageRef) (db.Poll, error) {
	participant, err := q.ChannelPollParticipantForUpdate(ctx, db.ChannelPollParticipantForUpdateParams{
		ChannelID: ref.PeerID,
		UserID:    viewerID,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return db.Poll{}, ErrNotMember
	case err != nil:
		return db.Poll{}, fmt.Errorf("lock poll channel membership: %w", err)
	}
	if channelMemberFromRow(participant).Banned(time.Now()) {
		return db.Poll{}, ErrNotMember
	}
	row, err := q.PollByChannelMessage(ctx, db.PollByChannelMessageParams{
		ChannelID: ref.PeerID,
		LocalID:   ref.LocalID,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return db.Poll{}, ErrMessageInvalid
	case err != nil:
		return db.Poll{}, fmt.Errorf("channel poll by message: %w", err)
	}
	locked, err := q.PollByIDForUpdate(ctx, row.ID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return db.Poll{}, ErrMessageInvalid
	case err != nil:
		return db.Poll{}, fmt.Errorf("lock channel poll: %w", err)
	}
	return locked, nil
}

func pollMessageMatches(msg db.Message, ref PollMessageRef) bool {
	if msg.Deleted || msg.ActionType != int16(ChatActionNone) || PeerType(msg.PeerType) != ref.PeerType || msg.PeerID != ref.PeerID {
		return false
	}
	if ref.PeerType != PeerTypeUser {
		return true
	}
	if msg.Out {
		return msg.FromID == msg.OwnerID
	}
	return msg.FromID == ref.PeerID && msg.OwnerID != msg.FromID
}

func normalizePollDraft(draft PollDraft, now time.Time) (PollDraft, error) {
	canonical, err := normalizePollDraftShape(draft)
	if err != nil {
		return PollDraft{}, err
	}
	if canonical.ClosePeriod > 0 {
		date := now.UTC().Add(time.Duration(canonical.ClosePeriod) * time.Second)
		canonical.CloseDate = &date
		canonical.ClosePeriod = 0
	} else {
		if err = validatePollCloseDate(canonical.CloseDate, now); err != nil {
			return PollDraft{}, err
		}
	}
	return canonical, nil
}

func normalizePollDraftShape(draft PollDraft) (PollDraft, error) {
	if len(draft.Question) == 0 || len(draft.Question) > maxPollTextBytes || !utf8.Valid(draft.Question) {
		return PollDraft{}, ErrPollInvalid
	}
	if len(draft.Answers) < 2 || len(draft.Answers) > maxPollAnswers {
		return PollDraft{}, ErrPollInvalid
	}
	if draft.ClosePeriod < 0 || draft.ClosePeriod > 10*60 || (draft.ClosePeriod > 0 && draft.CloseDate != nil) {
		return PollDraft{}, ErrPollInvalid
	}
	if draft.ClosePeriod > 0 && draft.ClosePeriod < 5 {
		return PollDraft{}, ErrPollInvalid
	}
	canonical := draft
	canonical.Question = bytes.Clone(draft.Question)
	descriptionEntities, err := normalizePollDescriptionEntities(draft.DescriptionEntities)
	if err != nil {
		return PollDraft{}, err
	}
	canonical.DescriptionEntities = descriptionEntities
	canonical.Solution = bytes.Clone(draft.Solution)
	if draft.CloseDate != nil {
		date := draft.CloseDate.UTC()
		canonical.CloseDate = &date
	}
	canonical.OpenAnswers = false
	if canonical.Quiz {
		canonical.RevotingDisabled = true
	}
	canonical.Answers = make([]PollAnswer, len(draft.Answers))
	seen := make(map[string]bool, len(draft.Answers))
	correctCount := 0
	for i, answer := range draft.Answers {
		if len(answer.Option) == 0 || len(answer.Option) > maxPollOptionBytes || len(answer.Text) == 0 || len(answer.Text) > maxPollTextBytes || !utf8.Valid(answer.Text) {
			return PollDraft{}, ErrPollInvalid
		}
		key := string(answer.Option)
		if seen[key] {
			return PollDraft{}, ErrPollInvalid
		}
		seen[key] = true
		if answer.Correct {
			correctCount++
		}
		canonical.Answers[i] = PollAnswer{Option: bytes.Clone(answer.Option), Text: bytes.Clone(answer.Text), Correct: answer.Correct}
	}
	if canonical.Quiz {
		if correctCount == 0 || (!canonical.MultipleChoice && correctCount != 1) {
			return PollDraft{}, ErrPollInvalid
		}
	} else if correctCount != 0 {
		return PollDraft{}, ErrPollInvalid
	}
	return canonical, nil
}

func validatePollCloseDate(closeDate *time.Time, now time.Time) error {
	if closeDate == nil {
		return nil
	}
	until := closeDate.Sub(now)
	if until < 5*time.Second || until > 10*time.Minute {
		return ErrPollInvalid
	}
	return nil
}

// ValidatePollDraft checks request-controlled poll fields before a caller
// creates the message row that will own the poll.
func (s *Store) ValidatePollDraft(draft PollDraft) error {
	_, err := normalizePollDraft(draft, s.now())
	return err
}

// ValidatePollDraftShape checks all answer and flag data without applying the
// time-sensitive close-date window. A retry can therefore validate its fixed
// answer payload before random_id dedup even after its original deadline.
func (s *Store) ValidatePollDraftShape(draft PollDraft) error {
	_, err := normalizePollDraftShape(draft)
	return err
}

func normalizePollSelection(selected [][]byte) ([][]byte, error) {
	seen := make(map[string]bool, len(selected))
	options := make([][]byte, 0, min(len(selected), maxPollAnswers))
	for _, option := range selected {
		if len(option) == 0 || len(option) > maxPollOptionBytes {
			return nil, ErrPollInvalid
		}
		key := string(option)
		if seen[key] {
			continue
		}
		seen[key] = true
		options = append(options, bytes.Clone(option))
	}
	if len(options) > maxPollAnswers {
		return nil, ErrPollInvalid
	}
	return options, nil
}

func samePollSelection(previous [][]byte, selected [][]byte) bool {
	if len(previous) != len(selected) {
		return false
	}
	set := make(map[string]bool, len(previous))
	for _, option := range previous {
		set[string(option)] = true
	}
	for _, option := range selected {
		if !set[string(option)] {
			return false
		}
	}
	return true
}

func pollView(ctx context.Context, q *db.Queries, row db.Poll, viewerID int64) (Poll, error) {
	closed, err := q.PollIsClosed(ctx, row.ID)
	if err != nil {
		return Poll{}, fmt.Errorf("poll closed state: %w", err)
	}
	voterCount, err := q.PollVoterCount(ctx, row.ID)
	if err != nil {
		return Poll{}, fmt.Errorf("poll voter count: %w", err)
	}
	_, voteErr := q.PollVoteByVoter(ctx, db.PollVoteByVoterParams{PollID: row.ID, VoterID: viewerID})
	hasVoted := voteErr == nil
	if voteErr != nil && !errors.Is(voteErr, pgx.ErrNoRows) {
		return Poll{}, fmt.Errorf("poll viewer vote: %w", voteErr)
	}
	options, err := q.PollOptionResults(ctx, db.PollOptionResultsParams{PollID: row.ID, ViewerID: viewerID})
	if err != nil {
		return Poll{}, fmt.Errorf("poll option results: %w", err)
	}
	reveal := closed || hasVoted
	poll := Poll{
		ID:                  row.ID,
		Creator:             row.CreatorID == viewerID,
		Question:            bytes.Clone(row.Question),
		DescriptionEntities: decodePollDescriptionEntities(row.DescriptionEntities),
		PublicVoters:        row.PublicVoters,
		MultipleChoice:      row.MultipleChoice,
		Quiz:                row.Quiz,
		OpenAnswers:         false,
		ShuffleAnswers:      row.ShuffleAnswers,
		RevotingDisabled:    row.RevotingDisabled,
		Closed:              closed,
		HasVoted:            hasVoted,
		VoterCount:          voterCount,
	}
	if row.CloseDate.Valid {
		date := row.CloseDate.Time
		poll.CloseDate = &date
	}
	if reveal {
		poll.Solution = bytes.Clone(row.Solution)
	}
	poll.Answers = make([]PollAnswer, 0, len(options))
	for _, option := range options {
		poll.Answers = append(poll.Answers, PollAnswer{
			Option:     bytes.Clone(option.Option),
			Text:       bytes.Clone(option.Text),
			Correct:    reveal && option.Correct,
			VoterCount: option.VoterCount,
			Chosen:     option.Chosen,
		})
	}
	return poll, nil
}

func randomPollID() (int64, error) {
	limit := big.NewInt(math.MaxInt64)
	id, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return 0, fmt.Errorf("crypto random: %w", err)
	}
	return id.Int64() + 1, nil
}
