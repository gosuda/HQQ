//go:build cgo

package bench

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	hqq "gosuda.org/hqq"
	"gosuda.org/hqq/bindgen/bench/microbench"
	"gosuda.org/hqq/bindgen/rt"
	"gosuda.org/hqq/launch"
	"gosuda.org/hqq/secureexec"
)

// Environment variables of the harness. README.md lists them.
const (
	envServer    = "HQQ_BENCH_SERVER"     // path of microbench_server; unset: the tests and benchmarks skip
	envWait      = "HQQ_BENCH_WAIT"       // "spin" (default, the pinned policy of 10.5) or "adaptive"
	envEcho      = "HQQ_BENCH_ECHO"       // path of hqq_echo; unset: BenchmarkRoundTrip/echo skips
	envServerCPU = "HQQ_BENCH_SERVER_CPU" // Linux: CPU the server is pinned to with taskset
)

// Session geometry of the default clients, BINDGEN.md 5.5. The "big" geometry makes a 64 KiB request
// and response fit one frame, so BenchmarkXor can set the in-place path against the chunked one.
const (
	defaultBufferCount = 64
	defaultBufferSize  = 16384
	bigBufferCount     = 8
	bigBufferSize      = 256 << 10

	// maxDepth is the deepest pipeline of any benchmark: the default buffer count.
	maxDepth = defaultBufferCount
)

// run state, set once by configure before any test runs.
var (
	waitName   = "spin"
	waitPolicy = hqq.WaitSpin
	serverPath string
	serverHash secureexec.Hash
	echoPath   string
	serverCPU  string
)

// clientCfg selects a session: the server flavour and the geometry and timeouts the client asks for.
// The zero value is the default session of 5.5 against a zero-copy server.
type clientCfg struct {
	copyRequest             bool // server started with --copy-request (7.2)
	bufferCount, bufferSize int  // 0: the defaults of 5.5
	callTimeout             time.Duration
}

var (
	harnessMu sync.Mutex
	servers   = map[bool]*launch.Server{}          // by copyRequest
	clients   = map[clientCfg]*microbench.Client{} // shared by the tests and benchmarks of one binary
)

func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	if err := configure(); err != nil {
		fmt.Fprintln(os.Stderr, "bench:", err)
		return 2
	}
	defer shutdown()
	return m.Run()
}

// configure reads the environment. The key: value lines it prints are benchfmt configuration, so
// benchstat can tell a spin run from an adaptive run (README.md).
func configure() error {
	switch w := strings.ToLower(os.Getenv(envWait)); w {
	case "", "spin":
		waitName, waitPolicy = "spin", hqq.WaitSpin
	case "adaptive":
		waitName, waitPolicy = "adaptive", hqq.WaitAdaptive
	default:
		return fmt.Errorf("%s=%q: want spin or adaptive", envWait, w)
	}
	// The library reads ML_TRACE at its first call and then appends a line per call, which would
	// time a file write. Neither the in-process library nor the server may see it.
	if err := os.Unsetenv("ML_TRACE"); err != nil {
		return err
	}
	echoPath = os.Getenv(envEcho)
	serverCPU = os.Getenv(envServerCPU)
	if serverCPU != "" {
		if runtime.GOOS != "linux" {
			return fmt.Errorf("%s needs taskset and is supported on Linux only", envServerCPU)
		}
		if _, err := strconv.Atoi(serverCPU); err != nil {
			return fmt.Errorf("%s=%q: want a CPU number", envServerCPU, serverCPU)
		}
	}
	serverPath = os.Getenv(envServer)
	if serverPath != "" {
		h, _, err := secureexec.HashFile(serverPath)
		if err != nil {
			return fmt.Errorf("%s: %w", envServer, err)
		}
		serverHash = h
	}
	fmt.Printf("hqq-wait: %s\n", waitName)
	if serverPath != "" {
		fmt.Printf("hqq-server-sha256: %s\n", serverHash)
	}
	if serverCPU != "" {
		fmt.Printf("hqq-server-cpu: %s\n", serverCPU)
	}
	return nil
}

// shutdown closes every client, then every server.
func shutdown() {
	harnessMu.Lock()
	defer harnessMu.Unlock()
	for cfg, c := range clients {
		if err := c.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "bench: close client %+v: %v\n", cfg, err)
		}
	}
	clear(clients)
	for copyReq, s := range servers {
		if err := s.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "bench: stop server (copy-request %v): %v\n", copyReq, err)
		}
	}
	clear(servers)
}

// requireServer skips when the server binary is not configured.
func requireServer(tb testing.TB) {
	tb.Helper()
	if serverPath == "" {
		tb.Skipf("%s is not set: build bindgen/bench/server and point it at microbench_server (README.md)", envServer)
	}
}

