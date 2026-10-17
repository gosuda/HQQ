package launcher_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gosuda.org/hqq/secureexec"
	"gosuda.org/hqq/secureexec/manifest"
	"gosuda.org/hqq/sqlite/launcher"
)

// e2eServerFlags are the link flags of the real server in the tests: quick
// heartbeats, so a dead peer is noticed within a few hundred milliseconds.
var e2eServerFlags = []string{"--heartbeat-ms", "20", "--peer-timeout-ms", "300"}

var listenerSeq atomic.Int64

// newListener returns a short unique listener name (macOS limits names to 31
// bytes).
func newListener() string {
	return fmt.Sprintf("lnch%x%x", os.Getpid()&0xfff, listenerSeq.Add(1))
}

// self describes this test binary, which is the stub server.
type self struct {
	path string // symlink-resolved path, what secureexec.Verify needs
	hash secureexec.Hash
	size int64
}

func selfInfo(t *testing.T) self {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		t.Fatal(err)
	}
	h, n, err := secureexec.HashFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	return self{path: exe, hash: h, size: n}
}

// stubConfig returns a Config that starts the test binary as the stub in mode,
// verified by its own digest. The stub's environment is the test's, plus
// LAUNCHER_STUB.
func stubConfig(t *testing.T, mode string) launcher.Config {
	t.Helper()
	s := selfInfo(t)
	return launcher.Config{
		Binary:       s.path,
		SHA256:       s.hash,
		Listener:     newListener(),
		Env:          append(os.Environ(), envStub+"="+mode),
		ReadyTimeout: 10 * time.Second,
		StopTimeout:  5 * time.Second,
	}
}

// withFile adds an environment variable naming a file under t.TempDir() and
// returns the file's path.
func withFile(t *testing.T, cfg *launcher.Config, key string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), strings.ToLower(key))
	cfg.Env = append(cfg.Env, key+"="+p)
	return p
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// signedManifest returns manifest.json and its signature for one artifact.
func signedManifest(t *testing.T, signer ed25519.PrivateKey, goos, goarch, name string, sum secureexec.Hash, size int64) ([]byte, []byte) {
	t.Helper()
	m := &manifest.Manifest{
		Version: manifest.Version,
		Name:    "hqq-sqlite-server",
		Release: "v0.0.0-test",
		Files: []manifest.File{{
			OS: goos, Arch: goarch, Name: name, SHA256: sum, Size: size,
		}},
		Licenses: []string{"MIT"},
	}
	data, err := manifest.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := manifest.Sign(data, signer)
	if err != nil {
		t.Fatal(err)
	}
	return data, sig
}

func TestStartHashOKAndStopGraceful(t *testing.T) {
	cfg := stubConfig(t, "ok")
	srv, err := launcher.Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if srv.Listener() != cfg.Listener {
		t.Fatalf("Listener() = %q, want %q", srv.Listener(), cfg.Listener)
	}
	if srv.PID() <= 0 {
		t.Fatalf("PID() = %d", srv.PID())
	}
	select {
	case <-srv.Done():
		t.Fatal("Done closed while the server runs")
	default:
	}
	if err := srv.Err(); err != nil {
		t.Fatalf("Err() while running = %v", err)
	}
	if err := srv.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-srv.Done():
	default:
		t.Fatal("Done not closed after Stop")
	}
	if err := srv.Err(); err != nil {
		t.Fatalf("Err() after graceful stop = %v", err)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestHashMismatchStartsNothing(t *testing.T) {
	cfg := stubConfig(t, "ok")
	mark := withFile(t, &cfg, envMark)
	cfg.SHA256[0] ^= 0xff
	_, err := launcher.Start(context.Background(), cfg)
	if !errors.Is(err, secureexec.ErrHashMismatch) {
		t.Fatalf("Start: %v, want ErrHashMismatch", err)
	}
	var se *launcher.StartError
	if !errors.As(err, &se) {
		t.Fatalf("error %T is not *StartError", err)
	}
	if exists(mark) {
		t.Fatal("the server ran despite the hash mismatch")
	}
}

func TestConfigRefusesUnverifiedAndAmbiguous(t *testing.T) {
	s := selfInfo(t)
	pub, priv, err := manifest.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	data, sig := signedManifest(t, priv, runtime.GOOS, runtime.GOARCH, filepath.Base(s.path), s.hash, s.size)
	src := &launcher.ManifestSource{Data: data, Signature: sig, PublicKey: pub, Dir: filepath.Dir(s.path)}
	cases := map[string]launcher.Config{
		"no binary":           {SHA256: s.hash},
		"no hash":             {Binary: s.path},
		"binary and manifest": {Binary: s.path, SHA256: s.hash, Manifest: src},
		"hash and manifest":   {SHA256: s.hash, Manifest: src},
		"manifest without dir": {Manifest: &launcher.ManifestSource{
			Data: data, Signature: sig, PublicKey: pub}},
		"short public key": {Manifest: &launcher.ManifestSource{
			Data: data, Signature: sig, PublicKey: pub[:5], Dir: filepath.Dir(s.path)}},
		"listener with slash":       {Binary: s.path, SHA256: s.hash, Listener: "a/b"},
		"listener starts with dash": {Binary: s.path, SHA256: s.hash, Listener: "-x"},
		"listener too long": {Binary: s.path, SHA256: s.hash,
			Listener: strings.Repeat("a", 32)},
		"alias with equals": {Binary: s.path, SHA256: s.hash,
			Aliases: map[string]string{"a=b": "/x"}},
		"alias without path": {Binary: s.path, SHA256: s.hash,
			Aliases: map[string]string{"a": ""}},
		"too many sessions": {Binary: s.path, SHA256: s.hash, MaxSessions: 70000},
		"negative sessions": {Binary: s.path, SHA256: s.hash, MaxSessions: -1},
		"perm with type bits": {Binary: s.path, SHA256: s.hash,
			Perm: 0o1000 | 0o600},
		"negative timeout": {Binary: s.path, SHA256: s.hash,
			ReadyTimeout: -time.Second},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := launcher.Start(context.Background(), cfg); !errors.Is(err, launcher.ErrConfig) {
				t.Fatalf("Start: %v, want ErrConfig", err)
			}
		})
	}
}

