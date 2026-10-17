package supervisor

import (
	"context"
	"crypto/ed25519"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"gosuda.org/hqq/secureexec"
	"gosuda.org/hqq/secureexec/manifest"
)

// skipWithoutSignals skips tests that depend on POSIX stop signals: Windows
// has no graceful signal, so services stop through stop_method or are killed.
func skipWithoutSignals(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("stop signals are POSIX-only; on Windows services stop via stop_method or are killed")
	}
}

func TestStartReadyAndStopBySignal(t *testing.T) {
	skipWithoutSignals(t)
	name := "svc"
	cfg := Config{Services: []ServiceSpec{fakeSpec(name, shortName(), "serve", nil)}}
	sv, rec := newTestSupervisor(t, cfg)
	ctx := ctxT(t, 20*time.Second)

	if err := sv.Start(ctx, name); err != nil {
		t.Fatalf("Start: %v", err)
	}
	st := waitState(t, sv, name, StateReady)
	if st.PID == 0 || st.StartedAt == nil {
		t.Fatalf("ready status lacks pid/start time: %+v", st)
	}
	if st.Health == nil || !st.Health.OK {
		t.Fatalf("ready status lacks a passing health result: %+v", st.Health)
	}
	if err := sv.Start(ctx, name); err != nil {
		t.Fatalf("second Start of a running service: %v", err)
	}
	if err := sv.Stop(ctx, name); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	st, err := sv.Status(name)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != StateStopped || st.PID != 0 {
		t.Fatalf("after Stop: state %s pid %d", st.State, st.PID)
	}
	if st.LastExit == nil || st.LastExit.Code != 0 || st.LastExit.Signal != "" {
		t.Fatalf("graceful stop should exit 0 without a signal, got %+v", st.LastExit)
	}
	if !rec.has(name, StateStopping, "stop requested") {
		t.Fatal("no stopping event")
	}
}

func TestGracefulStopByAdminMethod(t *testing.T) {
	// The stop signal is SIGKILL: the admin stop method must be what ends the
	// service cleanly, otherwise the exit would show the signal.
	spec := fakeSpec("svc", shortName(), "serve", nil)
	spec.StopMethod = fakeStop
	spec.StopSignal = "SIGKILL"
	sv, _ := newTestSupervisor(t, Config{Services: []ServiceSpec{spec}})
	ctx := ctxT(t, 20*time.Second)
	if err := sv.Start(ctx, spec.Name); err != nil {
		t.Fatal(err)
	}
	waitState(t, sv, spec.Name, StateReady)
	start := time.Now()
	if err := sv.Stop(ctx, spec.Name); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	st, _ := sv.Status(spec.Name)
	if st.LastExit == nil || st.LastExit.Code != 0 || st.LastExit.Signal != "" {
		t.Fatalf("admin stop should end the service cleanly, got %+v", st.LastExit)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("admin stop took %s", d)
	}
}

func TestStopKillsAfterStopTimeout(t *testing.T) {
	skipWithoutSignals(t)
	spec := fakeSpec("svc", shortName(), "ignore-term", nil)
	spec.StopTimeout = Duration(200 * time.Millisecond)
	sv, rec := newTestSupervisor(t, Config{Services: []ServiceSpec{spec}})
	ctx := ctxT(t, 20*time.Second)
	if err := sv.Start(ctx, spec.Name); err != nil {
		t.Fatal(err)
	}
	waitState(t, sv, spec.Name, StateReady)
	start := time.Now()
	if err := sv.Stop(ctx, spec.Name); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if d := time.Since(start); d < 150*time.Millisecond || d > 5*time.Second {
		t.Fatalf("Stop returned after %s, want about the stop timeout", d)
	}
	st, _ := sv.Status(spec.Name)
	if st.LastExit == nil || st.LastExit.Signal != "SIGKILL" {
		t.Fatalf("want the process killed with SIGKILL, got %+v", st.LastExit)
	}
	if !rec.has(spec.Name, StateStopping, "killing") {
		t.Fatal("no kill event")
	}
}

