package blobmode

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/teagramhq/teagram-server/internal/blobinventory"
)

const encryptedReportSchema = "teagram.blob-encryption-report/v1"
const encryptedInventorySchema = "teagram.blob-encryption-inventory/v1"

// EncryptedEvidence is the inventory proof accepted when the store moves from
// accepted plaintext to encrypted serving mode.
type EncryptedEvidence struct {
	InventorySHA256 string
	InventoryReport []byte
	ObjectCount     int64
	ByteTotal       int64
	KeyVersion      uint32
	Writes          string
}

// PublishEncrypted appends the durable encrypted authority only after the
// migration inventory has proved every referenced encrypted copy. The report,
// journal entry, and head record use the same private atomic publication
// contract as the rollout runner.
func PublishEncrypted(stateDir, reportRoot string, cfg EffectiveConfig, proof EncryptedEvidence) (retErr error) {
	if os.Geteuid() != 0 {
		return errors.New("encrypted blob-mode publication requires root")
	}
	if stateDir == "" || reportRoot == "" {
		return errors.New("encrypted blob-mode publication requires state and report directories")
	}
	if cfg.BlobS3 == nil {
		return reject("backend", "backend")
	}
	inventoryDigest := sha256.Sum256(proof.InventoryReport)
	if !lowerHexDigest(proof.InventorySHA256) || len(proof.InventoryReport) == 0 ||
		hex.EncodeToString(inventoryDigest[:]) != proof.InventorySHA256 || !json.Valid(proof.InventoryReport) ||
		proof.ObjectCount < 0 || proof.ByteTotal < 0 || proof.KeyVersion == 0 ||
		(proof.Writes != "stopped" && proof.Writes != "drained") {
		return reject("schema", "evidence")
	}
	var inventory struct {
		Schema    string               `json:"schema"`
		Encrypted blobinventory.Report `json:"encrypted"`
	}
	if err := json.Unmarshal(proof.InventoryReport, &inventory); err != nil ||
		inventory.Schema != encryptedInventorySchema ||
		inventory.Encrypted.Classes.ValidEnvelope.Count != proof.ObjectCount ||
		inventory.Encrypted.Classes.ValidEnvelope.LogicalBytes != proof.ByteTotal ||
		inventory.Encrypted.Classes.NoValidEnvelope.Count != 0 || inventory.Encrypted.Classes.Missing.Count != 0 {
		return reject("schema", "inventory")
	}
	if _, err := Read(stateDir, cfg); err != nil {
		return err
	}

	stateFile, journalDir, previous, err := openCurrentRecords(stateDir, cfg)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := journalDir.Close(); closeErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close blob-mode journal: %w", closeErr))
		}
		if closeErr := stateFile.Close(); closeErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close blob-mode directory: %w", closeErr))
		}
	}()
	if previous.Outcome != OutcomeS3Accepted {
		return reject("supersession", "outcome")
	}
	if len(previous.TransitionID) == 0 {
		return reject("schema", "transition_id")
	}

	if _, err := journalDir.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind blob-mode journal: %w", err)
	}
	journalEntries, err := readJournal(journalDir, 0)
	if err != nil {
		return err
	}
	if len(journalEntries) >= maxJournalEntries {
		return reject("oversize", "journal")
	}
	generation := int64(len(journalEntries) + 1)
	transitionID, err := newTransitionID()
	if err != nil {
		return fmt.Errorf("create encrypted transition id: %w", err)
	}
	if len(journalEntries) == 0 {
		return reject("missing", "journal")
	}
	previousRecord, err := parseRecord(journalEntries[len(journalEntries)-1].data)
	if err != nil {
		return err
	}
	volumesBytes, err := json.Marshal(previousRecord.Volumes)
	if err != nil {
		return fmt.Errorf("encode encrypted transition volumes: %w", err)
	}
	volumes := json.RawMessage(volumesBytes)

	reportEvidence := map[string]any{
		"encrypted_inventory_sha256": proof.InventorySHA256,
		"object_count":               proof.ObjectCount,
		"byte_total":                 proof.ByteTotal,
		"key_version":                proof.KeyVersion,
		"writes":                     proof.Writes,
	}
	inventoryName := fmt.Sprintf("teagramd-blob-encryption-inventory-%s.json", transitionID)
	report := map[string]any{
		"schema":           encryptedReportSchema,
		"generation":       generation,
		"transition_id":    transitionID,
		"outcome":          OutcomeEncrypted,
		"backend":          encryptedBackendValue(previousRecord.Backend),
		"volumes":          volumes,
		"evidence":         reportEvidence,
		"inventory_report": inventoryName,
	}
	reportBytes, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("encode encrypted transition report: %w", err)
	}
	reportDigest := sha256.Sum256(reportBytes)
	reportHash := hex.EncodeToString(reportDigest[:])
	reportName := fmt.Sprintf("teagramd-blob-mode-report-%s.json", transitionID)
	if err := publishPrivateReport(reportRoot, inventoryName, proof.InventoryReport); err != nil {
		return err
	}
	if err := publishPrivateReport(reportRoot, reportName, reportBytes); err != nil {
		return err
	}

	fullEvidence := map[string]any{
		"report_sha256":              reportHash,
		"encrypted_inventory_sha256": proof.InventorySHA256,
		"object_count":               proof.ObjectCount,
		"byte_total":                 proof.ByteTotal,
		"key_version":                proof.KeyVersion,
		"writes":                     proof.Writes,
	}
	recordValue := map[string]any{
		"schema":        "teagram.blob-mode/v1",
		"generation":    generation,
		"transition_id": transitionID,
		"supersedes":    previousRecord.TransitionID,
		"outcome":       OutcomeEncrypted,
		"backend":       encryptedBackendValue(previousRecord.Backend),
		"volumes":       volumes,
		"evidence":      fullEvidence,
		"published_at":  time.Now().UTC().Format(time.RFC3339),
	}
	recordBytes, err := json.Marshal(recordValue)
	if err != nil {
		return fmt.Errorf("encode encrypted blob-mode record: %w", err)
	}
	parsed, err := parseRecord(recordBytes)
	if err != nil {
		return err
	}
	if err := validateRecord(parsed, recordBytes, generation, previousRecord.TransitionID, previousRecord.Outcome, previousRecord.Backend, previousRecord.Volumes); err != nil {
		return err
	}
	if err := publishRecord(stateFile, journalDir, recordBytes, transitionID, generation); err != nil {
		return err
	}
	return nil
}

