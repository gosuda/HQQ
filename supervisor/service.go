package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"gosuda.org/hqq"
	"gosuda.org/hqq/lifeline"
	"gosuda.org/hqq/secureexec"
	"gosuda.org/hqq/secureexec/manifest"
)

// cmdKind is a request the Supervisor sends to a service loop.
type cmdKind int

const (
	cmdStart cmdKind = iota
	cmdStop
	cmdRestart
	cmdReload
	cmdShutdown
)

type command struct {
	kind  cmdKind
	spec  ServiceSpec
	reply chan error // buffered(1), may be nil
}

var (
	errStoppedBeforeStart = errors.New("supervisor: stopped before it was started")
)

// service owns one supervised process. Its loop goroutine is the only writer
// of the lifecycle fields below; snapshots for Status are published under mu.
type service struct {
	sv   *Supervisor
	name string
	cmds chan command
	done chan struct{} // closed when the loop ended

	spec         ServiceSpec // loop-owned definition, replaced by Reload
	desired      bool        // the service should run (until stopped or failed)
	explicit     bool        // a Start or Restart waits for the current process to exit
	quit         bool        // Shutdown: the loop ends once no process is left
	cur          *proc
	state        State
	lock         *fileLock
	out          *rotWriter // file output, reused across restarts
	spawned      bool       // a process ran before: restarts are counted, stale names cleaned
	restarts     int
	attempt      int         // consecutive backoff attempts
	windowStarts []time.Time // automatic restarts in the sliding window
	backoff      *time.Timer
	backoffC     <-chan time.Time
	nextRestart  time.Time
	lastExit     *Exit
	lastHealth   *HealthResult
	lastErr      string
	startWaiters []chan error
	stopWaiters  []chan error

	mu   sync.RWMutex
	snap Status
}

// proc is one running incarnation of a service.
type proc struct {
	cmd       *exec.Cmd
	pid       int
	startedAt time.Time
	ctx       context.Context // canceled when the process exited
	cancel    context.CancelFunc
	exited    chan struct{} // closed after Wait; waitErr and pstate are set before
	waitErr   error
	pstate    *os.ProcessState
	outs      procOutputs
	lifeline  *lifeline.Lifeline   // parent end of the stdin pipe (ParentPipe)
	verified  *secureexec.Verified // verified executable; nil when not verified
	mechanism string               // secureexec mechanism of verified

	stopping      bool
	healthRestart bool
	killTimer     *time.Timer
	killC         <-chan time.Time
	tick          *time.Ticker
	probeCh       chan probeResult
	probing       bool
	failures      int // consecutive failed health checks
}

type procOutputs struct {
	stdout, stderr io.Writer
	flush          func()
}

type probeResult struct {
	at      time.Time
	latency time.Duration
	err     error
}

func (s *Supervisor) newService(spec ServiceSpec) *service {
	sv := &service{
		sv:    s,
		name:  spec.Name,
		cmds:  make(chan command),
		done:  make(chan struct{}),
		spec:  spec,
		state: StateStopped,
	}
	sv.publish()
	go sv.loop()
	return sv
}

// send hands c to the loop. It fails if the loop has ended or ctx ends first.
func (sv *service) send(ctx context.Context, c command) error {
	select {
	case sv.cmds <- c:
		return nil
	case <-sv.done:
		return ErrShutdown
	case <-ctx.Done():
		return ctx.Err()
	}
}

// request sends c and waits for the loop to reply.
func (sv *service) request(ctx context.Context, c command) error {
	c.reply = make(chan error, 1)
	if err := sv.send(ctx, c); err != nil {
		return err
	}
	select {
	case err := <-c.reply:
		return err
	case <-sv.done:
		select {
		case err := <-c.reply:
			return err
		default:
			return ErrShutdown
		}
	case <-ctx.Done():
		return ctx.Err()
	}
}

// status returns the last published snapshot, with the uptime at now.
func (sv *service) status(now time.Time) Status {
	sv.mu.RLock()
	st := sv.snap
	sv.mu.RUnlock()
	if st.StartedAt != nil && st.PID != 0 {
		st.Uptime = Duration(now.Sub(*st.StartedAt))
	}
	return st
}

