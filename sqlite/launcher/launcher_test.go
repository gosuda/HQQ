package launcher_test

// The process mechanics (verification, ready line, lifeline, stop) are tested
// in launch against the same stub. These tests cover what this package adds:
// the SQLite command line, the embedded release key, the mapping of Config
// onto launch.Config, and Open.

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"gosuda.org/hqq/internal/launchtest"
	"gosuda.org/hqq/launch"
	"gosuda.org/hqq/secureexec"
	"gosuda.org/hqq/secureexec/manifest"
	"gosuda.org/hqq/sqlite/launcher"
)

// e2eServerFlags are the link flags of the real server in the tests: quick
// heartbeats, so a dead peer is noticed within a few hundred milliseconds.
var e2eServerFlags = []string{"--heartbeat-ms", "20", "--peer-timeout-ms", "300"}

// stubConfig returns a Config that starts the test binary as the stub in mode,
// verified by its own digest. The stub's environment is the test's, plus
// extraEnv.
func stubConfig(t *testing.T, mode string, extraEnv ...string) launcher.Config {
	t.Helper()
	s := launchtest.Self(t)
	return launcher.Config{
		Binary:       s.Path,
		SHA256:       s.Hash,
		Listener:     launchtest.NewListener(),
		Env:          launchtest.Env(mode, extraEnv...),
		ReadyTimeout: 10 * time.Second,
		StopTimeout:  5 * time.Second,
	}
}

func TestArgumentsCarryPolicyAndLifeline(t *testing.T) {
	report, reportEnv := launchtest.ReportFile(t)
	cfg := stubConfig(t, "ok", reportEnv)
	cfg.Listener = "argtest"
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
	if got := launchtest.ReadReport(t, report).Args; !slices.Equal(got, want) {
		t.Fatalf("arguments = %q\nwant %q", got, want)
	}
}

func TestDisableLifelineOmitsFlag(t *testing.T) {
	report, reportEnv := launchtest.ReportFile(t)
	cfg := stubConfig(t, "ok", reportEnv)
	cfg.DisableLifeline = true
	srv, err := launcher.Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if got := launchtest.ReadReport(t, report).Args; slices.Contains(got, "--parent-stdin") {
		t.Fatalf("DisableLifeline still passed --parent-stdin: %q", got)
	}
}

func TestGeneratedListenerName(t *testing.T) {
	report, reportEnv := launchtest.ReportFile(t)
	cfg := stubConfig(t, "ok", reportEnv)
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
	if got := launchtest.ReadReport(t, report).Args; len(got) == 0 || got[0] != n {
		t.Fatalf("first argument = %q, want the listener %q", got, n)
	}
}

func TestConfigRefusesInvalidSettings(t *testing.T) {
	cases := map[string]func(*launcher.Config){
		"no hash":                func(c *launcher.Config) { c.SHA256 = secureexec.Hash{} },
		"listener with slash":    func(c *launcher.Config) { c.Listener = "a/b" },
		"negative ready timeout": func(c *launcher.Config) { c.ReadyTimeout = -time.Second },
		"alias with equals":      func(c *launcher.Config) { c.Aliases = map[string]string{"a=b": "/x"} },
		"alias without name":     func(c *launcher.Config) { c.Aliases = map[string]string{"": "/x"} },
		"alias without path":     func(c *launcher.Config) { c.Aliases = map[string]string{"a": ""} },
		"too many sessions":      func(c *launcher.Config) { c.MaxSessions = 70000 },
		"negative sessions":      func(c *launcher.Config) { c.MaxSessions = -1 },
		"perm with type bits":    func(c *launcher.Config) { c.Perm = 0o1000 | 0o600 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			// A stub that runs would be a bug; it would stop with its parent.
			cfg := stubConfig(t, "ok")
			mutate(&cfg)
			if _, err := launcher.Start(context.Background(), cfg); !errors.Is(err, launcher.ErrConfig) {
				t.Fatalf("Start: %v, want ErrConfig", err)
			}
		})
	}
}

func TestHashMismatchStartsNothing(t *testing.T) {
	report, reportEnv := launchtest.ReportFile(t)
	cfg := stubConfig(t, "ok", reportEnv)
	cfg.SHA256[0] ^= 0xff
	_, err := launcher.Start(context.Background(), cfg)
	if !errors.Is(err, secureexec.ErrHashMismatch) {
		t.Fatalf("Start: %v, want ErrHashMismatch", err)
	}
	var se *launcher.StartError
	if !errors.As(err, &se) {
		t.Fatalf("error %T is not *StartError", err)
	}
	if launchtest.Exists(report) {
		t.Fatal("the server ran despite the hash mismatch")
	}
}

