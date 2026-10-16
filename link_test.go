package hqq

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"gosuda.org/hqq/internal/mmap"
	"gosuda.org/hqq/internal/mpmc"
	"gosuda.org/hqq/internal/protocol"
	"gosuda.org/hqq/internal/shm"
)

func TestLargeMessageThresholdAPI(t *testing.T) {
	if got := LargeMessageThreshold(64); got != 64 {
		t.Fatalf("LargeMessageThreshold(64) = %d, want 64", got)
	}
	if got := LargeMessageThreshold(7); got != 0 {
		t.Fatalf("LargeMessageThreshold(7) = %d, want 0", got)
	}
	if IsLargeMessage(64, 64) {
		t.Fatal("64-byte payload should still fit in a 64-byte Standard payload buffer")
	}
	if !IsLargeMessage(64, 65) {
		t.Fatal("65-byte payload should be large for a 64-byte Standard payload buffer")
	}

	// Sizing helpers work before any link exists and agree with the
	// configuration of a link built on SizeStandardLink-sized memory.
	bufferCount := 8
	bufferSize := 64
	_, standard, _ := newMemoryPair(t, bufferCount, bufferSize, LinkOptions{})
	if standard.BufferSize() != bufferSize || standard.BufferCount() != bufferCount {
		t.Fatalf("standard sizing = %d/%d", standard.BufferCount(), standard.BufferSize())
	}
	if got := LargeMessageThreshold(standard.BufferSize()); got != bufferSize {
		t.Fatalf("threshold = %d, want %d", got, bufferSize)
	}
	if !IsLargeMessage(standard.BufferSize(), bufferSize+1) {
		t.Fatalf("IsLargeMessage(%d, %d) = false", bufferSize, bufferSize+1)
	}

	primary, _ := newAdvancedPair(t, bufferCount, bufferSize)
	if got := LargeMessageThreshold(primary.BufferSize()); got != bufferSize {
		t.Fatalf("advanced threshold = %d, want %d", got, bufferSize)
	}
}

func TestAdvancedProtocolBitValuesMatchSpec(t *testing.T) {
	if FeatureMessages != 1<<0 {
		t.Fatalf("FeatureMessages = %d, want %d", FeatureMessages, 1<<0)
	}
	if FeatureRequestResponse != 1<<1 {
		t.Fatalf("FeatureRequestResponse = %d, want %d", FeatureRequestResponse, 1<<1)
	}
}

const hqqMultiProcessChildEnv = "HQQ_MULTIPROCESS_CHILD"

func TestHQQMultiProcessChild(t *testing.T) {
	if os.Getenv(hqqMultiProcessChildEnv) != "1" {
		t.Skip("helper for multi-process tests")
	}

	args := multiProcessChildArgs(t)
	if len(args) != 5 {
		t.Fatalf("child args = %q, want role name size bufferCount bufferSize", args)
	}
	role := args[0]
	name := args[1]
	size := mustAtoi(t, args[2])
	bufferCount := mustAtoi(t, args[3])
	bufferSize := mustAtoi(t, args[4])

	switch role {
	case "standard":
		runStandardLinkChildProcess(t, name, size, bufferCount, bufferSize)
	case "advanced":
		runAdvancedLinkChildProcess(t, name, size, bufferCount, bufferSize)
	case "crash", "graceful":
		runLivenessChildProcess(t, role, name, bufferCount, bufferSize)
	default:
		t.Fatalf("unknown child role %q", role)
	}
}

func TestMultiProcessStandardLink(t *testing.T) {
	name, smem, mapped, standard := createNamedStandardLink(t, 16, 128)
	defer cleanupNamedLink(t, smem, mapped, standard, true)

	child, output := startMultiProcessChild(t, "standard", name, int(SizeStandardLink(16, 128)), 16, 128)

	if err := standard.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetDeadline failed: %v", err)
	}
	request := []byte("standard parent to child")
	if n, err := standard.Write(request); err != nil || n != len(request) {
		t.Fatalf("parent Write = %d, %v", n, err)
	}
	buf := make([]byte, 128)
	n, err := standard.Read(buf)
	if err != nil {
		t.Fatalf("parent Read failed: %v\nchild output:\n%s", err, output.String())
	}
	if got, want := string(buf[:n]), "standard child ack"; got != want {
		t.Fatalf("parent Read = %q, want %q", got, want)
	}

	waitMultiProcessChild(t, child, output)
}

func TestMultiProcessAdvancedLink(t *testing.T) {
	const (
		bufferCount = 16
		bufferSize  = 64
	)
	name, smem, mapped, primary := createNamedAdvancedLink(t, bufferCount, bufferSize)
	defer cleanupNamedAdvancedLink(t, smem, mapped, primary, true)

	child, output := startMultiProcessChild(t, "advanced", name, int(SizeStandardLink(bufferCount, bufferSize)), bufferCount, bufferSize)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ok, err := primary.NegotiateProtocol(ctx, ProtocolVersion{Major: 1, Minor: 0}, FeatureMessages|FeatureRequestResponse)
	if err != nil || !ok {
		t.Fatalf("parent negotiation = %v, %v\nchild output:\n%s", ok, err, output.String())
	}

	request := patternedBytes(bufferSize*3+17, 11)
	response, err := primary.Call(ctx, 77, request)
	if err != nil {
		t.Fatalf("parent Call failed: %v\nchild output:\n%s", err, output.String())
	}
	if want := patternedBytes(bufferSize*2+5, 23); !bytes.Equal(response.Bytes(), want) {
		t.Fatalf("response mismatch len=%d want=%d", response.Len(), len(want))
	}
	response.Release()

	_, err = primary.Call(ctx, 78, []byte("refuse"))
	var refusal ResponseError
	if !errors.As(err, &refusal) || refusal.Status != 409 || string(refusal.Payload) != "child refuses" {
		t.Fatalf("parent Call error = %v, want ResponseError{409, child refuses}\nchild output:\n%s", err, output.String())
	}

	large := patternedBytes(bufferSize*4+9, 37)
	if err := primary.Send(ctx, large); err != nil {
		t.Fatalf("parent Send failed: %v\nchild output:\n%s", err, output.String())
	}
	ack, err := primary.Recv(ctx)
	if err != nil {
		t.Fatalf("parent Recv ack failed: %v\nchild output:\n%s", err, output.String())
	}
	if ack.StreamID() != 0 || string(ack.Bytes()) != "advanced child large ack" {
		t.Fatalf("ack = stream %d payload %q", ack.StreamID(), ack.Bytes())
	}
	ack.Release()

	waitMultiProcessChild(t, child, output)
}

// multiProcessCrashExitCode is the exit status of a child that simulates a
// crash (it exits without closing its link).
const multiProcessCrashExitCode = 3

func runLivenessChildProcess(t *testing.T, role, name string, bufferCount, bufferSize int) {
	link, err := OpenStandardLink(name, bufferCount, bufferSize, LinkOptions{HeartbeatInterval: 20 * time.Millisecond})
	if err != nil {
		t.Fatalf("child open: %v", err)
	}
	if _, err := link.Write([]byte("child " + role)); err != nil {
		t.Fatalf("child Write: %v", err)
	}
	if role == "crash" {
		// Keep heartbeating for a moment, then die without Close: no
		// deferred cleanup runs, exactly like a killed process.
		time.Sleep(100 * time.Millisecond)
		os.Exit(multiProcessCrashExitCode)
	}
	if err := link.Close(); err != nil {
		t.Fatalf("child Close: %v", err)
	}
}

func testMultiProcessLiveness(t *testing.T, role string, want error) {
	const bufferCount, bufferSize = 16, 128
	name := newPortableSharedMemoryName()
	parent, err := CreateStandardLink(name, bufferCount, bufferSize, 0o600,
		LinkOptions{HeartbeatInterval: 20 * time.Millisecond, PeerTimeout: 300 * time.Millisecond})
	if err != nil {
		if errors.Is(err, syscall.ENOSYS) {
			t.Skipf("shared memory is not supported on this platform: %v", err)
		}
		t.Fatalf("CreateStandardLink: %v", err)
	}
	defer parent.Close()

	child, output := startMultiProcessChild(t, role, name, int(SizeStandardLink(bufferCount, bufferSize)), bufferCount, bufferSize)
	parent.SetReadDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, bufferSize)
	n, err := parent.Read(buf)
	if err != nil || string(buf[:n]) != "child "+role {
		t.Fatalf("parent Read = %q, %v\nchild output:\n%s", buf[:n], err, output.String())
	}
	start := time.Now()
	if _, err := parent.Read(buf); !errors.Is(err, want) {
		t.Fatalf("parent Read after child %s = %v, want %v\nchild output:\n%s", role, err, want, output.String())
	}
	t.Logf("child %s detected after %v", role, time.Since(start))

	err = child.Wait()
	if role == "crash" {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != multiProcessCrashExitCode {
			t.Fatalf("crash child exit = %v, want status %d\nchild output:\n%s", err, multiProcessCrashExitCode, output.String())
		}
	} else if err != nil {
		t.Fatalf("child failed: %v\nchild output:\n%s", err, output.String())
	}
}

func TestMultiProcessPeerCrashIsDetected(t *testing.T) {
	testMultiProcessLiveness(t, "crash", ErrPeerGone)
}

func TestMultiProcessPeerCloseIsDetected(t *testing.T) {
	testMultiProcessLiveness(t, "graceful", io.EOF)
}

func multiProcessChildArgs(t *testing.T) []string {
	t.Helper()
	for i, arg := range os.Args {
		if arg == "--" {
			return os.Args[i+1:]
		}
	}
	t.Fatalf("missing -- child arg separator in %q", os.Args)
	return nil
}

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatalf("Atoi(%q) failed: %v", s, err)
	}
	return n
}

func startMultiProcessChild(t *testing.T, role, name string, size, bufferCount, bufferSize int) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()
	cmd := exec.Command(
		os.Args[0],
		"-test.run=^TestHQQMultiProcessChild$",
		"-test.v",
		"--",
		role,
		name,
		strconv.Itoa(size),
		strconv.Itoa(bufferCount),
		strconv.Itoa(bufferSize),
	)
	cmd.Env = append(os.Environ(), hqqMultiProcessChildEnv+"=1")
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting child failed: %v", err)
	}
	return cmd, &output
}

func waitMultiProcessChild(t *testing.T, cmd *exec.Cmd, output *bytes.Buffer) {
	t.Helper()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("child failed: %v\nchild output:\n%s", err, output.String())
	}
}

func createNamedStandardLink(t *testing.T, bufferCount, bufferSize int) (string, *shm.SharedMemory, []byte, *StandardLink) {
	t.Helper()
	name := newPortableSharedMemoryName()
	size := int(SizeStandardLink(bufferCount, bufferSize))
	smem, mapped := mapNamedSharedMemory(t, name, size, os.O_RDWR|os.O_CREATE|os.O_EXCL)
	link, err := NewStandardLink(mapped, bufferCount, bufferSize, LinkOptions{})
	if err != nil {
		_ = mmap.UnMap(mapped)
		_ = smem.Close()
		_ = smem.Delete()
		t.Fatalf("OpenStandardLink failed: %v", err)
	}
	return name, smem, mapped, link
}

func createNamedAdvancedLink(t *testing.T, bufferCount, bufferSize int) (string, *shm.SharedMemory, []byte, *AdvancedLink) {
	t.Helper()
	name := newPortableSharedMemoryName()
	size := int(SizeStandardLink(bufferCount, bufferSize))
	smem, mapped := mapNamedSharedMemory(t, name, size, os.O_RDWR|os.O_CREATE|os.O_EXCL)
	link, err := NewAdvancedLink(mapped, bufferCount, bufferSize, AdvancedOptions{})
	if err != nil {
		_ = mmap.UnMap(mapped)
		_ = smem.Close()
		_ = smem.Delete()
		t.Fatalf("NewAdvancedLink failed: %v", err)
	}
	return name, smem, mapped, link
}

// sharedMemoryNameSeq makes names unique within the test process: the wall
// clock alone is too coarse on some platforms (Windows), and two tests that
// got the same name would collide on objects a failing test left open.
var sharedMemoryNameSeq atomic.Uint32

