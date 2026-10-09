package prewarmdiag

import (
	"fmt"
	"io"
)

const prewarmFailureMarker = "e2e setup failed [setup:pgtest-prewarm]"

// ReportFailure writes the fixed prewarm marker and existing detail on failure.
func ReportFailure(out io.Writer, err error) int {
	if err == nil {
		return 0
	}
	if _, writeErr := fmt.Fprintln(out, prewarmFailureMarker); writeErr != nil {
		return 1
	}
	if _, writeErr := fmt.Fprintf(out, "pgtest prewarm: %v\n", err); writeErr != nil {
		return 1
	}
	return 1
}
