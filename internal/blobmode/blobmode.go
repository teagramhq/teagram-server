// Package blobmode validates the durable authority record for the blob backend
// used by the serving process.
package blobmode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"

	"github.com/teagramhq/teagram-server/internal/blob"
)

const (
	maxRecordBytes             = 4096
	maxJournalEntries          = 1000
	maxJournalDirectoryEntries = maxJournalEntries*2 + 2
	stackMoveOutcome           = "stack-moved-local"
	OutcomeS3Accepted          = "s3-accepted"
	OutcomeEncrypted           = "encrypted"
	DefaultDirectory           = "/run/teagramd/blob-mode"
	stackMoveSourceTGBlobs     = "telegram-server_tgblobs"
	stackMoveTargetTGBlobs     = "teagram_tgblobs"
	stackMoveSourceRustFSData  = "telegram-server_rustfsdata"
	stackMoveTargetRustFSData  = "teagram_rustfsdata"
)

// EffectiveConfig contains only the settings that identify a blob backend.
// Credentials and S3 region are intentionally outside the durable binding.
type EffectiveConfig struct {
	BlobDir string
	BlobS3  *blob.S3Config
}

// Authority is the validated current blob-mode record. Present is false when
// the authority directory is absent, or when an empty directory accompanies
// an S3 backend and therefore contains no authority to validate.
type Authority struct {
	Present bool
	Outcome string
}

type validationError struct {
	reason string
	field  string
}

func (e *validationError) Error() string {
	return "blob-mode validation failed: reason=" + e.reason + " field=" + e.field
}

func reject(reason, field string) error {
	return &validationError{reason: reason, field: field}
}

// Read validates the fixed record directory and binds its current head to the
// effective serving backend. An absent directory returns an absent authority;
// for S3, a completely empty directory has the same meaning. Nonempty state is
// always validated and cannot be bypassed by the fresh-S3 path.
func Read(path string, cfg EffectiveConfig) (Authority, error) {
	return readAt(path, cfg, 0)
}

// Validate checks the fixed record directory and binds its current head to the
// effective serving backend. An absent directory keeps pre-guard behavior.
func Validate(path string, cfg EffectiveConfig) error {
	_, err := Read(path, cfg)
	return err
}

func readAt(path string, cfg EffectiveConfig, expectedUID uint32) (authority Authority, err error) {
	var pathStat unix.Stat_t
	if err := unix.Lstat(path, &pathStat); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return Authority{}, nil
		}
		return Authority{}, reject("unavailable", "blob-mode")
	}
	if err := validateStat(&pathStat, unix.S_IFDIR, expectedUID, "blob-mode"); err != nil {
		return Authority{}, err
	}

	modeDir, err := openDirectory(path, expectedUID, "blob-mode", &pathStat)
	if err != nil {
		return Authority{}, err
	}
	defer func() {
		if closeErr := modeDir.Close(); closeErr != nil && err == nil {
			err = reject("unavailable", "blob-mode")
		}
	}()
	directoryEntries, err := modeDir.Readdirnames(1)
	if err != nil && !errors.Is(err, io.EOF) {
		return Authority{}, reject("unavailable", "blob-mode")
	}
	if len(directoryEntries) == 0 && errors.Is(err, io.EOF) && cfg.BlobS3 != nil {
		// Docker creates a missing bind source as an empty directory. In a fresh
		// S3 deployment that is the same state as a missing authority directory;
		// once any authority file exists, every existing validation still applies.
		return Authority{}, nil
	}

	modeBytes, err := readRecordAt(int(modeDir.Fd()), "mode.json", expectedUID, "mode.json")
	if err != nil {
		return Authority{}, err
	}

	journalDir, err := openDirectoryAt(int(modeDir.Fd()), "journal", expectedUID, "journal")
	if err != nil {
		return Authority{}, err
	}
	defer func() {
		if closeErr := journalDir.Close(); closeErr != nil && err == nil {
			err = reject("unavailable", "journal")
		}
	}()

	entries, err := readJournal(journalDir, expectedUID)
	if err != nil {
		return Authority{}, err
	}
	if len(entries) == 0 {
		return Authority{}, reject("missing", "journal")
	}
	current, _, err := validateJournalHead(entries, modeBytes, cfg)
	if err != nil {
		return Authority{}, err
	}
	return Authority{Present: true, Outcome: current.Outcome}, nil
}

