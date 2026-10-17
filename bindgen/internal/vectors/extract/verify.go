package extract

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// VerifyCommitted re-extracts the vectors from the document under root (the repository root) and reports
// every committed generated file that differs: bindgen/testdata/vectors.json and
// c/tests/generated/rpc_vectors.h. It is the check behind `go test ./bindgen -run TestDocVectors`.
func VerifyCommitted(root string) error {
	doc, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(DocRel)))
	if err != nil {
		return err
	}
	f, err := Extract(doc)
	if err != nil {
		return fmt.Errorf("%s: %w", DocRel, err)
	}
	js, err := f.Marshal()
	if err != nil {
		return err
	}
	var stale []error
	for _, o := range []struct {
		rel  string
		want []byte
		fix  string
	}{
		{JSONRel, js, "go run ./bindgen/internal/vectors -write"},
		{CHeaderRel, f.CHeader(), "go run ./bindgen/internal/vectors -c " + CHeaderRel},
	} {
		got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(o.rel)))
		if err != nil {
			return err
		}
		if !bytes.Equal(got, o.want) {
			stale = append(stale, fmt.Errorf("%s is stale: run %s", o.rel, o.fix))
		}
	}
	return errors.Join(stale...)
}
