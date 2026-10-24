package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// EncryptedObjectsPrefix is the physical S3 namespace used by the migration
// from accepted plaintext objects to encrypted media.
const EncryptedObjectsPrefix = "encrypted"

// EncryptedObjectKey returns the physical key used for an encrypted copy while
// preserving the original key as the envelope's authenticated identity.
func EncryptedObjectKey(key string) (string, error) {
	if err := ValidateKey(key); err != nil {
		return "", err
	}
	return EncryptedObjectsPrefix + "/" + key, nil
}

// PrefixedStore maps logical keys into one physical namespace. The mapping is
// below decorators such as [Encrypted], so envelopes remain bound to logical
// keys while the backing store sees only the prefixed keys.
type PrefixedStore struct {
	backend Store
	prefix  string
}

// NewPrefixedStore maps all operations below prefix. It is used to keep
// migrated encrypted copies beside accepted plaintext objects until an
// explicit cleanup pass removes the plaintext copies.
func NewPrefixedStore(backend Store, prefix string) (*PrefixedStore, error) {
	if backend == nil {
		return nil, errors.New("prefixed blob store: backend is required")
	}
	if err := ValidateKey(prefix); err != nil {
		return nil, fmt.Errorf("prefixed blob store: invalid prefix: %w", err)
	}
	return &PrefixedStore{backend: backend, prefix: prefix}, nil
}

func (p *PrefixedStore) physicalKey(key string) (string, error) {
	if err := ValidateKey(key); err != nil {
		return "", err
	}
	physical := p.prefix + "/" + key
	if err := ValidateKey(physical); err != nil {
		return "", fmt.Errorf("prefixed blob store: mapped key: %w", err)
	}
	return physical, nil
}

func (p *PrefixedStore) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	physical, err := p.physicalKey(key)
	if err != nil {
		return 0, err
	}
	return p.backend.Put(ctx, physical, r)
}

func (p *PrefixedStore) ReadAt(ctx context.Context, key string, offset, limit int64) ([]byte, error) {
	physical, err := p.physicalKey(key)
	if err != nil {
		return nil, err
	}
	return p.backend.ReadAt(ctx, physical, offset, limit)
}

func (p *PrefixedStore) Remove(ctx context.Context, key string) error {
	physical, err := p.physicalKey(key)
	if err != nil {
		return err
	}
	return p.backend.Remove(ctx, physical)
}

func (p *PrefixedStore) WalkPrefix(ctx context.Context, prefix string, fn func(Entry) error) error {
	if prefix == "" {
		return nil
	}
	physical, err := p.physicalKey(prefix)
	if err != nil {
		return err
	}
	return p.backend.WalkPrefix(ctx, physical, func(entry Entry) error {
		logical, ok := strings.CutPrefix(entry.Key, p.prefix+"/")
		if !ok {
			return fmt.Errorf("prefixed blob store: backend listed key outside prefix %q", p.prefix)
		}
		entry.Key = logical
		return fn(entry)
	})
}

// Walk enumerates only the mapped namespace and restores its logical keys.
// This makes a prefixed encrypted store usable with inventory without making
// neighboring plaintext objects look like part of its reference set.
func (p *PrefixedStore) Walk(ctx context.Context, fn func(Entry) error) error {
	tree, ok := p.backend.(interface {
		Walk(context.Context, func(Entry) error) error
	})
	if !ok {
		return errors.New("prefixed blob store: backend does not support full-tree enumeration")
	}
	return tree.Walk(ctx, func(entry Entry) error {
		if entry.Key == p.prefix {
			return nil
		}
		logical, ok := strings.CutPrefix(entry.Key, p.prefix+"/")
		if !ok {
			return nil
		}
		if err := ValidateKey(logical); err != nil {
			return fmt.Errorf("prefixed blob store: backend listed invalid mapped key: %w", err)
		}
		entry.Key = logical
		return fn(entry)
	})
}

func (p *PrefixedStore) OperationTimeout() time.Duration {
	if bounded, ok := p.backend.(OperationTimeoutProvider); ok {
		return bounded.OperationTimeout()
	}
	return 0
}
