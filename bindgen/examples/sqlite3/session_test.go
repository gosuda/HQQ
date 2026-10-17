//go:build unix

package sqlite3_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	hqq "gosuda.org/hqq"
	"gosuda.org/hqq/bindgen/examples/sqlite3"
	"gosuda.org/hqq/bindgen/examples/sqlite3/sqlite3srv"
	"gosuda.org/hqq/bindgen/rt"
)

// Liveness timing of the tests that wait for a dead peer: the server flags match the client options.
// The other tests keep the defaults (2 s), so that a loaded machine cannot fake a peer death.
var (
	fastLink       = hqq.LinkOptions{HeartbeatInterval: 50 * time.Millisecond, PeerTimeout: 500 * time.Millisecond}
	fastServerArgs = []string{"--heartbeat-ms", "50", "--peer-timeout-ms", "500"}
)

func withFastLink(o *rt.Options) { o.Link = fastLink }

// observer opens the database in a new session and checks what a crashed or closed session left behind:
// the write lock is free (BEGIN IMMEDIATE succeeds at once, with a busy timeout of zero) and the table
// holds exactly want rows.
func observer(t *testing.T, srv *sqlite3srv.Server, name string, want int64) {
	t.Helper()
	c := srv.Dial()
	db := open(t, c, name)
	if err := c.Sqlite3BusyTimeout(db, 0); err != nil {
		t.Fatal(err)
	}
	run(t, c, db, "BEGIN IMMEDIATE") // fails with SQLITE_BUSY while the dead session still holds the lock
	if n := one(t, c, db, "SELECT count(*) FROM k"); n != want {
		t.Fatalf("%v rows, want %d (an uncommitted row survived, or a committed one was lost)", n, want)
	}
	run(t, c, db, "ROLLBACK")
}

// writeTx begins a transaction on db and writes a row, without committing. It also leaves a statement
// in the middle of its result, which holds a read lock of its own.
func writeTx(t *testing.T, c *sqlite3.Client, db sqlite3.Db) (reading sqlite3.Stmt) {
	t.Helper()
	run(t, c, db, "BEGIN IMMEDIATE")
	run(t, c, db, "INSERT INTO k VALUES (100)")
	reading = prep(t, c, db, "SELECT v FROM k")
	if b := fetch(t, c, reading, 1); b.rc != sqlite3.SQLITE_ROW || len(b.rows) != 1 {
		t.Fatalf("the reading statement: rc %d, %d rows", b.rc, len(b.rows))
	}
	return reading
}

func seedK(t *testing.T, c *sqlite3.Client, db sqlite3.Db) {
	t.Helper()
	fast(t, c, db)
	run(t, c, db, "CREATE TABLE k (v INTEGER)")
	run(t, c, db, "INSERT INTO k VALUES (1)")
}

// TestClientKilledMidTransaction ends a session without the CLOSE exchange, as a crashed client does, in the
// middle of a write transaction with a statement that is half read. The server notices that the link is
// gone, finalizes the statements, rolls the transaction back and closes the connection: another session
// can write at once, and the uncommitted row is not there. It catches a teardown that skips a
// connection, and a session that blocks the lock until its handles are destroyed.
func TestClientKilledMidTransaction(t *testing.T) {
	srv := sqlite3srv.Start(t, sqlite3srv.Config{Args: fastServerArgs})
	c := srv.Dial(withFastLink)
	db := open(t, c, "crash.db")
	seedK(t, c, db)
	writeTx(t, c, db)

	sqlite3srv.KillClient(c)
	if !srv.Log.WaitCount(sqlite3srv.LineSessionEnded, 1, sqlite3srv.FailBound) {
		t.Fatalf("the server did not end the session of the killed client:\n%s", srv.Log.String())
	}
	observer(t, srv, "crash.db", 1)
}

// TestHelperCrashClient is the client process of TestClientProcessKilledMidTransaction: it begins a
// transaction, writes, reports, and waits to be killed. It runs only when SQLITE3GEN_HELPER is set.
func TestHelperCrashClient(t *testing.T) {
	spec := os.Getenv("SQLITE3GEN_HELPER")
	if spec == "" {
		t.Skip("helper process of TestClientProcessKilledMidTransaction")
	}
	listener, _, _ := strings.Cut(spec, "|")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := sqlite3.Dial(ctx, listener, rt.Options{Link: fastLink})
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper dial:", err)
		os.Exit(2)
	}
	db, err := c.Sqlite3OpenV2("crash.db", sqlite3.SQLITE_OPEN_READWRITE, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper open:", err)
		os.Exit(2)
	}
	writeTx(t, c, db)
	fmt.Println("in-tx")
	time.Sleep(time.Hour) // the parent kills this process
}

