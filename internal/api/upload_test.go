package api_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestSaveFilePartRejectsUnauthenticated(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	_, err := api.SaveFilePartForTest(s, 0, &tg.UploadSaveFilePartRequest{
		FileID: 999, FilePart: 0, Bytes: []byte("hello world"),
	})
	rpcError(t, err, "AUTH_KEY_UNREGISTERED")
}

func TestSaveFilePartRejectsBadInput(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	u, err := s.CreateUser(ctx, "+15551294001")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	tests := map[string]struct {
		req  tg.UploadSaveFilePartRequest
		want string
	}{
		"zero file id": {
			req:  tg.UploadSaveFilePartRequest{FileID: 0, FilePart: 0, Bytes: []byte("hello world")},
			want: "FILE_PART_INVALID",
		},
		"negative part": {
			req:  tg.UploadSaveFilePartRequest{FileID: 999, FilePart: -1, Bytes: []byte("hello world")},
			want: "FILE_PART_INVALID",
		},
		"part at max": {
			req:  tg.UploadSaveFilePartRequest{FileID: 999, FilePart: api.MaxFileParts(), Bytes: []byte("hello world")},
			want: "FILE_PART_INVALID",
		},
		"payload over protocol max": {
			req:  tg.UploadSaveFilePartRequest{FileID: 999, FilePart: 1, Bytes: bytes.Repeat([]byte{'a'}, store.MaxPartBytes+1)},
			want: "FILE_PART_TOO_BIG",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := api.SaveFilePartForTest(s, u.ID, &tc.req)
			rpcError(t, err, tc.want)
		})
	}
}

func TestSaveFilePartStoresPayload(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	u, err := s.CreateUser(ctx, "+15551294002")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	want := []byte("hello world")
	res, err := api.SaveFilePartForTest(s, u.ID, &tg.UploadSaveFilePartRequest{
		FileID: 999, FilePart: 0, Bytes: want,
	})
	if err != nil {
		t.Fatalf("save file part: %v", err)
	}
	if _, ok := res.(*tg.BoolTrue); !ok {
		t.Fatalf("result = %T, want *tg.BoolTrue", res)
	}
	got, ok, err := s.UploadPart(ctx, u.ID, 999, 0)
	if err != nil || !ok {
		t.Fatalf("read back: ok=%v err=%v", ok, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("payload = %q, want %q", got, want)
	}
}

// TestSaveFilePartAcceptsLastPart pins the boundary the rejection above sits on:
// one index below the derived maximum is a legal part of a max-size file.
func TestSaveFilePartAcceptsLastPart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	u, err := s.CreateUser(ctx, "+15551294003")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	last := api.MaxFileParts() - 1
	if _, err := api.SaveFilePartForTest(s, u.ID, &tg.UploadSaveFilePartRequest{
		FileID: 999, FilePart: last, Bytes: []byte("hello world"),
	}); err != nil {
		t.Fatalf("save part %d: %v", last, err)
	}
	if _, ok, err := s.UploadPart(ctx, u.ID, 999, last); err != nil || !ok {
		t.Fatalf("read back part %d: ok=%v err=%v", last, ok, err)
	}
}

// TestSaveFilePartRejectedIndexWritesNothing is the aliasing guard: a rejected
// index must not land on another part's row. file_part is 32 bits on the wire,
// so an index past MaxInt32 cannot arrive here at all — that branch of
// store.partIndexOf is covered in the store's own test — but an index past the
// derived per-file maximum can, and it must leave part 0 untouched.
func TestSaveFilePartRejectedIndexWritesNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	u, err := s.CreateUser(ctx, "+15551294004")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	want := []byte("hello world")
	if _, err := api.SaveFilePartForTest(s, u.ID, &tg.UploadSaveFilePartRequest{
		FileID: 999, FilePart: 0, Bytes: want,
	}); err != nil {
		t.Fatalf("save part 0: %v", err)
	}
	for _, bad := range []int{-1, api.MaxFileParts()} {
		_, err := api.SaveFilePartForTest(s, u.ID, &tg.UploadSaveFilePartRequest{
			FileID: 999, FilePart: bad, Bytes: []byte("clobber"),
		})
		rpcError(t, err, "FILE_PART_INVALID")
	}
	got, ok, err := s.UploadPart(ctx, u.ID, 999, 0)
	if err != nil || !ok {
		t.Fatalf("read back: ok=%v err=%v", ok, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("part 0 was overwritten: got %q, want %q", got, want)
	}
}

// TestSaveFilePartPerFileCap drives the store's per-file byte cap through the
// handler: the second max-size part takes the file past a 600 KiB cap.
func TestSaveFilePartPerFileCap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	u, err := s.CreateUser(ctx, "+15551294006")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	const cap600 = 600 * 1024
	full := bytes.Repeat([]byte{'a'}, store.MaxPartBytes)
	if _, err := api.SaveFilePartCappedForTest(s, u.ID, cap600, &tg.UploadSaveFilePartRequest{
		FileID: 999, FilePart: 0, Bytes: full,
	}); err != nil {
		t.Fatalf("save part 0: %v", err)
	}
	_, err = api.SaveFilePartCappedForTest(s, u.ID, cap600, &tg.UploadSaveFilePartRequest{
		FileID: 999, FilePart: 1, Bytes: full,
	})
	rpcError(t, err, "FILE_PART_TOO_BIG")
}

