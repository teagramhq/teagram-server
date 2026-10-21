package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestValidDSNIsLimitedToUnixSocketScratchDatabase(t *testing.T) {
	valid := fmt.Sprintf("postgres://postgres:%064x@/teagram?host=/run/postgres&sslmode=disable", 1)
	if !validDSN(valid) {
		t.Fatal("generated scratch database DSN was rejected")
	}
	for _, dsn := range []string{
		"postgres://postgres@/teagram?host=/run/postgres&sslmode=disable",
		"postgres://postgres:secret@/teagram?host=/run/postgres&sslmode=disable",
		"postgres://postgres:" + fmt.Sprintf("%064x", 1) + "@database/teagram?sslmode=disable",
		"postgres://postgres:" + fmt.Sprintf("%064x", 1) + "@/other?host=/run/postgres&sslmode=disable",
		"postgres://postgres:" + fmt.Sprintf("%064x", 1) + "@/teagram?host=/tmp&sslmode=disable",
		valid + "&application_name=unreviewed",
	} {
		if validDSN(dsn) {
			t.Fatal("accepted a database target outside the scratch socket")
		}
	}
}

func TestReadProtectedRequiresOwnedRegularPrivateFile(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "secret")
	if err := os.WriteFile(path, []byte("fixture\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	contents, err := readProtected(path, 0o400, 16)
	if err != nil || string(contents) != "fixture\n" {
		t.Fatal("owned private fixture file was rejected")
	}

	symlink := filepath.Join(directory, "linked-secret")
	if err := os.Symlink(path, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := readProtected(symlink, 0o400, 16); err == nil {
		t.Fatal("symlinked secret input was accepted")
	}
}

func TestSyntheticCredentialPatternsRejectOtherIdentities(t *testing.T) {
	if !usernameRE.MatchString("fixture_0123456789abcdef") || !authKeyRE.MatchString(fmt.Sprintf("%064x", 1)) {
		t.Fatal("generated synthetic identity formats were rejected")
	}
	for _, username := range []string{"real_user", "fixture_0123456789abcdeg", "fixture_short"} {
		if usernameRE.MatchString(username) {
			t.Fatal("accepted a non-synthetic username")
		}
	}
}
