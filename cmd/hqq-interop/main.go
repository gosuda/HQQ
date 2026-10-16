// Command hqq-interop is the Go implementation of the HQQ interoperability
// helper. One invocation runs one role of one scenario in this process; the
// driver (interop/) starts one process per role. The contract is
// conformance/INTEROP_CLI.md; any other implementation provides an
// executable with the same contract.
//
// Subcommands:
//
//	hqq-interop run --scenario X-01 --role creator|opener|a|b --name N ...
//	hqq-interop list [--impl LABEL]
//	hqq-interop gen-vectors [--out DIR]
//	hqq-interop version
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"gosuda.org/hqq/conformance"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}
	switch args[0] {
	case "run":
		return runCommand(args[1:], stdout, stderr)
	case "list":
		return listCommand(args[1:], stdout, stderr)
	case "gen-vectors":
		return genVectorsCommand(args[1:], stdout, stderr)
	case "version":
		return versionCommand(stdout)
	case "-h", "--help", "help":
		usage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "hqq-interop: unknown subcommand %q\n", args[0])
		usage(stderr)
		return exitUsage
	}
}

func usage(w io.Writer) {
	fmt.Fprintln(w, `usage:
  hqq-interop run --scenario ID --role creator|opener|a|b --name NAME [flags]
  hqq-interop list [--impl LABEL]
  hqq-interop gen-vectors [--out DIR]
  hqq-interop version`)
}

func listCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	impl := fs.String("impl", "go", "label of this implementation")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	return writeJSON(stdout, stderr, listScenarios(*impl))
}

func versionCommand(stdout io.Writer) int {
	return writeJSON(stdout, stdout, map[string]any{
		"helper":         "hqq-interop",
		"impl":           "go",
		"protocol":       "1.1",
		"segment_layout": "HQQ_STL3",
		"ring_layout":    conformance.RingVersion,
	})
}

func genVectorsCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gen-vectors", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", "conformance/vectors", "output directory (created if missing)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if err := conformance.Generate(*out); err != nil {
		fmt.Fprintln(stderr, "hqq-interop gen-vectors:", err)
		return exitFailed
	}
	fmt.Fprintln(stdout, "vectors written to", *out)
	return exitOK
}

func writeJSON(w, errw io.Writer, v any) int {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintln(errw, "hqq-interop:", err)
		return exitInternal
	}
	return exitOK
}
