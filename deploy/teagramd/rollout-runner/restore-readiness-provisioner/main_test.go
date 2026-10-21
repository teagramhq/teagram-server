package main

import (
	"strings"
	"testing"
)

func TestDatabaseDSNIsRestrictedToTheScratchLoopbackTarget(t *testing.T) {
	valid := []byte("postgres://postgres:" + "a5" + repeatHex("bc", 31) + "@127.0.0.1:5432/teagram?sslmode=disable")
	if !dsnPattern.Match(valid) {
		t.Fatal("loopback scratch DSN was rejected")
	}
	for _, candidate := range []string{
		"postgres://postgres:secret@database:5432/teagram?sslmode=disable",
		"postgres://postgres:" + "a5" + repeatHex("bc", 31) + "@198.51.100.2:5432/teagram?sslmode=disable",
		"postgres://postgres:" + "a5" + repeatHex("bc", 31) + "@127.0.0.1:5433/teagram?sslmode=disable",
		"postgres://postgres:" + "a5" + repeatHex("bc", 31) + "@127.0.0.1:5432/other?sslmode=disable",
	} {
		if dsnPattern.MatchString(candidate) {
			t.Fatalf("untrusted DSN accepted: %s", candidate)
		}
	}
}

func TestCreateUserConfirmationMustExactlyBindTheSyntheticName(t *testing.T) {
	const username = "fixture_0123456789abcdef"
	id, ok := parseCreateUserConfirmation([]byte("User created: "+username+" (user id: 314)\n"), []byte(username))
	if !ok || id != 314 {
		t.Fatalf("valid create-user confirmation = %d/%t, want 314/true", id, ok)
	}
	for _, confirmation := range []string{
		"User created: fixture_ffffffffffffffff (user id: 314)\n",
		"User created: " + username + " (user id: 0)\n",
		"User created: " + username + " (user id: 314)\nextra diagnostics",
		"raw-password-value",
	} {
		if _, ok := parseCreateUserConfirmation([]byte(confirmation), []byte(username)); ok {
			t.Fatalf("unexpected create-user output accepted: %q", confirmation)
		}
	}
}

func repeatHex(pair string, count int) string {
	return strings.Repeat(pair, count)
}
