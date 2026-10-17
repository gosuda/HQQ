// Command hqq-manifest generates, signs and verifies release manifests for
// package gosuda.org/hqq/secureexec, and hashes files.
//
// Exit status: 0 success, 1 failure, 2 usage error.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"gosuda.org/hqq/secureexec"
	"gosuda.org/hqq/secureexec/manifest"
)

const usage = `usage: hqq-manifest <command> [flags]

commands:
  keygen  -priv FILE -pub FILE
          create an ed25519 key pair (hex files; the private file is mode 0600)
  gen     -dir DIR -name NAME -release REL [-license ID]... -out FILE
          scan DIR for NAME_<os>_<arch>[.exe] artifacts and write an unsigned manifest
  sign    (-key FILE | -key-env VAR) -in FILE [-out FILE.sig]
          write the detached signature of a manifest
  verify  -pub FILE -in FILE -sig FILE [-dir DIR]
          check the signature (and, with -dir, every artifact's size and sha256)
  hash    FILE...
          print sha256sum-style lines: "<hex>  <file>"
`

// usageError marks mistakes on the command line (exit status 2).
type usageError struct{ err error }

func (e usageError) Error() string { return e.err.Error() }

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	var err error
	switch cmd {
	case "keygen":
		err = cmdKeygen(rest, stdout, stderr)
	case "gen":
		err = cmdGen(rest, stdout, stderr)
	case "sign":
		err = cmdSign(rest, stdout, stderr)
	case "verify":
		err = cmdVerify(rest, stdout, stderr)
	case "hash":
		err = cmdHash(rest, stdout, stderr)
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "hqq-manifest: unknown command %q\n\n%s", cmd, usage)
		return 2
	}
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
		return 0
	case isUsage(err):
		fmt.Fprintf(stderr, "hqq-manifest %s: %v\n", cmd, err)
		return 2
	default:
		fmt.Fprintf(stderr, "hqq-manifest %s: %v\n", cmd, err)
		return 1
	}
}

func isUsage(err error) bool {
	var ue usageError
	return errors.As(err, &ue)
}

// newFlags returns a FlagSet that reports to stderr and never exits.
func newFlags(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// parse parses args. Positional arguments are rejected unless allowed.
func parse(fs *flag.FlagSet, args []string, positional bool) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return usageError{err}
	}
	if !positional && fs.NArg() != 0 {
		return usageError{fmt.Errorf("unexpected argument %q", fs.Arg(0))}
	}
	return nil
}

// missing reports the first empty flag. pairs alternate name and value.
func missing(pairs ...string) error {
	for i := 0; i+1 < len(pairs); i += 2 {
		if pairs[i+1] == "" {
			return usageError{fmt.Errorf("-%s is required", pairs[i])}
		}
	}
	return nil
}

// stringList is a repeatable string flag.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func cmdKeygen(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("keygen", stderr)
	privPath := fs.String("priv", "", "private key output file (mode 0600)")
	pubPath := fs.String("pub", "", "public key output file")
	if err := parse(fs, args, false); err != nil {
		return err
	}
	if err := missing("priv", *privPath, "pub", *pubPath); err != nil {
		return err
	}
	pub, priv, err := manifest.GenerateKey()
	if err != nil {
		return err
	}
	// Both files are created exclusively: an existing key is never overwritten.
	if err := writeNew(*privPath, manifest.FormatPrivateKey(priv), 0o600); err != nil {
		return err
	}
	if err := writeNew(*pubPath, manifest.FormatPublicKey(pub), 0o644); err != nil {
		_ = os.Remove(*privPath)
		return err
	}
	fmt.Fprintf(stdout, "public key: %x\n", []byte(pub))
	return nil
}

// writeNew creates path exclusively with the given mode and writes data.
func writeNew(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func cmdGen(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("gen", stderr)
	dir := fs.String("dir", ".", "directory holding the artifacts")
	name := fs.String("name", "", "product name; artifacts are NAME_<os>_<arch>[.exe]")
	release := fs.String("release", "", "release identifier, for example v0.6.0")
	out := fs.String("out", "", "manifest output file")
	var licenses stringList
	fs.Var(&licenses, "license", "license identifier (repeatable)")
	if err := parse(fs, args, false); err != nil {
		return err
	}
	if err := missing("name", *name, "release", *release, "out", *out); err != nil {
		return err
	}
	m, err := manifest.Scan(*dir, *name, *release, []string(licenses))
	if err != nil {
		return err
	}
	data, err := manifest.Marshal(m)
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "wrote %s: %d artifacts\n", *out, len(m.Files))
	return nil
}

func cmdSign(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("sign", stderr)
	keyPath := fs.String("key", "", "private key file (hex seed)")
	keyEnv := fs.String("key-env", "", "name of an environment variable holding the private key (hex seed)")
	in := fs.String("in", "", "manifest to sign")
	out := fs.String("out", "", "signature output file (default: -in plus .sig)")
	if err := parse(fs, args, false); err != nil {
		return err
	}
	if err := missing("in", *in); err != nil {
		return err
	}
	if (*keyPath == "") == (*keyEnv == "") {
		return usageError{errors.New("exactly one of -key or -key-env is required")}
	}
	var keyText []byte
	if *keyPath != "" {
		b, err := os.ReadFile(*keyPath)
		if err != nil {
			return err
		}
		keyText = b
	} else {
		v := os.Getenv(*keyEnv)
		if v == "" {
			return fmt.Errorf("environment variable %s is empty", *keyEnv)
		}
		keyText = []byte(v)
	}
	priv, err := manifest.ParsePrivateKey(keyText)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(*in)
	if err != nil {
		return err
	}
	if _, err := manifest.Parse(data); err != nil {
		return fmt.Errorf("refusing to sign %s: %w", *in, err)
	}
	sig, err := manifest.Sign(data, priv)
	if err != nil {
		return err
	}
	dst := *out
	if dst == "" {
		dst = *in + ".sig"
	}
	if err := os.WriteFile(dst, sig, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "wrote %s\n", dst)
	return nil
}

func cmdVerify(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("verify", stderr)
	pubPath := fs.String("pub", "", "public key file")
	in := fs.String("in", "", "manifest to verify")
	sigPath := fs.String("sig", "", "detached signature file")
	dir := fs.String("dir", "", "if set, also check every artifact in this directory")
	if err := parse(fs, args, false); err != nil {
		return err
	}
	if err := missing("pub", *pubPath, "in", *in, "sig", *sigPath); err != nil {
		return err
	}
	pubText, err := os.ReadFile(*pubPath)
	if err != nil {
		return err
	}
	pub, err := manifest.ParsePublicKey(pubText)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(*in)
	if err != nil {
		return err
	}
	sig, err := os.ReadFile(*sigPath)
	if err != nil {
		return err
	}
	m, err := manifest.Verify(data, sig, pub)
	if err != nil {
		return err
	}
	if *dir != "" {
		if err := manifest.VerifyFiles(*dir, m); err != nil {
			return err
		}
	}
	fmt.Fprintf(stdout, "ok: %s %s, %d files\n", m.Name, m.Release, len(m.Files))
	return nil
}

func cmdHash(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("hash", stderr)
	if err := parse(fs, args, true); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return usageError{errors.New("at least one FILE is required")}
	}
	for _, path := range fs.Args() {
		sum, _, err := secureexec.HashFile(path)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s  %s\n", sum, path)
	}
	return nil
}