func TestCrashRestartWithBackoff(t *testing.T) {
	spec := fakeSpec("svc", shortName(), "crash", map[string]string{
		envAfter:   "50ms",
		envCounter: filepath.Join(tempDir(t), "count"),
		envCrashes: "2",
	})
	sv, rec := newTestSupervisor(t, Config{Services: []ServiceSpec{spec}})
	ctx := ctxT(t, 20*time.Second)
	if err := sv.Start(ctx, spec.Name); err != nil {
		t.Fatal(err)
	}
	st := waitFor2(t, sv, spec.Name, func(s Status) bool { return s.State == StateReady && s.Restarts == 2 })
	if st.LastExit == nil || st.LastExit.Code != 3 {
		t.Fatalf("last exit = %+v, want code 3", st.LastExit)
	}
	if n := rec.count(spec.Name, StateBackoff); n != 2 {
		t.Fatalf("backoff events = %d, want 2", n)
	}
}

func TestRestartLimitMovesToFailed(t *testing.T) {
	spec := fakeSpec("svc", shortName(), "crash", map[string]string{
		envAfter:   "10ms",
		envCounter: filepath.Join(tempDir(t), "count"),
		envCrashes: "1000",
	})
	spec.Restart.MaxRestarts = 2
	spec.Restart.Window = Duration(time.Minute)
	sv, _ := newTestSupervisor(t, Config{Services: []ServiceSpec{spec}})
	ctx := ctxT(t, 20*time.Second)
	if err := sv.Start(ctx, spec.Name); err != nil {
		t.Fatal(err)
	}
	st := waitState(t, sv, spec.Name, StateFailed)
	if st.Restarts != 2 {
		t.Fatalf("Restarts = %d, want 2", st.Restarts)
	}
	if st.LastError == "" || !strings.Contains(st.LastError, "restart limit reached") {
		t.Fatalf("LastError = %q", st.LastError)
	}
	time.Sleep(100 * time.Millisecond)
	if st, _ := sv.Status(spec.Name); st.State != StateFailed || st.Restarts != 2 {
		t.Fatalf("failed service restarted: state %s restarts %d", st.State, st.Restarts)
	}
}

func TestPolicyNeverFailsOnCrash(t *testing.T) {
	spec := fakeSpec("svc", "", "crash", map[string]string{
		envAfter:   "10ms",
		envCounter: filepath.Join(tempDir(t), "count"),
		envCrashes: "1000",
	})
	spec.Listener = ""
	spec.Health = HealthSpec{}
	spec.Restart.Policy = "never"
	sv, _ := newTestSupervisor(t, Config{Services: []ServiceSpec{spec}})
	ctx := ctxT(t, 20*time.Second)
	if err := sv.Start(ctx, spec.Name); err != nil {
		t.Fatal(err)
	}
	st := waitState(t, sv, spec.Name, StateFailed)
	if st.Restarts != 0 || st.LastExit == nil || st.LastExit.Code != 3 {
		t.Fatalf("never policy: restarts %d, exit %+v", st.Restarts, st.LastExit)
	}
}

func TestHealthFailureRestartsProcess(t *testing.T) {
	spec := fakeSpec("svc", shortName(), "close", map[string]string{
		envAfter:   "300ms",
		envCounter: filepath.Join(tempDir(t), "count"),
	})
	spec.Health.Interval = Duration(40 * time.Millisecond)
	spec.Health.Timeout = Duration(150 * time.Millisecond)
	spec.Health.Failures = 2
	sv, rec := newTestSupervisor(t, Config{Services: []ServiceSpec{spec}})
	ctx := ctxT(t, 20*time.Second)
	if err := sv.Start(ctx, spec.Name); err != nil {
		t.Fatal(err)
	}
	first := waitState(t, sv, spec.Name, StateReady)
	st := waitFor2(t, sv, spec.Name, func(s Status) bool { return s.Restarts >= 1 && s.State == StateReady && s.PID != first.PID })
	if st.LastExit == nil {
		t.Fatal("health restart left no exit record")
	}
	if !rec.has(spec.Name, StateStopping, "health restart") {
		t.Fatal("no health restart event")
	}
}

