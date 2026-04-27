package hqq

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"gosuda.org/hqq/internal/mmap"
	"gosuda.org/hqq/internal/shm"
)

// createAlignedBuffer creates a page-aligned buffer for testing
// This helper function ensures the shared memory buffer is properly aligned
func createAlignedBuffer(t *testing.T, size uintptr) ([]byte, uintptr) {
	// Create shared memory buffer with extra space for alignment
	alignment := syscall.Getpagesize()
	buffer := make([]byte, int(size)+alignment)

	// Find page-aligned offset
	offset := uintptr(unsafe.Pointer(&buffer[0]))
	if offset%uintptr(alignment) != 0 {
		offset = ((offset / uintptr(alignment)) + 1) * uintptr(alignment)
	}

	return buffer, offset
}

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

	bufferCount := 8
	bufferSize := 64
	size := SizeStandardLink(bufferCount, bufferSize)
	backing, offset := createAlignedBuffer(t, size)
	defer runtime.KeepAlive(backing)

	standard, err := OpenStandardLink(offset, bufferCount, bufferSize)
	if err != nil {
		t.Fatalf("OpenStandardLink failed: %v", err)
	}
	defer standard.Close()
	if standard.BufferSize() != bufferSize || standard.BufferCount() != bufferCount {
		t.Fatalf("standard sizing = %d/%d", standard.BufferCount(), standard.BufferSize())
	}
	if standard.LargeMessageThreshold() != bufferSize {
		t.Fatalf("standard threshold = %d, want %d", standard.LargeMessageThreshold(), bufferSize)
	}
	if !standard.IsLargeMessage(bufferSize + 1) {
		t.Fatalf("standard IsLargeMessage(%d) = false", bufferSize+1)
	}

	advanced, err := NewAdvancedLink(offset, bufferCount, bufferSize)
	if err != nil {
		t.Fatalf("NewAdvancedLink failed: %v", err)
	}
	defer advanced.Close()
	if advanced.LargeMessageThreshold() != bufferSize {
		t.Fatalf("advanced threshold = %d, want %d", advanced.LargeMessageThreshold(), bufferSize)
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
	defer cleanupNamedLink(t, smem, mapped, primary.slink, true)

	child, output := startMultiProcessChild(t, "advanced", name, int(SizeStandardLink(bufferCount, bufferSize)), bufferCount, bufferSize)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ok, err := primary.NegotiateProtocol(ctx, ProtocolVersion{Major: 1, Minor: 0}, FeatureLargeCopy|FeatureRequestResponse)
	if err != nil || !ok {
		t.Fatalf("parent negotiation = %v, %v\nchild output:\n%s", ok, err, output.String())
	}

	request := patternedBytes(bufferSize*3+17, 11)
	response, err := primary.Call(ctx, 77, request)
	if err != nil {
		t.Fatalf("parent Call failed: %v\nchild output:\n%s", err, output.String())
	}
	if want := patternedBytes(bufferSize*2+5, 23); !bytes.Equal(response, want) {
		t.Fatalf("response mismatch len=%d want=%d", len(response), len(want))
	}

	large := patternedBytes(bufferSize*4+9, 37)
	if _, err := primary.SendLarge(ctx, 0, large); err != nil {
		t.Fatalf("parent SendLarge failed: %v\nchild output:\n%s", err, output.String())
	}
	ack, err := primary.Receive(ctx)
	if err != nil {
		t.Fatalf("parent Receive ack failed: %v\nchild output:\n%s", err, output.String())
	}
	if !ack.IsData() || string(ack.Payload) != "advanced child large ack" {
		t.Fatalf("ack = flags %#x payload %q", ack.Flags, ack.Payload)
	}

	waitMultiProcessChild(t, child, output)
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
	smem, mapped, offset := mapNamedSharedMemory(t, name, size, os.O_RDWR|os.O_CREATE|os.O_EXCL)
	link, err := OpenStandardLink(offset, bufferCount, bufferSize)
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
	smem, mapped, offset := mapNamedSharedMemory(t, name, size, os.O_RDWR|os.O_CREATE|os.O_EXCL)
	link, err := NewAdvancedLink(offset, bufferCount, bufferSize)
	if err != nil {
		_ = mmap.UnMap(mapped)
		_ = smem.Close()
		_ = smem.Delete()
		t.Fatalf("NewAdvancedLink failed: %v", err)
	}
	return name, smem, mapped, link
}

func newPortableSharedMemoryName() string {
	// macOS keeps POSIX shm names short. Keep the name comfortably below
	// common 31-byte limits while preserving a leading slash for shm_open.
	pidPart := strconv.FormatInt(int64(os.Getpid()&0xfff), 36)
	timePart := strconv.FormatInt(time.Now().UnixNano()&0xffffff, 36)
	return "/hq" + pidPart + timePart
}

func openNamedStandardLink(t *testing.T, name string, size, bufferCount, bufferSize int) (*shm.SharedMemory, []byte, *StandardLink) {
	t.Helper()
	smem, mapped, offset := mapNamedSharedMemory(t, name, size, os.O_RDWR)
	link, err := OpenStandardLink(offset, bufferCount, bufferSize)
	if err != nil {
		_ = mmap.UnMap(mapped)
		_ = smem.Close()
		t.Fatalf("OpenStandardLink child failed: %v", err)
	}
	return smem, mapped, link
}

