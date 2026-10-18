//go:build unix

package sqlite3_test

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"gosuda.org/hqq/bindgen/examples/sqlite3"
	"gosuda.org/hqq/bindgen/examples/sqlite3/sqlite3srv"
	"gosuda.org/hqq/bindgen/rt"
)

// The policy tests cover the server's path and open refusals (server/sqlsrv_policy.c,
// server/sqlsrv_pathrules.c, server/sqlsrv_proto.c, and server/tests/test_units.c):
//
//	TestPathPolicy          sqlite/e2e/e2e_test.go TestPathPolicy, and test_policy in
//	                        server/tests/test_units.c (traversal, absolute paths, "..",
//	                        a missing parent directory)
//	TestPathPolicySymlinks  sqlite/e2e/e2e_test.go TestPathPolicySymlinks, and test_policy (links)
//	TestAliasOnlyPolicy     sqlite/e2e/e2e_test.go TestAliasOnlyPolicy
//	TestOpenFlags           test_validation in server/tests/test_units.c (sqlsrv_valid_open_flags)
//	TestNoURIAndNoMemory    URI and in-memory database rejection
//	TestAttachAndExtensions PROTOCOL.md section 12 (authorizer, extension loading)
//	TestSqlAndColumnLimits  PROTOCOL.md section 11 (sqlite3_limit), in errors_test.go
//	TestOpenMissingFile     PROTOCOL.md 6.1 ("does not exist and CREATE is not set": SQLITE_CANTOPEN)
//	TestStartupRejectsBadArguments  sqlite/e2e/e2e_test.go TestStartupRejectsBadArguments
//	TestSessionLimit        sqlite/e2e/e2e_test.go TestSessionLimit (in session_test.go)

// denied reports whether err is the refusal of the open policy: a status error ErrDenied.
func denied(err error) bool {
	return errors.Is(err, rt.ErrDenied)
}

