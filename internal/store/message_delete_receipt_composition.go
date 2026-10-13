package store

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5/pgconn"
)

const messageDeleteReceiptOwnerCapacity int64 = 65_536

var (
	ErrMessageDeleteReceiptCapacityExceeded = errors.New("message delete receipt capacity exceeded")
	ErrMessageDeleteReceiptTooManyTargets   = errors.New("message delete request exceeds the fresh-admission target limit")
	errMessageDeleteReceiptResolved         = errors.New("message delete receipt resolved after owner lock")
)

type messageDeleteReceiptAttempt struct {
	identity          MessageDeleteReceiptLookupIdentity
	request           MessageDeleteReceiptLookupRequest
	normalizedTargets []int32
	receiptKey        [16]byte
	lookup            MessageDeleteReceiptLookup
}

func (a *messageDeleteReceiptAttempt) committedResult(
	pts int,
	rawCount int,
	state string,
	captureReference *MessageDeleteReceiptCaptureReference,
) (*MessageDeleteLocalCommittedResult, error) {
	if pts < 0 || pts > math.MaxInt32 {
		return nil, fmt.Errorf("message delete pts %d does not fit int32", pts)
	}
	if rawCount < 0 || rawCount > math.MaxInt32 {
		return nil, fmt.Errorf("message delete raw count %d does not fit int32", rawCount)
	}
	result := &MessageDeleteLocalCommittedResult{
		Pts:        int32(pts),      // #nosec G115 -- checked against MaxInt32 above.
		PtsCount:   int32(rawCount), // #nosec G115 -- checked against MaxInt32 above.
		ReceiptKey: a.receiptKey,
		State:      state, CaptureReference: captureReference,
	}
	return result, nil
}

// composeMessageDelete is an inert request/admission/result composition. Its
// only entry point is the disposable test seam; live DeleteMessages continues
// to call the nil-hook path directly.
func (s *Store) composeMessageDelete(
	ctx context.Context,
	identity MessageDeleteReceiptLookupIdentity,
	request MessageDeleteReceiptLookupRequest,
	epoch int64,
	stream [16]byte,
	capture messageDeleteCaptureHook,
) (map[int64]int, MessageDeleteReceiptLookup, error) {
	lookup, err := s.LookupMessageDeleteReceipt(ctx, identity, request)
	if err != nil {
		return nil, MessageDeleteReceiptLookup{}, err
	}
	if lookup.Outcome != MessageDeleteReceiptLookupMissing {
		return nil, lookup, nil
	}
	if len(request.TargetIDs) > 100 {
		return nil, lookup, ErrMessageDeleteReceiptTooManyTargets
	}
	normalized, valid := normalizeMessageDeleteReceiptTargets(request.TargetIDs)
	if !valid || request.Method != "messages.deleteMessages" || request.IDSpace != "owner_message" {
		return nil, lookup, errors.New("invalid fresh message delete receipt request")
	}
	if capture == nil {
		return nil, lookup, errors.New("message delete composition requires a disposable capture hook")
	}

	attempt := &messageDeleteReceiptAttempt{
		identity: identity, request: request, normalizedTargets: normalized,
	}
	for attempt.receiptKey == ([16]byte{}) {
		if _, err := rand.Read(attempt.receiptKey[:]); err != nil {
			return nil, lookup, fmt.Errorf("draw message delete receipt key: %w", err)
		}
	}
	localIDs := make([]int64, len(request.TargetIDs))
	for i, id := range request.TargetIDs {
		localIDs[i] = int64(id)
	}
	perOwner, err := s.deleteMessagesWithReceipt(ctx, identity.OwnerID, localIDs, request.Revoke, capture, attempt)
	if errors.Is(err, errMessageDeleteReceiptResolved) {
		return nil, attempt.lookup, nil
	}
	if errors.Is(err, ErrMessageDeleteReceiptCapacityExceeded) {
		return nil, MessageDeleteReceiptLookup{}, err
	}
	if err != nil {
		if errors.Is(err, ErrMessageInvalid) || messageDeleteReceiptIdentityConflict(err) {
			afterRollback, lookupErr := s.LookupMessageDeleteReceipt(ctx, identity, request)
			if lookupErr != nil {
				return nil, MessageDeleteReceiptLookup{}, lookupErr
			}
			if afterRollback.Outcome != MessageDeleteReceiptLookupMissing {
				return nil, afterRollback, nil
			}
		}
		return nil, MessageDeleteReceiptLookup{}, err
	}
	return perOwner, attempt.lookup, nil
}

func messageDeleteReceiptIdentityConflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "message_delete_receipts_pkey"
}
