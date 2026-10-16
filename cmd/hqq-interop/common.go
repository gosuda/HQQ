package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"gosuda.org/hqq"
)

// Exit codes (INTEROP_CLI.md, section 3).
const (
	exitOK       = 0
	exitFailed   = 1
	exitUsage    = 2
	exitTimeout  = 3
	exitSkipped  = 4
	exitInternal = 5
)

// errSkip is returned by a scenario that does not apply to this platform or
// this implementation. The helper reports skipped=true and exits 4.
var errSkip = errors.New("skipped")

// checkError is a failed assertion made by a scenario.
type checkError struct{ msg string }

func (e *checkError) Error() string { return e.msg }

// failf builds the error a scenario returns for a failed assertion.
func failf(format string, args ...any) error {
	return &checkError{msg: fmt.Sprintf(format, args...)}
}

// config holds the parsed command line of `hqq-interop run` (INTEROP_CLI.md, section 2).
type config struct {
	Scenario      string
	Variant       string
	Step          int
	Role          string // normalized: "creator" or "opener"
	Name          string
	SyncDir       string
	Impl          string
	PeerImpl      string
	BufferCount   int
	BufferSize    int
	Size          int
	Messages      int
	Seed          uint64
	Timeout       time.Duration
	CrashAfter    string
	HeartbeatMs   int
	PeerTimeoutMs int
	AttachTimeout time.Duration
	Wait          string // "adaptive" (default) or "spin"
	Long          bool
	// Advanced limits (0 = library default). Both sides of a pair must use
	// the same values for limit scenarios; the driver passes them from params.
	AdvMaxMessage    uint64
	AdvMaxReassembly uint64
	AdvQueueSize     int
	// explicit names the flags given on the command line. Variant params
	// fill only the flags that were not given, so an explicit flag wins.
	explicit map[string]bool
}

// params are the per-variant parameters the driver passes as flags. A zero
// value means "not set": the helper applies its default.
type params struct {
	BufferCount     int    `json:"buffer_count,omitempty"`
	BufferSize      int    `json:"buffer_size,omitempty"`
	Size            int    `json:"size,omitempty"`
	Messages        int    `json:"messages,omitempty"`
	Seed            uint64 `json:"seed,omitempty"`
	HeartbeatMs     int    `json:"heartbeat_ms,omitempty"`
	PeerTimeoutMs   int    `json:"peer_timeout_ms,omitempty"`
	AttachTimeoutMs int    `json:"attach_timeout_ms,omitempty"`
	MaxMessage      uint64 `json:"max_message,omitempty"`
	MaxReassembly   uint64 `json:"max_reassembly,omitempty"`
	QueueSize       int    `json:"queue_size,omitempty"`
}

// step is one process launch of a scenario. The driver starts one process
// per listed role, waits for all of them and checks the expectations.
type step struct {
	Roles []string `json:"roles"`
	// Expect maps a role to one of: "ok" (exit 0, ok=true), "crash" (the
	// process kills itself at a crash point and reports crashed=true),
	// "ok|crash", "skip" (exit 4).
	Expect map[string]string `json:"expect"`
	// Args maps a role to extra flags, for example {"opener":["--crash-after","blocked"]}.
	Args map[string][]string `json:"args,omitempty"`
}

// variant is one parameterization of a scenario.
type variant struct {
	Name   string `json:"name"`
	Params params `json:"params"`
	Steps  []step `json:"steps"`
}

// scenario is one registered interop scenario.
type scenario struct {
	ID    string
	Title string
	// Kind is "standard" (Standard protocol, SPEC 9), "advanced" (Advanced
	// protocol, SPEC 10 and the 1.1 draft) or "acceptor" (Listen/Dial).
	Kind string
	// Platforms lists the GOOS values the scenario runs on; "any" for all.
	Platforms []string
	// TimeoutS is the per-process deadline the driver uses, in seconds.
	TimeoutS int
	Variants []variant
	Run      func(e *env) error
}

