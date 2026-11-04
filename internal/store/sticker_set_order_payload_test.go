package store_test

import (
	"reflect"
	"testing"

	"github.com/teagramhq/teagram-server/internal/store"
)

func TestStickerSetOrderPayloadRoundTrips(t *testing.T) {
	payload := store.StickerSetOrderChangedPayload(42, 7, 9, &store.StickerSetOrderUpdate{Masks: true, Emojis: true, SetIDs: []int64{3, -4}})
	ownerID, authKeyID, sessionID, order, err := store.ParseStickerSetChangedPayloadForTest(payload)
	if err != nil {
		t.Fatalf("parse order payload %q: %v", payload, err)
	}
	if ownerID != 42 || authKeyID != 7 || sessionID != 9 {
		t.Fatalf("identity = %d|%d|%d, want 42|7|9", ownerID, authKeyID, sessionID)
	}
	want := &store.StickerSetOrderUpdate{Masks: true, Emojis: true, SetIDs: []int64{3, -4}}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("order = %+v, want %+v", order, want)
	}
}

func TestStickerSetChangedPayloadWithoutOrderStillParses(t *testing.T) {
	ownerID, _, _, order, err := store.ParseStickerSetChangedPayloadForTest(store.StickerSetChangedPayload(42, 7, 9))
	if err != nil {
		t.Fatalf("parse plain payload: %v", err)
	}
	if ownerID != 42 || order != nil {
		t.Fatalf("owner = %d order = %+v, want 42 and nil order for a plain invalidation", ownerID, order)
	}
}

func TestStickerSetOrderPayloadRejectsMalformedOrder(t *testing.T) {
	for _, payload := range []string{"42|7|9|1|", "42|7|9|1|3,x", "42|7|9|x|3", "42|7|9|4|3"} {
		if _, _, _, _, err := store.ParseStickerSetChangedPayloadForTest(payload); err == nil {
			t.Errorf("parse %q succeeded, want error", payload)
		}
	}
}