// TestClientProcessKilledMidTransaction is TestClientKilledMidTransaction with a real process: the client
// is killed with SIGKILL, so nothing is closed or detached, and the server finds out from the heartbeat. It
// follows TestClientCrashRollsBack of the hand-written server (sqlite/e2e/e2e_test.go).
func TestClientProcessKilledMidTransaction(t *testing.T) {
	srv := sqlite3srv.Start(t, sqlite3srv.Config{Args: fastServerArgs})
	c := srv.Dial()
	db := open(t, c, "crash.db")
	seedK(t, c, db)

	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperCrashClient$", "-test.count=1")
	cmd.Env = append(os.Environ(), "SQLITE3GEN_HELPER="+srv.Listener()+"|crash.db")
	cmd.Stderr = os.Stderr
	var out bytes.Buffer
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	reported := make(chan struct{})
	go func() {
		buf := make([]byte, 64)
		for {
			n, err := stdout.Read(buf)
			out.Write(buf[:n])
			if bytes.Contains(out.Bytes(), []byte("in-tx")) {
				close(reported)
				return
			}
			if err != nil {
				return
			}
		}
	}()
	select {
	case <-reported:
	case <-time.After(sqlite3srv.FailBound):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("the helper did not start its transaction: %s", out.String())
	}
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()

	// Two sessions ended in the end (the helper's and this test's own, later); the helper's is the first.
	if !srv.Log.WaitCount(sqlite3srv.LineSessionEnded, 1, sqlite3srv.FailBound) {
		t.Fatalf("the server did not end the session of the killed process:\n%s", srv.Log.String())
	}
	observer(t, srv, "crash.db", 1)
}

// TestCloseTearsDownBeforeTheAck closes a session in an orderly way while it has an open write
// transaction, a half-read statement and an unfinalized statement. The server acknowledges the close
// after the teardown (BINDGEN.md 5.2), so when Close returns another session can take the write lock at
// once and sees none of the uncommitted rows.
func TestCloseTearsDownBeforeTheAck(t *testing.T) {
	srv := sqlite3srv.Start(t, sqlite3srv.Config{})
	c := srv.Dial()
	db := open(t, c, "close.db")
	seedK(t, c, db)
	writeTx(t, c, db)
	prep(t, c, db, "SELECT 1") // never finalized
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	observer(t, srv, "close.db", 1) // the proof that the teardown came first: the write lock is free as Close returns
	// The log line is only a second witness, and stderr reaches the test through a pipe, so it may arrive
	// a moment after Close returned: wait for it.
	if !srv.Log.WaitCount(sqlite3srv.LineSessionEnded, 1, sqlite3srv.FailBound) {
		t.Fatalf("the session end was not logged:\n%s", srv.Log.String())
	}
}

// TestCloseConnectionWithLiveStatements closes a connection that has statements: the server finalizes them first
// (they are the children of the connection), so sqlite3_close does not answer SQLITE_BUSY. The statements
// are gone afterwards, and an open transaction was rolled back by the close.
func TestCloseConnectionWithLiveStatements(t *testing.T) {
	srv := sqlite3srv.Start(t, sqlite3srv.Config{})
	c := srv.Dial()
	db := open(t, c, "live.db")
	seedK(t, c, db)
	reading := writeTx(t, c, db)
	idle := prep(t, c, db, "SELECT 2")
	if err := c.Sqlite3Close(db); err != nil {
		t.Fatalf("close with live statements: %v", err)
	}
	for _, st := range []sqlite3.Stmt{reading, idle} {
		if _, err := c.Sqlite3Step(st); !errors.Is(err, rt.ErrBadHandle) {
			t.Fatalf("step on a statement of the closed connection: %v, want ErrBadHandle", err)
		}
	}
	// The session goes on with a new connection, and the rolled-back row is not there.
	db2 := open(t, c, "live.db")
	if n := one(t, c, db2, "SELECT count(*) FROM k"); n != int64(1) {
		t.Fatalf("%v rows after the close, want 1", n)
	}
}

