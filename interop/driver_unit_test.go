package interop

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

// Fast unit tests of the driver's pure logic: no helper process is started.

func TestPairsAndRoleAssignments(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		pairs  [][2]string
		runs   int
	}{
		{
			name:   "one implementation runs once",
			labels: []string{"go"},
			pairs:  [][2]string{{"go", "go"}},
			runs:   1,
		},
		{
			name:   "two implementations run both assignments of the mixed pair",
			labels: []string{"go", "c"},
			pairs:  [][2]string{{"go", "go"}, {"go", "c"}, {"c", "c"}},
			runs:   4,
		},
		{
			name:   "three implementations",
			labels: []string{"go", "c", "rs"},
			pairs:  [][2]string{{"go", "go"}, {"go", "c"}, {"go", "rs"}, {"c", "c"}, {"c", "rs"}, {"rs", "rs"}},
			runs:   9,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pairs := unorderedPairs(tc.labels)
			if !reflect.DeepEqual(pairs, tc.pairs) {
				t.Fatalf("pairs = %v, want %v", pairs, tc.pairs)
			}
			runs := 0
			for _, p := range pairs {
				runs += len(roleAssignments(p[0], p[1]))
			}
			if runs != tc.runs {
				t.Fatalf("runs = %d, want %d", runs, tc.runs)
			}
		})
	}

	if got, want := roleAssignments("go", "go"), []assignment{{creator: "go", opener: "go"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("roleAssignments(go, go) = %v, want %v", got, want)
	}
	want := []assignment{{creator: "go", opener: "c"}, {creator: "c", opener: "go"}}
	if got := roleAssignments("go", "c"); !reflect.DeepEqual(got, want) {
		t.Errorf("roleAssignments(go, c) = %v, want %v", got, want)
	}
}

func TestRunNames(t *testing.T) {
	seen := make(map[string]bool)
	for i := range 1000 {
		name, err := makeRunName(uint64(i + 1))
		if err != nil {
			t.Fatal(err)
		}
		if len(name) > maxSegmentName {
			t.Fatalf("name %q is %d bytes, max %d", name, len(name), maxSegmentName)
		}
		for _, c := range []byte(name) {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				t.Fatalf("name %q has byte %q outside [a-z0-9-]", name, c)
			}
		}
		if seen[name] {
			t.Fatalf("name %q drawn twice", name)
		}
		seen[name] = true
	}

	// The random part alone must be distinct across draws (counter fixed).
	randomOnly := make(map[string]bool)
	for range 1000 {
		name, err := makeRunName(1)
		if err != nil {
			t.Fatal(err)
		}
		if randomOnly[name] {
			t.Fatalf("random part repeated: %q", name)
		}
		randomOnly[name] = true
	}

	if _, err := makeRunName(1 << 60); err == nil {
		t.Error("a name longer than maxSegmentName was accepted")
	}
	name, err := newRunName()
	if err != nil || !strings.HasPrefix(name, "hqqi-") {
		t.Errorf("newRunName() = %q, %v", name, err)
	}
}

func TestBuildRunArgs(t *testing.T) {
	got := buildRunArgs(runArgs{
		scenario: "X-01", variant: "v", step: 2, role: roleOpener, name: "n", syncDir: "/d",
		impl: "go", peerImpl: "c", timeoutS: 30, long: true,
		params: catParams{BufferCount: 8, Messages: 3, Seed: 7, HeartbeatMs: -1, MaxMessage: 1024},
		extra:  []string{"--crash-after", "claim"},
	})
	want := []string{
		"run", "--scenario", "X-01", "--variant", "v", "--step", "2", "--role", "opener",
		"--name", "n", "--sync-dir", "/d", "--impl", "go", "--peer-impl", "c",
		"--buffer-count=8", "--messages=3", "--seed=7", "--heartbeat=-1", "--max-message=1024",
		"--timeout=30", "--long", "--crash-after", "claim",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("buildRunArgs =\n%q\nwant\n%q", got, want)
	}

	// Zero parameters, no variant name, no long mode: only the required flags.
	got = buildRunArgs(runArgs{scenario: "X-02", step: 1, role: roleCreator, name: "n", syncDir: "d",
		impl: "go", peerImpl: "go", timeoutS: 5})
	want = []string{
		"run", "--scenario", "X-02", "--step", "1", "--role", "creator", "--name", "n",
		"--sync-dir", "d", "--impl", "go", "--peer-impl", "go", "--timeout=5",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("buildRunArgs (defaults) =\n%q\nwant\n%q", got, want)
	}
}