func TestManifestAcceptedAndRejected(t *testing.T) {
	s := selfInfo(t)
	pub, priv, err := manifest.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	_, otherPriv, err := manifest.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(s.path)
	dir := filepath.Dir(s.path)

	// Good: the entry for this platform, signed by the expected key.
	data, sig := signedManifest(t, priv, runtime.GOOS, runtime.GOARCH, name, s.hash, s.size)
	cfg := stubConfig(t, "ok")
	cfg.Binary, cfg.SHA256 = "", secureexec.Hash{}
	cfg.Manifest = &launcher.ManifestSource{Data: data, Signature: sig, PublicKey: pub, Dir: dir}
	srv, err := launcher.Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("manifest start: %v", err)
	}
	if err := srv.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Bad signature: signed by another key.
	_, badSig := signedManifest(t, otherPriv, runtime.GOOS, runtime.GOARCH, name, s.hash, s.size)
	cfg.Manifest = &launcher.ManifestSource{Data: data, Signature: badSig, PublicKey: pub, Dir: dir}
	_, err = launcher.Start(context.Background(), cfg)
	if !errors.Is(err, launcher.ErrManifest) || !errors.Is(err, manifest.ErrBadSignature) {
		t.Fatalf("bad signature: %v, want ErrManifest wrapping ErrBadSignature", err)
	}

	// Tampered manifest bytes under a valid signature for other bytes.
	tampered := bytes.Replace(data, []byte("v0.0.0-test"), []byte("v9.9.9-evil"), 1)
	cfg.Manifest = &launcher.ManifestSource{Data: tampered, Signature: sig, PublicKey: pub, Dir: dir}
	if _, err = launcher.Start(context.Background(), cfg); !errors.Is(err, manifest.ErrBadSignature) {
		t.Fatalf("tampered manifest: %v, want ErrBadSignature", err)
	}

	// No entry for this platform.
	data2, sig2 := signedManifest(t, priv, "plan9", "386", name, s.hash, s.size)
	cfg.Manifest = &launcher.ManifestSource{Data: data2, Signature: sig2, PublicKey: pub, Dir: dir}
	if _, err = launcher.Start(context.Background(), cfg); !errors.Is(err, launcher.ErrNoArtifact) {
		t.Fatalf("missing platform: %v, want ErrNoArtifact", err)
	}

	// No key passed: the embedded release key is a placeholder, so Start
	// refuses instead of trusting anything.
	cfg.Manifest = &launcher.ManifestSource{Data: data, Signature: sig, Dir: dir}
	if _, err = launcher.Start(context.Background(), cfg); !errors.Is(err, launcher.ErrNoReleaseKey) {
		t.Fatalf("no key: %v, want ErrNoReleaseKey", err)
	}
}

func TestReleaseKeyIsPlaceholderAndRefused(t *testing.T) {
	pub, err := launcher.ReleasePublicKey()
	if !errors.Is(err, launcher.ErrNoReleaseKey) {
		t.Fatalf("ReleasePublicKey = %x, %v; want ErrNoReleaseKey", pub, err)
	}
	if pub != nil {
		t.Fatalf("ReleasePublicKey returned a key with the error: %x", pub)
	}
}

func TestReadyTimeoutKillsAndReports(t *testing.T) {
	cfg := stubConfig(t, "no-ready")
	// Long enough that a loaded machine has started the stub and captured its
	// first stderr line before the timeout fires.
	cfg.ReadyTimeout = 2 * time.Second
	_, err := launcher.Start(context.Background(), cfg)
	if !errors.Is(err, launcher.ErrReadyTimeout) {
		t.Fatalf("Start: %v, want ErrReadyTimeout", err)
	}
	var rt *launcher.ReadyTimeoutError
	if !errors.As(err, &rt) {
		t.Fatalf("error %T is not *ReadyTimeoutError", err)
	}
	if !strings.Contains(rt.Output, "booting") {
		t.Fatalf("output %q does not carry the server's stderr", rt.Output)
	}
}

