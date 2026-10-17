//go:build !unix && !windows

package secureexec

import (
	"fmt"
	"runtime"
	"time"
)

// protect fails on platforms without a mechanism (for example plan9, js and wasip1).
func protect(string, *Options) (*protected, error) {
	return nil, fmt.Errorf("secureexec: %s/%s: %w", runtime.GOOS, runtime.GOARCH, ErrUnsupported)
}

// CleanVaults is not supported on this platform.
func CleanVaults(string, time.Duration) (int, error) {
	return 0, fmt.Errorf("secureexec: CleanVaults: %w", ErrUnsupported)
}
