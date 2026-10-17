package launcher

import (
	"errors"
	"fmt"
	"time"
)

var (
	// ErrConfig reports an invalid Config. Nothing was started.
	ErrConfig = errors.New("launcher: invalid config")

	// ErrManifest reports a manifest whose signature or content was rejected.
	// It wraps the manifest package's error (for example manifest.ErrBadSignature).
	ErrManifest = errors.New("launcher: manifest rejected")

	// ErrNoArtifact reports a verified manifest with no entry for runtime.GOOS
	// and runtime.GOARCH.
	ErrNoArtifact = errors.New("launcher: manifest has no artifact for this platform")

	// ErrNoReleaseKey reports that no usable release public key is embedded
	// (see ReleasePublicKey) and the caller passed none.
	ErrNoReleaseKey = errors.New("launcher: no release public key")

	// ErrReadyTimeout matches (errors.Is) a *ReadyTimeoutError.
	ErrReadyTimeout = errors.New("launcher: server not ready in time")

	// ErrStopTimeout is returned by Stop when the server did not exit within
	// StopTimeout and was killed.
	ErrStopTimeout = errors.New("launcher: server did not stop in time")
)

// maxOutputInError bounds the server output quoted in an error message.
const maxOutputInError = 1024

// StartError reports that the server could not be started: verification failed
// (Err wraps secureexec.ErrHashMismatch, secureexec.ErrUnsupported, ...), the
// process could not be created, or it exited before it was ready. Output is the
// end of the server's stdout and stderr.
type StartError struct {
	Path   string
	Err    error
	Output string
}

func (e *StartError) Error() string {
	return withOutput(fmt.Sprintf("launcher: start %s: %v", e.Path, e.Err), e.Output)
}

// Unwrap returns the underlying error.
func (e *StartError) Unwrap() error { return e.Err }

// ReadyTimeoutError reports that the server did not print its ready line within
// the ReadyTimeout. The process was killed. Output is the end of its output.
type ReadyTimeoutError struct {
	Listener string
	Timeout  time.Duration
	Output   string
}

func (e *ReadyTimeoutError) Error() string {
	return withOutput(fmt.Sprintf("launcher: server %s not ready after %v", e.Listener, e.Timeout), e.Output)
}

// Is makes errors.Is(err, ErrReadyTimeout) report true.
func (e *ReadyTimeoutError) Is(target error) bool { return target == ErrReadyTimeout }

// withOutput appends the (bounded) output to msg.
func withOutput(msg, out string) string {
	if out == "" {
		return msg
	}
	if len(out) > maxOutputInError {
		out = out[len(out)-maxOutputInError:]
	}
	return msg + "; output: " + out
}
