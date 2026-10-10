package prewarmdiag

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/teagramhq/teagram-server/internal/pgtest"
)

const (
	prewarmFailureMarker = "e2e setup failed [setup:pgtest-prewarm]"
	provenanceContent    = "pgtest-prewarm-provenance-v1:"
	provenanceOutput     = "e2e setup provenance [pgtest-prewarm:%s]\n"
	provenanceSuffix     = ".proof"
)

// ProvenanceDirectoryEnv names the per-run directory used to attest a prewarm
// failure to the diagnostic reporter.
const ProvenanceDirectoryEnv = "TEAGRAM_E2E_PREWARM_PROVENANCE_DIR"

// ReportFailure writes the fixed marker and detail on failure. Only errors
// returned by pgtest.Prewarm receive a provenance sidecar.
func ReportFailure(out io.Writer, provenanceDir string, err error) int {
	if err == nil {
		return 0
	}
	if _, writeErr := fmt.Fprintln(out, prewarmFailureMarker); writeErr != nil {
		return 1
	}
	if pgtest.IsPrewarmFailure(err) {
		if nonce, proofErr := writeProvenance(provenanceDir); proofErr == nil {
			if _, writeErr := fmt.Fprintf(out, provenanceOutput, nonce); writeErr != nil {
				return 1
			}
		}
	}
	if _, writeErr := fmt.Fprintf(out, "pgtest prewarm: %v\n", err); writeErr != nil {
		return 1
	}
	return 1
}

func writeProvenance(dir string) (nonce string, resultErr error) {
	if dir == "" {
		return "", os.ErrNotExist
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", err
	}
	defer func() {
		if closeErr := root.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr, closeErr)
		}
	}()

	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	nonce = hex.EncodeToString(random[:])
	file, err := root.OpenFile(nonce+provenanceSuffix, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := fmt.Fprintf(file, "%s%s\n", provenanceContent, nonce); err != nil {
		if closeErr := file.Close(); closeErr != nil {
			return "", errors.Join(err, closeErr)
		}
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	return nonce, nil
}