func validateJournalHead(entries []journalEntry, modeBytes []byte, cfg EffectiveConfig) (record, int, error) {
	if len(entries) == 0 {
		return record{}, 0, reject("missing", "journal")
	}
	head := len(entries) - 1
	if !bytes.Equal(modeBytes, entries[head].data) {
		if head == 0 || !bytes.Equal(modeBytes, entries[head-1].data) {
			return record{}, 0, reject("stale", "mode.json")
		}
		head--
	}

	transitionIDs := make(map[string]struct{}, len(entries))
	reportDigests := make(map[string]struct{}, len(entries))
	var previousID string
	var previousOutcome string
	var previousBackend backend
	var previousVolumes volumes
	var current record
	for index, entry := range entries {
		generation := int64(index + 1)
		entryRecord, err := parseRecord(entry.data)
		if err != nil {
			return record{}, 0, err
		}
		if index > head && (previousOutcome != OutcomeS3Accepted || entryRecord.Outcome != OutcomeEncrypted) {
			return record{}, 0, reject("stale", "mode.json")
		}
		if err := validateRecord(entryRecord, entry.data, generation, previousID, previousOutcome, previousBackend, previousVolumes); err != nil {
			return record{}, 0, err
		}
		if _, exists := transitionIDs[entryRecord.TransitionID]; exists {
			return record{}, 0, reject("reused", "transition_id")
		}
		transitionIDs[entryRecord.TransitionID] = struct{}{}
		if _, exists := reportDigests[entryRecord.Evidence.ReportSHA256]; exists {
			return record{}, 0, reject("reused", "evidence.report_sha256")
		}
		reportDigests[entryRecord.Evidence.ReportSHA256] = struct{}{}
		previousID = entryRecord.TransitionID
		previousOutcome = entryRecord.Outcome
		previousBackend = entryRecord.Backend
		previousVolumes = entryRecord.Volumes
		if index == head {
			current = entryRecord
		}
		if index >= head {
			if err := validateBackend(entryRecord.Backend, cfg); err != nil {
				return record{}, 0, err
			}
		}
	}
	return current, head, nil
}

type record struct {
	Schema       string          `json:"schema"`
	Generation   int64           `json:"generation"`
	TransitionID string          `json:"transition_id"`
	Supersedes   json.RawMessage `json:"supersedes"`
	Outcome      string          `json:"outcome"`
	Backend      backend         `json:"backend"`
	Volumes      volumes         `json:"volumes"`
	Evidence     evidence        `json:"evidence"`
	PublishedAt  string          `json:"published_at"`
}

type backend struct {
	Kind     string `json:"kind"`
	Dir      string `json:"dir"`
	Endpoint string `json:"endpoint"`
	Bucket   string `json:"bucket"`
	Prefix   string `json:"prefix"`
}

type volumes struct {
	TGBlobs    string          `json:"tgblobs"`
	RustFSData json.RawMessage `json:"rustfsdata"`
}

type evidence struct {
	ReportSHA256              string `json:"report_sha256"`
	SourceManifestSHA256      string `json:"source_manifest_sha256"`
	DestinationManifestSHA256 string `json:"destination_manifest_sha256"`
	S3CensusManifestSHA256    string `json:"s3_census_manifest_sha256"`
	RestoredManifestSHA256    string `json:"restored_manifest_sha256"`
	SourceTGBlobsVolume       string `json:"source_tgblobs_volume"`
	TargetTGBlobsVolume       string `json:"target_tgblobs_volume"`
	SourceTGBlobsInventory    string `json:"source_tgblobs_inventory_sha256"`
	TargetTGBlobsInventory    string `json:"target_tgblobs_inventory_sha256"`
	TGBlobsFileCount          *int64 `json:"tgblobs_file_count"`
	SourceRustFSDataVolume    string `json:"source_rustfsdata_volume"`
	TargetRustFSDataVolume    string `json:"target_rustfsdata_volume"`
	SourceRustFSDataInventory string `json:"source_rustfsdata_inventory_sha256"`
	TargetRustFSDataInventory string `json:"target_rustfsdata_inventory_sha256"`
	RustFSDataFileCount       *int64 `json:"rustfsdata_file_count"`
	ObjectCount               *int64 `json:"object_count"`
	ByteTotal                 *int64 `json:"byte_total"`
	CopyPasses                *int64 `json:"copy_passes"`
	RestorePasses             *int64 `json:"restore_passes"`
	RetainedCutoverKeyCount   *int64 `json:"retained_cutover_key_count"`
	EncryptedInventorySHA256  string `json:"encrypted_inventory_sha256"`
	KeyVersion                uint32 `json:"key_version"`
	Writes                    string `json:"writes"`
}

