// Command vectors extracts the golden vectors of spec/toolchain/BINDGEN.md (10.1) into
// bindgen/testdata/vectors.json and c/tests/generated/rpc_vectors.h.
//
// Usage, from anywhere inside the repository:
//
//	go run ./bindgen/internal/vectors -write            regenerate vectors.json
//	go run ./bindgen/internal/vectors -check            exit 1 if vectors.json is stale
//	go run ./bindgen/internal/vectors -c FILE           regenerate the C header FILE
//	go run ./bindgen/internal/vectors -check -c FILE    exit 1 if vectors.json or FILE is stale
//
// -write with -c regenerates both files. The document is the only source: the JSON and the header are
// functions of its fenced blocks.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gosuda.org/hqq/bindgen/internal/vectors/extract"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("vectors", flag.ContinueOnError)
	fs.SetOutput(stderr)
	write := fs.Bool("write", false, "write vectors.json")
	check := fs.Bool("check", false, "exit 1 if a generated file differs from the document")
	cOut := fs.String("c", "", "path of the C header to write (or to check with -check)")
	docPath := fs.String("doc", "", "path of BINDGEN.md (default: "+extract.DocRel+" in the repository)")
	jsonPath := fs.String("json", "", "path of vectors.json (default: "+extract.JSONRel+" in the repository)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *write && *check || !*write && !*check && *cOut == "" {
		fmt.Fprintln(stderr, "vectors: give -write, -check or -c FILE (-write and -check exclude each other)")
		return 2
	}
	if err := execute(*write, *check, *cOut, *docPath, *jsonPath, stdout, stderr); err != nil {
		fmt.Fprintln(stderr, "vectors:", err)
		return 1
	}
	return 0
}

func execute(write, check bool, cOut, docPath, jsonPath string, stdout, stderr io.Writer) error {
	if docPath == "" || jsonPath == "" {
		root, err := repoRoot()
		if err != nil {
			return err
		}
		if docPath == "" {
			docPath = filepath.Join(root, filepath.FromSlash(extract.DocRel))
		}
		if jsonPath == "" {
			jsonPath = filepath.Join(root, filepath.FromSlash(extract.JSONRel))
		}
	}
	doc, err := os.ReadFile(docPath)
	if err != nil {
		return err
	}
	f, err := extract.Extract(doc)
	if err != nil {
		return fmt.Errorf("%s: %w", docPath, err)
	}
	js, err := f.Marshal()
	if err != nil {
		return err
	}

	type output struct {
		path string
		data []byte
	}
	var outs []output
	if write || check {
		outs = append(outs, output{jsonPath, js})
	}
	if cOut != "" {
		outs = append(outs, output{cOut, f.CHeader()})
	}

	stale := false
	for _, o := range outs {
		if check {
			cur, err := os.ReadFile(o.path)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if err != nil || !bytes.Equal(cur, o.data) {
				fmt.Fprintf(stderr, "stale: %s; regenerate it with go run ./bindgen/internal/vectors\n", o.path)
				stale = true
				continue
			}
			fmt.Fprintf(stdout, "up to date: %s\n", o.path)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(o.path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(o.path, o.data, 0o644); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "wrote %s\n", o.path)
	}
	if stale {
		return errors.New("generated files are stale")
	}
	return nil
}

// repoRoot walks up from the working directory to the directory that holds go.mod.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("no go.mod above the working directory; pass -doc and -json")
		}
		dir = parent
	}
}
