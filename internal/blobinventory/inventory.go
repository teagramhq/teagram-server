// Package blobinventory reports how referenced media and objects in the blob
// store are represented. It is read-only and requires a full-tree listing so
// a partial backend view cannot produce a clean-looking inventory.
package blobinventory

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/teagramhq/teagram-server/internal/blob"
)

// Tree is the full-tree enumeration capability implemented by the configured
// local and S3 stores. It is separate from blob.Store's scoped WalkPrefix.
type Tree interface {
	Walk(context.Context, func(blob.Entry) error) error
}

// Reference is one database row that accounts for a blob object.
type Reference struct {
	Key          string
	Kind         string
	ID           string
	LogicalBytes int64
}

// ObjectID identifies an object without including user credentials or media
// metadata. Key is the exact storage key; ID is a file id or, for parts and
// unknown keys, the key itself.
type ObjectID struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	Key  string `json:"key"`
}

// Class reports the number of objects and their logical and physical sizes.
// IDs is populated for objects that are missing, unaccounted, or do not have
// a valid envelope matching their database reference.
type Class struct {
	Count         int64      `json:"count"`
	LogicalBytes  int64      `json:"logical_bytes"`
	PhysicalBytes int64      `json:"physical_bytes"`
	IDs           []ObjectID `json:"ids"`
}

// Classes partitions the inventory into referenced representations, absent
// references, and objects that no row names.
type Classes struct {
	ValidEnvelope   Class `json:"valid_envelope"`
	NoValidEnvelope Class `json:"no_valid_envelope"`
	Missing         Class `json:"missing"`
	Unaccounted     Class `json:"unaccounted"`
}

// KeyVersion counts every parseable header claim, including claims whose
// header did not authenticate. Retirable is conservative: any referenced
// object whose header claims this version keeps it false.
type KeyVersion struct {
	Version                  uint32 `json:"version"`
	Objects                  int64  `json:"objects"`
	ReferencedObjects        int64  `json:"referenced_objects"`
	ValidEnvelopes           int64  `json:"valid_envelopes"`
	ReferencedValidEnvelopes int64  `json:"referenced_valid_envelopes"`
	LogicalBytes             int64  `json:"logical_bytes"`
	PhysicalBytes            int64  `json:"physical_bytes"`
	ReferencedLogicalBytes   int64  `json:"referenced_logical_bytes"`
	ReferencedPhysicalBytes  int64  `json:"referenced_physical_bytes"`
	UnknownLogicalObjects    int64  `json:"unknown_logical_objects"`
	Retirable                bool   `json:"retirable"`
}

// Report is one complete inventory. Summary is intended for an operator; the
// class and version fields are the machine-readable record.
type Report struct {
	Summary                       string       `json:"summary"`
	Classes                       Classes      `json:"classes"`
	KeyVersions                   []KeyVersion `json:"key_versions"`
	UnversionedEnvelopeReferences int64        `json:"unversioned_envelope_references"`
}

