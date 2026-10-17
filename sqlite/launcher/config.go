package launcher

import (
	"crypto/ed25519"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gosuda.org/hqq/launch"
	"gosuda.org/hqq/secureexec"
)

// maxSessionsLimit is the server's upper bound for --max-sessions.
const maxSessionsLimit = 65536

// listenerPrefix starts the generated listener names.
const listenerPrefix = "hqqsql"

// Config describes the server to start. The zero value is not valid: a binary
// and its expected digest, or a manifest, are required.
type Config struct {
	// Binary is the server executable. It requires SHA256 and must not be
	// combined with Manifest.
	Binary string
	// SHA256 is the expected digest of Binary (for example from the release
	// notes). The zero value means "not set".
	SHA256 secureexec.Hash
	// Manifest selects the binary from a signed release manifest instead of
	// Binary and SHA256. The entry for runtime.GOOS and runtime.GOARCH is used.
	Manifest *ManifestSource

	// Listener is the HQQ listener name the server serves. Empty: a unique
	// name is generated.
	Listener string

	// AllowDir allows raw database paths inside the directory (see the server
	// README). Empty: aliases only.
	AllowDir string
	// Aliases maps database alias names to paths (--alias NAME=PATH).
	Aliases map[string]string
	// MaxSessions bounds the open sessions (1..65536). Zero: the server default.
	MaxSessions int
	// Perm is the listener permission. Zero: the server default (0600).
	Perm os.FileMode

	// ExtraArgs are appended to the server's arguments after the options.
	ExtraArgs []string
	// Env is the server's environment. Nil inherits the caller's.
	Env []string
	// Stdout and Stderr receive the server's output, except the ready line.
	// Their errors are ignored, so a broken log writer never stops the server.
	Stdout, Stderr io.Writer

	// ReadyTimeout bounds the wait for the ready line. Zero: 10s.
	ReadyTimeout time.Duration
	// StopTimeout bounds a graceful Stop before the server is killed. Zero: 10s.
	StopTimeout time.Duration

	// VerifyOptions are passed to secureexec.Verify (vault directory, policy).
	VerifyOptions []secureexec.Option

	// DisableLifeline starts the server without a parent lifeline (no
	// --parent-stdin). Stop then sends SIGTERM on Unix and CTRL_BREAK on Windows. Keep it false unless the
	// server must outlive this process.
	DisableLifeline bool
}

// ManifestSource is a signed release manifest and the directory that holds its
// artifacts.
type ManifestSource struct {
	// Data is the exact bytes of manifest.json.
	Data []byte
	// Signature is the text of manifest.json.sig.
	Signature []byte
	// PublicKey is the release public key. Nil selects ReleasePublicKey(),
	// which fails until the placeholder is replaced.
	PublicKey ed25519.PublicKey
	// Dir is the directory holding the artifacts; the entry's name is joined to
	// it.
	Dir string
}

// validate checks the SQLite-specific values. The generic ones (timeouts,
// listener name, binary and digest) are checked by launch.Start.
func (c Config) validate() error {
	if c.MaxSessions < 0 || c.MaxSessions > maxSessionsLimit {
		return fmt.Errorf("%w: MaxSessions %d: want 0 or 1..%d", ErrConfig, c.MaxSessions, maxSessionsLimit)
	}
	if c.Perm != 0 && c.Perm&^os.ModePerm != 0 {
		return fmt.Errorf("%w: Perm %o: want permission bits only", ErrConfig, c.Perm)
	}
	for name, path := range c.Aliases {
		if name == "" || strings.ContainsAny(name, "=\x00") {
			return fmt.Errorf("%w: alias name %q", ErrConfig, name)
		}
		if path == "" || strings.ContainsRune(path, 0) {
			return fmt.Errorf("%w: alias %q has an empty or invalid path", ErrConfig, name)
		}
	}
	return nil
}

// args returns the server's command line up to the lifeline flag: the listener
// and the database policy.
func (c Config) args(listener string) ([]string, error) {
	out := []string{listener}
	if c.AllowDir != "" {
		abs, err := filepath.Abs(c.AllowDir)
		if err != nil {
			return nil, fmt.Errorf("%w: AllowDir: %w", ErrConfig, err)
		}
		out = append(out, "--allow-dir", abs)
	}
	names := make([]string, 0, len(c.Aliases))
	for name := range c.Aliases {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		out = append(out, "--alias", name+"="+c.Aliases[name])
	}
	if c.MaxSessions != 0 {
		out = append(out, "--max-sessions", strconv.Itoa(c.MaxSessions))
	}
	if c.Perm != 0 {
		out = append(out, "--perm", strconv.FormatUint(uint64(c.Perm), 8))
	}
	return out, nil
}

// launchConfig maps c to the generic launcher's configuration. ExtraArgs are
// not part of it: Start passes them as launch.Start's trailing arguments, which
// follow the lifeline flag.
func (c Config) launchConfig() (launch.Config, error) {
	listener := c.Listener
	if listener == "" {
		var err error
		if listener, err = launch.NewListenerName(listenerPrefix); err != nil {
			return launch.Config{}, err
		}
	}
	args, err := c.args(listener)
	if err != nil {
		return launch.Config{}, err
	}
	lc := launch.Config{
		Binary:          c.Binary,
		SHA256:          c.SHA256,
		Listener:        listener,
		Args:            args,
		Env:             c.Env,
		Stdout:          c.Stdout,
		Stderr:          c.Stderr,
		ReadyTimeout:    c.ReadyTimeout,
		StopTimeout:     c.StopTimeout,
		VerifyOptions:   c.VerifyOptions,
		DisableLifeline: c.DisableLifeline,
	}
	if c.Manifest != nil {
		pub := c.Manifest.PublicKey
		if pub == nil {
			if pub, err = ReleasePublicKey(); err != nil {
				return launch.Config{}, err
			}
		}
		lc.Manifest = &launch.ManifestSource{
			Data:      c.Manifest.Data,
			Signature: c.Manifest.Signature,
			PublicKey: pub,
			Dir:       c.Manifest.Dir,
		}
	}
	return lc, nil
}
