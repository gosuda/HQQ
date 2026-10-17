//go:build unix

package rt

import (
	"errors"
	"os"
	"testing"

	hqq "gosuda.org/hqq"
)

// requireNameGone checks that the server's session segment name was unlinked:
// opening it must fail with a missing-name error (BINDGEN.md 4.11). The check
// is POSIX-only; see names_other_test.go.
func requireNameGone(t *testing.T, name string) {
	t.Helper()
	l, err := hqq.OpenStandardLink(name, 64, 16384, testLinkOpts())
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		if l != nil {
			_ = l.Close()
		}
		t.Errorf("session name %q still exists: %v", name, err)
	}
}
