package secureexec

import (
	"context"
	"crypto/subtle"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"sync"
)

// Mechanism names reported by Verified.Mechanism.
const (
	// MechanismMemfd: sealed in-kernel copy, executed through /proc/self/fd (Linux).
	MechanismMemfd = "memfd"
	// MechanismVault: private, verified copy in a per-run directory (POSIX).
	MechanismVault = "vault"
	// MechanismTrustedPath: the original file, executed by path; only used
	// with Policy.TrustOwnedReadOnly when the whole path is not writable by others.
	MechanismTrustedPath = "trusted-path"
	// MechanismWindowsShareLock: the original file held open with
	// FILE_SHARE_READ only (Windows).
	MechanismWindowsShareLock = "windows-share-lock"
)

// Policy tunes how the default mechanism is chosen.
type Policy struct {
	// TrustOwnedReadOnly prefers MechanismTrustedPath (no copy) when the opened
	// file and every directory above it are owned by root or the current user
	// and are not writable by group or other. If that check fails, the default
	// mechanism is used instead. It only protects against users other than
	// root and the owner; see README.md.
	TrustOwnedReadOnly bool
}

// Options configures Verify, Command and Start. Build it with Option values.
type Options struct {
	// VaultDir is the base directory for POSIX vaults. Empty selects
	// os.UserCacheDir()/hqq-vault (or os.TempDir()/hqq-vault if that fails).
	VaultDir string

	// Mechanism forces a mechanism. Empty selects the default. The only forced
	// value accepted is MechanismVault (POSIX); anything else fails with
	// ErrUnsupported.
	Mechanism string

	// Policy tunes mechanism selection.
	Policy Policy

	// Stdin, Stdout, Stderr, Env and Dir are copied into every *exec.Cmd
	// returned by Command (and used by Start). Zero values keep os/exec defaults.
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	Env            []string
	Dir            string
}

// Option mutates Options.
type Option func(*Options)

// WithVaultDir sets the POSIX vault base directory.
func WithVaultDir(dir string) Option {
	return func(o *Options) { o.VaultDir = dir }
}

// WithMechanism forces a mechanism (only MechanismVault is accepted).
func WithMechanism(mechanism string) Option {
	return func(o *Options) { o.Mechanism = mechanism }
}

// WithPolicy sets the selection policy.
func WithPolicy(p Policy) Option {
	return func(o *Options) { o.Policy = p }
}

// WithStdio sets the standard streams of the child. Nil keeps os/exec's default (no stream).
func WithStdio(stdin io.Reader, stdout, stderr io.Writer) Option {
	return func(o *Options) {
		o.Stdin, o.Stdout, o.Stderr = stdin, stdout, stderr
	}
}

// WithEnv sets the child environment (nil inherits the parent's).
func WithEnv(env []string) Option {
	return func(o *Options) { o.Env = env }
}

// WithDir sets the child working directory.
func WithDir(dir string) Option {
	return func(o *Options) { o.Dir = dir }
}

// Spec names an executable and the SHA-256 digest it must have.
type Spec struct {
	Path   string
	SHA256 Hash
}

// Verify is Verify(s.Path, s.SHA256, opts...).
func (s Spec) Verify(opts ...Option) (*Verified, error) {
	return Verify(s.Path, s.SHA256, opts...)
}

// protected is the platform result of protecting an executable: the object
// that will run, its digest, and what must stay alive until the child started.
type protected struct {
	mechanism string
	// execPath is passed as exec.Cmd.Path.
	execPath string
	// extra are passed as exec.Cmd.ExtraFiles (memfd only).
	extra []*os.File
	// hash is the digest of the protected object.
	hash Hash
	// release frees the protected object. It is called once.
	release func() error
}

func noRelease() error { return nil }

// Verified is an executable snapshot whose SHA-256 has been checked. Start a
// child with Command, then Close. Close only after the child has started (for
// the vault, the directory is removed; the running image is not affected on POSIX).
type Verified struct {
	path string
	p    *protected
	opts Options

	mu     sync.Mutex
	closed bool
}