func encryptedBackendValue(value backend) map[string]string {
	return map[string]string{
		"kind":     value.Kind,
		"endpoint": value.Endpoint,
		"bucket":   value.Bucket,
		"prefix":   value.Prefix,
	}
}

func openCurrentRecords(path string, cfg EffectiveConfig) (*os.File, *os.File, record, error) {
	var pathStat unix.Stat_t
	if err := unix.Lstat(path, &pathStat); err != nil {
		return nil, nil, record{}, reject("unavailable", "blob-mode")
	}
	if err := validateStat(&pathStat, unix.S_IFDIR, 0, "blob-mode"); err != nil {
		return nil, nil, record{}, err
	}
	stateDir, err := openDirectory(path, 0, "blob-mode", &pathStat)
	if err != nil {
		return nil, nil, record{}, err
	}
	if err := unix.Flock(int(stateDir.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if closeErr := stateDir.Close(); closeErr != nil {
			return nil, nil, record{}, errors.Join(reject("unavailable", "blob-mode"), fmt.Errorf("close blob-mode directory: %w", closeErr))
		}
		return nil, nil, record{}, reject("unavailable", "blob-mode")
	}
	journalDir, err := openDirectoryAt(int(stateDir.Fd()), "journal", 0, "journal")
	if err != nil {
		if closeErr := stateDir.Close(); closeErr != nil {
			return nil, nil, record{}, errors.Join(err, fmt.Errorf("close blob-mode directory: %w", closeErr))
		}
		return nil, nil, record{}, err
	}
	if err := recoverPublicationTemps(stateDir, journalDir); err != nil {
		if closeErr := journalDir.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close blob-mode journal: %w", closeErr))
		}
		if closeErr := stateDir.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close blob-mode directory: %w", closeErr))
		}
		return nil, nil, record{}, err
	}
	entries, err := readJournal(journalDir, 0)
	if err != nil {
		if closeErr := journalDir.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close blob-mode journal: %w", closeErr))
		}
		if closeErr := stateDir.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close blob-mode directory: %w", closeErr))
		}
		return nil, nil, record{}, err
	}
	if len(entries) == 0 {
		if closeErr := journalDir.Close(); closeErr != nil {
			err = errors.Join(reject("missing", "journal"), fmt.Errorf("close blob-mode journal: %w", closeErr))
		} else {
			err = reject("missing", "journal")
		}
		if closeErr := stateDir.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close blob-mode directory: %w", closeErr))
		}
		return nil, nil, record{}, err
	}
	modeBytes, err := readRecordAt(int(stateDir.Fd()), "mode.json", 0, "mode.json")
	if err != nil {
		if closeErr := journalDir.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close blob-mode journal: %w", closeErr))
		}
		if closeErr := stateDir.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close blob-mode directory: %w", closeErr))
		}
		return nil, nil, record{}, err
	}
	previous, head, err := validateJournalHead(entries, modeBytes, cfg)
	if err != nil {
		if closeErr := journalDir.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close blob-mode journal: %w", closeErr))
		}
		if closeErr := stateDir.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close blob-mode directory: %w", closeErr))
		}
		return nil, nil, record{}, err
	}
	// A complete encrypted tail is an uncommitted candidate. Remove it under
	// the publisher lock before reusing its generation; Read leaves it untouched.
	if head < len(entries)-1 {
		if err := removeUncommittedEncryptedEntry(journalDir, entries[head+1], head+2); err != nil {
			if closeErr := journalDir.Close(); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("close blob-mode journal: %w", closeErr))
			}
			if closeErr := stateDir.Close(); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("close blob-mode directory: %w", closeErr))
			}
			return nil, nil, record{}, err
		}
	}
	return stateDir, journalDir, previous, nil
}

