package rt

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	hqq "gosuda.org/hqq"
)

const helperEnv = "HQQ_RT_TEST_HELPER"

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) != "" {
		runHelperServer()
		return
	}
	os.Exit(m.Run())
}

// helperTB is the tbLike of the helper process.
type helperTB struct{}

func (helperTB) Helper()                   {}
func (helperTB) Cleanup(func())            {}
func (helperTB) Errorf(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) }
func (helperTB) Fatalf(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...); os.Exit(2) }

// runHelperServer is the server process of TestPeerDeath: it answers Add and
// never answers Hash.
func runHelperServer() {
	svc := startService(helperTB{}, serviceOpts{
		digest: microIface.Digest,
		handle: func(req fakeReq) fakeReply {
			if req.op == opHash {
				return fakeReply{none: true}
			}
			return microHandler(req)
		},
	})
	fmt.Printf("ready %s\n", svc.name)
	select {}
}

func TestPeerDeath(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), helperEnv+"=1")
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var name string
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if name != "" {
			_ = hqq.Unlink(name)
		}
	})
	lines := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(out).ReadString('\n')
		lines <- line
	}()
	select {
	case line := <-lines:
		name = strings.TrimSpace(strings.TrimPrefix(line, "ready "))
		if name == "" || !strings.HasPrefix(line, "ready ") {
			t.Fatalf("helper said %q", line)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("helper did not start")
	}

	c, err := Dial(context.Background(), name, microIface, dialOpts())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	cl := &stubClient{c}
	if v, err := cl.Add(1, 2); err != nil || v != 3 {
		t.Fatalf("Add = %d, %v", v, err)
	}

	f1, _ := cl.AddAsync(10, 1)
	f2, _ := cl.AddAsync(20, 1)
	hash := func() *Call {
		e, err := c.BeginRequest(opHash, StrWire(1), 8)
		if err != nil {
			t.Fatal(err)
		}
		PutStr(e.Body(), 0, "x")
		call, err := c.Submit(e)
		if err != nil {
			t.Fatal(err)
		}
		return call
	}
	h1, h2 := hash(), hash()
	time.Sleep(150 * time.Millisecond) // the server answers f1 and f2, then stalls on Hash
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()

	// Responses published before the death are still delivered.
	if v, err := f1.Wait(); err != nil || v != 11 {
		t.Fatalf("answered call 1 = %d, %v", v, err)
	}
	if v, err := f2.Wait(); err != nil || v != 21 {
		t.Fatalf("answered call 2 = %d, %v", v, err)
	}
	start := time.Now()
	_, err = c.Wait(h1, h1.ID())
	var te *TransportError
	if !errors.As(err, &te) || !errors.Is(err, hqq.ErrPeerGone) || !te.Unknown || !errors.Is(err, ErrOutcomeUnknown) || te.Func != "ml_hash" {
		t.Fatalf("unanswered call = %v, want ErrPeerGone with unknown outcome", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("death detected after %v", d)
	}
	if _, err := c.Wait(h2, h2.ID()); !errors.Is(err, hqq.ErrPeerGone) || !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("second unanswered call = %v", err)
	}
	if _, err := cl.Add(1, 1); !errors.Is(err, ErrSessionBroken) || errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("call on the broken session = %v", err)
	}
	if !errors.Is(c.Err(), hqq.ErrPeerGone) {
		t.Errorf("Err = %v", c.Err())
	}
	_ = c.Close()
}
