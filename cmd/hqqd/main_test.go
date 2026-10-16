package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"gosuda.org/hqq/supervisor"
)

// envSleeper turns the test binary into a service that sleeps until it gets
// SIGINT or SIGTERM.
const envSleeper = "HQQD_TEST_SLEEPER"

func TestMain(m *testing.M) {
	if os.Getenv(envSleeper) != "" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		<-ctx.Done()
		// syscall.Exit skips the race runtime's exit hook, which is slow.
		syscall.Exit(0)
	}
	os.Exit(m.Run())
}

var seq atomic.Int64

func shortName() string {
	return fmt.Sprintf("hqdc%x%x", os.Getpid()&0xfff, seq.Add(1))
}

// writeConfig writes a configuration file with an admin listener and one
// sleeping service, and returns its path.
func writeConfig(t *testing.T, admin string, services ...map[string]any) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if len(services) == 0 {
		services = []map[string]any{{
			"name":      "sleeper",
			"command":   exe,
			"env":       map[string]string{envSleeper: "1"},
			"restart":   map[string]any{"policy": "never"},
			"autostart": true,
		}}
	}
	doc := map[string]any{
		"admin":    map[string]any{"listener": admin},
		"services": services,
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "hqqd.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// startHQQD runs an in-process supervisor on path until the test ends.
func startHQQD(t *testing.T, path string, hup <-chan os.Signal) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	logger := log.New(&bytes.Buffer{}, "", 0)
	go func() { done <- runSupervisor(ctx, path, hup, logger) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("runSupervisor: %v", err)
			}
		case <-time.After(20 * time.Second):
			t.Errorf("runSupervisor did not return")
		}
	})
}

// ctl runs one ctl command against the listener and decodes its JSON output.
func ctl(t *testing.T, listener string, out any, args ...string) error {
	t.Helper()
	var buf bytes.Buffer
	all := append([]string{"-listener", listener, "-timeout", "15s"}, args...)
	err := ctlCommand(all, &buf, &buf)
	if err == nil && out != nil {
		if e := json.Unmarshal(buf.Bytes(), out); e != nil {
			t.Fatalf("ctl %v: output %q: %v", args, buf.String(), e)
		}
	}
	return err
}

func TestCtlAgainstInProcessHQQD(t *testing.T) {
	admin := shortName()
	path := writeConfig(t, admin)
	startHQQD(t, path, nil)

	// Autostart runs once the supervisor is up: poll until the service is ready.
	var list []supervisor.Status
	deadline := time.Now().Add(15 * time.Second)
	for {
		list = nil
		if err := ctl(t, admin, &list, "list"); err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(list) == 1 && list[0].State == supervisor.StateReady || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(list) != 1 || list[0].Name != "sleeper" || list[0].State != supervisor.StateReady {
		t.Fatalf("list = %+v", list)
	}
	first := list[0].PID
	if first == 0 {
		t.Fatal("ready service without pid")
	}

	var st supervisor.Status
	if err := ctl(t, admin, &st, "status", "sleeper"); err != nil || st.PID != first {
		t.Fatalf("status = %+v, %v", st, err)
	}
	if err := ctl(t, admin, &st, "restart", "sleeper"); err != nil || st.PID == first || st.State != supervisor.StateReady {
		t.Fatalf("restart = %+v, %v", st, err)
	}
	if err := ctl(t, admin, &st, "stop", "sleeper"); err != nil || st.State != supervisor.StateStopped {
		t.Fatalf("stop = %+v, %v", st, err)
	}
	if err := ctl(t, admin, &st, "start", "sleeper"); err != nil || st.State != supervisor.StateReady {
		t.Fatalf("start = %+v, %v", st, err)
	}

	err := ctl(t, admin, nil, "status", "nope")
	if err == nil || !strings.Contains(err.Error(), "unknown service") {
		t.Fatalf("status of unknown service: %v", err)
	}
	if err := ctl(t, admin, nil, "frobnicate"); err == nil {
		t.Fatal("unknown ctl command accepted")
	}
	if err := ctl(t, admin, nil, "status"); err == nil {
		t.Fatal("status without a name accepted")
	}

	// reload without a file re-reads the configuration file of hqqd.
	var reloaded []supervisor.Status
	if err := ctl(t, admin, &reloaded, "reload"); err != nil || len(reloaded) != 1 {
		t.Fatalf("reload: %+v, %v", reloaded, err)
	}

	// reload with a file adds a service that is not autostarted.
	exe, _ := os.Executable()
	newPath := writeConfig(t, admin,
		map[string]any{"name": "sleeper", "command": exe, "env": map[string]string{envSleeper: "1"}, "restart": map[string]any{"policy": "never"}, "autostart": true},
		map[string]any{"name": "idle", "command": exe, "env": map[string]string{envSleeper: "1"}, "autostart": false},
	)
	if err := ctl(t, admin, &reloaded, "reload", newPath); err != nil || len(reloaded) != 2 {
		t.Fatalf("reload file: %+v, %v", reloaded, err)
	}
	if err := ctl(t, admin, &st, "status", "idle"); err != nil || st.State != supervisor.StateStopped {
		t.Fatalf("new idle service: %+v, %v", st, err)
	}
}

func TestCtlCannotReachMissingSupervisor(t *testing.T) {
	var buf bytes.Buffer
	err := ctlCommand([]string{"-listener", shortName(), "-timeout", "300ms", "list"}, &buf, &buf)
	if err == nil {
		t.Fatal("ctl succeeded without a supervisor")
	}
}

func TestCheckAndConfigValidation(t *testing.T) {
	admin := shortName()
	path := writeConfig(t, admin)
	var out bytes.Buffer
	if err := checkCommand([]string{"-config", path}, &out); err != nil {
		t.Fatalf("check: %v", err)
	}
	if !strings.Contains(out.String(), "OK") {
		t.Fatalf("check output %q", out.String())
	}

	fine := writeConfig(t, "hqqd-admin", map[string]any{
		"name": "svc", "command": "/bin/true", "listener": "svc-listener",
	})
	if _, err := loadFile(fine); err != nil {
		t.Fatalf("distinct admin and service listeners rejected: %v", err)
	}
	clash := writeConfig(t, "same", map[string]any{
		"name": "svc", "command": "/bin/true", "listener": "same",
	})
	if _, err := loadFile(clash); err == nil || !strings.Contains(err.Error(), "admin listener") {
		t.Fatalf("admin/service listener clash: %v", err)
	}

	bad := filepath.Join(t.TempDir(), "bad.json")
	_ = os.WriteFile(bad, []byte(`{"services":[{"name":"a","command":"x","surprise":true}]}`), 0o600)
	if err := checkCommand([]string{"-config", bad}, &out); err == nil {
		t.Fatal("check accepted an unknown field")
	}
	perm := filepath.Join(t.TempDir(), "perm.json")
	_ = os.WriteFile(perm, []byte(`{"admin":{"perm":"rw"},"services":[]}`), 0o600)
	if err := checkCommand([]string{"-config", perm}, &out); err == nil {
		t.Fatal("check accepted a bad admin.perm")
	}
}

func TestUnknownCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run([]string{"bogus"}, &out, &errOut); err == nil {
		t.Fatal("unknown command accepted")
	}
	out.Reset()
	if err := run([]string{"help"}, &out, &errOut); err != nil || !strings.Contains(out.String(), "ctl") {
		t.Fatalf("help: %v %q", err, out.String())
	}
}