// TestServerKilled kills the server in the middle of a transaction. The client's next call fails within the
// peer timeout with an unknown outcome and the session is broken for good. A new server on the same
// directory finds the database as it was before the transaction (SQLite recovers its journal), and
// serves new sessions.
func TestServerKilled(t *testing.T) {
	srv := sqlite3srv.Start(t, sqlite3srv.Config{Args: fastServerArgs})
	c := srv.Dial(withFastLink)
	db := open(t, c, "kill.db")
	seedK(t, c, db)
	writeTx(t, c, db)

	srv.KillServer()
	start := time.Now()
	_, err := c.Sqlite3Changes64(db)
	if err == nil {
		t.Fatal("a call to a killed server succeeded")
	}
	if !errors.Is(err, hqq.ErrPeerGone) && !errors.Is(err, hqq.ErrPeerClosed) || !errors.Is(err, rt.ErrOutcomeUnknown) {
		t.Fatalf("error %v, want ErrPeerGone or ErrPeerClosed with an unknown outcome", err)
	}
	if d := time.Since(start); d > 6*time.Second {
		t.Fatalf("the failure took %v to surface", d)
	}
	if _, err := c.Sqlite3Changes64(db); !errors.Is(err, rt.ErrSessionBroken) {
		t.Fatalf("call on the broken session: %v, want ErrSessionBroken", err)
	}

	srv2 := sqlite3srv.Start(t, sqlite3srv.Config{AllowDir: srv.AllowDir})
	observer(t, srv2, "kill.db", 1)
}

// TestGracefulShutdown stops the server with a session that has an open transaction. The server exits with
// status 0, says so on stderr, ends the session through the teardown, and the next call of the client
// fails with ErrPeerClosed or the shutting-down status, as an unknown outcome.
func TestGracefulShutdown(t *testing.T) {
	srv := sqlite3srv.Start(t, sqlite3srv.Config{Args: fastServerArgs})
	c := srv.Dial(withFastLink)
	db := open(t, c, "stop.db")
	seedK(t, c, db)
	writeTx(t, c, db)

	ctx, cancel := context.WithTimeout(context.Background(), sqlite3srv.FailBound)
	defer cancel()
	if err := srv.Stop(ctx); err != nil {
		t.Fatalf("graceful exit: %v\n%s", err, srv.Log.String())
	}
	if !strings.Contains(srv.Log.String(), "shut down") {
		t.Fatalf("no shutdown message:\n%s", srv.Log.String())
	}
	if n := srv.Log.Count(sqlite3srv.LineSessionEnded); n != 1 {
		t.Fatalf("%d sessions ended, want 1:\n%s", n, srv.Log.String())
	}
	// The call is refused or fails: the link is closed (the session ended before the request was
	// committed, or the shutdown frame reached the call).
	_, err := c.Sqlite3Changes64(db)
	if err == nil || !errors.Is(err, hqq.ErrPeerClosed) && !errors.Is(err, hqq.ErrPeerGone) && !errors.Is(err, rt.ErrShuttingDown) {
		t.Fatalf("call after the shutdown: %v, want ErrPeerClosed, ErrPeerGone or ErrShuttingDown", err)
	}
	srv2 := sqlite3srv.Start(t, sqlite3srv.Config{AllowDir: srv.AllowDir})
	observer(t, srv2, "stop.db", 1)
}

// TestSessionLimit checks --max-sessions: a session beyond the limit is refused at Dial with the OPEN status
// BUSY (an *rt.OpenError that matches rt.ErrLimit), and a slot is free again after a session ended. It
// follows TestSessionLimit of the hand-written server, which refuses at Open instead.
func TestSessionLimit(t *testing.T) {
	srv := sqlite3srv.Start(t, sqlite3srv.Config{Args: []string{"--max-sessions", "2"}})
	c1 := srv.Dial()
	c2 := srv.Dial()
	_ = c2

	dial := func() (*sqlite3.Client, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return sqlite3.Dial(ctx, srv.Listener(), rt.Options{})
	}
	c3, err := dial()
	if err == nil {
		c3.Kill()
		t.Fatal("a third session was accepted with --max-sessions 2")
	}
	var oe *rt.OpenError
	if !errors.As(err, &oe) || oe.Code != 3 || !errors.Is(err, rt.ErrLimit) {
		t.Fatalf("third session: %v, want an OpenError with code 3 matching ErrLimit", err)
	}
	if err := c1.Close(); err != nil {
		t.Fatal(err)
	}
	// The slot is released after the CLOSE_ACK and after the link is closed (BINDGEN.md 5.2, steps 4 to 6),
	// so a dial right after Close may still see BUSY: retry while the refusal is the session limit.
	var c4 *sqlite3.Client
	deadline := time.Now().Add(sqlite3srv.FailBound)
	for {
		c4, err = dial()
		if err == nil {
			break
		}
		if !errors.As(err, &oe) || oe.Code != 3 || time.Now().After(deadline) {
			t.Fatalf("a session after a slot was freed: %v (the slot was not released within %v)", err, sqlite3srv.FailBound)
		}
		time.Sleep(2 * time.Millisecond) // the slot is released by another process
	}
	t.Cleanup(func() { _ = c4.Close() })
	if n, err := c4.Sqlite3LibversionNumber(); err != nil || n == 0 {
		t.Fatalf("the new session: %d, %v", n, err)
	}
}

