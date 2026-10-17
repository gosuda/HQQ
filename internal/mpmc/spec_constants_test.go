package mpmc

// Executable cross-check between spec/constants.json and the ring
// implementation: header layout, slot layout, wait-protocol tuning and the
// slot sequence protocol itself (publish: seq = pos+1, release:
// seq = pos+mask+1, wraparound of the 64-bit positions).

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"gosuda.org/hqq/internal/protocol"
)

type specRingField struct {
	Name   string  `json:"name"`
	Offset uintptr `json:"offset"`
	Size   uintptr `json:"size"`
}

type specRingConsts struct {
	Versions struct {
		RingLayout uint64 `json:"ring_layout"`
	} `json:"versions"`
	Ring struct {
		HeaderSize   uintptr                         `json:"header_size"`
		Magic        string                          `json:"magic"`
		LayoutVer    uint64                          `json:"layout_version"`
		CacheLine    uintptr                         `json:"cache_line"`
		SlotAlign    uintptr                         `json:"slot_align"`
		MinSlots     uint64                          `json:"min_slots"`
		Flags        struct{ Reserved, Init uint64 } `json:"flags"`
		HeaderFields []specRingField                 `json:"header_fields"`
		Slot         struct {
			SeqOffset     uintptr `json:"seq_offset"`
			SeqSize       uintptr `json:"seq_size"`
			PayloadOffset uintptr `json:"payload_offset"`
			PayloadSize   uintptr `json:"payload_size"`
			Unrounded     uintptr `json:"unrounded_size"`
			Stride        uintptr `json:"stride"`
			FirstSlot     uintptr `json:"first_slot_offset"`
		} `json:"slot"`
		WaitSides map[string]uint8 `json:"wait_sides"`
	} `json:"ring"`
	Packet struct {
		Size uintptr `json:"size"`
	} `json:"packet"`
	Wait struct {
		SpinIterations  int   `json:"spin_iterations"`
		YieldIterations int   `json:"yield_iterations"`
		MaxParkNS       int64 `json:"max_park_ns"`
		CheckIntervalNS int64 `json:"check_interval_ns"`
		MinSleepNS      int64 `json:"min_sleep_ns"`
		MaxSleepNS      int64 `json:"max_sleep_ns"`
	} `json:"wait"`
	Platform struct {
		WordWait   []string `json:"word_wait_platforms"`
		NoWordWait []string `json:"no_word_wait_platforms"`
	} `json:"platform"`
}

func loadSpecRing(t *testing.T) *specRingConsts {
	t.Helper()
	raw, err := os.ReadFile("../../spec/constants.json")
	if err != nil {
		t.Fatalf("read spec/constants.json: %v", err)
	}
	var c specRingConsts
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("parse spec/constants.json: %v", err)
	}
	return &c
}

func specEq[T comparable](t *testing.T, what string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v, spec/constants.json says %v", what, got, want)
	}
}

