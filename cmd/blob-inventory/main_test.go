package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/teagramhq/teagram-server/internal/blobinventory"
)

func TestWriteReportEmitsMachineReadableJSONAndHumanSummary(t *testing.T) {
	t.Parallel()
	wantSummary := "media inventory: valid envelope=2"
	report := blobinventory.Report{
		Summary: wantSummary,
		Classes: blobinventory.Classes{
			ValidEnvelope: blobinventory.Class{Count: 2, LogicalBytes: 10, PhysicalBytes: 246},
		},
		KeyVersions: []blobinventory.KeyVersion{{
			Version:           1,
			Objects:           2,
			ReferencedObjects: 2,
			Retirable:         false,
		}},
	}
	var stdout, stderr bytes.Buffer
	if err := writeReport(&stdout, &stderr, report); err != nil {
		t.Fatalf("write report: %v", err)
	}
	var decoded blobinventory.Report
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("stdout is not JSON: %v", err)
	}
	if decoded.Classes.ValidEnvelope.Count != 2 || decoded.KeyVersions[0].ReferencedObjects != 2 {
		t.Fatalf("decoded report = %+v, want counts in JSON", decoded)
	}
	if !strings.Contains(stderr.String(), wantSummary) {
		t.Fatalf("stderr = %q, want human summary", stderr.String())
	}
}
