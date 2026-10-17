//go:build cgo && !race

package bench

import (
	"testing"

	"gosuda.org/hqq/bindgen/bench/microbench"
)

// TestHotPathAllocs: the in-order path of the generated client, small payloads, CallTimeout == 0,
// allocates nothing once the session is warm (BINDGEN.md 1.3 item 4, 11.2). The race detector adds
// allocations of its own, so the test is excluded from -race builds. The benchmarks report
// allocs/op as well.
func TestHotPathAllocs(t *testing.T) {
	cl := client(t, clientCfg{})
	syncAfter(t, cl)
	src := xorSrc(64)
	dst := make([]byte, len(src))
	ctr, err := cl.CounterNew(0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := cl.CounterFree(ctr); err != nil {
			t.Errorf("CounterFree: %v", err)
		}
	}()

	cases := []struct {
		name string
		op   func() error
	}{
		{"Add", func() error { _, err := cl.Add(20, 22); return err }},
		{"Work", func() error { _, err := cl.Work(16); return err }},
		{"CounterAdd", func() error { _, err := cl.CounterAdd(ctr, 1); return err }},
		{"Xor-64B", func() error { _, err := cl.Xor(src, 0x5A, dst); return err }},
		{"Add-pipelined-8", func() error {
			var futs [8]microbench.AddFuture
			for i := range futs {
				f, err := cl.AddAsync(int32(i), 1)
				if err != nil {
					return err
				}
				futs[i] = f
			}
			for i := range futs {
				if _, err := futs[i].Wait(); err != nil {
					return err
				}
			}
			return nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var opErr error
			if allocs := testing.AllocsPerRun(1000, func() {
				if err := tc.op(); err != nil && opErr == nil {
					opErr = err
				}
			}); allocs != 0 {
				t.Errorf("%s allocates %v times per call, want 0", tc.name, allocs)
			}
			if opErr != nil {
				t.Errorf("%s: %v", tc.name, opErr)
			}
		})
	}
}