// TestPathPolicy checks that paths outside the allow directory are refused, and that a path inside is
// allowed and creates its file there.
func TestPathPolicy(t *testing.T) {
	srv := sqlite3srv.Start(t, sqlite3srv.Config{})
	c := srv.Dial()
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim.db")
	if err := os.WriteFile(victim, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"../escape.db",
		"sub/../../escape.db",
		"a/b/../../../escape.db",
		victim, // an absolute path outside
		"/etc/hosts",
		"..",
		"../",
		strings.Repeat("../", 20) + "etc/passwd",
	} {
		_, err := c.Sqlite3OpenV2(name, sqlite3.SQLITE_OPEN_READWRITE|sqlite3.SQLITE_OPEN_CREATE, nil)
		if !denied(err) {
			t.Errorf("open %q: %v, want a denial", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(srv.AllowDir), "escape.db")); err == nil {
		t.Error("a database was created above the allow directory")
	}
	// "." is the allow directory itself, which is not a database inside it.
	if _, err := c.Sqlite3OpenV2(".", sqlite3.SQLITE_OPEN_READWRITE|sqlite3.SQLITE_OPEN_CREATE, nil); err == nil {
		t.Error("open of the allow directory itself succeeded")
	}
	// A missing parent directory is not an escape: the open fails with SQLITE_CANTOPEN, before SQLite
	// is called (application status SVC_STATUS_SQLITE_BASE + SQLITE_CANTOPEN).
	_, err := c.Sqlite3OpenV2("missing/x.db", sqlite3.SQLITE_OPEN_READWRITE|sqlite3.SQLITE_OPEN_CREATE, nil)
	if se := statusErr(t, err); se.Status != statusCantOpen {
		t.Errorf("missing parent: status %d, want %d", se.Status, statusCantOpen)
	}
	// An absolute path that lies inside the allow directory is allowed.
	inAbs := filepath.Join(srv.AllowDir, "abs.db")
	db, err := c.Sqlite3OpenV2(inAbs, sqlite3.SQLITE_OPEN_READWRITE|sqlite3.SQLITE_OPEN_CREATE, nil)
	if err != nil {
		t.Fatalf("open %q: %v", inAbs, err)
	}
	run(t, c, db, "CREATE TABLE a (x)")
	// A path inside is allowed, in a sub directory too, and names the file there.
	if err := os.Mkdir(filepath.Join(srv.AllowDir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"inside.db", "sub/inside2.db", "sub/./inside3.db", "sub//inside4.db"} {
		db := open(t, c, name)
		run(t, c, db, "CREATE TABLE a (x)")
		if err := c.Sqlite3Close(db); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(srv.AllowDir, filepath.Clean(name))); err != nil {
			t.Errorf("database file of %q not created in the allow directory: %v", name, err)
		}
	}
	// A name with a dot-dot inside a component is just a name.
	db = open(t, c, "a..b.db")
	run(t, c, db, "CREATE TABLE a (x)")
}

// TestPathPolicySymlinks checks that links out of the allow directory do not help: the policy resolves them
// and refuses their targets. A link that stays inside is followed and allowed.
func TestPathPolicySymlinks(t *testing.T) {
	srv := sqlite3srv.Start(t, sqlite3srv.Config{})
	c := srv.Dial()
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim.db")
	if err := os.WriteFile(victim, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	links := []struct{ link, target string }{
		{"link.db", victim},
		{"dangling.db", filepath.Join(outside, "missing.db")},
		{"dirlink", outside},
	}
	for _, l := range links {
		if err := os.Symlink(l.target, filepath.Join(srv.AllowDir, l.link)); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"link.db", "dangling.db", "dirlink/victim.db", "dirlink/new.db"} {
		_, err := c.Sqlite3OpenV2(name, sqlite3.SQLITE_OPEN_READWRITE|sqlite3.SQLITE_OPEN_CREATE, nil)
		if !denied(err) {
			t.Errorf("open %q: %v, want a denial", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "new.db")); err == nil {
		t.Error("a database was created through a link")
	}
	if fi, err := os.Stat(victim); err != nil || fi.Size() != 0 {
		t.Errorf("the victim file changed: %v", err)
	}

	// A link to a file inside the directory is followed; it is the same database.
	db := open(t, c, "real.db")
	run(t, c, db, "CREATE TABLE r (x)")
	run(t, c, db, "INSERT INTO r VALUES (7)")
	if err := os.Symlink(filepath.Join(srv.AllowDir, "real.db"), filepath.Join(srv.AllowDir, "alias.db")); err != nil {
		t.Fatal(err)
	}
	db2 := open(t, c, "alias.db")
	if got := one(t, c, db2, "SELECT x FROM r"); got != int64(7) {
		t.Fatalf("through the inside link: %v", got)
	}
}

// TestAliasOnlyPolicy starts a server with an alias and no allow directory: the alias works, wherever its
// path is, and every raw path is refused.
func TestAliasOnlyPolicy(t *testing.T) {
	dir := t.TempDir()
	appPath := filepath.Join(dir, "app.db")
	srv := sqlite3srv.Start(t, sqlite3srv.Config{AllowDir: "-", Aliases: map[string]string{"app": appPath}})
	c := srv.Dial()
	db := open(t, c, "app")
	run(t, c, db, "CREATE TABLE a (v INTEGER)")
	if _, err := os.Stat(appPath); err != nil {
		t.Fatalf("alias database not created: %v", err)
	}
	for _, raw := range []string{"main", "main.db", appPath, "app.db", "App", "app/", "../x"} {
		_, err := c.Sqlite3OpenV2(raw, sqlite3.SQLITE_OPEN_READWRITE|sqlite3.SQLITE_OPEN_CREATE, nil)
		if !denied(err) {
			t.Errorf("raw path %q: %v, want a denial", raw, err)
		}
	}
}

// TestAliasAndAllowDir has both: an alias wins over a raw path of the same name, and an alias may point
// outside the allow directory (the operator wrote it).
func TestAliasAndAllowDir(t *testing.T) {
	elsewhere := t.TempDir()
	target := filepath.Join(elsewhere, "aliased.db")
	srv := sqlite3srv.Start(t, sqlite3srv.Config{Aliases: map[string]string{"shadow.db": target}})
	c := srv.Dial()
	db := open(t, c, "shadow.db")
	run(t, c, db, "CREATE TABLE a (v)")
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("the alias target was not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(srv.AllowDir, "shadow.db")); err == nil {
		t.Fatal("the raw path was used although an alias of that name exists")
	}
	// A raw path next to it still works.
	run(t, c, open(t, c, "plain.db"), "CREATE TABLE a (v)")
}

// TestOpenFlags checks the open flags: exactly one of READONLY and READWRITE, optionally CREATE, no other
// bit. The accepted and refused sets are those of test_validation in the hand-written server's unit
// tests; the further refused values are the ones that SQLite would honor: URI, in-memory, mutex mode,
// shared cache, no-follow, extended result codes.
func TestOpenFlags(t *testing.T) {
	srv := sqlite3srv.Start(t, sqlite3srv.Config{})
	c := srv.Dial()
	// A database that exists, so that READONLY can open it.
	seed := open(t, c, "flags.db")
	run(t, c, seed, "CREATE TABLE f (x)")
	if err := c.Sqlite3Close(seed); err != nil {
		t.Fatal(err)
	}

	accepted := []int32{0x1, 0x2, 0x6}
	refused := []int32{0x0, 0x3, 0x4, 0x8, 0x7,
		0x40 | 0x2,      // SQLITE_OPEN_URI
		0x80 | 0x2,      // SQLITE_OPEN_MEMORY
		0x100 | 0x2,     // SQLITE_OPEN_MAIN_DB
		0x8000 | 0x2,    // SQLITE_OPEN_NOMUTEX
		0x10000 | 0x2,   // SQLITE_OPEN_FULLMUTEX
		0x20000 | 0x2,   // SQLITE_OPEN_SHAREDCACHE
		0x40000 | 0x2,   // SQLITE_OPEN_PRIVATECACHE
		0x1000000 | 0x2, // SQLITE_OPEN_NOFOLLOW
		0x2000000 | 0x2, // SQLITE_OPEN_EXRESCODE
		-1, -2147483648, 0x7fffffff, 1 << 30,
	}
	for _, f := range accepted {
		db, err := c.Sqlite3OpenV2("flags.db", f, nil)
		if err != nil {
			t.Errorf("flags %#x: %v, want accepted", f, err)
			continue
		}
		if err := c.Sqlite3Close(db); err != nil {
			t.Errorf("close: %v", err)
		}
	}
	for _, f := range refused {
		if _, err := c.Sqlite3OpenV2("flags.db", f, nil); !denied(err) {
			t.Errorf("flags %#x: %v, want a denial", f, err)
		}
	}
	// READONLY|CREATE passes the policy (it does so in the hand-written server too) and is SQLite's own
	// refusal: SQLITE_MISUSE.
	_, err := c.Sqlite3OpenV2("flags.db", 0x5, nil)
	if le := libErr(t, err); le.Code != 21 {
		t.Errorf("flags 0x5: code %d, want SQLITE_MISUSE", le.Code)
	}
	// The vfs argument: only NULL.
	for _, vfs := range []string{"unix", "unix-none", "unix-dotfile", "unix-excl", ""} {
		_, err := c.Sqlite3OpenV2("flags.db", sqlite3.SQLITE_OPEN_READWRITE, &vfs)
		if !denied(err) {
			t.Errorf("vfs %q: %v, want a denial", vfs, err)
		}
	}
	// The session survived every refusal.
	db := open(t, c, "after.db")
	if got := one(t, c, db, "SELECT 1"); got != int64(1) {
		t.Fatalf("after the refusals: %v", got)
	}
}

// TestOpenReadOnly checks that READONLY is read-only: a write fails with SQLITE_READONLY.
func TestOpenReadOnly(t *testing.T) {
	srv := sqlite3srv.Start(t, sqlite3srv.Config{})
	c := srv.Dial()
	rw := open(t, c, "ro.db")
	run(t, c, rw, "CREATE TABLE r (x)")
	run(t, c, rw, "INSERT INTO r VALUES (1)")
	ro, err := c.Sqlite3OpenV2("ro.db", sqlite3.SQLITE_OPEN_READONLY, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := one(t, c, ro, "SELECT x FROM r"); got != int64(1) {
		t.Fatalf("read: %v", got)
	}
	st := prep(t, c, ro, "INSERT INTO r VALUES (2)")
	_, _, err = c.ExecStmt(st, nil, nil, nil)
	if le := libErr(t, err); le.Code&0xff != codeReadonly {
		t.Fatalf("write on a read-only connection: code %d %q, want SQLITE_READONLY", le.Code, le.Detail)
	}
}

// TestOpenMissingFile checks that a database that does not exist is not created without CREATE.
func TestOpenMissingFile(t *testing.T) {
	srv := sqlite3srv.Start(t, sqlite3srv.Config{})
	c := srv.Dial()
	for _, f := range []int32{sqlite3.SQLITE_OPEN_READWRITE, sqlite3.SQLITE_OPEN_READONLY} {
		_, err := c.Sqlite3OpenV2("nofile.db", f, nil)
		le := libErr(t, err)
		if le.Code&0xff != codeCantOpen || le.Detail != "unable to open database file" {
			t.Errorf("flags %#x: code %d %q, want SQLITE_CANTOPEN", f, le.Code, le.Detail)
		}
	}
	if _, err := os.Stat(filepath.Join(srv.AllowDir, "nofile.db")); err == nil {
		t.Fatal("the file was created without CREATE")
	}
	// A failed open leaves no connection behind: the session table does not fill up with them.
	// (The server of this test has the default 4096 entries; the loop is longer than that.)
	for range 5000 {
		if _, err := c.Sqlite3OpenV2("nofile.db", sqlite3.SQLITE_OPEN_READWRITE, nil); libErr(t, err).Code&0xff != codeCantOpen {
			t.Fatalf("open: %v", err)
		}
	}
	// An empty name, a name above 4,096 bytes and a name with a NUL are refused (the last two by the codec).
	for _, name := range []string{"", strings.Repeat("a", 4097), "a\x00b.db"} {
		if _, err := c.Sqlite3OpenV2(name, sqlite3.SQLITE_OPEN_READWRITE|sqlite3.SQLITE_OPEN_CREATE, nil); err == nil {
			t.Errorf("open of the name %q... succeeded", abbrev(name))
		}
	}
	if got := one(t, c, open(t, c, "ok.db"), "SELECT 2"); got != int64(2) {
		t.Fatal("the session does not work after the refusals")
	}
}

// TestNoURIAndNoMemory checks that the name is a file name and never a URI: a name that looks like a URI
// or like ":memory:" creates a file of that name inside the allow directory.
func TestNoURIAndNoMemory(t *testing.T) {
	srv := sqlite3srv.Start(t, sqlite3srv.Config{})
	c := srv.Dial()
	for _, name := range []string{"file:uri.db?mode=memory&cache=shared", ":memory:", "file::memory:", "x.db?immutable=1"} {
		db := open(t, c, name)
		run(t, c, db, "CREATE TABLE n (x)")
		if _, err := os.Stat(filepath.Join(srv.AllowDir, name)); err != nil {
			t.Errorf("%q did not create a file of that name: %v", name, err)
		}
		if err := c.Sqlite3Close(db); err != nil {
			t.Fatal(err)
		}
	}
}

// TestAttachAndExtensions checks the authorizer and the extension loader. ATTACH and DETACH are denied
// (SQLITE_AUTH at prepare); loading an extension is not possible: neither load_extension() in SQL nor any
// function of the service. Pragmas and everything else keep working.
func TestAttachAndExtensions(t *testing.T) {
	srv := sqlite3srv.Start(t, sqlite3srv.Config{})
	c := srv.Dial()
	db := open(t, c, "auth.db")
	other := filepath.Join(srv.AllowDir, "other.db")
	for _, sql := range []string{
		"ATTACH DATABASE '" + other + "' AS o",
		"ATTACH '" + filepath.Join(os.TempDir(), "outside-attach.db") + "' AS o",
		"ATTACH ':memory:' AS m",
		"DETACH DATABASE main",
		"DETACH o",
	} {
		_, err := c.PrepareOne(db, sql)
		if le := libErr(t, err); le.Code != codeAuth || le.Detail != "not authorized" {
			t.Errorf("%q: code %d %q, want SQLITE_AUTH", sql, le.Code, le.Detail)
		}
	}
	if _, err := os.Stat(other); err == nil {
		t.Error("ATTACH created a file")
	}
	for _, sql := range []string{
		"SELECT load_extension('/nonexistent/ext')",
		"SELECT load_extension('/nonexistent/ext', 'entry')",
	} {
		_, err := c.PrepareOne(db, sql)
		if le := libErr(t, err); le.Code != codeError {
			t.Errorf("%q: code %d %q, want SQLITE_ERROR", sql, le.Code, le.Detail)
		}
	}
	// Allowed: pragmas, temp tables, views, triggers, and in-memory temp storage.
	for _, sql := range []string{"PRAGMA journal_mode = DELETE", "PRAGMA user_version = 3", "CREATE TEMP TABLE tt (x)",
		"CREATE TABLE a (x)", "CREATE VIEW v AS SELECT * FROM a", "CREATE TRIGGER tr AFTER INSERT ON a BEGIN SELECT 1; END"} {
		run(t, c, db, sql)
	}
	if got := one(t, c, db, "PRAGMA user_version"); got != int64(3) {
		t.Fatalf("user_version = %v", got)
	}
}

// TestDefaultDeny checks the allowlist. The IDL declares no function that opens a hole (the list below is
// what the hand-written server also does not offer), and a request for an operation that is not in the
// interface is answered with E_UNKNOWN_OP by the server and does not disturb the session.
func TestDefaultDeny(t *testing.T) {
	idl, err := os.ReadFile("sqlite3.idl")
	if err != nil {
		t.Fatal(err)
	}
	for _, fn := range []string{"sqlite3_exec", "sqlite3_load_extension", "sqlite3_enable_load_extension",
		"sqlite3_config", "sqlite3_db_config", "sqlite3_set_authorizer", "sqlite3_create_function",
		"sqlite3_create_collation", "sqlite3_auto_extension", "sqlite3_open(", "sqlite3_open16", "sqlite3_db_filename",
		"sqlite3_interrupt", "sqlite3_backup_init", "sqlite3_blob_open", "sqlite3_trace", "sqlite3_get_table",
		"sqlite3_wal_hook", "sqlite3_file_control", "sqlite3_limit", "sqlite3_vfs_register", "sqlite3_serialize",
		"sqlite3_deserialize", "sqlite3_exec(", "sqlite3_free(", "sqlite3_malloc"} {
		for _, line := range strings.Split(string(idl), "\n") {
			l := strings.TrimSpace(line)
			if strings.HasPrefix(l, "//") {
				continue
			}
			if strings.Contains(l, "fn "+fn) {
				t.Errorf("sqlite3.idl declares %s", fn)
			}
		}
	}
	srv := sqlite3srv.Start(t, sqlite3srv.Config{})
	// The generated client cannot name an operation that the IDL does not declare, so this test dials with an
	// interface that has the digest of the real one and more operations than it.
	src, err := os.ReadFile("sqlite3_client.go")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`interface digest ([0-9a-f]{64})`).FindSubmatch(src)
	if m == nil {
		t.Fatal("no digest in sqlite3_client.go")
	}
	var digest [32]byte
	if _, err := hex.Decode(digest[:], m[1]); err != nil {
		t.Fatal(err)
	}
	var ops []string
	for i := range 70 {
		ops = append(ops, fmt.Sprintf("op%d", i+1))
	}
	ctx, cancel := context.WithTimeout(context.Background(), sqlite3srv.FailBound)
	defer cancel()
	conn, err := rt.Dial(ctx, srv.Listener(), &rt.Iface{Service: "sqlite3", Digest: digest, Ops: ops}, rt.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	for _, op := range []uint16{36, 40, 70} {
		e, err := conn.BeginRequest(op, 0, 64)
		if err != nil {
			t.Fatalf("op %d: BeginRequest: %v", op, err)
		}
		call, err := conn.Submit(e)
		if err != nil {
			t.Fatalf("op %d: Submit: %v", op, err)
		}
		_, err = conn.Wait(call, call.ID())
		if !errors.Is(err, rt.ErrUnknownOp) {
			t.Fatalf("op %d: %v, want ErrUnknownOp", op, err)
		}
	}
	// The session works after the unknown operations (a real operation: sqlite3_libversion_number is 11).
	e, err := conn.BeginRequest(11, 0, 64)
	if err != nil {
		t.Fatal(err)
	}
	call, err := conn.Submit(e)
	if err != nil {
		t.Fatal(err)
	}
	d, err := conn.Wait(call, call.ID())
	if err != nil {
		t.Fatalf("a real operation after the unknown ones: %v", err)
	}
	d.Release()
}

// TestStartupRejectsBadArguments checks that the server refuses a bad option without listening (exit 2),
// like the hand-written server.
func TestStartupRejectsBadArguments(t *testing.T) {
	bin := sqlite3srv.Binary(t)
	for _, args := range [][]string{
		{"--allow-dir", filepath.Join(t.TempDir(), "missing")},
		{"--allow-dir"},
		{"--alias", "noequals"},
		{"--alias", "=path"},
		{"--alias", "a=" + filepath.Join(t.TempDir(), "missing-dir", "x.db")},
		{"--max-sessions", "0"},
		{"--no-such-option"},
	} {
		out, err := runServer(bin, append([]string{"s3g-bad"}, args...)...)
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Errorf("%v: %v, want an exit status\n%s", args, err, out)
			continue
		}
		if ee.ExitCode() == 0 || strings.Contains(out, "ready s3g-bad") {
			t.Errorf("%v: exit %d with output %q, want a refusal", args, ee.ExitCode(), out)
		}
	}
	// A duplicate alias.
	out, err := runServer(bin, "s3g-bad", "--alias", "a="+t.TempDir()+"/x.db", "--alias", "a="+t.TempDir()+"/y.db")
	if err == nil || !strings.Contains(out, "duplicate alias") {
		t.Errorf("duplicate alias: %v, %q", err, out)
	}
}

// runServer runs the server binary with arguments and returns its combined output. A server that does not
// refuse its arguments would run forever, so the run is bounded.
func runServer(bin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	return string(out), err
}