func TestStartGraceExpiryWithPolicyNeverIsUnhealthy(t *testing.T) {
	spec := fakeSpec("svc", shortName(), "silent", nil)
	spec.Restart.Policy = "never"
	spec.Health.StartGrace = Duration(300 * time.Millisecond)
	spec.Health.Timeout = Duration(100 * time.Millisecond)
	sv, _ := newTestSupervisor(t, Config{Services: []ServiceSpec{spec}})
	ctx := ctxT(t, 20*time.Second)
	if err := sv.Start(ctx, spec.Name); err != nil {
		t.Fatal(err)
	}
	st := waitState(t, sv, spec.Name, StateUnhealthy)
	if st.Health == nil || st.Health.OK {
		t.Fatalf("unhealthy without a failing health result: %+v", st.Health)
	}
	if st.Restarts != 0 || st.PID == 0 {
		t.Fatalf("policy never must not restart: restarts %d pid %d", st.Restarts, st.PID)
	}
	if err := sv.Stop(ctx, spec.Name); err != nil {
		t.Fatal(err)
	}
}

func TestExclusiveLockContention(t *testing.T) {
	lockPath := filepath.Join(tempDir(t), "svc.lock")
	spec := fakeSpec("svc", "", "silent", nil)
	spec.Listener = ""
	spec.Health = HealthSpec{}
	spec.ExclusiveLock = lockPath
	sv, _ := newTestSupervisor(t, Config{Services: []ServiceSpec{spec}})
	ctx := ctxT(t, 20*time.Second)

	// Another holder (here: a second open file description) owns the lock.
	holder, err := acquireLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	err = sv.Start(ctx, spec.Name)
	if !errors.Is(err, ErrLockHeld) {
		t.Fatalf("Start under a held lock: %v, want ErrLockHeld", err)
	}
	if st, _ := sv.Status(spec.Name); st.State != StateFailed || st.PID != 0 {
		t.Fatalf("contended start: state %s pid %d", st.State, st.PID)
	}
	holder.release()

	if err := sv.Start(ctx, spec.Name); err != nil {
		t.Fatalf("Start after release: %v", err)
	}
	waitState(t, sv, spec.Name, StateReady)
	// While hqqd runs the service, the lock is ours.
	if l, err := acquireLock(lockPath); !errors.Is(err, ErrLockHeld) {
		if err == nil {
			l.release()
		}
		t.Fatalf("lock not held while running: %v", err)
	}
	if err := sv.Stop(ctx, spec.Name); err != nil {
		t.Fatal(err)
	}
	l, err := acquireLock(lockPath)
	if err != nil {
		t.Fatalf("lock still held after Stop: %v", err)
	}
	l.release()
}

