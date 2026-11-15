//nolint:testpackage // Exercise file metadata validation without root-owned fixtures.
package blobmode

import (
	"errors"
	"testing"

	"golang.org/x/sys/unix"
)

func TestValidateStat(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		stat       unix.Stat_t
		kind       uint32
		uid        uint32
		wantReason string
	}{
		{name: "regular file", stat: unix.Stat_t{Mode: unix.S_IFREG | 0o644, Uid: 1000}, kind: unix.S_IFREG, uid: 1000},
		{name: "directory", stat: unix.Stat_t{Mode: unix.S_IFDIR | 0o755, Uid: 1000}, kind: unix.S_IFDIR, uid: 1000},
		{name: "special permission bits", stat: unix.Stat_t{Mode: unix.S_IFREG | 0o4644, Uid: 1000}, kind: unix.S_IFREG, uid: 1000},
		{name: "wrong file type", stat: unix.Stat_t{Mode: unix.S_IFREG | 0o644, Uid: 1000}, kind: unix.S_IFDIR, uid: 1000, wantReason: "invalid"},
		{name: "wrong owner", stat: unix.Stat_t{Mode: unix.S_IFREG | 0o644, Uid: 1001}, kind: unix.S_IFREG, uid: 1000, wantReason: "ownership"},
		{name: "group writable", stat: unix.Stat_t{Mode: unix.S_IFREG | 0o664, Uid: 1000}, kind: unix.S_IFREG, uid: 1000, wantReason: "permissions"},
		{name: "world writable", stat: unix.Stat_t{Mode: unix.S_IFREG | 0o646, Uid: 1000}, kind: unix.S_IFREG, uid: 1000, wantReason: "permissions"},
		{name: "wide kind", stat: unix.Stat_t{Mode: unix.S_IFREG | 0o644, Uid: 1000}, kind: 0x10000 | unix.S_IFREG, uid: 1000, wantReason: "invalid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateStat(&tt.stat, tt.kind, tt.uid, "record")
			if tt.wantReason == "" {
				if err != nil {
					t.Fatalf("validate stat: %v", err)
				}
				return
			}
			var validationErr *validationError
			if !errors.As(err, &validationErr) || validationErr.reason != tt.wantReason || validationErr.field != "record" {
				t.Fatalf("validate stat = %v, want %s rejection for record", err, tt.wantReason)
			}
		})
	}
}
