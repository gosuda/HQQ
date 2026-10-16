//go:build windows

package shm

import (
	"errors"
	"fmt"
	"math"
	"os"
	"runtime"
	"syscall"
	"time"
	"unsafe"
)

var (
	kernel32 = syscall.NewLazyDLL("Kernel32.dll")
	advapi32 = syscall.NewLazyDLL("Advapi32.dll")
	ntdll    = syscall.NewLazyDLL("ntdll.dll")

	_CreateFileMappingW = kernel32.NewProc("CreateFileMappingW")
	_OpenFileMappingW   = kernel32.NewProc("OpenFileMappingW")
	_CreateSemaphoreW   = kernel32.NewProc("CreateSemaphoreW")
	_OpenSemaphoreW     = kernel32.NewProc("OpenSemaphoreW")
	_ReleaseSemaphore   = kernel32.NewProc("ReleaseSemaphore")

	_ConvertStringSecurityDescriptorToSecurityDescriptorW = advapi32.NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW")

	_NtQuerySection        = ntdll.NewProc("NtQuerySection")
	_RtlNtStatusToDosError = ntdll.NewProc("RtlNtStatusToDosError")
)

const ERROR_ALREADY_EXISTS syscall.Errno = 183

func nsCreateFileMapping(hFile syscall.Handle, lpFileMappingAttributes *syscall.SecurityAttributes, flProtect uint32, MaximumSize uint64, lpName string) (syscall.Handle, bool, error) {
	b, err := syscall.UTF16PtrFromString(lpName)
	if err != nil {
		return syscall.InvalidHandle, false, err
	}

	high := uint32(MaximumSize >> 32)
	low := uint32(MaximumSize & 0xffffffff)

	ret, _, err := _CreateFileMappingW.Call(
		uintptr(hFile),
		uintptr(unsafe.Pointer(lpFileMappingAttributes)),
		uintptr(flProtect),
		uintptr(high),
		uintptr(low),
		uintptr(unsafe.Pointer(b)),
	)
	runtime.KeepAlive(b)
	runtime.KeepAlive(lpFileMappingAttributes)

	if ret == 0 {
		if err == syscall.Errno(0) {
			err = syscall.EINVAL
		}
		return syscall.InvalidHandle, false, err
	}

	return syscall.Handle(ret), err == ERROR_ALREADY_EXISTS, nil
}

func nsOpenFileMapping(dwDesiredAccess uint32, bInheritHandle bool, lpName string) (syscall.Handle, error) {
	b, err := syscall.UTF16PtrFromString(lpName)
	if err != nil {
		return syscall.InvalidHandle, err
	}

	inherit := 0
	if bInheritHandle {
		inherit = 1
	}

	ret, _, err := _OpenFileMappingW.Call(
		uintptr(dwDesiredAccess),
		uintptr(inherit),
		uintptr(unsafe.Pointer(b)),
	)
	runtime.KeepAlive(b)

	if ret == 0 {
		if err == syscall.Errno(0) {
			err = syscall.EINVAL
		}
		return syscall.InvalidHandle, err
	}

	return syscall.Handle(ret), nil
}

// securityDescriptor is a self-relative security descriptor built from a Unix
// permission mode, usable as the SECURITY_ATTRIBUTES of a kernel object.
type securityDescriptor struct {
	attrs syscall.SecurityAttributes
	sd    uintptr // allocated by LocalAlloc
}

// currentUserSID returns the string SID of the user of the process token.
func currentUserSID() (string, error) {
	token, err := syscall.OpenCurrentProcessToken()
	if err != nil {
		return "", err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return "", err
	}
	return user.User.Sid.String()
}

// newSecurityDescriptor builds the security attributes for a mapping
// (section == true) or semaphore created with mode.
func newSecurityDescriptor(mode os.FileMode, section bool) (*securityDescriptor, error) {
	userSID, err := currentUserSID()
	if err != nil {
		return nil, fmt.Errorf("query current user: %w", err)
	}
	sddl, err := syscall.UTF16PtrFromString(securityDescriptorSDDL(mode, userSID, section))
	if err != nil {
		return nil, err
	}
	d := &securityDescriptor{}
	ret, _, err := _ConvertStringSecurityDescriptorToSecurityDescriptorW.Call(
		uintptr(unsafe.Pointer(sddl)),
		sddlRevision1,
		uintptr(unsafe.Pointer(&d.sd)),
		0,
	)
	runtime.KeepAlive(sddl)
	if ret == 0 {
		return nil, fmt.Errorf("convert security descriptor: %w", err)
	}
	d.attrs = syscall.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(syscall.SecurityAttributes{})),
		SecurityDescriptor: d.sd,
	}
	return d, nil
}

