package hqq

import (
	"errors"
	"os"
	"unsafe"

	"gosuda.org/hqq/internal/mmap"
	"gosuda.org/hqq/internal/shm"
)

// NamedStandardLink owns a named shared-memory mapping and a StandardLink
// opened on that page-aligned mapping. Close unmaps and closes local resources;
// call Unlink, or use CreateStandardLink's unlink-on-close behavior, to remove
// the shared-memory name.
type NamedStandardLink struct {
	*StandardLink

	name          string
	sharedMemory  *shm.SharedMemory
	mapping       []byte
	unlinkOnClose bool
}

// NamedAdvancedLink owns a named shared-memory mapping and an AdvancedLink
// opened on that page-aligned mapping.
type NamedAdvancedLink struct {
	*AdvancedLink

	name          string
	sharedMemory  *shm.SharedMemory
	mapping       []byte
	unlinkOnClose bool
}

// CreateStandardLink creates a named, page-aligned shared-memory region sized
// for a StandardLink and opens the first endpoint on it.
func CreateStandardLink(name string, bufferCount int, bufferSize int, mode os.FileMode) (*NamedStandardLink, error) {
	return openMappedNamedStandardLink(name, bufferCount, bufferSize, os.O_RDWR|os.O_CREATE|os.O_EXCL, mode, true)
}

// OpenNamedStandardLink opens an existing named shared-memory region and
// attaches a StandardLink endpoint to it.
func OpenNamedStandardLink(name string, bufferCount int, bufferSize int) (*NamedStandardLink, error) {
	return openMappedNamedStandardLink(name, bufferCount, bufferSize, os.O_RDWR, 0, false)
}

func openMappedNamedStandardLink(name string, bufferCount int, bufferSize int, flags int, mode os.FileMode, unlinkOnClose bool) (*NamedStandardLink, error) {
	size := SizeStandardLink(bufferCount, bufferSize)
	smem, mapped, offset, err := mapNamedLink(name, size, flags, mode)
	if err != nil {
		return nil, err
	}
	link, err := OpenStandardLink(offset, bufferCount, bufferSize)
	if err != nil {
		cleanupMappedLink(smem, mapped, flags&os.O_CREATE != 0)
		return nil, err
	}
	return &NamedStandardLink{
		StandardLink:  link,
		name:          name,
		sharedMemory:  smem,
		mapping:       mapped,
		unlinkOnClose: unlinkOnClose,
	}, nil
}

// CreateAdvancedLink creates a named shared-memory region and opens the first
// AdvancedLink endpoint with default AdvancedOptions.
func CreateAdvancedLink(name string, bufferCount int, bufferSize int, mode os.FileMode) (*NamedAdvancedLink, error) {
	return CreateAdvancedLinkWithOptions(name, bufferCount, bufferSize, mode, AdvancedOptions{})
}

// CreateAdvancedLinkWithOptions is CreateAdvancedLink with explicit advanced
// protocol safety and dispatcher queue options.
func CreateAdvancedLinkWithOptions(name string, bufferCount int, bufferSize int, mode os.FileMode, opts AdvancedOptions) (*NamedAdvancedLink, error) {
	return openMappedNamedAdvancedLink(name, bufferCount, bufferSize, os.O_RDWR|os.O_CREATE|os.O_EXCL, mode, true, opts)
}

// OpenNamedAdvancedLink opens an existing named shared-memory region and
// attaches an AdvancedLink endpoint with default AdvancedOptions.
func OpenNamedAdvancedLink(name string, bufferCount int, bufferSize int) (*NamedAdvancedLink, error) {
	return OpenNamedAdvancedLinkWithOptions(name, bufferCount, bufferSize, AdvancedOptions{})
}

// OpenNamedAdvancedLinkWithOptions is OpenNamedAdvancedLink with explicit
// advanced protocol safety and dispatcher queue options.
func OpenNamedAdvancedLinkWithOptions(name string, bufferCount int, bufferSize int, opts AdvancedOptions) (*NamedAdvancedLink, error) {
	return openMappedNamedAdvancedLink(name, bufferCount, bufferSize, os.O_RDWR, 0, false, opts)
}

