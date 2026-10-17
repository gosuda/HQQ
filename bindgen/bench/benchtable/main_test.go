package main

import (
	"bytes"
	"strings"
	"testing"
)

const sample = `hqq-wait: spin
goos: darwin
pkg: gosuda.org/hqq/bindgen/bench
BenchmarkWork/iters=0/cgo-8         	 1000	        30.0 ns/op	 33000000 calls/s	       0 B/op	       0 allocs/op
BenchmarkWork/iters=0/cgo-8         	 1000	        10.0 ns/op	 99000000 calls/s	       0 B/op	       0 allocs/op
BenchmarkWork/iters=0/cgo-8         	 1000	        20.0 ns/op	 50000000 calls/s	       0 B/op	       0 allocs/op
BenchmarkWork/iters=0/hqq-pipe-4-8  	 1000	       150.0 ns/op	  6600000 calls/s
BenchmarkWork/iters=64/cgo-8        	 1000	       120.0 ns/op
BenchmarkWork/iters=64/hqq-sync-8   	 1000	       1234.0 ns/op
BenchmarkXor/64B/cgo-8              	 1000	       500.0 ns/op	 128.00 MB/s
PASS
ok  	gosuda.org/hqq/bindgen/bench	1.0s
`

func runSample(t *testing.T, args ...string) string {
	t.Helper()
	var out, errOut bytes.Buffer
	if err := run(args, strings.NewReader(sample), &out, &errOut); err != nil {
		t.Fatalf("run(%v): %v", args, err)
	}
	return out.String()
}

func TestTableShowsMedianPerCellAndMissingCells(t *testing.T) {
	got := runSample(t, "-match", "^Work/")
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("want a header and two rows, got:\n%s", got)
	}
	header := strings.Fields(lines[0])
	if strings.Join(header, " ") != "ns/op cgo hqq-pipe-4 hqq-sync" {
		t.Errorf("header = %q: the GOMAXPROCS suffix must go and columns keep first-appearance order", header)
	}
	row0 := strings.Fields(lines[1])
	if strings.Join(row0, " ") != "Work/iters=0 20.0 150 -" {
		t.Errorf("row 0 = %q: the cgo cell is the median of 30, 10 and 20; hqq-sync has no result there", row0)
	}
	row1 := strings.Fields(lines[2])
	if strings.Join(row1, " ") != "Work/iters=64 120 - 1234" {
		t.Errorf("row 1 = %q", row1)
	}
}

func TestMetricSelectsAnotherUnit(t *testing.T) {
	got := runSample(t, "-metric", "calls/s", "-match", "^Work/iters=0/")
	if !strings.Contains(got, "calls/s") || !strings.Contains(got, "50000000") || !strings.Contains(got, "6600000") {
		t.Errorf("calls/s table:\n%s", got)
	}
}

func TestEmptyInputIsAnError(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run(nil, strings.NewReader("PASS\n"), &out, &errOut); err == nil {
		t.Fatal("no benchmark lines: want an error, got nil")
	}
}

func TestProcsSuffixStaysWhenResultsDiffer(t *testing.T) {
	in := "BenchmarkA/x-8 \t 10 \t 1.0 ns/op\nBenchmarkA/y-4 \t 10 \t 2.0 ns/op\n"
	var out, errOut bytes.Buffer
	if err := run(nil, strings.NewReader(in), &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "x-8") || !strings.Contains(out.String(), "y-4") {
		t.Errorf("suffixes must stay when they differ:\n%s", out.String())
	}
}
