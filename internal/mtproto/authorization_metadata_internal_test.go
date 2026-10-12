package mtproto

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestBoundedAuthorizationString(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "removes NUL bytes", input: "before\x00after", want: "beforeafter"},
		{name: "removes invalid UTF-8", input: string([]byte{'a', 0xff, 'b'}), want: "ab"},
		{
			name:  "truncates at a rune boundary",
			input: strings.Repeat("a", maxAuthorizationMetadataBytes-1) + "éz",
			want:  strings.Repeat("a", maxAuthorizationMetadataBytes-1),
		},
		{
			name:  "caps one megabyte input",
			input: strings.Repeat("x", 1<<20),
			want:  strings.Repeat("x", maxAuthorizationMetadataBytes),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := boundedAuthorizationString(tt.input)
			if got != tt.want {
				t.Fatalf("boundedAuthorizationString() length=%d, want length=%d", len(got), len(tt.want))
			}
			if !utf8.ValidString(got) {
				t.Fatal("boundedAuthorizationString() returned invalid UTF-8")
			}
			if len(got) > maxAuthorizationMetadataBytes {
				t.Fatalf("boundedAuthorizationString() returned %d bytes, limit is %d", len(got), maxAuthorizationMetadataBytes)
			}
		})
	}
}
