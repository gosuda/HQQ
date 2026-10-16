//go:build linux

package shm_test

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"testing"

	"gosuda.org/hqq/internal/shm"
)

func linuxShmName(t *testing.T, tag string) (name, path string) {
	t.Helper()
	if st, err := os.Stat("/dev/shm"); err != nil || !st.IsDir() {
		t.Skip("/dev/shm is not available")
	}
	name = fmt.Sprintf("hqqshm-%s-%d", tag, os.Getpid())
	path = "/dev/shm/" + name
	_ = os.Remove(path)
	t.Cleanup(func() { _ = os.Remove(path) })
	return name, path
}

// TestOpenRefusesNonRegularObjects: /dev/shm is typically world-writable, so
// a name there can be planted as a symlink or special file.
func TestOpenRefusesNonRegularObjects(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		name, path := linuxShmName(t, "sym")
		target := t.TempDir() + "/target"
		if err := os.WriteFile(target, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		for _, flags := range []int{os.O_RDWR, os.O_RDONLY, os.O_RDWR | os.O_CREATE} {
			if _, err := shm.OpenSharedMemory(name, pagesize, flags, 0o600); !errors.Is(err, shm.ErrInvalidName) {
				t.Errorf("open through a symlink with flags %#x = %v, want ErrInvalidName", flags, err)
			}
		}
		if st, err := os.Stat(target); err != nil || st.Size() != 0 {
			t.Fatalf("the symlink target was modified: %v, %v", st, err)
		}
		// Exclusive creation never follows an existing name either.
		if _, err := shm.OpenSharedMemory(name, pagesize, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600); !errors.Is(err, os.ErrExist) {
			t.Errorf("exclusive create over a symlink = %v, want os.ErrExist", err)
		}
		if _, err := os.Lstat(path); err != nil {
			t.Errorf("the planted symlink was removed: %v", err)
		}
	})

	t.Run("fifo", func(t *testing.T) {
		name, path := linuxShmName(t, "fifo")
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := shm.OpenSharedMemory(name, pagesize, os.O_RDWR, 0); !errors.Is(err, shm.ErrInvalidName) {
			t.Fatalf("open of a FIFO = %v, want ErrInvalidName", err)
		}
		if _, err := os.Lstat(path); err != nil {
			t.Errorf("the FIFO was removed: %v", err)
		}
	})

	t.Run("directory", func(t *testing.T) {
		name, path := linuxShmName(t, "dir")
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := shm.OpenSharedMemory(name, pagesize, os.O_RDONLY, 0); !errors.Is(err, shm.ErrInvalidName) {
			t.Fatalf("open of a directory = %v, want ErrInvalidName", err)
		}
	})
}

// TestCreateAllocatesStorage checks that a created object is backed by
// storage right away (fallocate), so that a full tmpfs fails the creation
// instead of faulting later.
func TestCreateAllocatesStorage(t *testing.T) {
	name, path := linuxShmName(t, "alloc")
	const size = 1 << 20
	sm, err := shm.OpenSharedMemory(name, size, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer sm.Close()
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		t.Fatal(err)
	}
	if st.Size != size {
		t.Fatalf("size = %d, want %d", st.Size, size)
	}
	if allocated := st.Blocks * 512; allocated < size {
		t.Fatalf("%d bytes allocated for a %d byte object", allocated, size)
	}
}

func TestNameRefersToSelf(t *testing.T) {
	name, _ := linuxShmName(t, "self")
	sm, err := shm.OpenSharedMemory(name, pagesize, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer sm.Close()
	if known, same, err := sm.NameRefersToSelf(); !known || !same || err != nil {
		t.Fatalf("fresh object: known %v, same %v, err %v", known, same, err)
	}

	// The name is replaced by another object.
	if err := shm.Unlink(name); err != nil {
		t.Fatal(err)
	}
	if known, same, err := sm.NameRefersToSelf(); !known || same || err != nil {
		t.Fatalf("removed name: known %v, same %v, err %v", known, same, err)
	}
	other, err := shm.OpenSharedMemory(name, pagesize, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if known, same, err := sm.NameRefersToSelf(); !known || same || err != nil {
		t.Fatalf("replaced name: known %v, same %v, err %v", known, same, err)
	}
	if known, same, err := other.NameRefersToSelf(); !known || !same || err != nil {
		t.Fatalf("new object: known %v, same %v, err %v", known, same, err)
	}
}