func TestJudge(t *testing.T) {
	base := func(mods ...func(*helperResult)) *helperResult {
		r := &helperResult{Scenario: "X-01", Variant: "v", Step: 1, Role: roleCreator, OK: true}
		for _, m := range mods {
			m(r)
		}
		return r
	}
	okCheck := func(r *helperResult) { r.Checks = []helperCheck{{Name: "a", OK: true}} }
	badCheck := func(r *helperResult) { r.Checks = []helperCheck{{Name: "seen", OK: false, Detail: "got 2"}} }
	notOK := func(r *helperResult) { r.OK = false }
	crashed := func(r *helperResult) { r.Crashed = true }
	skipped := func(r *helperResult) { r.Skipped = true; r.Error = "needs windows" }
	wantFail := verdictFail

	want := expectation{scenario: "X-01", variant: "v", step: 1, role: roleCreator}
	cases := []struct {
		name    string
		expect  string
		want    expectation
		outcome roleOutcome
		verdict verdict
	}{
		{"ok passes", expectOK, want, roleOutcome{exitCode: 0, result: base(okCheck)}, verdictPass},
		{"ok with exit 1", expectOK, want, roleOutcome{exitCode: 1, result: base()}, wantFail},
		{"ok with ok=false", expectOK, want, roleOutcome{exitCode: 0, result: base(notOK)}, wantFail},
		{"ok with a failed check", expectOK, want, roleOutcome{exitCode: 0, result: base(badCheck)}, wantFail},
		{"ok with skipped=true at exit 0", expectOK, want, roleOutcome{exitCode: 0, result: base(skipped)}, wantFail},
		{"ok skipped at exit 4 is a skip", expectOK, want, roleOutcome{exitCode: 4, result: base(skipped)}, verdictSkip},
		{"ok at exit 4 without skipped", expectOK, want, roleOutcome{exitCode: 4, result: base()}, wantFail},
		{"crash with the flag", expectCrash, want, roleOutcome{exitCode: -1, result: base(crashed)}, verdictPass},
		{"crash without the flag", expectCrash, want, roleOutcome{exitCode: -1, result: base()}, wantFail},
		{"crash flag but exit 0", expectCrash, want, roleOutcome{exitCode: 0, result: base(crashed)}, wantFail},
		{"crash with a failed check", expectCrash, want, roleOutcome{exitCode: -1, result: base(crashed, badCheck)}, wantFail},
		{"ok|crash crashed", expectOKCrash, want, roleOutcome{exitCode: -1, result: base(crashed)}, verdictPass},
		{"ok|crash ok", expectOKCrash, want, roleOutcome{exitCode: 0, result: base()}, verdictPass},
		{"ok|crash neither", expectOKCrash, want, roleOutcome{exitCode: 1, result: base(notOK)}, wantFail},
		{"skip passes", expectSkip, want, roleOutcome{exitCode: 4, result: base(skipped)}, verdictPass},
		{"skip at exit 0 fails", expectSkip, want, roleOutcome{exitCode: 0, result: base()}, wantFail},
		{"wrong scenario echo", expectOK, want, roleOutcome{exitCode: 0, result: base(func(r *helperResult) { r.Scenario = "X-02" })}, wantFail},
		{"wrong role echo", expectOK, want, roleOutcome{exitCode: 0, result: base(func(r *helperResult) { r.Role = roleOpener })}, wantFail},
		{"wrong step echo", expectOK, want, roleOutcome{exitCode: 0, result: base(func(r *helperResult) { r.Step = 2 })}, wantFail},
		{"wrong variant echo", expectOK, want, roleOutcome{exitCode: 0, result: base(func(r *helperResult) { r.Variant = "w" })}, wantFail},
		{"unchecked variant when the step names none", expectOK,
			expectation{scenario: "X-01", step: 1, role: roleCreator},
			roleOutcome{exitCode: 0, result: base(func(r *helperResult) { r.Variant = "any" })}, verdictPass},
		{"no result line", expectOK, want, roleOutcome{exitCode: 3, parseErr: "stdout is empty"}, wantFail},
		{"backstop kill", expectOK, want, roleOutcome{exitCode: -1, backstop: true, result: base(okCheck)}, wantFail},
		{"unknown expectation", "maybe", want, roleOutcome{exitCode: 0, result: base()}, wantFail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			j := judge(tc.expect, tc.want, tc.outcome)
			if j.verdict != tc.verdict {
				t.Fatalf("verdict = %v (%q), want %v", j.verdict, j.problems, tc.verdict)
			}
			if j.verdict != verdictPass && len(j.problems) == 0 {
				t.Fatal("non-pass verdict without a reason")
			}
		})
	}

	j := judge(expectOK, want, roleOutcome{exitCode: 4, result: base(skipped)})
	if len(j.problems) != 1 || !strings.Contains(j.problems[0], "needs windows") {
		t.Errorf("skip reason = %q, want the helper's error text", j.problems)
	}
	j = judge(expectOK, want, roleOutcome{exitCode: 0, result: base(badCheck)})
	if len(j.problems) == 0 || !strings.Contains(j.problems[0], `check "seen" failed: got 2`) {
		t.Errorf("failed check not reported: %q", j.problems)
	}
}