// free releases the descriptor memory. The attributes are not usable
// afterwards.
func (d *securityDescriptor) free() {
	if d != nil && d.sd != 0 {
		_, _ = syscall.LocalFree(syscall.Handle(d.sd))
		d.sd = 0
	}
}

// semaphore is a named kernel semaphore.
type semaphore struct {
	handle syscall.Handle
}

// createSemaphore creates the semaphore name with sa, or opens it if it
// already exists, in which case alreadyExists is true. The semaphore starts
// without tokens and holds up to math.MaxInt32.
func createSemaphore(sa *syscall.SecurityAttributes, name string) (sem *semaphore, alreadyExists bool, err error) {
	p, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return nil, false, err
	}
	ret, _, err := _CreateSemaphoreW.Call(
		uintptr(unsafe.Pointer(sa)),
		0,
		math.MaxInt32,
		uintptr(unsafe.Pointer(p)),
	)
	runtime.KeepAlive(p)
	runtime.KeepAlive(sa)
	if ret == 0 {
		if err == syscall.Errno(0) {
			err = syscall.EINVAL
		}
		return nil, false, err
	}
	// CreateSemaphoreW succeeds with ERROR_ALREADY_EXISTS when it opened an
	// existing semaphore instead of creating one.
	return &semaphore{handle: syscall.Handle(ret)}, err == ERROR_ALREADY_EXISTS, nil
}

// openSemaphore opens the existing semaphore name for waiting and releasing.
func openSemaphore(name string) (*semaphore, error) {
	p, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	ret, _, err := _OpenSemaphoreW.Call(
		standardSynchronize|semaphoreModifyState,
		0,
		uintptr(unsafe.Pointer(p)),
	)
	runtime.KeepAlive(p)
	if ret == 0 {
		if err == syscall.Errno(0) {
			err = syscall.EINVAL
		}
		return nil, err
	}
	return &semaphore{handle: syscall.Handle(ret)}, nil
}

// Wait implements Semaphore.
func (s *semaphore) Wait(timeout time.Duration) {
	ms := (timeout + time.Millisecond - 1) / time.Millisecond
	switch {
	case ms < 1:
		ms = 1
	case ms >= math.MaxUint32:
		ms = math.MaxUint32 - 1 // INFINITE is 0xFFFFFFFF
	}
	_, _ = syscall.WaitForSingleObject(s.handle, uint32(ms))
}

// Release implements Semaphore. ERROR_TOO_MANY_POSTS (the semaphore is at
// its maximum count) means that plenty of wake-ups are pending already.
func (s *semaphore) Release(n uint32) {
	if n == 0 {
		return
	}
	if n > math.MaxInt32 {
		n = math.MaxInt32
	}
	_, _, _ = _ReleaseSemaphore.Call(uintptr(s.handle), uintptr(n), 0)
}

func (s *semaphore) close() error {
	return syscall.CloseHandle(s.handle)
}

// closeSemaphores closes and clears every semaphore in sems.
func closeSemaphores(sems *[SemaphoreCount]Semaphore) error {
	var err error
	for i, sem := range sems {
		if sem != nil {
			err = errors.Join(err, sem.(*semaphore).close())
			sems[i] = nil
		}
	}
	return err
}

