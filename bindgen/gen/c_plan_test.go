package gen

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	"gosuda.org/hqq/bindgen/idl"
)

// opBody returns the text of the glue function of fn.
func opBody(t *testing.T, src, fn string) string {
	t.Helper()
	re := regexp.MustCompile(`(?s)static hqqrpc_status op_` + regexp.QuoteMeta(fn) + `\(hqqrpc_call \*c\) \{\n.*?\n\}\n`)
	m := re.FindString(src)
	if m == "" {
		t.Fatalf("no glue function for %s", fn)
	}
	return m
}

// TestCCopiesOfInArguments checks step 5 of 4.7.1 and 7.2: a str that relies on its NUL is always copied, every str,
// bytes and array of an @intercept function is copied, and the other in arguments are passed in place.
func TestCCopiesOfInArguments(t *testing.T) {
	cases := []struct {
		idl, source string
		fn          string
		copies      int
	}{
		{"bindgen/testdata/microlib/microlib.idl", "microlib.idl", "ml_hash", 1},                 // str<=255 without a length: copied
		{"bindgen/testdata/microlib/microlib.idl", "microlib.idl", "ml_xor", 0},                  // bytes of a function without @intercept: in place
		{"bindgen/testdata/microlib/microlib.idl", "microlib.idl", "ml_counter_add", 0},          // hooked, but no str, bytes or array argument
		{"bindgen/testdata/sqlite_lowlevel.idl", "sqlite_lowlevel.idl", "sqlite3_open_v2", 2},    // hooked: filename and the nullable zvfs
		{"bindgen/testdata/sqlite_lowlevel.idl", "sqlite_lowlevel.idl", "sqlite3_prepare_v2", 0}, // str with a length: in place
		{"bindgen/testdata/sqlite_lowlevel.idl", "sqlite_lowlevel.idl", "bind_text", 0},
		{"bindgen/gen/testdata/c/edge.idl", "edge.idl", "edge_guard", 1},
		{"bindgen/gen/testdata/c/edge.idl", "edge.idl", "edge_note", 1},
		{"bindgen/gen/testdata/c/edge.idl", "edge.idl", "edge_key", 0},
		{"bindgen/gen/testdata/c/shapes.idl", "shapes.idl", "sh_in_hooked", 4}, // u8[4], bytes, f64[], str
		{"bindgen/gen/testdata/c/shapes.idl", "shapes.idl", "sh_in_var", 0},
	}
	for _, tc := range cases {
		spec := parseSpec(t, tc.idl)
		src := string(genC(t, spec, tc.source)[spec.Service+"_server.c"])
		body := opBody(t, src, tc.fn)
		// the copies come before the response is reserved, and nothing is copied after the lock is taken
		begin := strings.Index(body, "hqqrpc_resp_begin")
		n := strings.Count(body[:begin], "hqqrpc_scratch(s, ")
		if n != tc.copies {
			t.Errorf("%s: %d private copies before the response reservation, want %d", tc.fn, n, tc.copies)
		}
		if strings.Contains(body[begin:], "hqqrpc_scratch(s, ") {
			t.Errorf("%s: a scratch allocation after the response reservation", tc.fn)
		}
	}
}

// TestCResponseBoundMatchesThePlan evaluates the RESP_BOUND expression that the glue passes to hqqrpc_resp_begin for
// the functions whose bound does not depend on a capacity, and compares it with Plan.StaticBound (4.6.3).
func TestCResponseBoundMatchesThePlan(t *testing.T) {
	num := regexp.MustCompile(`^\(uint64_t\)\d+( \+ HQQRPC_[A-Z_]+)*$|^\d+$|^HQQRPC_DETAIL_RESERVE$`)
	checked := 0
	for _, tc := range cGoldenCases {
		spec := parseSpec(t, tc.idl)
		src := string(genC(t, spec, tc.source)[spec.Service+"_server.c"])
		for _, f := range spec.Wire {
			body := opBody(t, src, f.Name)
			m := regexp.MustCompile(`hqqrpc_resp_begin\(c, (.*), &w\)\)`).FindStringSubmatch(body)
			if m == nil {
				t.Fatalf("%s: no hqqrpc_resp_begin", f.Name)
			}
			expr := m[1]
			if !num.MatchString(expr) {
				continue // depends on a capacity
			}
			want, err := f.Plan.StaticBound(f)
			if err != nil {
				t.Fatal(err)
			}
			if got := evalCBound(t, expr); got != want {
				t.Errorf("%s: the glue reserves %q = %d, the plan says %d", f.Name, expr, got, want)
			}
			checked++
		}
	}
	if checked < 20 {
		t.Errorf("only %d bounds were compared", checked)
	}
}

func evalCBound(t *testing.T, expr string) uint64 {
	t.Helper()
	expr = strings.ReplaceAll(expr, "(uint64_t)", "")
	var sum uint64
	for _, tok := range strings.Split(expr, " + ") {
		switch tok {
		case "HQQRPC_DETAIL_RESERVE":
			sum += idl.DetailReserve
		case "HQQRPC_VAR_SLACK":
			sum += idl.VarSlack
		default:
			n, err := strconv.ParseUint(tok, 10, 64)
			if err != nil {
				t.Fatalf("bound term %q in %q", tok, expr)
			}
			sum += n
		}
	}
	return sum
}
