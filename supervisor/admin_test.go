package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestAdminProtocolRoundTrip(t *testing.T) {
	svc := bareSpec("svc", "silent")
	sv, _ := newTestSupervisor(t, Config{Services: []ServiceSpec{svc}})

	listener := shortName()
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- ServeAdmin(ctx, sv, listener, 0o600, nil) }()
	t.Cleanup(cancel)

	dctx := ctxT(t, 10*time.Second)
	cli, err := DialAdmin(dctx, listener)
	if err != nil {
		t.Fatalf("DialAdmin: %v", err)
	}
	defer cli.Close()

	list, err := cli.List(dctx)
	if err != nil || len(list) != 1 || list[0].Name != "svc" || list[0].State != StateStopped {
		t.Fatalf("List = %+v, %v", list, err)
	}
	st, err := cli.Start(dctx, "svc")
	if err != nil || st.State != StateReady || st.PID == 0 {
		t.Fatalf("Start = %+v, %v", st, err)
	}
	pid := st.PID
	st, err = cli.Restart(dctx, "svc")
	if err != nil || st.State != StateReady || st.PID == pid || st.PID == 0 {
		t.Fatalf("Restart = %+v (old pid %d), %v", st, pid, err)
	}
	if got, err := cli.Status(dctx, "svc"); err != nil || got.PID != st.PID {
		t.Fatalf("Status = %+v, %v", got, err)
	}
	st, err = cli.Stop(dctx, "svc")
	if err != nil || st.State != StateStopped {
		t.Fatalf("Stop = %+v, %v", st, err)
	}

	var ae *AdminError
	if _, err := cli.Status(dctx, "nope"); !errors.As(err, &ae) || ae.Status != AdminStatusNotFound {
		t.Fatalf("unknown service: %v", err)
	}
	if _, err := cli.Start(dctx, ""); !errors.As(err, &ae) || ae.Status != AdminStatusBadRequest {
		t.Fatalf("empty name: %v", err)
	}
	if _, err := cli.Reload(dctx, nil); !errors.As(err, &ae) || ae.Status != AdminStatusBadRequest {
		t.Fatalf("reload without a file: %v", err)
	}
	if _, err := cli.Reload(dctx, []byte(`{"services":[{"name":"x"}]}`)); !errors.As(err, &ae) || ae.Status != AdminStatusBadRequest {
		t.Fatalf("invalid reload payload: %v", err)
	}

	extra := bareSpec("extra", "silent")
	extra.Autostart = ptrBool(false)
	payload, _ := json.Marshal(Config{Services: []ServiceSpec{svc, extra}})
	services, err := cli.Reload(dctx, payload)
	if err != nil || len(services) != 2 {
		t.Fatalf("Reload = %+v, %v", services, err)
	}

	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("ServeAdmin: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ServeAdmin did not return after cancel")
	}
}

func TestAdminReloadUsesConfigFile(t *testing.T) {
	sv, _ := newTestSupervisor(t, Config{})
	listener := shortName()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var reloads atomic.Int32
	go func() {
		_ = ServeAdmin(ctx, sv, listener, 0, func(context.Context) (Config, error) {
			reloads.Add(1)
			return Config{Services: []ServiceSpec{bareSpec("from-file", "silent")}}, nil
		})
	}()
	dctx := ctxT(t, 10*time.Second)
	cli, err := DialAdmin(dctx, listener)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	services, err := cli.Reload(dctx, nil)
	if err != nil || len(services) != 1 || services[0].Name != "from-file" || reloads.Load() != 1 {
		t.Fatalf("Reload from file = %+v, %v (reloads %d)", services, err, reloads.Load())
	}
}

func TestAdminListenerNameIsNotStolen(t *testing.T) {
	sv, _ := newTestSupervisor(t, Config{})
	listener := shortName()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = ServeAdmin(ctx, sv, listener, 0o600, nil) }()
	dctx := ctxT(t, 10*time.Second)
	cli, err := DialAdmin(dctx, listener)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	// A second admin on the same live name must fail, not take it over.
	if err := ServeAdmin(ctx, sv, listener, 0o600, nil); err == nil {
		t.Fatal("second ServeAdmin on a live name succeeded")
	}
}

func ptrBool(b bool) *bool { return &b }