// OpenSharedMemory opens or creates the named file mapping name. Names
// without a namespace prefix are created in the session-local "Local\"
// namespace, which does not require SeCreateGlobalPrivilege. Pass an explicit
// "Global\name" to share a mapping across sessions (for example between a
// service and a desktop application).
//
// The mapping is backed by the system paging file and is accompanied by
// SemaphoreCount named kernel semaphores (see Semaphore) whose names are the
// mapping's kernel object name plus a ".hqq-semN" suffix. Both names must fit
// the kernel's MAX_PATH limit for object names; longer names fail with
// ErrInvalidName. A creator makes the semaphores before the mapping, so that
// every endpoint that can open the mapping finds them.
//
// With os.O_EXCL, creation fails with an error matching os.ErrExist when the
// mapping name or any of its semaphore names already exists, which always
// means that some process holds a live handle to it.
//
// When creating, mode is translated to a DACL (Windows has no Unix modes):
//
//   - SYSTEM always gets full access.
//   - The owner bits apply to the user SID of the creating process's token:
//     read grants read access, write grants full access (a writing owner
//     controls the object). Without owner bits the user gets no ACE.
//   - The "other" bits apply to Everyone: read grants read access to the
//     mapping, write grants read and write access.
//   - The group bits are ignored. Windows has no per-object "owning group"
//     (the token's primary group is a broad, usually domain-wide group, and
//     granting it access would be far more permissive than a Unix group), so
//     to share a segment with other accounts use the "other" bits.
//
// The creator's own handles always have full access. Mode 0 therefore
// creates a segment that only the creating process and SYSTEM can reach,
// like a Unix 0000 file. mode is ignored when opening an existing mapping.
// The same DACL is applied to the semaphores (read access allows waiting,
// write access additionally allows releasing).
func OpenSharedMemory(name string, size int, flags int, mode os.FileMode) (*SharedMemory, error) {
	s := &SharedMemory{
		name: name,
		size: size,
	}

	objectName, err := windowsSharedMemoryName(name)
	if err != nil {
		return nil, err
	}

	if flags&os.O_CREATE != 0 {
		err = s.create(objectName, size, flags, mode)
	} else {
		err = s.open(objectName, flags)
	}
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (s *SharedMemory) create(objectName string, size, flags int, mode os.FileMode) (err error) {
	prot := syscall.PAGE_READONLY
	if flags&os.O_RDWR != 0 {
		prot = syscall.PAGE_READWRITE
	}

	sectionSD, err := newSecurityDescriptor(mode, true)
	if err != nil {
		return &os.PathError{Op: "CreateFileMapping", Path: s.name, Err: err}
	}
	defer sectionSD.free()
	semaphoreSD, err := newSecurityDescriptor(mode, false)
	if err != nil {
		return &os.PathError{Op: "CreateSemaphore", Path: s.name, Err: err}
	}
	defer semaphoreSD.free()

	defer func() {
		if err != nil {
			_ = closeSemaphores(&s.sems)
		}
	}()
	for i, suffix := range semaphoreSuffixes {
		sem, existed, serr := createSemaphore(&semaphoreSD.attrs, objectName+suffix)
		if serr != nil {
			return &os.PathError{Op: "CreateSemaphore", Path: s.name, Err: serr}
		}
		s.sems[i] = sem
		if existed && flags&os.O_EXCL != 0 {
			// A live handle to the name's semaphore exists: the name is in
			// use. The deferred cleanup closes the handles opened so far.
			return &os.PathError{Op: "CreateSemaphore", Path: s.name, Err: os.ErrExist}
		}
	}

	fd, alreadyExists, err := nsCreateFileMapping(
		syscall.InvalidHandle,
		&sectionSD.attrs,
		uint32(prot),
		uint64(size),
		objectName,
	)
	if err != nil {
		return &os.PathError{Op: "CreateFileMapping", Path: s.name, Err: err}
	}
	if alreadyExists && flags&os.O_EXCL != 0 {
		_ = syscall.CloseHandle(fd)
		return &os.PathError{Op: "CreateFileMapping", Path: s.name, Err: os.ErrExist}
	}
	s.fd = uintptr(fd)
	return nil
}

func (s *SharedMemory) open(objectName string, flags int) (err error) {
	// SECTION_QUERY lets CurrentSize read the size of the mapping.
	access := uint32(fileMapRead | sectionQuery)
	if flags&os.O_RDWR != 0 {
		access |= fileMapWrite
	}
	fd, err := nsOpenFileMapping(
		access,
		false,
		objectName,
	)
	if err != nil {
		return &os.PathError{Op: "OpenFileMapping", Path: s.name, Err: err}
	}
	s.fd = uintptr(fd)

	defer func() {
		if err != nil {
			_ = closeSemaphores(&s.sems)
			_ = syscall.CloseHandle(fd)
		}
	}()
	for i, suffix := range semaphoreSuffixes {
		sem, serr := openSemaphore(objectName + suffix)
		if serr != nil {
			return &os.PathError{Op: "OpenSemaphore", Path: s.name, Err: serr}
		}
		s.sems[i] = sem
	}
	return nil
}

// Unlink is a no-op on Windows after validating the name.
//
// A mapping created by OpenSharedMemory is backed by the system paging file,
// not by a file: its name lives in the kernel object namespace and exists
// exactly as long as at least one handle to the mapping (or, for the wake
// semaphores, to the semaphore) is open, in any process. Windows offers no
// call that removes the name of such an object while handles remain open, so
// "unlink now, keep using the mapping" cannot be expressed. The consequences
// are benign: a crashed process cannot leave a stale segment behind, because
// the kernel closes its handles and the object disappears with the last one.
//
// Unlink semantics would require backing the mapping with a real file opened
// with FILE_FLAG_DELETE_ON_CLOSE (or deleted via FileDispositionInfoEx
// with POSIX semantics) and sharing that file's path instead of an object
// name. That costs a writable directory that all endpoints agree on, file
// system permissions instead of kernel object security, dirty pages that the
// system may write back to disk (FILE_ATTRIBUTE_TEMPORARY only reduces
// that), files left behind when a process dies before deleting them, and the
// loss of the Local\/Global\ namespaces.
func Unlink(name string) error {
	_, err := windowsSharedMemoryName(name)
	return err
}

// Delete is a no-op on Windows; see Unlink.
func (s *SharedMemory) Delete() error {
	return nil
}

// NamesPersist is false on Windows: a name exists exactly as long as a handle
// to its mapping or semaphores is open, so a name never outlives its users.
// An existing name therefore always means a live handle somewhere, and
// creating over it with os.O_EXCL fails with os.ErrExist.
const NamesPersist = false

// NameRefersToSelf is not needed on Windows (Delete is a no-op): known is
// always false.
func (s *SharedMemory) NameRefersToSelf() (known, same bool, err error) {
	return false, false, nil
}

// sectionBasicInformation is SECTION_BASIC_INFORMATION from winternl.h.
type sectionBasicInformation struct {
	BaseAddress          uintptr
	AllocationAttributes uint32
	MaximumSize          int64
}

// CurrentSize returns the size of the mapping, which Windows fixes when the
// mapping is created. It is queried with NtQuerySection
// (SectionBasicInformation), the only call that reports the size of an
// existing section: VirtualQuery on a view only sees the part of the section
// that the view covers, and a view larger than the section cannot be created
// to find out. NtQuerySection requires SECTION_QUERY access, which
// OpenSharedMemory requests when opening. The size is the one passed to
// CreateFileMapping, so a mismatch with the size an opener expects is
// reported before the opener maps memory beyond the end of the section.
func (s *SharedMemory) CurrentSize() (int64, error) {
	var info sectionBasicInformation
	var returned uintptr
	status, _, _ := _NtQuerySection.Call(
		s.fd,
		0, // SectionBasicInformation
		uintptr(unsafe.Pointer(&info)),
		unsafe.Sizeof(info),
		uintptr(unsafe.Pointer(&returned)),
	)
	if uint32(status)>>30 == 3 { // NT_ERROR: severity "error"
		dos, _, _ := _RtlNtStatusToDosError.Call(status)
		if dos == 0 || dos == 0xffffffff { // no mapping to a Win32 error
			return 0, &os.PathError{Op: "NtQuerySection", Path: s.name, Err: fmt.Errorf("NTSTATUS %#x", uint32(status))}
		}
		return 0, &os.PathError{Op: "NtQuerySection", Path: s.name, Err: syscall.Errno(dos)}
	}
	return info.MaximumSize, nil
}

// Close closes the mapping handle and the wake semaphores. Views stay valid
// until unmapped.
func (s *SharedMemory) Close() error {
	return errors.Join(syscall.CloseHandle(syscall.Handle(s.fd)), closeSemaphores(&s.sems))
}