func openMappedNamedAdvancedLink(name string, bufferCount int, bufferSize int, flags int, mode os.FileMode, unlinkOnClose bool, opts AdvancedOptions) (*NamedAdvancedLink, error) {
	size := SizeStandardLink(bufferCount, bufferSize)
	smem, mapped, offset, err := mapNamedLink(name, size, flags, mode)
	if err != nil {
		return nil, err
	}
	link, err := NewAdvancedLinkWithOptions(offset, bufferCount, bufferSize, opts)
	if err != nil {
		cleanupMappedLink(smem, mapped, flags&os.O_CREATE != 0)
		return nil, err
	}
	return &NamedAdvancedLink{
		AdvancedLink:  link,
		name:          name,
		sharedMemory:  smem,
		mapping:       mapped,
		unlinkOnClose: unlinkOnClose,
	}, nil
}

// Name returns the shared-memory object name.
func (l *NamedStandardLink) Name() string {
	if l == nil {
		return ""
	}
	return l.name
}

// Name returns the shared-memory object name.
func (l *NamedAdvancedLink) Name() string {
	if l == nil {
		return ""
	}
	return l.name
}

// Close closes the link, unmaps the shared memory, closes the shared-memory
// handle, and unlinks the name for links created by CreateStandardLink.
func (l *NamedStandardLink) Close() error {
	if l == nil {
		return nil
	}
	var err error
	if l.StandardLink != nil {
		err = errors.Join(err, l.StandardLink.Close())
		l.StandardLink = nil
	}
	err = errors.Join(err, closeMappedResources(l.sharedMemory, l.mapping, l.unlinkOnClose))
	l.sharedMemory = nil
	l.mapping = nil
	return err
}

// Close closes the link, unmaps the shared memory, closes the shared-memory
// handle, and unlinks the name for links created by CreateAdvancedLink.
func (l *NamedAdvancedLink) Close() error {
	if l == nil {
		return nil
	}
	var err error
	if l.AdvancedLink != nil {
		err = errors.Join(err, l.AdvancedLink.Close())
		l.AdvancedLink = nil
	}
	err = errors.Join(err, closeMappedResources(l.sharedMemory, l.mapping, l.unlinkOnClose))
	l.sharedMemory = nil
	l.mapping = nil
	return err
}

// Unlink removes the shared-memory name. Existing mappings remain valid until
// closed by the operating system.
func (l *NamedStandardLink) Unlink() error {
	if l == nil || l.sharedMemory == nil {
		return nil
	}
	l.unlinkOnClose = false
	return l.sharedMemory.Delete()
}

// Unlink removes the shared-memory name. Existing mappings remain valid until
// closed by the operating system.
func (l *NamedAdvancedLink) Unlink() error {
	if l == nil || l.sharedMemory == nil {
		return nil
	}
	l.unlinkOnClose = false
	return l.sharedMemory.Delete()
}

func mapNamedLink(name string, size uintptr, flags int, mode os.FileMode) (*shm.SharedMemory, []byte, uintptr, error) {
	if size == 0 || uintptr(int(size)) != size {
		return nil, nil, 0, ErrInvalidSize
	}
	smem, err := shm.OpenSharedMemory(name, int(size), flags, mode)
	if err != nil {
		return nil, nil, 0, err
	}
	mapped, err := mmap.Map(smem.FD(), 0, int(size), mmap.PROT_READ|mmap.PROT_WRITE, mmap.MAP_SHARED)
	if err != nil {
		_ = smem.Close()
		if flags&os.O_CREATE != 0 {
			_ = smem.Delete()
		}
		return nil, nil, 0, err
	}
	if len(mapped) == 0 {
		cleanupMappedLink(smem, mapped, flags&os.O_CREATE != 0)
		return nil, nil, 0, ErrInvalidSize
	}
	offset := uintptr(unsafe.Pointer(unsafe.SliceData(mapped)))
	if offset%pagesize != 0 {
		cleanupMappedLink(smem, mapped, flags&os.O_CREATE != 0)
		return nil, nil, 0, ErrMemoryAlign
	}
	return smem, mapped, offset, nil
}

func cleanupMappedLink(smem *shm.SharedMemory, mapped []byte, unlink bool) {
	_ = closeMappedResources(smem, mapped, unlink)
}

func closeMappedResources(smem *shm.SharedMemory, mapped []byte, unlink bool) error {
	var err error
	if mapped != nil {
		err = errors.Join(err, mmap.UnMap(mapped))
	}
	if smem != nil {
		err = errors.Join(err, smem.Close())
		if unlink {
			err = errors.Join(err, smem.Delete())
		}
	}
	return err
}
