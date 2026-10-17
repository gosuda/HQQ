package idl_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"gosuda.org/hqq/bindgen/idl"
)

type vectorFile map[string]struct {
	IDL       string `json:"idl"`
	Canonical string `json:"canonical"`
	Digest    string `json:"digest"`
}

func loadVectors(t *testing.T) vectorFile {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "testdata", "vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v vectorFile
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func parseFile(t *testing.T, rel string, opts idl.Options) *idl.Spec {
	t.Helper()
	path := filepath.Join("..", "testdata", filepath.FromSlash(rel))
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := idl.Parse(rel, src, opts)
	if err != nil {
		t.Fatalf("parse %s:\n%v", rel, err)
	}
	return spec
}

// TestDocumentIDLFiles is the acceptance test of 11.2: both worked examples give the canonical text and the
// digest that the document prints (the vectors of 10.1).
func TestDocumentIDLFiles(t *testing.T) {
	vec := loadVectors(t)
	for _, name := range []string{"microlib", "sqlite_lowlevel"} {
		t.Run(name, func(t *testing.T) {
			v, ok := vec[name]
			if !ok {
				t.Fatalf("vectors.json has no %q", name)
			}
			spec := parseFile(t, v.IDL, idl.Options{})
			if got := string(spec.Canonical()); got != v.Canonical {
				t.Errorf("canonical text differs:\n got:\n%s\nwant:\n%s", got, v.Canonical)
			}
			d := spec.Digest()
			if got := hex.EncodeToString(d[:]); got != v.Digest {
				t.Errorf("digest %s, want %s", got, v.Digest)
			}
			if sum := sha256.Sum256(spec.Canonical()); sum != d {
				t.Error("Digest is not the SHA-256 of Canonical")
			}
		})
	}
}

// TestAuxiliaryIDLFiles: microlib_v2.idl is an incompatible revision (a different digest, the
// digest-mismatch test of 10.4) and microbench.idl declares ml_work.
func TestAuxiliaryIDLFiles(t *testing.T) {
	v1 := parseFile(t, "microlib/microlib.idl", idl.Options{})
	v2 := parseFile(t, "microlib/microlib_v2.idl", idl.Options{})
	if v1.Digest() == v2.Digest() {
		t.Fatal("microlib_v2.idl has the digest of microlib.idl")
	}
	if len(v1.Wire) != len(v2.Wire) || v2.Service != "microlib" {
		t.Error("microlib_v2.idl differs from microlib.idl by more than one parameter type")
	}
	add := v2.Func("ml_add")
	if add.Params[1].Type.Kind != idl.KI64 || v1.Func("ml_add").Params[1].Type.Kind != idl.KI32 {
		t.Error("ml_add parameter b is i32 in v1 and i64 in v2")
	}

	bench := parseFile(t, "microlib/microbench.idl", idl.Options{GoStrip: "ml_"})
	work := bench.Func("ml_work")
	if work == nil || work.Opcode != 2 || work.Ret.Kind != idl.KU64 || len(work.Params) != 1 || work.Params[0].Type.Kind != idl.KU32 {
		t.Fatalf("ml_work: %+v", work)
	}
	if got := dumpLayout(work.Plan.Req); got != "iters@0:u32 size=4 pad=8" {
		t.Errorf("request layout %s", got)
	}
	if got := dumpLayout(work.Plan.Resp); got != "ret@0:u64 size=8 pad=8" {
		t.Errorf("response layout %s", got)
	}
}
