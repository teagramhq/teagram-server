package api

import (
	"errors"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/mtproto"
)

func TestSaveFilePartDecodesLogicalBody(t *testing.T) {
	var packed bin.Buffer
	packed.PutInt32(0x12345678)
	var body bin.Buffer
	if err := (&tg.UploadSaveFilePartRequest{FileID: 80102, FilePart: 0, Bytes: []byte("upload body")}).Encode(&body); err != nil {
		t.Fatalf("encode request body: %v", err)
	}

	_, err := testHandlers(nil).handleSaveFilePart(&mtproto.Request{
		Buf: &packed, Body: &body,
	})
	if err == nil {
		t.Fatal("handleSaveFilePart succeeded for an unregistered key")
	}
	if !errors.Is(err, errAuthKeyUnreg) {
		t.Fatalf("handleSaveFilePart error = %v, want %v", err, errAuthKeyUnreg)
	}
}
