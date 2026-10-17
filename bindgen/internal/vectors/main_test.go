package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"gosuda.org/hqq/bindgen/internal/vectors/extract"
)

func TestRunModes(t *testing.T) {
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "vectors.json")
	hdr := filepath.Join(dir, "gen", "rpc_vectors.h")
	doc := filepath.Join("..", "..", "..", filepath.FromSlash(extract.DocRel))
	args := func(extra ...string) []string {
		return append([]string{"-doc", doc, "-json", jsonPath}, extra...)
	}
	var out, errb bytes.Buffer
	if rc := run(args("-check"), &out, &errb); rc != 1 {
		t.Errorf("-check without a file: rc %d, want 1", rc)
	}
	if rc := run(args("-write", "-c", hdr), &out, &errb); rc != 0 {
		t.Fatalf("-write: rc %d: %s", rc, errb.String())
	}
	if rc := run(args("-check", "-c", hdr), &out, &errb); rc != 0 {
		t.Fatalf("-check after -write: rc %d: %s", rc, errb.String())
	}
	if err := os.WriteFile(hdr, []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rc := run(args("-check", "-c", hdr), &out, &errb); rc != 1 {
		t.Errorf("-check with a stale header: rc %d, want 1", rc)
	}
	if rc := run(args(), &out, &errb); rc != 2 {
		t.Errorf("no mode: rc %d, want 2", rc)
	}
	if rc := run(args("-write", "-check"), &out, &errb); rc != 2 {
		t.Errorf("-write -check: rc %d, want 2", rc)
	}
}
