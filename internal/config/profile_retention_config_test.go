package config_test

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/mtproto"
)

// maxDurationRaw is the largest duration ParseDuration accepts, as the raw text
// an operator would type. A receipt bound and a part TTL both set to it are the
// case where a plain `ttl + deadline` sum overflows int64 nanoseconds.
const maxDurationRaw = "9223372036854775807ns"

func TestProfileIdentifierRetentionFloorIsTheRestoreCeilingPlusCleanupGrace(t *testing.T) {
	if config.ProfileRestoreCeiling != 90*24*time.Hour {
		t.Errorf("ProfileRestoreCeiling = %v, want the accepted 90-day backup age ceiling", config.ProfileRestoreCeiling)
	}
	if config.ProfileBackupCleanupGrace <= 0 {
		t.Errorf("ProfileBackupCleanupGrace = %v, want a positive delayed-cleanup allowance", config.ProfileBackupCleanupGrace)
	}
	if want := config.ProfileRestoreCeiling + config.ProfileBackupCleanupGrace; config.ProfileIdentifierRetentionFloor != want {
		t.Errorf("ProfileIdentifierRetentionFloor = %v, want %v", config.ProfileIdentifierRetentionFloor, want)
	}
	if config.ProfileIdentifierRetentionFloor > time.Duration(math.MaxInt64)-45*time.Second {
		t.Errorf("ProfileIdentifierRetentionFloor = %v, want room for the largest request deadline on top of it", config.ProfileIdentifierRetentionFloor)
	}
}

// TestLoadProfileRetentionDefaults is the "missing configuration uses safe
// documented defaults" case: an operator who sets nothing gets a horizon that
// already covers every resurrectable restore point, and the receipt default
// moves with the upload variables it has to outlive.
func TestLoadProfileRetentionDefaults(t *testing.T) {
	t.Setenv("TG_POSTGRES_DSN", "postgres://localhost/tg")
	t.Setenv("TG_AUTHKEY_ENC_KEY", validEncKey)

	cfg, err := config.Load(discardLog())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ProfileGalleryRetention != config.ProfileIdentifierRetentionFloor {
		t.Errorf("ProfileGalleryRetention = %v, want the restore-horizon floor %v",
			cfg.ProfileGalleryRetention, config.ProfileIdentifierRetentionFloor)
	}
	if cfg.ProfileDeleteOperationRetention != config.ProfileIdentifierRetentionFloor {
		t.Errorf("ProfileDeleteOperationRetention = %v, want the restore-horizon floor %v",
			cfg.ProfileDeleteOperationRetention, config.ProfileIdentifierRetentionFloor)
	}
	receiptMin := cfg.UploadPartTTL + cfg.RPCDeadline
	if cfg.ProfileReceiptRetention < config.ProfileIdentifierRetentionFloor {
		t.Errorf("ProfileReceiptRetention = %v, want at least %v", cfg.ProfileReceiptRetention, config.ProfileIdentifierRetentionFloor)
	}
	if cfg.ProfileReceiptRetention < receiptMin {
		t.Errorf("ProfileReceiptRetention = %v, want at least UploadPartTTL+RPCDeadline = %v", cfg.ProfileReceiptRetention, receiptMin)
	}
	if cfg.ProfileReceiptRetention > cfg.ProfileGalleryRetention+receiptMin {
		t.Errorf("ProfileReceiptRetention = %v, want the smallest bound that covers both horizons (<= %v)",
			cfg.ProfileReceiptRetention, cfg.ProfileGalleryRetention+receiptMin)
	}
}

// TestLoadProfileReceiptRetentionDefaultFollowsPartTTL covers the case the
// receipt floor exists for: a client that keeps one upload alive for a part TTL
// longer than the restore horizon must not be able to buy a receipt that
// expires while its retry is still legal.
func TestLoadProfileReceiptRetentionDefaultFollowsPartTTL(t *testing.T) {
	t.Setenv("TG_POSTGRES_DSN", "postgres://localhost/tg")
	t.Setenv("TG_AUTHKEY_ENC_KEY", validEncKey)
	ttl := 2400 * time.Hour
	if ttl <= config.ProfileIdentifierRetentionFloor {
		t.Fatalf("test part TTL %v must exceed the restore-horizon floor", ttl)
	}
	t.Setenv("TG_UPLOAD_PART_TTL", ttl.String())

	cfg, err := config.Load(discardLog())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := ttl + cfg.RPCDeadline; cfg.ProfileReceiptRetention != want {
		t.Errorf("ProfileReceiptRetention = %v, want the derived default %v", cfg.ProfileReceiptRetention, want)
	}
}