func newPortableSharedMemoryName() string {
	// macOS keeps POSIX shm names short. Keep the name comfortably below
	// common 31-byte limits while preserving a leading slash for shm_open.
	pidPart := strconv.FormatInt(int64(os.Getpid()&0xfff), 36)
	seqPart := strconv.FormatInt(int64(sharedMemoryNameSeq.Add(1)), 36)
	timePart := strconv.FormatInt(time.Now().UnixNano()&0xffffff, 36)
	return "/hq" + pidPart + "-" + seqPart + "-" + timePart
}

func openNamedStandardLink(t *testing.T, name string, size, bufferCount, bufferSize int) (*shm.SharedMemory, []byte, *StandardLink) {
	t.Helper()
	smem, mapped := mapNamedSharedMemory(t, name, size, os.O_RDWR)
	link, err := NewStandardLink(mapped, bufferCount, bufferSize, LinkOptions{})
	if err != nil {
		_ = mmap.UnMap(mapped)
		_ = smem.Close()
		t.Fatalf("OpenStandardLink child failed: %v", err)
	}
	return smem, mapped, link
}

func openNamedAdvancedLink(t *testing.T, name string, size, bufferCount, bufferSize int) (*shm.SharedMemory, []byte, *AdvancedLink) {
	t.Helper()
	smem, mapped := mapNamedSharedMemory(t, name, size, os.O_RDWR)
	link, err := NewAdvancedLink(mapped, bufferCount, bufferSize, AdvancedOptions{})
	if err != nil {
		_ = mmap.UnMap(mapped)
		_ = smem.Close()
		t.Fatalf("NewAdvancedLink child failed: %v", err)
	}
	return smem, mapped, link
}

func mapNamedSharedMemory(t *testing.T, name string, size int, flags int) (*shm.SharedMemory, []byte) {
	t.Helper()
	smem, err := shm.OpenSharedMemory(name, size, flags, 0600)
	if err != nil {
		if errors.Is(err, syscall.ENOSYS) {
			t.Skipf("shared memory is not supported on this platform: %v", err)
		}
		t.Fatalf("OpenSharedMemory(%q) failed: %v", name, err)
	}
	mapped, err := mmap.Map(smem.FD(), 0, size, mmap.PROT_READ|mmap.PROT_WRITE, mmap.MAP_SHARED)
	if err != nil {
		_ = smem.Close()
		if flags&os.O_CREATE != 0 {
			_ = smem.Delete()
		}
		t.Fatalf("mmap.Map(%q) failed: %v", name, err)
	}
	return smem, mapped
}

func cleanupNamedLink(t *testing.T, smem *shm.SharedMemory, mapped []byte, link *StandardLink, remove bool) {
	t.Helper()
	if link != nil {
		_ = link.Close()
	}
	if mapped != nil {
		if err := mmap.UnMap(mapped); err != nil {
			t.Fatalf("mmap.UnMap failed: %v", err)
		}
	}
	if smem != nil {
		if err := smem.Close(); err != nil {
			t.Fatalf("shared memory close failed: %v", err)
		}
		if remove {
			if err := smem.Delete(); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("shared memory delete failed: %v", err)
			}
		}
	}
}

func cleanupNamedAdvancedLink(t *testing.T, smem *shm.SharedMemory, mapped []byte, link *AdvancedLink, remove bool) {
	t.Helper()
	if link != nil {
		_ = link.Close()
	}
	if mapped != nil {
		if err := mmap.UnMap(mapped); err != nil {
			t.Fatalf("mmap.UnMap failed: %v", err)
		}
	}
	if smem != nil {
		if err := smem.Close(); err != nil {
			t.Fatalf("shared memory close failed: %v", err)
		}
		if remove {
			if err := smem.Delete(); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("shared memory delete failed: %v", err)
			}
		}
	}
}

func runStandardLinkChildProcess(t *testing.T, name string, size, bufferCount, bufferSize int) {
	smem, mapped, link := openNamedStandardLink(t, name, size, bufferCount, bufferSize)
	defer cleanupNamedLink(t, smem, mapped, link, false)

	if err := link.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("child SetDeadline failed: %v", err)
	}
	buf := make([]byte, bufferSize)
	n, err := link.Read(buf)
	if err != nil {
		t.Fatalf("child Read failed: %v", err)
	}
	if got, want := string(buf[:n]), "standard parent to child"; got != want {
		t.Fatalf("child Read = %q, want %q", got, want)
	}
	reply := []byte("standard child ack")
	if n, err := link.Write(reply); err != nil || n != len(reply) {
		t.Fatalf("child Write = %d, %v", n, err)
	}
}

func runAdvancedLinkChildProcess(t *testing.T, name string, size, bufferCount, bufferSize int) {
	smem, mapped, link := openNamedAdvancedLink(t, name, size, bufferCount, bufferSize)
	defer cleanupNamedAdvancedLink(t, smem, mapped, link, false)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ok, err := link.WaitForNegotiation(ctx)
	if err != nil || !ok {
		t.Fatalf("child WaitForNegotiation = %v, %v", ok, err)
	}

	mux := NewMux()
	mux.HandleFunc(77, func(_ context.Context, w *ResponseWriter, req *Request) {
		if req.StreamID() != 0 {
			t.Errorf("child request stream = %d, want 0", req.StreamID())
		}
		if want := patternedBytes(bufferSize*3+17, 11); !bytes.Equal(req.Payload(), want) {
			t.Errorf("child request mismatch len=%d want=%d", len(req.Payload()), len(want))
		}
		if _, err := w.Write(patternedBytes(bufferSize*2+5, 23)); err != nil {
			t.Errorf("child response Write failed: %v", err)
		}
	})
	mux.HandleFunc(78, func(_ context.Context, w *ResponseWriter, _ *Request) {
		w.WriteError(409, []byte("child refuses"))
	})
	serveCtx, stopServe := context.WithCancel(ctx)
	serveDone := make(chan error, 1)
	go func() { serveDone <- link.Serve(serveCtx, mux) }()
	defer func() {
		stopServe()
		<-serveDone
	}()

	large, err := link.Recv(ctx)
	if err != nil {
		t.Fatalf("child Recv large failed: %v", err)
	}
	if large.StreamID() != 0 {
		t.Fatalf("child large stream = %d, want 0", large.StreamID())
	}
	if want := patternedBytes(bufferSize*4+9, 37); !bytes.Equal(large.Bytes(), want) {
		t.Fatalf("child large mismatch len=%d want=%d", large.Len(), len(want))
	}
	large.Release()
	if err := link.Send(ctx, []byte("advanced child large ack")); err != nil {
		t.Fatalf("child Send ack failed: %v", err)
	}
}

func patternedBytes(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(int(seed) + i*31)
	}
	return b
}

// TestStandardLinkCreation tests the creation of standard links
// It verifies that primary and secondary links can be created correctly
func TestStandardLinkCreation(t *testing.T) {
	bufferCount := 1024
	bufferSize := 4096

	// Calculate required memory size
	if SizeStandardLink(bufferCount, bufferSize) == 0 {
		t.Fatal("SizeStandardLink returned 0")
	}

	mem := AlignedBuffer(SizeStandardLink(bufferCount, bufferSize))

	// Create primary link
	primary, err := NewStandardLink(mem, bufferCount, bufferSize, LinkOptions{})
	if err != nil {
		t.Fatalf("Failed to create primary link: %v", err)
	}
	defer primary.Close()

	if primary.Mode() != LinkModePrimary {
		t.Error("Primary link should be in primary mode")
	}

	if primary.Type() != LinkTypeStandard {
		t.Error("Link should be of type Standard")
	}

	// Create secondary link
	secondary, err := NewStandardLink(mem, bufferCount, bufferSize, LinkOptions{})
	if err != nil {
		t.Fatalf("Failed to create secondary link: %v", err)
	}
	defer secondary.Close()

	if secondary.Mode() != LinkModeSecondary {
		t.Error("Secondary link should be in secondary mode")
	}
}

// TestStandardLinkReadWrite tests basic read/write functionality
// It verifies that data can be transferred between primary and secondary links
func TestStandardLinkReadWrite(t *testing.T) {
	bufferCount := 1024
	bufferSize := 4096

	_, primary, secondary := newMemoryPair(t, bufferCount, bufferSize, LinkOptions{})

	// Test data
	testData := []byte("Hello, HQQ!")

	// Write from primary
	n, err := primary.Write(testData)
	if err != nil {
		t.Fatalf("Failed to write data: %v", err)
	}
	if n != len(testData) {
		t.Errorf("Expected to write %d bytes, wrote %d", len(testData), n)
	}

	// Read from secondary
	readBuffer := make([]byte, len(testData))
	n, err = secondary.Read(readBuffer)
	if err != nil {
		t.Fatalf("Failed to read data: %v", err)
	}
	if n != len(testData) {
		t.Errorf("Expected to read %d bytes, read %d", len(testData), n)
	}

	if string(readBuffer) != string(testData) {
		t.Errorf("Data mismatch: expected %s, got %s", string(testData), string(readBuffer))
	}
}

func TestStandardLinkBidirectionalReadWrite(t *testing.T) {
	bufferCount := 16
	bufferSize := 128
	_, primary, secondary := newMemoryPair(t, bufferCount, bufferSize, LinkOptions{})

	fromPrimary := []byte("primary to secondary")
	fromSecondary := []byte("secondary to primary")

	if n, err := primary.Write(fromPrimary); err != nil || n != len(fromPrimary) {
		t.Fatalf("primary.Write() = %d, %v", n, err)
	}
	if n, err := secondary.Write(fromSecondary); err != nil || n != len(fromSecondary) {
		t.Fatalf("secondary.Write() = %d, %v", n, err)
	}

	buf := make([]byte, bufferSize)
	n, err := secondary.Read(buf)
	if err != nil {
		t.Fatalf("secondary.Read() failed: %v", err)
	}
	if got := string(buf[:n]); got != string(fromPrimary) {
		t.Fatalf("secondary received %q, want %q", got, fromPrimary)
	}

	n, err = primary.Read(buf)
	if err != nil {
		t.Fatalf("primary.Read() failed: %v", err)
	}
	if got := string(buf[:n]); got != string(fromSecondary) {
		t.Fatalf("primary received %q, want %q", got, fromSecondary)
	}
}

func TestStandardLinkDoesNotOverwriteUnreadBuffers(t *testing.T) {
	bufferCount := 4
	bufferSize := 16
	_, primary, secondary := newMemoryPair(t, bufferCount, bufferSize, LinkOptions{})

	messages := [][]byte{
		[]byte("AAAA0001"),
		[]byte("BBBB0002"),
		[]byte("CCCC0003"),
		[]byte("DDDD0004"),
	}

	for _, msg := range messages {
		n, err := primary.Write(msg)
		if err != nil {
			t.Fatalf("Write(%q) failed: %v", msg, err)
		}
		if n != len(msg) {
			t.Fatalf("Write(%q) = %d, want %d", msg, n, len(msg))
		}
	}

	buf := make([]byte, bufferSize)
	for i, want := range messages {
		n, err := secondary.Read(buf)
		if err != nil {
			t.Fatalf("Read(%d) failed: %v", i, err)
		}
		if got := string(buf[:n]); got != string(want) {
			t.Fatalf("Read(%d) = %q, want %q", i, got, want)
		}
	}
}

func TestStandardLinkPartialReadPreservesRemainderAndReleasesBuffer(t *testing.T) {
	bufferCount := 2
	bufferSize := 16
	_, primary, secondary := newMemoryPair(t, bufferCount, bufferSize, LinkOptions{})

	first := []byte("0123456789ABCDEF")
	second := []byte("fedcba9876543210")

	if n, err := primary.Write(first); err != nil || n != len(first) {
		t.Fatalf("first Write() = %d, %v", n, err)
	}

	small := make([]byte, 5)
	n, err := secondary.Read(small)
	if err != nil {
		t.Fatalf("partial Read() failed: %v", err)
	}
	if got := string(small[:n]); got != "01234" {
		t.Fatalf("partial Read() = %q, want %q", got, "01234")
	}

	// The first ring slot should already be released even though the unread
	// tail is served from receiveBuffer.
	if n, err := primary.Write(second); err != nil || n != len(second) {
		t.Fatalf("second Write() = %d, %v", n, err)
	}

	tail := make([]byte, len(first)-5)
	n, err = secondary.Read(tail)
	if err != nil {
		t.Fatalf("tail Read() failed: %v", err)
	}
	if got := string(tail[:n]); got != "56789ABCDEF" {
		t.Fatalf("tail Read() = %q, want %q", got, "56789ABCDEF")
	}

	full := make([]byte, len(second))
	n, err = secondary.Read(full)
	if err != nil {
		t.Fatalf("second Read() failed: %v", err)
	}
	if got := string(full[:n]); got != string(second) {
		t.Fatalf("second Read() = %q, want %q", got, second)
	}
}