type journalEntry struct {
	data []byte
}

func openDirectory(path string, expectedUID uint32, field string, before *unix.Stat_t) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, reject("invalid", field)
	}
	var opened unix.Stat_t
	if err := unix.Fstat(fd, &opened); err != nil {
		if closeErr := unix.Close(fd); closeErr != nil {
			return nil, reject("unavailable", field)
		}
		return nil, reject("unavailable", field)
	}
	if before != nil && (before.Dev != opened.Dev || before.Ino != opened.Ino) {
		if closeErr := unix.Close(fd); closeErr != nil {
			return nil, reject("unavailable", field)
		}
		return nil, reject("changed", field)
	}
	if err := validateStat(&opened, unix.S_IFDIR, expectedUID, field); err != nil {
		if closeErr := unix.Close(fd); closeErr != nil {
			return nil, reject("unavailable", field)
		}
		return nil, err
	}
	return os.NewFile(uintptr(fd), field), nil
}

func openDirectoryAt(parentFD int, name string, expectedUID uint32, field string) (*os.File, error) {
	fd, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, reject("invalid", field)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		if closeErr := unix.Close(fd); closeErr != nil {
			return nil, reject("unavailable", field)
		}
		return nil, reject("unavailable", field)
	}
	if err := validateStat(&stat, unix.S_IFDIR, expectedUID, field); err != nil {
		if closeErr := unix.Close(fd); closeErr != nil {
			return nil, reject("unavailable", field)
		}
		return nil, err
	}
	return os.NewFile(uintptr(fd), field), nil
}

func validateStat(stat *unix.Stat_t, kind uint32, expectedUID uint32, field string) error {
	if stat.Mode&unix.S_IFMT != kind {
		return reject("invalid", field)
	}
	if stat.Uid != expectedUID {
		return reject("ownership", field)
	}
	if stat.Mode&0o022 != 0 {
		return reject("permissions", field)
	}
	return nil
}

func readRecordAt(dirFD int, name string, expectedUID uint32, field string) ([]byte, error) {
	fd, err := unix.Openat(dirFD, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, reject("invalid", field)
	}
	file := os.NewFile(uintptr(fd), field)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		if closeErr := file.Close(); closeErr != nil {
			return nil, reject("unavailable", field)
		}
		return nil, reject("unavailable", field)
	}
	if err := validateStat(&stat, unix.S_IFREG, expectedUID, field); err != nil {
		if closeErr := file.Close(); closeErr != nil {
			return nil, reject("unavailable", field)
		}
		return nil, err
	}
	if stat.Size > maxRecordBytes {
		if closeErr := file.Close(); closeErr != nil {
			return nil, reject("unavailable", field)
		}
		return nil, reject("oversize", field)
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxRecordBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return nil, reject("unavailable", field)
	}
	if len(data) > maxRecordBytes {
		return nil, reject("oversize", field)
	}
	return data, nil
}

func readJournal(dir *os.File, expectedUID uint32) ([]journalEntry, error) {
	// Bound enumeration while leaving room for ignored dotfiles used by atomic
	// publication.
	names, readErr := dir.Readdirnames(maxJournalDirectoryEntries)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return nil, reject("unavailable", "journal")
	}
	if len(names) >= maxJournalDirectoryEntries {
		return nil, reject("oversize", "journal")
	}
	names = slicesWithoutDotfiles(names)
	if len(names) > maxJournalEntries {
		return nil, reject("oversize", "journal")
	}
	sort.Strings(names)
	entries := make([]journalEntry, 0, len(names))
	for index, name := range names {
		if name != fmt.Sprintf("%010d.json", index+1) {
			return nil, reject("sequence", "journal")
		}
		data, err := readRecordAt(int(dir.Fd()), name, expectedUID, "journal")
		if err != nil {
			return nil, err
		}
		entries = append(entries, journalEntry{data: data})
	}
	return entries, nil
}

func slicesWithoutDotfiles(names []string) []string {
	kept := names[:0]
	for _, name := range names {
		if !strings.HasPrefix(name, ".") {
			kept = append(kept, name)
		}
	}
	return kept
}

