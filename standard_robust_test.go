package hqq

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func recoverPanic(f func()) (v any) {
	defer func() { v = recover() }()
	f()
	return nil
}

// TestWriteZeroCopyPanicPublishesTombstone checks that a panicking
// WriteZeroCopy callback neither wedges the ring nor shows readers an error
// packet: the abandoned slot is skipped like any tombstone.
func TestWriteZeroCopyPanicPublishesTombstone(t *testing.T) {
	_, writer, reader := newMemoryPair(t, 2, 64, LinkOptions{})

	// More panics than ring slots, each followed by a real message: the
	// readers must skip the tombstone and deliver the message.
	buf := make([]byte, 64)
	for i := 0; i < 5; i++ {
		got := recoverPanic(func() {
			_, _ = writer.WriteZeroCopy(func(b []byte) (int, error) {
				copy(b, "garbage")
				panic("boom")
			})
		})
		if got != "boom" {
			t.Fatalf("panic %d recovered %v, want boom", i, got)
		}
		if _, err := writer.Write([]byte("after")); err != nil {
			t.Fatalf("Write after panic %d: %v", i, err)
		}
		n, err := reader.Read(buf)
		if err != nil || string(buf[:n]) != "after" {
			t.Fatalf("Read after panic %d = %q, %v; want \"after\"", i, buf[:n], err)
		}
	}

	// Same through the zero-copy and reservation readers.
	_ = recoverPanic(func() {
		_, _ = writer.WriteZeroCopy(func([]byte) (int, error) { panic("boom") })
	})
	if _, err := writer.Write([]byte("again")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	var got string
	if _, err := reader.ReadZeroCopy(func(b []byte) error { got = string(b); return nil }); err != nil || got != "again" {
		t.Fatalf("ReadZeroCopy = %q, %v; want \"again\"", got, err)
	}
	_ = recoverPanic(func() {
		_, _ = writer.WriteZeroCopy(func([]byte) (int, error) { panic("boom") })
	})
	if _, err := writer.Write([]byte("third")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	res, err := reader.ReserveRead()
	if err != nil || string(res.Buffer()) != "third" {
		t.Fatalf("ReserveRead = %q, %v; want \"third\"", res.Buffer(), err)
	}
	if err := res.Release(); err != nil {
		t.Fatal(err)
	}
}

// TestWriteZeroCopyGoexitPublishesTombstone covers runtime.Goexit, which is
// what testing.T.FailNow does inside a callback.
func TestWriteZeroCopyGoexitPublishesTombstone(t *testing.T) {
	_, writer, reader := newMemoryPair(t, 2, 64, LinkOptions{})
	buf := make([]byte, 64)
	for i := 0; i < 3; i++ {
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = writer.WriteZeroCopy(func([]byte) (int, error) {
				runtime.Goexit()
				return 0, nil
			})
		}()
		<-done
		if _, err := writer.Write([]byte("ok")); err != nil {
			t.Fatalf("Write after Goexit %d: %v", i, err)
		}
		if n, err := reader.Read(buf); err != nil || string(buf[:n]) != "ok" {
			t.Fatalf("Read after Goexit %d = %q, %v; want \"ok\"", i, buf[:n], err)
		}
	}
}

// TestReadZeroCopyPanicReleasesSlot checks that a panicking ReadZeroCopy
// callback consumes its message and frees the slot, both for ring slots and
// for the buffered tail of a partially read message.
func TestReadZeroCopyPanicReleasesSlot(t *testing.T) {
	_, writer, reader := newMemoryPair(t, 2, 64, LinkOptions{})

	for i := 0; i < 5; i++ {
		if _, err := writer.Write([]byte("poison")); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
		got := recoverPanic(func() {
			_, _ = reader.ReadZeroCopy(func([]byte) error { panic("boom") })
		})
		if got != "boom" {
			t.Fatalf("panic %d recovered %v, want boom", i, got)
		}
	}
	if _, err := writer.Write([]byte("fine")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	buf := make([]byte, 64)
	if n, err := reader.Read(buf); err != nil || string(buf[:n]) != "fine" {
		t.Fatalf("Read = %q, %v; want \"fine\"", buf[:n], err)
	}

	// A message that did not fit into Read's buffer leaves a tail; a panic
	// while consuming it must not lose the buffer's backing store.
	if _, err := writer.Write([]byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	small := make([]byte, 4)
	if n, err := reader.Read(small); err != nil || n != 4 {
		t.Fatalf("partial Read = %d, %v", n, err)
	}
	_ = recoverPanic(func() {
		_, _ = reader.ReadZeroCopy(func([]byte) error { panic("boom") })
	})
	if _, err := writer.Write([]byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	if n, err := reader.Read(small); err != nil || string(small[:n]) != "0123" {
		t.Fatalf("Read after tail panic = %q, %v", small[:n], err)
	}
	var tail string
	if _, err := reader.ReadZeroCopy(func(b []byte) error { tail = string(b); return nil }); err != nil || tail != "456789" {
		t.Fatalf("remainder = %q, %v; want 456789", tail, err)
	}
}

// TestNamedLinkBlockedCallsWakePromptly exercises the wake path of named
// segments (futexes on Linux and macOS, named kernel semaphores on
// Windows): a parked reader or writer must be woken by its peer and
// complete. The generous bound only detects a hang; it deliberately does not
// measure wake-up latency, which is not reproducible on shared CI machines.
func TestNamedLinkBlockedCallsWakePromptly(t *testing.T) {
	name := newPortableSharedMemoryName()
	primary, err := CreateStandardLink(name, 2, 64, 0o600, LinkOptions{})
	if err != nil {
		t.Fatalf("CreateStandardLink: %v", err)
	}
	defer primary.Close()
	secondary, err := OpenStandardLink(name, 2, 64, LinkOptions{})
	if err != nil {
		t.Fatalf("OpenStandardLink: %v", err)
	}
	defer secondary.Close()

	type readResult struct {
		msg string
		err error
	}
	readCh := make(chan readResult, 1)
	go func() {
		buf := make([]byte, 64)
		n, err := secondary.Read(buf)
		readCh <- readResult{string(buf[:n]), err}
	}()
	time.Sleep(50 * time.Millisecond) // let the reader park
	if _, err := primary.Write([]byte("wake")); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-readCh:
		if r.err != nil || r.msg != "wake" {
			t.Fatalf("Read = %q, %v", r.msg, r.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("parked reader was never woken")
	}

	// Fill the ring, park a writer, then make room.
	for i := 0; i < 2; i++ {
		if _, err := primary.Write([]byte("fill")); err != nil {
			t.Fatal(err)
		}
	}
	writeCh := make(chan error, 1)
	go func() {
		_, err := primary.Write([]byte("blocked"))
		writeCh <- err
	}()
	time.Sleep(50 * time.Millisecond)
	if _, err := secondary.Read(make([]byte, 64)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-writeCh:
		if err != nil {
			t.Fatalf("blocked Write = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("parked writer was never woken")
	}

	// The peer's Close wakes a parked reader, which then reports EOF.
	closeCh := make(chan error, 1)
	go func() {
		for {
			if _, err := secondary.Read(make([]byte, 64)); err != nil {
				closeCh <- err
				return
			}
		}
	}()
	time.Sleep(50 * time.Millisecond)
	primary.Close()
	if err := waitErr(t, closeCh, 2*time.Second); !errors.Is(err, io.EOF) {
		t.Fatalf("Read after peer Close = %v, want io.EOF", err)
	}
}

func TestRingParkersMatchPlatform(t *testing.T) {
	name := newPortableSharedMemoryName()
	link, err := CreateStandardLink(name, 2, 64, 0o600, LinkOptions{})
	if err != nil {
		t.Fatalf("CreateStandardLink: %v", err)
	}
	defer link.Close()
	for ring, p := range link.parkers {
		if has := p != nil; has != (runtime.GOOS == "windows") {
			t.Fatalf("ring %d parker present = %v on %s", ring, has, runtime.GOOS)
		}
	}
}

func mustWrite(t *testing.T, l *StandardLink, msg string) {
	t.Helper()
	if _, err := l.Write([]byte(msg)); err != nil {
		t.Fatalf("Write(%q): %v", msg, err)
	}
}

func mustReadFrom(t *testing.T, l *StandardLink, size int) (string, net.Addr) {
	t.Helper()
	buf := make([]byte, size)
	n, addr, err := l.ReadFrom(buf)
	if err != nil {
		t.Fatalf("ReadFrom(%d bytes): %v", size, err)
	}
	return string(buf[:n]), addr
}

// TestReadFromIsDatagram: one ReadFrom returns at most one message; bytes
// that do not fit are discarded, never carried over to the next call.
func TestReadFromIsDatagram(t *testing.T) {
	_, primary, secondary := newMemoryPair(t, 4, 64, LinkOptions{})

	mustWrite(t, primary, "0123456789")
	mustWrite(t, primary, "next")
	if got, _ := mustReadFrom(t, secondary, 4); got != "0123" {
		t.Fatalf("truncated ReadFrom = %q, want 0123", got)
	}
	if got, _ := mustReadFrom(t, secondary, 64); got != "next" {
		t.Fatalf("ReadFrom after truncation = %q, want next (the rest of the first datagram must be discarded)", got)
	}

	// An empty buffer still consumes one datagram.
	mustWrite(t, primary, "a")
	mustWrite(t, primary, "b")
	if got, _ := mustReadFrom(t, secondary, 0); got != "" {
		t.Fatalf("empty ReadFrom = %q", got)
	}
	if got, _ := mustReadFrom(t, secondary, 64); got != "b" {
		t.Fatalf("ReadFrom after empty ReadFrom = %q, want b", got)
	}
}

// TestReadFromAddress: the address is the fixed peer endpoint, independent of
// what WriteTo was given, and equal to the peer's LocalAddr.
func TestReadFromAddress(t *testing.T) {
	_, primary, secondary := newMemoryPair(t, 4, 64, LinkOptions{})

	if _, err := primary.WriteTo([]byte("p"), &HQQAddr{LinkID: "somewhere"}); err != nil {
		t.Fatal(err)
	}
	if _, err := secondary.WriteTo([]byte("s"), nil); err != nil {
		t.Fatal(err)
	}
	if _, addr := mustReadFrom(t, secondary, 8); addr.String() != "hqq-primary" || addr.Network() != "hqq" {
		t.Fatalf("secondary ReadFrom addr = %v, want hqq-primary", addr)
	}
	if _, addr := mustReadFrom(t, primary, 8); addr.String() != "hqq-secondary" {
		t.Fatalf("primary ReadFrom addr = %v, want hqq-secondary", addr)
	}
	// Writing to some address does not change what ReadFrom reports.
	if _, err := secondary.WriteTo([]byte("x"), &HQQAddr{LinkID: "elsewhere"}); err != nil {
		t.Fatal(err)
	}
	if _, addr := mustReadFrom(t, primary, 8); addr.String() != "hqq-secondary" {
		t.Fatalf("addr after WriteTo = %v, want hqq-secondary", addr)
	}
	if primary.LocalAddr().String() != "hqq-primary" || secondary.LocalAddr().String() != "hqq-secondary" {
		t.Fatalf("LocalAddr = %v / %v, want hqq-primary / hqq-secondary", primary.LocalAddr(), secondary.LocalAddr())
	}
}

// TestReadFromServesPendingTail: the tail a Read left behind is the next
// datagram, so Read and ReadFrom never merge or reorder messages.
func TestReadFromServesPendingTail(t *testing.T) {
	_, primary, secondary := newMemoryPair(t, 4, 64, LinkOptions{})

	mustWrite(t, primary, "0123456789")
	mustWrite(t, primary, "next")
	if n, err := secondary.Read(make([]byte, 4)); err != nil || n != 4 {
		t.Fatalf("partial Read = %d, %v", n, err)
	}
	if got, _ := mustReadFrom(t, secondary, 64); got != "456789" {
		t.Fatalf("ReadFrom = %q, want the pending tail 456789", got)
	}
	if got, _ := mustReadFrom(t, secondary, 64); got != "next" {
		t.Fatalf("ReadFrom = %q, want next", got)
	}

	// A tail that is too long for ReadFrom's buffer is truncated like any
	// datagram; the rest is not carried over.
	mustWrite(t, primary, "0123456789")
	mustWrite(t, primary, "last")
	if n, err := secondary.Read(make([]byte, 4)); err != nil || n != 4 {
		t.Fatalf("partial Read = %d, %v", n, err)
	}
	if got, _ := mustReadFrom(t, secondary, 2); got != "45" {
		t.Fatalf("truncated tail = %q, want 45", got)
	}
	if got, _ := mustReadFrom(t, secondary, 64); got != "last" {
		t.Fatalf("ReadFrom = %q, want last", got)
	}
}

// TestRemainderTailsNeverMerge forces two queued tails (as two concurrent
// readers that were both mid-message leave behind) and checks that every read
// call returns bytes of exactly one message, in arrival order.
func TestRemainderTailsNeverMerge(t *testing.T) {
	_, primary, secondary := newMemoryPair(t, 4, 64, LinkOptions{})
	twoTails := func() {
		secondary.recv.store([]byte("AAAAAA"))
		secondary.recv.store([]byte("BBBBBB"))
	}
	read := func(size int) string {
		t.Helper()
		buf := make([]byte, size)
		n, err := secondary.Read(buf)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		return string(buf[:n])
	}

	twoTails()
	if got := read(64); got != "AAAAAA" {
		t.Fatalf("first Read = %q, want AAAAAA", got)
	}
	if got := read(4); got != "BBBB" {
		t.Fatalf("second Read = %q, want BBBB", got)
	}
	if got := read(64); got != "BB" {
		t.Fatalf("third Read = %q, want BB", got)
	}

	twoTails()
	var zc string
	if _, err := secondary.ReadZeroCopy(func(b []byte) error { zc = string(b); return nil }); err != nil || zc != "AAAAAA" {
		t.Fatalf("ReadZeroCopy = %q, %v; want AAAAAA", zc, err)
	}
	res, err := secondary.ReserveRead()
	if err != nil || string(res.Buffer()) != "BBBBBB" {
		t.Fatalf("ReserveRead = %q, %v; want BBBBBB", res.Buffer(), err)
	}
	if err := res.Release(); err != nil {
		t.Fatal(err)
	}

	twoTails()
	if got, _ := mustReadFrom(t, secondary, 64); got != "AAAAAA" {
		t.Fatalf("ReadFrom = %q, want AAAAAA", got)
	}
	if got, _ := mustReadFrom(t, secondary, 64); got != "BBBBBB" {
		t.Fatalf("ReadFrom = %q, want BBBBBB", got)
	}

	// Queued tails come before newer messages; afterwards the ring is read again.
	twoTails()
	mustWrite(t, primary, "fresh")
	if got := read(64); got != "AAAAAA" {
		t.Fatalf("Read = %q, want AAAAAA", got)
	}
	if got := read(64); got != "BBBBBB" {
		t.Fatalf("Read = %q, want BBBBBB", got)
	}
	if got := read(64); got != "fresh" {
		t.Fatalf("Read = %q, want fresh", got)
	}
}

// TestConcurrentPartialReadsNeverMixMessages: every message consists of one
// repeated letter; however concurrent readers with small buffers interleave,
// no single Read may return two different letters, and no byte is lost.
func TestConcurrentPartialReadsNeverMixMessages(t *testing.T) {
	_, primary, secondary := newMemoryPair(t, 8, 64, LinkOptions{})
	const (
		messages = 3000
		msgLen   = 48
		readers  = 4
	)
	go func() {
		for i := range messages {
			msg := bytes.Repeat([]byte{byte('a' + i%26)}, msgLen)
			if _, err := primary.Write(msg); err != nil {
				return
			}
		}
	}()

	// One generous deadline for the whole test; the reader that sees the last
	// byte moves it to "now" to wake the others.
	secondary.SetReadDeadline(time.Now().Add(10 * time.Second))
	var total atomic.Int64
	errCh := make(chan error, readers)
	var wg sync.WaitGroup
	for r := range readers {
		wg.Add(1)
		go func(size int) {
			defer wg.Done()
			buf := make([]byte, size)
			for total.Load() < messages*msgLen {
				n, err := secondary.Read(buf)
				if err == ErrTimeout {
					if total.Load() >= messages*msgLen {
						return
					}
					errCh <- errors.New("Read timed out before every byte arrived")
					return
				}
				if err != nil {
					errCh <- err
					return
				}
				if n == 0 || bytes.Count(buf[:n], buf[:1]) != n {
					errCh <- fmt.Errorf("Read mixed messages: %q", buf[:n])
					return
				}
				if total.Add(int64(n)) >= messages*msgLen {
					// Wake the other readers parked on the empty ring.
					secondary.SetReadDeadline(time.Now())
				}
			}
		}(5 + 3*r)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	if got := total.Load(); got != messages*msgLen {
		t.Fatalf("read %d bytes, want %d", got, messages*msgLen)
	}
}

// TestCloseWaitsForRemainderCallback: a ReadZeroCopy callback that serves the
// buffered tail of a message is an in-flight call like any other, so Close
// waits for it.
func TestCloseWaitsForRemainderCallback(t *testing.T) {
	_, primary, secondary := newMemoryPair(t, 4, 64, LinkOptions{})
	mustWrite(t, primary, "0123456789")
	if n, err := secondary.Read(make([]byte, 4)); err != nil || n != 4 {
		t.Fatalf("partial Read = %d, %v", n, err)
	}

	inCallback := make(chan struct{})
	release := make(chan struct{})
	readDone := make(chan error, 1)
	go func() {
		_, err := secondary.ReadZeroCopy(func(b []byte) error {
			close(inCallback)
			<-release
			return nil
		})
		readDone <- err
	}()
	<-inCallback

	closeDone := make(chan struct{})
	go func() {
		secondary.Close()
		close(closeDone)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for !secondary.closed.Load() {
		if time.Now().After(deadline) {
			t.Fatal("Close never started")
		}
		runtime.Gosched()
	}
	select {
	case <-closeDone:
		t.Fatal("Close returned while a remainder callback was still running")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := waitErr(t, readDone, 2*time.Second); err != nil {
		t.Fatalf("ReadZeroCopy = %v", err)
	}
	select {
	case <-closeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return after the callback finished")
	}
}

// TestReadsAfterCloseFailEvenWithQueuedTail: a queued tail is not served once
// the link is closed.
func TestReadsAfterCloseFailEvenWithQueuedTail(t *testing.T) {
	_, primary, secondary := newMemoryPair(t, 4, 64, LinkOptions{})
	mustWrite(t, primary, "0123456789")
	if n, err := secondary.Read(make([]byte, 4)); err != nil || n != 4 {
		t.Fatalf("partial Read = %d, %v", n, err)
	}
	secondary.Close()
	if _, err := secondary.Read(make([]byte, 8)); !errors.Is(err, ErrClosed) {
		t.Fatalf("Read = %v, want ErrClosed", err)
	}
	if _, err := secondary.ReadZeroCopy(func([]byte) error { return nil }); !errors.Is(err, ErrClosed) {
		t.Fatalf("ReadZeroCopy = %v, want ErrClosed", err)
	}
	if _, err := secondary.ReserveRead(); !errors.Is(err, ErrClosed) {
		t.Fatalf("ReserveRead = %v, want ErrClosed", err)
	}
	if _, _, err := secondary.ReadFrom(make([]byte, 8)); !errors.Is(err, ErrClosed) {
		t.Fatalf("ReadFrom = %v, want ErrClosed", err)
	}
}