func TestParseResult(t *testing.T) {
	good := `{"scenario":"X-01","variant":"","step":1,"role":"creator","impl":"go","peer_impl":"c",` +
		`"ok":true,"skipped":false,"crashed":false,"error_kind":"","error":"",` +
		`"checks":[{"name":"opener sees the message","ok":true,"detail":""}],"sent":1,"received":1,` +
		`"elapsed_ms":42,"meta":{"epoch":2},"future_field":{"x":[1,2]},"another":true}`
	other := `{"scenario":"X-09","ok":false}`

	r, err := parseResult([]byte("warning: noise on stdout\n" + other + "\n" + good + "\r\n\n  \n"))
	if err != nil {
		t.Fatalf("parseResult: %v", err)
	}
	if r.Scenario != "X-01" || !r.OK || r.Impl != "go" || r.PeerImpl != "c" || r.Step != 1 {
		t.Errorf("last non-empty line not used: %+v", r)
	}
	if len(r.Checks) != 1 || r.Checks[0].Name != "opener sees the message" || r.Sent != 1 {
		t.Errorf("checks or counters lost: %+v", r)
	}

	for name, stdout := range map[string]string{
		"empty":          "",
		"blank lines":    "\n \n\n",
		"plain text":     "hello\n",
		"array":          "[1,2]\n",
		"null":           "null\n",
		"truncated JSON": `{"scenario":"X-01",`,
	} {
		if _, err := parseResult([]byte(stdout)); err == nil {
			t.Errorf("%s: parseResult accepted %q", name, stdout)
		}
	}
}

func TestTailBuffer(t *testing.T) {
	var b tailBuffer
	b.Write(bytes.Repeat([]byte("a"), 20000))
	b.Write([]byte("END"))
	s := b.String()
	if !strings.HasSuffix(s, "END") {
		t.Errorf("tail does not end with the last write")
	}
	if !strings.Contains(s, "truncated") {
		t.Errorf("truncation not reported")
	}
	if len(s) > stderrTailMax+100 {
		t.Errorf("tail is %d bytes, want at most %d", len(s), stderrTailMax+100)
	}
}

func TestParseImplSpecs(t *testing.T) {
	ok := map[string][]implSpec{
		"":                      {{label: "go"}},
		"   ":                   {{label: "go"}},
		"go=":                   {{label: "go"}},
		"go=,c=/x/hqq-interop":  {{label: "go"}, {label: "c", path: "/x/hqq-interop"}},
		"c=/a=b":                {{label: "c", path: "/a=b"}},
		"go=/bin/go-helper,c=1": {{label: "go", path: "/bin/go-helper"}, {label: "c", path: "1"}},
	}
	for in, want := range ok {
		got, err := parseImplSpecs(in)
		if err != nil {
			t.Errorf("parseImplSpecs(%q): %v", in, err)
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("parseImplSpecs(%q) = %v, want %v", in, got, want)
		}
	}
	for _, in := range []string{"go", "Go=/x", "c=", "go=/a,go=/b", "go=/a,", "go=/a,,c=/b", "go-x=/a", "c_1=/a"} {
		if _, err := parseImplSpecs(in); err == nil {
			t.Errorf("parseImplSpecs(%q) accepted", in)
		}
	}
}

