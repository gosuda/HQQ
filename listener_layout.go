package hqq

import (
	"strconv"
	"strings"
	"unsafe"

	"gosuda.org/hqq/internal/mpmc"
)

// Listener segment layout (magic "HQQ_LSN1", layout version 1.0). The
// specification, including the state machine and the validation rules, is
// spec/ext/LISTENER_DRAFT.md.
//
//	offset 0       HEADER (listenerHeaderSize = 4096 bytes, uint64 words)
//	offset 4096    MPMC_RING of connectRecord (internal/mpmc layout, capacity slots)
//	               padded up to a multiple of the page size
//
// A segment name is not a link name: clients create one ordinary link
// segment of their own and announce it with a connect record in the ring.
const (
	// listenerMagic identifies a listener segment ("HQQ_LSN1").
	listenerMagic uint64 = 0x4851515f4c534e31

	// listenerMajor and listenerMinor are the layout version. A different
	// major version is incompatible; the minor version is informational.
	listenerMajor uint64 = 1
	listenerMinor uint64 = 0

	// listenerKnownRequiredFeatures is the set of requiredFeatures bits this
	// build understands. No listener layout feature is defined yet.
	listenerKnownRequiredFeatures uint64 = 0

	// The listener states. They share their values with the link superblock
	// states.
	listenerStateEmpty        = superblockStateEmpty
	listenerStateInitializing = superblockStateInitializing
	listenerStateReady        = superblockStateReady

	// listenerHeaderSize is the size of the header; the ring starts right
	// behind it, whatever the page size is.
	listenerHeaderSize = 4096
	listenerRingOffset = listenerHeaderSize

	// listenerRecordSize is the size of a connect record: with the 8-byte
	// sequence word of the ring slot it fills exactly 256 bytes.
	listenerRecordSize = 248

	// listenerMaxNameLen is the size of the name field of a connect record.
	listenerMaxNameLen = 184

	// listenerMaxPrefixLen is the size of the link name prefix field in the
	// header.
	listenerMaxPrefixLen = 32

	// listenerSuffixLen is the length of the random part of a link name:
	// 16 lowercase hexadecimal digits.
	listenerSuffixLen = 16

	// listenerStemLen bounds the part of the listener name that is copied
	// into the link name prefix. With the dot and the random suffix a link
	// name stays below the 31-byte limit of macOS.
	listenerStemLen = 12

	// DefaultAcceptorQueueSize is the default number of connect records the
	// listener ring holds.
	DefaultAcceptorQueueSize = 64

	// maxAcceptorQueueSize bounds AcceptorConfig.QueueSize.
	maxAcceptorQueueSize = 1 << 16

	// The internal/mpmc ring layout facts that stuck-slot recovery needs: the
	// byte offsets of the consumer cursor and the producer cursor in the
	// ring header, the size of the header, and the stride of a slot of a
	// connectRecord. The sequence word of a slot is its first word. A test
	// pins these against the real ring.
	ringReadCursorOffset  = 128
	ringWriteCursorOffset = 256
	ringHeaderSize        = mpmc.HeaderSize
	connectSlotStride     = 256
)

// listenerHeader is the first 4096 bytes of a listener segment. Every field
// is accessed atomically (the prefix is written before the segment becomes
// Ready and never changes afterwards). Field order is part of the
// shared-memory ABI.
//
// state, creatorToken and the other fields up to creatorToken have the same
// offsets as in the link superblock, so the name-ownership checks of named
// segments (namedMapping.ownsName) work on listener segments unchanged.
type listenerHeader struct {
	magic            uint64 // @0   listenerMagic
	state            uint64 // @8   listenerState*
	major            uint64 // @16  listenerMajor
	minor            uint64 // @24  listenerMinor
	ringOffset       uint64 // @32  listenerRingOffset
	recordSize       uint64 // @40  listenerRecordSize
	capacity         uint64 // @48  slots of the ring
	layoutSize       uint64 // @56  mapped size in bytes
	epoch            uint64 // @64  incremented on every (re)initialization
	initToken        uint64 // @72  token of the endpoint currently initializing
	initHeartbeat    uint64 // @80  unix nanos of the initializer's last progress
	requiredFeatures uint64 // @88  layout features a reader must understand
	creatorToken     uint64 // @96  identifies the instance that created the name
	_                [3]uint64

	serverToken     uint64 // @128 owner token of the acceptor; 0 none, detachedToken closed
	serverPID       uint64 // @136 diagnostic only
	serverHeartbeat uint64 // @144 unix nanos, refreshed every serverInterval
	serverInterval  uint64 // @152 heartbeat interval in nanoseconds; 0 = disabled
	maxBufferCount  uint64 // @160 largest BufferCount the acceptor accepts
	maxBufferSize   uint64 // @168 largest BufferSize the acceptor accepts
	maxLinkSize     uint64 // @176 largest link segment (SizeStandardLink) it accepts
	features        uint64 // @184 features the acceptor supports

	prefixLen uint64                     // @192 length of prefix
	prefix    [listenerMaxPrefixLen]byte // @200 link name prefix
}

var _ [listenerHeaderSize - unsafe.Sizeof(listenerHeader{})]struct{} // must fit the header