// publish copies the loop-owned state into the snapshot.
func (sv *service) publish() {
	st := Status{
		Name:      sv.name,
		State:     sv.state,
		Restarts:  sv.restarts,
		LastExit:  sv.lastExit,
		Health:    sv.lastHealth,
		LastError: sv.lastErr,
	}
	if sv.cur != nil {
		st.PID = sv.cur.pid
		t := sv.cur.startedAt
		st.StartedAt = &t
		st.Mechanism = sv.cur.mechanism
		st.Verified = st.Mechanism != ""
	}
	if sv.state == StateBackoff && !sv.nextRestart.IsZero() {
		t := sv.nextRestart
		st.NextRestart = &t
	}
	sv.mu.Lock()
	sv.snap = st
	sv.mu.Unlock()
}

// setState records a state change, publishes it and emits an event when the
// state changed or a message is given.
func (sv *service) setState(st State, msg string) {
	changed := st != sv.state
	sv.state = st
	sv.publish()
	if changed || msg != "" {
		ev := Event{Time: time.Now(), Service: sv.name, State: st, Message: msg}
		if sv.cur != nil {
			ev.PID = sv.cur.pid
		}
		sv.sv.emit(ev)
	}
}

// loop is the service goroutine: it serializes commands, process exits,
// health results, kill deadlines and backoff timers.
func (sv *service) loop() {
	defer close(sv.done)
	for {
		exitC, tickC, probeC, killC := sv.waits()
		select {
		case c := <-sv.cmds:
			sv.command(c)
		case <-exitC:
			sv.onExit()
		case <-tickC:
			sv.probe()
		case r := <-probeC:
			sv.onProbe(r)
		case <-killC:
			sv.onKillTimeout()
		case <-sv.backoffC:
			sv.backoffC = nil
			sv.backoff = nil
			sv.autoRestart()
		}
		if sv.quit && sv.cur == nil {
			sv.teardown()
			return
		}
	}
}

// waits returns the channels of the current process, nil when there is none.
func (sv *service) waits() (<-chan struct{}, <-chan time.Time, <-chan probeResult, <-chan time.Time) {
	p := sv.cur
	if p == nil {
		return nil, nil, nil, nil
	}
	var tick <-chan time.Time
	if p.tick != nil {
		tick = p.tick.C
	}
	return p.exited, tick, p.probeCh, p.killC
}

func respond(ch chan error, err error) {
	if ch != nil {
		ch <- err
	}
}

func (sv *service) command(c command) {
	switch c.kind {
	case cmdStart:
		sv.start(c.reply)
	case cmdRestart:
		sv.restart(c.reply)
	case cmdStop:
		sv.stop(c.reply)
	case cmdReload:
		sv.reload(c.spec, c.reply)
	case cmdShutdown:
		sv.quit = true
		sv.stop(c.reply)
	}
}

func (sv *service) start(reply chan error) {
	if sv.quit {
		respond(reply, ErrShutdown)
		return
	}
	sv.desired = true
	if sv.cur != nil {
		if sv.cur.stopping {
			// The process is on its way out: start the next one after it exits.
			sv.explicit = true
			sv.startWaiters = append(sv.startWaiters, reply)
			return
		}
		respond(reply, nil)
		return
	}
	sv.cancelBackoff()
	sv.windowStarts = nil
	sv.attempt = 0
	sv.spawnNow(reply, false)
}

func (sv *service) restart(reply chan error) {
	if sv.quit {
		respond(reply, ErrShutdown)
		return
	}
	sv.desired = true
	if sv.cur == nil {
		sv.cancelBackoff()
		sv.spawnNow(reply, false)
		return
	}
	sv.explicit = true
	sv.startWaiters = append(sv.startWaiters, reply)
	sv.stopProc(sv.cur, "restart requested")
}

func (sv *service) stop(reply chan error) {
	sv.desired = false
	sv.explicit = false
	sv.cancelBackoff()
	if reply != nil {
		sv.stopWaiters = append(sv.stopWaiters, reply)
	}
	if sv.cur == nil {
		sv.finish(StateStopped, "")
		return
	}
	sv.stopProc(sv.cur, "stop requested")
}