func parseRecord(data []byte) (record, error) {
	if len(data) == 0 || bytes.HasPrefix(data, []byte{0xef, 0xbb, 0xbf}) || !utf8.Valid(data) {
		return record{}, reject("encoding", "record")
	}
	if err := checkUniqueJSON(data); err != nil {
		return record{}, reject("schema", "record")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var result record
	if err := decoder.Decode(&result); err != nil {
		return record{}, reject("schema", "record")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return record{}, reject("schema", "record")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return record{}, reject("schema", "record")
	}
	if !hasExactKeys(fields, []string{"schema", "generation", "transition_id", "supersedes", "outcome", "backend", "volumes", "evidence", "published_at"}) {
		return record{}, reject("schema", "record")
	}
	backendFields, err := objectFields(fields["backend"])
	if err != nil {
		return record{}, reject("schema", "backend")
	}
	switch result.Backend.Kind {
	case "local":
		if !hasExactKeys(backendFields, []string{"kind", "dir"}) {
			return record{}, reject("schema", "backend")
		}
	case "s3":
		if !hasExactKeys(backendFields, []string{"kind", "endpoint", "bucket", "prefix"}) {
			return record{}, reject("schema", "backend")
		}
		var prefix *string
		if err := json.Unmarshal(backendFields["prefix"], &prefix); err != nil || prefix == nil {
			return record{}, reject("schema", "backend.prefix")
		}
	default:
		return record{}, reject("schema", "backend.kind")
	}
	volumeFields, err := objectFields(fields["volumes"])
	if err != nil || !hasExactKeys(volumeFields, []string{"tgblobs", "rustfsdata"}) {
		return record{}, reject("schema", "volumes")
	}
	evidenceFields, err := objectFields(fields["evidence"])
	if err != nil || !hasRequiredEvidenceKeys(result.Outcome, evidenceFields) {
		return record{}, reject("schema", "evidence")
	}
	return result, nil
}

func checkUniqueJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return errors.New("not an object")
	}
	if err := scanObject(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON")
	}
	return nil
}

func scanObject(decoder *json.Decoder) error {
	seen := make(map[string]struct{})
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := keyToken.(string)
		if !ok {
			return errors.New("invalid object key")
		}
		if _, duplicate := seen[key]; duplicate {
			return errors.New("duplicate object key")
		}
		seen[key] = struct{}{}
		value, err := decoder.Token()
		if err != nil {
			return err
		}
		if err := scanValue(decoder, value); err != nil {
			return err
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return errors.New("invalid object")
	}
	return nil
}

func scanArray(decoder *json.Decoder) error {
	for decoder.More() {
		value, err := decoder.Token()
		if err != nil {
			return err
		}
		if err := scanValue(decoder, value); err != nil {
			return err
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim(']') {
		return errors.New("invalid array")
	}
	return nil
}

func scanValue(decoder *json.Decoder, value json.Token) error {
	delimiter, ok := value.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		return scanObject(decoder)
	case '[':
		return scanArray(decoder)
	default:
		return errors.New("unexpected delimiter")
	}
}

func objectFields(data json.RawMessage) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return nil, errors.New("not an object")
	}
	return fields, nil
}

func hasExactKeys(fields map[string]json.RawMessage, expected []string) bool {
	if len(fields) != len(expected) {
		return false
	}
	for _, key := range expected {
		if _, ok := fields[key]; !ok {
			return false
		}
	}
	return true
}

func hasRequiredEvidenceKeys(outcome string, fields map[string]json.RawMessage) bool {
	var expected []string
	switch outcome {
	case "initial-local":
		expected = []string{"report_sha256"}
	case "s3-accepted":
		expected = []string{"report_sha256", "source_manifest_sha256", "destination_manifest_sha256", "object_count", "byte_total", "copy_passes"}
	case OutcomeEncrypted:
		expected = []string{"report_sha256", "encrypted_inventory_sha256", "object_count", "byte_total", "key_version", "writes"}
	case "recovered-local":
		expected = []string{"report_sha256", "s3_census_manifest_sha256", "restored_manifest_sha256", "object_count", "byte_total", "restore_passes", "retained_cutover_key_count"}
	case stackMoveOutcome:
		expected = []string{
			"report_sha256",
			"source_tgblobs_volume", "target_tgblobs_volume",
			"source_tgblobs_inventory_sha256", "target_tgblobs_inventory_sha256", "tgblobs_file_count",
			"source_rustfsdata_volume", "target_rustfsdata_volume",
			"source_rustfsdata_inventory_sha256", "target_rustfsdata_inventory_sha256", "rustfsdata_file_count",
		}
	default:
		return false
	}
	return hasExactKeys(fields, expected)
}

