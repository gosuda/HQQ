package bindgen

import (
	"testing"

	"gosuda.org/hqq/bindgen/internal/vectors/extract"
)

// TestDocVectors re-extracts the vector blocks of spec/toolchain/BINDGEN.md (10.1) and fails if
// testdata/vectors.json or c/tests/generated/rpc_vectors.h differs. `go run ./bindgen/internal/vectors
// -write -c c/tests/generated/rpc_vectors.h` regenerates both.
func TestDocVectors(t *testing.T) {
	if err := extract.VerifyCommitted(".."); err != nil {
		t.Error(err)
	}
}
