package supervisor

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDurationJSON(t *testing.T) {
	var d struct {
		D Duration `json:"d"`
	}
	if err := json.Unmarshal([]byte(`{"d":"250ms"}`), &d); err != nil {
		t.Fatal(err)
	}
	if time.Duration(d.D) != 250*time.Millisecond {
		t.Fatalf("parsed %v", time.Duration(d.D))
	}
	out, _ := json.Marshal(d)
	if string(out) != `{"d":"250ms"}` {
		t.Fatalf("marshal = %s", out)
	}
	for _, bad := range []string{`{"d":5}`, `{"d":"5x"}`, `{"d":"-1s"}`} {
		var x struct {
			D Duration `json:"d"`
		}
		if err := json.Unmarshal([]byte(bad), &x); err == nil && bad != `{"d":"-1s"}` {
			t.Fatalf("%s accepted", bad)
		}
	}
}

// fullExample is the services part of the configuration documented in
// cmd/hqqd/README.md (the "admin" block is read by hqqd itself).
const fullExample = `{
  "services": [
    {
      "name": "echo",
      "command": "/usr/local/bin/hqq-echo",
      "args": ["--listener", "echo"],
      "env": {"HQQ_LOG": "info"},
      "dir": "/var/lib/echo",
      "listener": "echo",
      "autostart": true,
      "restart": {
        "policy": "on-failure",
        "backoff": {"initial": "250ms", "max": "10s", "jitter": 0.2},
        "max_restarts": 5,
        "window": "1m",
        "reset_after": "30s"
      },
      "stop_timeout": "5s",
      "stop_signal": "SIGTERM",
      "stop_method": 7,
      "health": {"interval": "1s", "timeout": "500ms", "failures": 3, "start_grace": "5s", "ping_method": 1},
      "exclusive_lock": "/var/lock/echo.lock",
      "output": {"mode": "file", "path": "/var/log/echo.log", "max_bytes": 10485760, "max_files": 3}
    }
  ]
}`

func TestDecodeFullExample(t *testing.T) {
	cfg, err := DecodeConfig([]byte(fullExample))
	if err != nil {
		t.Fatal(err)
	}
	s := cfg.Services[0]
	if s.Name != "echo" || s.Listener != "echo" || len(s.Args) != 2 || s.Env["HQQ_LOG"] != "info" {
		t.Fatalf("decoded %+v", s)
	}
	if s.Restart.Backoff.initial() != 250*time.Millisecond || s.Restart.Backoff.max() != 10*time.Second {
		t.Fatalf("backoff %+v", s.Restart.Backoff)
	}
	if s.Health.interval() != time.Second || s.Health.startGrace() != 5*time.Second || s.Health.PingMethod != 1 {
		t.Fatalf("health %+v", s.Health)
	}
	if s.StopMethod != 7 || s.stopTimeout() != 5*time.Second || s.Output.maxFiles() != 3 {
		t.Fatalf("stop/output %+v %+v", s, s.Output)
	}
}

func TestDecodeRejects(t *testing.T) {
	base := func(svc string) string {
		return `{"services":[` + svc + `]}`
	}
	cases := map[string]string{
		"unknown field":     base(`{"name":"a","command":"x","bogus":1}`),
		"no name":           base(`{"command":"x"}`),
		"bad name":          base(`{"name":"a b","command":"x"}`),
		"no command":        base(`{"name":"a"}`),
		"bad policy":        base(`{"name":"a","command":"x","restart":{"policy":"sometimes"}}`),
		"bad signal":        base(`{"name":"a","command":"x","stop_signal":"SIGWHAT"}`),
		"bad duration":      base(`{"name":"a","command":"x","stop_timeout":"ten"}`),
		"stop method alone": base(`{"name":"a","command":"x","stop_method":7}`),
		"initial > max":     base(`{"name":"a","command":"x","restart":{"backoff":{"initial":"2s","max":"1s"}}}`),
		"jitter > 1":        base(`{"name":"a","command":"x","restart":{"backoff":{"jitter":1.5}}}`),
		"file no path":      base(`{"name":"a","command":"x","output":{"mode":"file"}}`),
		"bad output mode":   base(`{"name":"a","command":"x","output":{"mode":"tty"}}`),
		"dup name":          base(`{"name":"a","command":"x"},{"name":"a","command":"y"}`),
		"dup listener":      base(`{"name":"a","command":"x","listener":"l"},{"name":"b","command":"y","listener":"l"}`),
		"dup lock":          base(`{"name":"a","command":"x","exclusive_lock":"/tmp/l"},{"name":"b","command":"y","exclusive_lock":"/tmp/l"}`),
		"trailing data":     `{"services":[]} {}`,
	}
	for name, doc := range cases {
		if _, err := DecodeConfig([]byte(doc)); err == nil {
			t.Errorf("%s: accepted %s", name, doc)
		}
	}
}