func TestStandardLinkZeroCopyReadWrite(t *testing.T) {
	bufferCount := 8
	bufferSize := 64
	_, primary, secondary := newMemoryPair(t, bufferCount, bufferSize, LinkOptions{})

	payload := []byte("zero-copy payload")
	n, err := primary.WriteZeroCopy(func(buffer []byte) (int, error) {
		return copy(buffer, payload), nil
	})
	if err != nil {
		t.Fatalf("WriteZeroCopy failed: %v", err)
	}
	if n != len(payload) {
		t.Fatalf("WriteZeroCopy size = %d, want %d", n, len(payload))
	}

	n, err = secondary.ReadZeroCopy(func(buffer []byte) error {
		if got := string(buffer); got != string(payload) {
			return fmt.Errorf("ReadZeroCopy payload = %q, want %q", got, payload)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ReadZeroCopy failed: %v", err)
	}
	if n != len(payload) {
		t.Fatalf("ReadZeroCopy size = %d, want %d", n, len(payload))
	}

	regularPayload := []byte("regular write to zero-copy read")
	if n, err := primary.Write(regularPayload); err != nil || n != len(regularPayload) {
		t.Fatalf("Write() = %d, %v", n, err)
	}
	n, err = secondary.ReadZeroCopy(func(buffer []byte) error {
		if got := string(buffer); got != string(regularPayload) {
			return fmt.Errorf("ReadZeroCopy after Write payload = %q, want %q", got, regularPayload)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ReadZeroCopy after Write failed: %v", err)
	}
	if n != len(regularPayload) {
		t.Fatalf("ReadZeroCopy after Write size = %d, want %d", n, len(regularPayload))
	}

	n, err = primary.WriteZeroCopy(func(buffer []byte) (int, error) {
		return copy(buffer, payload), nil
	})
	if err != nil || n != len(payload) {
		t.Fatalf("WriteZeroCopy before Read() = %d, %v", n, err)
	}
	readBuffer := make([]byte, bufferSize)
	n, err = secondary.Read(readBuffer)
	if err != nil {
		t.Fatalf("Read after WriteZeroCopy failed: %v", err)
	}
	if got := string(readBuffer[:n]); got != string(payload) {
		t.Fatalf("Read after WriteZeroCopy = %q, want %q", got, payload)
	}
}

func TestStandardLinkReserveCommitReadRelease(t *testing.T) {
	bufferCount := 8
	bufferSize := 64
	_, primary, secondary := newMemoryPair(t, bufferCount, bufferSize, LinkOptions{})

	payload := []byte("reserve-commit")
	writeReservation, err := primary.ReserveWrite()
	if err != nil {
		t.Fatalf("ReserveWrite failed: %v", err)
	}
	if writeReservation.Slot() >= uint64(bufferCount) {
		t.Fatalf("write slot = %d, want < %d", writeReservation.Slot(), bufferCount)
	}
	n := copy(writeReservation.Buffer(), payload)
	if err := writeReservation.Commit(n); err != nil {
		t.Fatalf("Commit failed: %v", err)
	}
	if err := writeReservation.Commit(n); err != ErrInvalidOp {
		t.Fatalf("second Commit error = %v, want ErrInvalidOp", err)
	}

	readReservation, err := secondary.ReserveRead()
	if err != nil {
		t.Fatalf("ReserveRead failed: %v", err)
	}
	if got := string(readReservation.Buffer()); got != string(payload) {
		t.Fatalf("ReserveRead payload = %q, want %q", got, payload)
	}
	if readReservation.Slot() != writeReservation.Slot() {
		t.Fatalf("read slot = %d, want %d", readReservation.Slot(), writeReservation.Slot())
	}
	if err := readReservation.Release(); err != nil {
		t.Fatalf("Release failed: %v", err)
	}
	if err := readReservation.Release(); err != ErrInvalidOp {
		t.Fatalf("second Release error = %v, want ErrInvalidOp", err)
	}
}

func TestStandardLinkDispatchModesCanMix(t *testing.T) {
	bufferCount := 16
	bufferSize := 64
	_, primary, secondary := newMemoryPair(t, bufferCount, bufferSize, LinkOptions{})

	send := func(mode string, payload []byte) {
		t.Helper()
		switch mode {
		case "Write":
			n, err := primary.Write(payload)
			if err != nil || n != len(payload) {
				t.Fatalf("%s(%q) = %d, %v", mode, payload, n, err)
			}
		case "WriteZeroCopy":
			n, err := primary.WriteZeroCopy(func(buffer []byte) (int, error) {
				return copy(buffer, payload), nil
			})
			if err != nil || n != len(payload) {
				t.Fatalf("%s(%q) = %d, %v", mode, payload, n, err)
			}
		case "ReserveWrite":
			reservation, err := primary.ReserveWrite()
			if err != nil {
				t.Fatalf("%s(%q) failed: %v", mode, payload, err)
			}
			n := copy(reservation.Buffer(), payload)
			if err := reservation.Commit(n); err != nil {
				t.Fatalf("%s(%q).Commit(%d) failed: %v", mode, payload, n, err)
			}
		default:
			t.Fatalf("unknown send mode %q", mode)
		}
	}

	receive := func(mode string) []byte {
		t.Helper()
		switch mode {
		case "Read":
			buffer := make([]byte, bufferSize)
			n, err := secondary.Read(buffer)
			if err != nil {
				t.Fatalf("%s failed: %v", mode, err)
			}
			return append([]byte(nil), buffer[:n]...)
		case "ReadZeroCopy":
			var got []byte
			n, err := secondary.ReadZeroCopy(func(buffer []byte) error {
				got = append([]byte(nil), buffer...)
				return nil
			})
			if err != nil {
				t.Fatalf("%s failed: %v", mode, err)
			}
			if n != len(got) {
				t.Fatalf("%s size = %d, copied %d", mode, n, len(got))
			}
			return got
		case "ReserveRead":
			reservation, err := secondary.ReserveRead()
			if err != nil {
				t.Fatalf("%s failed: %v", mode, err)
			}
			got := append([]byte(nil), reservation.Buffer()...)
			if err := reservation.Release(); err != nil {
				t.Fatalf("%s.Release failed: %v", mode, err)
			}
			return got
		default:
			t.Fatalf("unknown receive mode %q", mode)
			return nil
		}
	}

	for _, tc := range []struct {
		sendMode    string
		receiveMode string
	}{
		{"Write", "Read"},
		{"Write", "ReadZeroCopy"},
		{"Write", "ReserveRead"},
		{"WriteZeroCopy", "Read"},
		{"WriteZeroCopy", "ReadZeroCopy"},
		{"WriteZeroCopy", "ReserveRead"},
		{"ReserveWrite", "Read"},
		{"ReserveWrite", "ReadZeroCopy"},
		{"ReserveWrite", "ReserveRead"},
	} {
		t.Run(tc.sendMode+"To"+tc.receiveMode, func(t *testing.T) {
			payload := []byte(tc.sendMode + "->" + tc.receiveMode)
			send(tc.sendMode, payload)
			if got := receive(tc.receiveMode); string(got) != string(payload) {
				t.Fatalf("%s -> %s payload = %q, want %q", tc.sendMode, tc.receiveMode, got, payload)
			}
		})
	}
}

func TestStandardLinkReserveAbortTombstoneIsSkipped(t *testing.T) {
	bufferCount := 2
	bufferSize := 32
	_, primary, secondary := newMemoryPair(t, bufferCount, bufferSize, LinkOptions{})

	for i := 0; i < bufferCount; i++ {
		reservation, err := primary.ReserveWrite()
		if err != nil {
			t.Fatalf("ReserveWrite(%d) failed: %v", i, err)
		}
		if err := reservation.Abort(ErrCodeInvalidOp); err != nil {
			t.Fatalf("Abort(%d) failed: %v", i, err)
		}
	}

	if err := secondary.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline failed: %v", err)
	}
	if _, err := secondary.Read(make([]byte, bufferSize)); err != ErrTimeout {
		t.Fatalf("Read after tombstones error = %v, want ErrTimeout", err)
	}
	if err := secondary.SetReadDeadline(time.Time{}); err != nil {
		t.Fatalf("reset read deadline failed: %v", err)
	}

	payload := []byte("after tombstone")
	if n, err := primary.Write(payload); err != nil || n != len(payload) {
		t.Fatalf("Write after tombstone = %d, %v", n, err)
	}
	buf := make([]byte, bufferSize)
	n, err := secondary.Read(buf)
	if err != nil {
		t.Fatalf("Read after tombstone failed: %v", err)
	}
	if got := string(buf[:n]); got != string(payload) {
		t.Fatalf("payload = %q, want %q", got, payload)
	}
}

func TestStandardLinkReserveInvalidCommitPublishesTombstone(t *testing.T) {
	bufferCount := 2
	bufferSize := 32
	_, primary, secondary := newMemoryPair(t, bufferCount, bufferSize, LinkOptions{})

	reservation, err := primary.ReserveWrite()
	if err != nil {
		t.Fatalf("ReserveWrite failed: %v", err)
	}
	if err := reservation.Commit(bufferSize + 1); err != ErrBufferOverflow {
		t.Fatalf("oversized Commit error = %v, want ErrBufferOverflow", err)
	}

	payload := []byte("valid")
	if n, err := primary.Write(payload); err != nil || n != len(payload) {
		t.Fatalf("Write after invalid Commit = %d, %v", n, err)
	}

	buf := make([]byte, bufferSize)
	n, err := secondary.Read(buf)
	if err != nil {
		t.Fatalf("Read after invalid Commit failed: %v", err)
	}
	if got := string(buf[:n]); got != string(payload) {
		t.Fatalf("payload = %q, want %q", got, payload)
	}
}

func TestStandardLinkWriteReservationHoldsSlotUntilAbort(t *testing.T) {
	bufferCount := 2
	bufferSize := 32
	_, primary, secondary := newMemoryPair(t, bufferCount, bufferSize, LinkOptions{})

	first, err := primary.ReserveWrite()
	if err != nil {
		t.Fatalf("first ReserveWrite failed: %v", err)
	}
	second, err := primary.ReserveWrite()
	if err != nil {
		t.Fatalf("second ReserveWrite failed: %v", err)
	}

	if err := primary.SetWriteDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatalf("SetWriteDeadline failed: %v", err)
	}
	if _, err := primary.ReserveWrite(); err != ErrTimeout {
		t.Fatalf("third ReserveWrite error = %v, want ErrTimeout", err)
	}
	if err := primary.SetWriteDeadline(time.Time{}); err != nil {
		t.Fatalf("reset write deadline failed: %v", err)
	}

	if err := first.Abort(ErrCodeInvalidOp); err != nil {
		t.Fatalf("first Abort failed: %v", err)
	}
	if err := second.Abort(ErrCodeInvalidOp); err != nil {
		t.Fatalf("second Abort failed: %v", err)
	}

	if err := secondary.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline failed: %v", err)
	}
	if _, err := secondary.ReserveRead(); err != ErrTimeout {
		t.Fatalf("ReserveRead after abort tombstones error = %v, want ErrTimeout", err)
	}
	if err := secondary.SetReadDeadline(time.Time{}); err != nil {
		t.Fatalf("reset read deadline failed: %v", err)
	}

	payload := []byte("after abort")
	if n, err := primary.Write(payload); err != nil || n != len(payload) {
		t.Fatalf("Write after Abort = %d, %v", n, err)
	}
	buf := make([]byte, bufferSize)
	n, err := secondary.Read(buf)
	if err != nil {
		t.Fatalf("Read after Abort failed: %v", err)
	}
	if got := string(buf[:n]); got != string(payload) {
		t.Fatalf("payload = %q, want %q", got, payload)
	}
}

func TestStandardLinkReadReservationHoldsSlotUntilRelease(t *testing.T) {
	bufferCount := 2
	bufferSize := 32
	_, primary, secondary := newMemoryPair(t, bufferCount, bufferSize, LinkOptions{})

	for i := 0; i < bufferCount; i++ {
		payload := []byte{byte('A' + i)}
		if n, err := primary.Write(payload); err != nil || n != len(payload) {
			t.Fatalf("Write(%d) = %d, %v", i, n, err)
		}
	}

	first, err := secondary.ReserveRead()
	if err != nil {
		t.Fatalf("first ReserveRead failed: %v", err)
	}
	second, err := secondary.ReserveRead()
	if err != nil {
		t.Fatalf("second ReserveRead failed: %v", err)
	}

	if err := primary.SetWriteDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatalf("SetWriteDeadline failed: %v", err)
	}
	if _, err := primary.Write([]byte("blocked")); err != ErrTimeout {
		t.Fatalf("Write while read slots held error = %v, want ErrTimeout", err)
	}
	if err := primary.SetWriteDeadline(time.Time{}); err != nil {
		t.Fatalf("reset write deadline failed: %v", err)
	}

	if err := first.Release(); err != nil {
		t.Fatalf("first Release failed: %v", err)
	}
	if err := second.Release(); err != nil {
		t.Fatalf("second Release failed: %v", err)
	}

	payload := []byte("unblocked")
	if n, err := primary.Write(payload); err != nil || n != len(payload) {
		t.Fatalf("Write after Release = %d, %v", n, err)
	}
	buf := make([]byte, bufferSize)
	n, err := secondary.Read(buf)
	if err != nil {
		t.Fatalf("Read after Release failed: %v", err)
	}
	if got := string(buf[:n]); got != string(payload) {
		t.Fatalf("payload = %q, want %q", got, payload)
	}
}

