package store

import (
	"context"
	"fmt"
	"strconv"

	"github.com/teagramhq/teagram-server/internal/blob"
)

// BlobInventoryReference is one stored file or live part row that names a blob
// object. Unstored files and part rows with an empty key name no object and are
// not included.
type BlobInventoryReference struct {
	Key          string
	Kind         string
	ID           string
	LogicalBytes int64
}

// BlobInventoryReferences returns all database rows that account for an
// object in the configured blob store. The query is read-only; file ids map to
// their canonical blob key while upload part rows already carry their key.
func (s *Store) BlobInventoryReferences(ctx context.Context) ([]BlobInventoryReference, error) {
	rows, err := s.q.BlobInventoryReferences(ctx)
	if err != nil {
		return nil, fmt.Errorf("blob inventory references: %w", err)
	}
	refs := make([]BlobInventoryReference, 0, len(rows))
	for _, row := range rows {
		switch row.Kind {
		case "file":
			refs = append(refs, BlobInventoryReference{
				Key:          blob.Key(row.FileID),
				Kind:         "file",
				ID:           strconv.FormatInt(row.FileID, 10),
				LogicalBytes: row.LogicalBytes,
			})
		case "part":
			refs = append(refs, BlobInventoryReference{
				Key:          row.BlobKey,
				Kind:         "part",
				ID:           row.BlobKey,
				LogicalBytes: row.LogicalBytes,
			})
		default:
			return nil, fmt.Errorf("blob inventory references: unexpected row kind %q", row.Kind)
		}
	}
	return refs, nil
}
