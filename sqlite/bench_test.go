//go:build unix

package sqlite_test

import (
	"bytes"
	"database/sql"
	"fmt"
	"strconv"
	"sync"
	"testing"

	_ "gosuda.org/hqq/sqlite" // registers hqq-sqlite
)

// Benchmarks of the driver (hqq-sqlite) through database/sql.
//
//	go test -run '^$' -bench . -benchtime 2s -count 5 ./sqlite
//
// Benchmarks run with journal_mode=WAL and synchronous=OFF, so that the
// benchmarks measure the request path and not the fsync of the file system.

// benchPragmas is the pragma part of every benchmark DSN.
const benchPragmas = "&pragma.journal_mode=WAL&pragma.synchronous=OFF"

// engineBench runs fn on a fresh server and database. opts is appended to the
// DSN, after the pragmas.
func engineBench(b *testing.B, opts string, fn func(b *testing.B, db *sql.DB)) {
	b.Helper()
	s := startGenServer(b)
	db := openBenchDB(b, s, "bench.db", benchPragmas+opts)
	fn(b, db)
}

// mustB fails the benchmark when err is not nil.
func mustB(b *testing.B, err error, what string) {
	b.Helper()
	if err != nil {
		b.Fatalf("%s: %v", what, err)
	}
}

// seedKV creates table kv(id INTEGER PRIMARY KEY, v TEXT) with rows 1..n, where
// v is the decimal id, in one transaction.
func seedKV(b *testing.B, db *sql.DB, n int) {
	b.Helper()
	_, err := db.Exec("CREATE TABLE kv (id INTEGER PRIMARY KEY, v TEXT)")
	mustB(b, err, "create kv")
	tx, err := db.Begin()
	mustB(b, err, "begin")
	st, err := tx.Prepare("INSERT INTO kv (id, v) VALUES (?, ?)")
	mustB(b, err, "prepare")
	for i := 1; i <= n; i++ {
		_, err := st.Exec(i, strconv.Itoa(i))
		mustB(b, err, "seed insert")
	}
	mustB(b, st.Close(), "close")
	mustB(b, tx.Commit(), "commit")
}

func BenchmarkSelectOne(b *testing.B) {
	engineBench(b, "", func(b *testing.B, db *sql.DB) {
		db.SetMaxOpenConns(1)
		var v int64
		mustB(b, db.QueryRow("SELECT 1").Scan(&v), "warm up")
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			if err := db.QueryRow("SELECT 1").Scan(&v); err != nil {
				b.Fatal(err)
			}
			if v != 1 {
				b.Fatalf("SELECT 1 = %d", v)
			}
		}
	})
}

const pointRows = 10000

func BenchmarkPointSelectPrepared(b *testing.B) {
	engineBench(b, "", func(b *testing.B, db *sql.DB) {
		db.SetMaxOpenConns(1)
		seedKV(b, db, pointRows)
		st, err := db.Prepare("SELECT v FROM kv WHERE id = ?")
		mustB(b, err, "prepare")
		defer st.Close()
		var v string
		b.ReportAllocs()
		b.ResetTimer()
		for i := range b.N {
			id := i%pointRows + 1
			if err := st.QueryRow(id).Scan(&v); err != nil {
				b.Fatal(err)
			}
			if v != strconv.Itoa(id) {
				b.Fatalf("id %d: v = %q", id, v)
			}
		}
	})
}

// insertRows is the number of rows one iteration of BenchmarkInsertTx inserts.
const insertRows = 1000

// BenchmarkInsertTx inserts insertRows rows with one prepared statement inside
// one transaction per iteration.
func BenchmarkInsertTx(b *testing.B) {
	engineBench(b, "", func(b *testing.B, db *sql.DB) {
		db.SetMaxOpenConns(1)
		_, err := db.Exec("CREATE TABLE ins (id INTEGER PRIMARY KEY, v TEXT)")
		mustB(b, err, "create")
		b.ReportAllocs()
		b.ResetTimer()
		next := 1
		for range b.N {
			tx, err := db.Begin()
			mustB(b, err, "begin")
			st, err := tx.Prepare("INSERT INTO ins (id, v) VALUES (?, ?)")
			mustB(b, err, "prepare")
			for range insertRows {
				if _, err := st.Exec(next, "value"); err != nil {
					b.Fatal(err)
				}
				next++
			}
			mustB(b, st.Close(), "close")
			mustB(b, tx.Commit(), "commit")
		}
		b.StopTimer()
		var n int64
		mustB(b, db.QueryRow("SELECT count(*) FROM ins").Scan(&n), "count")
		if n != int64(b.N)*insertRows {
			b.Fatalf("count = %d, want %d", n, int64(b.N)*insertRows)
		}
	})
}

