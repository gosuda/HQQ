package interop

import (
	"fmt"
	"os"
	"runtime"
	"testing"
	"time"

	"gosuda.org/hqq"
)

const (
	// envLong set to "1" passes --long to every helper.
	envLong = "HQQ_INTEROP_LONG"

	// maxConcurrentScenarios bounds the variant runs in flight at once.
	maxConcurrentScenarios = 4
)

// TestInteropMatrix runs every scenario that both helpers of a pair list, in
// both role assignments, one variant at a time. It is the process-running test;
// the pure logic is covered by the unit tests in driver_unit_test.go.
func TestInteropMatrix(t *testing.T) {
	specs, err := parseImplSpecs(os.Getenv(envImpls))
	if err != nil {
		t.Fatal(err)
	}
	labels := make([]string, 0, len(specs))
	bins := make(map[string]string, len(specs))
	for _, s := range specs {
		labels = append(labels, s.label)
		bins[s.label] = resolveHelper(t, s)
	}
	cats := make(map[string]catalogue, len(specs))
	for _, label := range labels {
		cats[label] = listCatalogue(t, bins[label], label)
	}
	m := &matrix{
		bins:  bins,
		long:  os.Getenv(envLong) == "1",
		slots: make(chan struct{}, maxConcurrentScenarios),
	}
	for _, pair := range unorderedPairs(labels) {
		for _, as := range roleAssignments(pair[0], pair[1]) {
			plans, problems := planPair(cats[as.creator], cats[as.opener], runtime.GOOS)
			for _, msg := range problems {
				t.Error(msg)
			}
			for _, plan := range plans {
				for _, v := range plan.variants {
					g := &group{
						m:        m,
						scenario: plan.id,
						variant:  v,
						timeoutS: plan.timeoutS,
						impl:     map[string]string{roleCreator: as.creator, roleOpener: as.opener},
					}
					t.Run(g.name(), func(t *testing.T) { m.runGroup(t, g) })
				}
			}
		}
	}
}

// matrix is the state shared by all scenario groups.
type matrix struct {
	bins  map[string]string // implementation label -> helper executable
	long  bool
	slots chan struct{} // semaphore over the groups in flight
}

// group is one variant of one scenario for one role assignment. Its steps run
// in order, with one segment name and one sync directory.
type group struct {
	m        *matrix
	scenario string
	variant  catVariant
	timeoutS int
	impl     map[string]string // role -> implementation label
}

// name is the subtest name: "<creator>-<opener>/<scenario>/<variant>".
func (g *group) name() string {
	v := g.variant.Name
	if v == "" {
		v = "default"
	}
	return fmt.Sprintf("%s-%s/%s/%s", g.impl[roleCreator], g.impl[roleOpener], g.scenario, v)
}

// segmentRun is the per-group resources the processes share.
type segmentRun struct {
	name string // segment name (--name)
	dir  string // rendezvous directory (--sync-dir)
}

// runGroup runs the steps of one variant in order. A failed step ends the
// group, because later steps depend on the state it leaves behind.
func (m *matrix) runGroup(t *testing.T, g *group) {
	t.Parallel()
	m.slots <- struct{}{}
	defer func() { <-m.slots }()

	name, err := newRunName()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("", "hqq-interop-")
	if err != nil {
		t.Fatalf("sync dir: %v", err)
	}
	run := segmentRun{name: name, dir: dir}
	defer func() {
		_ = hqq.Unlink(name)
		_ = os.RemoveAll(dir)
	}()
	for i, st := range g.variant.Steps {
		n := i + 1
		if !t.Run(fmt.Sprintf("step%d", n), func(t *testing.T) {
			m.runStep(t, g, run, n, st)
		}) {
			t.Logf("step%d failed: the later steps of this variant are not run", n)
			break
		}
	}
}

// runStep starts one process per role of the step, waits for all of them and
// judges each result. Every failing role is reported; the step is skipped only
// when no role failed and at least one role was skipped by its helper.
func (m *matrix) runStep(t *testing.T, g *group, run segmentRun, n int, st catStep) {
	procs := make([]*proc, 0, len(st.Roles))
	defer func() {
		// Kill whatever is still running, then wait for every process.
		for _, p := range procs {
			p.cancel()
		}
		for _, p := range procs {
			<-p.done
		}
	}()
	for _, role := range st.Roles {
		own := g.impl[role]
		args := buildRunArgs(runArgs{
			scenario: g.scenario,
			variant:  g.variant.Name,
			step:     n,
			role:     role,
			name:     run.name,
			syncDir:  run.dir,
			impl:     own,
			peerImpl: g.impl[otherRole(role)],
			timeoutS: g.timeoutS,
			long:     m.long,
			params:   g.variant.Params,
			extra:    st.Args[role],
		})
		deadline := time.Duration(g.timeoutS)*time.Second + backstopSlack
		procs = append(procs, startProc(m.bins[own], args, deadline))
	}
	for _, p := range procs {
		<-p.done
	}

	skipMsg := ""
	for i, role := range st.Roles {
		want := expectation{scenario: g.scenario, variant: g.variant.Name, step: n, role: role}
		o := procs[i].outcome()
		j := judge(st.Expect[role], want, o)
		switch j.verdict {
		case verdictFail:
			t.Error(describeFailure(want, g.impl[role], g.impl[otherRole(role)], st.Expect[role], o, j.problems))
		case verdictSkip:
			if skipMsg == "" {
				skipMsg = fmt.Sprintf("role %s (impl %s): %s", role, g.impl[role], j.problems[0])
			}
		}
	}
	if skipMsg != "" {
		t.Skip(skipMsg)
	}
}
