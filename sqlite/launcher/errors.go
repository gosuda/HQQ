package launcher

import (
	"errors"

	"gosuda.org/hqq/launch"
)

var (
	// ErrConfig reports an invalid Config. Nothing was started.
	ErrConfig = launch.ErrConfig

	// ErrManifest reports a manifest whose signature or content was rejected.
	// It wraps the manifest package's error (for example manifest.ErrBadSignature).
	ErrManifest = launch.ErrManifest

	// ErrNoArtifact reports a verified manifest with no entry for runtime.GOOS
	// and runtime.GOARCH.
	ErrNoArtifact = launch.ErrNoArtifact

	// ErrNoReleaseKey reports that no usable release public key is embedded
	// (see ReleasePublicKey) and the caller passed none.
	ErrNoReleaseKey = errors.New("launcher: no release public key")

	// ErrReadyTimeout matches (errors.Is) a *ReadyTimeoutError.
	ErrReadyTimeout = launch.ErrReadyTimeout

	// ErrStopTimeout is returned by Stop when the server did not exit within
	// StopTimeout and was killed.
	ErrStopTimeout = launch.ErrStopTimeout
)
