package store

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/teagramhq/teagram-server/internal/store/db"
)

const srpChallengeIDAttempts = 100
const srpChallengeSweepBatch = 1000

// SRPChallenge is the server-side state needed to verify one SRP proof.
type SRPChallenge struct {
	UserID  int64
	BSecret []byte
	BPublic []byte
}

// IssueSRPChallenge stores one challenge for the auth key and user. A fresh
// challenge replaces the prior one for that pair; challenges on other auth
// keys remain independent. The secret is sealed before it is written.
func (s *Store) IssueSRPChallenge(ctx context.Context, authKeyID int64, challenge SRPChallenge, ttl time.Duration) (int64, error) {
	if ttl <= 0 {
		return 0, errors.New("issue SRP challenge: ttl must be positive")
	}
	sealedSecret, err := s.cipher.Seal(challenge.BSecret)
	if err != nil {
		return 0, fmt.Errorf("issue SRP challenge: seal secret: %w", err)
	}
	for range srpChallengeIDAttempts {
		id, err := newSRPChallengeID()
		if err != nil {
			return 0, err
		}
		err = s.q.UpsertSRPChallenge(ctx, db.UpsertSRPChallengeParams{
			SrpID:     id,
			AuthKeyID: authKeyID,
			UserID:    challenge.UserID,
			BSecret:   sealedSecret,
			BPublic:   challenge.BPublic,
			TtlUs:     ttl.Microseconds(),
		})
		if err == nil {
			return id, nil
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "srp_challenges_pkey" {
			continue
		}
		return 0, fmt.Errorf("issue SRP challenge: %w", err)
	}
	return 0, errors.New("issue SRP challenge: could not allocate srp_id")
}

// ConsumeSRPChallenge atomically deletes and returns a live challenge. Unknown,
// expired, replayed, or wrong-auth-key ids all return ok=false.
func (s *Store) ConsumeSRPChallenge(ctx context.Context, srpID, authKeyID int64) (SRPChallenge, bool, error) {
	row, err := s.q.ConsumeSRPChallenge(ctx, db.ConsumeSRPChallengeParams{
		SrpID: srpID, AuthKeyID: authKeyID,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return SRPChallenge{}, false, nil
	case err != nil:
		return SRPChallenge{}, false, fmt.Errorf("consume SRP challenge: %w", err)
	}
	secret, err := s.cipher.Open(row.BSecret)
	if err != nil {
		return SRPChallenge{}, false, fmt.Errorf("consume SRP challenge: open secret: %w", err)
	}
	return SRPChallenge{UserID: row.UserID, BSecret: secret, BPublic: row.BPublic}, true, nil
}

// SweepExpiredSRPChallenges deletes challenges past their five-minute validity
// window. It runs periodically so idle accounts do not retain stale rows.
func (s *Store) SweepExpiredSRPChallenges(ctx context.Context) (int64, error) {
	var total int64
	for {
		n, err := s.q.SweepExpiredSRPChallenges(ctx, int32(srpChallengeSweepBatch))
		total += n
		if err != nil {
			return total, fmt.Errorf("sweep expired SRP challenges: %w", err)
		}
		if n < srpChallengeSweepBatch {
			return total, nil
		}
	}
}

func newSRPChallengeID() (int64, error) {
	var buf [8]byte
	for range srpChallengeIDAttempts {
		if _, err := rand.Read(buf[:]); err != nil {
			return 0, fmt.Errorf("issue SRP challenge: random id: %w", err)
		}
		id := int64(binary.BigEndian.Uint64(buf[:])) //nolint:gosec // G115: opaque id, sign is irrelevant
		if id != 0 {
			return id, nil
		}
	}
	return 0, errors.New("issue SRP challenge: could not allocate srp_id")
}