func TestSpecRingHeaderLayout(t *testing.T) {
	c := loadSpecRing(t)
	r := c.Ring

	magic, err := strconv.ParseUint(strings.TrimPrefix(r.Magic, "0x"), 16, 64)
	if err != nil {
		t.Fatal(err)
	}
	specEq(t, "_mpmc_magic", _mpmc_magic, magic)
	specEq(t, "layoutVersion", uint64(layoutVersion), r.LayoutVer)
	specEq(t, "layoutVersion (versions.ring_layout)", uint64(layoutVersion), c.Versions.RingLayout)
	specEq(t, "HeaderSize", uintptr(HeaderSize), r.HeaderSize)
	specEq(t, "cacheLine", uintptr(cacheLine), r.CacheLine)
	specEq(t, "slotAlign", uintptr(slotAlign), r.SlotAlign)
	specEq(t, "_mpmc_reserved", uint64(_mpmc_reserved), r.Flags.Reserved)
	specEq(t, "_mpmc_init", uint64(_mpmc_init), r.Flags.Init)
	specEq(t, "sizeof header", unsafe.Sizeof(_mring{}), r.HeaderSize)
	specEq(t, "validRingSize(min)", validRingSize(r.MinSlots), true)
	specEq(t, "validRingSize(min-1)", validRingSize(r.MinSlots-1), false)
	specEq(t, "validRingSize(3)", validRingSize(3), false)

	var h _mring
	offsets := map[string]uintptr{
		"magic":        unsafe.Offsetof(h._magic),
		"size":         unsafe.Offsetof(h._size),
		"flag":         unsafe.Offsetof(h._flag),
		"stride":       unsafe.Offsetof(h._stride),
		"version":      unsafe.Offsetof(h._version),
		"r":            unsafe.Offsetof(h.r),
		"w":            unsafe.Offsetof(h.w),
		"notEmpty":     unsafe.Offsetof(h.notEmpty),
		"emptyWaiters": unsafe.Offsetof(h.emptyWaiters),
		"notFull":      unsafe.Offsetof(h.notFull),
		"fullWaiters":  unsafe.Offsetof(h.fullWaiters),
	}
	sizes := map[string]uintptr{
		"magic": 8, "size": 8, "flag": 8, "stride": 8, "version": 8, "r": 8, "w": 8,
		"notEmpty": 4, "emptyWaiters": 4, "notFull": 4, "fullWaiters": 4,
	}
	specEq(t, "number of header fields", len(c.Ring.HeaderFields), len(offsets))
	for _, f := range r.HeaderFields {
		off, ok := offsets[f.Name]
		if !ok {
			t.Errorf("spec ring field %q unknown to the test", f.Name)
			continue
		}
		specEq(t, "offset of ring header "+f.Name, off, f.Offset)
		specEq(t, "size of ring header "+f.Name, sizes[f.Name], f.Size)
	}
	// Independently written groups never share a cache-line-sized line.
	lines := map[uintptr][]string{}
	for name, off := range offsets {
		lines[off/r.CacheLine] = append(lines[off/r.CacheLine], name)
	}
	if len(lines) != 4 {
		t.Errorf("header words occupy %d lines of %d bytes, want 4: %v", len(lines), r.CacheLine, lines)
	}

	// Slot layout and stride rule: round_up(8 + sizeof(T), 64).
	var e _melem[protocol.Packet]
	specEq(t, "slot seq offset", unsafe.Offsetof(e._seq), r.Slot.SeqOffset)
	specEq(t, "slot payload offset", unsafe.Offsetof(e._data), r.Slot.PayloadOffset)
	specEq(t, "slot unrounded size", unsafe.Sizeof(e), r.Slot.Unrounded)
	specEq(t, "packet size", unsafe.Sizeof(protocol.Packet{}), c.Packet.Size)
	specEq(t, "slot payload size", c.Packet.Size, r.Slot.PayloadSize)
	specEq(t, "stride[Packet]", stride[protocol.Packet](), r.Slot.Stride)
	specEq(t, "first slot offset", uintptr(HeaderSize), r.Slot.FirstSlot)
	specEq(t, "stride[[56]byte]", stride[[56]byte](), 64)
	specEq(t, "stride[[57]byte]", stride[[57]byte](), 128)
	specEq(t, "stride[[120]byte]", stride[[120]byte](), 128)
	specEq(t, "stride[[121]byte]", stride[[121]byte](), 192)

	// Ring sizes: header + stride * n; invalid slot counts give 0.
	for _, n := range []uintptr{2, 4, 256, 1 << 20} {
		specEq(t, "SizeMPMCRing", SizeMPMCRing[protocol.Packet](n), r.HeaderSize+r.Slot.Stride*n)
	}
	for _, n := range []uintptr{0, 1, 3, 6} {
		specEq(t, "SizeMPMCRing(invalid)", SizeMPMCRing[protocol.Packet](n), 0)
	}

	specEq(t, "NotEmpty side", uint8(NotEmpty), r.WaitSides["notEmpty"])
	specEq(t, "NotFull side", uint8(NotFull), r.WaitSides["notFull"])
}

func TestSpecWaitTuning(t *testing.T) {
	c := loadSpecRing(t)
	w := c.Wait
	specEq(t, "spinIterations", spinIterations, w.SpinIterations)
	specEq(t, "yieldIterations", yieldIterations, w.YieldIterations)
	specEq(t, "maxPark", int64(maxPark), w.MaxParkNS)
	specEq(t, "checkInterval", int64(checkInterval), w.CheckIntervalNS)
	specEq(t, "minSleep", int64(minSleep), w.MinSleepNS)
	specEq(t, "maxSleep", int64(maxSleep), w.MaxSleepNS)
	if runtime.GOOS == "windows" {
		specEq(t, "word wait supported on windows", parkingSupported, false)
	}
	for _, goos := range c.Platform.WordWait {
		if goos == runtime.GOOS {
			specEq(t, "parkingSupported on "+goos, parkingSupported, true)
		}
	}
	for _, goos := range c.Platform.NoWordWait {
		if goos == runtime.GOOS {
			specEq(t, "parkingSupported on "+goos, parkingSupported, false)
		}
	}
}

