package hqq

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"gosuda.org/hqq/internal/mmap"
	"gosuda.org/hqq/internal/shm"
)

// openNamedPeer opens name as the second endpoint and fails the test if it is
// not a Secondary of a live link.
func openNamedPeer(t *testing.T, name string, opts LinkOptions) *NamedStandardLink {
	t.Helper()
	peer, err := OpenStandardLink(name, 4, 64, opts)
	if err != nil {
		t.Fatalf("OpenStandardLink(%q) = %v", name, err)
	}
	if peer.Mode() != LinkModeSecondary {
		peer.Close()
		t.Fatalf("mode = %v, want secondary", peer.Mode())
	}
	return peer
}

// nameExists reports whether the name can be opened as a segment of any
// state, without attaching an endpoint.
func nameExists(t *testing.T, name string) bool {
	t.Helper()
	smem, err := shm.OpenSharedMemory(name, int(pagesize), os.O_RDONLY, 0)
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		t.Fatalf("probing %q: %v", name, err)
	}
	_ = smem.Close()
	return true
}

func TestCloseDoesNotRemoveRecreatedName(t *testing.T) {
	if isWindows() {
		t.Skip("Windows names cannot be unlinked while handles are open")
	}
	for _, via := range []string{"Close", "Unlink"} {
		t.Run(via, func(t *testing.T) {
			name := newPortableSharedMemoryName()
			a, err := CreateStandardLink(name, 4, 64, 0o600, LinkOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			// The name is removed under A and created again by B.
			if err := Unlink(name); err != nil {
				t.Fatal(err)
			}
			b, err := CreateStandardLink(name, 4, 64, 0o600, LinkOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close()

			switch via {
			case "Close":
				if err := a.Close(); err != nil {
					t.Fatal(err)
				}
			case "Unlink":
				if err := a.Unlink(); err != nil {
					t.Fatalf("Unlink = %v, want nil for a name that is not ours", err)
				}
			}
			peer := openNamedPeer(t, name, LinkOptions{})
			peer.Close()

			// B still owns the name.
			if err := b.Close(); err != nil {
				t.Fatal(err)
			}
			if nameExists(t, name) {
				t.Fatal("name still exists after its owner closed")
			}
		})
	}
}

// After a successful create the segment is claimed, so a zero-filled object
// (state Empty, no creatorToken) that replaced the name is not ours and Close
// must leave it alone (SPEC 11.4).
func TestNamedCloseKeepsEmptyReplacementAfterClaim(t *testing.T) {
	if isWindows() {
		t.Skip("Windows names cannot be unlinked while handles are open")
	}
	name := newPortableSharedMemoryName()
	t.Cleanup(func() { _ = Unlink(name) })
	a, err := CreateStandardLink(name, 4, 64, 0o600, LinkOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := Unlink(name); err != nil {
		t.Fatal(err)
	}
	repl, err := shm.OpenSharedMemory(name, 2*int(pagesize), createFlags, 0o600)
	if err != nil {
		t.Fatalf("replacement create: %v", err)
	}
	defer repl.Close()
	if err := a.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if !nameExists(t, name) {
		t.Fatal("Close removed a zero-filled replacement that is not ours")
	}
}

func TestCloseDoesNotRemoveReusedName(t *testing.T) {
	name := newPortableSharedMemoryName()
	a, err := CreateStandardLink(name, 4, 64, 0o600, fastLiveness)
	if err != nil {
		t.Fatal(err)
	}
	// A crashes: its endpoint stops heartbeating without detaching.
	simulateCrash(a.StandardLink)
	time.Sleep(250 * time.Millisecond)

	// B reuses the very same object, so only the creator token tells the
	// two instances apart.
	b, err := CreateStandardLink(name, 4, 64, 0o600, fastLiveness)
	if err != nil {
		t.Fatalf("CreateStandardLink over a crashed segment = %v", err)
	}
	defer b.Close()
	if b.Mode() != LinkModePrimary {
		t.Fatalf("mode after reuse = %v, want primary", b.Mode())
	}
	if a.instance == b.instance {
		t.Fatal("reusing creator did not get a new instance token")
	}

	// A, still holding its mapping of the same object, goes away.
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	peer := openNamedPeer(t, name, fastLiveness)
	defer peer.Close()
	if _, err := b.Write([]byte("reused")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	if n, err := peer.Read(buf); err != nil || string(buf[:n]) != "reused" {
		t.Fatalf("Read = %q, %v", buf[:n], err)
	}

	peer.Close()
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if nameExists(t, name) {
		t.Fatal("name still exists after the reusing creator closed")
	}
}

func TestCreateOverLiveSegmentIsBusy(t *testing.T) {
	name := newPortableSharedMemoryName()
	a, err := CreateStandardLink(name, 4, 64, 0o600, LinkOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	_, err = CreateStandardLink(name, 4, 64, 0o600, LinkOptions{})
	if !errors.Is(err, ErrLinkBusy) || !errors.Is(err, ErrFailedInit) {
		t.Fatalf("second create next to a live primary = %v, want ErrLinkBusy", err)
	}
	// The failed attempt neither removed nor changed the segment.
	peer := openNamedPeer(t, name, LinkOptions{})
	defer peer.Close()
	if _, err := a.Write([]byte("still mine")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	if n, err := peer.Read(buf); err != nil || string(buf[:n]) != "still mine" {
		t.Fatalf("Read = %q, %v", buf[:n], err)
	}
	if _, err := CreateStandardLink(name, 4, 64, 0o600, LinkOptions{}); !errors.Is(err, ErrLinkBusy) {
		t.Fatalf("create next to two live endpoints = %v, want ErrLinkBusy", err)
	}
}

// createForeignObject creates the name as an object that is not a segment and
// returns a function that reads back its first page. Where names do not
// outlive their handles (Windows) the object's handle stays open until the
// test ends, like the process that owns the object would keep it.
func createForeignObject(t *testing.T, name string, size int, fill func(page []byte)) func() []byte {
	t.Helper()
	smem, mapped := mapNamedSharedMemory(t, name, size, os.O_RDWR|os.O_CREATE|os.O_EXCL)
	fill(mapped[:min(size, int(pagesize))])
	if err := mmap.UnMap(mapped); err != nil {
		t.Fatal(err)
	}
	if shm.NamesPersist {
		if err := smem.Close(); err != nil {
			t.Fatal(err)
		}
	} else {
		t.Cleanup(func() { _ = smem.Close() })
	}
	t.Cleanup(func() { _ = Unlink(name) })
	return func() []byte {
		smem, mapped := mapNamedSharedMemory(t, name, size, os.O_RDWR)
		defer smem.Close()
		defer mmap.UnMap(mapped)
		return append([]byte(nil), mapped[:min(size, int(pagesize))]...)
	}
}

// closeUnexpected closes a link that a failing assertion did not expect to
// be opened, so that it cannot keep its name alive for later tests.
func closeUnexpected(l *NamedStandardLink) {
	if l != nil {
		_ = l.Close()
	}
}

func TestCreateNeverTouchesForeignObject(t *testing.T) {
	size := int(SizeStandardLink(4, 64))

	t.Run("data in the first page", func(t *testing.T) {
		name := newPortableSharedMemoryName()
		snapshot := createForeignObject(t, name, size, func(page []byte) {
			copy(page[100:], "somebody else's data")
		})
		before := snapshot()
		if l, err := CreateStandardLink(name, 4, 64, 0o600, LinkOptions{}); !errors.Is(err, ErrIncompatibleLayout) {
			closeUnexpected(l)
			t.Fatalf("Create over foreign data = %v, want ErrIncompatibleLayout", err)
		}
		start := time.Now()
		if l, err := OpenStandardLink(name, 4, 64, LinkOptions{}); !errors.Is(err, ErrIncompatibleLayout) {
			closeUnexpected(l)
			t.Fatalf("Open over foreign data = %v, want ErrIncompatibleLayout", err)
		}
		if time.Since(start) > time.Second {
			t.Fatalf("Open waited %v on foreign data", time.Since(start))
		}
		if after := snapshot(); string(after) != string(before) {
			t.Fatal("foreign object was modified")
		}
		if !nameExists(t, name) {
			t.Fatal("foreign name was removed")
		}
	})

	t.Run("smaller than the segment", func(t *testing.T) {
		name := newPortableSharedMemoryName()
		createForeignObject(t, name, int(pagesize), func([]byte) {})
		if l, err := CreateStandardLink(name, 4, 64, 0o600, LinkOptions{}); !errors.Is(err, ErrInvalidSize) {
			closeUnexpected(l)
			t.Fatalf("Create over a small object = %v, want ErrInvalidSize", err)
		}
		if !nameExists(t, name) {
			t.Fatal("small foreign object was removed")
		}
	})

	t.Run("segment of layout v2", func(t *testing.T) {
		name := newPortableSharedMemoryName()
		a, err := CreateStandardLink(name, 4, 64, 0o600, LinkOptions{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = Unlink(name) })
		atomic.StoreUint64(&(*standardSuperblock)(unsafe.Pointer(&a.mapping[0])).magic, layoutV2Magic)
		if !shm.NamesPersist {
			// Keep the object alive after its endpoint closes, as a
			// process still holding it would.
			holder, holderMap := mapNamedSharedMemory(t, name, size, os.O_RDWR)
			_ = mmap.UnMap(holderMap)
			t.Cleanup(func() { _ = holder.Close() })
		}
		a.unlinkOnClose = false
		a.Close() // both endpoints gone: a v3 segment would be reused

		if l, err := CreateStandardLink(name, 4, 64, 0o600, LinkOptions{}); !errors.Is(err, ErrIncompatibleLayout) {
			closeUnexpected(l)
			t.Fatalf("Create over a v2 segment = %v, want ErrIncompatibleLayout", err)
		}
		smem, mapped := mapNamedSharedMemory(t, name, size, os.O_RDWR)
		defer smem.Close()
		defer mmap.UnMap(mapped)
		if got := atomic.LoadUint64(&(*standardSuperblock)(unsafe.Pointer(&mapped[0])).magic); got != layoutV2Magic {
			t.Fatalf("magic = %#x, the v2 segment was modified", got)
		}
	})
}

// TestFailedCreateRemovesItsName: a creator that cannot finish must not leave
// an unusable name behind.
func TestFailedCreateRemovesItsName(t *testing.T) {
	name := newPortableSharedMemoryName()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if l, err := openNamedStandard(ctx, name, 4, 64, true, 0o600, LinkOptions{}); !errors.Is(err, context.Canceled) {
		closeUnexpected(l)
		t.Fatalf("create with a canceled context = %v", err)
	}
	if nameExists(t, name) {
		_ = Unlink(name)
		t.Fatal("failed create left its name behind")
	}
}

func TestDialStopsWithContextOnStuckInitializer(t *testing.T) {
	name := newPortableSharedMemoryName()
	cfg := Config{BufferCount: 16, BufferSize: 64}
	size := int(SizeStandardLink(cfg.BufferCount, cfg.BufferSize))
	smem, mapped := mapNamedSharedMemory(t, name, size, os.O_RDWR|os.O_CREATE|os.O_EXCL)
	defer func() {
		_ = mmap.UnMap(mapped)
		_ = smem.Close()
		_ = smem.Delete()
	}()
	// An initializer that is alive as far as anybody can tell, and a Dial
	// whose AttachTimeout (5 seconds) far exceeds its context.
	sb := (*standardSuperblock)(unsafe.Pointer(&mapped[0]))
	store(&sb.state, superblockStateInitializing)
	store(&sb.major, superblockMajor)
	store(&sb.initToken, 0xabc)
	store(&sb.initHeartbeat, nowNanos())

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	l, err := Dial(ctx, name, cfg)
	if !errors.Is(err, context.DeadlineExceeded) {
		if l != nil {
			_ = l.Close()
		}
		t.Fatalf("Dial = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Dial returned after %v, not at its 100ms context", elapsed)
	}
}

func TestDialOnForeignObjectFailsFast(t *testing.T) {
	name := newPortableSharedMemoryName()
	cfg := Config{BufferCount: 16, BufferSize: 64}
	size := int(SizeStandardLink(cfg.BufferCount, cfg.BufferSize))
	createForeignObject(t, name, size, func(page []byte) { page[200] = 1 })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if l, err := Dial(ctx, name, cfg); !errors.Is(err, ErrIncompatibleLayout) {
		if l != nil {
			_ = l.Close()
		}
		t.Fatalf("Dial over foreign data = %v, want ErrIncompatibleLayout", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("Dial retried a permanent error for %v", time.Since(start))
	}
}