// TestSaveFilePartOutstandingCap drives the per-user outstanding-bytes cap,
// which is twice the per-file one, across three files that are each legal on
// their own. The mapping under test is the one the M5 threat model rules on:
// over the cap the account gets FLOOD_WAIT, not INTERNAL.
func TestSaveFilePartOutstandingCap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	u, err := s.CreateUser(ctx, "+15551294007")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	const cap600 = 600 * 1024
	full := bytes.Repeat([]byte{'a'}, store.MaxPartBytes)
	for _, fileID := range []int64{1, 2} {
		if _, err := api.SaveFilePartCappedForTest(s, u.ID, cap600, &tg.UploadSaveFilePartRequest{
			FileID: fileID, FilePart: 0, Bytes: full,
		}); err != nil {
			t.Fatalf("save file %d: %v", fileID, err)
		}
	}
	_, err = api.SaveFilePartCappedForTest(s, u.ID, cap600, &tg.UploadSaveFilePartRequest{
		FileID: 3, FilePart: 0, Bytes: full,
	})
	rpcError(t, err, "FLOOD_WAIT_60")
}

// TestSaveFilePartRowCap covers the row-count half of the same outstanding cap,
// which reports the same FLOOD_WAIT: with a 64 KiB per-file cap the row cap is
// four, so the fifth one-byte part of a fifth file is rejected on rows rather
// than on bytes.
func TestSaveFilePartRowCap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	u, err := s.CreateUser(ctx, "+15551294008")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	const cap64 = 64 * 1024
	for fileID := int64(1); fileID <= 4; fileID++ {
		if _, err := api.SaveFilePartCappedForTest(s, u.ID, cap64, &tg.UploadSaveFilePartRequest{
			FileID: fileID, FilePart: 0, Bytes: []byte("x"),
		}); err != nil {
			t.Fatalf("save file %d: %v", fileID, err)
		}
	}
	_, err = api.SaveFilePartCappedForTest(s, u.ID, cap64, &tg.UploadSaveFilePartRequest{
		FileID: 5, FilePart: 0, Bytes: []byte("x"),
	})
	rpcError(t, err, "FLOOD_WAIT_60")
}

func TestConfiguredUploadByteLimitBoundaries(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	u, err := s.CreateUser(ctx, "+15551294031")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	for _, tc := range []struct {
		name    string
		fileID  int64
		size    int
		wantErr string
	}{
		{name: "499 KiB accepted", fileID: 9031, size: 499 * 1024},
		{name: "501 KiB rejected", fileID: 9032, size: 501 * 1024, wantErr: "FILE_PART_TOO_BIG"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := bytes.Repeat([]byte{'a'}, tc.size)
			partSize := store.MinPartBytesForRowCap
			partCount := 0
			for offset := 0; offset < len(payload); offset += partSize {
				end := min(offset+partSize, len(payload))
				_, err := api.SaveFilePartCappedForTest(s, u.ID, 512000, &tg.UploadSaveFilePartRequest{
					FileID: tc.fileID, FilePart: partCount, Bytes: payload[offset:end],
				})
				partCount++
				if end == len(payload) && tc.wantErr != "" {
					rpcError(t, err, tc.wantErr)
					break
				}
				if err != nil {
					t.Fatalf("save part %d: %v", partCount-1, err)
				}
			}
			parts, maxIndex, totalBytes, err := s.UploadPartsSummary(ctx, u.ID, tc.fileID)
			if err != nil {
				t.Fatalf("upload summary: %v", err)
			}
			wantParts := int64(partCount)
			wantBytes := int64(tc.size)
			wantMaxIndex := partCount - 1
			if tc.wantErr != "" {
				wantParts = 0
				wantBytes = 0
				wantMaxIndex = -1
			}
			if parts != wantParts || totalBytes != wantBytes || maxIndex != wantMaxIndex {
				t.Fatalf("stored parts = %d, max index = %d, bytes = %d; want %d, %d, %d", parts, maxIndex, totalBytes, wantParts, wantMaxIndex, wantBytes)
			}
			if tc.wantErr == "" {
				for part := range partCount {
					start := part * partSize
					end := min(start+partSize, len(payload))
					got, ok, err := s.UploadPart(ctx, u.ID, tc.fileID, part)
					if err != nil || !ok || !bytes.Equal(got, payload[start:end]) {
						t.Fatalf("accepted part %d: ok=%v bytes=%d err=%v", part, ok, len(got), err)
					}
				}
			}
		})
	}
}

