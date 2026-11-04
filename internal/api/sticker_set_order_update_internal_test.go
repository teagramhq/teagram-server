package api

import (
	"reflect"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/store"
)

func TestStickerSetsUpdateCarriesOrderKindAndIDs(t *testing.T) {
	order := &store.StickerSetOrderUpdate{Masks: true, SetIDs: []int64{-5, 7}}
	updates := stickerSetsUpdate(order)
	if len(updates.Updates) != 1 {
		t.Fatalf("updates = %d entries, want 1", len(updates.Updates))
	}
	if _, ok := updates.Updates[0].(*tg.UpdateStickerSetsOrder); !ok {
		t.Fatalf("update = %T, want *tg.UpdateStickerSetsOrder", updates.Updates[0])
	}

	buf := &bin.Buffer{}
	if err := updates.Encode(buf); err != nil {
		t.Fatalf("encode updates: %v", err)
	}
	var decoded tg.Updates
	if err := decoded.Decode(&bin.Buffer{Buf: buf.Buf}); err != nil {
		t.Fatalf("decode updates: %v", err)
	}
	got, ok := decoded.Updates[0].(*tg.UpdateStickerSetsOrder)
	if !ok {
		t.Fatalf("decoded update = %T, want *tg.UpdateStickerSetsOrder", decoded.Updates[0])
	}
	if !got.Masks || got.Emojis || !reflect.DeepEqual(got.Order, []int64{-5, 7}) {
		t.Fatalf("decoded order update = masks %v emojis %v order %v, want masks only with [-5 7]", got.Masks, got.Emojis, got.Order)
	}
}

func TestStickerSetsUpdateWithoutOrderIsPlainInvalidation(t *testing.T) {
	updates := stickerSetsUpdate(nil)
	if _, ok := updates.Updates[0].(*tg.UpdateStickerSets); !ok {
		t.Fatalf("update = %T, want *tg.UpdateStickerSets", updates.Updates[0])
	}
}
