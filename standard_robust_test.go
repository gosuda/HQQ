package hqq

import (
	"errors"
	"io"
	"runtime"
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
// Windows): a parked reader or writer must be woken by its peer long before
// the periodic park timeout.
func TestNamedLinkBlockedCallsWakePromptly(t *testing.T) {
	name := newPortableSharedMemoryName()
	primary, err := CreateStandardLink(name, 2, 64, 0o600)
	if err != nil {
		t.Fatalf("CreateStandardLink: %v", err)
	}
	defer primary.Close()
	secondary, err := OpenNamedStandardLink(name, 2, 64)
	if err != nil {
		t.Fatalf("OpenNamedStandardLink: %v", err)
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
	start := time.Now()
	if _, err := primary.Write([]byte("wake")); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-readCh:
		if r.err != nil || r.msg != "wake" {
			t.Fatalf("Read = %q, %v", r.msg, r.err)
		}
		if d := time.Since(start); d > 50*time.Millisecond {
			t.Fatalf("parked reader woke after %v, want a prompt wake-up", d)
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
	start = time.Now()
	if _, err := secondary.Read(make([]byte, 64)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-writeCh:
		if err != nil {
			t.Fatalf("blocked Write = %v", err)
		}
		if d := time.Since(start); d > 50*time.Millisecond {
			t.Fatalf("parked writer woke after %v, want a prompt wake-up", d)
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
	link, err := CreateStandardLink(name, 2, 64, 0o600)
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