var registry = map[string]*scenario{}

// register adds a scenario. Called from init functions of the scenario files.
func register(s *scenario) {
	if _, dup := registry[s.ID]; dup {
		panic("hqq-interop: duplicate scenario " + s.ID)
	}
	if len(s.Variants) == 0 {
		panic("hqq-interop: scenario " + s.ID + " has no variants")
	}
	if len(s.Platforms) == 0 {
		s.Platforms = []string{"any"}
	}
	if s.TimeoutS == 0 {
		s.TimeoutS = 30
	}
	registry[s.ID] = s
}

// sortedScenarios returns the registered scenarios ordered by ID.
func sortedScenarios() []*scenario {
	out := make([]*scenario, 0, len(registry))
	for _, s := range registry {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// check is one named assertion inside a result.
type check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// result is the single JSON object printed on stdout (INTEROP_CLI.md, section 4).
type result struct {
	Scenario  string         `json:"scenario"`
	Variant   string         `json:"variant"`
	Step      int            `json:"step"`
	Role      string         `json:"role"`
	Impl      string         `json:"impl"`
	PeerImpl  string         `json:"peer_impl"`
	OK        bool           `json:"ok"`
	Skipped   bool           `json:"skipped"`
	Crashed   bool           `json:"crashed"`
	ErrorKind string         `json:"error_kind"`
	Error     string         `json:"error"`
	Checks    []check        `json:"checks"`
	Sent      int            `json:"sent"`
	Received  int            `json:"received"`
	ElapsedMs int64          `json:"elapsed_ms"`
	Meta      map[string]any `json:"meta,omitempty"`
}

// env is the state of one scenario run in one process.
type env struct {
	cfg    config
	ctx    context.Context
	res    result
	start  time.Time
	failed bool
	stdout io.Writer
}

// check records an assertion. A failed check makes the run fail.
func (e *env) check(name string, ok bool, detail string) {
	e.res.Checks = append(e.res.Checks, check{Name: name, OK: ok, Detail: detail})
	if !ok {
		e.failed = true
	}
}

// checkErr records an assertion that err matches the sentinel want (errors.Is).
func (e *env) checkErr(name string, err, want error) {
	if want == nil {
		e.check(name, err == nil, errText(err))
		return
	}
	e.check(name, errors.Is(err, want), errText(err))
}

// meta attaches a value to the result (diagnostic only).
func (e *env) meta(key string, v any) {
	if e.res.Meta == nil {
		e.res.Meta = map[string]any{}
	}
	e.res.Meta[key] = v
}

// other returns the peer role of role.
func other(role string) string {
	if role == "creator" {
		return "opener"
	}
	return "creator"
}

// barrier synchronizes this process with the peer process of the same step
// (INTEROP_CLI.md, section 6): it creates <sync-dir>/s<step>-<phase>.<role>
// and waits until <sync-dir>/s<step>-<phase>.<peer role> exists.
func (e *env) barrier(phase string) error {
	base := fmt.Sprintf("s%d-%s", e.cfg.Step, phase)
	mine := filepath.Join(e.cfg.SyncDir, base+"."+e.cfg.Role)
	if err := os.WriteFile(mine, nil, 0o644); err != nil {
		return fmt.Errorf("barrier %s: %w", phase, err)
	}
	peer := filepath.Join(e.cfg.SyncDir, base+"."+other(e.cfg.Role))
	for {
		if _, err := os.Stat(peer); err == nil {
			return nil
		}
		select {
		case <-e.ctx.Done():
			return fmt.Errorf("barrier %s: waiting for peer: %w", phase, e.ctx.Err())
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// maybeCrash kills this process with SIGKILL (TerminateProcess on Windows)
// when point equals --crash-after. The result line is printed first with
// crashed=true and ok=true, so the driver can tell an injected crash from a
// real one. It returns only when point does not match.
func (e *env) maybeCrash(point string) {
	if e.cfg.CrashAfter != point {
		return
	}
	e.res.Crashed = true
	e.res.OK = !e.failed
	e.finish()
	killSelf()
}

// finish stamps the elapsed time and prints the result line.
func (e *env) finish() {
	e.res.ElapsedMs = time.Since(e.start).Milliseconds()
	printResult(e.stdout, &e.res)
}

// errorKind maps an error to the canonical kind name of INTEROP_CLI.md,
// section 4. The order matters: more specific sentinels come first.
func errorKind(err error) string {
	if err == nil {
		return ""
	}
	var chk *checkError
	if errors.As(err, &chk) {
		return "CheckFailed"
	}
	var re hqq.ResponseError
	if errors.As(err, &re) {
		return "ResponseError"
	}
	for _, m := range errorKinds {
		if errors.Is(err, m.err) {
			return m.name
		}
	}
	return "Unknown"
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// printResult writes r as one JSON line.
func printResult(w io.Writer, r *result) {
	if r.Checks == nil {
		r.Checks = []check{}
	}
	b, err := json.Marshal(r)
	if err != nil {
		b = []byte(fmt.Sprintf(`{"ok":false,"error_kind":"Internal","error":%q}`, err.Error()))
	}
	_, _ = w.Write(append(b, '\n'))
}

// listDoc is the output of `hqq-interop list`.
type listDoc struct {
	Helper    string         `json:"helper"`
	Impl      string         `json:"impl"`
	Protocol  string         `json:"protocol"`
	Layout    string         `json:"layout"`
	Runtime   string         `json:"runtime"`
	Scenarios []scenarioInfo `json:"scenarios"`
}

type scenarioInfo struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Kind      string    `json:"kind"`
	Platforms []string  `json:"platforms"`
	TimeoutS  int       `json:"timeout_s"`
	Variants  []variant `json:"variants"`
}

func listScenarios(impl string) listDoc {
	doc := listDoc{
		Helper:    "hqq-interop",
		Impl:      impl,
		Protocol:  "1.1",
		Layout:    "HQQ_STL3",
		Runtime:   runtime.Version(),
		Scenarios: []scenarioInfo{},
	}
	for _, s := range sortedScenarios() {
		doc.Scenarios = append(doc.Scenarios, scenarioInfo{
			ID: s.ID, Title: s.Title, Kind: s.Kind, Platforms: s.Platforms,
			TimeoutS: s.TimeoutS, Variants: s.Variants,
		})
	}
	return doc
}

// platformOK reports whether the scenario runs on this GOOS.
func platformOK(s *scenario) bool {
	for _, p := range s.Platforms {
		if p == "any" || p == runtime.GOOS {
			return true
		}
	}
	return false
}

// parseFlags parses the arguments of `run` into a config.
func parseFlags(args []string, stderr io.Writer) (config, error) {
	var c config
	var sec float64 = 20
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&c.Scenario, "scenario", "", "scenario ID, for example X-01")
	fs.StringVar(&c.Variant, "variant", "", "variant name (default: the first variant)")
	fs.IntVar(&c.Step, "step", 1, "1-based step number")
	role := fs.String("role", "", "creator|opener|a|b (a = creator, b = opener)")
	fs.StringVar(&c.Name, "name", "", "segment name (driver-generated, unique per run)")
	fs.StringVar(&c.SyncDir, "sync-dir", "", "rendezvous directory shared by the processes of one run")
	fs.StringVar(&c.Impl, "impl", "go", "label of this implementation in results")
	fs.StringVar(&c.PeerImpl, "peer-impl", "", "label of the peer implementation")
	fs.IntVar(&c.BufferCount, "buffer-count", 4, "slots per direction (power of two)")
	fs.IntVar(&c.BufferSize, "buffer-size", 256, "payload bytes per slot (multiple of 8)")
	fs.IntVar(&c.Size, "size", 0, "message payload size (0 = buffer-size)")
	fs.IntVar(&c.Messages, "messages", 100, "number of messages")
	fs.Uint64Var(&c.Seed, "seed", 1, "seed of the deterministic payload generator")
	fs.Float64Var(&sec, "timeout", 20, "hard deadline of this process in seconds")
	fs.StringVar(&c.CrashAfter, "crash-after", "", "crash point at which to SIGKILL this process")
	fs.IntVar(&c.HeartbeatMs, "heartbeat", 0, "heartbeat interval in ms (0 = default 250, negative = disabled)")
	fs.IntVar(&c.PeerTimeoutMs, "peer-timeout", 0, "peer timeout in ms (0 = default 5000)")
	attach := fs.Int("attach-timeout", 0, "attach timeout in ms (0 = default 5000)")
	fs.StringVar(&c.Wait, "wait", "adaptive", "wait policy: adaptive|spin")
	fs.BoolVar(&c.Long, "long", false, "run long soak sizes where a scenario has them")
	fs.Uint64Var(&c.AdvMaxMessage, "max-message", 0, "Advanced MaxMessageSize in bytes (0 = default 64 MiB)")
	fs.Uint64Var(&c.AdvMaxReassembly, "max-reassembly", 0, "Advanced MaxReassemblyBytes in bytes (0 = 4 x MaxMessageSize)")
	fs.IntVar(&c.AdvQueueSize, "queue-size", 0, "Advanced IncomingQueueSize (0 = buffer-count)")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	c.explicit = map[string]bool{}
	fs.Visit(func(f *flag.Flag) { c.explicit[f.Name] = true })
	if fs.NArg() != 0 {
		return c, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	switch *role {
	case "creator", "a":
		c.Role = "creator"
	case "opener", "b":
		c.Role = "opener"
	default:
		return c, fmt.Errorf("--role must be creator|opener|a|b, got %q", *role)
	}
	if c.Scenario == "" {
		return c, errors.New("--scenario is required")
	}
	if c.Name == "" {
		return c, errors.New("--name is required")
	}
	if c.Step < 1 {
		return c, errors.New("--step must be >= 1")
	}
	if c.Messages < 0 || c.BufferCount <= 0 || c.BufferSize <= 0 || c.Size < 0 {
		return c, errors.New("sizes and counts must be positive")
	}
	if c.Wait != "adaptive" && c.Wait != "spin" {
		return c, fmt.Errorf("--wait must be adaptive|spin, got %q", c.Wait)
	}
	c.Timeout = time.Duration(sec * float64(time.Second))
	c.AttachTimeout = time.Duration(*attach) * time.Millisecond
	return c, nil
}

// runCommand implements `hqq-interop run`.
func runCommand(args []string, stdout, stderr io.Writer) int {
	cfg, err := parseFlags(args, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "hqq-interop:", err)
		return exitUsage
	}
	s := registry[cfg.Scenario]
	if s == nil {
		fmt.Fprintf(stderr, "hqq-interop: unknown scenario %q\n", cfg.Scenario)
		return exitUsage
	}
	v, ok := findVariant(s, cfg.Variant)
	if !ok {
		fmt.Fprintf(stderr, "hqq-interop: scenario %s has no variant %q\n", s.ID, cfg.Variant)
		return exitUsage
	}
	if cfg.Step > len(v.Steps) {
		fmt.Fprintf(stderr, "hqq-interop: scenario %s variant %q has %d steps\n", s.ID, v.Name, len(v.Steps))
		return exitUsage
	}
	if !containsString(v.Steps[cfg.Step-1].Roles, cfg.Role) {
		fmt.Fprintf(stderr, "hqq-interop: role %q is not part of step %d of %s\n", cfg.Role, cfg.Step, s.ID)
		return exitUsage
	}
	cfg.Variant = v.Name
	applyParams(&cfg, v.Params)
	if cfg.Size == 0 {
		cfg.Size = cfg.BufferSize
	}

	e := &env{cfg: cfg, start: time.Now(), stdout: stdout}
	e.res = result{Scenario: s.ID, Variant: v.Name, Step: cfg.Step, Role: cfg.Role,
		Impl: cfg.Impl, PeerImpl: cfg.PeerImpl, Checks: []check{}}
	if !platformOK(s) {
		e.res.Skipped, e.res.OK = true, true
		e.res.Error = "scenario not applicable on " + runtime.GOOS
		e.finish()
		return exitSkipped
	}
	if cfg.SyncDir == "" && len(v.Steps[cfg.Step-1].Roles) > 1 {
		fmt.Fprintln(stderr, "hqq-interop: --sync-dir is required for multi-process steps")
		return exitUsage
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()
	e.ctx = ctx

	// Watchdog: if the scenario hangs past its deadline, report and exit.
	watchdog := time.AfterFunc(cfg.Timeout+2*time.Second, func() {
		e.res.OK = false
		e.res.ErrorKind = "HarnessTimeout"
		e.res.Error = "process exceeded its deadline"
		e.finish()
		os.Exit(exitTimeout)
	})
	defer watchdog.Stop()

	err = s.Run(e)
	switch {
	case errors.Is(err, errSkip):
		e.res.Skipped, e.res.OK = true, true
		e.res.Error = errText(err)
		e.finish()
		return exitSkipped
	case err == nil && !e.failed:
		e.res.OK = true
		e.finish()
		return exitOK
	default:
		if err == nil {
			err = failf("one or more checks failed")
		}
		e.res.OK = false
		e.res.ErrorKind = errorKind(err)
		e.res.Error = errText(err)
		e.finish()
		return exitFailed
	}
}

func findVariant(s *scenario, name string) (variant, bool) {
	if name == "" {
		return s.Variants[0], true
	}
	for _, v := range s.Variants {
		if v.Name == name {
			return v, true
		}
	}
	return variant{}, false
}

// applyParams fills the config fields whose flags were not given explicitly
// from the variant parameters. An explicit flag always wins, so a step can
// override the variant with its own args (for example the X-02 opener).
func applyParams(c *config, p params) {
	set := func(flag string, given bool, apply func()) {
		if given && !c.explicit[flag] {
			apply()
		}
	}
	set("buffer-count", p.BufferCount != 0, func() { c.BufferCount = p.BufferCount })
	set("buffer-size", p.BufferSize != 0, func() { c.BufferSize = p.BufferSize })
	set("size", p.Size != 0, func() { c.Size = p.Size })
	set("messages", p.Messages != 0, func() { c.Messages = p.Messages })
	set("seed", p.Seed != 0, func() { c.Seed = p.Seed })
	set("heartbeat", p.HeartbeatMs != 0, func() { c.HeartbeatMs = p.HeartbeatMs })
	set("peer-timeout", p.PeerTimeoutMs != 0, func() { c.PeerTimeoutMs = p.PeerTimeoutMs })
	set("attach-timeout", p.AttachTimeoutMs != 0, func() {
		c.AttachTimeout = time.Duration(p.AttachTimeoutMs) * time.Millisecond
	})
	set("max-message", p.MaxMessage != 0, func() { c.AdvMaxMessage = p.MaxMessage })
	set("max-reassembly", p.MaxReassembly != 0, func() { c.AdvMaxReassembly = p.MaxReassembly })
	set("queue-size", p.QueueSize != 0, func() { c.AdvQueueSize = p.QueueSize })
}

// main dispatch helpers ----------------------------------------------------

// errorKinds maps sentinel errors to their canonical names, most specific first.
// Filled in by errors.go.
var errorKinds []errorKindEntry

type errorKindEntry struct {
	err  error
	name string
}