// specRing is a ring formatted in 8-byte aligned memory, with raw access to
// its memory image.
type specRing struct {
	mem  []uint64
	ring *MPMCRing[protocol.Packet]
	size uint64
}

func newSpecRing(t *testing.T, slots uint64) *specRing {
	t.Helper()
	n := SizeMPMCRing[protocol.Packet](uintptr(slots))
	mem := make([]uint64, n/8)
	p := unsafe.Pointer(&mem[0])
	Format[protocol.Packet](p, slots)
	ring := Attach[protocol.Packet](p, time.Second)
	if ring == nil {
		t.Fatal("Attach failed on a freshly formatted ring")
	}
	return &specRing{mem: mem, ring: ring, size: slots}
}

func (r *specRing) bytes() []byte {
	return unsafe.Slice((*byte)(unsafe.Pointer(&r.mem[0])), len(r.mem)*8)
}

func (r *specRing) u64(off uintptr) uint64 { return binary.LittleEndian.Uint64(r.bytes()[off:]) }
func (r *specRing) u32(off uintptr) uint32 { return binary.LittleEndian.Uint32(r.bytes()[off:]) }

func (r *specRing) put64(off uintptr, v uint64) { binary.LittleEndian.PutUint64(r.bytes()[off:], v) }

func (r *specRing) seq(c *specRingConsts, slot uint64) uint64 {
	return r.u64(c.Ring.Slot.FirstSlot + uintptr(slot)*c.Ring.Slot.Stride + c.Ring.Slot.SeqOffset)
}

func (r *specRing) putSeq(c *specRingConsts, slot, v uint64) {
	r.put64(c.Ring.Slot.FirstSlot+uintptr(slot)*c.Ring.Slot.Stride+c.Ring.Slot.SeqOffset, v)
}

func (r *specRing) field(c *specRingConsts, name string) uintptr {
	for _, f := range c.Ring.HeaderFields {
		if f.Name == name {
			return f.Offset
		}
	}
	panic("unknown ring field " + name)
}

func (r *specRing) packet(c *specRingConsts, slot uint64) []byte {
	off := c.Ring.Slot.FirstSlot + uintptr(slot)*c.Ring.Slot.Stride + c.Ring.Slot.PayloadOffset
	return r.bytes()[off : off+c.Packet.Size]
}

// TestSpecFormatImage checks the memory image that Format leaves behind.
func TestSpecFormatImage(t *testing.T) {
	c := loadSpecRing(t)
	const slots = 8
	r := newSpecRing(t, slots)
	magic, _ := strconv.ParseUint(strings.TrimPrefix(c.Ring.Magic, "0x"), 16, 64)
	specEq(t, "image magic", r.u64(r.field(c, "magic")), magic)
	specEq(t, "image size", r.u64(r.field(c, "size")), uint64(slots))
	specEq(t, "image flag", r.u64(r.field(c, "flag")), c.Ring.Flags.Init)
	specEq(t, "image stride", r.u64(r.field(c, "stride")), uint64(c.Ring.Slot.Stride))
	specEq(t, "image version", r.u64(r.field(c, "version")), c.Ring.LayoutVer)
	specEq(t, "image r", r.u64(r.field(c, "r")), 0)
	specEq(t, "image w", r.u64(r.field(c, "w")), 0)
	for _, name := range []string{"notEmpty", "emptyWaiters", "notFull", "fullWaiters"} {
		specEq(t, "image "+name, r.u32(r.field(c, name)), 0)
	}
	for i := uint64(0); i < slots; i++ {
		specEq(t, "initial seq["+strconv.FormatUint(i, 10)+"]", r.seq(c, i), i)
		for _, b := range r.packet(c, i) {
			if b != 0 {
				t.Fatalf("slot %d payload is not zeroed by Format", i)
			}
		}
	}
	// Format clears stale wake words and waiter counts left by a dead process.
	r.put64(r.field(c, "r"), 99)
	binary.LittleEndian.PutUint32(r.bytes()[r.field(c, "emptyWaiters"):], 5)
	binary.LittleEndian.PutUint32(r.bytes()[r.field(c, "notFull"):], 7)
	Format[protocol.Packet](unsafe.Pointer(&r.mem[0]), slots)
	specEq(t, "r after re-Format", r.u64(r.field(c, "r")), 0)
	specEq(t, "emptyWaiters after re-Format", r.u32(r.field(c, "emptyWaiters")), 0)
	specEq(t, "notFull after re-Format", r.u32(r.field(c, "notFull")), 0)
}