// TestAdvancedLinkCreation tests the creation of advanced links
// It verifies that advanced links can be created with proper initialization
func TestAdvancedLinkCreation(t *testing.T) {
	bufferCount := 1024
	bufferSize := 4096

	advLink := newTestAdvancedLink(t, bufferCount, bufferSize, AdvancedOptions{})

	if advLink.Type() != LinkTypeAdvanced {
		t.Error("Link should be of type Advanced")
	}

	if advLink.ProtocolState() != ProtocolStateNone {
		t.Error("Protocol state should be None initially")
	}

	// Test that connection management is available
	if advLink.listening {
		t.Error("Advanced link should not be listening initially")
	}
}

// TestAdvancedLinkProtocolNegotiation tests protocol negotiation between advanced links
// It verifies that features and versions can be negotiated successfully
func TestAdvancedLinkProtocolNegotiation(t *testing.T) {
	bufferCount := 1024
	bufferSize := 4096

	primary, secondary := newAdvancedPair(t, bufferCount, bufferSize)

	// Test negotiation
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	version := ProtocolVersion{Major: 1, Minor: 0}
	features := FeatureMessages | FeatureRequestResponse

	// Start negotiation in goroutines
	var wg sync.WaitGroup
	wg.Add(2)

	var primaryErr, secondaryErr error
	var primarySuccess, secondarySuccess bool

	// Primary initiates negotiation
	go func() {
		defer wg.Done()
		primarySuccess, primaryErr = primary.NegotiateProtocol(ctx, version, features)
	}()

	// Secondary waits for negotiation
	go func() {
		defer wg.Done()
		secondarySuccess, secondaryErr = secondary.WaitForNegotiation(ctx)
	}()

	// Wait for both to complete
	wg.Wait()

	// Check results
	if primaryErr != nil {
		t.Fatalf("Primary negotiation failed: %v", primaryErr)
	}
	if secondaryErr != nil {
		t.Fatalf("Secondary negotiation failed: %v", secondaryErr)
	}

	if !primarySuccess {
		t.Error("Primary negotiation should have succeeded")
	}
	if !secondarySuccess {
		t.Error("Secondary negotiation should have succeeded")
	}

	// Check protocol states
	if primary.ProtocolState() != ProtocolStateNegotiated {
		t.Errorf("Primary protocol state should be Negotiated, got %v", primary.ProtocolState())
	}
	if secondary.ProtocolState() != ProtocolStateNegotiated {
		t.Errorf("Secondary protocol state should be Negotiated, got %v", secondary.ProtocolState())
	}

	// Check negotiated version
	negotiatedVersion := primary.NegotiatedVersion()
	if negotiatedVersion.Major != 1 || negotiatedVersion.Minor != 0 {
		t.Errorf("Expected version 1.0, got %d.%d", negotiatedVersion.Major, negotiatedVersion.Minor)
	}

	// Check negotiated features
	negotiatedFeatures := primary.NegotiatedFeatures()
	if (negotiatedFeatures & FeatureMessages) == 0 {
		t.Error("messages feature should be negotiated")
	}
	if (negotiatedFeatures & FeatureRequestResponse) == 0 {
		t.Error("request-response feature should be negotiated")
	}

	// Check feature flags
	if primary.NegotiatedFeatures()&FeatureMessages == 0 {
		t.Error("messages should be enabled")
	}
	if primary.NegotiatedFeatures()&FeatureRequestResponse == 0 {
		t.Error("request-response should be enabled")
	}
}

func negotiateAdvancedPair(t *testing.T, primary, secondary *AdvancedLink) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	version := ProtocolVersion{Major: 1, Minor: 0}
	features := FeatureMessages | FeatureRequestResponse

	var wg sync.WaitGroup
	wg.Add(2)

	var primaryErr, secondaryErr error
	var primarySuccess, secondarySuccess bool
	go func() {
		defer wg.Done()
		primarySuccess, primaryErr = primary.NegotiateProtocol(ctx, version, features)
	}()
	go func() {
		defer wg.Done()
		secondarySuccess, secondaryErr = secondary.WaitForNegotiation(ctx)
	}()
	wg.Wait()

	if primaryErr != nil {
		t.Fatalf("primary negotiation failed: %v", primaryErr)
	}
	if secondaryErr != nil {
		t.Fatalf("secondary negotiation failed: %v", secondaryErr)
	}
	if !primarySuccess || !secondarySuccess {
		t.Fatalf("negotiation success = %v/%v, want true/true", primarySuccess, secondarySuccess)
	}
}

func TestAdvancedLinkRequiresNegotiation(t *testing.T) {
	link, _ := newAdvancedPair(t, 8, 32)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := link.Send(ctx, []byte("payload")); !isNotNegotiated(err) {
		t.Fatalf("Send before negotiation = %v, want ErrNotNegotiated", err)
	}
	if _, err := link.Call(ctx, 1, []byte("payload")); !isNotNegotiated(err) {
		t.Fatalf("Call before negotiation = %v, want ErrNotNegotiated", err)
	}
	if _, err := link.Recv(ctx); !isNotNegotiated(err) {
		t.Fatalf("Recv before negotiation = %v, want ErrNotNegotiated", err)
	}
	if err := link.ListenStreams(ctx); !isNotNegotiated(err) {
		t.Fatalf("Listen before negotiation = %v, want ErrNotNegotiated", err)
	}
	if _, err := link.OpenStream(ctx); !isNotNegotiated(err) {
		t.Fatalf("Dial before negotiation = %v, want ErrNotNegotiated", err)
	}
}

// isNotNegotiated reports whether err is ErrNotNegotiated and still matches
// the broader ErrInvalidOp it used to be reported as.
func isNotNegotiated(err error) bool {
	return errors.Is(err, ErrNotNegotiated) && errors.Is(err, ErrInvalidOp)
}

func TestAdvancedLinkMessages(t *testing.T) {
	primary, secondary := newAdvancedPair(t, 8, 16)
	negotiateAdvancedPair(t, primary, secondary)

	payload := make([]byte, 97)
	for i := range payload {
		payload[i] = byte(i*17 + 3)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := primary.Send(ctx, payload); err != nil {
		t.Fatalf("Send failed: %v", err)
	}
	message, err := secondary.Recv(ctx)
	if err != nil {
		t.Fatalf("Recv failed: %v", err)
	}
	if message.StreamID() != 0 {
		t.Fatalf("message stream = %d, want 0", message.StreamID())
	}
	if message.Len() != len(payload) {
		t.Fatalf("message length = %d, want %d", message.Len(), len(payload))
	}
	if string(message.Bytes()) != string(payload) {
		t.Fatalf("payload mismatch: got %x want %x", message.Bytes(), payload)
	}
	message.Release()
}

func TestAdvancedLinkRequestResponse(t *testing.T) {
	primary, secondary := newAdvancedPair(t, 16, 16)
	negotiateAdvancedPair(t, primary, secondary)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	requestPayload := []byte("request payload that spans multiple chunks")
	responsePayload := []byte("response payload that also spans multiple chunks")
	serverErr := make(chan error, 1)
	go secondary.Serve(ctx, HandlerFunc(func(_ context.Context, w *ResponseWriter, req *Request) {
		if req.Method() != 42 {
			serverErr <- fmt.Errorf("method = %d, want 42", req.Method())
			return
		}
		if string(req.Payload()) != string(requestPayload) {
			serverErr <- fmt.Errorf("request payload = %q, want %q", req.Payload(), requestPayload)
			return
		}
		_, err := w.Write(responsePayload)
		serverErr <- err
	}))

	response, err := primary.Call(ctx, 42, requestPayload)
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}
	if string(response.Bytes()) != string(responsePayload) {
		t.Fatalf("response = %q, want %q", response.Bytes(), responsePayload)
	}
	response.Release()
	if err := <-serverErr; err != nil {
		t.Fatalf("server failed: %v", err)
	}
}

func TestAdvancedLinkErrorResponse(t *testing.T) {
	primary, secondary := newAdvancedPair(t, 8, 32)
	negotiateAdvancedPair(t, primary, secondary)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go secondary.Serve(ctx, HandlerFunc(func(_ context.Context, w *ResponseWriter, _ *Request) {
		w.WriteError(404, []byte("missing method"))
	}))

	_, err := primary.Call(ctx, 99, []byte("hello"))
	if err == nil {
		t.Fatal("Call succeeded, want error response")
	}
	responseErr, ok := err.(ResponseError)
	if !ok {
		t.Fatalf("Call error = %T %v, want ResponseError", err, err)
	}
	if responseErr.Status != 404 || string(responseErr.Payload) != "missing method" {
		t.Fatalf("response error = status %d payload %q", responseErr.Status, responseErr.Payload)
	}
}

func TestAdvancedLinkEmptyRequestResponse(t *testing.T) {
	primary, secondary := newAdvancedPair(t, 8, 32)
	negotiateAdvancedPair(t, primary, secondary)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	serverErr := make(chan error, 1)
	go secondary.Serve(ctx, HandlerFunc(func(_ context.Context, _ *ResponseWriter, req *Request) {
		if len(req.Payload()) != 0 {
			serverErr <- fmt.Errorf("request payload=%q", req.Payload())
			return
		}
		serverErr <- nil
	}))

	response, err := primary.Call(ctx, 1, nil)
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}
	if response.Len() != 0 {
		t.Fatalf("response payload = %q, want empty", response.Bytes())
	}
	response.Release()
	if err := <-serverErr; err != nil {
		t.Fatalf("server failed: %v", err)
	}
}