// BenchmarkInsertAutocommit inserts one row per Exec, each its own transaction.
func BenchmarkInsertAutocommit(b *testing.B) {
	engineBench(b, "", func(b *testing.B, db *sql.DB) {
		db.SetMaxOpenConns(1)
		_, err := db.Exec("CREATE TABLE auto (id INTEGER PRIMARY KEY, v TEXT)")
		mustB(b, err, "create")
		b.ReportAllocs()
		b.ResetTimer()
		for i := range b.N {
			res, err := db.Exec("INSERT INTO auto (id, v) VALUES (?, ?)", i+1, "value")
			if err != nil {
				b.Fatal(err)
			}
			if id, _ := res.LastInsertId(); id != int64(i+1) {
				b.Fatalf("LastInsertId = %d, want %d", id, i+1)
			}
		}
		b.StopTimer()
		var n int64
		mustB(b, db.QueryRow("SELECT count(*) FROM auto").Scan(&n), "count")
		if n != int64(b.N) {
			b.Fatalf("count = %d, want %d", n, b.N)
		}
	})
}

const scanRows = 100000

// scanSum is the sum of the ids 1..scanRows.
const scanSum = int64(scanRows) * (scanRows + 1) / 2

// BenchmarkScan reads 100000 rows of (id, text) per iteration with the default
// fetch size and with a large one.
func BenchmarkScan(b *testing.B) {
	for _, fetch := range []int{256, 4096} {
		b.Run(fmt.Sprintf("fetch=%d", fetch), func(b *testing.B) {
			engineBench(b, "&fetch="+strconv.Itoa(fetch), func(b *testing.B, db *sql.DB) {
				db.SetMaxOpenConns(1)
				seedKV(b, db, scanRows)
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					rows, err := db.Query("SELECT id, v FROM kv")
					mustB(b, err, "query")
					var n, sum int64
					for rows.Next() {
						var id int64
						var v string
						if err := rows.Scan(&id, &v); err != nil {
							b.Fatal(err)
						}
						sum += id
						n++
					}
					mustB(b, rows.Err(), "rows")
					mustB(b, rows.Close(), "close")
					if n != scanRows || sum != scanSum {
						b.Fatalf("scanned %d rows with id sum %d, want %d and %d", n, sum, scanRows, scanSum)
					}
				}
			})
		})
	}
}

// BenchmarkBlobRoundTrip stores a 1 MiB blob and reads it back per iteration.
func BenchmarkBlobRoundTrip(b *testing.B) {
	const size = 1 << 20
	want := make([]byte, size)
	for i := range want {
		want[i] = byte(i % 251)
	}
	engineBench(b, "", func(b *testing.B, db *sql.DB) {
		db.SetMaxOpenConns(1)
		_, err := db.Exec("CREATE TABLE blobs (id INTEGER PRIMARY KEY, v BLOB)")
		mustB(b, err, "create")
		b.SetBytes(2 * size)
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			if _, err := db.Exec("INSERT OR REPLACE INTO blobs (id, v) VALUES (1, ?)", want); err != nil {
				b.Fatal(err)
			}
			var got []byte
			if err := db.QueryRow("SELECT v FROM blobs WHERE id = 1").Scan(&got); err != nil {
				b.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				b.Fatalf("blob round trip: got %d bytes, want %d", len(got), len(want))
			}
		}
	})
}

// parallelism is the number of goroutines of BenchmarkPointSelectParallel.
const parallelism = 8

// BenchmarkPointSelectParallel runs prepared point selects from eight goroutines
// over eight connections. One iteration is one select.
func BenchmarkPointSelectParallel(b *testing.B) {
	engineBench(b, "", func(b *testing.B, db *sql.DB) {
		db.SetMaxOpenConns(parallelism)
		db.SetMaxIdleConns(parallelism)
		seedKV(b, db, pointRows)
		st, err := db.Prepare("SELECT v FROM kv WHERE id = ?")
		mustB(b, err, "prepare")
		defer st.Close()
		// Open every connection before the timer starts.
		var warm sync.WaitGroup
		for w := range parallelism {
			warm.Add(1)
			go func() {
				defer warm.Done()
				var v string
				_ = st.QueryRow(w + 1).Scan(&v)
			}()
		}
		warm.Wait()
		b.ReportAllocs()
		b.ResetTimer()
		errc := make(chan error, parallelism)
		var wg sync.WaitGroup
		for w := range parallelism {
			n := b.N / parallelism
			if w < b.N%parallelism {
				n++
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				var v string
				for i := range n {
					id := (w*7919+i)%pointRows + 1
					if err := st.QueryRow(id).Scan(&v); err != nil {
						errc <- err
						return
					}
					if v != strconv.Itoa(id) {
						errc <- fmt.Errorf("id %d: v = %q", id, v)
						return
					}
				}
			}()
		}
		wg.Wait()
		close(errc)
		for err := range errc {
			b.Fatal(err)
		}
	})
}