func (sv *service) reload(spec ServiceSpec, reply chan error) {
	if sv.quit {
		respond(reply, ErrShutdown)
		return
	}
	sv.spec = spec
	respond(reply, nil)
	if sv.desired && sv.cur != nil {
		sv.restart(nil)
	}
}

// finish ends the desired state: the service is not wanted to run any more.
// It releases the exclusive lock and answers the waiters.
func (sv *service) finish(st State, reason string) {
	sv.desired = false
	sv.explicit = false
	sv.nextRestart = time.Time{}
	sv.cancelBackoff()
	sv.releaseLock()
	sv.lastErr = reason
	sv.setState(st, reason)
	for _, ch := range sv.stopWaiters {
		respond(ch, nil)
	}
	sv.stopWaiters = nil
	for _, ch := range sv.startWaiters {
		respond(ch, errStoppedBeforeStart)
	}
	sv.startWaiters = nil
}

// teardown runs when the loop ends: everything is released.
func (sv *service) teardown() {
	sv.cancelBackoff()
	sv.releaseLock()
	if sv.out != nil {
		_ = sv.out.Close()
		sv.out = nil
	}
	for _, ch := range sv.startWaiters {
		respond(ch, ErrShutdown)
	}
	sv.startWaiters = nil
}

func (sv *service) cancelBackoff() {
	if sv.backoff != nil {
		sv.backoff.Stop()
		sv.backoff = nil
		sv.backoffC = nil
	}
}

func (sv *service) releaseLock() {
	if sv.lock != nil {
		sv.lock.release()
		sv.lock = nil
	}
}

// stopProc begins the graceful stop of p: the admin stop method when set,
// else the stop signal; the kill follows after stop_timeout.
func (sv *service) stopProc(p *proc, why string) {
	if p.stopping {
		return
	}
	p.stopping = true
	timeout := sv.spec.stopTimeout()
	p.killTimer = time.NewTimer(timeout)
	p.killC = p.killTimer.C
	sv.setState(StateStopping, why)
	spec := sv.spec
	if spec.StopMethod != 0 && spec.Listener != "" {
		sig, _ := spec.stopSignal()
		proc := p.cmd.Process
		go func() {
			if err := adminStop(p.ctx, spec.Listener, spec.StopMethod, timeout); err != nil {
				if err := signalProcess(proc, sig); err != nil {
					killProcess(proc)
				}
			}
		}()
		return
	}
	sig, _ := spec.stopSignal()
	if err := signalProcess(p.cmd.Process, sig); err != nil {
		killProcess(p.cmd.Process)
	}
}

// adminStop asks the service to shut down through its stop method.
func adminStop(ctx context.Context, listener string, method uint64, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	link, err := hqq.Connect(ctx, listener, hqq.Config{})
	if err != nil {
		return err
	}
	defer link.Close()
	msg, err := link.Call(ctx, method, nil)
	if err != nil {
		return err
	}
	msg.Release()
	return nil
}

func (sv *service) onKillTimeout() {
	p := sv.cur
	if p == nil {
		return
	}
	p.killC = nil
	sv.sv.emit(Event{Time: time.Now(), Service: sv.name, State: sv.state, PID: p.pid, Message: "stop timeout elapsed: killing"})
	killProcess(p.cmd.Process)
}

// spawnNow starts the process and answers reply and the start waiters. The
// state is published before the answers, so a caller that sees its Start
// return also sees the resulting state. auto marks a restart the backoff
// scheduled: its failure counts against the restart window.
func (sv *service) spawnNow(reply chan error, auto bool) {
	waiters := append([]chan error{reply}, sv.startWaiters...)
	sv.startWaiters = nil
	if err := sv.spawn(); err != nil {
		if auto {
			sv.scheduleRestart(0, "start failed: "+err.Error())
		} else {
			sv.finish(StateFailed, "start failed: "+err.Error())
		}
		for _, ch := range waiters {
			respond(ch, err)
		}
		return
	}
	if sv.spawned {
		sv.restarts++
	}
	sv.spawned = true
	sv.lastErr = ""
	sv.lastHealth = nil
	if sv.spec.Listener == "" {
		sv.setState(StateReady, "")
	} else {
		sv.setState(StateStarting, "")
		sv.probe()
	}
	for _, ch := range waiters {
		respond(ch, nil)
	}
}

