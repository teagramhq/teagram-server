package srp

import "math/big"

// Test-only accessors into unexported internals, kept in a _test.go file so they
// never ship in the production build.

// Pad exposes pad for padding-exactness tests.
func Pad(b []byte) []byte { return pad(b) }

// Prime returns a copy of the modulus p for tests.
func Prime() *big.Int { return new(big.Int).Set(p) }