// TestSpecAttachValidation: Attach accepts only a ring whose magic, flag,
// stride, layout version and size match.
func TestSpecAttachValidation(t *testing.T) {
	c := loadSpecRing(t)
	for name, mod := range map[string]func(r *specRing){
		"version":         func(r *specRing) { r.put64(r.field(c, "version"), c.Ring.LayoutVer+1) },
		"stride":          func(r *specRing) { r.put64(r.field(c, "stride"), uint64(c.Ring.Slot.Stride)*2) },
		"size not pow2":   func(r *specRing) { r.put64(r.field(c, "size"), 6) },
		"size too small":  func(r *specRing) { r.put64(r.field(c, "size"), 1) },
		"magic":           func(r *specRing) { r.put64(r.field(c, "magic"), 1) },
		"init flag clear": func(r *specRing) { r.put64(r.field(c, "flag"), 0) },
	} {
		r := newSpecRing(t, 4)
		mod(r)
		start := time.Now()
		if got := Attach[protocol.Packet](unsafe.Pointer(&r.mem[0]), 20*time.Millisecond); got != nil {
			t.Errorf("%s: Attach accepted an incompatible ring", name)
		}
		if name != "magic" && name != "init flag clear" && time.Since(start) > 15*time.Millisecond {
			t.Errorf("%s: an incompatible layout must be rejected at once, took %v", name, time.Since(start))
		}
	}
}

// eachMode runs fn against a CAS handle and against an exclusive handle: the
// slot protocol and the memory image it leaves are the same in both modes.
func eachMode(t *testing.T, fn func(t *testing.T, exclusive bool)) {
	t.Helper()
	t.Run("cas", func(t *testing.T) { fn(t, false) })
	t.Run("exclusive", func(t *testing.T) { fn(t, true) })
}

// TestSpecSlotProtocol walks the slot sequence protocol: claim, publish
// (seq = pos+1), release (seq = pos+mask+1), full and empty detection.
func TestSpecSlotProtocol(t *testing.T) { eachMode(t, testSpecSlotProtocol) }

func testSpecSlotProtocol(t *testing.T, exclusive bool) {
	c := loadSpecRing(t)
	const slots = 4
	r := newSpecRing(t, slots)
	r.ring.SetExclusive(exclusive)
	ring := r.ring
	pk := func(i uint64) protocol.Packet { return protocol.NewPacket(protocol.OpStandardLinkCopy, i, i+1, i+2) }

	for i := uint64(0); i < slots; i++ {
		ring.Enqueue(pk(i))
		specEq(t, "published seq", r.seq(c, i), i+1)
		specEq(t, "w", r.u64(r.field(c, "w")), i+1)
		want := pk(i)
		if got := r.packet(c, i); string(got) != string(want[:]) {
			t.Fatalf("slot %d holds %x, want %x", i, got, want[:])
		}
	}
	// Full: a non-waiting producer fails and changes nothing.
	if err := ring.EnqueueZeroCopyCancel(&Cancel{NoWait: true}, func(uint64, *protocol.Packet) {}); err != ErrTimeout {
		t.Fatalf("enqueue on a full ring: %v, want ErrTimeout", err)
	}
	specEq(t, "w unchanged on full", r.u64(r.field(c, "w")), slots)
	for i := uint64(0); i < slots; i++ {
		got := ring.Dequeue()
		if got != pk(i) {
			t.Fatalf("dequeued %x, want %x (FIFO)", got[:], pk(i))
		}
		specEq(t, "released seq", r.seq(c, i), i+slots) // pos + mask + 1
		specEq(t, "r", r.u64(r.field(c, "r")), i+1)
	}
	// Empty: a non-waiting consumer fails and changes nothing.
	if err := ring.DequeueZeroCopyCancel(&Cancel{NoWait: true}, func(uint64, *protocol.Packet) {}); err != ErrTimeout {
		t.Fatalf("dequeue on an empty ring: %v, want ErrTimeout", err)
	}
	specEq(t, "r unchanged on empty", r.u64(r.field(c, "r")), slots)
	// Second lap reuses slot 0 with seq = slots + 1 after publish.
	ring.Enqueue(pk(9))
	specEq(t, "second-lap published seq", r.seq(c, 0), slots+1)
	specEq(t, "second-lap slot index", r.u64(r.field(c, "w"))&(slots-1), 1)
	_ = ring.Dequeue()
	specEq(t, "second-lap released seq", r.seq(c, 0), 2*slots)
}