// spawn prepares the lock, the output and the command, and starts the process.
func (sv *service) spawn() error {
	spec := sv.spec
	if err := sv.lockFor(spec); err != nil {
		return err
	}
	if spec.Listener != "" && sv.spawned {
		cleanStaleListener(spec)
	}
	outs, err := sv.outputs(spec)
	if err != nil {
		return err
	}
	cmd, v, err := buildCmd(spec)
	if err != nil {
		outs.flush()
		return err
	}
	cmd.Dir = spec.Dir
	cmd.Env = environ(spec.Env)
	cmd.Stdout = outs.stdout
	cmd.Stderr = outs.stderr
	cmd.WaitDelay = stopPipeWaitDelay
	setProcAttr(cmd)
	var ll *lifeline.Lifeline
	if spec.ParentPipe {
		ll, err = lifeline.Attach(cmd)
	}
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		outs.flush()
		if ll != nil {
			_ = ll.Close()
		}
		if v != nil {
			_ = v.Close()
		}
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &proc{
		cmd:       cmd,
		pid:       cmd.Process.Pid,
		startedAt: time.Now(),
		ctx:       ctx,
		cancel:    cancel,
		exited:    make(chan struct{}),
		outs:      outs,
		lifeline:  ll,
		verified:  v,
	}
	if v != nil {
		p.mechanism = v.Mechanism()
	}
	go func() {
		p.waitErr = cmd.Wait()
		p.pstate = cmd.ProcessState
		if p.lifeline != nil {
			_ = p.lifeline.Close()
		}
		if p.verified != nil {
			_ = p.verified.Close()
		}
		close(p.exited)
	}()
	if spec.Listener != "" {
		p.probeCh = make(chan probeResult, 1)
		p.tick = time.NewTicker(min(spec.Health.interval(), pollWhileStarting))
	}
	sv.cur = p
	return nil
}

// lockFor makes sure the exclusive lock of spec is held, when it has one.
func (sv *service) lockFor(spec ServiceSpec) error {
	if sv.lock != nil && sv.lock.path == spec.ExclusiveLock {
		return nil
	}
	sv.releaseLock()
	if spec.ExclusiveLock == "" {
		return nil
	}
	l, err := acquireLock(spec.ExclusiveLock)
	if err != nil {
		return fmt.Errorf("exclusive_lock %s: %w", spec.ExclusiveLock, err)
	}
	sv.lock = l
	return nil
}

// outputs returns the writers of the next process, reusing the file output
// when its settings did not change.
func (sv *service) outputs(spec ServiceSpec) (procOutputs, error) {
	o := spec.Output
	if o.mode() != modeFile && sv.out != nil {
		_ = sv.out.Close()
		sv.out = nil
	}
	switch o.mode() {
	case modeFile:
		if sv.out != nil && (sv.out.path != o.Path || sv.out.max != o.maxBytes() || sv.out.keep != o.maxFiles()) {
			_ = sv.out.Close()
			sv.out = nil
		}
		if sv.out == nil {
			w, err := openRotWriter(o.Path, o.maxBytes(), o.maxFiles())
			if err != nil {
				return procOutputs{}, fmt.Errorf("output: %w", err)
			}
			sv.out = w
		}
		return procOutputs{stdout: sv.out, stderr: sv.out, flush: func() {}}, nil
	case modePrefix:
		so := newPrefixWriter(prefixFor(spec.Name), os.Stdout)
		se := newPrefixWriter(prefixFor(spec.Name), os.Stderr)
		return procOutputs{stdout: so, stderr: se, flush: func() { so.Flush(); se.Flush() }}, nil
	default:
		return procOutputs{stdout: os.Stdout, stderr: os.Stderr, flush: func() {}}, nil
	}
}

// environ is hqqd's environment with the extra variables appended; exec keeps
// the last value of a repeated key.
func environ(extra map[string]string) []string {
	env := os.Environ()
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}