func openNamedAdvancedLink(t *testing.T, name string, size, bufferCount, bufferSize int) (*shm.SharedMemory, []byte, *AdvancedLink) {
	t.Helper()
	smem, mapped, offset := mapNamedSharedMemory(t, name, size, os.O_RDWR)
	link, err := NewAdvancedLink(offset, bufferCount, bufferSize)
	if err != nil {
		_ = mmap.UnMap(mapped)
		_ = smem.Close()
		t.Fatalf("NewAdvancedLink child failed: %v", err)
	}
	return smem, mapped, link
}

func mapNamedSharedMemory(t *testing.T, name string, size int, flags int) (*shm.SharedMemory, []byte, uintptr) {
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
	return smem, mapped, uintptr(unsafe.Pointer(&mapped[0]))
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
	defer cleanupNamedLink(t, smem, mapped, link.slink, false)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ok, err := link.WaitForNegotiation(ctx)
	if err != nil || !ok {
		t.Fatalf("child WaitForNegotiation = %v, %v", ok, err)
	}

	request, err := link.Receive(ctx)
	if err != nil {
		t.Fatalf("child Receive request failed: %v", err)
	}
	if !request.IsRequest() || request.MethodID != 77 {
		t.Fatalf("child request flags=%#x method=%d", request.Flags, request.MethodID)
	}
	if want := patternedBytes(bufferSize*3+17, 11); !bytes.Equal(request.Payload, want) {
		t.Fatalf("child request mismatch len=%d want=%d", len(request.Payload), len(want))
	}
	if err := link.Respond(ctx, request, 201, patternedBytes(bufferSize*2+5, 23)); err != nil {
		t.Fatalf("child Respond failed: %v", err)
	}

	large, err := link.Receive(ctx)
	if err != nil {
		t.Fatalf("child Receive large failed: %v", err)
	}
	if !large.IsData() {
		t.Fatalf("child large flags=%#x, want data", large.Flags)
	}
	if want := patternedBytes(bufferSize*4+9, 37); !bytes.Equal(large.Payload, want) {
		t.Fatalf("child large mismatch len=%d want=%d", len(large.Payload), len(want))
	}
	if _, err := link.SendLarge(ctx, 0, []byte("advanced child large ack")); err != nil {
		t.Fatalf("child SendLarge ack failed: %v", err)
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
	size := SizeStandardLink(bufferCount, bufferSize)
	if size == 0 {
		t.Fatal("SizeStandardLink returned 0")
	}

	// Create aligned buffer
	backing, offset := createAlignedBuffer(t, size)
	defer runtime.KeepAlive(backing)

	// Create primary link
	primary, err := OpenStandardLink(offset, bufferCount, bufferSize)
	if err != nil {
		t.Fatalf("Failed to create primary link: %v", err)
	}
	defer primary.Close()

	if primary.GetMode() != LinkModePrimary {
		t.Error("Primary link should be in primary mode")
	}

	if primary.GetType() != LinkTypeStandard {
		t.Error("Link should be of type Standard")
	}

	// Create secondary link
	secondary, err := OpenStandardLink(offset, bufferCount, bufferSize)
	if err != nil {
		t.Fatalf("Failed to create secondary link: %v", err)
	}
	defer secondary.Close()

	if secondary.GetMode() != LinkModeSecondary {
		t.Error("Secondary link should be in secondary mode")
	}
}

// TestStandardLinkReadWrite tests basic read/write functionality
// It verifies that data can be transferred between primary and secondary links
func TestStandardLinkReadWrite(t *testing.T) {
	bufferCount := 1024
	bufferSize := 4096

	// Calculate required memory size
	size := SizeStandardLink(bufferCount, bufferSize)

	// Create aligned buffer
	backing, offset := createAlignedBuffer(t, size)
	defer runtime.KeepAlive(backing)

	// Create links
	primary, err := OpenStandardLink(offset, bufferCount, bufferSize)
	if err != nil {
		t.Fatalf("Failed to create primary link: %v", err)
	}
	defer primary.Close()

	secondary, err := OpenStandardLink(offset, bufferCount, bufferSize)
	if err != nil {
		t.Fatalf("Failed to create secondary link: %v", err)
	}
	defer secondary.Close()

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
	size := SizeStandardLink(bufferCount, bufferSize)
	backing, offset := createAlignedBuffer(t, size)
	defer runtime.KeepAlive(backing)

	primary, err := OpenStandardLink(offset, bufferCount, bufferSize)
	if err != nil {
		t.Fatalf("Failed to create primary link: %v", err)
	}
	defer primary.Close()

	secondary, err := OpenStandardLink(offset, bufferCount, bufferSize)
	if err != nil {
		t.Fatalf("Failed to create secondary link: %v", err)
	}
	defer secondary.Close()

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
	size := SizeStandardLink(bufferCount, bufferSize)
	backing, offset := createAlignedBuffer(t, size)
	defer runtime.KeepAlive(backing)

	primary, err := OpenStandardLink(offset, bufferCount, bufferSize)
	if err != nil {
		t.Fatalf("Failed to create primary link: %v", err)
	}
	defer primary.Close()

	secondary, err := OpenStandardLink(offset, bufferCount, bufferSize)
	if err != nil {
		t.Fatalf("Failed to create secondary link: %v", err)
	}
	defer secondary.Close()

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
	size := SizeStandardLink(bufferCount, bufferSize)
	backing, offset := createAlignedBuffer(t, size)
	defer runtime.KeepAlive(backing)

	primary, err := OpenStandardLink(offset, bufferCount, bufferSize)
	if err != nil {
		t.Fatalf("Failed to create primary link: %v", err)
	}
	defer primary.Close()

	secondary, err := OpenStandardLink(offset, bufferCount, bufferSize)
	if err != nil {
		t.Fatalf("Failed to create secondary link: %v", err)
	}
	defer secondary.Close()

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
	size := SizeStandardLink(bufferCount, bufferSize)
	backing, offset := createAlignedBuffer(t, size)
	defer runtime.KeepAlive(backing)

	primary, err := OpenStandardLink(offset, bufferCount, bufferSize)
	if err != nil {
		t.Fatalf("Failed to create primary link: %v", err)
	}
	defer primary.Close()

	secondary, err := OpenStandardLink(offset, bufferCount, bufferSize)
	if err != nil {
		t.Fatalf("Failed to create secondary link: %v", err)
	}
	defer secondary.Close()

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
	size := SizeStandardLink(bufferCount, bufferSize)
	backing, offset := createAlignedBuffer(t, size)
	defer runtime.KeepAlive(backing)

	primary, err := OpenStandardLink(offset, bufferCount, bufferSize)
	if err != nil {
		t.Fatalf("Failed to create primary link: %v", err)
	}
	defer primary.Close()

	secondary, err := OpenStandardLink(offset, bufferCount, bufferSize)
	if err != nil {
		t.Fatalf("Failed to create secondary link: %v", err)
	}
	defer secondary.Close()

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
	size := SizeStandardLink(bufferCount, bufferSize)
	backing, offset := createAlignedBuffer(t, size)
	defer runtime.KeepAlive(backing)

	primary, err := OpenStandardLink(offset, bufferCount, bufferSize)
	if err != nil {
		t.Fatalf("Failed to create primary link: %v", err)
	}
	defer primary.Close()

	secondary, err := OpenStandardLink(offset, bufferCount, bufferSize)
	if err != nil {
		t.Fatalf("Failed to create secondary link: %v", err)
	}
	defer secondary.Close()

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
	size := SizeStandardLink(bufferCount, bufferSize)
	backing, offset := createAlignedBuffer(t, size)
	defer runtime.KeepAlive(backing)

	primary, err := OpenStandardLink(offset, bufferCount, bufferSize)
	if err != nil {
		t.Fatalf("Failed to create primary link: %v", err)
	}
	defer primary.Close()

	secondary, err := OpenStandardLink(offset, bufferCount, bufferSize)
	if err != nil {
		t.Fatalf("Failed to create secondary link: %v", err)
	}
	defer secondary.Close()

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
	size := SizeStandardLink(bufferCount, bufferSize)
	backing, offset := createAlignedBuffer(t, size)
	defer runtime.KeepAlive(backing)

	primary, err := OpenStandardLink(offset, bufferCount, bufferSize)
	if err != nil {
		t.Fatalf("Failed to create primary link: %v", err)
	}
	defer primary.Close()

	secondary, err := OpenStandardLink(offset, bufferCount, bufferSize)
	if err != nil {
		t.Fatalf("Failed to create secondary link: %v", err)
	}
	defer secondary.Close()

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
	size := SizeStandardLink(bufferCount, bufferSize)
	backing, offset := createAlignedBuffer(t, size)
	defer runtime.KeepAlive(backing)

	primary, err := OpenStandardLink(offset, bufferCount, bufferSize)
	if err != nil {
		t.Fatalf("Failed to create primary link: %v", err)
	}
	defer primary.Close()

	secondary, err := OpenStandardLink(offset, bufferCount, bufferSize)
	if err != nil {
		t.Fatalf("Failed to create secondary link: %v", err)
	}
	defer secondary.Close()

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
	size := SizeStandardLink(bufferCount, bufferSize)
	backing, offset := createAlignedBuffer(t, size)
	defer runtime.KeepAlive(backing)

	primary, err := OpenStandardLink(offset, bufferCount, bufferSize)
	if err != nil {
		t.Fatalf("Failed to create primary link: %v", err)
	}
	defer primary.Close()

	secondary, err := OpenStandardLink(offset, bufferCount, bufferSize)
	if err != nil {
		t.Fatalf("Failed to create secondary link: %v", err)
	}
	defer secondary.Close()

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

	// Calculate required memory size
	size := SizeStandardLink(bufferCount, bufferSize)
	backing, offset := createAlignedBuffer(t, size)
	defer runtime.KeepAlive(backing)

	// Create advanced link
	advLink, err := NewAdvancedLink(offset, bufferCount, bufferSize)
	if err != nil {
		t.Fatalf("Failed to create advanced link: %v", err)
	}
	defer advLink.Close()

	if advLink.GetType() != LinkTypeAdvanced {
		t.Error("Link should be of type Advanced")
	}

	if advLink.GetProtocolState() != ProtocolStateNone {
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

	// Calculate required memory size
	size := SizeStandardLink(bufferCount, bufferSize)
	backing, offset := createAlignedBuffer(t, size)
	defer runtime.KeepAlive(backing)

	// Create advanced links
	primary, err := NewAdvancedLink(offset, bufferCount, bufferSize)
	if err != nil {
		t.Fatalf("Failed to create primary advanced link: %v", err)
	}
	defer primary.Close()

	secondary, err := NewAdvancedLink(offset, bufferCount, bufferSize)
	if err != nil {
		t.Fatalf("Failed to create secondary advanced link: %v", err)
	}
	defer secondary.Close()

	// Test negotiation
	ctx := context.Background()
	version := ProtocolVersion{Major: 1, Minor: 0}
	features := FeatureLargeCopy | FeatureRequestResponse

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
	if primary.GetProtocolState() != ProtocolStateNegotiated {
		t.Errorf("Primary protocol state should be Negotiated, got %v", primary.GetProtocolState())
	}
	if secondary.GetProtocolState() != ProtocolStateNegotiated {
		t.Errorf("Secondary protocol state should be Negotiated, got %v", secondary.GetProtocolState())
	}

	// Check negotiated version
	negotiatedVersion := primary.GetNegotiatedVersion()
	if negotiatedVersion.Major != 1 || negotiatedVersion.Minor != 0 {
		t.Errorf("Expected version 1.0, got %d.%d", negotiatedVersion.Major, negotiatedVersion.Minor)
	}

	// Check negotiated features
	negotiatedFeatures := primary.GetNegotiatedFeatures()
	if (negotiatedFeatures & FeatureLargeCopy) == 0 {
		t.Error("large-copy feature should be negotiated")
	}
	if (negotiatedFeatures & FeatureRequestResponse) == 0 {
		t.Error("request-response feature should be negotiated")
	}

	// Check feature flags
	if !primary.IsLargeCopyEnabled() {
		t.Error("large-copy should be enabled")
	}
	if !primary.IsRequestResponseEnabled() {
		t.Error("request-response should be enabled")
	}
}

// TestAdvancedLinkReadWrite tests read/write functionality with advanced links
// It verifies that data transfer works after protocol negotiation
func TestAdvancedLinkReadWrite(t *testing.T) {
	bufferCount := 1024
	bufferSize := 4096

	// Calculate required memory size
	size := SizeStandardLink(bufferCount, bufferSize)
	backing, offset := createAlignedBuffer(t, size)
	defer runtime.KeepAlive(backing)

	// Create advanced links
	primary, err := NewAdvancedLink(offset, bufferCount, bufferSize)
	if err != nil {
		t.Fatalf("Failed to create primary advanced link: %v", err)
	}
	defer primary.Close()

	secondary, err := NewAdvancedLink(offset, bufferCount, bufferSize)
	if err != nil {
		t.Fatalf("Failed to create secondary advanced link: %v", err)
	}
	defer secondary.Close()

	// Perform protocol negotiation with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	version := ProtocolVersion{Major: 1, Minor: 0}
	features := FeatureLargeCopy | FeatureRequestResponse

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

	// Check negotiation results
	if primaryErr != nil {
		t.Fatalf("Primary negotiation failed: %v", primaryErr)
	}
	if secondaryErr != nil {
		t.Fatalf("Secondary negotiation failed: %v", secondaryErr)
	}

	if !primarySuccess || !secondarySuccess {
		t.Fatal("Protocol negotiation should have succeeded")
	}

	// Verify negotiation was successful
	if primary.GetProtocolState() != ProtocolStateNegotiated {
		t.Errorf("Primary protocol state should be Negotiated, got %v", primary.GetProtocolState())
	}
	if secondary.GetProtocolState() != ProtocolStateNegotiated {
		t.Errorf("Secondary protocol state should be Negotiated, got %v", secondary.GetProtocolState())
	}

	// Check negotiated version
	negotiatedVersion := primary.GetNegotiatedVersion()
	if negotiatedVersion.Major != 1 || negotiatedVersion.Minor != 0 {
		t.Errorf("Expected version 1.0, got %d.%d", negotiatedVersion.Major, negotiatedVersion.Minor)
	}

	// Check negotiated features
	negotiatedFeatures := primary.GetNegotiatedFeatures()
	if (negotiatedFeatures & FeatureLargeCopy) == 0 {
		t.Error("large-copy feature should be negotiated")
	}
	if (negotiatedFeatures & FeatureRequestResponse) == 0 {
		t.Error("request-response feature should be negotiated")
	}

	// Check feature flags
	if !primary.IsLargeCopyEnabled() {
		t.Error("large-copy should be enabled")
	}
	if !primary.IsRequestResponseEnabled() {
		t.Error("request-response should be enabled")
	}
}

func negotiateAdvancedPair(t *testing.T, primary, secondary *AdvancedLink) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	version := ProtocolVersion{Major: 1, Minor: 0}
	features := FeatureLargeCopy | FeatureRequestResponse

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

func newAdvancedPair(t *testing.T, bufferCount, bufferSize int) ([]byte, *AdvancedLink, *AdvancedLink) {
	t.Helper()
	size := SizeStandardLink(bufferCount, bufferSize)
	backing, offset := createAlignedBuffer(t, size)

	primary, err := NewAdvancedLink(offset, bufferCount, bufferSize)
	if err != nil {
		t.Fatalf("Failed to create primary advanced link: %v", err)
	}
	secondary, err := NewAdvancedLink(offset, bufferCount, bufferSize)
	if err != nil {
		primary.Close()
		t.Fatalf("Failed to create secondary advanced link: %v", err)
	}
	return backing, primary, secondary
}

func TestAdvancedLinkLargeCopy(t *testing.T) {
	backing, primary, secondary := newAdvancedPair(t, 8, 16)
	defer runtime.KeepAlive(backing)
	defer primary.Close()
	defer secondary.Close()
	negotiateAdvancedPair(t, primary, secondary)

	payload := make([]byte, 97)
	for i := range payload {
		payload[i] = byte(i*17 + 3)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	messageID, err := primary.SendLarge(ctx, 0, payload)
	if err != nil {
		t.Fatalf("SendLarge failed: %v", err)
	}
	message, err := secondary.Receive(ctx)
	if err != nil {
		t.Fatalf("Receive failed: %v", err)
	}
	if !message.IsData() {
		t.Fatalf("message flags = %#x, want data", message.Flags)
	}
	if message.MessageID != messageID {
		t.Fatalf("message id = %d, want %d", message.MessageID, messageID)
	}
	if string(message.Payload) != string(payload) {
		t.Fatalf("payload mismatch: got %x want %x", message.Payload, payload)
	}
}

func TestAdvancedLinkRequestResponse(t *testing.T) {
	backing, primary, secondary := newAdvancedPair(t, 16, 16)
	defer runtime.KeepAlive(backing)
	defer primary.Close()
	defer secondary.Close()
	negotiateAdvancedPair(t, primary, secondary)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	requestPayload := []byte("request payload that spans multiple chunks")
	responsePayload := []byte("response payload that also spans multiple chunks")
	serverErr := make(chan error, 1)
	go func() {
		request, err := secondary.Receive(ctx)
		if err != nil {
			serverErr <- err
			return
		}
		if !request.IsRequest() {
			serverErr <- fmt.Errorf("received flags %#x, want request", request.Flags)
			return
		}
		if request.MethodID != 42 {
			serverErr <- fmt.Errorf("method = %d, want 42", request.MethodID)
			return
		}
		if string(request.Payload) != string(requestPayload) {
			serverErr <- fmt.Errorf("request payload = %q, want %q", request.Payload, requestPayload)
			return
		}
		serverErr <- secondary.Respond(ctx, request, 200, responsePayload)
	}()

	response, err := primary.Call(ctx, 42, requestPayload)
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}
	if string(response) != string(responsePayload) {
		t.Fatalf("response = %q, want %q", response, responsePayload)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("server failed: %v", err)
	}
}

func TestAdvancedLinkErrorResponse(t *testing.T) {
	backing, primary, secondary := newAdvancedPair(t, 8, 32)
	defer runtime.KeepAlive(backing)
	defer primary.Close()
	defer secondary.Close()
	negotiateAdvancedPair(t, primary, secondary)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	serverErr := make(chan error, 1)
	go func() {
		request, err := secondary.Receive(ctx)
		if err != nil {
			serverErr <- err
			return
		}
		serverErr <- secondary.RespondError(ctx, request, 404, []byte("missing method"))
	}()

	_, err := primary.Call(ctx, 99, []byte("hello"))
	if err == nil {
		t.Fatal("Call succeeded, want error response")
	}
	responseErr, ok := err.(AdvancedResponseError)
	if !ok {
		t.Fatalf("Call error = %T %v, want AdvancedResponseError", err, err)
	}
	if responseErr.Status != 404 || string(responseErr.Payload) != "missing method" {
		t.Fatalf("response error = status %d payload %q", responseErr.Status, responseErr.Payload)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("server failed: %v", err)
	}
}

func TestAdvancedLinkEmptyRequestResponse(t *testing.T) {
	backing, primary, secondary := newAdvancedPair(t, 8, 32)
	defer runtime.KeepAlive(backing)
	defer primary.Close()
	defer secondary.Close()
	negotiateAdvancedPair(t, primary, secondary)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	serverErr := make(chan error, 1)
	go func() {
		request, err := secondary.Receive(ctx)
		if err != nil {
			serverErr <- err
			return
		}
		if !request.IsRequest() || len(request.Payload) != 0 {
			serverErr <- fmt.Errorf("request flags=%#x payload=%q", request.Flags, request.Payload)
			return
		}
		serverErr <- secondary.Respond(ctx, request, 204, nil)
	}()

	response, err := primary.Call(ctx, 1, nil)
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}
	if len(response) != 0 {
		t.Fatalf("response payload = %q, want empty", response)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("server failed: %v", err)
	}
}

// TestAdvancedLinkStatistics tests statistics collection for advanced links
// It verifies that statistics are properly initialized and tracked
func TestAdvancedLinkStatistics(t *testing.T) {
	bufferCount := 1024
	bufferSize := 4096

	// Calculate required memory size
	size := SizeStandardLink(bufferCount, bufferSize)
	backing, offset := createAlignedBuffer(t, size)
	defer runtime.KeepAlive(backing)

	// Create advanced link
	advLink, err := NewAdvancedLink(offset, bufferCount, bufferSize)
	if err != nil {
		t.Fatalf("Failed to create advanced link: %v", err)
	}
	defer advLink.Close()

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
	backing, offset := createAlignedBuffer(t, 4096)
	defer runtime.KeepAlive(backing)
	// Create misalignment by adding 1
	offset++

	_, err := OpenStandardLink(offset, 1024, 4096)
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

func TestStandardLinkDeadlines(t *testing.T) {
	bufferCount := 2
	bufferSize := 64
	size := SizeStandardLink(bufferCount, bufferSize)
	backing, offset := createAlignedBuffer(t, size)
	defer runtime.KeepAlive(backing)

	primary, err := OpenStandardLink(offset, bufferCount, bufferSize)
	if err != nil {
		t.Fatalf("Failed to create primary link: %v", err)
	}
	defer primary.Close()

	secondary, err := OpenStandardLink(offset, bufferCount, bufferSize)
	if err != nil {
		t.Fatalf("Failed to create secondary link: %v", err)
	}
	defer secondary.Close()

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
	size := SizeStandardLink(bufferCount, bufferSize)
	backing, offset := createAlignedBuffer(t, size)
	defer runtime.KeepAlive(backing)

	primary, err := OpenStandardLink(offset, bufferCount, bufferSize)
	if err != nil {
		t.Fatalf("Failed to create primary link: %v", err)
	}
	defer primary.Close()

	secondary, err := OpenStandardLink(offset, bufferCount, bufferSize)
	if err != nil {
		t.Fatalf("Failed to create secondary link: %v", err)
	}
	defer secondary.Close()

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
	size := SizeStandardLink(bufferCount, bufferSize)
	backing, offset := createAlignedBuffer(t, size)
	defer runtime.KeepAlive(backing)

	primary, err := OpenStandardLink(offset, bufferCount, bufferSize)
	if err != nil {
		t.Fatalf("Failed to create primary link: %v", err)
	}
	defer primary.Close()

	secondary, err := OpenStandardLink(offset, bufferCount, bufferSize)
	if err != nil {
		t.Fatalf("Failed to create secondary link: %v", err)
	}
	defer secondary.Close()

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

	// Calculate required memory size
	size := SizeStandardLink(bufferCount, bufferSize)
	backing, offset := createAlignedBuffer(t, size)
	defer runtime.KeepAlive(backing)

	// Create advanced link
	advLink, err := NewAdvancedLink(offset, bufferCount, bufferSize)
	if err != nil {
		t.Fatalf("Failed to create advanced link: %v", err)
	}
	defer advLink.Close()

	// Test negotiation with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	version := ProtocolVersion{Major: 1, Minor: 0}
	features := FeatureLargeCopy

	success, err := advLink.NegotiateProtocol(ctx, version, features)
	if err == nil {
		t.Error("Expected timeout error")
	}
	if success {
		t.Error("Negotiation should not succeed without peer")
	}

	if advLink.GetProtocolState() != ProtocolStateFailed {
		t.Errorf("Protocol state should be Failed, got %v", advLink.GetProtocolState())
	}
}

// TestConcurrentReadWrite tests concurrent read/write operations
// It verifies that multiple goroutines can safely use the link simultaneously
func TestConcurrentReadWrite(t *testing.T) {
	bufferCount := 1024
	bufferSize := 4096

	// Calculate required memory size
	size := SizeStandardLink(bufferCount, bufferSize)
	backing, offset := createAlignedBuffer(t, size)
	defer runtime.KeepAlive(backing)

	// Create links
	primary, err := OpenStandardLink(offset, bufferCount, bufferSize)
	if err != nil {
		t.Fatalf("Failed to create primary link: %v", err)
	}
	defer primary.Close()

	secondary, err := OpenStandardLink(offset, bufferCount, bufferSize)
	if err != nil {
		t.Fatalf("Failed to create secondary link: %v", err)
	}
	defer secondary.Close()

	// Test concurrent read/write
	const numMessages = 100
	messageSize := 100

	// Use channels to synchronize and verify data
	writeChan := make(chan int, numMessages)
	readChan := make(chan int, numMessages)
	errorChan := make(chan error, 2)

	var wg sync.WaitGroup
	wg.Add(2)

	// Writer goroutine
	go func() {
		defer wg.Done()
		for i := 0; i < numMessages; i++ {
			data := make([]byte, messageSize)
			for j := 0; j < messageSize; j++ {
				data[j] = byte(i % 256)
			}

			n, err := primary.Write(data)
			if err != nil {
				errorChan <- fmt.Errorf("write failed at message %d: %v", i, err)
				return
			}
			if n != messageSize {
				errorChan <- fmt.Errorf("expected to write %d bytes, wrote %d at message %d", messageSize, n, i)
				return
			}

			writeChan <- i
		}
		close(writeChan)
	}()

	// Reader goroutine
	go func() {
		defer wg.Done()
		readBuffer := make([]byte, messageSize)
		for i := 0; i < numMessages; i++ {
			n, err := secondary.Read(readBuffer)
			if err != nil {
				errorChan <- fmt.Errorf("read failed at message %d: %v", i, err)
				return
			}
			if n != messageSize {
				errorChan <- fmt.Errorf("expected to read %d bytes, got %d at message %d", messageSize, n, i)
				return
			}

			// Verify the data content
			expectedByte := byte(i % 256)
			for j, b := range readBuffer {
				if b != expectedByte {
					errorChan <- fmt.Errorf("data mismatch at message %d, byte %d: expected %d, got %d", i, j, expectedByte, b)
					return
				}
			}

			readChan <- i
		}
		close(readChan)
	}()

	// Wait for completion
	wg.Wait()
	close(errorChan)

	// Check for errors
	for err := range errorChan {
		t.Error(err)
	}

	// Verify all messages were written and read
	writeCount := 0
	for range writeChan {
		writeCount++
	}
	if writeCount != numMessages {
		t.Errorf("Expected %d messages to be written, got %d", numMessages, writeCount)
	}

	readCount := 0
	for range readChan {
		readCount++
	}
	if readCount != numMessages {
		t.Errorf("Expected %d messages to be read, got %d", numMessages, readCount)
	}

}

// BenchmarkStandardLinkReadWrite benchmarks the standard link read/write performance
// It measures throughput for basic data transfer operations
func BenchmarkStandardLinkReadWrite(b *testing.B) {
	bufferCount := 1024
	bufferSize := 1 << 12

	// Calculate required memory size
	size := SizeStandardLink(bufferCount, bufferSize)
	backing, offset := createAlignedBuffer(nil, size)
	defer runtime.KeepAlive(backing)

	// Create links
	primary, err := OpenStandardLink(offset, bufferCount, bufferSize)
	if err != nil {
		b.Fatalf("Failed to create primary link: %v", err)
	}
	defer primary.Close()

	secondary, err := OpenStandardLink(offset, bufferCount, bufferSize)
	if err != nil {
		b.Fatalf("Failed to create secondary link: %v", err)
	}
	defer secondary.Close()

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

	size := SizeStandardLink(bufferCount, bufferSize)
	backing, offset := createAlignedBuffer(nil, size)
	defer runtime.KeepAlive(backing)

	primary, err := OpenStandardLink(offset, bufferCount, bufferSize)
	if err != nil {
		b.Fatalf("Failed to create primary link: %v", err)
	}
	defer primary.Close()

	secondary, err := OpenStandardLink(offset, bufferCount, bufferSize)
	if err != nil {
		b.Fatalf("Failed to create secondary link: %v", err)
	}
	defer secondary.Close()

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

	size := SizeStandardLink(bufferCount, bufferSize)
	backing, offset := createAlignedBuffer(nil, size)
	defer runtime.KeepAlive(backing)

	primary, err := OpenStandardLink(offset, bufferCount, bufferSize)
	if err != nil {
		b.Fatalf("Failed to create primary link: %v", err)
	}
	defer primary.Close()

	secondary, err := OpenStandardLink(offset, bufferCount, bufferSize)
	if err != nil {
		b.Fatalf("Failed to create secondary link: %v", err)
	}
	defer secondary.Close()

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

	size := SizeStandardLink(bufferCount, bufferSize)
	backing, offset := createAlignedBuffer(nil, size)
	defer runtime.KeepAlive(backing)

	primary, err := OpenStandardLink(offset, bufferCount, bufferSize)
	if err != nil {
		b.Fatalf("Failed to create primary link: %v", err)
	}
	defer primary.Close()

	secondary, err := OpenStandardLink(offset, bufferCount, bufferSize)
	if err != nil {
		b.Fatalf("Failed to create secondary link: %v", err)
	}
	defer secondary.Close()

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
	size := SizeStandardLink(bufferCount, bufferSize)
	backing, offset := createAlignedBuffer(nil, size)
	defer runtime.KeepAlive(backing)

	primary, err := OpenStandardLink(offset, bufferCount, bufferSize)
	if err != nil {
		b.Fatalf("Failed to create primary link: %v", err)
	}
	defer primary.Close()

	secondary, err := OpenStandardLink(offset, bufferCount, bufferSize)
	if err != nil {
		b.Fatalf("Failed to create secondary link: %v", err)
	}
	defer secondary.Close()

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

	size := SizeStandardLink(bufferCount, bufferSize)
	backing, offset := createAlignedBuffer(nil, size)
	defer runtime.KeepAlive(backing)

	primary, err := OpenStandardLink(offset, bufferCount, bufferSize)
	if err != nil {
		b.Fatalf("Failed to create primary link: %v", err)
	}
	defer primary.Close()

	secondary, err := OpenStandardLink(offset, bufferCount, bufferSize)
	if err != nil {
		b.Fatalf("Failed to create secondary link: %v", err)
	}
	defer secondary.Close()

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

	// Calculate required memory size
	size := SizeStandardLink(bufferCount, bufferSize)

	// Create aligned buffer
	backing, offset := createAlignedBuffer(t, size)
	defer runtime.KeepAlive(backing)

	// Create links
	primary, err := OpenStandardLink(offset, bufferCount, bufferSize)
	if err != nil {
		t.Fatalf("Failed to create primary link: %v", err)
	}
	defer primary.Close()

	// Test data larger than buffer size
	testData := make([]byte, bufferSize+100) // 100 bytes larger than buffer

	// Write from primary - should fail with buffer overflow error
	_, err = primary.Write(testData)
	if err != ErrBufferOverflow {
		t.Errorf("Expected ErrBufferOverflow, got %v", err)
	}
}

// TestStandardLinkPacketConn tests PacketConn interface implementation
// It verifies that StandardLink properly implements net.PacketConn
func TestStandardLinkPacketConn(t *testing.T) {
	bufferCount := 1024
	bufferSize := 4096

	// Calculate required memory size
	size := SizeStandardLink(bufferCount, bufferSize)

	// Create aligned buffer
	backing, offset := createAlignedBuffer(t, size)
	defer runtime.KeepAlive(backing)

	// Create links
	primary, err := OpenStandardLink(offset, bufferCount, bufferSize)
	if err != nil {
		t.Fatalf("Failed to create primary link: %v", err)
	}
	defer primary.Close()

	secondary, err := OpenStandardLink(offset, bufferCount, bufferSize)
	if err != nil {
		t.Fatalf("Failed to create secondary link: %v", err)
	}
	defer secondary.Close()

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
	err = primary.SetDeadline(time.Now().Add(time.Second))
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

func BenchmarkAdvancedLinkLargeCopy(b *testing.B) {
	const (
		bufferCount = 1024
		bufferSize  = 256
		payloadSize = 4096
	)
	size := SizeStandardLink(bufferCount, bufferSize)
	backing, offset := createAlignedBuffer(nil, size)
	defer runtime.KeepAlive(backing)

	primary, err := NewAdvancedLink(offset, bufferCount, bufferSize)
	if err != nil {
		b.Fatalf("Failed to create primary advanced link: %v", err)
	}
	defer primary.Close()
	secondary, err := NewAdvancedLink(offset, bufferCount, bufferSize)
	if err != nil {
		b.Fatalf("Failed to create secondary advanced link: %v", err)
	}
	defer secondary.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	negotiateAdvancedPairForBenchmark(b, ctx, primary, secondary)
	cancel()

	payload := make([]byte, payloadSize)
	b.SetBytes(payloadSize)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ctx := context.Background()
		if _, err := primary.SendLarge(ctx, 0, payload); err != nil {
			b.Fatalf("SendLarge failed: %v", err)
		}
		message, err := secondary.Receive(ctx)
		if err != nil {
			b.Fatalf("Receive failed: %v", err)
		}
		if len(message.Payload) != payloadSize {
			b.Fatalf("payload size = %d, want %d", len(message.Payload), payloadSize)
		}
	}
}

func BenchmarkAdvancedLinkRequestResponse(b *testing.B) {
	const (
		bufferCount = 1024
		bufferSize  = 256
		payloadSize = 1024
	)
	size := SizeStandardLink(bufferCount, bufferSize)
	backing, offset := createAlignedBuffer(nil, size)
	defer runtime.KeepAlive(backing)

	primary, err := NewAdvancedLink(offset, bufferCount, bufferSize)
	if err != nil {
		b.Fatalf("Failed to create primary advanced link: %v", err)
	}
	defer primary.Close()
	secondary, err := NewAdvancedLink(offset, bufferCount, bufferSize)
	if err != nil {
		b.Fatalf("Failed to create secondary advanced link: %v", err)
	}
	defer secondary.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	negotiateAdvancedPairForBenchmark(b, ctx, primary, secondary)
	cancel()

	request := make([]byte, payloadSize)
	response := make([]byte, payloadSize)
	done := make(chan error, 1)
	go func() {
		for i := 0; i < b.N; i++ {
			message, err := secondary.Receive(context.Background())
			if err != nil {
				done <- err
				return
			}
			if err := secondary.Respond(context.Background(), message, 0, response); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()

	b.SetBytes(payloadSize * 2)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		reply, err := primary.Call(context.Background(), 7, request)
		if err != nil {
			b.Fatalf("Call failed: %v", err)
		}
		if len(reply) != payloadSize {
			b.Fatalf("reply size = %d, want %d", len(reply), payloadSize)
		}
	}
	if err := <-done; err != nil {
		b.Fatalf("server failed: %v", err)
	}
}

func negotiateAdvancedPairForBenchmark(b *testing.B, ctx context.Context, primary, secondary *AdvancedLink) {
	b.Helper()
	var wg sync.WaitGroup
	wg.Add(2)
	var primaryErr, secondaryErr error
	go func() {
		defer wg.Done()
		_, primaryErr = primary.NegotiateProtocol(ctx, ProtocolVersion{Major: 1, Minor: 0}, FeatureLargeCopy|FeatureRequestResponse)
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