func TestAdvancedLinkConcurrentCallsUsePendingTable(t *testing.T) {
	const requestCount = 16
	client, server := newAdvancedPair(t, 64, 32)
	negotiateAdvancedPair(t, client, server)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// The handlers wait until every request has arrived and then answer in
	// reverse order: request i is answered after request i+1.
	var arrived atomic.Int32
	allArrived := make(chan struct{})
	answered := make([]chan struct{}, requestCount+1)
	for i := range answered {
		answered[i] = make(chan struct{})
	}
	close(answered[requestCount])
	go server.Serve(ctx, HandlerFunc(func(ctx context.Context, w *ResponseWriter, req *Request) {
		method := req.Method()
		if arrived.Add(1) == requestCount {
			close(allArrived)
		}
		select {
		case <-allArrived:
		case <-ctx.Done():
			return
		}
		select {
		case <-answered[method+1]:
		case <-ctx.Done():
			return
		}
		_, _ = w.Write([]byte(fmt.Sprintf("response-%02d", method)))
		close(answered[method])
	}))

	var wg sync.WaitGroup
	errCh := make(chan error, requestCount)
	for i := range requestCount {
		wg.Add(1)
		go func() {
			defer wg.Done()
			request := []byte(fmt.Sprintf("request-%02d", i))
			response, err := client.Call(ctx, uint64(i), request)
			if err != nil {
				errCh <- fmt.Errorf("Call(%d) failed: %w", i, err)
				return
			}
			want := fmt.Sprintf("response-%02d", i)
			if string(response.Bytes()) != want {
				errCh <- fmt.Errorf("Call(%d) response = %q, want %q", i, response.Bytes(), want)
			}
			response.Release()
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestAdvancedLinkLogicalStreamDialAcceptAndCall(t *testing.T) {
	client, server := newAdvancedPair(t, 16, 32)
	negotiateAdvancedPair(t, client, server)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := server.ListenStreams(ctx); err != nil {
		t.Fatalf("ListenStreams failed: %v", err)
	}
	accepted := make(chan *Stream, 1)
	acceptErr := make(chan error, 1)
	go func() {
		stream, err := server.AcceptStream(ctx)
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- stream
	}()

	stream, err := client.OpenStream(ctx)
	if err != nil {
		t.Fatalf("OpenStream failed: %v", err)
	}
	select {
	case got := <-accepted:
		if got.ID() != stream.ID() {
			t.Fatalf("accepted stream = %d, want %d", got.ID(), stream.ID())
		}
	case err := <-acceptErr:
		t.Fatalf("Accept failed: %v", err)
	case <-ctx.Done():
		t.Fatalf("Accept timed out: %v", ctx.Err())
	}

	serverErr := make(chan error, 1)
	go server.Serve(ctx, HandlerFunc(func(_ context.Context, w *ResponseWriter, req *Request) {
		if req.StreamID() != stream.ID() || req.Method() != 9 {
			serverErr <- fmt.Errorf("request stream=%d method=%d", req.StreamID(), req.Method())
			return
		}
		_, err := w.Write([]byte("stream-ok"))
		serverErr <- err
	}))
	response, err := stream.Call(ctx, 9, []byte("stream-ping"))
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}
	if response.StreamID() != stream.ID() || string(response.Bytes()) != "stream-ok" {
		t.Fatalf("response stream=%d payload=%q", response.StreamID(), response.Bytes())
	}
	response.Release()
	if err := <-serverErr; err != nil {
		t.Fatalf("server failed: %v", err)
	}
}

func TestAdvancedLinkMaxMessageSizeGuardsSendAndReceive(t *testing.T) {
	t.Run("send", func(t *testing.T) {
		primary, secondary := newAdvancedPairWithOptions(t, 8, 16, AdvancedOptions{MaxMessageSize: 32}, AdvancedOptions{})
		negotiateAdvancedPair(t, primary, secondary)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := primary.Send(ctx, make([]byte, 33)); err != ErrBufferOverflow {
			t.Fatalf("Send oversized error = %v, want ErrBufferOverflow", err)
		}
	})

	t.Run("receive", func(t *testing.T) {
		primary, secondary := newAdvancedPairWithOptions(t, 16, 16, AdvancedOptions{}, AdvancedOptions{MaxMessageSize: 32})
		negotiateAdvancedPair(t, primary, secondary)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := primary.Send(ctx, make([]byte, 33)); err != nil {
			t.Fatalf("Send failed: %v", err)
		}
		if _, err := secondary.Recv(ctx); err != ErrBufferOverflow {
			t.Fatalf("Recv oversized error = %v, want ErrBufferOverflow", err)
		}
	})
}

// TestAdvancedLinkStatistics tests statistics collection for advanced links
// It verifies that statistics are properly initialized and tracked
func TestAdvancedLinkStatistics(t *testing.T) {
	bufferCount := 1024
	bufferSize := 4096

	advLink := newTestAdvancedLink(t, bufferCount, bufferSize, AdvancedOptions{})

	if got := advLink.Stats(); got != (AdvancedStats{}) {
		t.Errorf("initial Stats = %+v, want zero value", got)
	}
}

// TestStandardLinkErrors tests error handling in standard links
// It verifies that invalid configurations are properly rejected
func TestStandardLinkErrors(t *testing.T) {
	// Test invalid buffer count
	size := SizeStandardLink(1, 4096)
	if size != 0 {
		t.Error("SizeStandardLink should return 0 for invalid buffer count")
	}

	// Test invalid buffer size
	size = SizeStandardLink(1024, 7)
	if size != 0 {
		t.Error("SizeStandardLink should return 0 for invalid buffer size")
	}

	// Test memory alignment error
	mem := AlignedBuffer(4096)
	// Create misalignment by skipping 1 byte
	_, err := NewStandardLink(mem[1:], 1024, 4096, LinkOptions{})
	if err != ErrMemoryAlign {
		t.Errorf("Expected ErrMemoryAlign, got %v", err)
	}

	// Test non-power-of-two buffer count. Standard Protocol requires exact
	// data-ring/buffer symmetry for slot-owned payload transfer.
	size = SizeStandardLink(6, 4096)
	if size != 0 {
		t.Error("SizeStandardLink should return 0 for non-power-of-two buffer count")
	}
}

func TestStandardLinkSuperblockRejectsMismatchedConfig(t *testing.T) {
	mem := AlignedBuffer(SizeStandardLink(8, 64))

	primary, err := NewStandardLink(mem, 8, 64, LinkOptions{})
	if err != nil {
		t.Fatalf("NewStandardLink primary failed: %v", err)
	}
	defer primary.Close()

	var geometryErr *GeometryError
	if _, err := NewStandardLink(mem, 16, 64, LinkOptions{}); !errors.Is(err, ErrInvalidSize) || !errors.As(err, &geometryErr) {
		t.Fatalf("NewStandardLink with mismatched bufferCount = %v, want *GeometryError wrapping ErrInvalidSize", err)
	}
	if _, err := NewStandardLink(mem, 8, 128, LinkOptions{}); !errors.Is(err, ErrInvalidSize) || !errors.As(err, &geometryErr) {
		t.Fatalf("NewStandardLink with mismatched bufferSize = %v, want *GeometryError wrapping ErrInvalidSize", err)
	}
}

func TestNamedStandardLinkAPI(t *testing.T) {
	name := newPortableSharedMemoryName()
	primary, err := CreateStandardLink(name, 8, 64, 0600, LinkOptions{})
	if err != nil {
		if errors.Is(err, syscall.ENOSYS) {
			t.Skipf("shared memory is not supported on this platform: %v", err)
		}
		t.Fatalf("CreateStandardLink failed: %v", err)
	}
	defer primary.Close()

	secondary, err := OpenStandardLink(name, 8, 64, LinkOptions{})
	if err != nil {
		t.Fatalf("OpenStandardLink failed: %v", err)
	}
	defer secondary.Close()

	payload := []byte("named standard link")
	if n, err := primary.Write(payload); err != nil || n != len(payload) {
		t.Fatalf("Write = %d, %v", n, err)
	}
	buf := make([]byte, 64)
	n, err := secondary.Read(buf)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	if string(buf[:n]) != string(payload) {
		t.Fatalf("Read = %q, want %q", buf[:n], payload)
	}
}

func TestStandardLinkDeadlines(t *testing.T) {
	bufferCount := 2
	bufferSize := 64
	_, primary, secondary := newMemoryPair(t, bufferCount, bufferSize, LinkOptions{})

	if err := secondary.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline failed: %v", err)
	}
	if _, err := secondary.Read(make([]byte, 8)); err != ErrTimeout {
		t.Fatalf("Read without data error = %v, want ErrTimeout", err)
	}
	if err := secondary.SetReadDeadline(time.Time{}); err != nil {
		t.Fatalf("reset read deadline failed: %v", err)
	}

	payload := []byte("01234567")
	for i := 0; i < bufferCount; i++ {
		if n, err := primary.Write(payload); err != nil || n != len(payload) {
			t.Fatalf("filling Write(%d) = %d, %v", i, n, err)
		}
	}
	if err := primary.SetWriteDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatalf("SetWriteDeadline failed: %v", err)
	}
	if _, err := primary.Write(payload); err != ErrTimeout {
		t.Fatalf("Write without an available ring slot error = %v, want ErrTimeout", err)
	}
}

func TestStandardLinkHighVolumeOrderedDelivery(t *testing.T) {
	bufferCount := 64
	bufferSize := 64
	messageCount := 4096
	_, primary, secondary := newMemoryPair(t, bufferCount, bufferSize, LinkOptions{})

	errCh := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		payload := make([]byte, bufferSize)
		for i := 0; i < messageCount; i++ {
			binary.LittleEndian.PutUint64(payload[:8], uint64(i))
			for j := 8; j < len(payload); j++ {
				payload[j] = byte(i + j)
			}
			if n, err := primary.Write(payload); err != nil || n != len(payload) {
				errCh <- fmt.Errorf("Write(%d) = %d, %v", i, n, err)
				return
			}
		}
	}()

	go func() {
		defer wg.Done()
		buf := make([]byte, bufferSize)
		for i := 0; i < messageCount; i++ {
			n, err := secondary.Read(buf)
			if err != nil {
				errCh <- fmt.Errorf("Read(%d) failed: %v", i, err)
				return
			}
			if n != bufferSize {
				errCh <- fmt.Errorf("Read(%d) size = %d, want %d", i, n, bufferSize)
				return
			}
			if got := binary.LittleEndian.Uint64(buf[:8]); got != uint64(i) {
				errCh <- fmt.Errorf("Read(%d) sequence = %d", i, got)
				return
			}
		}
	}()

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
}

func TestStandardLinkMultiPProgress(t *testing.T) {
	oldProcs := runtime.GOMAXPROCS(2)
	defer runtime.GOMAXPROCS(oldProcs)

	bufferCount := 64
	bufferSize := 128
	messageCount := 8192
	_, primary, secondary := newMemoryPair(t, bufferCount, bufferSize, LinkOptions{})

	done := make(chan error, 2)
	go func() {
		payload := make([]byte, bufferSize)
		for i := 0; i < messageCount; i++ {
			binary.LittleEndian.PutUint64(payload[:8], uint64(i))
			if n, err := primary.Write(payload); err != nil || n != len(payload) {
				done <- fmt.Errorf("Write(%d) = %d, %v", i, n, err)
				return
			}
		}
		done <- nil
	}()

	go func() {
		buf := make([]byte, bufferSize)
		for i := 0; i < messageCount; i++ {
			n, err := secondary.Read(buf)
			if err != nil {
				done <- fmt.Errorf("Read(%d) failed: %v", i, err)
				return
			}
			if n != bufferSize {
				done <- fmt.Errorf("Read(%d) size = %d, want %d", i, n, bufferSize)
				return
			}
			if got := binary.LittleEndian.Uint64(buf[:8]); got != uint64(i) {
				done <- fmt.Errorf("Read(%d) sequence = %d", i, got)
				return
			}
		}
		done <- nil
	}()

	timeout := time.After(5 * time.Second)
	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-timeout:
			t.Fatal("StandardLink stalled with GOMAXPROCS=2")
		}
	}
}

// TestAdvancedLinkNegotiationTimeout tests timeout handling during negotiation
// It verifies that negotiation properly times out when no peer is present
func TestAdvancedLinkNegotiationTimeout(t *testing.T) {
	bufferCount := 1024
	bufferSize := 4096

	advLink := newTestAdvancedLink(t, bufferCount, bufferSize, AdvancedOptions{})

	// Test negotiation with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	version := ProtocolVersion{Major: 1, Minor: 0}
	features := FeatureMessages

	success, err := advLink.NegotiateProtocol(ctx, version, features)
	if err == nil {
		t.Error("Expected timeout error")
	}
	if success {
		t.Error("Negotiation should not succeed without peer")
	}

	if advLink.ProtocolState() != ProtocolStateFailed {
		t.Errorf("Protocol state should be Failed, got %v", advLink.ProtocolState())
	}
}