func TestBackoffDelay(t *testing.T) {
	b := BackoffSpec{Initial: Duration(100 * time.Millisecond), Max: Duration(time.Second), Jitter: ptrFloat(0)}
	want := []time.Duration{100, 200, 400, 800, 1000, 1000}
	for n, w := range want {
		if got := b.delay(n, 0.5); got != w*time.Millisecond {
			t.Errorf("delay(%d) = %s, want %s", n, got, w*time.Millisecond)
		}
	}
	j := BackoffSpec{Initial: Duration(time.Second), Max: Duration(time.Second)}
	lo, hi := j.delay(0, 0), j.delay(0, 0.999999)
	if lo < 800*time.Millisecond || hi > 1200*time.Millisecond || lo >= hi {
		t.Errorf("jitter bounds: %s .. %s", lo, hi)
	}
	if got := (BackoffSpec{}).delay(100, 0.5); got != defaultBackoffMax {
		t.Errorf("default cap: %s", got)
	}
	if got := (BackoffSpec{Max: Duration(100 * time.Millisecond)}).initial(); got != 100*time.Millisecond {
		t.Errorf("initial with only max set: %s", got)
	}
	if _, err := DecodeConfig([]byte(`{"services":[{"name":"a","command":"x","restart":{"backoff":{"max":"100ms"}}}]}`)); err != nil {
		t.Errorf("max alone below the default initial was rejected: %v", err)
	}
}

func ptrFloat(f float64) *float64 { return &f }

func TestRotWriterRotatesAndKeeps(t *testing.T) {
	dir := tempDir(t)
	path := filepath.Join(dir, "svc.log")
	w, err := openRotWriter(path, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"aaaaaa\n", "bbbbbb\n", "cccccc\n", "dddddd\n"} {
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	_ = w.Close()
	read := func(p string) string {
		b, _ := os.ReadFile(p)
		return string(b)
	}
	if read(path) != "dddddd\n" || read(path+".1") != "cccccc\n" || read(path+".2") != "bbbbbb\n" {
		t.Fatalf("rotation: %q %q %q", read(path), read(path+".1"), read(path+".2"))
	}
	if _, err := os.Stat(path + ".3"); err == nil {
		t.Fatal("more rotated files than max_files")
	}
}

func TestPrefixWriterLines(t *testing.T) {
	var out bytes.Buffer
	w := newPrefixWriter("svc | ", &out)
	_, _ = w.Write([]byte("one\ntw"))
	_, _ = w.Write([]byte("o\nthree"))
	if got := out.String(); got != "svc | one\nsvc | two\n" {
		t.Fatalf("before flush: %q", got)
	}
	w.Flush()
	if got := out.String(); got != "svc | one\nsvc | two\nsvc | three\n" {
		t.Fatalf("after flush: %q", got)
	}
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	if _, err := New(Config{Services: []ServiceSpec{{Name: "a"}}}); err == nil {
		t.Fatal("New accepted a service without a command")
	}
	if !strings.Contains(mustErr(New(Config{Services: []ServiceSpec{{Name: "a", Command: "x", Restart: RestartSpec{Policy: "zzz"}}}})), "policy") {
		t.Fatal("error does not name the policy")
	}
}

func mustErr(_ *Supervisor, err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
