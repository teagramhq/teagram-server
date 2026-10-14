package store

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/store/db"
)

type MessageDeleteReceiptLookupOutcome string

const (
	MessageDeleteReceiptLookupMissing                   MessageDeleteReceiptLookupOutcome = "Missing"
	MessageDeleteReceiptLookupRequestMismatch           MessageDeleteReceiptLookupOutcome = "ReceiptRequestMismatch"
	MessageDeleteReceiptLookupFoundLocalCommittedResult MessageDeleteReceiptLookupOutcome = "FoundLocalCommittedResult"
)

// MessageDeleteReceiptLookupIdentity is the alpha-local transport identity
// scoped to the authenticated owner. AuthKeyID preserves all eight wire bytes.
type MessageDeleteReceiptLookupIdentity struct {
	OwnerID   int64
	AuthKeyID [8]byte
	SessionID int64
	MsgID     int64
}

// MessageDeleteReceiptLookupRequest contains only the fields that decide if a
// saved result belongs to this retry. TargetIDs is read but never modified.
type MessageDeleteReceiptLookupRequest struct {
	Method    string
	IDSpace   string
	Revoke    bool
	TargetIDs []int32
}

// MessageDeleteReceiptLookup carries no persistence row on any outcome.
// Result is non-nil only when Outcome is FoundLocalCommittedResult.
type MessageDeleteReceiptLookup struct {
	Outcome MessageDeleteReceiptLookupOutcome
	Result  *MessageDeleteLocalCommittedResult
}

// MessageDeleteLocalCommittedResult is the caller's saved local result. It is
// not a provider proof or a durable acknowledgement.
type MessageDeleteLocalCommittedResult struct {
	Pts              int32
	PtsCount         int32
	ReceiptKey       [16]byte
	State            string
	CaptureReference *MessageDeleteReceiptCaptureReference
}

// MessageDeleteReceiptCaptureReference contains only the opaque local capture
// reference carried by a receipt.
type MessageDeleteReceiptCaptureReference struct {
	OperationKey [16]byte
	Epoch        int64
	StreamID     [16]byte
	Position     int64
}

// LookupMessageDeleteReceipt reads an owner-scoped receipt without executing
// deletion, resolving targets, acquiring locks, or changing receipt state.
func (s *Store) LookupMessageDeleteReceipt(
	ctx context.Context,
	identity MessageDeleteReceiptLookupIdentity,
	request MessageDeleteReceiptLookupRequest,
) (MessageDeleteReceiptLookup, error) {
	return lookupMessageDeleteReceipt(ctx, s.q, identity, request)
}

func lookupMessageDeleteReceipt(
	ctx context.Context,
	q *db.Queries,
	identity MessageDeleteReceiptLookupIdentity,
	request MessageDeleteReceiptLookupRequest,
) (MessageDeleteReceiptLookup, error) {
	receipt, err := q.MessageDeleteReceiptForLookup(ctx, db.MessageDeleteReceiptForLookupParams{
		OwnerID: identity.OwnerID, AuthKeyID: identity.AuthKeyID[:],
		SessionID: identity.SessionID, MsgID: identity.MsgID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return MessageDeleteReceiptLookup{Outcome: MessageDeleteReceiptLookupMissing}, nil
	}
	if err != nil {
		return MessageDeleteReceiptLookup{}, fmt.Errorf("lookup message delete receipt: %w", err)
	}

	targets, validTargets := normalizeMessageDeleteReceiptTargets(request.TargetIDs)
	if receipt.Method != request.Method || receipt.IDSpace != request.IDSpace ||
		receipt.Revoke != request.Revoke || !validTargets || !slices.Equal(receipt.TargetIds, targets) {
		return MessageDeleteReceiptLookup{Outcome: MessageDeleteReceiptLookupRequestMismatch}, nil
	}

	result := &MessageDeleteLocalCommittedResult{
		Pts: receipt.Pts, PtsCount: receipt.PtsCount, State: receipt.State,
	}
	copy(result.ReceiptKey[:], receipt.ReceiptKey)
	switch receipt.State {
	case "pending", "confirmed":
		if receipt.Epoch == nil || receipt.Position == nil || len(receipt.OperationKey) != len(result.ReceiptKey) || len(receipt.StreamID) != len(result.ReceiptKey) {
			return MessageDeleteReceiptLookup{}, errors.New("message delete receipt has an invalid capture reference")
		}
		reference := &MessageDeleteReceiptCaptureReference{
			Epoch: *receipt.Epoch, Position: *receipt.Position,
		}
		copy(reference.OperationKey[:], receipt.OperationKey)
		copy(reference.StreamID[:], receipt.StreamID)
		result.CaptureReference = reference
	case "local_no_change":
	default:
		return MessageDeleteReceiptLookup{}, fmt.Errorf("message delete receipt has unsupported state %q", receipt.State)
	}
	return MessageDeleteReceiptLookup{
		Outcome: MessageDeleteReceiptLookupFoundLocalCommittedResult,
		Result:  result,
	}, nil
}

func normalizeMessageDeleteReceiptTargets(raw []int32) ([]int32, bool) {
	if len(raw) == 0 {
		return []int32{}, true
	}
	unique := make([]int32, 0, min(len(raw), 100))
	seen := make(map[int32]struct{}, min(len(raw), 100))
	for _, id := range raw {
		if id <= 0 {
			return nil, false
		}
		if _, ok := seen[id]; ok {
			continue
		}
		if len(unique) == 100 {
			return nil, false
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	slices.Sort(unique)
	return unique, true
}
