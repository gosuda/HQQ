package interop

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync/atomic"
)

// The schemas of conformance/INTEROP_CLI.md: the catalogue printed by `list`
// (section 7) and the result line (section 4). They are written out here, not
// imported from cmd/hqq-interop: the driver depends on the contract alone.
// Unknown JSON fields are ignored everywhere.

// Roles and expectations (sections 3, 7 and 9).
const (
	roleCreator = "creator"
	roleOpener  = "opener"

	expectOK      = "ok"
	expectCrash   = "crash"
	expectOKCrash = "ok|crash"
	expectSkip    = "skip"

	// exitSkipped is the exit status of a role that does not apply (section 3).
	exitSkipped = 4
)

// maxSegmentName is the longest segment name the contract allows (section 2).
const maxSegmentName = 30

// catalogue is the output of `hqq-interop list`. label is the driver's label
// of the helper; it is not part of the JSON.
type catalogue struct {
	label     string
	Scenarios []catScenario `json:"scenarios"`
}

type catScenario struct {
	ID        string       `json:"id"`
	Platforms []string     `json:"platforms"`
	TimeoutS  int          `json:"timeout_s"`
	Variants  []catVariant `json:"variants"`
}

type catVariant struct {
	Name   string    `json:"name"`
	Params catParams `json:"params"`
	Steps  []catStep `json:"steps"`
}

// catParams are the variant parameters the driver passes as flags. Zero means
// absent: the helper's default applies (section 7).
type catParams struct {
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

// catStep is one launch of a scenario: one process per listed role.
type catStep struct {
	Roles  []string            `json:"roles"`
	Expect map[string]string   `json:"expect"`
	Args   map[string][]string `json:"args,omitempty"`
}

// parseCatalogue decodes the output of `hqq-interop list` and checks the parts
// of the schema the driver relies on.
func parseCatalogue(data []byte) (catalogue, error) {
	var c catalogue
	if err := json.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("catalogue is not valid JSON: %w", err)
	}
	seen := make(map[string]bool, len(c.Scenarios))
	for _, s := range c.Scenarios {
		if s.ID == "" {
			return c, errors.New("catalogue: scenario without id")
		}
		if seen[s.ID] {
			return c, fmt.Errorf("catalogue: scenario %s listed twice", s.ID)
		}
		seen[s.ID] = true
		if s.TimeoutS <= 0 {
			return c, fmt.Errorf("scenario %s: timeout_s must be positive", s.ID)
		}
		if len(s.Variants) == 0 {
			return c, fmt.Errorf("scenario %s: no variants", s.ID)
		}
		names := make(map[string]bool, len(s.Variants))
		for _, v := range s.Variants {
			// "" means the first variant to the helper (section 2), so with
			// several variants every name must be non-empty and unique.
			if len(s.Variants) > 1 && (v.Name == "" || names[v.Name]) {
				return c, fmt.Errorf("scenario %s: variant name %q is empty or repeated", s.ID, v.Name)
			}
			names[v.Name] = true
		}
		for _, v := range s.Variants {
			if len(v.Steps) == 0 {
				return c, fmt.Errorf("scenario %s variant %q: no steps", s.ID, v.Name)
			}
			for i, st := range v.Steps {
				if err := validateStep(st); err != nil {
					return c, fmt.Errorf("scenario %s variant %q step %d: %w", s.ID, v.Name, i+1, err)
				}
			}
		}
	}
	return c, nil
}

// validateStep checks that every role of a step is creator or opener and has
// one of the four expectations.
func validateStep(st catStep) error {
	if len(st.Roles) == 0 {
		return errors.New("no roles")
	}
	seen := make(map[string]bool, len(st.Roles))
	for _, r := range st.Roles {
		if r != roleCreator && r != roleOpener {
			return fmt.Errorf("role %q: want creator or opener", r)
		}
		if seen[r] {
			return fmt.Errorf("role %q listed twice", r)
		}
		seen[r] = true
		switch st.Expect[r] {
		case expectOK, expectCrash, expectOKCrash, expectSkip:
		default:
			return fmt.Errorf("role %s: expect %q is not ok, crash, ok|crash or skip", r, st.Expect[r])
		}
	}
	return nil
}