// TestParallelSessions runs 16 sessions that write to one database and 4 that read it, all at the same time.
// Every write succeeds (the busy handler waits for the lock: the sessions really run SQLite at the same
// time), and the table ends with every row exactly once. It catches shared state between sessions (the
// message of a failed call, a handle table) and a library entry that is not thread-safe.
func TestParallelSessions(t *testing.T) {
	srv := sqlite3srv.Start(t, sqlite3srv.Config{})
	setup := srv.Dial()
	sdb := open(t, setup, "par.db")
	run(t, setup, sdb, "PRAGMA journal_mode = WAL")
	fast(t, setup, sdb)
	run(t, setup, sdb, "CREATE TABLE p (id INTEGER PRIMARY KEY, who INTEGER, v TEXT)")

	const writers, readers, perWriter = 16, 4, 40
	var wg sync.WaitGroup
	errs := make(chan error, writers+readers)
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- func() error {
				c, err := dialClient(srv)
				if err != nil {
					return err
				}
				defer c.Close()
				db, err := c.Sqlite3OpenV2("par.db", sqlite3.SQLITE_OPEN_READWRITE, nil)
				if err != nil {
					return err
				}
				if err := c.Sqlite3BusyTimeout(db, 30000); err != nil {
					return err
				}
				st, err := c.PrepareOne(db, "INSERT INTO p (id, who, v) VALUES (?, ?, ?)")
				if err != nil {
					return err
				}
				for i := range perWriter {
					k, wd, d := pack(w*1000+i, w, fmt.Sprintf("writer %d row %d", w, i))
					if _, _, err := c.ExecStmt(st, k, wd, d); err != nil {
						return fmt.Errorf("writer %d row %d: %w", w, i, err)
					}
				}
				return nil
			}()
		}()
	}
	for r := range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- func() error {
				c, err := dialClient(srv)
				if err != nil {
					return err
				}
				defer c.Close()
				db, err := c.Sqlite3OpenV2("par.db", sqlite3.SQLITE_OPEN_READONLY, nil)
				if err != nil {
					return err
				}
				if err := c.Sqlite3BusyTimeout(db, 30000); err != nil {
					return err
				}
				st, err := c.PrepareOne(db, "SELECT id, who, v FROM p ORDER BY id")
				if err != nil {
					return err
				}
				for range 20 {
					for {
						rows, err := c.FetchRows(st, 64)
						if err != nil {
							return err
						}
						if rows.Rc == sqlite3.SQLITE_DONE {
							break
						}
						if rows.Rc != sqlite3.SQLITE_ROW {
							return fmt.Errorf("reader %d: rc %d %s", r, rows.Rc, rows.Errmsg)
						}
					}
					if err := c.Sqlite3Reset(st); err != nil {
						return err
					}
				}
				return nil
			}()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	rows := queryAll(t, setup, sdb, 0, "SELECT id, who, v FROM p ORDER BY id")
	if len(rows) != writers*perWriter {
		t.Fatalf("%d rows, want %d", len(rows), writers*perWriter)
	}
	for _, r := range rows {
		id := r[0].(int64)
		w, i := id/1000, id%1000
		if r[1] != w || r[2] != fmt.Sprintf("writer %d row %d", w, i) {
			t.Fatalf("row %d is %v", id, r)
		}
	}
}

// dialClient opens a session without the cleanup of a test, for the goroutines of a test.
func dialClient(srv *sqlite3srv.Server) (*sqlite3.Client, error) {
	ctx, cancel := context.WithTimeout(context.Background(), sqlite3srv.FailBound)
	defer cancel()
	return sqlite3.Dial(ctx, srv.Listener(), rt.Options{})
}