func validateRecord(
	record record,
	data []byte,
	generation int64,
	previousID, previousOutcome string,
	previousBackend backend,
	previousVolumes volumes,
) error {
	if record.Schema != "teagram.blob-mode/v1" || record.Generation != generation || !canonicalUUID(record.TransitionID) {
		return reject("schema", "record")
	}
	if generation == 1 {
		if record.Outcome != "initial-local" || !bytes.Equal(bytes.TrimSpace(record.Supersedes), []byte("null")) {
			return reject("supersession", "supersedes")
		}
	} else {
		var supersedes string
		if json.Unmarshal(record.Supersedes, &supersedes) != nil || !canonicalUUID(supersedes) || supersedes != previousID || record.Outcome == "initial-local" {
			return reject("supersession", "supersedes")
		}
		if record.Outcome == "recovered-local" && previousOutcome != "s3-accepted" {
			return reject("supersession", "outcome")
		}
		if record.Outcome == OutcomeS3Accepted && previousOutcome != "initial-local" && previousOutcome != "recovered-local" && previousOutcome != stackMoveOutcome {
			return reject("supersession", "outcome")
		}
		if record.Outcome == OutcomeEncrypted && previousOutcome != OutcomeS3Accepted {
			return reject("supersession", "outcome")
		}
		if record.Outcome == stackMoveOutcome && previousOutcome != "initial-local" && previousOutcome != "recovered-local" {
			return reject("supersession", "outcome")
		}
		if record.Outcome == stackMoveOutcome {
			var previousRustFSDataVolume string
			if previousBackend != record.Backend ||
				previousVolumes.TGBlobs != stackMoveSourceTGBlobs ||
				!bytes.Equal(bytes.TrimSpace(previousVolumes.RustFSData), []byte("null")) &&
					(json.Unmarshal(previousVolumes.RustFSData, &previousRustFSDataVolume) != nil || previousRustFSDataVolume != stackMoveSourceRustFSData) {
				return reject("supersession", "outcome")
			}
		}
	}
	if record.Outcome == "initial-local" && record.Backend.Kind != "local" ||
		record.Outcome == "s3-accepted" && record.Backend.Kind != "s3" ||
		record.Outcome == OutcomeEncrypted && record.Backend.Kind != "s3" ||
		record.Outcome == "recovered-local" && record.Backend.Kind != "local" ||
		record.Outcome == stackMoveOutcome && record.Backend.Kind != "local" {
		return reject("schema", "outcome")
	}
	if record.Backend.Kind == "local" && record.Backend.Dir == "" {
		return reject("schema", "backend.dir")
	}
	if record.Backend.Kind == "s3" {
		prefix, err := blob.NormalizeS3Prefix(record.Backend.Prefix)
		if record.Backend.Endpoint == "" || record.Backend.Bucket == "" {
			return reject("schema", "backend")
		}
		if err != nil || prefix != record.Backend.Prefix {
			return reject("schema", "backend.prefix")
		}
	}
	if record.Volumes.TGBlobs == "" {
		return reject("schema", "volumes.tgblobs")
	}
	if generation == 1 {
		if !bytes.Equal(bytes.TrimSpace(record.Volumes.RustFSData), []byte("null")) {
			return reject("schema", "volumes.rustfsdata")
		}
	} else {
		var volumeName string
		if bytes.Equal(bytes.TrimSpace(record.Volumes.RustFSData), []byte("null")) ||
			json.Unmarshal(record.Volumes.RustFSData, &volumeName) != nil || volumeName == "" {
			return reject("schema", "volumes.rustfsdata")
		}
		if record.Outcome == stackMoveOutcome &&
			(record.Volumes.TGBlobs != stackMoveTargetTGBlobs || volumeName != stackMoveTargetRustFSData) {
			return reject("schema", "volumes")
		}
	}
	if err := validateEvidence(record.Outcome, record.Evidence); err != nil {
		return err
	}
	publishedAt, err := time.Parse(time.RFC3339, record.PublishedAt)
	if err != nil {
		return reject("schema", "published_at")
	}
	_, offset := publishedAt.Zone()
	if offset != 0 {
		return reject("schema", "published_at")
	}
	if len(data) > maxRecordBytes {
		return reject("oversize", "record")
	}
	return nil
}

