package launch

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"gosuda.org/hqq/secureexec"
	"gosuda.org/hqq/secureexec/manifest"
)

const (
	// ReadyPrefix starts the ready line a server prints on stdout once its
	// listener exists. The whole line is ReadyPrefix, Config.Listener and a
	// newline, with no other bytes (a carriage return does not match):
	//
	//	ready <listener>\n
	ReadyPrefix = "ready "

	// DefaultLifelineFlag is the flag that makes a server shut down when its
	// standard input reaches end of file (the C server's --parent-stdin).
	DefaultLifelineFlag = "--parent-stdin"

	defaultReadyTimeout = 10 * time.Second
	defaultStopTimeout  = 10 * time.Second

	// maxListenerName bounds the listener name: macOS shared memory names are
	// limited to 31 bytes.
	maxListenerName = 31
)

// Config describes the server to start. The zero value is not valid: a binary
// and its expected digest, or a manifest, and a listener name are required.
type Config struct {
	// Binary is the server executable. It requires SHA256 and must not be
	// combined with Manifest. A relative path is resolved against the working
	// directory of this process, not against Dir.
	Binary string
	// SHA256 is the expected digest of Binary (for example from the release
	// notes). The zero value means "not set".
	SHA256 secureexec.Hash
	// Manifest selects the binary from a signed release manifest instead of
	// Binary and SHA256. The entry for runtime.GOOS and runtime.GOARCH is used.
	Manifest *ManifestSource

	// Listener is the HQQ listener name the server serves, and the name Start
	// waits for in the ready line (see ReadyPrefix). Required. Start does not
	// pass it to the server: put it where the server expects it, usually as the
	// first element of Args. NewListenerName makes a unique one.
	//
	// The name is at most 31 bytes of letters, digits, '-', '_' and '.', and
	// does not start with '-'.
	Listener string

	// Args are the server's arguments that precede the lifeline flag.
	Args []string
	// Env is the server's environment. Nil inherits the caller's.
	Env []string
	// Dir is the server's working directory. Empty: the caller's.
	Dir string
	// Stdout and Stderr receive the server's output, except the ready line.
	// Their errors are ignored, so a broken log writer never stops the server.
	Stdout, Stderr io.Writer

	// ReadyTimeout bounds the wait for the ready line. Zero: 10s. Negative is
	// invalid.
	ReadyTimeout time.Duration
	// StopTimeout bounds a graceful Stop before the server is killed. Zero:
	// 10s. Negative is invalid.
	StopTimeout time.Duration

	// VerifyOptions are passed to secureexec.Verify (vault directory, policy).
	VerifyOptions []secureexec.Option

	// DisableLifeline starts the server without a parent lifeline: no lifeline
	// flag, and Stop sends SIGTERM on Unix and CTRL_BREAK on Windows. Keep it
	// false unless the server must outlive this process. It is exclusive with
	// LifelineFlag.
	DisableLifeline bool
	// LifelineFlag is the argument that turns on the server's lifeline. Empty:
	// DefaultLifelineFlag. It must start with '-'; a value such as
	// "--parent-stdin=force" is allowed.
	LifelineFlag string
}

// ManifestSource is a signed release manifest and the directory that holds its
// artifacts.
type ManifestSource struct {
	// Data is the exact bytes of manifest.json.
	Data []byte
	// Signature is the text of manifest.json.sig.
	Signature []byte
	// PublicKey is the release public key. Required: there is no default key
	// at this level.
	PublicKey ed25519.PublicKey
	// Dir is the directory holding the artifacts; the entry's name is joined to
	// it.
	Dir string
}

// NewListenerName returns prefix, '-' and 12 random hex digits: a unique
// listener name that satisfies the Config.Listener rules. It fails with
// ErrConfig when prefix does not fit them.
func NewListenerName(prefix string) (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("launch: listener name: %w", err)
	}
	name := prefix + "-" + hex.EncodeToString(b[:])
	if err := validateListener(name); err != nil {
		return "", err
	}
	return name, nil
}

// validateListener checks a listener name against the Config.Listener rules.
func validateListener(n string) error {
	if n == "" {
		return fmt.Errorf("%w: Listener is required", ErrConfig)
	}
	if len(n) > maxListenerName {
		return fmt.Errorf("%w: listener %q is longer than %d bytes", ErrConfig, n, maxListenerName)
	}
	if strings.HasPrefix(n, "-") {
		return fmt.Errorf("%w: listener %q must not start with '-'", ErrConfig, n)
	}
	for _, r := range n {
		ok := r == '-' || r == '_' || r == '.' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if !ok {
			return fmt.Errorf("%w: listener %q may only contain letters, digits, '-', '_' and '.'", ErrConfig, n)
		}
	}
	return nil
}

// validate checks the values that do not depend on the binary.
func (c Config) validate() error {
	if c.ReadyTimeout < 0 || c.StopTimeout < 0 {
		return fmt.Errorf("%w: timeouts must not be negative", ErrConfig)
	}
	if c.DisableLifeline && c.LifelineFlag != "" {
		return fmt.Errorf("%w: LifelineFlag is exclusive with DisableLifeline", ErrConfig)
	}
	if c.LifelineFlag != "" && !strings.HasPrefix(c.LifelineFlag, "-") {
		return fmt.Errorf("%w: LifelineFlag %q must start with '-'", ErrConfig, c.LifelineFlag)
	}
	return validateListener(c.Listener)
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

// argv returns the server's command line: Args, the lifeline flag unless it is
// disabled, then extra. The caller's trailing arguments follow the flag, so a
// "--" terminator or an operand among them cannot turn it into an operand.
func (c Config) argv(extra []string) []string {
	n := len(c.Args) + len(extra)
	if !c.DisableLifeline {
		n++
	}
	out := make([]string, 0, n)
	out = append(out, c.Args...)
	if !c.DisableLifeline {
		flag := c.LifelineFlag
		if flag == "" {
			flag = DefaultLifelineFlag
		}
		out = append(out, flag)
	}
	return append(out, extra...)
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
