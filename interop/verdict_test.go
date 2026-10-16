package interop

import (
	"fmt"
	"strings"
)

// expectation is what a role's result line must echo and which expectation
// the step set for the role (sections 4 and 7).
type expectation struct {
	scenario string
	variant  string // "" = not checked
	step     int
	role     string
}

// roleOutcome is what the driver knows about one finished process.
type roleOutcome struct {
	exitCode int           // -1: killed by a signal
	backstop bool          // the driver's deadline killed the process
	result   *helperResult // nil when no result line could be parsed
	parseErr string        // why result is nil
	stderr   string        // tail of stderr
}

type verdict int

const (
	verdictPass verdict = iota
	verdictSkip
	verdictFail
)

// judgement is the verdict on one role. For verdictFail, problems lists the
// reasons; for verdictSkip, problems[0] is the skip reason.
type judgement struct {
	verdict  verdict
	problems []string
}

func passed() judgement {
	return judgement{verdict: verdictPass}
}

func failed(msg string) judgement {
	return judgement{verdict: verdictFail, problems: []string{msg}}
}

// judge evaluates the expectation of one role against its outcome.
//
//   - ok: exit 0 and ok=true (a skip, exit 4 with skipped=true, is a skip).
//   - crash: crashed=true and a non-zero status (a signal counts). The printed
//     line decides; the status only has to show that the process did not exit 0.
//   - ok|crash: crash when the line says crashed=true, ok otherwise.
//   - skip: exit 4 with skipped=true.
func judge(expect string, want expectation, o roleOutcome) judgement {
	if o.backstop {
		return failed("the driver's backstop deadline passed; the process was killed")
	}
	r := o.result
	if r == nil {
		return failed(fmt.Sprintf("no result line (%s); exit %d", o.parseErr, o.exitCode))
	}
	var echo []string
	if r.Scenario != want.scenario {
		echo = append(echo, fmt.Sprintf("result scenario %q, want %q", r.Scenario, want.scenario))
	}
	if want.variant != "" && r.Variant != want.variant {
		echo = append(echo, fmt.Sprintf("result variant %q, want %q", r.Variant, want.variant))
	}
	if r.Step != want.step {
		echo = append(echo, fmt.Sprintf("result step %d, want %d", r.Step, want.step))
	}
	if r.Role != want.role {
		echo = append(echo, fmt.Sprintf("result role %q, want %q", r.Role, want.role))
	}
	if len(echo) > 0 {
		return judgement{verdict: verdictFail, problems: echo}
	}
	switch expect {
	case expectOK:
		return judgeOK(o, r)
	case expectCrash:
		return judgeCrash(o, r)
	case expectOKCrash:
		if r.Crashed {
			return judgeCrash(o, r)
		}
		return judgeOK(o, r)
	case expectSkip:
		if o.exitCode == exitSkipped && r.Skipped {
			return passed()
		}
		return failed(fmt.Sprintf("exit %d, skipped=%t; want exit 4 with skipped=true", o.exitCode, r.Skipped))
	default:
		return failed(fmt.Sprintf("unknown expectation %q", expect))
	}
}

func judgeOK(o roleOutcome, r *helperResult) judgement {
	if r.Skipped {
		if o.exitCode == exitSkipped {
			return judgement{verdict: verdictSkip, problems: []string{skipText(r)}}
		}
		return failed(fmt.Sprintf("skipped=true with exit %d; a skip exits 4", o.exitCode))
	}
	var probs []string
	if o.exitCode == exitSkipped {
		probs = append(probs, "exit 4 without skipped=true")
	} else if o.exitCode != 0 {
		probs = append(probs, fmt.Sprintf("exit %d, want 0", o.exitCode))
	}
	if !r.OK {
		probs = append(probs, "ok=false")
	}
	probs = append(probs, failedChecks(r)...)
	if len(probs) > 0 {
		return judgement{verdict: verdictFail, problems: probs}
	}
	return passed()
}

func judgeCrash(o roleOutcome, r *helperResult) judgement {
	var probs []string
	if r.Skipped {
		probs = append(probs, "skipped=true, want a crash")
	}
	if !r.Crashed {
		probs = append(probs, "crashed=false, want a crash at the crash point")
	}
	if o.exitCode == 0 {
		probs = append(probs, "exit 0; a crashed process must die by signal or exit non-zero")
	}
	probs = append(probs, failedChecks(r)...)
	if len(probs) > 0 {
		return judgement{verdict: verdictFail, problems: probs}
	}
	return passed()
}

// failedChecks lists the assertions of a result that did not hold.
func failedChecks(r *helperResult) []string {
	var out []string
	for _, c := range r.Checks {
		if !c.OK {
			out = append(out, fmt.Sprintf("check %q failed: %s", c.Name, c.Detail))
		}
	}
	return out
}

func skipText(r *helperResult) string {
	if r.Error == "" {
		return "helper skipped"
	}
	return "helper skipped: " + r.Error
}

// describeFailure formats the report of one failing role.
func describeFailure(want expectation, impl, peer, expect string, o roleOutcome, problems []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "scenario %s variant %q step %d role %s: impl %s (peer %s), expect %s, exit %d\n",
		want.scenario, want.variant, want.step, want.role, impl, peer, expect, o.exitCode)
	if r := o.result; r != nil {
		fmt.Fprintf(&b, "  error_kind: %s\n  error: %s\n", r.ErrorKind, r.Error)
	}
	for _, p := range problems {
		fmt.Fprintf(&b, "  problem: %s\n", p)
	}
	if o.stderr != "" {
		fmt.Fprintf(&b, "  stderr tail:\n    %s\n", strings.ReplaceAll(strings.TrimRight(o.stderr, "\n"), "\n", "\n    "))
	}
	return b.String()
}
