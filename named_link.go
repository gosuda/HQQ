package hqq

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"unsafe"

	"gosuda.org/hqq/internal/mmap"
	"gosuda.org/hqq/internal/mpmc"
	"gosuda.org/hqq/internal/shm"
)

// namedMapping owns a named shared-memory object and its mapping. mu
// serializes release and unlink, so concurrent Close/Unlink calls on the
// owning link neither double-unmap nor close a reused handle.
type namedMapping struct {
	name         string
	mu           sync.Mutex
	sharedMemory *shm.SharedMemory
	mapping      []byte

	// unlinkOnClose asks release to remove the name, provided the name still
	// refers to this endpoint's instance of the segment (see ownsName).
	unlinkOnClose bool

	// fresh is set when this endpoint created the object with O_EXCL, so a
	// segment that nobody initialized (state Empty) is still its own.
	fresh bool

	// instance identifies the incarnation of the segment this endpoint
	// joined: the creatorToken that the creating endpoint stored in the
	// superblock. Creators know it in advance; other endpoints record it
	// when they attach.
	instance uint64

	// parkers are the cross-process wake objects of the two rings on
	// platforms that need them (Windows: named kernel semaphores owned by
	// sharedMemory, closed together with it by release). Entries are nil
	// elsewhere.
	parkers [2]mpmc.Parker
}

// ringParkers returns the Parkers of ring 0 and ring 1 built from the wake
// semaphores of smem, or nil entries if the platform parks on words inside
// the shared memory instead.
func ringParkers(smem *shm.SharedMemory) [2]mpmc.Parker {
	var parkers [2]mpmc.Parker
	for ring := range parkers {
		notEmpty, notFull := smem.Semaphore(2*ring), smem.Semaphore(2*ring+1)
		if notEmpty != nil && notFull != nil {
			parkers[ring] = mpmc.NewSemaphoreParker(notEmpty, notFull)
		}
	}
	return parkers
}

// release unmaps the memory, closes the handle and, if requested, removes
// the name (only if it still refers to this endpoint's instance). It must
// only run after the link on the mapping is closed. Only the first call does
// anything.
func (m *namedMapping) release() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var err error
	remove := false
	if m.sharedMemory != nil && m.unlinkOnClose {
		// Decide while the mapping and the descriptor are still open.
		remove, err = m.ownsName()
	}
	if m.mapping != nil {
		err = errors.Join(err, mmap.UnMap(m.mapping))
		m.mapping = nil
	}
	if m.sharedMemory != nil {
		err = errors.Join(err, m.sharedMemory.Close())
		if remove {
			err = errors.Join(err, ignoreNotExist(shm.Unlink(m.name)))
		}
		m.sharedMemory = nil
	}
	return err
}

func (m *namedMapping) unlink() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sharedMemory == nil {
		return nil
	}
	m.unlinkOnClose = false
	remove, err := m.ownsName()
	if remove {
		err = errors.Join(err, ignoreNotExist(m.sharedMemory.Delete()))
	}
	return err
}

// ownsName reports whether the name still refers to this endpoint's instance
// of the segment, so that removing it cannot remove a segment that was
// created by somebody else after the name of ours was removed or reused. A
// missing name is not ours: there is nothing to remove. m.mu must be held and
// the descriptor and mapping must still be open.
//
// An instance is the object plus the creatorToken in its superblock:
//
//   - Linux: the object behind the name must have the device and inode of our
//     descriptor (fstat against lstat of the name), and the creatorToken in
//     our mapping must be our instance's.
//   - macOS and FreeBSD offer no inode identity for shm names, so the name is
//     opened read-only again and the creatorToken in the first page of
//     whatever it refers to now is compared with ours.
//   - Windows removes names by itself when the last handle closes, so there is
//     never anything to remove.
func (m *namedMapping) ownsName() (bool, error) {
	if !shm.NamesPersist {
		return false, nil
	}
	known, same, err := m.sharedMemory.NameRefersToSelf()
	if err != nil {
		return false, ignoreNotExist(err)
	}
	var state, token uint64
	switch {
	case known && !same:
		return false, nil
	case known && m.mapping != nil:
		sb := (*standardSuperblock)(unsafe.Pointer(unsafe.SliceData(m.mapping)))
		state, token = atomic.LoadUint64(&sb.state), atomic.LoadUint64(&sb.creatorToken)
	case known:
		// Same object, but never mapped: nobody can have initialized it
		// through us, and we created it if fresh.
		return m.fresh, nil
	default:
		var found bool
		state, token, found, err = peekSuperblock(m.name)
		if err != nil || !found {
			return false, ignoreNotExist(err)
		}
	}
	if token == m.instance && token != 0 {
		return true, nil
	}
	// A segment that was created but never claimed has no token yet.
	return m.fresh && state == superblockStateEmpty, nil
}