// TestLoadProfileGalleryRetention pins the identifier-only gallery ledger
// horizon: at the floor is accepted, one nanosecond under it is not, and a
// malformed value fails by name rather than reading as the default.
func TestLoadProfileGalleryRetention(t *testing.T) {
	t.Setenv("TG_POSTGRES_DSN", "postgres://localhost/tg")
	t.Setenv("TG_AUTHKEY_ENC_KEY", validEncKey)

	floor := config.ProfileIdentifierRetentionFloor
	tests := []struct {
		name    string
		raw     string
		want    time.Duration
		wantErr bool
	}{
		{name: "unset uses the restore horizon", raw: "", want: floor},
		{name: "at floor", raw: floor.String(), want: floor},
		{name: "one unit above floor", raw: (floor + time.Nanosecond).String(), want: floor + time.Nanosecond},
		{name: "one unit below floor", raw: (floor - time.Nanosecond).String(), wantErr: true},
		{name: "restore ceiling alone is not enough", raw: (90 * 24 * time.Hour).String(), wantErr: true},
		{name: "a month", raw: "720h", wantErr: true},
		{name: "a year", raw: "8760h", want: 8760 * time.Hour},
		{name: "zero", raw: "0s", wantErr: true},
		{name: "negative", raw: "-1h", wantErr: true},
		{name: "not a duration", raw: "90d", wantErr: true},
		{name: "maximum duration", raw: maxDurationRaw, want: time.Duration(math.MaxInt64)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TG_PROFILE_GALLERY_RETENTION", tc.raw)
			cfg, err := config.Load(discardLog())
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Load succeeded with TG_PROFILE_GALLERY_RETENTION=%s, got %v", tc.raw, cfg.ProfileGalleryRetention)
				}
				if !strings.Contains(err.Error(), "TG_PROFILE_GALLERY_RETENTION") {
					t.Errorf("error %q does not name TG_PROFILE_GALLERY_RETENTION", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load with TG_PROFILE_GALLERY_RETENTION=%s: %v", tc.raw, err)
			}
			if cfg.ProfileGalleryRetention != tc.want {
				t.Errorf("ProfileGalleryRetention = %v, want %v", cfg.ProfileGalleryRetention, tc.want)
			}
		})
	}
}

// TestLoadProfileReceiptRetention pins the terminal upload receipt horizon: it
// has to cover the gallery ledger horizon and the longest retry one upload can
// still make, which is the part TTL plus the deadline that request ran under.
func TestLoadProfileReceiptRetention(t *testing.T) {
	t.Setenv("TG_POSTGRES_DSN", "postgres://localhost/tg")
	t.Setenv("TG_AUTHKEY_ENC_KEY", validEncKey)

	floor := config.ProfileIdentifierRetentionFloor
	deadline := mtproto.DefaultRPCDeadline
	ttl := 2400 * time.Hour
	tests := []struct {
		name    string
		ttl     string
		gallery string
		raw     string
		want    time.Duration
		wantErr string
	}{
		{name: "at floor", raw: floor.String(), want: floor},
		{name: "one unit above floor", raw: (floor + time.Nanosecond).String(), want: floor + time.Nanosecond},
		{name: "one unit below floor", raw: (floor - time.Nanosecond).String(), wantErr: "TG_PROFILE_RECEIPT_RETENTION"},
		{name: "restore ceiling alone is not enough", raw: (90 * 24 * time.Hour).String(), wantErr: "TG_PROFILE_RECEIPT_RETENTION"},
		{name: "zero", raw: "0s", wantErr: "TG_PROFILE_RECEIPT_RETENTION"},
		{name: "negative", raw: "-1h", wantErr: "TG_PROFILE_RECEIPT_RETENTION"},
		{name: "not a duration", raw: "soon", wantErr: "TG_PROFILE_RECEIPT_RETENTION"},
		{
			name:    "covers part TTL plus the request deadline exactly",
			ttl:     ttl.String(),
			raw:     (ttl + deadline).String(),
			want:    ttl + deadline,
			wantErr: "",
		},
		{
			name:    "one unit short of part TTL plus deadline",
			ttl:     ttl.String(),
			raw:     (ttl + deadline - time.Nanosecond).String(),
			wantErr: "TG_PROFILE_RECEIPT_RETENTION",
		},
		{
			name:    "receipt shorter than the part TTL fails by the TTL's name",
			ttl:     ttl.String(),
			raw:     floor.String(),
			wantErr: "TG_UPLOAD_PART_TTL",
		},
		{
			name:    "below the configured gallery horizon",
			gallery: "3000h",
			raw:     "2400h",
			wantErr: "TG_PROFILE_RECEIPT_RETENTION",
		}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TG_UPLOAD_PART_TTL", tc.ttl)
			t.Setenv("TG_PROFILE_GALLERY_RETENTION", tc.gallery)
			t.Setenv("TG_PROFILE_RECEIPT_RETENTION", tc.raw)
			cfg, err := config.Load(discardLog())
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("Load succeeded with TG_PROFILE_RECEIPT_RETENTION=%s, got %v", tc.raw, cfg.ProfileReceiptRetention)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %q does not name %s", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load with TG_PROFILE_RECEIPT_RETENTION=%s: %v", tc.raw, err)
			}
			if cfg.ProfileReceiptRetention != tc.want {
				t.Errorf("ProfileReceiptRetention = %v, want %v", cfg.ProfileReceiptRetention, tc.want)
			}
		})
	}
}

