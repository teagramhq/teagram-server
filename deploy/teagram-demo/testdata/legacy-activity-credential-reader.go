// This fixture preserves the pre-change activity image's 0400-only inventory
// read, so the scheduling harness can exercise the real legacy permission rule.
package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"syscall"
)

func readLegacyCredential(path string) (string, error) {
	info, err := os.Lstat(path) // #nosec G703 -- the harness passes a file under its private temporary directory.
	if err != nil {
		return "", errors.New("activity credential inventory is unavailable")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int64(stat.Uid) != int64(os.Geteuid()) || !info.Mode().IsRegular() || info.Mode().Perm() != 0400 || info.Size() > 64*1024 {
		return "", errors.New("activity credential inventory must be an owned regular mode 0400 file")
	}
	data, err := os.ReadFile(path) // #nosec G304,G703 -- path is a test fixture under the private temporary directory.
	if err != nil {
		return "", errors.New("activity credential inventory is unavailable")
	}
	var inventory map[string]string
	if err := json.Unmarshal(data, &inventory); err != nil {
		return "", errors.New("activity credential inventory is invalid")
	}
	password, ok := inventory["demo_ren"]
	if !ok || len(password) != 64 {
		return "", errors.New("activity account credential is missing or invalid")
	}
	if _, err := hex.DecodeString(password); err != nil {
		return "", errors.New("activity account credential is missing or invalid")
	}
	return password, nil
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: legacy-activity-credential-reader <inventory>")
		os.Exit(2)
	}
	if _, err := readLegacyCredential(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("legacy activity credential read")
}
