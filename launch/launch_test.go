package launch_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"gosuda.org/hqq/internal/launchtest"
	"gosuda.org/hqq/launch"
	"gosuda.org/hqq/secureexec"
	"gosuda.org/hqq/secureexec/manifest"
)

// stubConfig returns a Config that starts the test binary as the stub in mode,
// verified by its own digest. The stub gets its listener name as the first
// argument and the test's environment plus extraEnv.
func stubConfig(t *testing.T, mode string, extraEnv ...string) launch.Config {
	t.Helper()
	s := launchtest.Self(t)
	listener := launchtest.NewListener()
	return launch.Config{
		Binary:       s.Path,
		SHA256:       s.Hash,
		Listener:     listener,
		Args:         []string{listener},
		Env:          launchtest.Env(mode, extraEnv...),
		ReadyTimeout: 10 * time.Second,
		StopTimeout:  5 * time.Second,
	}
}

// start starts the server and stops it when the test ends.
func start(t *testing.T, cfg launch.Config, args ...string) *launch.Server {
	t.Helper()
	srv, err := launch.Start(context.Background(), cfg, args...)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

func TestStartRunsVerifiedBinaryAndStopsGracefully(t *testing.T) {
	cfg := stubConfig(t, "ok")
	srv, err := launch.Start(context.Background(), cfg)
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
	report, reportEnv := launchtest.ReportFile(t)
	cfg := stubConfig(t, "ok", reportEnv)
	cfg.SHA256[0] ^= 0xff
	_, err := launch.Start(context.Background(), cfg)
	if !errors.Is(err, secureexec.ErrHashMismatch) {
		t.Fatalf("Start: %v, want ErrHashMismatch", err)
	}
	var se *launch.StartError
	if !errors.As(err, &se) {
		t.Fatalf("error %T is not *StartError", err)
	}
	if launchtest.Exists(report) {
		t.Fatal("the server ran despite the hash mismatch")
	}
}

func TestConfigRefusesUnverifiedAndAmbiguous(t *testing.T) {
	s := launchtest.Self(t)
	pub, priv, err := manifest.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	data, sig := launchtest.SignedManifest(t, priv, runtime.GOOS, runtime.GOARCH, filepath.Base(s.Path), s.Hash, s.Size)
	signed := func() *launch.ManifestSource {
		return &launch.ManifestSource{Data: data, Signature: sig, PublicKey: pub, Dir: filepath.Dir(s.Path)}
	}
	cases := map[string]func(*launch.Config){
		"no binary":           func(c *launch.Config) { c.Binary = "" },
		"no hash":             func(c *launch.Config) { c.SHA256 = secureexec.Hash{} },
		"binary and manifest": func(c *launch.Config) { c.Manifest = signed() },
		"hash and manifest":   func(c *launch.Config) { c.Binary, c.Manifest = "", signed() },
		"manifest without dir": func(c *launch.Config) {
			c.Binary, c.SHA256, c.Manifest = "", secureexec.Hash{}, signed()
			c.Manifest.Dir = ""
		},
		"short public key": func(c *launch.Config) {
			c.Binary, c.SHA256, c.Manifest = "", secureexec.Hash{}, signed()
			c.Manifest.PublicKey = pub[:5]
		},
		"nil public key": func(c *launch.Config) {
			c.Binary, c.SHA256, c.Manifest = "", secureexec.Hash{}, signed()
			c.Manifest.PublicKey = nil
		},
		"no listener":               func(c *launch.Config) { c.Listener = "" },
		"listener with slash":       func(c *launch.Config) { c.Listener = "a/b" },
		"listener starts with dash": func(c *launch.Config) { c.Listener = "-x" },
		"listener too long":         func(c *launch.Config) { c.Listener = strings.Repeat("a", 32) },
		"negative ready timeout":    func(c *launch.Config) { c.ReadyTimeout = -time.Second },
		"negative stop timeout":     func(c *launch.Config) { c.StopTimeout = -time.Second },
		"lifeline flag when disabled": func(c *launch.Config) {
			c.DisableLifeline, c.LifelineFlag = true, "--parent-stdin"
		},
		"lifeline flag that is not a flag": func(c *launch.Config) { c.LifelineFlag = "parent-stdin" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			// A stub that runs would be a bug; it would stop with its parent.
			cfg := stubConfig(t, "ok")
			mutate(&cfg)
			if _, err := launch.Start(context.Background(), cfg); !errors.Is(err, launch.ErrConfig) {
				t.Fatalf("Start: %v, want ErrConfig", err)
			}
		})
	}
}