// helperCheck is one named assertion of a role (section 4).
type helperCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// helperResult is the result line of one process (section 4).
type helperResult struct {
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
	Checks    []helperCheck  `json:"checks"`
	Sent      int            `json:"sent"`
	Received  int            `json:"received"`
	ElapsedMs int64          `json:"elapsed_ms"`
	Meta      map[string]any `json:"meta"`
}

// parseResult returns the result line of a process: the last non-empty line
// of its stdout, which must be a JSON object.
func parseResult(stdout []byte) (helperResult, error) {
	var r helperResult
	lines := bytes.Split(stdout, []byte{'\n'})
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}
		if line[0] != '{' {
			return r, fmt.Errorf("last non-empty stdout line is not a JSON object: %q", clip(string(line), 200))
		}
		if err := json.Unmarshal(line, &r); err != nil {
			return r, fmt.Errorf("result line is not valid JSON: %w", err)
		}
		return r, nil
	}
	return r, errors.New("stdout is empty: no result line")
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// otherRole returns the peer role of role.
func otherRole(role string) string {
	if role == roleCreator {
		return roleOpener
	}
	return roleCreator
}

// runCounter makes segment names unique within one driver process.
var runCounter atomic.Uint64

// newRunName returns the segment name of one variant run.
func newRunName() (string, error) {
	return makeRunName(runCounter.Add(1))
}

// makeRunName returns "hqqi-<10 hex digits>-<counter>": 5 random bytes from
// crypto/rand and the counter. It is at most maxSegmentName bytes long and uses
// only [a-z0-9-] (section 2).
func makeRunName(counter uint64) (string, error) {
	var b [5]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("segment name: %w", err)
	}
	name := "hqqi-" + hex.EncodeToString(b[:]) + "-" + strconv.FormatUint(counter, 10)
	if len(name) > maxSegmentName {
		return "", fmt.Errorf("segment name %q is longer than %d bytes", name, maxSegmentName)
	}
	return name, nil
}

// runArgs describes one process: one role of one step.
type runArgs struct {
	scenario string
	variant  string
	step     int
	role     string
	name     string
	syncDir  string
	impl     string
	peerImpl string
	timeoutS int
	long     bool
	params   catParams
	extra    []string // step.args[role], appended verbatim
}

// buildRunArgs returns the argv of `hqq-interop run` (without the program
// name). Numeric parameters are passed in --name=value form and only when
// non-zero, so a negative heartbeat is never read as a flag.
func buildRunArgs(a runArgs) []string {
	args := []string{"run", "--scenario", a.scenario}
	if a.variant != "" {
		args = append(args, "--variant", a.variant)
	}
	args = append(args,
		"--step", strconv.Itoa(a.step),
		"--role", a.role,
		"--name", a.name,
		"--sync-dir", a.syncDir,
		"--impl", a.impl,
		"--peer-impl", a.peerImpl,
	)
	p := a.params
	args = appendIntFlag(args, "--buffer-count", p.BufferCount)
	args = appendIntFlag(args, "--buffer-size", p.BufferSize)
	args = appendIntFlag(args, "--size", p.Size)
	args = appendIntFlag(args, "--messages", p.Messages)
	args = appendUintFlag(args, "--seed", p.Seed)
	args = appendIntFlag(args, "--heartbeat", p.HeartbeatMs)
	args = appendIntFlag(args, "--peer-timeout", p.PeerTimeoutMs)
	args = appendIntFlag(args, "--attach-timeout", p.AttachTimeoutMs)
	args = appendUintFlag(args, "--max-message", p.MaxMessage)
	args = appendUintFlag(args, "--max-reassembly", p.MaxReassembly)
	args = appendIntFlag(args, "--queue-size", p.QueueSize)
	args = append(args, "--timeout="+strconv.Itoa(a.timeoutS))
	if a.long {
		args = append(args, "--long")
	}
	return append(args, a.extra...)
}

func appendIntFlag(args []string, name string, v int) []string {
	if v == 0 {
		return args
	}
	return append(args, name+"="+strconv.Itoa(v))
}

func appendUintFlag(args []string, name string, v uint64) []string {
	if v == 0 {
		return args
	}
	return append(args, name+"="+strconv.FormatUint(v, 10))
}
