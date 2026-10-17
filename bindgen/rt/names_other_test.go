//go:build !unix

package rt

import "testing"

// requireNameGone is a no-op where shared-memory names are not POSIX names.
// On Windows a name exists only while some handle to the object is open and
// cannot be unlinked while it is mapped, so "the server unlinked the name at
// HELLO" is not observable there: the other end still holds its handle when
// the test looks, and opening the name finds a live segment. Unlink after
// HELLO is a POSIX property of the design (BINDGEN.md 4.11), and the C server
// is POSIX-only. The rest of each test still runs.
func requireNameGone(t *testing.T, _ string) { t.Helper() }
