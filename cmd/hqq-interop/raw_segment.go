package main

import (
	"fmt"
	"math/rand/v2"
	"os"
	"sync/atomic"
	"time"
	"unsafe"

	"gosuda.org/hqq/conformance"
	"gosuda.org/hqq/internal/mmap"
	"gosuda.org/hqq/internal/shm"
)

// rawZeroChunk is the largest zeroing step of an initializer (SPEC 5.3).
const rawZeroChunk = 8 << 20

// rawSegment is a mapped HQQ segment that the raw scenarios access with 64-bit
// atomics at the offsets of conformance/layout.go. The raw scenarios exist for
// the states the public API cannot reach (crash points, crafted packets).
type rawSegment struct {
	smem  *shm.SharedMemory
	mem   []byte
	count int
	size  int
	reg   conformance.Regions
}

// openRawSegment maps the named segment with geometry count x size read-write.
// With create set the object is created exclusively (zero-filled); otherwise it
// must exist and be at least as large as the layout (SPEC 11.3).
func openRawSegment(name string, count, size int, create bool) (*rawSegment, error) {
	reg, ok := conformance.ComputeRegions(os.Getpagesize(), count, size)
	if !ok {
		return nil, fmt.Errorf("raw: geometry %dx%d is outside SPEC 2.2", count, size)
	}
	flags := os.O_RDWR
	if create {
		flags |= os.O_CREATE | os.O_EXCL
	}
	smem, err := shm.OpenSharedMemory(name, reg.Total, flags, 0o600)
	if err != nil {
		return nil, err
	}
	if !create {
		cur, err := smem.CurrentSize()
		if err != nil || cur < int64(reg.Total) {
			_ = smem.Close()
			return nil, fmt.Errorf("raw: %s has size %d, want at least %d (%v)", name, cur, reg.Total, err)
		}
	}
	mem, err := mmap.Map(smem.FD(), 0, reg.Total, mmap.PROT_READ|mmap.PROT_WRITE, mmap.MAP_SHARED)
	if err != nil {
		_ = smem.Close()
		return nil, err
	}
	return &rawSegment{smem: smem, mem: mem, count: count, size: size, reg: reg}, nil
}

// close unmaps the segment and closes its handle. The name is not removed.
func (s *rawSegment) close() {
	_ = mmap.UnMap(s.mem)
	_ = s.smem.Close()
}

// word returns the 64-bit word at byte offset off of the mapping.
func (s *rawSegment) word(off int) *uint64 { return (*uint64)(unsafe.Pointer(&s.mem[off])) }

func (s *rawSegment) load(off int) uint64          { return atomic.LoadUint64(s.word(off)) }
func (s *rawSegment) store(off int, v uint64)      { atomic.StoreUint64(s.word(off), v) }
func (s *rawSegment) add(off int, d uint64) uint64 { return atomic.AddUint64(s.word(off), d) }
func (s *rawSegment) cas(off int, old, v uint64) bool {
	return atomic.CompareAndSwapUint64(s.word(off), old, v)
}

// store32 stores a 32-bit wake word or waiter count (SPEC 7.1).
func (s *rawSegment) store32(off int, v uint32) {
	atomic.StoreUint32((*uint32)(unsafe.Pointer(&s.mem[off])), v)
}

// rawView is a read-only observation of a named segment.
type rawView struct {
	Page         []byte // copy of the first page
	Magic        uint64
	State        uint64
	Major        uint64
	Epoch        uint64
	InitToken    uint64
	CreatorToken uint64
}

// rawObserve maps the first page of the named object read-only, loads the
// superblock words atomically and returns them with a copy of the page. It
// never writes (SPEC 3.3, 11.4).
func rawObserve(name string) (rawView, error) {
	page := os.Getpagesize()
	smem, err := shm.OpenSharedMemory(name, 0, os.O_RDWR, 0)
	if err != nil {
		return rawView{}, err
	}
	defer func() { _ = smem.Close() }()
	cur, err := smem.CurrentSize()
	if err != nil || cur < int64(page) {
		return rawView{}, fmt.Errorf("raw: %s is not sized yet (%d bytes, %v)", name, cur, err)
	}
	mem, err := mmap.Map(smem.FD(), 0, page, mmap.PROT_READ, mmap.MAP_SHARED)
	if err != nil {
		return rawView{}, err
	}
	defer func() { _ = mmap.UnMap(mem) }()
	s := &rawSegment{mem: mem}
	return rawView{
		Page:         append([]byte(nil), mem...),
		Magic:        s.load(conformance.SBMagic),
		State:        s.load(conformance.SBState),
		Major:        s.load(conformance.SBMajor),
		Epoch:        s.load(conformance.SBEpoch),
		InitToken:    s.load(conformance.SBInitToken),
		CreatorToken: s.load(conformance.SBCreatorToken),
	}, nil
}

// rawNow is the wall-clock stamp used by the superblock heartbeat words.
func rawNow() uint64 { return uint64(time.Now().UnixNano()) }

// rawToken returns a random owner token: neither 0 nor 1 (SPEC 3.2).
func rawToken() uint64 {
	for {
		if t := rand.Uint64(); t != 0 && t != conformance.TokenDetached {
			return t
		}
	}
}
