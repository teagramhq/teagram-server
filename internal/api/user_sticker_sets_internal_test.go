package api

import (
	"reflect"
	"testing"
	"time"

	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/store"
)

var userStickerSetSummarySink tg.StickerSet

func TestUserStickerSetListSummaryAvoidsItemProjectionAllocations(t *testing.T) {
	installedAt := time.Unix(1_720_000_000, 0)
	set := store.UserStickerSet{
		ID: 42, AccessHash: 73, CreatorID: 9, Title: "Stars", ShortName: "stars",
		Kind: "emojis", TextColor: true, ItemCount: 200, Hash: 123,
	}
	items := make([]store.UserStickerSetItem, 200)
	for i := range items {
		items[i] = store.UserStickerSetItem{
			Position: i, FileID: int64(i + 1), AccessHash: int64(i + 1000), Size: 32,
			Date: installedAt, Emoji: "⭐", Keywords: "star,glow", Format: "webp",
			Width: 100, Height: 100,
		}
	}
	large := set
	large.Items = items

	summaryAllocs := testing.AllocsPerRun(50, func() {
		userStickerSetSummarySink = userStickerSetSummaryTL(large, 9, &installedAt)
	})
	fullAllocs := testing.AllocsPerRun(50, func() {
		userStickerSetSummarySink = userStickerSetTL(large, 9, 7, &installedAt).Set
	})
	if summaryAllocs*2 >= fullAllocs {
		t.Fatalf("list summary allocated %.0f times for 200 items, not less than half the full projection's %.0f", summaryAllocs, fullAllocs)
	}

	want := userStickerSetTL(large, 9, 7, &installedAt).Set
	got := userStickerSetSummaryTL(large, 9, &installedAt)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("summary projection = %+v, want full set summary %+v", got, want)
	}
}