// recoverPublicationTemps removes only private publication staging files while
// holding the state directory lock. Neither temporary-file namespace is part
// of the journal authority; a retry can safely rebuild any interrupted write.
func recoverPublicationTemps(stateDir, journalDir *os.File) error {
	if err := removePublicationTemps(stateDir, ".mode.json.tmp-", "mode.json"); err != nil {
		return err
	}
	return removePublicationTemps(journalDir, ".tmp-", "journal")
}

func removePublicationTemps(dir *os.File, prefix, field string) (retErr error) {
	fd, err := unix.Openat(int(dir.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return reject("unavailable", field)
	}
	entriesDir := os.NewFile(uintptr(fd), field+" temporary directory")
	entries, readErr := entriesDir.ReadDir(-1)
	closeErr := entriesDir.Close()
	if readErr != nil || closeErr != nil {
		return reject("unavailable", field)
	}
	removed := false
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		if !canonicalUUID(strings.TrimPrefix(name, prefix)) {
			return reject("invalid", field)
		}
		var stat unix.Stat_t
		if err := unix.Fstatat(int(dir.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return reject("unavailable", field)
		}
		if err := validateStat(&stat, unix.S_IFREG, 0, field); err != nil {
			return err
		}
		if stat.Size > maxRecordBytes {
			return reject("oversize", field)
		}
		if err := unix.Unlinkat(int(dir.Fd()), name, 0); err != nil {
			return reject("unavailable", field)
		}
		removed = true
	}
	if removed {
		if err := dir.Sync(); err != nil {
			return reject("unavailable", field)
		}
	}
	return nil
}

func removeUncommittedEncryptedEntry(journalDir *os.File, entry journalEntry, generation int) error {
	orphan, err := parseRecord(entry.data)
	if err != nil {
		return err
	}
	journalName := fmt.Sprintf("%010d.json", generation)
	if err := unix.Unlinkat(int(journalDir.Fd()), journalName, 0); err != nil {
		return reject("unavailable", "journal")
	}
	tempName := ".tmp-" + orphan.TransitionID
	if err := unix.Unlinkat(int(journalDir.Fd()), tempName, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return reject("unavailable", "journal")
	}
	if err := journalDir.Sync(); err != nil {
		return reject("unavailable", "journal")
	}
	return nil
}

func publishPrivateReport(rootPath, name string, data []byte) error {
	if rootPath == "" || filepath.Base(rootPath) == "." {
		return reject("invalid", "report-root")
	}
	created := false
	if err := os.Mkdir(rootPath, 0o700); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return reject("unavailable", "report-root")
		}
	} else {
		created = true
	}
	if created {
		parentPath := filepath.Dir(rootPath)
		var parentStat unix.Stat_t
		if err := unix.Lstat(parentPath, &parentStat); err != nil {
			return reject("unavailable", "report-root")
		}
		parent, err := openDirectory(parentPath, 0, "report-root", &parentStat)
		if err != nil {
			return err
		}
		if err := parent.Sync(); err != nil {
			if closeErr := parent.Close(); closeErr != nil {
				return errors.Join(reject("unavailable", "report-root"), fmt.Errorf("close report parent: %w", closeErr))
			}
			return reject("unavailable", "report-root")
		}
		if err := parent.Close(); err != nil {
			return reject("unavailable", "report-root")
		}
	}
	var before unix.Stat_t
	if err := unix.Lstat(rootPath, &before); err != nil {
		return reject("unavailable", "report-root")
	}
	if err := validateStat(&before, unix.S_IFDIR, 0, "report-root"); err != nil {
		return err
	}
	dir, err := openDirectory(rootPath, 0, "report-root", &before)
	if err != nil {
		return err
	}
	if err := writeSyncedAt(dir, name, data, 0o600); err != nil {
		if closeErr := dir.Close(); closeErr != nil {
			return errors.Join(err, fmt.Errorf("close encrypted report directory: %w", closeErr))
		}
		return err
	}
	if err := dir.Sync(); err != nil {
		if closeErr := dir.Close(); closeErr != nil {
			return errors.Join(reject("unavailable", "report-root"), fmt.Errorf("close encrypted report directory: %w", closeErr))
		}
		return reject("unavailable", "report-root")
	}
	if err := dir.Close(); err != nil {
		return reject("unavailable", "report-root")
	}
	return nil
}