// Verify opens path, snapshots its content with the platform mechanism,
// compares the SHA-256 of that snapshot with want (in constant time), and
// returns a Verified that starts the verified bytes. The check is performed on
// the object that will be executed, so swapping or rewriting path after Verify
// returns does not change what runs.
//
// A mismatch returns a *HashMismatchError (errors.Is(err, ErrHashMismatch)).
func Verify(path string, want Hash, opts ...Option) (*Verified, error) {
	o := newOptions(opts)
	if o.Mechanism != "" && o.Mechanism != MechanismVault {
		return nil, fmt.Errorf("secureexec: mechanism %q: %w", o.Mechanism, ErrUnsupported)
	}
	p, err := protect(path, &o)
	if err != nil {
		return nil, err
	}
	if subtle.ConstantTimeCompare(p.hash[:], want[:]) != 1 {
		_ = p.release()
		return nil, &HashMismatchError{Path: path, Got: p.hash, Want: want}
	}
	return &Verified{path: path, p: p, opts: o}, nil
}

func newOptions(opts []Option) Options {
	var o Options
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	return o
}

// Command returns an *exec.Cmd whose Path is the protected object and whose
// Args[0] is the path given to Verify. ExtraFiles is set as the mechanism
// requires. The caller may set Stdin, Stdout, Stderr, Env and Dir; values
// configured with options are already applied. Call Start on the result while
// the Verified is still open. ctx is used as with exec.CommandContext.
func (v *Verified) Command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, v.p.execPath, args...)
	cmd.Args[0] = v.path
	cmd.ExtraFiles = slices.Clone(v.p.extra)
	cmd.Stdin = v.opts.Stdin
	cmd.Stdout = v.opts.Stdout
	cmd.Stderr = v.opts.Stderr
	cmd.Env = slices.Clone(v.opts.Env)
	cmd.Dir = v.opts.Dir
	return cmd
}

// Mechanism reports which mechanism protects the executable (see the
// Mechanism* constants).
func (v *Verified) Mechanism() string { return v.p.mechanism }

// Hash returns the digest of the protected object (equal to the expected digest).
func (v *Verified) Hash() Hash { return v.p.hash }

// Close releases the memfd, vault or handle. It is idempotent. Call it after
// the children have started; a Command built after Close cannot be started.
func (v *Verified) Close() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return nil
	}
	v.closed = true
	return v.p.release()
}

// Process is a child started by Start. Its protected object is released when
// the child exits, whether or not Wait is called.
type Process struct {
	cmd   *exec.Cmd
	mech  string
	done  chan struct{}
	state *os.ProcessState
	err   error
}

// Start is Verify, Command and cmd.Start in one call. The protected object is
// released as soon as the child exits. args are passed after the logical path.
func Start(ctx context.Context, path string, want Hash, args []string, opts ...Option) (*Process, error) {
	v, err := Verify(path, want, opts...)
	if err != nil {
		return nil, err
	}
	cmd := v.Command(ctx, args...)
	if err := cmd.Start(); err != nil {
		_ = v.Close()
		return nil, err
	}
	p := &Process{cmd: cmd, mech: v.Mechanism(), done: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		if cerr := v.Close(); err == nil {
			err = cerr
		}
		p.state, p.err = cmd.ProcessState, err
		close(p.done)
	}()
	return p, nil
}

// Pid returns the child's process id.
func (p *Process) Pid() int { return p.cmd.Process.Pid }

// Mechanism reports the mechanism that protects the child's executable.
func (p *Process) Mechanism() string { return p.mech }

// Done is closed once the child has exited and its protected object is released.
func (p *Process) Done() <-chan struct{} { return p.done }

// Wait blocks until the child exits and returns its state. It may be called
// more than once.
func (p *Process) Wait() (*os.ProcessState, error) {
	<-p.done
	return p.state, p.err
}

// Kill kills the child.
func (p *Process) Kill() error { return p.cmd.Process.Kill() }

// Signal sends sig to the child.
func (p *Process) Signal(sig os.Signal) error { return p.cmd.Process.Signal(sig) }