// The header offsets are part of the shared-memory ABI.
const (
	_ = uint(unsafe.Offsetof(listenerHeader{}.state) - 8)
	_ = uint(8 - unsafe.Offsetof(listenerHeader{}.state))
	_ = uint(unsafe.Offsetof(listenerHeader{}.creatorToken) - 96)
	_ = uint(96 - unsafe.Offsetof(listenerHeader{}.creatorToken))
	_ = uint(unsafe.Offsetof(listenerHeader{}.serverToken) - 128)
	_ = uint(128 - unsafe.Offsetof(listenerHeader{}.serverToken))
	_ = uint(unsafe.Offsetof(listenerHeader{}.prefix) - 200)
	_ = uint(200 - unsafe.Offsetof(listenerHeader{}.prefix))

	// The listener and the link superblock share these offsets (see
	// listenerHeader).
	_ = uint(unsafe.Offsetof(listenerHeader{}.state) - unsafe.Offsetof(standardSuperblock{}.state))
	_ = uint(unsafe.Offsetof(standardSuperblock{}.state) - unsafe.Offsetof(listenerHeader{}.state))
	_ = uint(unsafe.Offsetof(listenerHeader{}.creatorToken) - unsafe.Offsetof(standardSuperblock{}.creatorToken))
	_ = uint(unsafe.Offsetof(standardSuperblock{}.creatorToken) - unsafe.Offsetof(listenerHeader{}.creatorToken))
)

// connectRecord is one slot payload of the listener ring: the request of a
// client to be served over the link segment it created. It is pointer-free
// and every bit pattern is valid input; receivers validate every field.
// Field order is part of the shared-memory ABI.
type connectRecord struct {
	clientToken uint64 // @0  creatorToken of the client's link segment
	bufferCount uint64 // @8  BufferCount of the link segment
	bufferSize  uint64 // @16 BufferSize of the link segment
	features    uint64 // @24 features the client requests (informational)
	flags       uint64 // @32 must be 0
	nameLen     uint64 // @40 length of name
	clientPID   uint64 // @48 diagnostic only
	reserved    uint64 // @56 must be 0

	name [listenerMaxNameLen]byte // @64 name of the link segment
}

var (
	_ [listenerRecordSize - unsafe.Sizeof(connectRecord{})]struct{}
	_ [unsafe.Sizeof(connectRecord{}) - listenerRecordSize]struct{}
	_ [connectSlotStride - mpmcSlotSize]struct{}
	_ [mpmcSlotSize - connectSlotStride]struct{}
)

// mpmcSlotSize is the size of a ring slot of a connectRecord before it is
// rounded up to the slot alignment: the sequence word plus the record.
const mpmcSlotSize = 8 + listenerRecordSize

// listenerLayoutSize returns the size of a listener segment whose ring holds
// capacity records, or 0 if capacity is not a power of two between 2 and
// maxAcceptorQueueSize.
func listenerLayoutSize(capacity int) uintptr {
	if !isPowerOfTwo(capacity) || capacity < 2 || capacity > maxAcceptorQueueSize {
		return 0
	}
	ring := mpmc.SizeMPMCRing[connectRecord](uintptr(capacity))
	if ring == 0 {
		return 0
	}
	total, ok := alignedSum(listenerRingOffset, ring)
	if !ok {
		return 0
	}
	return total
}

// listenerLinkPrefix derives the prefix of the link segment names of
// clients from a listener name: the scope prefix of the name ("Global\" or
// "Local\", Windows) if there is one, up to listenerStemLen characters of
// the rest (anything outside [A-Za-z0-9_-] becomes '_'), and a dot. The
// acceptor stores the result in the listener header; clients use it as it is
// and never derive it themselves.
func listenerLinkPrefix(listenerName string) string {
	scope := ""
	for _, p := range []string{`Global\`, `Local\`} {
		if len(listenerName) > len(p) && strings.EqualFold(listenerName[:len(p)], p) {
			scope = listenerName[:len(p)]
			listenerName = listenerName[len(p):]
			break
		}
	}
	listenerName = strings.TrimPrefix(listenerName, "/")
	var b strings.Builder
	b.WriteString(scope)
	n := 0
	for i := 0; i < len(listenerName) && n < listenerStemLen; i, n = i+1, n+1 {
		c := listenerName[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
			b.WriteByte(c)
		default:
			b.WriteByte('_')
		}
	}
	b.WriteByte('.')
	return b.String()
}

// newLinkName returns a new link segment name: prefix and 16 random
// lowercase hexadecimal digits.
func newLinkName(prefix string) string {
	var buf [listenerSuffixLen]byte
	for i := range buf {
		buf[i] = '0'
	}
	digits := strconv.AppendUint(buf[:0], newToken(), 16)
	copy(buf[listenerSuffixLen-len(digits):], digits)
	return prefix + string(buf[:])
}

// recordName returns the name field of rec if its length is within bounds.
func (r *connectRecord) recordName() (string, bool) {
	if r.nameLen == 0 || r.nameLen > listenerMaxNameLen {
		return "", false
	}
	return string(r.name[:r.nameLen]), true
}

// setName stores name in the record; it reports false if it does not fit.
func (r *connectRecord) setName(name string) bool {
	if len(name) == 0 || len(name) > listenerMaxNameLen {
		return false
	}
	r.nameLen = uint64(len(name))
	r.name = [listenerMaxNameLen]byte{}
	copy(r.name[:], name)
	return true
}

// validLinkName reports whether name is prefix followed by exactly
// listenerSuffixLen lowercase hexadecimal digits.
func validLinkName(prefix, name string) bool {
	if len(name) != len(prefix)+listenerSuffixLen || !strings.HasPrefix(name, prefix) {
		return false
	}
	for i := len(prefix); i < len(name); i++ {
		if c := name[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