func validateEvidence(outcome string, evidence evidence) error {
	if !lowerHexDigest(evidence.ReportSHA256) {
		return reject("schema", "evidence.report_sha256")
	}
	switch outcome {
	case "initial-local":
		return nil
	case "s3-accepted":
		if !lowerHexDigest(evidence.SourceManifestSHA256) || !lowerHexDigest(evidence.DestinationManifestSHA256) ||
			evidence.ObjectCount == nil || *evidence.ObjectCount < 0 || evidence.ByteTotal == nil || *evidence.ByteTotal < 0 ||
			evidence.CopyPasses == nil || (*evidence.CopyPasses != 1 && *evidence.CopyPasses != 2) {
			return reject("schema", "evidence")
		}
	case OutcomeEncrypted:
		if !lowerHexDigest(evidence.EncryptedInventorySHA256) || evidence.ObjectCount == nil || *evidence.ObjectCount < 0 ||
			evidence.ByteTotal == nil || *evidence.ByteTotal < 0 || evidence.KeyVersion == 0 ||
			(evidence.Writes != "stopped" && evidence.Writes != "drained") {
			return reject("schema", "evidence")
		}
	case "recovered-local":
		if !lowerHexDigest(evidence.S3CensusManifestSHA256) || !lowerHexDigest(evidence.RestoredManifestSHA256) ||
			evidence.ObjectCount == nil || *evidence.ObjectCount < 0 || evidence.ByteTotal == nil || *evidence.ByteTotal < 0 ||
			evidence.RestorePasses == nil || (*evidence.RestorePasses != 1 && *evidence.RestorePasses != 2) ||
			evidence.RetainedCutoverKeyCount == nil || *evidence.RetainedCutoverKeyCount < 0 {
			return reject("schema", "evidence")
		}
	case stackMoveOutcome:
		if evidence.SourceTGBlobsVolume != stackMoveSourceTGBlobs ||
			evidence.TargetTGBlobsVolume != stackMoveTargetTGBlobs ||
			evidence.SourceTGBlobsInventory == "" ||
			evidence.SourceTGBlobsInventory != evidence.TargetTGBlobsInventory ||
			!lowerHexDigest(evidence.SourceTGBlobsInventory) ||
			evidence.TGBlobsFileCount == nil || *evidence.TGBlobsFileCount < 0 ||
			evidence.SourceRustFSDataVolume != stackMoveSourceRustFSData ||
			evidence.TargetRustFSDataVolume != stackMoveTargetRustFSData ||
			evidence.SourceRustFSDataInventory == "" ||
			evidence.SourceRustFSDataInventory != evidence.TargetRustFSDataInventory ||
			!lowerHexDigest(evidence.SourceRustFSDataInventory) ||
			evidence.RustFSDataFileCount == nil || *evidence.RustFSDataFileCount < 0 {
			return reject("schema", "evidence")
		}
	default:
		return reject("schema", "outcome")
	}
	return nil
}

func validateBackend(backend backend, cfg EffectiveConfig) error {
	if cfg.BlobS3 == nil {
		if backend.Kind != "local" || filepath.Clean(cfg.BlobDir) != backend.Dir {
			return reject("backend", "backend")
		}
		return nil
	}
	recordPrefix, err := blob.NormalizeS3Prefix(backend.Prefix)
	if err != nil || recordPrefix != backend.Prefix {
		return reject("backend", "backend.prefix")
	}
	configPrefix, err := blob.NormalizeS3Prefix(cfg.BlobS3.Prefix)
	if err != nil || backend.Kind != "s3" || backend.Endpoint != cfg.BlobS3.Endpoint ||
		backend.Bucket != cfg.BlobS3.Bucket || backend.Prefix != configPrefix {
		return reject("backend", "backend")
	}
	return nil
}

func canonicalUUID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	for index, char := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			continue
		}
		if !lowerHexDigit(char) {
			return false
		}
	}
	return true
}

func lowerHexDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if !lowerHexDigit(char) {
			return false
		}
	}
	return true
}

func lowerHexDigit(char rune) bool {
	return char >= '0' && char <= '9' || char >= 'a' && char <= 'f'
}
