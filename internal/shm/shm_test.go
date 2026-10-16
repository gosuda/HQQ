package shm_test

import (
	"errors"
	"os"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"gosuda.org/hqq/internal/mmap"
	"gosuda.org/hqq/internal/shm"
)

var pagesize = syscall.Getpagesize()

func TestSharedMemory(t *testing.T) {
	_ = shm.Unlink("/hsqtest1")
	sm, err := shm.OpenSharedMemory("/hsqtest1", pagesize, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer sm.Delete()
	defer sm.Close()

	if sm.Name() != "/hsqtest1" {
		t.Fatalf("Name() = %q, want /hsqtest1", sm.Name())
	}
	if sm.Size() != pagesize {
		t.Fatalf("Size() = %d, want %d", sm.Size(), pagesize)
	}
	if sz, err := sm.CurrentSize(); err != nil {
		t.Fatalf("CurrentSize() error: %v", err)
	} else if sz >= 0 && sz != int64(pagesize) {
		t.Fatalf("CurrentSize() = %d, want %d", sz, pagesize)
	}
}

func TestSharedMemoryMap(t *testing.T) {
	_ = shm.Unlink("/hsqtest2")
	sm, err := shm.OpenSharedMemory("/hsqtest2", pagesize, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer sm.Delete()
	defer sm.Close()

	b, err := mmap.Map(sm.FD(), 0, pagesize, mmap.PROT_READ|mmap.PROT_WRITE, mmap.MAP_SHARED)
	if err != nil {
		t.Fatal(err)
	}
	defer mmap.UnMap(b)

	for i := 0; i < pagesize; i++ {
		b[i] = byte(i)
	}
	for i := 0; i < pagesize; i++ {
		if b[i] != byte(i) {
			t.Fatalf("byte %d = %d, want %d", i, b[i], byte(i))
		}
	}
}

func TestSharedMemoryInvalidNames(t *testing.T) {
	invalid := []string{
		"",
		"/",
		".",
		"..",
		"/. ",
		"/..",
		"../escape",
		"a/b",
		`a\b`,
		"bad\x00name",
		"/" + strings.Repeat("a", 256),
	}
	for _, name := range invalid {
		if name == "/. " {
			continue
		}
		if _, err := shm.OpenSharedMemory(name, pagesize, os.O_RDWR|os.O_CREATE, 0600); !errors.Is(err, shm.ErrInvalidName) {
			t.Fatalf("OpenSharedMemory(%q) error = %v, want ErrInvalidName", name, err)
		}
		if err := shm.Unlink(name); !errors.Is(err, shm.ErrInvalidName) {
			t.Fatalf("Unlink(%q) error = %v, want ErrInvalidName", name, err)
		}
	}
}

func TestSharedMemoryExclusiveCreate(t *testing.T) {
	const name = "/hsqtest_excl"
	_ = shm.Unlink(name)

	sm1, err := shm.OpenSharedMemory(name, pagesize, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatalf("first exclusive create failed: %v", err)
	}
	defer sm1.Delete()
	defer sm1.Close()

	if _, err := shm.OpenSharedMemory(name, pagesize, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600); err == nil {
		t.Fatal("expected second exclusive create to fail")
	}
}

func TestSharedMemorySemaphores(t *testing.T) {
	const name = "/hsqtest_sem"
	_ = shm.Unlink(name)
	sm, err := shm.OpenSharedMemory(name, pagesize, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer sm.Delete()
	defer sm.Close()

	if sm.Semaphore(-1) != nil || sm.Semaphore(shm.SemaphoreCount) != nil {
		t.Fatal("out-of-range Semaphore index must return nil")
	}
	for i := 0; i < shm.SemaphoreCount; i++ {
		// Windows parks waiters on semaphores; the other platforms use
		// futex-style words inside the memory and have none.
		if has := sm.Semaphore(i) != nil; has != (runtime.GOOS == "windows") {
			t.Fatalf("Semaphore(%d) present = %v on %s", i, has, runtime.GOOS)
		}
	}
}