// TestDialBurstBeyondControlLinks documents what a client sees when more sessions connect at the same moment
// than the server has control links (BINDGEN.md 4.11, --max-control-links): the surplus control links are
// closed at once, and Dial fails with a transport error (hqq.ErrPeerClosed) or, if the close came late,
// a timeout; there is no status that says "try again". The sessions that did connect work, and the
// server is unaffected. A connection pool that opens sessions together must retry Dial or the server must
// be started with more control links (this server's default is 256, the hand-written server's
// listener has no such limit).
func TestDialBurstBeyondControlLinks(t *testing.T) {
	srv := sqlite3srv.Start(t, sqlite3srv.Config{Args: []string{"--max-control-links", "1"}})
	const n = 40
	type result struct {
		c   *sqlite3.Client
		err error
	}
	res := make(chan result, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			c, err := sqlite3.Dial(ctx, srv.Listener(), rt.Options{ConnectTimeout: 3 * time.Second})
			res <- result{c, err}
		}()
	}
	wg.Wait()
	close(res)
	ok, failed := 0, 0
	for r := range res {
		if r.err != nil {
			failed++
			if !errors.Is(r.err, hqq.ErrPeerClosed) && !errors.Is(r.err, hqq.ErrTimeout) && !errors.Is(r.err, hqq.ErrPeerGone) {
				t.Errorf("a refused dial: %v, want a transport error", r.err)
			}
			continue
		}
		ok++
		if v, err := r.c.Sqlite3LibversionNumber(); err != nil || v == 0 {
			t.Errorf("a session that connected: %d, %v", v, err)
		}
		if err := r.c.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	}
	t.Logf("%d of %d simultaneous dials connected, %d were refused", ok, n, failed)
	if ok == 0 {
		t.Fatal("no dial connected")
	}
	// The server serves a new session afterwards.
	c := srv.Dial()
	if v, err := c.Sqlite3LibversionNumber(); err != nil || v == 0 {
		t.Fatalf("a session after the burst: %d, %v", v, err)
	}
}

// TestCloseRetainOnFail reaches the @retain_on_fail annotation of sqlite3_close, which no honest sequence of
// calls can: the server finalizes the statements of a connection before it closes it, so SQLite has no reason
// to answer SQLITE_BUSY. The test build of the server (build.sh -T) leaks a statement on request, and then
// the close fails with SQLITE_BUSY. The annotation keeps the connection in the handle table: it
// is still usable, a second close fails the same way, and the statements that the first close destroyed
// stay destroyed (the cascade is not rolled back, BINDGEN.md 5.1). At the end of the session the teardown
// tries once more, fails, and counts it (the count is the aux of the CLOSE_ACK, which Client.Close does not
// return): the session ends all the same.
func TestCloseRetainOnFail(t *testing.T) {
	srv := sqlite3srv.Start(t, sqlite3srv.Config{Env: []string{"SQLITE3GEN_TEST_LEAK_STMT=1"}})
	c := srv.Dial()
	db := open(t, c, "retain.db")
	seedK(t, c, db)
	st := prep(t, c, db, "SELECT v FROM k") // the test build leaks one more statement here

	err := c.Sqlite3Close(db)
	if err == nil {
		t.Skip("the server was built without the test hooks (build.sh -T): sqlite3_close cannot fail here")
	}
	if le := libErr(t, err); le.Code&0xff != codeBusy {
		t.Fatalf("close with a leaked statement: code %d, want SQLITE_BUSY", le.Code)
	}
	// The entry is still live: the connection answers.
	if v, err := c.Sqlite3GetAutocommit(db); err != nil || v != 1 {
		t.Fatalf("the connection after the failed close: %d, %v", v, err)
	}
	if n := one(t, c, db, "SELECT count(*) FROM k"); n != int64(1) {
		t.Fatalf("a query on the retained connection: %v", n)
	}
	// The statement that the session knew was destroyed by the cascade, whatever the close did.
	if _, err := c.Sqlite3Step(st); !errors.Is(err, rt.ErrBadHandle) {
		t.Fatalf("step on a statement of the failed close: %v, want ErrBadHandle", err)
	}
	if le := libErr(t, c.Sqlite3Close(db)); le.Code&0xff != codeBusy {
		t.Fatalf("second close: code %d, want SQLITE_BUSY", le.Code)
	}
	// An ordinary session end: the teardown cannot close the connection either and does not crash.
	if err := c.Close(); err != nil {
		t.Fatalf("session close: %v", err)
	}
	if !srv.Log.WaitCount(sqlite3srv.LineSessionEnded, 1, sqlite3srv.FailBound) {
		t.Fatalf("the session did not end:\n%s", srv.Log.String())
	}
}
