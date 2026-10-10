//nolint:testpackage // The private error type is the setup-provenance capability.
package pgtest

import (
	"errors"
	"fmt"
	"testing"
)

func TestPrewarmFailureProvenance(t *testing.T) {
	cause := errors.New("synthetic prewarm error")
	failure := prewarmError{cause: cause}

	if !IsPrewarmFailure(failure) {
		t.Fatal("typed prewarm error was not recognized")
	}
	if !IsPrewarmFailure(fmt.Errorf("wrapped: %w", failure)) {
		t.Fatal("wrapped typed prewarm error was not recognized")
	}
	if IsPrewarmFailure(cause) {
		t.Fatal("ordinary error was recognized as a prewarm error")
	}
	if !errors.Is(failure, cause) {
		t.Fatal("typed prewarm error does not preserve its cause")
	}
}
