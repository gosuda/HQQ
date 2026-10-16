package conformance

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"gosuda.org/hqq"
	"gosuda.org/hqq/internal/mmap"
	"gosuda.org/hqq/internal/protocol"
	"gosuda.org/hqq/internal/shm"
)

// vectorsDir is the committed vector tree, relative to this package.
const vectorsDir = "vectors"

// attachTimeout keeps attach-path tests fast (SPEC 5.1, AT).
const attachTimeout = 300 * time.Millisecond

// loadManifest reads and decodes vectors/manifest.json.
func loadManifest(t *testing.T) *Manifest {
	t.Helper()
	var m Manifest
	if err := json.Unmarshal(readVector(t, "manifest.json"), &m); err != nil {
		t.Fatalf("decode manifest.json: %v", err)
	}
	if m.Schema != ManifestSchema {
		t.Fatalf("manifest schema %d, want %d", m.Schema, ManifestSchema)
	}
	return &m
}

// readVector returns the bytes of a file of the committed vector tree.
func readVector(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(vectorsDir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read vector %s: %v (regenerate with: go run ./cmd/hqq-interop gen-vectors --out conformance/vectors)", rel, err)
	}
	return b
}

// mustDecodeHex decodes a hex vector, failing the test on bad input.
func mustDecodeHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex %q: %v", s, err)
	}
	return b
}

// packetOf converts a 64-byte hex packet into a protocol.Packet.
func packetOf(t *testing.T, s string) protocol.Packet {
	t.Helper()
	b := mustDecodeHex(t, s)
	if len(b) != protocol.PacketSize {
		t.Fatalf("packet %q has %d bytes, want %d", s, len(b), protocol.PacketSize)
	}
	return protocol.Packet(b)
}

// sentinels maps the canonical error kinds of INTEROP_CLI section 4.1 that
// the package exports to their sentinel values.
var sentinels = map[string]error{
	"ErrMemoryAlign":        hqq.ErrMemoryAlign,
	"ErrInvalidSize":        hqq.ErrInvalidSize,
	"ErrMemorySmall":        hqq.ErrMemorySmall,
	"ErrFailedInit":         hqq.ErrFailedInit,
	"ErrConnNotFound":       hqq.ErrConnNotFound,
	"ErrInvalidOp":          hqq.ErrInvalidOp,
	"ErrBufferOverflow":     hqq.ErrBufferOverflow,
	"ErrInvalidName":        hqq.ErrInvalidName,
	"ErrLinkBusy":           hqq.ErrLinkBusy,
	"ErrIncompatibleLayout": hqq.ErrIncompatibleLayout,
	"ErrClosed":             hqq.ErrClosed,
	"ErrPeerClosed":         hqq.ErrPeerClosed,
	"ErrPeerGone":           hqq.ErrPeerGone,
	"ErrTimeout":            hqq.ErrTimeout,
	"EOF":                   io.EOF,
	"ErrProtocol":           hqq.ErrProtocol,
	"ErrMessageAborted":     hqq.ErrMessageAborted,
	"ErrReassemblyLimit":    hqq.ErrReassemblyLimit,
	"ErrIncomingOverflow":   hqq.ErrIncomingOverflow,
	"ErrUnexpectedPacket":   hqq.ErrUnexpectedPacket,
	"ErrShuttingDown":       hqq.ErrShuttingDown,
}

// matchesKind reports whether err is the canonical error kind want. The most
// specific category must match: an ErrFailedInit expectation is not satisfied
// by ErrLinkBusy or ErrIncompatibleLayout, which also match it.
func matchesKind(err error, want string) bool {
	if err == nil {
		return false
	}
	switch want {
	case "ResponseError":
		var re hqq.ResponseError
		return errors.As(err, &re)
	case "ErrFailedInit":
		return errors.Is(err, hqq.ErrFailedInit) &&
			!errors.Is(err, hqq.ErrLinkBusy) && !errors.Is(err, hqq.ErrIncompatibleLayout)
	}
	s, ok := sentinels[want]
	if !ok {
		panic("conformance: no sentinel for error kind " + want)
	}
	return errors.Is(err, s)
}

// hostPage is the page size of the host, which selects the family that Go
// runs on this machine.
func hostPage() int { return os.Getpagesize() }

var nameCounter atomic.Uint64

// uniqueName returns a shared-memory name unique per process and per call,
// within the 30-byte limit of macOS (without the leading slash).
func uniqueName() string {
	return fmt.Sprintf("hqqc%d-%d", os.Getpid()%100000, nameCounter.Add(1))
}

// testOpts are the link options of the attach tests: no heartbeat goroutine,
// a short attach timeout and a 1 ms peer timeout (the stale window of an
// initializer is then its 2 s floor, SPEC 5.1).
func testOpts() hqq.LinkOptions {
	return hqq.LinkOptions{
		HeartbeatInterval: -1,
		AttachTimeout:     attachTimeout,
		PeerTimeout:       time.Millisecond,
	}
}

// createNamedObject creates the shared-memory object name with size bytes,
// writes image at its start (the rest stays zero) and returns its mapping.
// The name is unlinked when the test ends, after the links closed.
func createNamedObject(t *testing.T, name string, image []byte, size int) []byte {
	t.Helper()
	smem, err := shm.OpenSharedMemory(name, size, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	view, err := mmap.Map(smem.FD(), 0, size, mmap.PROT_READ|mmap.PROT_WRITE, mmap.MAP_SHARED)
	_ = smem.Close()
	if err != nil {
		_ = hqq.Unlink(name)
		t.Fatalf("map %s: %v", name, err)
	}
	copy(view, image)
	t.Cleanup(func() {
		_ = mmap.UnMap(view)
		_ = hqq.Unlink(name)
	})
	return view
}

// segmentWord reads the little-endian word at off of an image or a view.
func segmentWord(b []byte, off int) uint64 {
	return binary.LittleEndian.Uint64(b[off : off+8])
}

// storeWord writes the word at off of a shared mapping with an atomic store,
// as a peer would. The mapping is 8-byte aligned (page aligned).
func storeWord(b []byte, off int, v uint64) {
	atomic.StoreUint64((*uint64)(unsafe.Pointer(&b[off])), v)
}

// firstDiffOutside returns the first offset outside [lo, hi) at which a and b
// differ (a length difference counts), or -1 when they agree there.
func firstDiffOutside(a, b []byte, lo, hi int) int {
	n := min(len(a), len(b))
	for i := range n {
		if i >= lo && i < hi {
			continue
		}
		if a[i] != b[i] {
			return i
		}
	}
	if len(a) != len(b) {
		return n
	}
	return -1
}