func publishRecord(stateDir, journalDir *os.File, recordBytes []byte, transitionID string, generation int64) error {
	journalName := fmt.Sprintf("%010d.json", generation)
	journalTemp := ".tmp-" + transitionID
	if err := writeSyncedAt(journalDir, journalTemp, recordBytes, 0o644); err != nil {
		return err
	}
	if err := unix.Linkat(int(journalDir.Fd()), journalTemp, int(journalDir.Fd()), journalName, 0); err != nil {
		unlinkErr := unix.Unlinkat(int(journalDir.Fd()), journalTemp, 0)
		if unlinkErr != nil && !errors.Is(unlinkErr, unix.ENOENT) {
			return errors.Join(reject("unavailable", "journal"), fmt.Errorf("remove incomplete journal publication: %w", unlinkErr))
		}
		return reject("unavailable", "journal")
	}
	if err := journalDir.Sync(); err != nil {
		return reject("unavailable", "journal")
	}
	if err := unix.Unlinkat(int(journalDir.Fd()), journalTemp, 0); err != nil {
		return reject("unavailable", "journal")
	}
	if err := journalDir.Sync(); err != nil {
		return reject("unavailable", "journal")
	}
	modeTemp := ".mode.json.tmp-" + transitionID
	if err := writeSyncedAt(stateDir, modeTemp, recordBytes, 0o644); err != nil {
		return err
	}
	if err := unix.Renameat(int(stateDir.Fd()), modeTemp, int(stateDir.Fd()), "mode.json"); err != nil {
		return reject("unavailable", "mode.json")
	}
	if err := stateDir.Sync(); err != nil {
		return reject("unavailable", "mode.json")
	}
	return nil
}

func writeSyncedAt(dir *os.File, name string, data []byte, mode uint32) error {
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, mode)
	if err != nil {
		return reject("unavailable", "publication")
	}
	file := os.NewFile(uintptr(fd), name)
	cleanup := func(primary error) error {
		if closeErr := file.Close(); closeErr != nil {
			primary = errors.Join(primary, fmt.Errorf("close publication file: %w", closeErr))
		}
		if unlinkErr := unix.Unlinkat(int(dir.Fd()), name, 0); unlinkErr != nil && !errors.Is(unlinkErr, unix.ENOENT) {
			primary = errors.Join(primary, fmt.Errorf("remove incomplete publication file: %w", unlinkErr))
		}
		return primary
	}
	if err := unix.Fchown(fd, 0, 0); err != nil {
		return cleanup(reject("unavailable", "publication"))
	}
	if err := unix.Fchmod(fd, mode); err != nil {
		return cleanup(reject("unavailable", "publication"))
	}
	written, err := file.Write(data)
	if err != nil {
		return cleanup(reject("unavailable", "publication"))
	}
	if written != len(data) {
		return cleanup(io.ErrShortWrite)
	}
	if err := file.Sync(); err != nil {
		return cleanup(reject("unavailable", "publication"))
	}
	if err := file.Close(); err != nil {
		if unlinkErr := unix.Unlinkat(int(dir.Fd()), name, 0); unlinkErr != nil && !errors.Is(unlinkErr, unix.ENOENT) {
			return errors.Join(reject("unavailable", "publication"), fmt.Errorf("remove incomplete publication file: %w", unlinkErr))
		}
		return reject("unavailable", "publication")
	}
	return nil
}

func newTransitionID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	id[6] = id[6]&0x0f | 0x40
	id[8] = id[8]&0x3f | 0x80
	encoded := hex.EncodeToString(id[:])
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
}