// TestConcurrentReadWrite tests concurrent read/write operations
// It verifies that multiple goroutines can safely use the link simultaneously
// and that a failure of either side ends the test promptly: both links carry
// a deadline, and every goroutine reports exactly one result.
func TestConcurrentReadWrite(t *testing.T) {
	bufferCount := 1024
	bufferSize := 4096

	_, primary, secondary := newMemoryPair(t, bufferCount, bufferSize, LinkOptions{})

	const numMessages = 100
	const messageSize = 100

	deadline := time.Now().Add(10 * time.Second)
	if err := primary.SetWriteDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	if err := secondary.SetReadDeadline(deadline); err != nil {
		t.Fatal(err)
	}

	results := make(chan error, 2)

	go func() {
		for i := range numMessages {
			data := bytes.Repeat([]byte{byte(i % 256)}, messageSize)
			n, err := primary.Write(data)
			if err != nil {
				results <- fmt.Errorf("write failed at message %d: %v", i, err)
				return
			}
			if n != messageSize {
				results <- fmt.Errorf("expected to write %d bytes, wrote %d at message %d", messageSize, n, i)
				return
			}
		}
		results <- nil
	}()

	go func() {
		readBuffer := make([]byte, messageSize)
		for i := range numMessages {
			n, err := secondary.Read(readBuffer)
			if err != nil {
				results <- fmt.Errorf("read failed at message %d: %v", i, err)
				return
			}
			if n != messageSize {
				results <- fmt.Errorf("expected to read %d bytes, got %d at message %d", messageSize, n, i)
				return
			}
			expectedByte := byte(i % 256)
			for j, b := range readBuffer {
				if b != expectedByte {
					results <- fmt.Errorf("data mismatch at message %d, byte %d: expected %d, got %d", i, j, expectedByte, b)
					return
				}
			}
		}
		results <- nil
	}()

	for range 2 {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
}

// BenchmarkStandardLinkReadWrite benchmarks the standard link read/write performance
// It measures throughput for basic data transfer operations
func BenchmarkStandardLinkReadWrite(b *testing.B) {
	bufferCount := 1024
	bufferSize := 1 << 12

	_, primary, secondary := newMemoryPair(b, bufferCount, bufferSize, LinkOptions{})

	// Test data
	testData := make([]byte, 1<<12)
	readBuffer := make([]byte, 1<<12)
	for i := range testData {
		testData[i] = byte(i % 256)
	}
	b.SetBytes(int64(len(testData)))

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := primary.Write(testData)
		if err != nil {
			b.Fatalf("Write failed: %v", err)
		}

		_, err = secondary.Read(readBuffer)
		if err != nil {
			b.Fatalf("Read failed: %v", err)
		}
	}
}

func BenchmarkStandardLinkReadWriteSizes(b *testing.B) {
	for _, payloadSize := range []int{8, 64, 4096} {
		b.Run(fmt.Sprintf("%dB", payloadSize), func(b *testing.B) {
			benchmarkStandardLinkReadWrite(b, payloadSize)
		})
	}
}

func benchmarkStandardLinkReadWrite(b *testing.B, payloadSize int) {
	bufferCount := 1024
	bufferSize := payloadSize
	if bufferSize < 8 {
		bufferSize = 8
	}

	_, primary, secondary := newMemoryPair(b, bufferCount, bufferSize, LinkOptions{})

	testData := make([]byte, payloadSize)
	readBuffer := make([]byte, payloadSize)
	for i := range testData {
		testData[i] = byte(i)
	}

	b.SetBytes(int64(payloadSize))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		n, err := primary.Write(testData)
		if err != nil {
			b.Fatalf("Write failed: %v", err)
		}
		if n != len(testData) {
			b.Fatalf("write size = %d, want %d", n, len(testData))
		}

		n, err = secondary.Read(readBuffer)
		if err != nil {
			b.Fatalf("Read failed: %v", err)
		}
		if n != len(testData) {
			b.Fatalf("read size = %d, want %d", n, len(testData))
		}
	}
}

func BenchmarkStandardLinkZeroCopySizes(b *testing.B) {
	for _, payloadSize := range []int{8, 64, 4096} {
		b.Run(fmt.Sprintf("%dB", payloadSize), func(b *testing.B) {
			benchmarkStandardLinkZeroCopy(b, payloadSize)
		})
	}
}

func benchmarkStandardLinkZeroCopy(b *testing.B, payloadSize int) {
	bufferCount := 1024
	bufferSize := payloadSize
	if bufferSize < 8 {
		bufferSize = 8
	}

	_, primary, secondary := newMemoryPair(b, bufferCount, bufferSize, LinkOptions{})

	b.SetBytes(int64(payloadSize))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		n, err := primary.WriteZeroCopy(func(buffer []byte) (int, error) {
			buffer[0] = byte(i)
			buffer[payloadSize-1] = byte(i >> 8)
			return payloadSize, nil
		})
		if err != nil {
			b.Fatalf("WriteZeroCopy failed: %v", err)
		}
		if n != payloadSize {
			b.Fatalf("WriteZeroCopy size = %d, want %d", n, payloadSize)
		}

		n, err = secondary.ReadZeroCopy(func(buffer []byte) error {
			_ = buffer[0]
			_ = buffer[len(buffer)-1]
			return nil
		})
		if err != nil {
			b.Fatalf("ReadZeroCopy failed: %v", err)
		}
		if n != payloadSize {
			b.Fatalf("ReadZeroCopy size = %d, want %d", n, payloadSize)
		}
	}
}

func BenchmarkStandardLinkReserveCommitSizes(b *testing.B) {
	for _, payloadSize := range []int{8, 64, 4096} {
		b.Run(fmt.Sprintf("%dB", payloadSize), func(b *testing.B) {
			benchmarkStandardLinkReserveCommit(b, payloadSize)
		})
	}
}

func benchmarkStandardLinkReserveCommit(b *testing.B, payloadSize int) {
	bufferCount := 1024
	bufferSize := payloadSize
	if bufferSize < 8 {
		bufferSize = 8
	}

	_, primary, secondary := newMemoryPair(b, bufferCount, bufferSize, LinkOptions{})

	b.SetBytes(int64(payloadSize))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		writeReservation, err := primary.ReserveWrite()
		if err != nil {
			b.Fatalf("ReserveWrite failed: %v", err)
		}
		buffer := writeReservation.Buffer()
		buffer[0] = byte(i)
		buffer[payloadSize-1] = byte(i >> 8)
		if err := writeReservation.Commit(payloadSize); err != nil {
			b.Fatalf("Commit failed: %v", err)
		}

		readReservation, err := secondary.ReserveRead()
		if err != nil {
			b.Fatalf("ReserveRead failed: %v", err)
		}
		readBuffer := readReservation.Buffer()
		_ = readBuffer[0]
		_ = readBuffer[len(readBuffer)-1]
		if err := readReservation.Release(); err != nil {
			b.Fatalf("Release failed: %v", err)
		}
	}
}

func BenchmarkStandardLinkPipeline(b *testing.B) {
	bufferCount := 1024
	bufferSize := 1 << 12
	_, primary, secondary := newMemoryPair(b, bufferCount, bufferSize, LinkOptions{})

	testData := make([]byte, bufferSize)
	readBuffer := make([]byte, bufferSize)
	for i := range testData {
		testData[i] = byte(i % 256)
	}
	b.SetBytes(int64(len(testData)))

	start := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		<-start
		for i := 0; i < b.N; i++ {
			n, err := secondary.Read(readBuffer)
			if err != nil {
				done <- err
				return
			}
			if n != len(testData) {
				done <- fmt.Errorf("read size = %d, want %d", n, len(testData))
				return
			}
		}
		done <- nil
	}()

	b.ResetTimer()
	close(start)
	for i := 0; i < b.N; i++ {
		n, err := primary.Write(testData)
		if err != nil {
			b.Fatalf("Write failed: %v", err)
		}
		if n != len(testData) {
			b.Fatalf("write size = %d, want %d", n, len(testData))
		}
	}
	if err := <-done; err != nil {
		b.Fatalf("reader failed: %v", err)
	}
}

func BenchmarkStandardLinkPipelineSizes(b *testing.B) {
	for _, payloadSize := range []int{8, 64, 4096} {
		b.Run(fmt.Sprintf("%dB", payloadSize), func(b *testing.B) {
			benchmarkStandardLinkPipeline(b, payloadSize)
		})
	}
}

func benchmarkStandardLinkPipeline(b *testing.B, payloadSize int) {
	bufferCount := 1024
	bufferSize := payloadSize
	if bufferSize < 8 {
		bufferSize = 8
	}

	_, primary, secondary := newMemoryPair(b, bufferCount, bufferSize, LinkOptions{})

	testData := make([]byte, payloadSize)
	readBuffer := make([]byte, payloadSize)
	for i := range testData {
		testData[i] = byte(i)
	}

	b.SetBytes(int64(payloadSize))
	b.ReportAllocs()

	start := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		<-start
		for i := 0; i < b.N; i++ {
			n, err := secondary.Read(readBuffer)
			if err != nil {
				done <- err
				return
			}
			if n != len(testData) {
				done <- fmt.Errorf("read size = %d, want %d", n, len(testData))
				return
			}
		}
		done <- nil
	}()

	b.ResetTimer()
	close(start)
	for i := 0; i < b.N; i++ {
		n, err := primary.Write(testData)
		if err != nil {
			b.Fatalf("Write failed: %v", err)
		}
		if n != len(testData) {
			b.Fatalf("write size = %d, want %d", n, len(testData))
		}
	}
	if err := <-done; err != nil {
		b.Fatalf("reader failed: %v", err)
	}
}

// TestStandardLinkBufferOverflow tests buffer overflow handling
// It verifies that attempts to write data larger than buffer size are rejected
func TestStandardLinkBufferOverflow(t *testing.T) {
	bufferCount := 1024
	bufferSize := 4096

	primary := newTestStandardLink(t, bufferCount, bufferSize, LinkOptions{})

	// Test data larger than buffer size
	testData := make([]byte, bufferSize+100) // 100 bytes larger than buffer

	// Write from primary - should fail with buffer overflow error
	_, err := primary.Write(testData)
	if err != ErrBufferOverflow {
		t.Errorf("Expected ErrBufferOverflow, got %v", err)
	}
}

// TestStandardLinkPacketConn tests PacketConn interface implementation
// It verifies that StandardLink properly implements net.PacketConn
func TestStandardLinkPacketConn(t *testing.T) {
	bufferCount := 1024
	bufferSize := 4096

	_, primary, secondary := newMemoryPair(t, bufferCount, bufferSize, LinkOptions{})

	// Test that StandardLink implements PacketConn interface
	var _ net.PacketConn = primary
	var _ net.PacketConn = secondary

	// Test LocalAddr
	if primary.LocalAddr() == nil {
		t.Error("LocalAddr should not be nil")
	}
	if primary.LocalAddr().Network() != "hqq" {
		t.Errorf("Expected network 'hqq', got %s", primary.LocalAddr().Network())
	}

	// Test SetDeadline/SetReadDeadline/SetWriteDeadline
	err := primary.SetDeadline(time.Now().Add(time.Second))
	if err != nil {
		t.Errorf("SetDeadline failed: %v", err)
	}

	err = primary.SetReadDeadline(time.Now().Add(time.Second))
	if err != nil {
		t.Errorf("SetReadDeadline failed: %v", err)
	}

	err = primary.SetWriteDeadline(time.Now().Add(time.Second))
	if err != nil {
		t.Errorf("SetWriteDeadline failed: %v", err)
	}

	// Test WriteTo with custom address
	testData := []byte("Hello, PacketConn!")
	customAddr := &HQQAddr{LinkID: "test-connection"}

	n, err := primary.WriteTo(testData, customAddr)
	if err != nil {
		t.Fatalf("WriteTo failed: %v", err)
	}
	if n != len(testData) {
		t.Errorf("Expected to write %d bytes, wrote %d", len(testData), n)
	}

	// Test ReadFrom
	readBuffer := make([]byte, len(testData))
	n, addr, err := secondary.ReadFrom(readBuffer)
	if err != nil {
		t.Fatalf("ReadFrom failed: %v", err)
	}
	if n != len(testData) {
		t.Errorf("Expected to read %d bytes, read %d", len(testData), n)
	}
	if addr == nil {
		t.Error("ReadFrom should return a non-nil address")
	}
	if string(readBuffer) != string(testData) {
		t.Errorf("Data mismatch: expected %s, got %s", string(testData), string(readBuffer))
	}

	// Test WriteTo with buffer overflow
	largeData := make([]byte, bufferSize+100)
	_, err = primary.WriteTo(largeData, customAddr)
	if err != ErrBufferOverflow {
		t.Errorf("Expected ErrBufferOverflow for large data, got %v", err)
	}
}

