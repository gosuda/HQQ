package zstd_test

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"gosuda.org/hqq/bindgen/examples/zstd"
	"gosuda.org/hqq/bindgen/rt"
)

// newPool returns a pool of sessions on the shared server that closes its sessions when the test ends.
func newPool(t *testing.T, size int) *rt.Pool[*zstd.Client] {
	t.Helper()
	requireServer(t)
	p := rt.NewPool(size,
		func(ctx context.Context) (*zstd.Client, error) {
			return zstd.Dial(ctx, shared.Listener(), zstd.DefaultOptions())
		},
		func(c *zstd.Client) error { return c.Close() })
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Errorf("close pool: %v", err)
		}
	})
	return p
}

// TestPoolGoroutinesShareSessionsWithoutInterference runs 24 goroutines over a pool of 6 sessions.
// Every goroutine borrows a session for one round trip, with sizes on both sides of the one-frame
// and CONT-frame boundaries, and checks its own data: a response delivered to the wrong caller, a
// frame mixed up between sessions or a shared buffer would show as a differing payload. The race
// detector watches the hand-offs of sessions between goroutines.
func TestPoolGoroutinesShareSessionsWithoutInterference(t *testing.T) {
	const goroutines, rounds = 24, 20
	pool := newPool(t, 6)
	sizes := []int{100, 5000, 65_000, 70_000, 300_000}
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for g := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			for r := range rounds {
				size := sizes[(g+r)%len(sizes)]
				level := int32(1 + (g+r)%5)
				src := textData(size, uint64(g*1000+r))
				c, err := pool.Get(ctx)
				if err != nil {
					errs <- fmt.Errorf("goroutine %d: get: %w", g, err)
					return
				}
				frame, err := c.Compress(make([]byte, zstd.CompressBound(uint64(size))), src, level)
				var out []byte
				if err == nil {
					out, err = c.Decompress(make([]byte, size), frame)
				}
				if err != nil {
					pool.Discard(c)
					errs <- fmt.Errorf("goroutine %d round %d (%d bytes, level %d): %w", g, r, size, level, err)
					return
				}
				pool.Put(c)
				if !bytes.Equal(out, src) {
					errs <- fmt.Errorf("goroutine %d round %d: %d bytes came back different", g, r, size)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// TestPipelinedClientsWithOwnContexts gives each of 8 goroutines its own session and its own
// contexts, and keeps 6 calls in flight per session: CompressCCtx futures are issued first and waited
// for in order, then the same for decompression. Contexts are per session, so none is shared.
func TestPipelinedClientsWithOwnContexts(t *testing.T) {
	requireServer(t)
	const goroutines, depth = 8, 6
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for g := range goroutines {
		c := newClient(t)
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- pipelinedRound(c, g, depth)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
}

func pipelinedRound(c *zstd.Client, g, depth int) error {
	cctx, err := c.CreateCCtx()
	if err != nil {
		return err
	}
	dctx, err := c.CreateDCtx()
	if err != nil {
		return err
	}
	srcs := make([][]byte, depth)
	dsts := make([][]byte, depth)
	futs := make([]zstd.CompressCCtxFuture, depth)
	for i := range depth {
		srcs[i] = textData(20_000+i*7000, uint64(g*100+i))
		dsts[i] = make([]byte, zstd.CompressBound(uint64(len(srcs[i]))))
		if futs[i], err = c.CompressCCtxAsync(cctx, dsts[i], srcs[i], 3); err != nil {
			return fmt.Errorf("session %d: issue %d: %w", g, i, err)
		}
	}
	frames := make([][]byte, depth)
	for i := range depth {
		if frames[i], err = futs[i].Wait(); err != nil {
			return fmt.Errorf("session %d: compress %d: %w", g, i, err)
		}
	}
	outs := make([][]byte, depth)
	dfuts := make([]zstd.DecompressDCtxFuture, depth)
	for i := range depth {
		outs[i] = make([]byte, len(srcs[i]))
		if dfuts[i], err = c.DecompressDCtxAsync(dctx, outs[i], frames[i]); err != nil {
			return fmt.Errorf("session %d: issue decompress %d: %w", g, i, err)
		}
	}
	for i := range depth {
		got, err := dfuts[i].Wait()
		if err != nil || !bytes.Equal(got, srcs[i]) {
			return fmt.Errorf("session %d: decompress %d: %d bytes, %v", g, i, len(got), err)
		}
	}
	if err := c.FreeCCtx(cctx); err != nil {
		return err
	}
	return c.FreeDCtx(dctx)
}

// TestWaitInAnyOrder waits for pipelined futures in reverse order, which makes the runtime buffer the
// earlier responses, and checks that every future still gets its own result.
func TestWaitInAnyOrder(t *testing.T) {
	c := newClient(t)
	const n = 5
	srcs := make([][]byte, n)
	dsts := make([][]byte, n)
	futs := make([]zstd.CompressFuture, n)
	for i := range n {
		srcs[i] = textData(3000+i*1111, uint64(i))
		dsts[i] = make([]byte, zstd.CompressBound(uint64(len(srcs[i]))))
		var err error
		if futs[i], err = c.CompressAsync(dsts[i], srcs[i], 1); err != nil {
			t.Fatal(err)
		}
	}
	for i := n - 1; i >= 0; i-- {
		frame, err := futs[i].Wait()
		if err != nil {
			t.Fatalf("future %d: %v", i, err)
		}
		out, err := c.Decompress(make([]byte, len(srcs[i])), frame)
		if err != nil || !bytes.Equal(out, srcs[i]) {
			t.Fatalf("future %d decoded to %d bytes, %v", i, len(out), err)
		}
	}
}
