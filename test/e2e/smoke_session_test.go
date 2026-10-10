package e2e_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/gotd/td/session"
)

type missingOnceSessionStorage struct {
	storage *session.StorageMemory
	missing bool
}

func (s *missingOnceSessionStorage) LoadSession(ctx context.Context) ([]byte, error) {
	if !s.missing {
		s.missing = true
		return nil, session.ErrNotFound
	}
	return s.storage.LoadSession(ctx)
}

func (s *missingOnceSessionStorage) StoreSession(ctx context.Context, data []byte) error {
	return s.storage.StoreSession(ctx, data)
}

func TestLoadSmokeSessionWaitsForInitialSave(t *testing.T) {
	backing := &session.StorageMemory{}
	want := &session.Data{DC: 2, AuthKeyID: []byte("12345678")}
	if err := (&session.Loader{Storage: backing}).Save(context.Background(), want); err != nil {
		t.Fatal(err)
	}

	got, err := loadSmokeSession(context.Background(), &missingOnceSessionStorage{storage: backing})
	if err != nil {
		t.Fatal(err)
	}
	if got.DC != want.DC || !bytes.Equal(got.AuthKeyID, want.AuthKeyID) {
		t.Fatalf("loaded session = %+v, want DC %d and auth key ID %x", got, want.DC, want.AuthKeyID)
	}
}