func TestManifest(t *testing.T) {
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
	good, goodSig := launchtest.SignedManifest(t, priv, runtime.GOOS, runtime.GOARCH, name, s.Hash, s.Size)

	// run starts the stub from a manifest and reports whether it ran.
	run := func(t *testing.T, data, sig []byte) (srv *launch.Server, ran bool, err error) {
		t.Helper()
		report, reportEnv := launchtest.ReportFile(t)
		cfg := stubConfig(t, "ok", reportEnv)
		cfg.Binary, cfg.SHA256 = "", secureexec.Hash{}
		cfg.Manifest = &launch.ManifestSource{Data: data, Signature: sig, PublicKey: pub, Dir: dir}
		srv, err = launch.Start(context.Background(), cfg)
		if err == nil {
			t.Cleanup(func() { _ = srv.Close() })
		}
		return srv, launchtest.Exists(report), err
	}

	t.Run("entry for this platform is started", func(t *testing.T) {
		srv, ran, err := run(t, good, goodSig)
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		if !ran {
			t.Fatal("the stub did not run")
		}
		if err := srv.Stop(context.Background()); err != nil {
			t.Fatalf("Stop: %v", err)
		}
	})
	t.Run("signature by another key", func(t *testing.T) {
		_, badSig := launchtest.SignedManifest(t, otherPriv, runtime.GOOS, runtime.GOARCH, name, s.Hash, s.Size)
		_, ran, err := run(t, good, badSig)
		if !errors.Is(err, launch.ErrManifest) || !errors.Is(err, manifest.ErrBadSignature) {
			t.Fatalf("Start: %v, want ErrManifest wrapping ErrBadSignature", err)
		}
		if ran {
			t.Fatal("the stub ran under a rejected manifest")
		}
	})
	t.Run("manifest edited after signing", func(t *testing.T) {
		tampered := bytes.Replace(good, []byte("v0.0.0-test"), []byte("v9.9.9-evil"), 1)
		_, ran, err := run(t, tampered, goodSig)
		if !errors.Is(err, launch.ErrManifest) || !errors.Is(err, manifest.ErrBadSignature) {
			t.Fatalf("Start: %v, want ErrManifest wrapping ErrBadSignature", err)
		}
		if ran {
			t.Fatal("the stub ran under a tampered manifest")
		}
	})
	t.Run("no entry for this platform", func(t *testing.T) {
		data, sig := launchtest.SignedManifest(t, priv, "plan9", "386", name, s.Hash, s.Size)
		_, ran, err := run(t, data, sig)
		if !errors.Is(err, launch.ErrNoArtifact) {
			t.Fatalf("Start: %v, want ErrNoArtifact", err)
		}
		if ran {
			t.Fatal("the stub ran without a manifest entry")
		}
	})
	t.Run("signed digest differs from the file", func(t *testing.T) {
		wrong := s.Hash
		wrong[0] ^= 0xff
		data, sig := launchtest.SignedManifest(t, priv, runtime.GOOS, runtime.GOARCH, name, wrong, s.Size)
		_, ran, err := run(t, data, sig)
		if !errors.Is(err, secureexec.ErrHashMismatch) {
			t.Fatalf("Start: %v, want ErrHashMismatch", err)
		}
		if ran {
			t.Fatal("the stub ran although the file does not match the signed digest")
		}
	})
}

