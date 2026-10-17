package launcher_test

// End-to-end tests against the real C server. HQQ_SQLITE_SERVER names the
// binary (see examples/sqlite/server/README.md); without it every test skips.
// The server is started through the launcher, so the binary is hash-verified.

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gosuda.org/hqq"
	"gosuda.org/hqq/internal/launchtest"
	"gosuda.org/hqq/secureexec"
	"gosuda.org/hqq/sqlite"
	"gosuda.org/hqq/sqlite/internal/contract"
	"gosuda.org/hqq/sqlite/launcher"
)

// serverBinary returns the resolved path and digest of the real server, or
// skips the test.
func serverBinary(t *testing.T) (string, secureexec.Hash) {
	t.Helper()
	bin := os.Getenv("HQQ_SQLITE_SERVER")
	if bin == "" {
		t.Skip("HQQ_SQLITE_SERVER is not set: build the C server and point it there")
	}
	resolved, err := filepath.EvalSymlinks(bin)
	if err != nil {
		t.Skipf("HQQ_SQLITE_SERVER: %v", err)
	}
	abs, err := filepath.Abs(resolved)
	if err != nil {
		t.Fatal(err)
	}
	sum, _, err := secureexec.HashFile(abs)
	if err != nil {
		t.Fatal(err)
	}
	return abs, sum
}

// e2eConfig is the launcher configuration of the real server with allow
// directory dir.
func e2eConfig(bin string, sum secureexec.Hash, dir string) launcher.Config {
	return launcher.Config{
		Binary:       bin,
		SHA256:       sum,
		AllowDir:     dir,
		ExtraArgs:    e2eServerFlags,
		ReadyTimeout: 15 * time.Second,
		StopTimeout:  10 * time.Second,
	}
}

// dbConfig is the driver configuration of the tests: quick link heartbeats.
func dbConfig() sqlite.Config {
	return sqlite.Config{
		Timeout:     5 * time.Second,
		BusyTimeout: 5 * time.Second,
		Link: hqq.Config{Advanced: hqq.AdvancedOptions{Link: hqq.LinkOptions{
			HeartbeatInterval: 20 * time.Millisecond,
			PeerTimeout:       300 * time.Millisecond,
		}}},
	}
}

// lockedBuffer is a bytes.Buffer safe for the launcher's output goroutines.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// Watchdog bounds of one contract subtest. The contract's context-free calls
// (db.Exec) wait for a response without a deadline, so a server that stops
// answering would hold the run until the go test timeout. After
// watchdogKill the server is killed and its output logged; if the test is
// still stuck watchdogPanic later, the process panics, which dumps every
// goroutine.
const (
	watchdogKill  = 60 * time.Second
	watchdogPanic = 30 * time.Second
)

// contractDB starts one server (with its own allow directory) for the calling
// subtest and returns a *sql.DB on its "main" database. Both are closed by
// t.Cleanup, DB first.
func contractDB(t *testing.T) *sql.DB {
	t.Helper()
	bin, sum := serverBinary(t)
	cfg := e2eConfig(bin, sum, t.TempDir())
	out := &lockedBuffer{}
	cfg.Stderr = out
	m, err := launcher.OpenManaged(context.Background(), cfg,
		launcher.WithDatabase("main"), launcher.WithSQLiteConfig(dbConfig()))
	if err != nil {
		t.Fatal(err)
	}
	name := t.Name()
	hang := time.AfterFunc(watchdogKill, func() {
		t.Logf("%s: no progress for %v; killing the server (pid %d)\nserver stderr:\n%s",
			name, watchdogKill, m.Server.PID(), out.String())
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_ = m.Server.Stop(ctx) // a canceled context kills it
		time.Sleep(watchdogPanic)
		panic(fmt.Sprintf("%s still stuck %v after the server was killed", name, watchdogPanic))
	})
	t.Cleanup(func() {
		hang.Stop()
		if err := m.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
		if t.Failed() {
			t.Logf("server stderr:\n%s", out.String())
		}
	})
	return m.DB
}

// TestContract runs the shared database/sql contract suite through the launcher.
func TestContract(t *testing.T) {
	contract.RunContract(t, contractDB, contract.Capabilities{Engine: "c", FullSQL: true})
}

// TestStartQueryStop starts the server, runs a few statements through a
// *sql.DB, and checks that Stop ends the server gracefully (status 0, through
// the lifeline's end of file).
func TestStartQueryStop(t *testing.T) {
	bin, sum := serverBinary(t)
	ctx := context.Background()
	srv, err := launcher.Start(ctx, e2eConfig(bin, sum, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	c, err := sqlite.NewConnector(srv.Listener(), withDatabase("main"))
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(c)
	for _, q := range []string{"CREATE TABLE t (v INTEGER)", "INSERT INTO t VALUES (1), (2)"} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	var n int64
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM t").Scan(&n); err != nil {
		t.Fatalf("query: %v", err)
	}
	if n != 2 {
		t.Fatalf("count = %d, want 2", n)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("db close: %v", err)
	}
	if err := srv.Stop(ctx); err != nil {
		t.Fatalf("Stop (graceful): %v", err)
	}
	if err := srv.Err(); err != nil {
		t.Fatalf("Err after graceful stop: %v", err)
	}
}

// withDatabase returns the driver configuration with the database "main".
func withDatabase(db string) sqlite.Config {
	cfg := dbConfig()
	cfg.Database = db
	return cfg
}

// TestWrongHashRefused checks that a digest that does not match the binary
// stops the start before the server runs.
func TestWrongHashRefused(t *testing.T) {
	bin, sum := serverBinary(t)
	sum[0] ^= 0xff
	_, err := launcher.Start(context.Background(), e2eConfig(bin, sum, t.TempDir()))
	if !errors.Is(err, secureexec.ErrHashMismatch) {
		t.Fatalf("Start: %v, want ErrHashMismatch", err)
	}
}

// TestParentKilledServerExits starts the server from a helper process, waits
// until a client can use it, then SIGKILLs the helper. The helper's descriptors
// close, so the server reads end of file on its lifeline and must exit on its
// own.
func TestParentKilledServerExits(t *testing.T) {
	bin, _ := serverBinary(t)
	dir := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	helper := exec.Command(exe)
	helper.Env = append(os.Environ(), envE2EParent+"=1", envE2EBinary+"="+bin, envE2EDir+"="+dir)
	helper.Stderr = os.Stderr
	stdout, err := helper.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	defer helper.Process.Kill()

	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("helper output: %v", err)
	}
	var pid int
	var listener string
	if _, err := fmt.Sscanf(strings.TrimSpace(line), "pid %d listener %s", &pid, &listener); err != nil {
		t.Fatalf("helper line %q: %v", line, err)
	}
	if err := launchtest.ProbeProcess(pid); err != nil {
		t.Fatalf("server %d not running: %v", pid, err)
	}

	// The server serves while its parent lives.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, err := sqlite.NewConnector(listener, withDatabase("main"))
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(c)
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping before the kill: %v", err)
	}
	_ = db.Close()

	if err := helper.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = helper.Wait()

	deadline := time.Now().Add(10 * time.Second)
	for launchtest.ProbeProcess(pid) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("server %d still running 10s after its parent was killed", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