func TestManifestSourceReachesTheLauncher(t *testing.T) {
	s := launchtest.Self(t)
	pub, priv, err := manifest.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	_, otherPriv, err := manifest.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(s.Path)
	dir := filepath.Dir(s.Path)
	data, sig := launchtest.SignedManifest(t, priv, runtime.GOOS, runtime.GOARCH, name, s.Hash, s.Size)
	manifestConfig := func(t *testing.T, src *launcher.ManifestSource) launcher.Config {
		cfg := stubConfig(t, "ok")
		cfg.Binary, cfg.SHA256 = "", secureexec.Hash{}
		cfg.Manifest = src
		return cfg
	}

	srv, err := launcher.Start(context.Background(), manifestConfig(t,
		&launcher.ManifestSource{Data: data, Signature: sig, PublicKey: pub, Dir: dir}))
	if err != nil {
		t.Fatalf("manifest start: %v", err)
	}
	if err := srv.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	_, badSig := launchtest.SignedManifest(t, otherPriv, runtime.GOOS, runtime.GOARCH, name, s.Hash, s.Size)
	_, err = launcher.Start(context.Background(), manifestConfig(t,
		&launcher.ManifestSource{Data: data, Signature: badSig, PublicKey: pub, Dir: dir}))
	if !errors.Is(err, launcher.ErrManifest) || !errors.Is(err, manifest.ErrBadSignature) {
		t.Fatalf("bad signature: %v, want ErrManifest wrapping ErrBadSignature", err)
	}

	otherData, otherSig := launchtest.SignedManifest(t, priv, "plan9", "386", name, s.Hash, s.Size)
	_, err = launcher.Start(context.Background(), manifestConfig(t,
		&launcher.ManifestSource{Data: otherData, Signature: otherSig, PublicKey: pub, Dir: dir}))
	if !errors.Is(err, launcher.ErrNoArtifact) {
		t.Fatalf("missing platform: %v, want ErrNoArtifact", err)
	}
}

// The embedded release key is a placeholder, so a manifest without a key is
// refused instead of trusting anything.
func TestManifestWithoutKeyUsesTheEmbeddedKeyAndIsRefused(t *testing.T) {
	s := launchtest.Self(t)
	_, priv, err := manifest.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	data, sig := launchtest.SignedManifest(t, priv, runtime.GOOS, runtime.GOARCH, filepath.Base(s.Path), s.Hash, s.Size)
	report, reportEnv := launchtest.ReportFile(t)
	cfg := stubConfig(t, "ok", reportEnv)
	cfg.Binary, cfg.SHA256 = "", secureexec.Hash{}
	cfg.Manifest = &launcher.ManifestSource{Data: data, Signature: sig, Dir: filepath.Dir(s.Path)}
	_, err = launcher.Start(context.Background(), cfg)
	if !errors.Is(err, launcher.ErrNoReleaseKey) {
		t.Fatalf("Start: %v, want ErrNoReleaseKey", err)
	}
	if launchtest.Exists(report) {
		t.Fatal("the server ran without a release key")
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

func TestTimeoutsReachTheLauncher(t *testing.T) {
	t.Run("ready", func(t *testing.T) {
		cfg := stubConfig(t, "no-ready")
		cfg.ReadyTimeout = 300 * time.Millisecond
		_, err := launcher.Start(context.Background(), cfg)
		var rt *launcher.ReadyTimeoutError
		if !errors.Is(err, launcher.ErrReadyTimeout) || !errors.As(err, &rt) || rt.Timeout != cfg.ReadyTimeout {
			t.Fatalf("Start: %v, want a *ReadyTimeoutError after %v", err, cfg.ReadyTimeout)
		}
	})
	t.Run("stop", func(t *testing.T) {
		cfg := stubConfig(t, "ignore-eof")
		cfg.StopTimeout = 300 * time.Millisecond
		srv, err := launcher.Start(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := srv.Stop(context.Background()); !errors.Is(err, launcher.ErrStopTimeout) {
			t.Fatalf("Stop: %v, want ErrStopTimeout", err)
		}
	})
}

func TestOutputWritersReachTheLauncher(t *testing.T) {
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
	if got, want := stdout.String(), "chatter\nafter\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if got, want := stderr.String(), "warn\n"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
}

// The errors of this package are the ones of launch, so a caller can match
// either.
func TestErrorsAreThoseOfLaunch(t *testing.T) {
	pairs := []struct {
		name         string
		mine, theirs error
	}{
		{"ErrConfig", launcher.ErrConfig, launch.ErrConfig},
		{"ErrManifest", launcher.ErrManifest, launch.ErrManifest},
		{"ErrNoArtifact", launcher.ErrNoArtifact, launch.ErrNoArtifact},
		{"ErrReadyTimeout", launcher.ErrReadyTimeout, launch.ErrReadyTimeout},
		{"ErrStopTimeout", launcher.ErrStopTimeout, launch.ErrStopTimeout},
	}
	for _, p := range pairs {
		if p.mine != p.theirs {
			t.Errorf("launcher.%s is not launch.%s", p.name, p.name)
		}
	}
	cfg := stubConfig(t, "ok")
	cfg.Listener = "a/b"
	if _, err := launcher.Start(context.Background(), cfg); !errors.Is(err, launch.ErrConfig) {
		t.Fatalf("Start: %v, want launch.ErrConfig", err)
	}
	var _ *launch.Server = (*launcher.Server)(nil)
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
	report, reportEnv := launchtest.ReportFile(t)
	cfg := stubConfig(t, "ok", reportEnv)
	_, _, err := launcher.Open(context.Background(), cfg)
	if !errors.Is(err, launcher.ErrNoDatabase) {
		t.Fatalf("Open: %v, want ErrNoDatabase", err)
	}
	if launchtest.Exists(report) {
		t.Fatal("the server started without a database")
	}
}