// TestSpecPositionWraparound positions are 64-bit counters modulo 2^64; the
// full/empty tests are signed differences (int64(seq - pos)).
func TestSpecPositionWraparound(t *testing.T) { eachMode(t, testSpecPositionWraparound) }

func testSpecPositionWraparound(t *testing.T, exclusive bool) {
	c := loadSpecRing(t)
	const slots = 4
	r := newSpecRing(t, slots)
	r.ring.SetExclusive(exclusive)
	start := ^uint64(0) - 1 // 2^64 - 2: positions 2^64-2, 2^64-1, 0, 1 cross the wrap
	r.put64(r.field(c, "r"), start)
	r.put64(r.field(c, "w"), start)
	for k := uint64(0); k < slots; k++ {
		pos := start + k
		r.putSeq(c, pos&(slots-1), pos) // free for the producer owning pos
	}
	pk := func(i uint64) protocol.Packet { return protocol.NewPacket(protocol.OpStandardLinkCopy, i) }
	for k := uint64(0); k < slots; k++ {
		r.ring.Enqueue(pk(k))
		pos := start + k
		specEq(t, "published seq across wrap", r.seq(c, pos&(slots-1)), pos+1)
	}
	specEq(t, "w wrapped", r.u64(r.field(c, "w")), start+slots)
	if err := r.ring.EnqueueZeroCopyCancel(&Cancel{NoWait: true}, func(uint64, *protocol.Packet) {}); err != ErrTimeout {
		t.Fatalf("a full ring across the wrap accepted a producer: %v", err)
	}
	for k := uint64(0); k < slots; k++ {
		if got := r.ring.Dequeue(); got != pk(k) {
			t.Fatalf("dequeued %x, want %x", got[:], pk(k))
		}
		pos := start + k
		specEq(t, "released seq across wrap", r.seq(c, pos&(slots-1)), pos+slots)
	}
	specEq(t, "r wrapped", r.u64(r.field(c, "r")), start+slots)
}

// TestSpecWakeFields checks which words the waiting protocol touches: a
// producer that publishes while consumers are registered bumps notEmpty, a
// release bumps notFull only when fullWaiters != 0.
func TestSpecWakeFields(t *testing.T) { eachMode(t, testSpecWakeFields) }

func testSpecWakeFields(t *testing.T, exclusive bool) {
	c := loadSpecRing(t)
	r := newSpecRing(t, 4)
	r.ring.SetExclusive(exclusive)
	ne, ew, nf, fw := r.field(c, "notEmpty"), r.field(c, "emptyWaiters"), r.field(c, "notFull"), r.field(c, "fullWaiters")

	r.ring.Enqueue(protocol.NewPacket(protocol.OpStandardLinkCopy, 1))
	specEq(t, "notEmpty untouched without waiters", r.u32(ne), 0)
	_ = r.ring.Dequeue()
	specEq(t, "notFull untouched without waiters", r.u32(nf), 0)

	binary.LittleEndian.PutUint32(r.bytes()[ew:], 1) // a consumer process parked
	r.ring.Enqueue(protocol.NewPacket(protocol.OpStandardLinkCopy, 2))
	specEq(t, "notEmpty bumped by publish with a waiter", r.u32(ne), 1)
	specEq(t, "notFull untouched by publish", r.u32(nf), 0)
	binary.LittleEndian.PutUint32(r.bytes()[fw:], 1) // a producer process parked
	_ = r.ring.Dequeue()
	specEq(t, "notFull bumped by release with a waiter", r.u32(nf), 1)
	specEq(t, "waiter counts are not modified by wakers", r.u32(ew)+r.u32(fw), 2)
}
