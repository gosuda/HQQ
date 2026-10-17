//go:build unix

package sqlite3_test

import (
	"fmt"
	"testing"

	"gosuda.org/hqq/bindgen/examples/sqlite3"
	"gosuda.org/hqq/bindgen/examples/sqlite3/sqlite3srv"
)

// The benchmarks measure the cost of the calls that a database/sql driver makes, over shared memory, on
// a database file with the syncs off (the cost of the disk is not what they are about). They are not run
// by `go test`. To prove that they run (one iteration each):
//
//	go test -run '^$' -bench . -benchtime 1x ./bindgen/examples/sqlite3/
//
// Numbers are for the owner of the machine to take; none is recorded in this repository.

func benchSession(b *testing.B) (*sqlite3.Client, sqlite3.Db) {
	b.Helper()
	srv := sqlite3srv.Start(b, sqlite3srv.Config{})
	c := srv.Dial()
	db := open(b, c, "bench.db")
	run(b, c, db, "PRAGMA journal_mode = WAL")
	run(b, c, db, "PRAGMA synchronous = OFF")
	return c, db
}

// BenchmarkRoundTrip is the cheapest call: sqlite3_libversion_number, which does no work. It is the
// cost of the transport and the codec of one synchronous call.
func BenchmarkRoundTrip(b *testing.B) {
	c, _ := benchSession(b)
	b.ResetTimer()
	for range b.N {
		if _, err := c.Sqlite3LibversionNumber(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPipelinedCalls issues 32 calls and then waits for them: the cost per call when the client
// does not wait for each response.
func BenchmarkPipelinedCalls(b *testing.B) {
	c, db := benchSession(b)
	futs := make([]sqlite3.Sqlite3GetAutocommitFuture, 0, 32)
	b.ResetTimer()
	for range b.N {
		futs = futs[:0]
		for range 32 {
			f, err := c.Sqlite3GetAutocommitAsync(db)
			if err != nil {
				b.Fatal(err)
			}
			futs = append(futs, f)
		}
		for _, f := range futs {
			if _, err := f.Wait(); err != nil {
				b.Fatal(err)
			}
		}
	}
	b.ReportMetric(float64(32), "calls/op")
}

// BenchmarkInsertEightParams measures one INSERT of eight parameters, by three paths: exec_stmt (one call), bind_values
// and step (two calls), and a bind per parameter with the binds and the step in flight together.
func BenchmarkInsertEightParams(b *testing.B) {
	setup := func(b *testing.B) (*sqlite3.Client, sqlite3.Db, sqlite3.Stmt) {
		c, db := benchSession(b)
		run(b, c, db, "CREATE TABLE i (id INTEGER PRIMARY KEY, a INTEGER, b INTEGER, c REAL, d TEXT, e TEXT, f BLOB, g, h)")
		return c, db, prep(b, c, db, "INSERT INTO i (a, b, c, d, e, f, g, h) VALUES (?, ?, ?, ?, ?, ?, ?, ?)")
	}
	args := func(i int) []any {
		return []any{i, int64(i) * 3, 1.5, "alpha", "a somewhat longer text value of about forty b", pattern(32, 1), nil, zeroblob(4)}
	}
	b.Run("exec_stmt", func(b *testing.B) {
		c, _, st := setup(b)
		b.ResetTimer()
		for i := range b.N {
			k, w, d := pack(args(i)...)
			if _, _, err := c.ExecStmt(st, k, w, d); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("bind_values+step", func(b *testing.B) {
		c, _, st := setup(b)
		b.ResetTimer()
		for i := range b.N {
			k, w, d := pack(args(i)...)
			if err := c.BindValues(st, k, w, d); err != nil {
				b.Fatal(err)
			}
			if _, err := c.Sqlite3Step(st); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("pipelined binds+step", func(b *testing.B) {
		c, _, st := setup(b)
		binds := make([]sqlite3.Sqlite3BindInt64Future, 0, 8)
		b.ResetTimer()
		for i := range b.N {
			reset, err := c.Sqlite3ResetAsync(st)
			if err != nil {
				b.Fatal(err)
			}
			binds = binds[:0]
			bi := func(idx int32, v int64) {
				f, err := c.Sqlite3BindInt64Async(st, idx, v)
				if err != nil {
					b.Fatal(err)
				}
				binds = append(binds, f)
			}
			bi(1, int64(i))
			bi(2, int64(i)*3)
			// The remaining parameters have other types, so they go through the typed binds.
			fd, _ := c.Sqlite3BindDoubleAsync(st, 3, 1.5)
			ft, _ := c.BindTextAsync(st, 4, []byte("alpha"))
			fe, _ := c.BindTextAsync(st, 5, []byte("a somewhat longer text value of about forty b"))
			ff, _ := c.BindBlobAsync(st, 6, pattern(32, 1))
			fg, _ := c.Sqlite3BindNullAsync(st, 7)
			fh, _ := c.Sqlite3BindZeroblob64Async(st, 8, 4)
			step, err := c.Sqlite3StepAsync(st)
			if err != nil {
				b.Fatal(err)
			}
			if err := reset.Wait(); err != nil {
				b.Fatal(err)
			}
			for _, f := range binds {
				if err := f.Wait(); err != nil {
					b.Fatal(err)
				}
			}
			for _, w := range []func() error{fd.Wait, ft.Wait, fe.Wait, ff.Wait, fg.Wait, fh.Wait} {
				if err := w(); err != nil {
					b.Fatal(err)
				}
			}
			if _, err := step.Wait(); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkPointQuery is a query that returns one row: bind_values and fetch_rows, with the statement
// prepared once.
func BenchmarkPointQuery(b *testing.B) {
	c, db := benchSession(b)
	seq(b, c, db, 1000)
	st := prep(b, c, db, "SELECT id, name, score FROM t WHERE id = ?")
	b.ResetTimer()
	for i := range b.N {
		k, w, d := pack(i%1000 + 1)
		if err := c.BindValues(st, k, w, d); err != nil {
			b.Fatal(err)
		}
		r, err := c.FetchRows(st, 0)
		if err != nil || len(r.Cells) != 3 {
			b.Fatalf("fetch: %d cells, %v", len(r.Cells), err)
		}
	}
}

// BenchmarkPrepareFinalize is the cost of preparing a statement and freeing it.
func BenchmarkPrepareFinalize(b *testing.B) {
	c, db := benchSession(b)
	seq(b, c, db, 10)
	b.ResetTimer()
	for range b.N {
		st, err := c.PrepareOne(db, "SELECT id, name, score FROM t WHERE id = ?")
		if err != nil {
			b.Fatal(err)
		}
		if err := c.Sqlite3Finalize(st); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkScan reads a table of 20,000 rows of three columns in batches of 256, without and with the
// prefetch of the next batch (issued after the previous one was decoded).
func BenchmarkScan(b *testing.B) {
	const n = 20000
	for _, prefetch := range []bool{false, true} {
		b.Run(fmt.Sprintf("prefetch=%v", prefetch), func(b *testing.B) {
			c, db := benchSession(b)
			seq(b, c, db, n)
			st := prep(b, c, db, "SELECT id, name, score FROM t")
			b.ResetTimer()
			for range b.N {
				rows := 0
				fut, err := c.FetchRowsAsync(st, 256)
				if err != nil {
					b.Fatal(err)
				}
				for {
					r, err := fut.Wait()
					if err != nil {
						b.Fatal(err)
					}
					more := r.Rc == sqlite3.SQLITE_ROW
					if more && prefetch {
						if fut, err = c.FetchRowsAsync(st, 256); err != nil {
							b.Fatal(err)
						}
					}
					rows += len(r.Cells) / int(r.Ncols)
					if !more {
						break
					}
					if !prefetch {
						if fut, err = c.FetchRowsAsync(st, 256); err != nil {
							b.Fatal(err)
						}
					}
				}
				if rows != n {
					b.Fatalf("%d rows, want %d", rows, n)
				}
				if err := c.Sqlite3Reset(st); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(n), "rows/op")
		})
	}
}

// BenchmarkLargeBlob inserts and reads back a 1 MiB blob: the multi-frame request, and the chunked read.
func BenchmarkLargeBlob(b *testing.B) {
	c, db := benchSession(b)
	run(b, c, db, "CREATE TABLE lb (b BLOB)")
	blob := pattern(1<<20, 1)
	ins := prep(b, c, db, "INSERT INTO lb VALUES (?)")
	k, w, d := pack(blob)
	b.SetBytes(int64(len(blob)))
	b.ResetTimer()
	for range b.N {
		if _, _, err := c.ExecStmt(ins, k, w, d); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	sel := prep(b, c, db, "SELECT b FROM lb LIMIT 1")
	buf := make([]byte, 256<<10)
	b.StartTimer()
	for range b.N {
		if err := c.Sqlite3Reset(sel); err != nil {
			b.Fatal(err)
		}
		r, err := c.FetchRows(sel, 1)
		if err != nil || len(r.Cells) != 1 || r.Cells[0].Tag != sqlite3.ValueBigBlob {
			b.Fatalf("fetch: %+v, %v", r.Cells, err)
		}
		for off := uint64(0); off < r.Cells[0].BigBlob; {
			got, err := c.ColumnChunk(sel, 0, off, buf)
			if err != nil {
				b.Fatal(err)
			}
			off += uint64(len(got))
		}
	}
}