// server returns the benchmark server of the given flavour, started on first use with launch.Start
// and stopped by shutdown. harnessMu is held.
func server(tb testing.TB, copyRequest bool) *launch.Server {
	tb.Helper()
	if s, ok := servers[copyRequest]; ok {
		return s
	}
	name, err := launch.NewListenerName("mb")
	if err != nil {
		tb.Fatalf("listener name: %v", err)
	}
	args := []string{name}
	if waitPolicy == hqq.WaitSpin {
		args = append(args, "--spin")
	}
	if copyRequest {
		args = append(args, "--copy-request")
	}
	var stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s, err := launch.Start(ctx, launch.Config{
		Binary:   serverPath,
		SHA256:   serverHash,
		Listener: name,
		Args:     args,
		Env:      serverEnv(),
		Stderr:   &stderr,
	})
	if err != nil {
		tb.Fatalf("start %s %s: %v\n%s", serverPath, strings.Join(args, " "), err, stderr.String())
	}
	if serverCPU != "" {
		out, err := exec.Command("taskset", "-a", "-p", "-c", serverCPU, strconv.Itoa(s.PID())).CombinedOutput()
		if err != nil {
			_ = s.Close()
			tb.Fatalf("taskset -a -p -c %s %d: %v\n%s", serverCPU, s.PID(), err, out)
		}
	}
	servers[copyRequest] = s
	return s
}

// serverEnv is the environment of the server: ours without ML_TRACE.
func serverEnv() []string {
	return slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, "ML_TRACE=")
	})
}

// client returns the shared session for cfg, dialed on first use. The pipelined benchmarks drain
// every call they start, and a leaf that fails leaves the session usable: see syncAfter.
func client(tb testing.TB, cfg clientCfg) *microbench.Client {
	tb.Helper()
	requireServer(tb)
	harnessMu.Lock()
	defer harnessMu.Unlock()
	if c, ok := clients[cfg]; ok {
		return c
	}
	srv := server(tb, cfg.copyRequest)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := microbench.Dial(ctx, srv.Listener(), rt.Options{
		BufferCount: cfg.bufferCount,
		BufferSize:  cfg.bufferSize,
		CallTimeout: cfg.callTimeout,
		Link:        hqq.LinkOptions{WaitPolicy: waitPolicy},
	})
	if err != nil {
		tb.Fatalf("dial %s (%+v): %v", srv.Listener(), cfg, err)
	}
	clients[cfg] = c
	return c
}

// syncAfter makes the end of tb wait for every call a failed test or benchmark left unanswered, so
// that the next user of the shared session starts clean.
func syncAfter(tb testing.TB, c *microbench.Client) {
	tb.Helper()
	tb.Cleanup(func() {
		if err := c.Conn().Sync(); err != nil {
			tb.Errorf("Sync after %s: %v", tb.Name(), err)
		}
	})
}

// echoLink starts hqq_echo as a server on a fresh named Standard link and returns the client end
// of that link, opened the way rt opens a session link: with the benchmark wait policy and a 2 s PeerTimeout.
// The echo process ends, and the segment goes, when tb is done.
func echoLink(tb testing.TB) *hqq.NamedStandardLink {
	tb.Helper()
	if echoPath == "" {
		tb.Skipf("%s is not set: point it at the hqq_echo of the c/ build (README.md)", envEcho)
	}
	name, err := launch.NewListenerName("mbe")
	if err != nil {
		tb.Fatalf("segment name: %v", err)
	}
	args := []string{
		"--create", name, "--server",
		"--count", strconv.Itoa(defaultBufferCount), "--size", strconv.Itoa(defaultBufferSize),
		"--wait", waitName,
	}
	var out bytes.Buffer
	cmd := exec.Command(echoPath, args...)
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		tb.Fatalf("start %s %s: %v", echoPath, strings.Join(args, " "), err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	var link *hqq.NamedStandardLink
	deadline := time.Now().Add(10 * time.Second)
	for {
		link, err = hqq.OpenStandardLink(name, defaultBufferCount, defaultBufferSize,
			hqq.LinkOptions{WaitPolicy: waitPolicy, PeerTimeout: 2 * time.Second})
		if err == nil {
			break
		}
		select {
		case werr := <-exited:
			tb.Fatalf("%s exited before the link opened: %v\n%s", echoPath, werr, out.String())
		default:
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			<-exited
			tb.Fatalf("open echo link %s: %v\n%s", name, err, out.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	tb.Cleanup(func() {
		// hqq_echo ends its loop on the 3-byte END marker.
		if w, err := link.ReserveWrite(); err == nil {
			copy(w.Buffer(), "END")
			err = w.Commit(3)
			if err != nil {
				tb.Errorf("echo END: %v", err)
			}
		} else {
			tb.Errorf("echo END reserve: %v", err)
		}
		select {
		case err := <-exited:
			if err != nil {
				tb.Errorf("%s: %v\n%s", echoPath, err, out.String())
			}
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-exited
			tb.Errorf("%s did not stop after END\n%s", echoPath, out.String())
		}
		if err := link.Close(); err != nil && !errors.Is(err, hqq.ErrClosed) {
			tb.Errorf("close echo link: %v", err)
		}
	})
	return link
}