// TestLoadProfilePartTTLBeyondReceiptRetention is the startup gate the accepted
// threat model names: a part TTL a receipt cannot outlive is refused, and
// the error names TG_UPLOAD_PART_TTL because that is the variable the operator
// has to lower.
func TestLoadProfilePartTTLBeyondReceiptRetention(t *testing.T) {
	t.Setenv("TG_POSTGRES_DSN", "postgres://localhost/tg")
	t.Setenv("TG_AUTHKEY_ENC_KEY", validEncKey)

	floor := config.ProfileIdentifierRetentionFloor
	tests := []struct {
		name     string
		ttl      string
		receipt  string
		wantName string
	}{
		{name: "TTL past receipt retention", ttl: (floor + time.Hour).String(), receipt: floor.String(), wantName: "TG_UPLOAD_PART_TTL"},
		{name: "TTL one unit past receipt retention", ttl: (floor + time.Nanosecond).String(), receipt: floor.String(), wantName: "TG_UPLOAD_PART_TTL"},
		{name: "default TTL with a receipt below the horizon", ttl: "", receipt: (floor - time.Nanosecond).String(), wantName: "TG_PROFILE_RECEIPT_RETENTION"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TG_UPLOAD_PART_TTL", tc.ttl)
			t.Setenv("TG_PROFILE_RECEIPT_RETENTION", tc.receipt)
			_, err := config.Load(discardLog())
			if err == nil {
				t.Fatalf("Load succeeded with TG_UPLOAD_PART_TTL=%s and TG_PROFILE_RECEIPT_RETENTION=%s", tc.ttl, tc.receipt)
			}
			if !strings.Contains(err.Error(), tc.wantName) {
				t.Errorf("error %q does not name %s", err, tc.wantName)
			}
		})
	}
}

// TestLoadProfileDeleteOperationRetention bounds the local deletion-operation
// dedup receipt. The bound is the restore horizon: a dedup row compacted
// before the last restore point that can revive it dies with the retry it
// exists to dedup.
func TestLoadProfileDeleteOperationRetention(t *testing.T) {
	t.Setenv("TG_POSTGRES_DSN", "postgres://localhost/tg")
	t.Setenv("TG_AUTHKEY_ENC_KEY", validEncKey)

	floor := config.ProfileIdentifierRetentionFloor
	tests := []struct {
		name    string
		raw     string
		want    time.Duration
		wantErr bool
	}{
		{name: "unset uses the restore horizon", raw: "", want: floor},
		{name: "at floor", raw: floor.String(), want: floor},
		{name: "one unit below floor", raw: (floor - time.Nanosecond).String(), wantErr: true},
		{name: "an hour", raw: "1h", wantErr: true},
		{name: "two years", raw: "17520h", want: 17520 * time.Hour},
		{name: "zero", raw: "0s", wantErr: true},
		{name: "negative", raw: "-1h", wantErr: true},
		{name: "not a duration", raw: "90d", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TG_PROFILE_DELETE_OP_RETENTION", tc.raw)
			cfg, err := config.Load(discardLog())
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Load succeeded with TG_PROFILE_DELETE_OP_RETENTION=%s, got %v", tc.raw, cfg.ProfileDeleteOperationRetention)
				}
				if !strings.Contains(err.Error(), "TG_PROFILE_DELETE_OP_RETENTION") {
					t.Errorf("error %q does not name TG_PROFILE_DELETE_OP_RETENTION", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load with TG_PROFILE_DELETE_OP_RETENTION=%s: %v", tc.raw, err)
			}
			if cfg.ProfileDeleteOperationRetention != tc.want {
				t.Errorf("ProfileDeleteOperationRetention = %v, want %v", cfg.ProfileDeleteOperationRetention, tc.want)
			}
		})
	}
}

