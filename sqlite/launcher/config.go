package launcher

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"gosuda.org/hqq/secureexec"
	"gosuda.org/hqq/secureexec/manifest"
)

const (
	defaultReadyTimeout = 10 * time.Second
	defaultStopTimeout  = 10 * time.Second
	// maxListenerName bounds the listener name: macOS shared memory names are
	// limited to 31 bytes.
	maxListenerName = 31
	// maxSessionsLimit is the server's upper bound for --max-sessions.
	maxSessionsLimit = 65536
)

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

// resolve returns the path of the binary and the digest it must have.
func (c Config) resolve() (string, secureexec.Hash, error) {
	var zero secureexec.Hash
	if c.Manifest == nil {
		if c.Binary == "" {
			return "", zero, fmt.Errorf("%w: Binary or Manifest is required", ErrConfig)
		}
		if c.SHA256 == zero {
			return "", zero, fmt.Errorf("%w: SHA256 or Manifest is required: refusing to start an unverified binary", ErrConfig)
		}
		return c.Binary, c.SHA256, nil
	}
	if c.Binary != "" || c.SHA256 != zero {
		return "", zero, fmt.Errorf("%w: Manifest is exclusive with Binary and SHA256", ErrConfig)
	}
	if c.Manifest.Dir == "" {
		return "", zero, fmt.Errorf("%w: Manifest.Dir is required", ErrConfig)
	}
	pub := c.Manifest.PublicKey
	if pub == nil {
		var err error
		if pub, err = ReleasePublicKey(); err != nil {
			return "", zero, err
		}
	}
	if len(pub) != ed25519.PublicKeySize {
		return "", zero, fmt.Errorf("%w: Manifest.PublicKey has %d bytes, want %d", ErrConfig, len(pub), ed25519.PublicKeySize)
	}
	m, err := manifest.Verify(c.Manifest.Data, c.Manifest.Signature, pub)
	if err != nil {
		return "", zero, fmt.Errorf("%w: %w", ErrManifest, err)
	}
	f, ok := m.Find(runtime.GOOS, runtime.GOARCH)
	if !ok {
		return "", zero, fmt.Errorf("%w: %s/%s", ErrNoArtifact, runtime.GOOS, runtime.GOARCH)
	}
	return filepath.Join(c.Manifest.Dir, f.Name), f.SHA256, nil
}

// listenerName returns the configured listener name or a generated one.
func (c Config) listenerName() (string, error) {
	if c.Listener == "" {
		var b [6]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", fmt.Errorf("launcher: listener name: %w", err)
		}
		return "hqqsql-" + hex.EncodeToString(b[:]), nil
	}
	n := c.Listener
	if len(n) > maxListenerName {
		return "", fmt.Errorf("%w: listener %q is longer than %d bytes", ErrConfig, n, maxListenerName)
	}
	if strings.HasPrefix(n, "-") {
		return "", fmt.Errorf("%w: listener %q must not start with '-'", ErrConfig, n)
	}
	for _, r := range n {
		ok := r == '-' || r == '_' || r == '.' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if !ok {
			return "", fmt.Errorf("%w: listener %q may only contain letters, digits, '-', '_' and '.'", ErrConfig, n)
		}
	}
	return n, nil
}

// validate checks the values that do not depend on the binary.
func (c Config) validate() error {
	if c.ReadyTimeout < 0 || c.StopTimeout < 0 {
		return fmt.Errorf("%w: timeouts must not be negative", ErrConfig)
	}
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

// args returns the server's command line for listener.
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
	if !c.DisableLifeline {
		out = append(out, "--parent-stdin")
	}
	return slices.Concat(out, c.ExtraArgs), nil
}

// timeouts returns the ready and stop timeouts with defaults applied.
func (c Config) timeouts() (ready, stop time.Duration) {
	ready, stop = c.ReadyTimeout, c.StopTimeout
	if ready == 0 {
		ready = defaultReadyTimeout
	}
	if stop == 0 {
		stop = defaultStopTimeout
	}
	return ready, stop
}
