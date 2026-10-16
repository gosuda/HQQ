package conformance

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// treeFiles returns every regular file under root as a slash path mapped to
// its bytes.
func treeFiles(t *testing.T, root string) map[string][]byte {
	t.Helper()
	out := make(map[string][]byte)
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = b
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

// TestGenerateDeterministic (G-08 image bytes, section 2 of the plan) generates
// the vector tree twice into separate directories and requires byte-identical
// trees.
func TestGenerateDeterministic(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	if err := Generate(a); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if err := Generate(b); err != nil {
		t.Fatalf("generate again: %v", err)
	}
	ta, tb := treeFiles(t, a), treeFiles(t, b)
	if len(ta) == 0 {
		t.Fatal("generation wrote no files")
	}
	for p, want := range ta {
		got, ok := tb[p]
		if !ok {
			t.Errorf("second generation lacks %s", p)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs between two generations", p)
		}
	}
	for p := range tb {
		if _, ok := ta[p]; !ok {
			t.Errorf("second generation has extra file %s", p)
		}
	}
}

// TestCommittedVectorsCurrent requires the committed vectors to equal a fresh
// generation. Golden sequences exist per page family: a family that the
// committed tree does not carry is not compared (it was produced on another
// host), and families present in the committed tree must match.
func TestCommittedVectorsCurrent(t *testing.T) {
	fresh := t.TempDir()
	if err := Generate(fresh); err != nil {
		t.Fatalf("generate: %v", err)
	}
	const regen = "run `go run ./cmd/hqq-interop gen-vectors --out conformance/vectors`"
	got := treeFiles(t, fresh)
	committed := treeFiles(t, vectorsDir)
	hostSeq := "sequences/" + strconv.Itoa(hostPage()) + "/"
	hostCommitted := false
	for p := range committed {
		if strings.HasPrefix(p, hostSeq) {
			hostCommitted = true
		}
	}
	for p, want := range got {
		if strings.HasPrefix(p, "sequences/") && !strings.HasPrefix(p, hostSeq) {
			continue
		}
		if strings.HasPrefix(p, hostSeq) && !hostCommitted {
			continue
		}
		have, ok := committed[p]
		if !ok {
			t.Errorf("committed vectors lack %s; %s", p, regen)
			continue
		}
		if !bytes.Equal(have, want) {
			t.Errorf("committed %s differs from a fresh generation (first difference at byte %d); %s",
				p, firstDiffOutside(have, want, 0, 0), regen)
		}
	}
	for p := range committed {
		if strings.HasPrefix(p, "sequences/") {
			continue // other page families are produced on their hosts
		}
		if _, ok := got[p]; !ok {
			t.Errorf("committed vectors carry %s, which the generator no longer produces; %s", p, regen)
		}
	}
}