// TestLoadProfileRetentionOverflowFailsSafe is the boundary case a naive
// sum gets wrong: the receipt bound has to cover part TTL plus the request
// deadline, and a TTL near the duration ceiling makes that sum overflow to a
// negative and look satisfied. Every case here must fail, and fail by name.
func TestLoadProfileRetentionOverflowFailsSafe(t *testing.T) {
	t.Setenv("TG_POSTGRES_DSN", "postgres://localhost/tg")
	t.Setenv("TG_AUTHKEY_ENC_KEY", validEncKey)

	maxDur := maxDurationRaw
	tests := []struct {
		name     string
		ttl      string
		gallery  string
		receipt  string
		wantName string
	}{
		{name: "max TTL against a max receipt", ttl: maxDur, receipt: maxDur, wantName: "TG_UPLOAD_PART_TTL"},
		{name: "max TTL against the floor receipt", ttl: maxDur, receipt: config.ProfileIdentifierRetentionFloor.String(), wantName: "TG_UPLOAD_PART_TTL"},
		{name: "max gallery with a floor receipt", gallery: maxDur, receipt: config.ProfileIdentifierRetentionFloor.String(), wantName: "TG_PROFILE_RECEIPT_RETENTION"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TG_UPLOAD_PART_TTL", tc.ttl)
			t.Setenv("TG_PROFILE_GALLERY_RETENTION", tc.gallery)
			t.Setenv("TG_PROFILE_RECEIPT_RETENTION", tc.receipt)
			_, err := config.Load(discardLog())
			if err == nil {
				t.Fatal("Load succeeded with an unrepresentable retention bound")
			}
			if !strings.Contains(err.Error(), tc.wantName) {
				t.Errorf("error %q does not name %s", err, tc.wantName)
			}
		})
	}
}

// TestLoadProfileRetentionLeavesExistingDeploymentAlone is the no-regression
// case: a fully configured non-photo deployment that sets none of the profile
// variables keeps loading, at the documented defaults.
func TestLoadProfileRetentionLeavesExistingDeploymentAlone(t *testing.T) {
	t.Setenv("TG_POSTGRES_DSN", "postgres://localhost/tg")
	t.Setenv("TG_AUTHKEY_ENC_KEY", validEncKey)
	t.Setenv("TG_LISTEN_ADDR", ":2443")
	t.Setenv("TG_BLOB_DIR", "blobs")
	t.Setenv("TG_MAX_FILE_BYTES", "2097152")
	t.Setenv("TG_UPLOAD_PART_TTL", "2h")
	t.Setenv("TG_MEDIA_ERASURE_MIN_AGE", "48h")
	t.Setenv("TG_MEDIA_ERASURE_INTERVAL_MIN", "90m")
	t.Setenv("TG_MEDIA_ERASURE_INTERVAL_MAX", "240m")
	t.Setenv("TG_MEDIA_ERASURE_DESTRUCTIVE", "true")
	t.Setenv("TG_BLOB_SCAN_TEMP_MIN_AGE", "48h")
	t.Setenv("TG_RPC_DEADLINE", "30s")
	t.Setenv("TG_STATEMENT_TIMEOUT", "17s")
	t.Setenv("TG_REPLICA_COUNT", "2")
	t.Setenv("TG_REGISTRATION", "closed")
	t.Setenv("TG_RATE_LIMIT_MESSAGE_SEND", "60")
	t.Setenv("TG_RATE_LIMIT_MESSAGE_SEND_WINDOW", "60s")

	cfg, err := config.Load(discardLog())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ProfileGalleryRetention < config.ProfileIdentifierRetentionFloor {
		t.Errorf("ProfileGalleryRetention = %v, want at least %v", cfg.ProfileGalleryRetention, config.ProfileIdentifierRetentionFloor)
	}
	if cfg.ProfileReceiptRetention < cfg.UploadPartTTL+cfg.RPCDeadline {
		t.Errorf("ProfileReceiptRetention = %v, want at least %v", cfg.ProfileReceiptRetention, cfg.UploadPartTTL+cfg.RPCDeadline)
	}
	if cfg.ProfileDeleteOperationRetention < config.ProfileIdentifierRetentionFloor {
		t.Errorf("ProfileDeleteOperationRetention = %v, want at least %v", cfg.ProfileDeleteOperationRetention, config.ProfileIdentifierRetentionFloor)
	}
}