// Scan compares the database references with every entry in blobs' full-tree
// view. Header inspection reads at most the fixed envelope header and validates
// its wrapped data key, never an encrypted chunk. The caller must quiesce blob
// writes and row changes when it needs a cutover-grade point-in-time result.
func Scan(ctx context.Context, blobs blob.Store, ring *blob.KeyRing, references []Reference) (Report, error) {
	tree, ok := blobs.(Tree)
	if !ok {
		return Report{}, errors.New("blob inventory: configured store does not support full-tree enumeration")
	}
	byKey := make(map[string]Reference, len(references))
	for _, ref := range references {
		if err := blob.ValidateKey(ref.Key); err != nil {
			return Report{}, fmt.Errorf("blob inventory: invalid referenced key: %w", err)
		}
		if ref.Kind != "file" && ref.Kind != "part" {
			return Report{}, fmt.Errorf("blob inventory: invalid reference kind %q", ref.Kind)
		}
		if ref.ID == "" || ref.LogicalBytes < 0 {
			return Report{}, fmt.Errorf("blob inventory: invalid reference for key %q", ref.Key)
		}
		if _, exists := byKey[ref.Key]; exists {
			return Report{}, fmt.Errorf("blob inventory: duplicate database reference for key %q", ref.Key)
		}
		byKey[ref.Key] = ref
	}

	report := Report{
		Classes: Classes{
			Missing:     Class{IDs: make([]ObjectID, 0)},
			Unaccounted: Class{IDs: make([]ObjectID, 0)},
		},
		KeyVersions: make([]KeyVersion, 0),
	}
	versions := make(map[uint32]*KeyVersion)
	if ring != nil {
		for _, version := range ring.Versions() {
			versions[version] = &KeyVersion{Version: version}
		}
	}
	seenReferences := make(map[string]struct{}, len(byKey))

	if err := tree.Walk(ctx, func(entry blob.Entry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Dir {
			if _, referenced := byKey[entry.Key]; referenced {
				return fmt.Errorf("blob inventory: referenced key %q is a directory", entry.Key)
			}
			return nil
		}
		if entry.Size < 0 {
			return fmt.Errorf("blob inventory: full-tree listing returned negative size for %q", entry.Key)
		}
		ref, referenced := byKey[entry.Key]
		if referenced {
			if _, duplicate := seenReferences[entry.Key]; duplicate {
				return fmt.Errorf("blob inventory: full-tree listing returned key %q twice", entry.Key)
			}
			seenReferences[entry.Key] = struct{}{}
		}

		if !entry.Regular {
			if referenced {
				return addClass(&report.Classes.NoValidEnvelope, ref.LogicalBytes, 0, nil)
			}
			id := objectIDForKey(entry.Key)
			return addClass(&report.Classes.Unaccounted, 0, 0, &id)
		}
		if err := blob.ValidateKey(entry.Key); err != nil {
			if referenced {
				return fmt.Errorf("blob inventory: referenced key is invalid: %w", err)
			}
			id := objectIDForKey(entry.Key)
			return addClass(&report.Classes.Unaccounted, entry.Size, entry.Size, &id)
		}

		info, inspectErr := blob.InspectEnvelopeHeader(ctx, blobs, ring, entry.Key, entry.Size)
		if errors.Is(inspectErr, blob.ErrEnvelopeKeyUnavailable) {
			return fmt.Errorf("blob inventory: cannot authenticate envelope header for %q: %w", entry.Key, inspectErr)
		}
		validEnvelope := inspectErr == nil
		if inspectErr != nil && !errors.Is(inspectErr, blob.ErrEnvelope) {
			return fmt.Errorf("blob inventory: inspect %q: %w", entry.Key, inspectErr)
		}
		if referenced && info.HasEnvelopeMagic && !info.HasKeyVersion {
			report.UnversionedEnvelopeReferences++
		}
		if info.HasKeyVersion {
			version := versions[info.KeyVersion]
			if version == nil {
				version = &KeyVersion{Version: info.KeyVersion}
				versions[info.KeyVersion] = version
			}
			version.Objects++
			if referenced {
				version.ReferencedObjects++
			}
			if info.HasLogicalBytes {
				if err := addBytes(&version.LogicalBytes, info.LogicalBytes); err != nil {
					return fmt.Errorf("blob inventory: key version %d logical bytes: %w", info.KeyVersion, err)
				}
				if referenced {
					if err := addBytes(&version.ReferencedLogicalBytes, info.LogicalBytes); err != nil {
						return fmt.Errorf("blob inventory: key version %d referenced logical bytes: %w", info.KeyVersion, err)
					}
				}
			} else {
				version.UnknownLogicalObjects++
			}
			if err := addBytes(&version.PhysicalBytes, entry.Size); err != nil {
				return fmt.Errorf("blob inventory: key version %d physical bytes: %w", info.KeyVersion, err)
			}
			if referenced {
				if err := addBytes(&version.ReferencedPhysicalBytes, entry.Size); err != nil {
					return fmt.Errorf("blob inventory: key version %d referenced physical bytes: %w", info.KeyVersion, err)
				}
			}
			if validEnvelope {
				version.ValidEnvelopes++
				if referenced {
					version.ReferencedValidEnvelopes++
				}
			}
		}

		if referenced {
			if validEnvelope && info.LogicalBytes == ref.LogicalBytes {
				return addClass(&report.Classes.ValidEnvelope, info.LogicalBytes, entry.Size, nil)
			}
			id := ObjectID{Kind: ref.Kind, ID: ref.ID, Key: entry.Key}
			return addClass(&report.Classes.NoValidEnvelope, ref.LogicalBytes, entry.Size, &id)
		}
		id := objectIDForKey(entry.Key)
		logicalBytes := entry.Size
		if info.HasLogicalBytes {
			logicalBytes = info.LogicalBytes
		}
		return addClass(&report.Classes.Unaccounted, logicalBytes, entry.Size, &id)
	}); err != nil {
		return Report{}, fmt.Errorf("blob inventory: walk: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return Report{}, fmt.Errorf("blob inventory: walk: %w", err)
	}

	for key, ref := range byKey {
		if _, exists := seenReferences[key]; exists {
			continue
		}
		id := ObjectID{Kind: ref.Kind, ID: ref.ID, Key: key}
		if err := addClass(&report.Classes.Missing, ref.LogicalBytes, 0, &id); err != nil {
			return Report{}, fmt.Errorf("blob inventory: missing object totals: %w", err)
		}
	}

	versionNumbers := make([]uint32, 0, len(versions))
	for version := range versions {
		versionNumbers = append(versionNumbers, version)
	}
	slices.Sort(versionNumbers)
	for _, number := range versionNumbers {
		version := *versions[number]
		version.Retirable = version.ReferencedObjects == 0 && report.UnversionedEnvelopeReferences == 0
		report.KeyVersions = append(report.KeyVersions, version)
	}
	slices.SortFunc(report.Classes.Missing.IDs, func(a, b ObjectID) int {
		return strings.Compare(a.Key, b.Key)
	})
	slices.SortFunc(report.Classes.Unaccounted.IDs, func(a, b ObjectID) int {
		return strings.Compare(a.Key, b.Key)
	})
	for i := 1; i < len(report.Classes.Unaccounted.IDs); i++ {
		if report.Classes.Unaccounted.IDs[i-1].Key == report.Classes.Unaccounted.IDs[i].Key {
			return Report{}, fmt.Errorf("blob inventory: full-tree listing returned key %q twice", report.Classes.Unaccounted.IDs[i].Key)
		}
	}
	report.Summary = makeSummary(report)
	return report, nil
}

func addClass(class *Class, logicalBytes, physicalBytes int64, id *ObjectID) error {
	if logicalBytes < 0 || physicalBytes < 0 {
		return errors.New("negative byte total")
	}
	if err := addBytes(&class.LogicalBytes, logicalBytes); err != nil {
		return err
	}
	if err := addBytes(&class.PhysicalBytes, physicalBytes); err != nil {
		return err
	}
	class.Count++
	if id != nil {
		class.IDs = append(class.IDs, *id)
	}
	return nil
}

func addBytes(total *int64, value int64) error {
	if value < 0 || *total > math.MaxInt64-value {
		return errors.New("byte total overflow")
	}
	*total += value
	return nil
}

func objectIDForKey(key string) ObjectID {
	if id, ok := blob.ParseKey(key); ok {
		return ObjectID{Kind: "file", ID: strconv.FormatInt(id, 10), Key: key}
	}
	if blob.ParsePartKey(key) {
		return ObjectID{Kind: "part", ID: key, Key: key}
	}
	return ObjectID{Kind: "unknown", ID: key, Key: key}
}

func makeSummary(report Report) string {
	parts := make([]string, 0, len(report.KeyVersions))
	for _, version := range report.KeyVersions {
		state := "retirable"
		if !version.Retirable {
			state = "not retirable"
		}
		parts = append(parts, fmt.Sprintf("%d=%s", version.Version, state))
	}
	versionSummary := "no key versions observed"
	if len(parts) > 0 {
		versionSummary = "key versions " + strings.Join(parts, ", ")
	}
	if report.UnversionedEnvelopeReferences > 0 {
		// A referenced header with no readable version conservatively blocks
		// retirement of every key in the ring.
		versionSummary += fmt.Sprintf("; %d unversioned envelope references block retirement", report.UnversionedEnvelopeReferences)
	}
	return fmt.Sprintf("media inventory (headers only): %d valid envelopes, %d without a valid envelope, %d missing, %d unaccounted; %s",
		report.Classes.ValidEnvelope.Count,
		report.Classes.NoValidEnvelope.Count,
		report.Classes.Missing.Count,
		report.Classes.Unaccounted.Count,
		versionSummary,
	)
}
