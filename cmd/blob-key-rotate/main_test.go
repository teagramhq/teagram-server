package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/teagramhq/teagram-server/internal/blobinventory"
	"github.com/teagramhq/teagram-server/internal/blobmode"
)

func TestRequireEncryptedAuthorityRejectsMissingAndPlaintextModes(t *testing.T) {
	t.Parallel()
	for _, authority := range []blobmode.Authority{
		{},
		{Present: true, Outcome: blobmode.OutcomeS3Accepted},
	} {
		if err := requireEncryptedAuthority(authority); err == nil || !strings.Contains(err.Error(), "encrypted blob-mode authority") {
			t.Fatalf("authority %+v error = %v, want encrypted authority requirement", authority, err)
		}
	}
	if err := requireEncryptedAuthority(blobmode.Authority{Present: true, Outcome: blobmode.OutcomeEncrypted}); err != nil {
		t.Fatalf("encrypted authority rejected: %v", err)
	}
}

func TestRequireCurrentTargetRejectsVersionMismatch(t *testing.T) {
	t.Parallel()
	if err := requireCurrentTarget(2, 1); err == nil || !strings.Contains(err.Error(), "must match current media key version 2") {
		t.Fatalf("target mismatch error = %v, want current version requirement", err)
	}
	if err := requireCurrentTarget(2, 2); err != nil {
		t.Fatalf("current target rejected: %v", err)
	}
}

func TestRequireReferencesUnchangedRejectsPartCommittedDuringInventory(t *testing.T) {
	t.Parallel()

	// The new part row commits while the full-tree scan is running.
	beforeScan := []blobinventory.Reference{
		{Key: "files/21", Kind: "file", ID: "21", LogicalBytes: 5},
	}
	partKey := "parts/00000000000000000000000000000000"
	afterScan := []blobinventory.Reference{
		{Key: "files/21", Kind: "file", ID: "21", LogicalBytes: 5},
		{Key: partKey, Kind: "part", ID: partKey, LogicalBytes: 7},
	}

	err := requireReferencesUnchanged(beforeScan, afterScan)
	if err == nil || !strings.Contains(err.Error(), "references changed during final inventory") {
		t.Fatalf("reference reconciliation error = %v, want fail-closed drift error", err)
	}
}

func TestCommandResultEncodesRotationAndInventoryForOperators(t *testing.T) {
	t.Parallel()
	result := commandResult{
		Mode: "rewrap", KeyVersion: 2, Objects: 4, Rotated: 4, LogicalBytes: 91,
		Inventory: blobinventory.Report{Summary: "key versions 1=retirable, 2=not retirable"},
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("encode command result: %v", err)
	}
	var got struct {
		Mode       string               `json:"mode"`
		KeyVersion uint32               `json:"key_version"`
		Objects    int                  `json:"objects"`
		Inventory  blobinventory.Report `json:"inventory"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode command result: %v", err)
	}
	if got.Mode != "rewrap" || got.KeyVersion != 2 || got.Objects != 4 || got.Inventory.Summary == "" {
		t.Fatalf("encoded result = %+v, want rotation counts and inventory summary", got)
	}
}
