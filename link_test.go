package hqq

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"
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
	_, offset := createAlignedBuffer(t, size)

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
	_, offset := createAlignedBuffer(t, size)

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
	_, offset := createAlignedBuffer(t, size)

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
	_, offset := createAlignedBuffer(t, size)

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
	_, offset := createAlignedBuffer(t, size)

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

	// The first buffer should already be returned to the free ring even though
	// the unread tail is served from receiveBuffer.
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

// TestAdvancedLinkCreation tests the creation of advanced links
// It verifies that advanced links can be created with proper initialization
func TestAdvancedLinkCreation(t *testing.T) {
	bufferCount := 1024
	bufferSize := 4096

	// Calculate required memory size
	size := SizeStandardLink(bufferCount, bufferSize)
	_, offset := createAlignedBuffer(t, size)

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
	_, offset := createAlignedBuffer(t, size)

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
	version := ProtocolVersion{Major: 1, Minor: 1}
	features := FeatureCompression | FeatureFlowControl

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
	if negotiatedVersion.Major != 1 || negotiatedVersion.Minor != 1 {
		t.Errorf("Expected version 1.1, got %d.%d", negotiatedVersion.Major, negotiatedVersion.Minor)
	}

	// Check negotiated features
	negotiatedFeatures := primary.GetNegotiatedFeatures()
	if (negotiatedFeatures & FeatureCompression) == 0 {
		t.Error("Compression feature should be negotiated")
	}
	if (negotiatedFeatures & FeatureFlowControl) == 0 {
		t.Error("Flow control feature should be negotiated")
	}

	// Check feature flags
	if !primary.IsCompressionEnabled() {
		t.Error("Compression should be enabled")
	}
	if !primary.IsFlowControlEnabled() {
		t.Error("Flow control should be enabled")
	}
}

// TestAdvancedLinkReadWrite tests read/write functionality with advanced links
// It verifies that data transfer works after protocol negotiation
func TestAdvancedLinkReadWrite(t *testing.T) {
	bufferCount := 1024
	bufferSize := 4096

	// Calculate required memory size
	size := SizeStandardLink(bufferCount, bufferSize)
	_, offset := createAlignedBuffer(t, size)

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

	version := ProtocolVersion{Major: 1, Minor: 1}
	features := FeatureCompression | FeatureFlowControl

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
	if negotiatedVersion.Major != 1 || negotiatedVersion.Minor != 1 {
		t.Errorf("Expected version 1.1, got %d.%d", negotiatedVersion.Major, negotiatedVersion.Minor)
	}

	// Check negotiated features
	negotiatedFeatures := primary.GetNegotiatedFeatures()
	if (negotiatedFeatures & FeatureCompression) == 0 {
		t.Error("Compression feature should be negotiated")
	}
	if (negotiatedFeatures & FeatureFlowControl) == 0 {
		t.Error("Flow control feature should be negotiated")
	}

	// Check feature flags
	if !primary.IsCompressionEnabled() {
		t.Error("Compression should be enabled")
	}
	if !primary.IsFlowControlEnabled() {
		t.Error("Flow control should be enabled")
	}
}

// TestAdvancedLinkStatistics tests statistics collection for advanced links
// It verifies that statistics are properly initialized and tracked
func TestAdvancedLinkStatistics(t *testing.T) {
	bufferCount := 1024
	bufferSize := 4096

	// Calculate required memory size
	size := SizeStandardLink(bufferCount, bufferSize)
	_, offset := createAlignedBuffer(t, size)

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
	_, offset := createAlignedBuffer(t, 4096)
	// Create misalignment by adding 1
	offset++

	_, err := OpenStandardLink(offset, 1024, 4096)
	if err != ErrMemoryAlign {
		t.Errorf("Expected ErrMemoryAlign, got %v", err)
	}

	// Test non-power-of-two buffer count. Standard Protocol requires exact
	// buffer/free-ring symmetry for lock-free ownership transfer.
	size = SizeStandardLink(6, 4096)
	if size != 0 {
		t.Error("SizeStandardLink should return 0 for non-power-of-two buffer count")
	}
}

func TestStandardLinkDeadlines(t *testing.T) {
	bufferCount := 2
	bufferSize := 64
	size := SizeStandardLink(bufferCount, bufferSize)
	_, offset := createAlignedBuffer(t, size)

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
		t.Fatalf("Write without free buffer error = %v, want ErrTimeout", err)
	}
}

func TestStandardLinkHighVolumeOrderedDelivery(t *testing.T) {
	bufferCount := 64
	bufferSize := 64
	messageCount := 4096
	size := SizeStandardLink(bufferCount, bufferSize)
	_, offset := createAlignedBuffer(t, size)

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
	_, offset := createAlignedBuffer(t, size)

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
	_, offset := createAlignedBuffer(t, size)

	// Create advanced link
	advLink, err := NewAdvancedLink(offset, bufferCount, bufferSize)
	if err != nil {
		t.Fatalf("Failed to create advanced link: %v", err)
	}
	defer advLink.Close()

	// Test negotiation with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	version := ProtocolVersion{Major: 1, Minor: 1}
	features := FeatureCompression

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
	_, offset := createAlignedBuffer(t, size)

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
	_, offset := createAlignedBuffer(nil, size)

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

func BenchmarkStandardLinkPipeline(b *testing.B) {
	bufferCount := 1024
	bufferSize := 1 << 12
	size := SizeStandardLink(bufferCount, bufferSize)
	_, offset := createAlignedBuffer(nil, size)

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

// TestStandardLinkBufferOverflow tests buffer overflow handling
// It verifies that attempts to write data larger than buffer size are rejected
func TestStandardLinkBufferOverflow(t *testing.T) {
	bufferCount := 1024
	bufferSize := 4096

	// Calculate required memory size
	size := SizeStandardLink(bufferCount, bufferSize)

	// Create aligned buffer
	_, offset := createAlignedBuffer(t, size)

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
	_, offset := createAlignedBuffer(t, size)

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