func negotiateAdvancedPairForBenchmark(b *testing.B, ctx context.Context, primary, secondary *AdvancedLink) {
	b.Helper()
	var wg sync.WaitGroup
	wg.Add(2)
	var primaryErr, secondaryErr error
	go func() {
		defer wg.Done()
		_, primaryErr = primary.NegotiateProtocol(ctx, ProtocolVersion{Major: 1, Minor: 0}, FeatureMessages|FeatureRequestResponse)
	}()
	go func() {
		defer wg.Done()
		_, secondaryErr = secondary.WaitForNegotiation(ctx)
	}()
	wg.Wait()
	if primaryErr != nil {
		b.Fatalf("primary negotiation failed: %v", primaryErr)
	}
	if secondaryErr != nil {
		b.Fatalf("secondary negotiation failed: %v", secondaryErr)
	}
}

// advancedTestLinks returns a negotiated Advanced pair (client is the
// Primary, server the Secondary).
func advancedTestLinks(t *testing.T, bufferCount, bufferSize int, clientOpts, serverOpts AdvancedOptions) (client, server *AdvancedLink) {
	t.Helper()
	client, server = newAdvancedPairWithOptions(t, bufferCount, bufferSize, clientOpts, serverOpts)
	negotiateAdvancedPair(t, client, server)
	return client, server
}

// injectAdvancedFrame publishes one raw OpConnCopy frame from l, bypassing
// the chunking of the public API (to forge aborts and partial messages).
func injectAdvancedFrame(t *testing.T, l *AdvancedLink, connectionID, messageID, totalSize, chunkOffset, flags, aux uint64, chunk []byte) {
	t.Helper()
	s := l.slink
	if err := s.enterTx(true); err != nil {
		t.Fatalf("enterTx: %v", err)
	}
	defer s.exitTx()
	c := mpmc.Cancel{Closed: s.closeCh, Check: s.checkFn, Deadline: time.Now().Add(2 * time.Second)}
	if err := l.sendAdvancedChunk(&c, connectionID, messageID, totalSize, chunkOffset, flags, aux, chunk, nil); err != nil {
		t.Fatalf("inject frame: %v", err)
	}
}

func TestPendingCallReleaseRecyclesOnlyCleanChannels(t *testing.T) {
	l := newAdvancedLink(&StandardLink{bufferCount: 4}, AdvancedOptions{})

	// An entry nobody touched is released by its owner, which may recycle
	// the (empty) channel.
	key := advancedCallKey{streamID: 1, messageID: 1}
	ch := l.registerPendingCall(key)
	if !l.unregisterPendingCall(key, ch) {
		t.Fatalf("unregisterPendingCall of an untouched call = false, want true")
	}

	// failPendingCalls and routeResponse remove the entry first and send a
	// result on the channel: unregisterPendingCall must report that and the
	// channel must never come back from the pool.
	for name, remove := range map[string]func(key advancedCallKey){
		"failPendingCalls": func(advancedCallKey) { l.failPendingCalls(ErrClosed) },
		"routeResponse": func(key advancedCallKey) {
			hdr := advHeader{streamID: key.streamID, messageID: key.messageID, flags: AdvFlagResponse}
			if !l.routeResponse(hdr, l.pool.get(1), nil) {
				t.Fatalf("routeResponse did not find the pending call")
			}
		},
	} {
		key := advancedCallKey{streamID: 2, messageID: 2}
		dirty := l.registerPendingCall(key)
		remove(key)
		if l.unregisterPendingCall(key, dirty) {
			t.Fatalf("%s: unregisterPendingCall = true after the entry was removed", name)
		}
		if len(dirty) != 1 {
			t.Fatalf("%s: dirty channel holds %d results, want 1", name, len(dirty))
		}
		for i := 0; i < 256; i++ {
			k := advancedCallKey{streamID: 3, messageID: uint64(i)}
			got := l.registerPendingCall(k)
			if got == dirty || len(got) != 0 {
				t.Fatalf("%s: registerPendingCall returned a dirty channel (len %d)", name, len(got))
			}
			l.unregisterPendingCall(k, got)
		}
	}
}

// TestAdvancedLinkPendingPoolStress hammers the synchronous call path with
// concurrent responders so that recycled channels are reused constantly.
func TestAdvancedLinkPendingPoolStress(t *testing.T) {
	client, server := advancedTestLinks(t, 64, 64, AdvancedOptions{}, AdvancedOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	go server.Serve(ctx, HandlerFunc(func(_ context.Context, w *ResponseWriter, req *Request) {
		_, _ = w.Write(req.Payload())
	}))

	var wg sync.WaitGroup
	errCh := make(chan error, 8)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				want := fmt.Sprintf("g%d-i%d", g, i)
				got, err := client.Call(ctx, 1, []byte(want))
				if err != nil {
					errCh <- fmt.Errorf("Call %s: %w", want, err)
					return
				}
				if string(got.Bytes()) != want {
					errCh <- fmt.Errorf("Call %s returned %q (stale pooled result)", want, got.Bytes())
					return
				}
				got.Release()
			}
		}(g)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
}

func TestAdvancedServeDropsAbortedRequest(t *testing.T) {
	client, server := advancedTestLinks(t, 16, 32, AdvancedOptions{}, AdvancedOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Count every request, whatever its method.
	var handled atomic.Int32
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- server.Serve(ctx, HandlerFunc(func(_ context.Context, w *ResponseWriter, req *Request) {
			handled.Add(1)
			_, _ = w.Write(req.Payload())
		}))
	}()

	// A request whose multi-chunk send failed mid-way: first chunk, then
	// the abort frame the sender emits. A stray abort without any partial
	// state follows.
	injectAdvancedFrame(t, client, 0, 900, 64, 0, AdvFlagBegin|AdvFlagRequest, 7, make([]byte, 32))
	injectAdvancedFrame(t, client, 0, 900, 0, 0, AdvFlagAbort|AdvFlagRequest, 0, nil)
	injectAdvancedFrame(t, client, 0, 901, 0, 0, AdvFlagAbort|AdvFlagRequest, 0, nil)

	got, err := client.Call(ctx, 7, []byte("real"))
	if err != nil {
		t.Fatalf("Call after aborted request: %v", err)
	}
	if string(got.Bytes()) != "real" {
		t.Fatalf("Call after aborted request = %q", got.Bytes())
	}
	got.Release()
	if n := handled.Load(); n != 1 {
		t.Fatalf("handler ran %d times, want 1 (the aborted request must not be served)", n)
	}
	// Aborts are dropped: nothing reaches the data queue of the stream.
	shortCtx, shortCancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer shortCancel()
	if m, err := server.Recv(shortCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Recv after aborted request = (%v, %v), want deadline exceeded (aborts are dropped)", m != nil, err)
	}
	cancel()
	<-serveErr
}

