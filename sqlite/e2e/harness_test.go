// Package e2e runs the Go driver (gosuda.org/hqq/sqlite) against the C server
// (examples/sqlite/server) over real HQQ links.
//
// The server binary is named by HQQ_SQLITE_SERVER. When it is not set, or it
// does not exist, every test skips. Build it with libhqq's CMake; see
// examples/sqlite/server/README.md. The tests start and stop the server
// themselves, one per scenario.
package e2e

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"gosuda.org/hqq"
	"gosuda.org/hqq/sqlite"
	"gosuda.org/hqq/sqlite/internal/wire"
)

// fastLink makes heartbeats quick, so a dead peer is seen within a few hundred
// milliseconds. The server flags in fastServerFlags match it.
var fastLink = hqq.LinkOptions{
	HeartbeatInterval: 20 * time.Millisecond,
	PeerTimeout:       300 * time.Millisecond,
}

var fastServerFlags = []string{"--heartbeat-ms", "20", "--peer-timeout-ms", "300"}

// serverBinary returns the path of the C server, or skips the test.
func serverBinary(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("HQQ_SQLITE_SERVER")
	if bin == "" {
		t.Skip("HQQ_SQLITE_SERVER is not set: build the C server and point it there")
	}
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("HQQ_SQLITE_SERVER: %v", err)
	}
	return bin
}

var listenerSeq atomic.Uint64

// listenerName returns a short unique listener name (the macOS shared memory
// name limit is 31 bytes).
func listenerName() string {
	return fmt.Sprintf("sqe%d-%d", os.Getpid()%100000, listenerSeq.Add(1))
}

// readyWatch is the server's stdout. It closes ready when the line
// "ready <name>" has been written.
type readyWatch struct {
	mu    sync.Mutex
	buf   []byte
	want  []byte
	ready chan struct{}
	once  sync.Once
}

func (w *readyWatch) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	if bytes.Contains(w.buf, w.want) {
		w.once.Do(func() { close(w.ready) })
	}
	return len(p), nil
}

// syncBuf collects the server's stderr for failure messages.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// server is one running C server process.
type server struct {
	name   string
	cmd    *exec.Cmd
	exited chan struct{}
	stderr *syncBuf
}

// startServer runs the server on name with args and waits for its ready line.
// It returns an error instead of failing the test, so that a restart can retry.
// The server's standard input is /dev/null.
func startServer(bin, name string, args ...string) (*server, error) {
	return startServerStdin(bin, name, nil, args...)
}

// startServerStdin is startServer with the server's standard input set to
// stdin (nil: /dev/null). The caller keeps the other ends of the pipe.
func startServerStdin(bin, name string, stdin *os.File, args ...string) (*server, error) {
	stdout := &readyWatch{want: []byte("ready " + name + "\n"), ready: make(chan struct{})}
	stderr := &syncBuf{}
	cmd := exec.Command(bin, append([]string{name}, args...)...)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	s := &server{name: name, cmd: cmd, exited: make(chan struct{}), stderr: stderr}
	go func() {
		_ = cmd.Wait()
		close(s.exited)
	}()
	select {
	case <-stdout.ready:
		return s, nil
	case <-s.exited:
		return nil, fmt.Errorf("server %s exited before it was ready: %s", name, stderr.String())
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		<-s.exited
		return nil, fmt.Errorf("server %s not ready after 10s: %s", name, stderr.String())
	}
}

// term sends SIGTERM (graceful shutdown) and returns the exit error, or an
// error when the process did not exit within d.
func (s *server) term(d time.Duration) error {
	if err := s.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		return err
	}
	select {
	case <-s.exited:
		return s.waitErr()
	case <-time.After(d):
		s.kill()
		return fmt.Errorf("server %s did not exit within %v: %s", s.name, d, s.stderr.String())
	}
}

// waitErr returns an error when an exited server did not exit with status 0.
func (s *server) waitErr() error {
	ps := s.cmd.ProcessState
	if ps == nil {
		return fmt.Errorf("server %s: no exit status", s.name)
	}
	if !ps.Success() {
		return fmt.Errorf("server %s: exit status %d", s.name, ps.ExitCode())
	}
	return nil
}

// kill sends SIGKILL: no cleanup runs in the server (PROTOCOL.md section 10).
func (s *server) kill() {
	_ = s.cmd.Process.Kill()
	<-s.exited
}

// alive reports whether the process is still running.
func (s *server) alive() bool {
	select {
	case <-s.exited:
		return false
	default:
		return true
	}
}

// close stops the server if it still runs. Used by t.Cleanup.
func (s *server) close() {
	if !s.alive() {
		return
	}
	if err := s.term(10 * time.Second); err != nil {
		s.kill()
	}
}

// startTestServer starts a server with an allow directory (a fresh temporary
// directory, returned) and the fast link flags. args are added to the command.
func startTestServer(t *testing.T, args ...string) (*server, string, string) {
	t.Helper()
	bin := serverBinary(t)
	dir := t.TempDir()
	name := listenerName()
	all := append(append([]string{"--allow-dir", dir}, fastServerFlags...), args...)
	srv, err := startServer(bin, name, all...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("server stderr:\n%s", srv.stderr.String())
		}
		srv.close()
	})
	return srv, name, dir
}

// driverConfig is the connector configuration of the tests.
func driverConfig(db string) sqlite.Config {
	return sqlite.Config{
		Database:    db,
		Timeout:     5 * time.Second,
		BusyTimeout: 5 * time.Second,
		Link:        hqq.Config{Advanced: hqq.AdvancedOptions{Link: fastLink}},
	}
}

// openDB returns a sql.DB on the database db of the listener name.
func openDB(t *testing.T, name, db string) *sql.DB {
	t.Helper()
	return openDBWith(t, name, driverConfig(db))
}

// openDBWith returns a sql.DB on a new connector with cfg.
func openDBWith(t *testing.T, name string, cfg sqlite.Config) *sql.DB {
	t.Helper()
	c, err := sqlite.NewConnector(name, cfg)
	if err != nil {
		t.Fatal(err)
	}
	d := sql.OpenDB(c) // DB.Close also closes the connector
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// execer is *sql.DB or *sql.Tx.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func mustExec(t *testing.T, db execer, query string, args ...any) sql.Result {
	t.Helper()
	res, err := db.Exec(query, args...)
	if err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
	return res
}

func queryInt(t *testing.T, db *sql.DB, query string, args ...any) int64 {
	t.Helper()
	var v int64
	if err := db.QueryRow(query, args...).Scan(&v); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return v
}

// sqliteCode returns the SQLite code of an error from the driver, or -1.
func sqliteCode(err error) int {
	var se *sqlite.Error
	if errors.As(err, &se) {
		return se.Code
	}
	return -1
}

// protoCode returns the protocol error code of an error, or 0.
func protoCode(err error) wire.ProtoCode {
	var pe *sqlite.ProtocolError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return 0
}

// waitUntil polls cond until it holds or d passes.
func waitUntil(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", d, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ctxWithin returns a context that ends after d, for the calls that must not block.
func ctxWithin(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}