func TestExitBeforeReadyIsStartError(t *testing.T) {
	_, err := launcher.Start(context.Background(), stubConfig(t, "exit-early"))
	var se *launcher.StartError
	if !errors.As(err, &se) {
		t.Fatalf("Start: %v, want *StartError", err)
	}
	if !strings.Contains(se.Output, "boom") {
		t.Fatalf("output %q does not carry the stderr", se.Output)
	}
}

func TestStartContextBoundsOnlyTheStart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := launcher.Start(ctx, stubConfig(t, "no-ready"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Start: %v, want DeadlineExceeded", err)
	}
}

func TestStopForcedKillsAfterTimeout(t *testing.T) {
	cfg := stubConfig(t, "ignore-eof")
	cfg.StopTimeout = 300 * time.Millisecond
	srv, err := launcher.Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err = srv.Stop(context.Background())
	if !errors.Is(err, launcher.ErrStopTimeout) {
		t.Fatalf("Stop: %v, want ErrStopTimeout", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("forced stop took %v", d)
	}
	select {
	case <-srv.Done():
	default:
		t.Fatal("Done not closed after a forced stop")
	}
	if srv.Err() == nil {
		t.Fatal("Err() is nil after a killed server")
	}
}

func TestStopWithCanceledContextKills(t *testing.T) {
	srv, err := launcher.Start(context.Background(), stubConfig(t, "ignore-eof"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := srv.Stop(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Stop: %v, want context.Canceled", err)
	}
}

func TestCrashAfterReadyReportsExitStatus(t *testing.T) {
	srv, err := launcher.Start(context.Background(), stubConfig(t, "crash-after-ready"))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-srv.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("server did not exit")
	}
	var ee *exec.ExitError
	if !errors.As(srv.Err(), &ee) || ee.ExitCode() != 5 {
		t.Fatalf("Err() = %v, want exit status 5", srv.Err())
	}
	if err := srv.Close(); !errors.As(err, &ee) {
		t.Fatalf("Close after crash = %v, want the exit status", err)
	}
}

func TestOutputCaptureForwardsEverythingButReadyLine(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cfg := stubConfig(t, "noisy")
	cfg.Stdout = &stdout
	cfg.Stderr = &stderr
	srv, err := launcher.Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// Stop returns after the process exited and its output was copied.
	if got, want := stdout.String(), "chatter\nafter\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if got, want := stderr.String(), "warn\n"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
}

func TestArgumentsCarryPolicyAndLifeline(t *testing.T) {
	cfg := stubConfig(t, "ok")
	cfg.Listener = "argtest"
	argsFile := withFile(t, &cfg, envArgs)
	cfg.AllowDir = t.TempDir()
	cfg.Aliases = map[string]string{"b": "/x/y.db", "a": "/p"}
	cfg.MaxSessions = 7
	cfg.Perm = 0o640
	cfg.ExtraArgs = []string{"--heartbeat-ms", "20"}
	srv, err := launcher.Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	b, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"argtest",
		"--allow-dir", cfg.AllowDir,
		"--alias", "a=/p",
		"--alias", "b=/x/y.db",
		"--max-sessions", "7",
		"--perm", "640",
		"--parent-stdin",
		"--heartbeat-ms", "20",
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("arguments = %q\nwant %q", got, want)
	}
}

func TestDisableLifelineOmitsFlagAndStopsWithSIGTERM(t *testing.T) {
	cfg := stubConfig(t, "ok")
	argsFile := withFile(t, &cfg, envArgs)
	cfg.DisableLifeline = true
	srv, err := launcher.Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		// Stop kills there: no graceful signal exists, so only the exit is checked.
		_ = srv.Stop(context.Background())
		return
	}
	if err := srv.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	b, _ := os.ReadFile(argsFile)
	if strings.Contains(string(b), "--parent-stdin") {
		t.Fatalf("DisableLifeline still passed --parent-stdin: %s", b)
	}
}

func TestGeneratedListenerName(t *testing.T) {
	cfg := stubConfig(t, "ok")
	cfg.Listener = ""
	srv, err := launcher.Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	n := srv.Listener()
	if !strings.HasPrefix(n, "hqqsql-") || len(n) > 31 {
		t.Fatalf("generated listener %q", n)
	}
}

func TestOpenManagedClosesDBThenServer(t *testing.T) {
	m, err := launcher.OpenManaged(context.Background(), stubConfig(t, "ok"),
		launcher.WithDatabase("main"))
	if err != nil {
		t.Fatal(err)
	}
	if m.DB == nil || m.Server == nil {
		t.Fatal("OpenManaged returned a nil DB or Server")
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-m.Server.Done():
	default:
		t.Fatal("server still running after Managed.Close")
	}
}

func TestOpenWithoutDatabaseStartsNothing(t *testing.T) {
	cfg := stubConfig(t, "ok")
	mark := withFile(t, &cfg, envMark)
	_, _, err := launcher.Open(context.Background(), cfg)
	if !errors.Is(err, launcher.ErrNoDatabase) {
		t.Fatalf("Open: %v, want ErrNoDatabase", err)
	}
	if exists(mark) {
		t.Fatal("the server started without a database")
	}
}