func TestAdvancedReceiveDropsAbortedMessages(t *testing.T) {
	client, server := advancedTestLinks(t, 16, 32, AdvancedOptions{}, AdvancedOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	injectAdvancedFrame(t, client, 0, 910, 64, 0, AdvFlagBegin, 0, make([]byte, 32))
	injectAdvancedFrame(t, client, 0, 910, 0, 0, AdvFlagAbort, 0, nil)
	injectAdvancedFrame(t, client, 0, 911, 0, 0, AdvFlagAbort, 0, nil)
	if err := client.Send(ctx, []byte("complete")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	message, err := server.Recv(ctx)
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if message.StreamID() != 0 || string(message.Bytes()) != "complete" {
		t.Fatalf("Recv returned stream=%d payload=%q, want the complete data message", message.StreamID(), message.Bytes())
	}
	message.Release()
	if len(server.assemblies) != 0 || server.assemblyBytes != 0 {
		t.Fatalf("aborted message left %d assemblies / %d bytes", len(server.assemblies), server.assemblyBytes)
	}
}

func TestAdvancedAbortedResponseFailsCall(t *testing.T) {
	for _, tc := range []struct {
		name       string
		dispatcher bool
		partial    bool
	}{
		{"sync/abort-only", false, false},
		{"sync/partial", false, true},
		{"dispatcher/abort-only", true, false},
		{"dispatcher/partial", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, server := advancedTestLinks(t, 16, 32, AdvancedOptions{}, AdvancedOptions{})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if tc.dispatcher {
				if err := client.startDispatcher(); err != nil {
					t.Fatal(err)
				}
			}

			// The server abandons the first response mid-transfer instead of
			// answering it (the late real response its handler returns is
			// unmatched and dropped); the next call is answered normally.
			var handled atomic.Int32
			go server.Serve(ctx, HandlerFunc(func(_ context.Context, w *ResponseWriter, req *Request) {
				if handled.Add(1) == 1 {
					if tc.partial {
						injectAdvancedFrame(t, server, req.StreamID(), req.msg.messageID, 64, 0, AdvFlagBegin|AdvFlagResponse, 5, make([]byte, 32))
					}
					injectAdvancedFrame(t, server, req.StreamID(), req.msg.messageID, 0, 0, AdvFlagAbort|AdvFlagResponse, 0, nil)
					return
				}
				_, _ = w.Write([]byte("pong"))
			}))

			response, err := client.Stream().Call(ctx, 1, []byte("ping"))
			if !errors.Is(err, ErrMessageAborted) || !errors.Is(err, ErrProtocol) {
				t.Fatalf("Call with aborted response = %v, want ErrMessageAborted", err)
			}
			if response != nil {
				t.Fatalf("aborted call returned a message of %d bytes", response.Len())
			}
			got, err := client.Call(ctx, 1, []byte("ping"))
			if err != nil {
				t.Fatalf("Call after aborted response: %v", err)
			}
			if string(got.Bytes()) != "pong" {
				t.Fatalf("Call after aborted response = %q", got.Bytes())
			}
			got.Release()
		})
	}
}

func TestAdvancedOpenStreamAllocatesDisjointIDsPerRole(t *testing.T) {
	const streams = 6
	primary, secondary := advancedTestLinks(t, 32, 32, AdvancedOptions{}, AdvancedOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, l := range []*AdvancedLink{primary, secondary} {
		if err := l.ListenStreams(ctx); err != nil {
			t.Fatalf("ListenStreams: %v", err)
		}
	}

	type dialed struct {
		id  uint64
		err error
	}
	results := make(chan dialed, 2*streams)
	var wg sync.WaitGroup
	for _, l := range []*AdvancedLink{primary, secondary} {
		for i := 0; i < streams; i++ {
			wg.Add(1)
			go func(l *AdvancedLink) {
				defer wg.Done()
				s, err := l.OpenStream(ctx)
				var id uint64
				if s != nil {
					id = s.ID()
				}
				results <- dialed{id, err}
			}(l)
		}
	}
	wg.Wait()
	close(results)

	seen := make(map[uint64]bool)
	var even, odd int
	for r := range results {
		if r.err != nil {
			t.Fatalf("OpenStream: %v", r.err)
		}
		if r.id == 0 || seen[r.id] {
			t.Fatalf("connection ID %d is zero or duplicated", r.id)
		}
		seen[r.id] = true
		if r.id%2 == 0 {
			even++
		} else {
			odd++
		}
	}
	if even != streams || odd != streams {
		t.Fatalf("got %d even and %d odd IDs, want %d each", even, odd, streams)
	}

	// Each side accepted exactly the other side's IDs.
	for _, tc := range []struct {
		name   string
		l      *AdvancedLink
		parity uint64
	}{{"primary accepts secondary IDs", primary, 1}, {"secondary accepts primary IDs", secondary, 0}} {
		for i := 0; i < streams; i++ {
			s, err := tc.l.AcceptStream(ctx)
			if err != nil {
				t.Fatalf("%s: AcceptStream: %v", tc.name, err)
			}
			if id := s.ID(); id%2 != tc.parity || id == 0 {
				t.Fatalf("%s: accepted ID %d with the wrong parity", tc.name, id)
			}
		}
	}
}

func TestAdvancedConnCreateRefusesForgedIDs(t *testing.T) {
	for _, tc := range []struct {
		name               string
		victimIsPrimary    bool
		valid, wrongParity uint64
	}{
		{"primary victim", true, 3, 4},
		{"secondary victim", false, 4, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := advancedTestLinks(t, 16, 32, AdvancedOptions{}, AdvancedOptions{})
			victim, forger := a, b
			if !tc.victimIsPrimary {
				victim, forger = b, a
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := victim.ListenStreams(ctx); err != nil {
				t.Fatalf("ListenStreams: %v", err)
			}
			create := func(id uint64) {
				t.Helper()
				if err := forger.sendPacket(ctx, protocol.NewPacket(protocol.OpConnCreate, id)); err != nil {
					t.Fatalf("send create(%d): %v", id, err)
				}
			}
			// The forger never started a dispatcher, so Recv surfaces the
			// OpError refusals directly.
			expectRefused := func(what string) {
				t.Helper()
				if _, err := forger.Recv(ctx); !errors.Is(err, ErrInvalidOp) {
					t.Fatalf("%s: Recv = %v, want a refusal (ErrInvalidOp)", what, err)
				}
			}

			create(0)
			expectRefused("connection ID 0")
			create(tc.wrongParity)
			expectRefused("wrong parity")

			create(tc.valid)
			accepted, err := victim.AcceptStream(ctx)
			if err != nil {
				t.Fatalf("AcceptStream = %v, want stream %d", err, tc.valid)
			}
			if accepted.ID() != tc.valid {
				t.Fatalf("AcceptStream = %d, want %d", accepted.ID(), tc.valid)
			}
			victim.controlMu.Lock()
			_, registered := victim.streams[tc.valid]
			victim.controlMu.Unlock()
			if !registered {
				t.Fatalf("accepted connection %d is not registered", tc.valid)
			}

			create(tc.valid)
			expectRefused("duplicate")
			victim.controlMu.Lock()
			_, stillRegistered := victim.streams[tc.valid]
			victim.controlMu.Unlock()
			if !stillRegistered {
				t.Fatalf("duplicate create dropped the live connection")
			}
			shortCtx, shortCancel := context.WithTimeout(ctx, 50*time.Millisecond)
			defer shortCancel()
			if s, err := victim.AcceptStream(shortCtx); err == nil {
				t.Fatalf("refused create was queued for AcceptStream as %d", s.ID())
			}
		})
	}
}

func TestAdvancedReassemblyLimitBoundsIncompleteMessages(t *testing.T) {
	const (
		bufferSize = 64
		overhead   = advancedAssemblyOverhead
	)
	l := newAdvancedLink(&StandardLink{bufferCount: 4}, AdvancedOptions{
		MaxMessageSize:     1000,
		MaxReassemblyBytes: (300 + overhead) + (100 + overhead),
	})
	buffer := make([]byte, bufferSize)
	consume := func(conn, msg, total, off, size, flags, aux uint64) (*Message, advHeader, chunkOutcome) {
		t.Helper()
		p := protocol.NewConnCopyPacket(conn, msg, total, off, size, flags, aux)
		m, hdr, outcome, err := l.consumeAdvancedChunk(&p, buffer)
		if err != nil {
			t.Fatalf("consume(msg %d off %d): %v", msg, off, err)
		}
		return m, hdr, outcome
	}
	// send feeds a whole message in bufferSize chunks and returns the
	// outcome of its first and last chunk.
	send := func(msg, total, kind uint64) (first, last chunkOutcome) {
		for off := uint64(0); off < total; off += bufferSize {
			size := min(uint64(bufferSize), total-off)
			flags := kind
			if off == 0 {
				flags |= AdvFlagBegin
			}
			if off+size == total {
				flags |= AdvFlagEnd
			}
			m, _, o := consume(0, msg, total, off, size, flags, 0)
			m.Release()
			if off == 0 {
				first = o
			}
			last = o
		}
		return first, last
	}
	// begin feeds only the first chunk.
	begin := func(msg, total, kind uint64) (advHeader, chunkOutcome) {
		m, hdr, o := consume(0, msg, total, 0, bufferSize, AdvFlagBegin|kind, 0)
		m.Release()
		return hdr, o
	}
	rest := func(msg, total, kind uint64) (last chunkOutcome) {
		for off := uint64(bufferSize); off < total; off += bufferSize {
			size := min(uint64(bufferSize), total-off)
			flags := kind
			if off+size == total {
				flags |= AdvFlagEnd
			}
			m, _, o := consume(0, msg, total, off, size, flags, 0)
			m.Release()
			last = o
		}
		return last
	}

	if _, o := begin(1, 300, 0); o != chunkPartial || l.assemblyBytes != 300+overhead {
		t.Fatalf("first message: outcome %v, charged %d", o, l.assemblyBytes)
	}
	// Does not fit next to message 1: refused without allocating.
	refused, o := begin(2, 300, AdvFlagRequest)
	if o != chunkRefused || !refused.isAbort() || !refused.isRequest() || refused.messageID != 2 {
		t.Fatalf("over-limit request: outcome %v, message %+v", o, refused)
	}
	refusedResponse, o := begin(3, 300, AdvFlagResponse)
	if o != chunkRefused || !refusedResponse.isAbort() || !refusedResponse.isResponse() {
		t.Fatalf("over-limit response: outcome %v, message %+v", o, refusedResponse)
	}
	if len(l.assemblies) != 1 || l.assemblyBytes != 300+overhead || len(l.rejected) != 2 {
		t.Fatalf("refusal allocated: %d assemblies, %d bytes, %d rejected", len(l.assemblies), l.assemblyBytes, len(l.rejected))
	}
	// A message that still fits is accepted, a single-chunk message always is.
	if _, o := begin(4, 100, 0); o != chunkPartial || l.assemblyBytes != 300+overhead+100+overhead {
		t.Fatalf("fitting message: outcome %v, charged %d", o, l.assemblyBytes)
	}
	before := len(l.assemblies)
	single, _, o := consume(0, 5, 50, 0, 50, AdvFlagBegin|AdvFlagEnd, 0)
	if o != chunkComplete || single.Len() != 50 || len(l.assemblies) != before {
		t.Fatalf("single-chunk message: outcome %v, %d assemblies", o, len(l.assemblies))
	}
	single.Release()

	// The remaining chunks of refused messages are drained, and an abort
	// for one is consumed silently.
	if o := rest(2, 300, AdvFlagRequest); o != chunkPartial {
		t.Fatalf("draining refused request: outcome %v", o)
	}
	if _, _, o := consume(0, 3, 0, 0, 0, AdvFlagAbort|AdvFlagResponse, 0); o != chunkPartial {
		t.Fatalf("abort of a refused message: outcome %v", o)
	}
	if len(l.rejected) != 0 || len(l.rejectedOrder) != 0 {
		t.Fatalf("rejected markers left behind: %d/%d", len(l.rejected), len(l.rejectedOrder))
	}

	// Completing and aborting messages releases their charge.
	if o := rest(1, 300, 0); o != chunkComplete {
		t.Fatalf("completing message 1: outcome %v", o)
	}
	if _, _, o := consume(0, 4, 0, 0, 0, AdvFlagAbort, 0); o != chunkAborted {
		t.Fatalf("aborting message 4: outcome %v", o)
	}
	if len(l.assemblies) != 0 || l.assemblyBytes != 0 {
		t.Fatalf("assemblies not released: %d assemblies, %d bytes", len(l.assemblies), l.assemblyBytes)
	}
	if first, last := send(6, 300, 0); first != chunkPartial || last != chunkComplete {
		t.Fatalf("message after release: first %v, last %v", first, last)
	}
}

func TestAdvancedReassemblyLimitDefaultAndRejectedBound(t *testing.T) {
	l := newAdvancedLink(&StandardLink{bufferCount: 4}, AdvancedOptions{MaxMessageSize: 1 << 20})
	if l.maxReassemblyBytes != 4<<20 {
		t.Fatalf("default MaxReassemblyBytes = %d, want 4 * MaxMessageSize", l.maxReassemblyBytes)
	}
	if l := newAdvancedLink(&StandardLink{bufferCount: 4}, AdvancedOptions{MaxMessageSize: ^uint64(0)}); l.maxReassemblyBytes != ^uint64(0) {
		t.Fatalf("default MaxReassemblyBytes overflowed to %d", l.maxReassemblyBytes)
	}
	if l := newAdvancedLink(&StandardLink{bufferCount: 4}, AdvancedOptions{MaxReassemblyBytes: 77}); l.maxReassemblyBytes != 77 {
		t.Fatalf("explicit MaxReassemblyBytes = %d, want 77", l.maxReassemblyBytes)
	}

	// The set of refused messages being drained is bounded (oldest first).
	l.maxReassemblyBytes = 0
	buffer := make([]byte, 16)
	for i := 0; i < maxRejectedMessages+10; i++ {
		p := protocol.NewConnCopyPacket(0, uint64(i+1), 100, 0, 16, AdvFlagBegin, 0)
		if _, _, o, err := l.consumeAdvancedChunk(&p, buffer); err != nil || o != chunkRefused {
			t.Fatalf("message %d: outcome %v, err %v", i, o, err)
		}
	}
	if len(l.rejected) != maxRejectedMessages || len(l.rejectedOrder) != maxRejectedMessages {
		t.Fatalf("rejected set holds %d/%d keys, want %d", len(l.rejected), len(l.rejectedOrder), maxRejectedMessages)
	}
}

func TestAdvancedReassemblyLimitRefusesRequest(t *testing.T) {
	client, server := advancedTestLinks(t, 16, 32, AdvancedOptions{}, AdvancedOptions{MaxReassemblyBytes: 1000})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go server.Serve(ctx, HandlerFunc(func(_ context.Context, w *ResponseWriter, req *Request) {
		_, _ = w.Write(req.Payload())
	}))

	// A hog eats most of the server's budget: 600 + overhead of 1000.
	hog := make([]byte, 600)
	injectAdvancedFrame(t, client, 0, 950, 600, 0, AdvFlagBegin, 0, hog[:32])

	request := bytes.Repeat([]byte("r"), 300)
	_, err := client.Call(ctx, 1, request)
	var re ResponseError
	if !errors.As(err, &re) || re.Status != uint64(ErrCodeBufferOverflow) {
		t.Fatalf("Call over the reassembly limit = %v, want ResponseError(ErrCodeBufferOverflow)", err)
	}
	if got := server.Stats().DroppedMessages; got != 1 {
		t.Fatalf("server DroppedMessages = %d, want 1", got)
	}

	// Finish the hog (32-byte chunks); the budget is released.
	for off := uint64(32); off < 600; off += 32 {
		size := min(uint64(32), 600-off)
		flags := uint64(0)
		if off+size == 600 {
			flags = AdvFlagEnd
		}
		injectAdvancedFrame(t, client, 0, 950, 600, off, flags, 0, hog[off:off+size])
	}
	got, err := client.Call(ctx, 1, request)
	if err != nil {
		t.Fatalf("Call after the budget was released: %v", err)
	}
	if !bytes.Equal(got.Bytes(), request) {
		t.Fatalf("Call after the budget was released = %d bytes, want %d", got.Len(), len(request))
	}
	got.Release()
	// The completed hog is delivered once, as a data message of stream 0.
	hogMessage, err := server.Recv(ctx)
	if err != nil {
		t.Fatalf("Recv of the completed hog: %v", err)
	}
	if hogMessage.Len() != 600 {
		t.Fatalf("completed hog has %d bytes, want 600", hogMessage.Len())
	}
	hogMessage.Release()
}

func TestAdvancedReassemblyLimitRefusesResponse(t *testing.T) {
	client, server := advancedTestLinks(t, 16, 32, AdvancedOptions{MaxReassemblyBytes: 1000}, AdvancedOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	response := bytes.Repeat([]byte("p"), 300)
	go server.Serve(ctx, HandlerFunc(func(_ context.Context, w *ResponseWriter, _ *Request) { _, _ = w.Write(response) }))

	hog := make([]byte, 600)
	injectAdvancedFrame(t, server, 0, 960, 600, 0, AdvFlagBegin, 0, hog[:32])

	_, err := client.Call(ctx, 1, []byte("x"))
	if !errors.Is(err, ErrReassemblyLimit) || !errors.Is(err, ErrBufferOverflow) {
		t.Fatalf("Call with a refused response = %v, want ErrReassemblyLimit", err)
	}
	if got := client.Stats().DroppedMessages; got != 1 {
		t.Fatalf("client DroppedMessages = %d, want 1", got)
	}
	// Release the budget by aborting the hog; the link keeps working.
	injectAdvancedFrame(t, server, 0, 960, 0, 0, AdvFlagAbort, 0, nil)
	got, err := client.Call(ctx, 1, []byte("x"))
	if err != nil {
		t.Fatalf("Call after the budget was released: %v", err)
	}
	if !bytes.Equal(got.Bytes(), response) {
		t.Fatalf("Call after the budget was released = %d bytes, want %d", got.Len(), len(response))
	}
	got.Release()
}
