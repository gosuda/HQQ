package hqq

import (
	"errors"
	"os"
	"unsafe"

	"gosuda.org/hqq/internal/mmap"
	"gosuda.org/hqq/internal/shm"
)

// namedMapping owns a named shared-memory object and its mapping.
type namedMapping struct {
	name          string
	sharedMemory  *shm.SharedMemory
	mapping       []byte
	unlinkOnClose bool
}

// release unmaps the memory, closes the handle and, if requested, removes
// the name. It must only run after the link on the mapping is closed.
func (m *namedMapping) release() error {
	var err error
	if m.mapping != nil {
		err = errors.Join(err, mmap.UnMap(m.mapping))
		m.mapping = nil
	}
	if m.sharedMemory != nil {
		err = errors.Join(err, m.sharedMemory.Close())
		if m.unlinkOnClose {
			err = errors.Join(err, ignoreNotExist(m.sharedMemory.Delete()))
		}
		m.sharedMemory = nil
	}
	return err
}

func (m *namedMapping) unlink() error {
	if m.sharedMemory == nil {
		return nil
	}
	m.unlinkOnClose = false
	return m.sharedMemory.Delete()
}

func ignoreNotExist(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// mapNamed opens (or creates) name and maps size bytes of it.
func mapNamed(name string, size uintptr, flags int, mode os.FileMode) (namedMapping, error) {
	if size == 0 || uintptr(int(size)) != size {
		return namedMapping{}, ErrInvalidSize
	}
	smem, err := shm.OpenSharedMemory(name, int(size), flags, mode)
	if err != nil {
		return namedMapping{}, err
	}
	created := flags&os.O_CREATE != 0
	m := namedMapping{name: name, sharedMemory: smem, unlinkOnClose: created}
	if !created {
		// The creator sizes the object right after creating it; touching a
		// mapping beyond the object's end would raise SIGBUS.
		cur, err := smem.CurrentSize()
		switch {
		case err != nil:
			_ = m.release()
			return namedMapping{}, err
		case cur == 0:
			_ = m.release()
			return namedMapping{}, errSegmentNotReady
		case cur > 0 && cur < int64(size):
			_ = m.release()
			return namedMapping{}, ErrInvalidSize
		}
	}
	mapped, err := mmap.Map(smem.FD(), 0, int(size), mmap.PROT_READ|mmap.PROT_WRITE, mmap.MAP_SHARED)
	if err != nil {
		_ = m.release()
		return namedMapping{}, err
	}
	m.mapping = mapped
	if len(mapped) == 0 || uintptr(unsafe.Pointer(unsafe.SliceData(mapped)))%pagesize != 0 {
		_ = m.release()
		return namedMapping{}, ErrMemoryAlign
	}
	return m, nil
}

// openNamed maps name and opens a link on it with open.
func openNamed[L any](name string, bufferCount, bufferSize int, flags int, mode os.FileMode, open func(mem []byte) (L, error)) (L, namedMapping, error) {
	var zero L
	size := SizeStandardLink(bufferCount, bufferSize)
	if size == 0 {
		return zero, namedMapping{}, ErrInvalidSize
	}
	m, err := mapNamed(name, size, flags, mode)
	if err != nil {
		return zero, namedMapping{}, err
	}
	link, err := open(m.mapping)
	if err != nil {
		_ = m.release()
		return zero, namedMapping{}, err
	}
	return link, m, nil
}

const createFlags = os.O_RDWR | os.O_CREATE | os.O_EXCL

// NamedStandardLink owns a named shared-memory mapping and a StandardLink
// opened on it. Close closes the link, unmaps the memory, and removes the
// name if this endpoint created it.
type NamedStandardLink struct {
	*StandardLink
	namedMapping
}

// NamedAdvancedLink owns a named shared-memory mapping and an AdvancedLink
// opened on it.
type NamedAdvancedLink struct {
	*AdvancedLink
	namedMapping
}

// CreateStandardLink creates a named shared-memory segment sized for a
// StandardLink and opens the first (Primary) endpoint on it. It fails with
// an error matching os.ErrExist if the name exists; a name left behind by a
// crashed process can be removed with Unlink, or reused with
// OpenNamedStandardLink (which re-initializes stale segments).
func CreateStandardLink(name string, bufferCount int, bufferSize int, mode os.FileMode) (*NamedStandardLink, error) {
	return CreateStandardLinkWithOptions(name, bufferCount, bufferSize, mode, LinkOptions{})
}

// CreateStandardLinkWithOptions is CreateStandardLink with explicit options.
func CreateStandardLinkWithOptions(name string, bufferCount int, bufferSize int, mode os.FileMode, opts LinkOptions) (*NamedStandardLink, error) {
	return openNamedStandard(name, bufferCount, bufferSize, createFlags, mode, opts)
}

// OpenNamedStandardLink opens an existing named segment and attaches a
// StandardLink endpoint to it.
func OpenNamedStandardLink(name string, bufferCount int, bufferSize int) (*NamedStandardLink, error) {
	return OpenNamedStandardLinkWithOptions(name, bufferCount, bufferSize, LinkOptions{})
}

// OpenNamedStandardLinkWithOptions is OpenNamedStandardLink with explicit
// options.
func OpenNamedStandardLinkWithOptions(name string, bufferCount int, bufferSize int, opts LinkOptions) (*NamedStandardLink, error) {
	return openNamedStandard(name, bufferCount, bufferSize, os.O_RDWR, 0, opts)
}

func openNamedStandard(name string, bufferCount, bufferSize, flags int, mode os.FileMode, opts LinkOptions) (*NamedStandardLink, error) {
	link, m, err := openNamed(name, bufferCount, bufferSize, flags, mode, func(mem []byte) (*StandardLink, error) {
		return OpenStandardLinkMemory(mem, bufferCount, bufferSize, opts)
	})
	if err != nil {
		return nil, err
	}
	return &NamedStandardLink{StandardLink: link, namedMapping: m}, nil
}

// CreateAdvancedLink creates a named shared-memory segment and opens the
// first AdvancedLink endpoint with default AdvancedOptions.
func CreateAdvancedLink(name string, bufferCount int, bufferSize int, mode os.FileMode) (*NamedAdvancedLink, error) {
	return CreateAdvancedLinkWithOptions(name, bufferCount, bufferSize, mode, AdvancedOptions{})
}

// CreateAdvancedLinkWithOptions is CreateAdvancedLink with explicit options.
func CreateAdvancedLinkWithOptions(name string, bufferCount int, bufferSize int, mode os.FileMode, opts AdvancedOptions) (*NamedAdvancedLink, error) {
	return openNamedAdvanced(name, bufferCount, bufferSize, createFlags, mode, opts)
}

// OpenNamedAdvancedLink opens an existing named segment and attaches an
// AdvancedLink endpoint with default AdvancedOptions.
func OpenNamedAdvancedLink(name string, bufferCount int, bufferSize int) (*NamedAdvancedLink, error) {
	return OpenNamedAdvancedLinkWithOptions(name, bufferCount, bufferSize, AdvancedOptions{})
}

// OpenNamedAdvancedLinkWithOptions is OpenNamedAdvancedLink with explicit
// options.
func OpenNamedAdvancedLinkWithOptions(name string, bufferCount int, bufferSize int, opts AdvancedOptions) (*NamedAdvancedLink, error) {
	return openNamedAdvanced(name, bufferCount, bufferSize, os.O_RDWR, 0, opts)
}

func openNamedAdvanced(name string, bufferCount, bufferSize, flags int, mode os.FileMode, opts AdvancedOptions) (*NamedAdvancedLink, error) {
	link, m, err := openNamed(name, bufferCount, bufferSize, flags, mode, func(mem []byte) (*AdvancedLink, error) {
		return NewAdvancedLinkMemory(mem, bufferCount, bufferSize, opts)
	})
	if err != nil {
		return nil, err
	}
	return &NamedAdvancedLink{AdvancedLink: link, namedMapping: m}, nil
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

// Close closes the link (waiting for in-flight calls), unmaps the shared
// memory, closes the shared-memory handle, and removes the name if this
// endpoint created it. Close is idempotent.
func (l *NamedStandardLink) Close() error {
	if l == nil {
		return nil
	}
	var err error
	if l.StandardLink != nil {
		err = l.StandardLink.Close()
	}
	return errors.Join(err, l.release())
}

// Close closes the link (waiting for in-flight calls), unmaps the shared
// memory, closes the shared-memory handle, and removes the name if this
// endpoint created it. Close is idempotent.
func (l *NamedAdvancedLink) Close() error {
	if l == nil {
		return nil
	}
	var err error
	if l.AdvancedLink != nil {
		err = l.AdvancedLink.Close()
	}
	return errors.Join(err, l.release())
}

// Unlink removes the shared-memory name now (instead of at Close). Existing
// mappings remain valid until closed.
func (l *NamedStandardLink) Unlink() error {
	if l == nil {
		return nil
	}
	return l.unlink()
}

// Unlink removes the shared-memory name now (instead of at Close). Existing
// mappings remain valid until closed.
func (l *NamedAdvancedLink) Unlink() error {
	if l == nil {
		return nil
	}
	return l.unlink()
}

// Unlink removes the named shared-memory object name, for example one left
// behind by a crashed process. Processes that still map it are not affected.
// On Windows it is a no-op: mappings disappear with their last handle.
func Unlink(name string) error {
	return shm.Unlink(name)
}