func TestReloadAddsChangesAndRemoves(t *testing.T) {
	mk := func(name string, env map[string]string) ServiceSpec {
		return fakeSpec(name, shortName(), "serve", env)
	}
	a := mk("a", nil)
	c := mk("c", nil)
	d := mk("d", nil)
	sv, _ := newTestSupervisor(t, Config{Services: []ServiceSpec{a, c, d}})
	ctx := ctxT(t, 30*time.Second)
	for _, n := range []string{"a", "c", "d"} {
		if err := sv.Start(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range []string{"a", "c", "d"} {
		waitState(t, sv, n, StateReady)
	}
	dPID, _ := sv.Status("d")
	aPID, _ := sv.Status("a")

	// a changes its environment (restarted), c is removed, d is unchanged, b is new.
	a2 := mk("a", map[string]string{envTag: "2"})
	b := mk("b", nil)
	if err := sv.Reload(ctx, Config{Services: []ServiceSpec{a2, d, b}}); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if _, err := sv.Status("c"); !errors.Is(err, ErrUnknownService) {
		t.Fatalf("removed service still known: %v", err)
	}
	waitState(t, sv, "b", StateReady)
	st := waitFor2(t, sv, "a", func(s Status) bool { return s.State == StateReady && s.PID != aPID.PID })
	if st.PID == aPID.PID {
		t.Fatal("changed service was not restarted")
	}
	if dNow, _ := sv.Status("d"); dNow.PID != dPID.PID {
		t.Fatalf("unchanged service restarted: pid %d -> %d", dPID.PID, dNow.PID)
	}
	names := []string{}
	for _, s := range sv.List() {
		names = append(names, s.Name)
	}
	if len(names) != 3 || names[0] != "a" || names[1] != "b" || names[2] != "d" {
		t.Fatalf("List = %v", names)
	}
	if err := sv.Reload(ctx, Config{Services: []ServiceSpec{{Name: "x"}}}); err == nil {
		t.Fatal("Reload accepted an invalid config")
	}
}

func TestRunStartsAutostartAndShutsDown(t *testing.T) {
	auto := fakeSpec("auto", shortName(), "serve", nil)
	manual := fakeSpec("manual", shortName(), "serve", nil)
	off := false
	manual.Autostart = &off
	sv, rec := newTestSupervisor(t, Config{Services: []ServiceSpec{auto, manual}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sv.Run(ctx) }()

	waitState(t, sv, "auto", StateReady)
	if st, _ := sv.Status("manual"); st.State != StateStopped {
		t.Fatalf("manual service started by Run: %s", st.State)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not return")
	}
	if !rec.has("auto", StateStopped, "") {
		t.Fatal("auto service was not stopped on shutdown")
	}
	if _, err := sv.Status("auto"); !errors.Is(err, ErrShutdown) {
		t.Fatalf("Status after shutdown: %v", err)
	}
}

func TestStartFailureIsReported(t *testing.T) {
	spec := ServiceSpec{Name: "missing", Command: filepath.Join(tempDir(t), "no-such-binary")}
	sv, _ := newTestSupervisor(t, Config{Services: []ServiceSpec{spec}})
	ctx := ctxT(t, 10*time.Second)
	if err := sv.Start(ctx, "missing"); err == nil {
		t.Fatal("Start of a missing binary succeeded")
	}
	st, _ := sv.Status("missing")
	if st.State != StateFailed || st.LastError == "" {
		t.Fatalf("status = %+v", st)
	}
	if _, err := sv.Status("nope"); !errors.Is(err, ErrUnknownService) {
		t.Fatalf("unknown service: %v", err)
	}
}

func TestOutputFileAndPrefixModes(t *testing.T) {
	dir := tempDir(t)
	file := filepath.Join(dir, "svc.log")
	spec := fakeSpec("svc", "", "silent", nil)
	spec.Listener = ""
	spec.Health = HealthSpec{}
	spec.Output = OutputSpec{Mode: "file", Path: file}
	sv, _ := newTestSupervisor(t, Config{Services: []ServiceSpec{spec}})
	ctx := ctxT(t, 20*time.Second)
	if err := sv.Start(ctx, spec.Name); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "file output", func() bool {
		b, _ := os.ReadFile(file)
		return strings.Contains(string(b), "fake silent started") && strings.Contains(string(b), "fake stderr")
	})
	if err := sv.Stop(ctx, spec.Name); err != nil {
		t.Fatal(err)
	}
}

func TestEnvironmentAndDirAreApplied(t *testing.T) {
	dir := tempDir(t)
	out := filepath.Join(dir, "env.txt")
	spec := ServiceSpec{
		Name:    "env",
		Command: "/bin/sh",
		Args:    []string{"-c", "pwd > " + out + "; echo \"$HQQD_TEST_VAR\" >> " + out},
		Dir:     dir,
		Env:     map[string]string{"HQQD_TEST_VAR": "from-config"},
		Restart: RestartSpec{Policy: "never"},
	}
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	sv, _ := newTestSupervisor(t, Config{Services: []ServiceSpec{spec}})
	ctx := ctxT(t, 20*time.Second)
	if err := sv.Start(ctx, "env"); err != nil {
		t.Fatal(err)
	}
	var b []byte
	waitFor(t, "shell output", func() bool {
		b, _ = os.ReadFile(out)
		return strings.Contains(string(b), "from-config")
	})
	waitFor(t, "stop after exit", func() bool {
		st, _ := sv.Status("env")
		return st.State == StateStopped
	})
	if !strings.Contains(string(b), dir) {
		t.Fatalf("env/dir not applied: %q", b)
	}
}

// waitFor2 waits until pred holds for the status of name and returns it.
func waitFor2(t *testing.T, sv *Supervisor, name string, pred func(Status) bool) Status {
	t.Helper()
	var out Status
	waitFor(t, "predicate on "+name, func() bool {
		s, err := sv.Status(name)
		out = s
		return err == nil && pred(s)
	})
	return out
}

func TestStopDuringBackoffPreventsRestart(t *testing.T) {
	spec := fakeSpec("svc", "", "crash", map[string]string{
		envAfter:   "10ms",
		envCounter: filepath.Join(tempDir(t), "count"),
		envCrashes: "1000",
	})
	spec.Listener = ""
	spec.Health = HealthSpec{}
	spec.Restart.Backoff = BackoffSpec{Initial: Duration(time.Hour), Max: Duration(time.Hour)}
	sv, _ := newTestSupervisor(t, Config{Services: []ServiceSpec{spec}})
	ctx := ctxT(t, 20*time.Second)
	if err := sv.Start(ctx, spec.Name); err != nil {
		t.Fatal(err)
	}
	st := waitState(t, sv, spec.Name, StateBackoff)
	if st.NextRestart == nil || st.LastExit == nil || st.LastExit.Code != 3 {
		t.Fatalf("backoff status = %+v", st)
	}
	if err := sv.Stop(ctx, spec.Name); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if st, _ := sv.Status(spec.Name); st.State != StateStopped || st.Restarts != 0 || st.NextRestart != nil {
		t.Fatalf("after Stop in backoff: %+v", st)
	}
}

// fileExists reports whether path exists.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// selfDigest returns the path, SHA-256 digest and size of the running test
// binary: the executable the fake services run.
func selfDigest(t *testing.T) (string, secureexec.Hash, int64) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	sum, size, err := secureexec.HashFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	return exe, sum, size
}

func TestSHA256StartsVerifiedProcess(t *testing.T) {
	_, sum, _ := selfDigest(t)
	spec := bareSpec("verified", "silent")
	spec.SHA256 = sum.String()
	sv, _ := newTestSupervisor(t, Config{Services: []ServiceSpec{spec}})
	ctx := ctxT(t, 20*time.Second)
	if err := sv.Start(ctx, spec.Name); err != nil {
		t.Fatal(err)
	}
	st := waitState(t, sv, spec.Name, StateReady)
	if !st.Verified || st.Mechanism == "" {
		t.Fatalf("status = %+v", st)
	}
	if err := sv.Stop(ctx, spec.Name); err != nil {
		t.Fatal(err)
	}
	if st, _ := sv.Status(spec.Name); st.Verified || st.Mechanism != "" {
		t.Fatalf("stopped service still reports verification: %+v", st)
	}
}

func TestSHA256MismatchNeverStartsProcess(t *testing.T) {
	spec := bareSpec("bad", "silent")
	spec.SHA256 = strings.Repeat("0", 64)
	counter := filepath.Join(tempDir(t), "count")
	spec.Env[envCounter] = counter
	sv, rec := newTestSupervisor(t, Config{Services: []ServiceSpec{spec}})
	ctx := ctxT(t, 20*time.Second)
	if err := sv.Start(ctx, spec.Name); !errors.Is(err, secureexec.ErrHashMismatch) {
		t.Fatalf("Start: %v, want a hash mismatch", err)
	}
	st, _ := sv.Status(spec.Name)
	if st.State != StateFailed || !strings.Contains(st.LastError, "sha256 mismatch") {
		t.Fatalf("status = %+v", st)
	}
	if !rec.has(spec.Name, StateFailed, "sha256 mismatch") {
		t.Fatal("no failed event names the mismatch")
	}
	if _, err := os.Stat(counter); err == nil {
		t.Fatal("the service process started")
	}
}

// writeManifest writes into dir a manifest.json holding entry, and its
// signature made with signer. It returns the manifest block of a service that
// reads them.
func writeManifest(t *testing.T, dir string, signer ed25519.PrivateKey, pub ed25519.PublicKey, entry manifest.File) *ManifestSpec {
	t.Helper()
	data, err := manifest.Marshal(&manifest.Manifest{
		Version:  manifest.Version,
		Name:     "svc",
		Release:  "v1",
		Files:    []manifest.File{entry},
		Licenses: []string{"MIT"},
	})
	if err != nil {
		t.Fatal(err)
	}
	sig, err := manifest.Sign(data, signer)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file+".sig", sig, 0o600); err != nil {
		t.Fatal(err)
	}
	return &ManifestSpec{File: file, Signature: file + ".sig", PublicKey: string(manifest.FormatPublicKey(pub))}
}