func TestOversizedUploadRemovesEarlierParts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	u, err := s.CreateUser(ctx, "+15551294032")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	const fileID = 9033
	if _, err := api.SaveFilePartCappedForTest(s, u.ID, 800000, &tg.UploadSaveFilePartRequest{
		FileID: fileID, FilePart: 0, Bytes: bytes.Repeat([]byte{'a'}, 400*1024),
	}); err != nil {
		t.Fatalf("save first part: %v", err)
	}
	_, err = api.SaveFilePartCappedForTest(s, u.ID, 800000, &tg.UploadSaveFilePartRequest{
		FileID: fileID, FilePart: 1, Bytes: bytes.Repeat([]byte{'b'}, 400*1024),
	})
	rpcError(t, err, "FILE_PART_TOO_BIG")

	parts, _, _, err := s.UploadPartsSummary(ctx, u.ID, fileID)
	if err != nil {
		t.Fatalf("upload summary: %v", err)
	}
	if parts != 0 {
		t.Fatalf("stored parts after over-limit rejection = %d, want 0", parts)
	}
}

func TestOversizedPartRemovesEarlierParts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	u, err := s.CreateUser(ctx, "+15551294035")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	const fileID = 9036
	if _, err := api.SaveFilePartCappedForTest(s, u.ID, 512000, &tg.UploadSaveFilePartRequest{
		FileID: fileID, FilePart: 0, Bytes: bytes.Repeat([]byte{'a'}, 499*1024),
	}); err != nil {
		t.Fatalf("save initial part: %v", err)
	}
	_, err = api.SaveFilePartCappedForTest(s, u.ID, 512000, &tg.UploadSaveFilePartRequest{
		FileID: fileID, FilePart: 0, Bytes: bytes.Repeat([]byte{'b'}, 515*1024),
	})
	rpcError(t, err, "FILE_PART_TOO_BIG")

	parts, _, _, err := s.UploadPartsSummary(ctx, u.ID, fileID)
	if err != nil {
		t.Fatalf("upload summary: %v", err)
	}
	if parts != 0 {
		t.Fatalf("stored parts after oversized-part rejection = %d, want 0", parts)
	}
}

func TestOversizedBigFileIsRejectedBeforeStoringPart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	u, err := s.CreateUser(ctx, "+15551294033")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	const fileID = 9034
	_, err = api.SaveBigFilePartCappedForTest(s, u.ID, 512000, &tg.UploadSaveBigFilePartRequest{
		FileID: fileID, FilePart: 0, FileTotalParts: 20, Bytes: bytes.Repeat([]byte{'v'}, store.MaxPartBytes),
	})
	rpcError(t, err, "FILE_PART_TOO_BIG")

	parts, _, _, err := s.UploadPartsSummary(ctx, u.ID, fileID)
	if err != nil {
		t.Fatalf("upload summary: %v", err)
	}
	if parts != 0 {
		t.Fatalf("stored parts after declared-size rejection = %d, want 0", parts)
	}
}

func TestDeclaredBigFileSizeIsRejectedBeforeStoringPart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	u, err := s.CreateUser(ctx, "+15551294034")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	const fileID = 9035
	_, err = api.SaveBigFilePartCappedForTest(s, u.ID, 800000, &tg.UploadSaveBigFilePartRequest{
		FileID: fileID, FilePart: 0, FileTotalParts: 2, Bytes: bytes.Repeat([]byte{'v'}, 400*1024),
	})
	rpcError(t, err, "FILE_PART_TOO_BIG")

	parts, _, _, err := s.UploadPartsSummary(ctx, u.ID, fileID)
	if err != nil {
		t.Fatalf("upload summary: %v", err)
	}
	if parts != 0 {
		t.Fatalf("stored parts after declared-size rejection = %d, want 0", parts)
	}
}

func TestSaveBigFilePartValidatesTotalParts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	u, err := s.CreateUser(ctx, "+15551294005")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	tests := map[string]struct {
		req  tg.UploadSaveBigFilePartRequest
		want string
	}{
		"part past declared total": {
			req:  tg.UploadSaveBigFilePartRequest{FileID: 999, FilePart: 3, FileTotalParts: 3, Bytes: []byte("hello world")},
			want: "FILE_PART_INVALID",
		},
		"total parts zero": {
			req:  tg.UploadSaveBigFilePartRequest{FileID: 999, FilePart: 0, FileTotalParts: 0, Bytes: []byte("hello world")},
			want: "FILE_PART_INVALID",
		},
		"total parts past max": {
			req:  tg.UploadSaveBigFilePartRequest{FileID: 999, FilePart: 0, FileTotalParts: api.MaxFileParts() + 1, Bytes: []byte("hello world")},
			want: "FILE_PART_TOO_BIG",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := api.SaveBigFilePartForTest(s, u.ID, &tc.req)
			rpcError(t, err, tc.want)
		})
	}

	if _, err := api.SaveBigFilePartForTest(s, u.ID, &tg.UploadSaveBigFilePartRequest{
		FileID: 999, FilePart: 2, FileTotalParts: 3, Bytes: []byte("hello world"),
	}); err != nil {
		t.Fatalf("save big file part: %v", err)
	}
	if _, ok, err := s.UploadPart(ctx, u.ID, 999, 2); err != nil || !ok {
		t.Fatalf("read back: ok=%v err=%v", ok, err)
	}
}
