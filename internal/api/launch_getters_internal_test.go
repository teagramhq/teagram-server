package api

import "testing"

func TestPaginateFeaturedStickerSets(t *testing.T) {
	tests := []struct {
		name   string
		count  int
		offset int
		limit  int
		want   []int
	}{
		{name: "empty set", count: 0, offset: 0, limit: 1},
		{name: "single set", count: 1, offset: 0, limit: 1, want: []int{0}},
		{name: "two sets first page", count: 2, offset: 0, limit: 1, want: []int{0}},
		{name: "two sets second page", count: 2, offset: 1, limit: 1, want: []int{1}},
		{name: "three sets middle page", count: 3, offset: 1, limit: 1, want: []int{1}},
		{name: "three sets final page", count: 3, offset: 2, limit: 2, want: []int{2}},
		{name: "limit extends past end", count: 3, offset: 1, limit: 3, want: []int{1, 2}},
		{name: "offset at end", count: 3, offset: 3, limit: 1},
		{name: "offset past end", count: 3, offset: 4, limit: 1},
		{name: "zero limit", count: 3, offset: 0, limit: 0},
		{name: "negative limit", count: 3, offset: 0, limit: -1},
		{name: "negative offset clamps to zero", count: 3, offset: -1, limit: 2, want: []int{0, 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sets := make([]*builtInStickerSet, tt.count)
			for i := range sets {
				sets[i] = &builtInStickerSet{}
			}
			got := paginateFeaturedStickerSets(sets, tt.offset, tt.limit)
			if len(got) != len(tt.want) {
				t.Fatalf("paginateFeaturedStickerSets(%d sets, %d, %d) returned %d sets, want %d", tt.count, tt.offset, tt.limit, len(got), len(tt.want))
			}
			for i, wantIndex := range tt.want {
				if got[i] != sets[wantIndex] {
					t.Errorf("page item %d = %p, want set %d (%p)", i, got[i], wantIndex, sets[wantIndex])
				}
			}
		})
	}
}