// buildCmd returns the command of spec. With sha256 or manifest, the command
// runs from a snapshot verified against the expected digest; the returned
// Verified must be closed once the child has exited. Without them the command
// is a plain exec.Cmd and the Verified is nil.
func buildCmd(spec ServiceSpec) (*exec.Cmd, *secureexec.Verified, error) {
	if spec.SHA256 == "" && spec.Manifest == nil {
		return exec.Command(spec.Command, spec.Args...), nil, nil
	}
	path, err := verifiedPath(spec)
	if err != nil {
		return nil, nil, err
	}
	want, err := expectedDigest(spec, path)
	if err != nil {
		return nil, nil, err
	}
	v, err := secureexec.Verify(path, want)
	if err != nil {
		return nil, nil, fmt.Errorf("verify: %w", err)
	}
	return v.Command(context.Background(), spec.Args...), v, nil
}

// verifiedPath returns the file that spec.Command runs, as exec finds it: a
// bare name is looked up in PATH, and a relative path is relative to Dir.
func verifiedPath(spec ServiceSpec) (string, error) {
	c := spec.Command
	if filepath.Base(c) == c && !strings.ContainsAny(c, `/\`) {
		return exec.LookPath(c)
	}
	if spec.Dir != "" && !filepath.IsAbs(c) {
		return filepath.Join(spec.Dir, c), nil
	}
	return c, nil
}

// expectedDigest returns the digest that the executable at path must have:
// spec.SHA256, or the digest of its artifact in the signed manifest for this
// platform. The manifest is read and verified on every call.
func expectedDigest(spec ServiceSpec, path string) (secureexec.Hash, error) {
	if spec.Manifest == nil {
		return secureexec.ParseHash(spec.SHA256)
	}
	m := spec.Manifest
	data, err := os.ReadFile(m.File)
	if err != nil {
		return secureexec.Hash{}, fmt.Errorf("manifest: %w", err)
	}
	sig, err := os.ReadFile(m.Signature)
	if err != nil {
		return secureexec.Hash{}, fmt.Errorf("manifest: %w", err)
	}
	pub, err := manifest.ParsePublicKey([]byte(m.PublicKey))
	if err != nil {
		return secureexec.Hash{}, err
	}
	mf, err := manifest.Verify(data, sig, pub)
	if err != nil {
		return secureexec.Hash{}, fmt.Errorf("manifest %s: %w", m.File, err)
	}
	f, ok := mf.Find(runtime.GOOS, runtime.GOARCH)
	if !ok {
		return secureexec.Hash{}, fmt.Errorf("no artifact for %s/%s in manifest", runtime.GOOS, runtime.GOARCH)
	}
	if f.Name != filepath.Base(path) {
		return secureexec.Hash{}, fmt.Errorf("command %s is not the manifest artifact %s for %s/%s", path, f.Name, runtime.GOOS, runtime.GOARCH)
	}
	return f.SHA256, nil
}

// onExit handles the end of the current process: it decides between a
// restart, a failure and a plain stop.
func (sv *service) onExit() {
	p := sv.cur
	sv.cur = nil
	p.cancel()
	if p.killTimer != nil {
		p.killTimer.Stop()
	}
	if p.tick != nil {
		p.tick.Stop()
	}
	p.outs.flush()

	exit := Exit{Code: -1, At: time.Now()}
	if p.pstate != nil {
		exit.Code = p.pstate.ExitCode()
		if ws, ok := p.pstate.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			exit.Signal = signalName(ws.Signal())
		}
	}
	sv.lastExit = &exit
	sv.lastHealth = nil
	failed := exit.Code != 0 || exit.Signal != ""
	ranFor := time.Since(p.startedAt)
	why := describeExit(exit)

	switch {
	case sv.explicit && sv.desired:
		sv.explicit = false
		sv.notifyStopped()
		sv.spawnNow(nil, false)
	case !sv.desired:
		sv.finish(StateStopped, "")
	case p.healthRestart || sv.shouldRestart(failed):
		sv.scheduleRestart(ranFor, why)
	case failed:
		sv.finish(StateFailed, why)
	default:
		sv.finish(StateStopped, "")
	}
	if sv.cur == nil {
		sv.publish()
	}
}

// notifyStopped answers the stop waiters: the process they waited for is gone.
func (sv *service) notifyStopped() {
	for _, ch := range sv.stopWaiters {
		respond(ch, nil)
	}
	sv.stopWaiters = nil
}

// shouldRestart reports whether the policy restarts after an exit.
func (sv *service) shouldRestart(failed bool) bool {
	switch sv.spec.Restart.policy() {
	case policyAlways:
		return true
	case policyOnFailure:
		return failed
	}
	return false
}

// scheduleRestart books the next automatic restart after backoff, or fails
// the service when the restart limit within the window is reached.
func (sv *service) scheduleRestart(ranFor time.Duration, why string) {
	rs := sv.spec.Restart
	if ranFor >= rs.resetAfter() {
		sv.attempt = 0
	}
	now := time.Now()
	window := rs.window()
	kept := sv.windowStarts[:0]
	for _, t := range sv.windowStarts {
		if now.Sub(t) < window {
			kept = append(kept, t)
		}
	}
	sv.windowStarts = kept
	if len(kept) >= rs.maxRestarts() {
		sv.finish(StateFailed, fmt.Sprintf("restart limit reached: %d restarts within %s (last: %s)", len(kept), window, why))
		return
	}
	delay := rs.Backoff.delay(sv.attempt, rand.Float64())
	sv.attempt++
	sv.nextRestart = now.Add(delay)
	sv.backoff = time.NewTimer(delay)
	sv.backoffC = sv.backoff.C
	sv.setState(StateBackoff, fmt.Sprintf("%s; restarting in %s", why, delay.Round(time.Millisecond)))
}

// autoRestart starts the service when its backoff timer fired.
func (sv *service) autoRestart() {
	if !sv.desired || sv.quit {
		return
	}
	sv.nextRestart = time.Time{}
	sv.windowStarts = append(sv.windowStarts, time.Now())
	sv.spawnNow(nil, true)
}

// probe runs one health check of the current process in the background.
func (sv *service) probe() {
	p := sv.cur
	if p == nil || p.probing || p.probeCh == nil {
		return
	}
	p.probing = true
	spec := sv.spec
	go func() {
		start := time.Now()
		err := checkHealth(p.ctx, spec.Listener, spec.Health.timeout(), spec.Health.PingMethod)
		p.probeCh <- probeResult{at: start, latency: time.Since(start), err: err}
	}()
}

// onProbe applies the result of a health check.
func (sv *service) onProbe(r probeResult) {
	p := sv.cur
	if p == nil {
		return
	}
	p.probing = false
	hr := &HealthResult{OK: r.err == nil, At: r.at, Latency: Duration(r.latency)}
	if r.err != nil {
		hr.Error = r.err.Error()
	}
	sv.lastHealth = hr
	if p.stopping {
		sv.publish()
		return
	}
	if r.err == nil {
		p.failures = 0
		if sv.state == StateStarting || sv.state == StateUnhealthy {
			if p.tick != nil {
				p.tick.Reset(sv.spec.Health.interval())
			}
			sv.setState(StateReady, "")
		} else {
			sv.publish()
		}
		return
	}
	p.failures++
	h := sv.spec.Health
	if sv.state == StateStarting {
		if time.Since(p.startedAt) < h.startGrace() {
			sv.publish()
			return
		}
		sv.healthFailed(p, fmt.Sprintf("not ready within %s: %s", h.startGrace(), hr.Error))
		return
	}
	if p.failures < h.failures() {
		if sv.state != StateUnhealthy {
			sv.setState(StateUnhealthy, "health check failed: "+hr.Error)
		} else {
			sv.publish()
		}
		return
	}
	sv.healthFailed(p, fmt.Sprintf("%d consecutive health failures: %s", p.failures, hr.Error))
}

// healthFailed restarts an unhealthy process, or only marks it unhealthy when
// the policy forbids restarts.
func (sv *service) healthFailed(p *proc, reason string) {
	if p.stopping {
		return
	}
	if sv.spec.Restart.policy() == policyNever {
		sv.setState(StateUnhealthy, reason)
		return
	}
	p.healthRestart = true
	sv.stopProc(p, "health restart: "+reason)
}

func describeExit(e Exit) string {
	if e.Signal != "" {
		return "killed by " + e.Signal
	}
	return fmt.Sprintf("exited with code %d", e.Code)
}

// signalName returns the name of a signal known to signalByName, else its
// String form.
func signalName(sig syscall.Signal) string {
	for name, s := range signalByName {
		if s == sig {
			return name
		}
	}
	return sig.String()
}