func TestManifestSelectsVerifiedArtifact(t *testing.T) {
	exe, sum, size := selfDigest(t)
	pub, priv, err := manifest.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	_, other, err := manifest.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	good := manifest.File{OS: runtime.GOOS, Arch: runtime.GOARCH, Name: filepath.Base(exe), SHA256: sum, Size: size}
	otherOS := good
	otherOS.OS = "nonesuch"
	renamed := good
	renamed.Name = "renamed-" + good.Name
	cases := []struct {
		name   string
		signer ed25519.PrivateKey
		entry  manifest.File
		ok     bool
		err    error  // expected errors.Is target, if any
		msg    string // expected substring of the Start error, if any
	}{
		{name: "signed artifact", signer: priv, entry: good, ok: true},
		{name: "bad signature", signer: other, entry: good, err: manifest.ErrBadSignature},
		{name: "no entry for platform", signer: priv, entry: otherOS, msg: "no artifact for " + runtime.GOOS + "/" + runtime.GOARCH},
		{name: "name mismatch", signer: priv, entry: renamed, msg: "is not the manifest artifact"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := tempDir(t)
			spec := bareSpec("signed", "silent")
			spec.Manifest = writeManifest(t, dir, tc.signer, pub, tc.entry)
			counter := filepath.Join(dir, "count")
			spec.Env[envCounter] = counter
			sv, _ := newTestSupervisor(t, Config{Services: []ServiceSpec{spec}})
			ctx := ctxT(t, 20*time.Second)
			err := sv.Start(ctx, spec.Name)
			if tc.ok {
				if err != nil {
					t.Fatal(err)
				}
				st := waitState(t, sv, spec.Name, StateReady)
				if !st.Verified || st.Mechanism == "" {
					t.Fatalf("status = %+v", st)
				}
				return
			}
			if err == nil {
				t.Fatal("Start succeeded")
			}
			if tc.err != nil && !errors.Is(err, tc.err) {
				t.Fatalf("Start: %v, want %v", err, tc.err)
			}
			if tc.msg != "" && !strings.Contains(err.Error(), tc.msg) {
				t.Fatalf("Start: %v, want %q", err, tc.msg)
			}
			st, _ := sv.Status(spec.Name)
			if st.State != StateFailed || st.LastError == "" {
				t.Fatalf("status = %+v", st)
			}
			if _, err := os.Stat(counter); err == nil {
				t.Fatal("the service process started")
			}
		})
	}
}

// TestParentPipeEndsChildWhenSupervisorDies runs a supervisor in a helper
// process. The child watches the pipe on its stdin: it must not end while the
// supervisor lives, and must end soon after the supervisor is killed.
func TestParentPipeEndsChildWhenSupervisorDies(t *testing.T) {
	dir := tempDir(t)
	ready := filepath.Join(dir, "ready")
	mark := filepath.Join(dir, "exited")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	helper := exec.Command(exe)
	helper.Env = append(os.Environ(), envSupervisor+"=1", envReady+"="+ready, envMark+"="+mark)
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = helper.Process.Kill()
		_ = helper.Wait()
	})
	waitFor(t, "the child to watch its lifeline", func() bool { return fileExists(ready) })
	time.Sleep(300 * time.Millisecond)
	if fileExists(mark) {
		t.Fatal("the child ended while its supervisor is alive")
	}
	if err := helper.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = helper.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for !fileExists(mark) {
		if time.Now().After(deadline) {
			t.Fatal("the child did not end after its supervisor was killed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