func TestParseCatalogue(t *testing.T) {
	doc := `{"helper":"hqq-interop","impl":"go","future":1,"scenarios":[{"id":"X-01","title":"t","kind":"standard",
	 "platforms":["any"],"timeout_s":30,"unknown":{},
	 "variants":[{"name":"v","params":{"buffer_size":256,"heartbeat_ms":-1,"extra":2},
	   "steps":[{"roles":["creator","opener"],"expect":{"creator":"ok","opener":"ok|crash"},
	     "args":{"opener":["--crash-after","claim"]}}]}]}]}`
	c, err := parseCatalogue([]byte(doc))
	if err != nil {
		t.Fatalf("parseCatalogue: %v", err)
	}
	if len(c.Scenarios) != 1 || c.Scenarios[0].Variants[0].Params.HeartbeatMs != -1 {
		t.Fatalf("catalogue not decoded: %+v", c)
	}
	st := c.Scenarios[0].Variants[0].Steps[0]
	if st.Args["opener"][1] != "claim" || st.Expect["opener"] != expectOKCrash {
		t.Errorf("step not decoded: %+v", st)
	}

	bad := map[string]string{
		"unknown expectation": `{"scenarios":[{"id":"X","timeout_s":1,"variants":[{"name":"","steps":[{"roles":["creator"],"expect":{"creator":"maybe"}}]}]}]}`,
		"role a":              `{"scenarios":[{"id":"X","timeout_s":1,"variants":[{"name":"","steps":[{"roles":["a"],"expect":{"a":"ok"}}]}]}]}`,
		"role without expect": `{"scenarios":[{"id":"X","timeout_s":1,"variants":[{"name":"","steps":[{"roles":["opener"],"expect":{}}]}]}]}`,
		"zero timeout":        `{"scenarios":[{"id":"X","timeout_s":0,"variants":[{"name":"","steps":[{"roles":["opener"],"expect":{"opener":"ok"}}]}]}]}`,
		"no variants":         `{"scenarios":[{"id":"X","timeout_s":1,"variants":[]}]}`,
		"unnamed variant":     `{"scenarios":[{"id":"X","timeout_s":1,"variants":[{"name":"a","steps":[{"roles":["opener"],"expect":{"opener":"ok"}}]},{"name":"","steps":[{"roles":["opener"],"expect":{"opener":"ok"}}]}]}]}`,
		"duplicate id":        `{"scenarios":[{"id":"X","timeout_s":1,"variants":[{"name":"","steps":[{"roles":["opener"],"expect":{"opener":"ok"}}]}]},{"id":"X","timeout_s":1,"variants":[{"name":"","steps":[{"roles":["opener"],"expect":{"opener":"ok"}}]}]}]}`,
	}
	for name, in := range bad {
		if _, err := parseCatalogue([]byte(in)); err == nil {
			t.Errorf("%s: parseCatalogue accepted the document", name)
		}
	}
}

func TestPlanPair(t *testing.T) {
	creatorDoc := `{"scenarios":[
	 {"id":"X-01","platforms":["any"],"timeout_s":10,"variants":[{"name":"v","params":{"messages":2},"steps":[{"roles":["creator","opener"],"expect":{"creator":"ok","opener":"ok"}}]}]},
	 {"id":"X-02","platforms":["windows"],"timeout_s":10,"variants":[{"name":"v","steps":[{"roles":["creator"],"expect":{"creator":"ok"}}]}]},
	 {"id":"X-03","timeout_s":10,"variants":[{"name":"v","steps":[{"roles":["creator"],"expect":{"creator":"ok"}}]}]},
	 {"id":"X-04","platforms":["any"],"timeout_s":10,"variants":[{"name":"v","steps":[{"roles":["creator"],"expect":{"creator":"ok"}}]}]}
	]}`
	openerDoc := `{"scenarios":[
	 {"id":"X-01","platforms":["any"],"timeout_s":20,"variants":[{"name":"v","params":{"messages":2},"steps":[{"roles":["creator","opener"],"expect":{"creator":"ok","opener":"ok"}}]}]},
	 {"id":"X-02","platforms":["windows"],"timeout_s":10,"variants":[{"name":"v","steps":[{"roles":["creator"],"expect":{"creator":"ok"}}]}]},
	 {"id":"X-03","timeout_s":10,"variants":[{"name":"v","steps":[{"roles":["opener"],"expect":{"opener":"ok"}}]}]}
	]}`
	creator, err := parseCatalogue([]byte(creatorDoc))
	if err != nil {
		t.Fatal(err)
	}
	opener, err := parseCatalogue([]byte(openerDoc))
	if err != nil {
		t.Fatal(err)
	}
	creator.label, opener.label = "go", "c"

	plans, problems := planPair(creator, opener, "linux")
	if len(plans) != 1 || plans[0].id != "X-01" || plans[0].timeoutS != 20 {
		t.Errorf("linux plans = %+v, want X-01 with the larger timeout", plans)
	}
	if len(problems) != 1 || !strings.Contains(problems[0], "X-03") {
		t.Errorf("linux problems = %q, want the X-03 variant mismatch", problems)
	}

	plans, _ = planPair(creator, opener, "windows")
	if len(plans) != 2 || plans[0].id != "X-01" || plans[1].id != "X-02" {
		t.Errorf("windows plans = %+v, want X-01 and X-02", plans)
	}
}
