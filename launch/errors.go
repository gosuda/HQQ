package launch

import (
	"errors"
	"fmt"
	"time"
)

var (
	// ErrConfig reports an invalid Config. Nothing was started.
	ErrConfig = errors.New("launch: invalid config")

	// ErrManifest reports a manifest whose signature or content was rejected.
	// It wraps the manifest package's error (for example manifest.ErrBadSignature).
	ErrManifest = errors.New("launch: manifest rejected")

	// ErrNoArtifact reports a verified manifest with no entry for runtime.GOOS
	// and runtime.GOARCH.
	ErrNoArtifact = errors.New("launch: manifest has no artifact for this platform")

	// ErrReadyTimeout matches (errors.Is) a *ReadyTimeoutError.
	ErrReadyTimeout = errors.New("launch: server not ready in time")

	// ErrStopTimeout is returned by Stop when the server did not exit within
	// StopTimeout and was killed.
	ErrStopTimeout = errors.New("launch: server did not stop in time")
)

// maxOutputInError bounds the server output quoted in an error message.
const maxOutputInError = 1024

// StartError reports that the server could not be started: verification failed
// (Err wraps secureexec.ErrHashMismatch, secureexec.ErrUnsupported, ...), the
// process could not be created, the server exited before it was ready, or ctx
// ended first. Output is the end of the server's stdout and stderr.
type StartError struct {
	Path   string
	Err    error
	Output string
}

func (e *StartError) Error() string {
	return withOutput(fmt.Sprintf("launch: start %s: %v", e.Path, e.Err), e.Output)
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
	return withOutput(fmt.Sprintf("launch: server %s not ready after %v", e.Listener, e.Timeout), e.Output)
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
