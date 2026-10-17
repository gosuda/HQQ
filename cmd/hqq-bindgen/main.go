// Command hqq-bindgen generates a Go client and a C server from an IDL file, so
// that Go can call a C library over HQQ shared memory without cgo. The IDL, the
// generated code and the runtimes are specified in spec/toolchain/BINDGEN.md.
//
//	hqq-bindgen [flags] file.idl
//	  -go-out DIR         write <service>_client.go into DIR
//	  -go-package NAME    package clause of the Go file (default: the service name)
//	  -go-strip PREFIX    remove PREFIX from every function name before CamelCasing
//	  -c-out DIR          write <service>_server.h and <service>_server.c into DIR
//	  -check              write nothing; exit 1 and name the files if any output differs
//	  -print-canonical    print the canonical text and exit
//	  -print-digest       print the SHA-256 of the canonical text as 64 hex digits and exit
//
// A typical consumer wires it with go:generate and runs the same command with
// -check in CI, the pattern of c/tools/gen_constants.py --check.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gosuda.org/hqq/bindgen/gen"
	"gosuda.org/hqq/bindgen/idl"
)

// Exit statuses.
const (
	exitOK    = 0
	exitFail  = 1 // invalid IDL, generation failed, or -check found differences
	exitUsage = 2 // bad command line or an I/O error
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hqq-bindgen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: hqq-bindgen [flags] file.idl")
		fs.PrintDefaults()
	}
	goOut := fs.String("go-out", "", "write <service>_client.go into `DIR`")
	goPackage := fs.String("go-package", "", "package clause of the Go file (default: the service name)")
	goStrip := fs.String("go-strip", "", "remove `PREFIX` from every function name before CamelCasing")
	cOut := fs.String("c-out", "", "write <service>_server.h and <service>_server.c into `DIR`")
	check := fs.Bool("check", false, "write nothing; exit 1 and name the files if any output differs")
	printCanonical := fs.Bool("print-canonical", false, "print the canonical text and exit")
	printDigest := fs.Bool("print-digest", false, "print the SHA-256 of the canonical text as 64 hex digits and exit")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return exitUsage
	}
	if *printCanonical && *printDigest {
		fmt.Fprintln(stderr, "hqq-bindgen: -print-canonical and -print-digest are exclusive")
		return exitUsage
	}
	generating := *goOut != "" || *cOut != ""
	if !*printCanonical && !*printDigest && !generating {
		fmt.Fprintln(stderr, "hqq-bindgen: nothing to do: give -go-out, -c-out, -print-canonical or -print-digest")
		fs.Usage()
		return exitUsage
	}

	path := fs.Arg(0)
	src, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(stderr, "hqq-bindgen:", err)
		return exitUsage
	}
	spec, err := idl.Parse(path, src, idl.Options{GoStrip: *goStrip})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitFail
	}

	switch {
	case *printCanonical:
		if _, err := stdout.Write(spec.Canonical()); err != nil {
			fmt.Fprintln(stderr, "hqq-bindgen:", err)
			return exitUsage
		}
		return exitOK
	case *printDigest:
		d := spec.Digest()
		fmt.Fprintf(stdout, "%x\n", d[:])
		return exitOK
	}

	var out []output
	if *goOut != "" {
		files, err := gen.Go(spec, gen.GoOptions{Package: *goPackage, Strip: *goStrip, Source: path})
		if err != nil {
			fmt.Fprintln(stderr, "hqq-bindgen:", err)
			return exitFail
		}
		out = append(out, output{dir: *goOut, files: files})
	}
	if *cOut != "" {
		files, err := gen.C(spec, gen.COptions{Source: path})
		if err != nil {
			fmt.Fprintln(stderr, "hqq-bindgen:", err)
			return exitFail
		}
		out = append(out, output{dir: *cOut, files: files})
	}

	if *check {
		return checkOutputs(out, stderr)
	}
	return writeOutputs(out, stderr)
}

// output is the set of files destined for one directory.
type output struct {
	dir   string
	files []gen.File
}

// checkOutputs compares every generated file with the file on disk and names
// each one that is missing or differs.
func checkOutputs(out []output, stderr io.Writer) int {
	status := exitOK
	for _, o := range out {
		for _, f := range o.files {
			p := filepath.Join(o.dir, f.Name)
			old, err := os.ReadFile(p)
			switch {
			case errors.Is(err, os.ErrNotExist):
				fmt.Fprintf(stderr, "hqq-bindgen: %s is missing\n", p)
				status = exitFail
			case err != nil:
				fmt.Fprintln(stderr, "hqq-bindgen:", err)
				return exitUsage
			case !bytes.Equal(old, f.Data):
				fmt.Fprintf(stderr, "hqq-bindgen: %s is out of date\n", p)
				status = exitFail
			}
		}
	}
	return status
}

// writeOutputs writes every generated file, creating the directories. A file
// that already has the generated content is left alone, so its modification
// time does not change.
func writeOutputs(out []output, stderr io.Writer) int {
	for _, o := range out {
		if err := os.MkdirAll(o.dir, 0o755); err != nil {
			fmt.Fprintln(stderr, "hqq-bindgen:", err)
			return exitUsage
		}
		for _, f := range o.files {
			p := filepath.Join(o.dir, f.Name)
			if old, err := os.ReadFile(p); err == nil && bytes.Equal(old, f.Data) {
				continue
			}
			if err := os.WriteFile(p, f.Data, 0o644); err != nil {
				fmt.Fprintln(stderr, "hqq-bindgen:", err)
				return exitUsage
			}
		}
	}
	return exitOK
}