func TestReadyTimeoutKillsChild(t *testing.T) {
	report, reportEnv := launchtest.ReportFile(t)
	cfg := stubConfig(t, "no-ready", reportEnv)
	// Long enough that a loaded machine has started the stub and captured its
	// first stderr line before the timeout fires.
	cfg.ReadyTimeout = 2 * time.Second
	_, err := launch.Start(context.Background(), cfg)
	if !errors.Is(err, launch.ErrReadyTimeout) {
		t.Fatalf("Start: %v, want ErrReadyTimeout", err)
	}
	var rt *launch.ReadyTimeoutError
	if !errors.As(err, &rt) {
		t.Fatalf("error %T is not *ReadyTimeoutError", err)
	}
	if rt.Listener != cfg.Listener || rt.Timeout != cfg.ReadyTimeout {
		t.Fatalf("ReadyTimeoutError = {%q, %v}, want {%q, %v}", rt.Listener, rt.Timeout, cfg.Listener, cfg.ReadyTimeout)
	}
	if !strings.Contains(rt.Output, "booting") {
		t.Fatalf("output %q does not carry the server's stderr", rt.Output)
	}
	if err := launchtest.ProbeProcess(launchtest.ReadReport(t, report).PID); err == nil {
		t.Fatal("the server still runs after the ready timeout")
	}
}

func TestLinesThatAreNotTheReadyLineDoNotMakeTheServerReady(t *testing.T) {
	cases := map[string]struct {
		mode, wantOutput string
	}{
		"other listener and longer name": {"wrong-ready", "ready other"},
		"no newline":                     {"unterminated-ready", "ready lnch"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := stubConfig(t, c.mode)
			cfg.ReadyTimeout = 2 * time.Second
			_, err := launch.Start(context.Background(), cfg)
			var rt *launch.ReadyTimeoutError
			if !errors.As(err, &rt) {
				t.Fatalf("Start: %v, want *ReadyTimeoutError", err)
			}
			if !strings.Contains(rt.Output, c.wantOutput) {
				t.Fatalf("output %q does not show the stub's line %q", rt.Output, c.wantOutput)
			}
		})
	}
}

func TestExitBeforeReadyIsStartError(t *testing.T) {
	_, err := launch.Start(context.Background(), stubConfig(t, "exit-early"))
	var se *launch.StartError
	if !errors.As(err, &se) {
		t.Fatalf("Start: %v, want *StartError", err)
	}
	if !strings.Contains(se.Output, "boom") {
		t.Fatalf("output %q does not carry the stderr", se.Output)
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 3 {
		t.Fatalf("Start: %v, want the exit status 3", err)
	}
}

func TestStartContextDeadlineEndsTheStart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := launch.Start(ctx, stubConfig(t, "no-ready"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Start: %v, want DeadlineExceeded", err)
	}
}

func TestStartCanceledContextKillsChild(t *testing.T) {
	report, reportEnv := launchtest.ReportFile(t)
	cfg := stubConfig(t, "no-ready", reportEnv)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := launch.Start(ctx, cfg)
		result <- err
	}()
	launchtest.WaitForFile(t, report, 10*time.Second)
	cancel()
	var err error
	select {
	case err = <-result:
	case <-time.After(10 * time.Second):
		t.Fatal("Start did not return after the context was canceled")
	}
	var se *launch.StartError
	if !errors.As(err, &se) || !errors.Is(err, context.Canceled) {
		t.Fatalf("Start: %v, want *StartError wrapping context.Canceled", err)
	}
	if err := launchtest.ProbeProcess(launchtest.ReadReport(t, report).PID); err == nil {
		t.Fatal("the server still runs after the start was canceled")
	}
}

func TestStartContextBoundsOnlyTheStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	srv, err := launch.Start(ctx, stubConfig(t, "ok"))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-srv.Done():
		t.Fatal("canceling the start context stopped the running server")
	case <-time.After(100 * time.Millisecond):
	}
	if err := srv.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestStopKillsAfterStopTimeout(t *testing.T) {
	cfg := stubConfig(t, "ignore-eof")
	cfg.StopTimeout = 300 * time.Millisecond
	srv := start(t, cfg)
	started := time.Now()
	err := srv.Stop(context.Background())
	if !errors.Is(err, launch.ErrStopTimeout) {
		t.Fatalf("Stop: %v, want ErrStopTimeout", err)
	}
	if d := time.Since(started); d > 5*time.Second {
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
	if err := launchtest.ProbeProcess(srv.PID()); err == nil {
		t.Fatal("the server still runs after the forced stop")
	}
}

func TestStopWithCanceledContextKills(t *testing.T) {
	srv := start(t, stubConfig(t, "ignore-eof"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := srv.Stop(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Stop: %v, want context.Canceled", err)
	}
	select {
	case <-srv.Done():
	default:
		t.Fatal("Done not closed after Stop with a canceled context")
	}
}

func TestCrashAfterReadyReportsExitStatus(t *testing.T) {
	srv, err := launch.Start(context.Background(), stubConfig(t, "crash-after-ready"))
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

func TestOutputIsForwardedExceptTheReadyLine(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cfg := stubConfig(t, "noisy")
	cfg.Stdout = &stdout
	cfg.Stderr = &stderr
	srv, err := launch.Start(context.Background(), cfg)
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

func TestErrorOutputKeepsOnlyTheEndOfTheServerOutput(t *testing.T) {
	_, err := launch.Start(context.Background(), stubConfig(t, "flood"))
	var se *launch.StartError
	if !errors.As(err, &se) {
		t.Fatalf("Start: %v, want *StartError", err)
	}
	if !strings.HasSuffix(se.Output, "END\n") {
		t.Fatalf("output ends with %q, want the end of the server's output", se.Output[max(0, len(se.Output)-16):])
	}
	// The server wrote about 5 KB.
	if len(se.Output) > 4096 {
		t.Fatalf("StartError.Output has %d bytes, want a bounded tail", len(se.Output))
	}
	if msg := err.Error(); len(msg) > 1536 {
		t.Fatalf("error message has %d bytes, want the output in it bounded", len(msg))
	}
}

func TestCommandLineIsArgsLifelineFlagThenStartArguments(t *testing.T) {
	report, reportEnv := launchtest.ReportFile(t)
	cfg := stubConfig(t, "ok", reportEnv)
	cfg.Args = append(cfg.Args, "--first")
	start(t, cfg, "--", "operand")
	got := launchtest.ReadReport(t, report).Args
	want := []string{cfg.Listener, "--first", launch.DefaultLifelineFlag, "--", "operand"}
	if !slices.Equal(got, want) {
		t.Fatalf("arguments = %q, want %q", got, want)
	}
}

func TestCustomLifelineFlagIsPassedAndTiesTheServerToThePipe(t *testing.T) {
	const flag = "--exit-on-stdin-eof"
	report, reportEnv := launchtest.ReportFile(t)
	cfg := stubConfig(t, "ok", reportEnv, launchtest.EnvLifelineFlag+"="+flag)
	cfg.LifelineFlag = flag
	srv, err := launch.Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	got := launchtest.ReadReport(t, report).Args
	want := []string{cfg.Listener, flag}
	if !slices.Equal(got, want) {
		t.Fatalf("arguments = %q, want %q", got, want)
	}
	// The stub leaves on end of file only when it saw its lifeline flag.
	if err := srv.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestDisableLifelineOmitsFlagAndStopsWithSignal(t *testing.T) {
	report, reportEnv := launchtest.ReportFile(t)
	cfg := stubConfig(t, "ok", reportEnv)
	cfg.DisableLifeline = true
	srv, err := launch.Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := launchtest.ReadReport(t, report).Args; !slices.Equal(got, []string{cfg.Listener}) {
		t.Fatalf("arguments = %q, want only the listener", got)
	}
	if runtime.GOOS == "windows" {
		// Stop kills there when no console is attached: only the exit is checked.
		_ = srv.Stop(context.Background())
		return
	}
	if err := srv.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestEnvIsTheServersEnvironmentAndNilInherits(t *testing.T) {
	t.Setenv("LAUNCH_TEST_PARENT_ONLY", "1")
	t.Run("explicit", func(t *testing.T) {
		report, reportEnv := launchtest.ReportFile(t)
		cfg := stubConfig(t, "ok")
		cfg.Env = []string{launchtest.EnvStub + "=ok", reportEnv, "LAUNCH_TEST_CANARY=1"}
		if root := os.Getenv("SYSTEMROOT"); root != "" {
			cfg.Env = append(cfg.Env, "SYSTEMROOT="+root)
		}
		start(t, cfg)
		env := launchtest.ReadReport(t, report).Env
		if !slices.Contains(env, "LAUNCH_TEST_CANARY=1") {
			t.Fatalf("the server's environment lacks Config.Env: %q", env)
		}
		if slices.Contains(env, "LAUNCH_TEST_PARENT_ONLY=1") {
			t.Fatal("the server inherited the caller's environment although Config.Env is set")
		}
	})
	t.Run("nil", func(t *testing.T) {
		report, reportEnv := launchtest.ReportFile(t)
		cfg := stubConfig(t, "ok")
		cfg.Env = nil
		t.Setenv(launchtest.EnvStub, "ok")
		t.Setenv(launchtest.EnvReport, strings.TrimPrefix(reportEnv, launchtest.EnvReport+"="))
		start(t, cfg)
		if env := launchtest.ReadReport(t, report).Env; !slices.Contains(env, "LAUNCH_TEST_PARENT_ONLY=1") {
			t.Fatalf("the server did not inherit the caller's environment: %q", env)
		}
	})
}

func TestDirIsTheServersWorkingDirectory(t *testing.T) {
	report, reportEnv := launchtest.ReportFile(t)
	cfg := stubConfig(t, "ok", reportEnv)
	cfg.Dir = t.TempDir()
	start(t, cfg)
	got, err := filepath.EvalSymlinks(launchtest.ReadReport(t, report).Dir)
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(cfg.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("working directory = %q, want %q", got, want)
	}
}

func TestNewListenerName(t *testing.T) {
	pattern := regexp.MustCompile(`^svc-[0-9a-f]{12}$`)
	a, err := launch.NewListenerName("svc")
	if err != nil {
		t.Fatal(err)
	}
	b, err := launch.NewListenerName("svc")
	if err != nil {
		t.Fatal(err)
	}
	if !pattern.MatchString(a) || !pattern.MatchString(b) || a == b {
		t.Fatalf("names %q and %q: want two different svc-<12 hex digits>", a, b)
	}
	for _, prefix := range []string{"", "a/b", strings.Repeat("a", 19)} {
		if name, err := launch.NewListenerName(prefix); !errors.Is(err, launch.ErrConfig) {
			t.Errorf("NewListenerName(%q) = %q, %v; want ErrConfig", prefix, name, err)
		}
	}
}

// TestLifelineClosesWhenParentDies starts the stub from a helper process,
// SIGKILLs the helper, and waits for the stub to exit: its stdin reached end of
// file because the kernel closed the helper's write end of the lifeline pipe.
func TestLifelineClosesWhenParentDies(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	helper := exec.Command(exe)
	helper.Env = append(os.Environ(), envParent+"=1")
	helper.Stderr = os.Stderr
	stdout, err := helper.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = helper.Process.Kill() }()

	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("helper output: %v", err)
	}
	var pid int
	if _, err := fmt.Sscanf(strings.TrimSpace(line), "pid %d", &pid); err != nil {
		t.Fatalf("helper line %q: %v", line, err)
	}
	if err := launchtest.ProbeProcess(pid); err != nil {
		t.Fatalf("server %d not running while its parent lives: %v", pid, err)
	}

	if err := helper.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = helper.Wait()
	launchtest.WaitGone(t, pid, 10*time.Second)
}
