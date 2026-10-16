package interop

import (
	"encoding/json"
	"fmt"
	"sort"
)

// assignment is one run of a pair: which implementation is the creator and
// which is the opener.
type assignment struct {
	creator string
	opener  string
}

// unorderedPairs returns each pair {A, B} of implementations once, including
// A == B. The ordered pairs (A, B) and (B, A) describe the same runs: each
// unordered pair yields both role assignments (roleAssignments), so it is
// listed once here.
func unorderedPairs(labels []string) [][2]string {
	var pairs [][2]string
	for i := range labels {
		for j := i; j < len(labels); j++ {
			pairs = append(pairs, [2]string{labels[i], labels[j]})
		}
	}
	return pairs
}

// roleAssignments returns the creator/opener assignments of the pair (a, b):
// creator=a, opener=b, and creator=b, opener=a when a != b. For a == b the two
// assignments are the same runs, so only one is returned.
func roleAssignments(a, b string) []assignment {
	out := []assignment{{creator: a, opener: b}}
	if a != b {
		out = append(out, assignment{creator: b, opener: a})
	}
	return out
}

// scenarioPlan is a scenario that both helpers list, that applies on the host,
// and whose variants and steps are identical in both catalogues.
type scenarioPlan struct {
	id       string
	variants []catVariant
	timeoutS int // the larger timeout_s of the two catalogues
}

// planPair intersects the catalogues of the creator and the opener helper. A
// scenario whose variants or steps differ between the two is not run: the
// cross product is then undefined, and it is reported in problems.
func planPair(creator, opener catalogue, goos string) ([]scenarioPlan, []string) {
	byID := make(map[string]catScenario, len(opener.Scenarios))
	for _, s := range opener.Scenarios {
		byID[s.ID] = s
	}
	var plans []scenarioPlan
	var problems []string
	for _, a := range creator.Scenarios {
		b, ok := byID[a.ID]
		if !ok || !appliesOn(a, goos) || !appliesOn(b, goos) {
			continue
		}
		ka, errA := variantsKey(a.Variants)
		kb, errB := variantsKey(b.Variants)
		if errA != nil || errB != nil {
			problems = append(problems, fmt.Sprintf("scenario %s: cannot encode variants: %v; %v", a.ID, errA, errB))
			continue
		}
		if ka != kb {
			problems = append(problems, fmt.Sprintf("scenario %s: variants or steps differ between %s and %s; not run",
				a.ID, creator.label, opener.label))
			continue
		}
		plans = append(plans, scenarioPlan{id: a.ID, variants: a.Variants, timeoutS: max(a.TimeoutS, b.TimeoutS)})
	}
	sort.Slice(plans, func(i, j int) bool { return plans[i].id < plans[j].id })
	return plans, problems
}

// appliesOn reports whether the scenario runs on goos: its platforms contain
// "any" or goos. An empty platform list means "any".
func appliesOn(s catScenario, goos string) bool {
	if len(s.Platforms) == 0 {
		return true
	}
	for _, p := range s.Platforms {
		if p == "any" || p == goos {
			return true
		}
	}
	return false
}

// variantsKey is a canonical encoding of variants, used to compare catalogues.
func variantsKey(vs []catVariant) (string, error) {
	b, err := json.Marshal(vs)
	return string(b), err
}
