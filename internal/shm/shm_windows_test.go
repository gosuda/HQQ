//go:build windows

package shm_test

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"gosuda.org/hqq/internal/shm"
)

func TestWindowsSemaphoresAreSharedAcrossHandles(t *testing.T) {
	const name = "hsqtest_winsem"
	creator, err := shm.OpenSharedMemory(name, pagesize, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer creator.Close()
	opener, err := shm.OpenSharedMemory(name, pagesize, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer opener.Close()

	for i := 0; i < shm.SemaphoreCount; i++ {
		c, o := creator.Semaphore(i), opener.Semaphore(i)
		if c == nil || o == nil {
			t.Fatalf("Semaphore(%d) missing", i)
		}
		// No token yet: Wait must run into its timeout.
		start := time.Now()
		o.Wait(20 * time.Millisecond)
		if d := time.Since(start); d < 15*time.Millisecond {
			t.Fatalf("Wait without token returned after %v", d)
		}
		// A token released through one handle is consumed through the other.
		c.Release(1)
		start = time.Now()
		o.Wait(5 * time.Second)
		if d := time.Since(start); d > time.Second {
			t.Fatalf("Wait with a token took %v", d)
		}
	}
}

func TestWindowsCurrentSize(t *testing.T) {
	const name = "hsqtest_wincursize"
	creator, err := shm.OpenSharedMemory(name, 3*pagesize, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer creator.Close()
	opener, err := shm.OpenSharedMemory(name, 3*pagesize, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer opener.Close()
	for who, sm := range map[string]*shm.SharedMemory{"creator": creator, "opener": opener} {
		got, err := sm.CurrentSize()
		if err != nil || got != int64(3*pagesize) {
			t.Fatalf("%s CurrentSize = %d, %v; want %d", who, got, err, 3*pagesize)
		}
	}
}

func TestWindowsExclusiveCreateCleansUp(t *testing.T) {
	const name = "hsqtest_winexcl"
	first, err := shm.OpenSharedMemory(name, pagesize, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := shm.OpenSharedMemory(name, pagesize, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600); !errors.Is(err, os.ErrExist) {
		t.Fatalf("second exclusive create = %v, want os.ErrExist", err)
	}
	first.Close()
	// Everything went away with the last handle: the name can be reused.
	again, err := shm.OpenSharedMemory(name, pagesize, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatalf("create after close: %v", err)
	}
	again.Close()
}

func TestWindowsOverlongNameIsInvalid(t *testing.T) {
	name := strings.Repeat("a", 250)
	if _, err := shm.OpenSharedMemory(name, pagesize, os.O_RDWR|os.O_CREATE, 0600); !errors.Is(err, shm.ErrInvalidName) {
		t.Fatalf("OpenSharedMemory(250 chars) = %v, want ErrInvalidName", err)
	}
}

func TestWindowsOpenMissingFails(t *testing.T) {
	if _, err := shm.OpenSharedMemory("hsqtest_winmissing", pagesize, os.O_RDWR, 0); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("open of a missing mapping = %v, want os.ErrNotExist", err)
	}
}