// peekSuperblock reads the state and creatorToken from the first page of the
// object that name refers to right now, without modifying it. found is false
// if the name does not exist or the object cannot be one of ours (shorter
// than the superblock page).
func peekSuperblock(name string) (state, token uint64, found bool, err error) {
	smem, err := shm.OpenSharedMemory(name, int(pagesize), os.O_RDONLY, 0)
	if err != nil {
		return 0, 0, false, err
	}
	defer func() { err = errors.Join(err, smem.Close()) }()
	size, err := smem.CurrentSize()
	if err != nil || size < int64(pagesize) {
		return 0, 0, false, err
	}
	page, err := mmap.Map(smem.FD(), 0, int(pagesize), mmap.PROT_READ, mmap.MAP_SHARED)
	if err != nil {
		return 0, 0, false, err
	}
	defer func() { err = errors.Join(err, mmap.UnMap(page)) }()
	// The mapping is read-only, so the words are read plainly (an atomic
	// load may write on some 32-bit platforms). Aligned 64-bit reads are
	// atomic on every 64-bit platform HQQ supports.
	sb := (*standardSuperblock)(unsafe.Pointer(unsafe.SliceData(page)))
	return sb.state, sb.creatorToken, true, nil
}

func ignoreNotExist(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// mapNamed opens (or creates) name and maps size bytes of it.
func mapNamed(name string, size uintptr, flags int, mode os.FileMode) (*namedMapping, error) {
	if size == 0 || uintptr(int(size)) != size {
		return nil, ErrInvalidSize
	}
	smem, err := shm.OpenSharedMemory(name, int(size), flags, mode)
	if err != nil {
		return nil, err
	}
	created := flags&os.O_CREATE != 0
	m := &namedMapping{name: name, sharedMemory: smem, unlinkOnClose: created, fresh: created, parkers: ringParkers(smem)}
	if !created {
		// The creator sizes the object right after creating it; touching a
		// mapping beyond the object's end would raise SIGBUS.
		cur, err := smem.CurrentSize()
		switch {
		case err != nil:
			_ = m.release()
			return nil, err
		case cur == 0:
			_ = m.release()
			return nil, errSegmentNotReady
		case cur > 0 && cur < int64(size):
			_ = m.release()
			return nil, ErrInvalidSize
		}
	}
	mapped, err := mmap.Map(smem.FD(), 0, int(size), mmap.PROT_READ|mmap.PROT_WRITE, mmap.MAP_SHARED)
	if err != nil {
		_ = m.release()
		return nil, err
	}
	m.mapping = mapped
	if len(mapped) == 0 || uintptr(unsafe.Pointer(unsafe.SliceData(mapped)))%pagesize != 0 {
		_ = m.release()
		return nil, ErrMemoryAlign
	}
	return m, nil
}

// createNamed creates the object name exclusively. If the name exists it
// opens the existing object instead, to be re-initialized by the caller if
// its endpoints are gone (see claimSuperblock with roleCreator).
func createNamed(name string, size uintptr, mode os.FileMode) (*namedMapping, error) {
	for attempt := 0; ; attempt++ {
		m, err := mapNamed(name, size, createFlags, mode)
		if !errors.Is(err, os.ErrExist) {
			return m, err
		}
		m, err = mapNamed(name, size, os.O_RDWR, 0)
		if errors.Is(err, os.ErrNotExist) && attempt < 3 {
			continue // removed between the two opens: create it
		}
		return m, err
	}
}

// namedAttach is what the named constructors tell openStandardLink about the
// endpoint they are opening.
type namedAttach struct {
	ctx          context.Context
	role         attachRole
	creatorToken uint64
	parkers      [2]mpmc.Parker
}

// apply returns opts with the named-segment fields set.
func (a namedAttach) apply(opts LinkOptions) LinkOptions {
	opts.ctx = a.ctx
	opts.role = a.role
	opts.creatorToken = a.creatorToken
	opts.parkers = a.parkers
	return opts
}

// openNamed maps name (creating it, or reusing a dead one, if create is set)
// and opens a link on it with open, which receives the mapped memory and the
// attach parameters. ctx bounds the wait for the segment to become usable.
func openNamed[L any](ctx context.Context, name string, bufferCount, bufferSize int, create bool, mode os.FileMode, open func(mem []byte, a namedAttach) (L, error)) (L, *namedMapping, error) {
	return openNamedInstance(ctx, name, bufferCount, bufferSize, create, mode, 0, open)
}

// openNamedInstance is openNamed with an optional instance check for
// openers: if expect is not zero, the segment must be the instance whose
// creatorToken is expect (the one a connecting client announced, see
// Acceptor). The token is checked before the endpoint attaches, so that a
// name that was replaced meanwhile is not joined, and once more after the
// attach; a mismatch fails with errInstanceMismatch.
func openNamedInstance[L any](ctx context.Context, name string, bufferCount, bufferSize int, create bool, mode os.FileMode, expect uint64, open func(mem []byte, a namedAttach) (L, error)) (L, *namedMapping, error) {
	var zero L
	size := SizeStandardLink(bufferCount, bufferSize)
	if size == 0 {
		return zero, nil, ErrInvalidSize
	}
	attach := namedAttach{ctx: orBackground(ctx), role: roleOpener}
	var (
		m   *namedMapping
		err error
	)
	if create {
		attach.role = roleCreator
		attach.creatorToken = newToken()
		m, err = createNamed(name, size, mode)
	} else {
		m, err = mapNamed(name, size, os.O_RDWR, 0)
	}
	if err != nil {
		return zero, nil, err
	}
	attach.parkers = m.parkers
	m.instance = attach.creatorToken
	if expect != 0 && !create {
		sb := (*standardSuperblock)(unsafe.Pointer(unsafe.SliceData(m.mapping)))
		if atomic.LoadUint64(&sb.creatorToken) != expect {
			_ = m.release()
			return zero, nil, errInstanceMismatch
		}
	}
	link, err := open(m.mapping, attach)
	if err != nil {
		_ = m.release()
		return zero, nil, err
	}
	// The segment is now claimed (or joined): it is no longer an object that
	// nobody initialized, so a later Empty object under the same name is not
	// ours (SPEC 11.4).
	m.fresh = false
	if create {
		// We initialized the segment under a new creatorToken: the name is
		// ours to remove.
		m.unlinkOnClose = true
	} else {
		sb := (*standardSuperblock)(unsafe.Pointer(unsafe.SliceData(m.mapping)))
		m.instance = atomic.LoadUint64(&sb.creatorToken)
		if expect != 0 && m.instance != expect {
			// The segment was re-initialized between the check and the
			// attach: detach again.
			if c, ok := any(link).(interface{ Close() error }); ok {
				_ = c.Close()
			}
			_ = m.release()
			return zero, nil, errInstanceMismatch
		}
	}
	return link, m, nil
}

const createFlags = os.O_RDWR | os.O_CREATE | os.O_EXCL

// NamedStandardLink owns a named shared-memory mapping and a StandardLink
// opened on it. Close closes the link, unmaps the memory, and removes the
// name if this endpoint created it and the name still refers to its segment.
type NamedStandardLink struct {
	*StandardLink
	*namedMapping
}

// NamedAdvancedLink owns a named shared-memory mapping and an AdvancedLink
// opened on it.
type NamedAdvancedLink struct {
	*AdvancedLink
	*namedMapping
}

// CreateStandardLink creates a named shared-memory segment sized for a
// StandardLink and opens the first (Primary) endpoint on it. Close (or
// Unlink) removes the name again, but only while it still refers to this
// endpoint's segment: a name that was removed and created again by somebody
// else is left alone.
//
// If the name already exists, CreateStandardLink looks at the existing
// segment instead of failing:
//
//   - If both of its endpoints are closed or dead (for example after a crash
//     of the previous creator), the segment is re-initialized: the caller
//     becomes its Primary and owns the name from now on.
//   - If an endpoint of it is still alive, the call fails with ErrLinkBusy.
//   - If it is not an HQQ segment of this layout version, the call fails with
//     ErrIncompatibleLayout and the object is not modified. An existing
//     object that is smaller than the requested segment fails with
//     ErrInvalidSize, and one that is not sized yet with a transient
//     ErrFailedInit; both leave it alone. A name that is not a regular
//     shared-memory object (on Linux: a symbolic link, device or directory)
//     fails with ErrInvalidName.
//
// A name left behind by a crashed process can also be removed with Unlink.
//
// On Windows, perm is translated to a security descriptor: the owner bits
// apply to the creating user, the "other" bits to Everyone, SYSTEM always has
// full access, and the group bits are ignored (see internal/shm
// OpenSharedMemory). Blocked calls park on named kernel semaphores that
// accompany the segment, so a segment name must leave room for a ".hqq-semN"
// suffix within the kernel's 259-character limit for object names. A Windows
// name exists only while some process holds a handle to it, so an existing
// name always has a live owner at the operating system level.
func CreateStandardLink(name string, bufferCount int, bufferSize int, perm os.FileMode, opts LinkOptions) (*NamedStandardLink, error) {
	return openNamedStandard(context.Background(), name, bufferCount, bufferSize, true, perm, opts)
}

// OpenStandardLink opens an existing named segment created by
// CreateStandardLink and attaches a StandardLink endpoint to it as the
// Secondary. An opener never initializes a segment: if the creator has not
// initialized it yet, OpenStandardLink waits up to opts.AttachTimeout and then
// fails with an error matching ErrFailedInit; if every endpoint of the
// segment closed or died it fails at once with an error matching
// ErrFailedInit. A new CreateStandardLink (or Listen) over the name reuses
// such a segment.
func OpenStandardLink(name string, bufferCount int, bufferSize int, opts LinkOptions) (*NamedStandardLink, error) {
	return openNamedStandard(context.Background(), name, bufferCount, bufferSize, false, 0, opts)
}

func openNamedStandard(ctx context.Context, name string, bufferCount, bufferSize int, create bool, mode os.FileMode, opts LinkOptions) (*NamedStandardLink, error) {
	link, m, err := openNamed(ctx, name, bufferCount, bufferSize, create, mode, func(mem []byte, a namedAttach) (*StandardLink, error) {
		return NewStandardLink(mem, bufferCount, bufferSize, a.apply(opts))
	})
	if err != nil {
		return nil, err
	}
	return &NamedStandardLink{StandardLink: link, namedMapping: m}, nil
}

// CreateAdvancedLink creates a named shared-memory segment and opens the
// first (Primary) AdvancedLink endpoint on it. See CreateStandardLink for the
// name, perm and existing-name semantics.
func CreateAdvancedLink(name string, bufferCount int, bufferSize int, perm os.FileMode, opts AdvancedOptions) (*NamedAdvancedLink, error) {
	return openNamedAdvanced(context.Background(), name, bufferCount, bufferSize, true, perm, opts)
}

// OpenAdvancedLink opens an existing named segment created by
// CreateAdvancedLink and attaches an AdvancedLink endpoint to it. See
// OpenStandardLink.
func OpenAdvancedLink(name string, bufferCount int, bufferSize int, opts AdvancedOptions) (*NamedAdvancedLink, error) {
	return openNamedAdvanced(context.Background(), name, bufferCount, bufferSize, false, 0, opts)
}

func openNamedAdvanced(ctx context.Context, name string, bufferCount, bufferSize int, create bool, mode os.FileMode, opts AdvancedOptions) (*NamedAdvancedLink, error) {
	return openNamedAdvancedInstance(ctx, name, bufferCount, bufferSize, create, mode, 0, opts)
}

// openNamedAdvancedInstance is openNamedAdvanced with the instance check of
// openNamedInstance.
func openNamedAdvancedInstance(ctx context.Context, name string, bufferCount, bufferSize int, create bool, mode os.FileMode, expect uint64, opts AdvancedOptions) (*NamedAdvancedLink, error) {
	link, m, err := openNamedInstance(ctx, name, bufferCount, bufferSize, create, mode, expect, func(mem []byte, a namedAttach) (*AdvancedLink, error) {
		opts.Link = a.apply(opts.Link)
		return NewAdvancedLink(mem, bufferCount, bufferSize, opts)
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

// Unlink removes the shared-memory name now (instead of at Close), if it
// still refers to this link's segment; a name that is gone or was replaced by
// another segment is left alone and Unlink returns nil. Existing mappings
// remain valid until closed. On Windows it is a no-op.
func (l *NamedStandardLink) Unlink() error {
	if l == nil {
		return nil
	}
	return l.unlink()
}

// Unlink removes the shared-memory name now (instead of at Close), if it
// still refers to this link's segment; a name that is gone or was replaced by
// another segment is left alone and Unlink returns nil. Existing mappings
// remain valid until closed. On Windows it is a no-op.
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
