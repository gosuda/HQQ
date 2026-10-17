//go:build unix

package microlib_test

import (
	"errors"
	"testing"

	"gosuda.org/hqq/bindgen/rt"
	"gosuda.org/hqq/launch"
	"gosuda.org/hqq/secureexec"
)

// TestLaunchVerifiesTheBinary: launch.Start refuses a server binary whose SHA-256 is not the pinned
// one, and starts no process (BINDGEN.md 7.5, D17). Every other test in this package starts the server
// with the digest of the file computed by startServer, which is the success side.
func TestLaunchVerifiesTheBinary(t *testing.T) {
	bin := serverBinary(t)
	hash, _, err := secureexec.HashFile(bin)
	wantNoErr(t, "HashFile", err)
	wrong := hash
	wrong[0] ^= 0xff
	name := uniqueName(t)
	ctx, cancel := contextWithFail()
	defer cancel()
	srv, err := launch.Start(ctx, launch.Config{Binary: bin, SHA256: wrong, Listener: name, Args: []string{name}})
	if err == nil {
		_ = srv.Close()
		t.Fatal("launch.Start accepted a binary with the wrong SHA-256")
	}
	var se *launch.StartError
	if !errors.As(err, &se) || !errors.Is(err, secureexec.ErrHashMismatch) {
		t.Fatalf("launch.Start error = %v (%T), want *launch.StartError wrapping secureexec.ErrHashMismatch", err, err)
	}
	if nameExists(t, name) {
		t.Fatalf("the listener %q exists although no server was started", name)
	}
}

// TestLaunchNewListenerName: the name from launch.NewListenerName (the one the design's example uses)
// works as the listener of the server and is accepted by the generated client.
func TestLaunchNewListenerName(t *testing.T) {
	bin := serverBinary(t)
	hash, _, err := secureexec.HashFile(bin)
	wantNoErr(t, "HashFile", err)
	name, err := launch.NewListenerName("ml")
	wantNoErr(t, "NewListenerName", err)
	ctx, cancel := contextWithFail()
	defer cancel()
	srv, err := launch.Start(ctx, launch.Config{
		Binary: bin, SHA256: hash, Listener: name,
		Args: []string{name, "--max-sessions", "4"},
	})
	wantNoErr(t, "launch.Start", err)
	defer func() {
		if err := srv.Close(); err != nil {
			t.Errorf("server did not stop cleanly: %v", err)
		}
	}()
	if srv.Listener() != name {
		t.Fatalf("Listener() = %q, want %q", srv.Listener(), name)
	}

	c, err := dialWith(ctx, srv.Listener(), rt.Options{})
	wantNoErr(t, "Dial", err)
	defer func() { wantNoErr(t, "Close", c.Close()) }()
	if got, err := c.Add(20, 22); err != nil || got != 42 {
		t.Fatalf("Add = (%d, %v)", got, err)
	}
}
