package mtproto

import (
	"strconv"
	"testing"
)

func TestAuthorizationMetadataAPIID(t *testing.T) {
	type testCase struct {
		name  string
		value int
		want  int32
	}
	tests := []testCase{
		{name: "minimum", value: -1 << 31, want: -1 << 31},
		{name: "maximum", value: 1<<31 - 1, want: 1<<31 - 1},
		{name: "ordinary", value: 123, want: 123},
	}
	if strconv.IntSize == 64 {
		tests = append(tests,
			testCase{name: "above maximum", value: int(int64(1 << 31)), want: 0},
			testCase{name: "below minimum", value: int(int64(-1<<31) - 1), want: 0},
		)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := authorizationMetadataAPIID(tt.value); got != tt.want {
				t.Fatalf("authorizationMetadataAPIID(%d) = %d, want %d", tt.value, got, tt.want)
			}
		})
	}
}
