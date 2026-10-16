package shm

// This file holds the pure naming and security-descriptor logic of the
// Windows backend. It has no build constraint so that it can be tested on
// every platform; nothing else uses it outside Windows.

import (
	"fmt"
	"os"
	"strings"
	"unicode/utf16"
)

// Access rights. FILE_MAP_* are the SECTION_* rights; the rights of a
// semaphore are SEMAPHORE_QUERY_STATE (0x1) and SEMAPHORE_MODIFY_STATE (0x2)
// on top of the standard rights.
const (
	fileMapWrite = 0x0002 // SECTION_MAP_WRITE
	fileMapRead  = 0x0004 // SECTION_MAP_READ

	sectionQuery = 0x0001 // required by NtQuerySection

	standardReadControl = 0x00020000 // READ_CONTROL
	standardSynchronize = 0x00100000 // SYNCHRONIZE

	semaphoreQueryState  = 0x0001
	semaphoreModifyState = 0x0002

	sectionAllAccess   = 0x000F001F // SECTION_ALL_ACCESS
	semaphoreAllAccess = 0x001F0003 // SEMAPHORE_ALL_ACCESS

	sddlRevision1 = 1
)

// maxObjectName is the longest kernel object name (in UTF-16 code units,
// without the terminating NUL) accepted by CreateFileMapping and
// CreateSemaphore: they limit names to MAX_PATH characters.
const maxObjectName = 259

// semaphoreSuffixes are appended to the mapping's kernel object name to name
// its wake semaphores. Kernel object names share one namespace across object
// types, so the suffixes make the names distinct from the mapping itself.
var semaphoreSuffixes = [SemaphoreCount]string{".hqq-sem0", ".hqq-sem1", ".hqq-sem2", ".hqq-sem3"}

// permRights maps the read/write permission bits of one Unix class to access
// masks. Reading grants the rights needed to map the object for reading;
// writing grants read and write. A class without bits gets no access.
type permRights struct{ section, semaphore uint32 }

func rightsForBits(bits os.FileMode) permRights {
	var r permRights
	if bits&0o4 != 0 || bits&0o2 != 0 {
		r.section = standardReadControl | sectionQuery | fileMapRead
		r.semaphore = standardReadControl | standardSynchronize | semaphoreQueryState
	}
	if bits&0o2 != 0 {
		r.section |= fileMapWrite
		r.semaphore |= semaphoreModifyState
	}
	return r
}

// securityDescriptorSDDL returns the SDDL DACL that mode translates to for
// the mapping (section == true) or its semaphores, given the SID string of
// the current user. See OpenSharedMemory for the mapping rules.
func securityDescriptorSDDL(mode os.FileMode, userSID string, section bool) string {
	pick := func(r permRights) uint32 {
		if section {
			return r.section
		}
		return r.semaphore
	}
	all := uint32(semaphoreAllAccess)
	if section {
		all = sectionAllAccess
	}

	var b strings.Builder
	// P: protected, nothing is inherited into the descriptor.
	b.WriteString("D:P")
	fmt.Fprintf(&b, "(A;;0x%x;;;SY)", all)
	owner := mode.Perm() >> 6 & 0o7
	if owner != 0 {
		mask := pick(rightsForBits(owner))
		if owner&0o2 != 0 {
			mask = all // a writing owner controls the object
		}
		fmt.Fprintf(&b, "(A;;0x%x;;;%s)", mask, userSID)
	}
	if other := mode.Perm() & 0o7; other != 0 {
		fmt.Fprintf(&b, "(A;;0x%x;;;WD)", pick(rightsForBits(other)))
	}
	return b.String()
}

// windowsSharedMemoryName maps a portable name to a kernel object name.
// "Global\x" and "Local\x" are kept; other names get the "Local\" prefix. The
// result, including the longest wake-semaphore suffix, must fit the kernel's
// limit for object names.
func windowsSharedMemoryName(name string) (string, error) {
	objectName, err := kernelObjectName(name)
	if err != nil {
		return "", err
	}
	longest := 0
	for _, suffix := range semaphoreSuffixes {
		longest = max(longest, len(suffix))
	}
	if n := len(utf16.Encode([]rune(objectName))); n+longest > maxObjectName {
		return "", fmt.Errorf("%w: %q is longer than %d characters once its kernel object name %q and wake-semaphore suffix are formed",
			ErrInvalidName, name, maxObjectName-longest-len(`Local\`), objectName)
	}
	return objectName, nil
}

func kernelObjectName(name string) (string, error) {
	for _, prefix := range []string{`Global\`, `Local\`} {
		if len(name) > len(prefix) && strings.EqualFold(name[:len(prefix)], prefix) {
			base, err := baseName(name[len(prefix):], 0)
			if err != nil {
				return "", err
			}
			return prefix + base, nil
		}
	}
	base, err := baseName(name, 0)
	if err != nil {
		return "", err
	}
	return `Local\` + base, nil
}
