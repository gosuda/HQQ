package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const microlibIDL = "../../bindgen/testdata/microlib/microlib.idl"

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestGenerateThenCheck(t *testing.T) {
	dir := t.TempDir()
	goDir, cDir := filepath.Join(dir, "go"), filepath.Join(dir, "c")
	gen := []string{"-go-out", goDir, "-go-strip", "ml_", "-c-out", cDir, microlibIDL}

	if code, _, stderr := runCLI(t, append([]string{"-check"}, gen...)...); code != exitFail ||
		!strings.Contains(stderr, "microlib_client.go is missing") {
		t.Fatalf("check on empty directories: code %d, stderr %q", code, stderr)
	}
	if code, _, stderr := runCLI(t, gen...); code != exitOK {
		t.Fatalf("generate: code %d, stderr %q", code, stderr)
	}
	for _, name := range []string{
		filepath.Join(goDir, "microlib_client.go"),
		filepath.Join(cDir, "microlib_server.h"),
		filepath.Join(cDir, "microlib_server.c"),
	} {
		if _, err := os.Stat(name); err != nil {
			t.Fatalf("expected output: %v", err)
		}
	}
	if code, _, stderr := runCLI(t, append([]string{"-check"}, gen...)...); code != exitOK {
		t.Fatalf("check after generate: code %d, stderr %q", code, stderr)
	}

	// A stale file is named and fails the check; generating repairs it.
	stale := filepath.Join(cDir, "microlib_server.c")
	if err := os.WriteFile(stale, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runCLI(t, append([]string{"-check"}, gen...)...)
	if code != exitFail || !strings.Contains(stderr, stale+" is out of date") {
		t.Fatalf("check with a stale file: code %d, stderr %q", code, stderr)
	}
	if code, _, _ := runCLI(t, gen...); code != exitOK {
		t.Fatalf("regenerate: code %d", code)
	}
	if code, _, _ := runCLI(t, append([]string{"-check"}, gen...)...); code != exitOK {
		t.Fatalf("check after regenerate: code %d", code)
	}
}

func TestCheckWritesNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "never")
	if code, _, _ := runCLI(t, "-check", "-go-out", dir, "-go-strip", "ml_", microlibIDL); code != exitFail {
		t.Fatalf("code %d, want %d", code, exitFail)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("-check created %s (err %v)", dir, err)
	}
}

func TestPrintDigestAndCanonical(t *testing.T) {
	code, out, stderr := runCLI(t, "-print-digest", microlibIDL)
	if code != exitOK || len(strings.TrimSuffix(out, "\n")) != 64 {
		t.Fatalf("print-digest: code %d, out %q, stderr %q", code, out, stderr)
	}
	code, canon, stderr := runCLI(t, "-print-canonical", microlibIDL)
	if code != exitOK || canon == "" {
		t.Fatalf("print-canonical: code %d, stderr %q", code, stderr)
	}
}

func TestInvalidIDLReportsPositions(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "bad.idl")
	if err := os.WriteFile(bad, []byte("service bad;\nfunc ;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runCLI(t, "-print-digest", bad)
	if code != exitFail || !strings.Contains(stderr, "bad.idl:") {
		t.Fatalf("code %d, stderr %q", code, stderr)
	}
}

func TestUsageErrors(t *testing.T) {
	cases := map[string][]string{
		"no file":         {"-print-digest"},
		"nothing to do":   {microlibIDL},
		"exclusive print": {"-print-digest", "-print-canonical", microlibIDL},
		"missing input":   {"-print-digest", filepath.Join(t.TempDir(), "absent.idl")},
		"unknown flag":    {"-bogus", microlibIDL},
		"two input files": {"-print-digest", microlibIDL, microlibIDL},
	}
	for name, args := range cases {
		if code, _, _ := runCLI(t, args...); code != exitUsage {
			t.Errorf("%s: code %d, want %d", name, code, exitUsage)
		}
	}
}
