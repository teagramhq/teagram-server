package main

import (
	"fmt"
	"testing"
)

func TestValidDatabaseDSN(t *testing.T) {
	dsn := fmt.Sprintf("postgres://postgres:%064x@/teagram?host=/run/postgres&sslmode=disable", 1)
	if !validDatabaseDSN(dsn) {
		t.Fatal("generated scratch database DSN was rejected")
	}
}

func TestRejectsDatabaseDSNsOutsideScratchTarget(t *testing.T) {
	valid := fmt.Sprintf("postgres://postgres:%064x@/teagram?host=/run/postgres&sslmode=disable", 1)
	for _, dsn := range []string{
		"postgres://postgres@/teagram?host=/run/postgres&sslmode=disable",
		"postgres://postgres:secret@/teagram?host=/run/postgres&sslmode=disable",
		"postgres://postgres:" + fmt.Sprintf("%064x", 1) + "@database/teagram?sslmode=disable",
		"postgres://postgres:" + fmt.Sprintf("%064x", 1) + "@/other?host=/run/postgres&sslmode=disable",
		"postgres://postgres:" + fmt.Sprintf("%064x", 1) + "@/teagram?host=/tmp&sslmode=disable",
		valid + "&application_name=unreviewed",
	} {
		if validDatabaseDSN(dsn) {
			t.Fatalf("accepted an unreviewed database target")
		}
	}
}
