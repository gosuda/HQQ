package interop

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"gosuda.org/hqq"
)

// envAdvEcho names the built c/tools hqq_adv_echo executable that
// BenchmarkAdvancedCallCServer starts. Without it the benchmark is skipped.
const envAdvEcho = "HQQ_ADV_ECHO"

// BenchmarkAdvancedCallCServer measures a sequential 64-byte Call from the Go
// AdvancedLink to a C server that echoes it with hqq_adv_serve, as ns/op plus
// the p50 and p99 of the individual calls. Build the server with
//
//	cmake -S c -B /tmp/hqqc -G Ninja -DCMAKE_BUILD_TYPE=Release && cmake --build /tmp/hqqc
//	HQQ_ADV_ECHO=/tmp/hqqc/tools/hqq_adv_echo go test ./interop -run '^$' -bench AdvancedCallCServer
//
// The wait policy of both ends is varied; the in-process Go Serve pair
// (BenchmarkAdvancedLinkRequestResponse in the module root) is the reference.
func BenchmarkAdvancedCallCServer(b *testing.B) {
	echo := os.Getenv(envAdvEcho)
	if echo == "" {
		b.Skipf("%s is not set", envAdvEcho)
	}
	if abs, err := filepath.Abs(echo); err == nil {
		echo = abs
	}
	policies := []struct {
		name   string
		flag   string
		policy hqq.WaitPolicy
	}{
		{"adaptive", "adaptive", hqq.WaitAdaptive},
		{"spin", "spin", hqq.WaitSpin},
	}
	for _, p := range policies {
		b.Run(p.name+"/64B", func(b *testing.B) {
			benchmarkCServerCall(b, echo, p.flag, p.policy, 64)
		})
	}
}

func benchmarkCServerCall(b *testing.B, echo, waitFlag string, policy hqq.WaitPolicy, size int) {
	const (
		bufferCount = 1024
		bufferSize  = 256
	)
	name := fmt.Sprintf("hqq-advbench-%d-%d", os.Getpid(), time.Now().UnixNano()%1000000)
	cmd := exec.Command(echo, name, "--count", fmt.Sprint(bufferCount), "--size", fmt.Sprint(bufferSize), "--wait", waitFlag)
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		b.Fatalf("hqq_adv_echo stdout: %v", err)
	}
	if err := cmd.Start(); err != nil {
		b.Fatalf("start %s: %v", echo, err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	stopServer := func() {
		_ = cmd.Process.Kill()
		<-exited
	}
	defer stopServer()

	ready := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(out).ReadString('\n')
		ready <- strings.TrimSpace(line)
	}()
	select {
	case line := <-ready:
		if line != "ready" {
			b.Fatalf("hqq_adv_echo printed %q, want ready", line)
		}
	case <-time.After(10 * time.Second):
		b.Fatalf("hqq_adv_echo did not report ready")
	}

	link, err := hqq.OpenAdvancedLink(name, bufferCount, bufferSize, hqq.AdvancedOptions{Link: hqq.LinkOptions{WaitPolicy: policy}})
	if err != nil {
		b.Fatalf("OpenAdvancedLink(%q): %v", name, err)
	}
	defer func() {
		if err := link.Close(); err != nil {
			b.Errorf("Close: %v", err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := link.Negotiate(ctx, hqq.FeatureNone); err != nil {
		b.Fatalf("Negotiate: %v", err)
	}

	request := make([]byte, size)
	for i := range request {
		request[i] = byte(i)
	}
	call := func() {
		reply, err := link.Call(ctx, 7, request)
		if err != nil {
			b.Fatalf("Call: %v", err)
		}
		if reply.Len() != size {
			b.Fatalf("response length = %d, want %d", reply.Len(), size)
		}
		reply.Release()
	}
	for range 1000 {
		call()
	}

	samples := make([]time.Duration, 0, b.N)
	b.SetBytes(int64(size) * 2)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		start := time.Now()
		call()
		samples = append(samples, time.Since(start))
	}
	b.StopTimer()
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	b.ReportMetric(float64(samples[len(samples)/2].Nanoseconds()), "p50-ns")
	b.ReportMetric(float64(samples[len(samples)*99/100].Nanoseconds()), "p99-ns")
}
