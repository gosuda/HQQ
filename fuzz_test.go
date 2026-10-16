package hqq

import (
	"encoding/binary"
	"testing"
	"time"
	"unsafe"

	"gosuda.org/hqq/internal/protocol"
)

// FuzzConsumeAdvancedChunk feeds arbitrary frame sequences into the Advanced
// message assembler. It must never panic, never allocate more than
// MaxMessageSize for one message or MaxReassemblyBytes for all incomplete
// messages, and only return complete messages whose payload length equals
// the announced total size.
func FuzzConsumeAdvancedChunk(f *testing.F) {
	const bufferSize = 64
	frame := func(conn, msg, total, off, size, flags, aux uint64, fill byte) []byte {
		b := make([]byte, 7*8+1)
		for i, v := range []uint64{conn, msg, total, off, size, flags, aux} {
			binary.LittleEndian.PutUint64(b[i*8:], v)
		}
		b[56] = fill
		return b
	}
	f.Add(frame(0, 1, 10, 0, 10, AdvFlagBegin|AdvFlagEnd, 0, 'a'))
	f.Add(append(frame(0, 1, 100, 0, 64, AdvFlagBegin|AdvFlagRequest, 7, 'a'),
		frame(0, 1, 100, 64, 36, AdvFlagEnd|AdvFlagRequest, 0, 'b')...))
	f.Add(frame(0, 2, 0, 0, 0, AdvFlagBegin|AdvFlagEnd|AdvFlagResponse|AdvFlagError, 9, 0))
	f.Add(frame(3, 4, 50, 0, 0, AdvFlagAbort, 0, 0))
	f.Add(frame(0, 1, 1<<40, 0, 64, AdvFlagBegin, 0, 0))
	f.Add(append(append(frame(0, 1, 4000, 0, 64, AdvFlagBegin, 0, 'a'),
		frame(0, 2, 4000, 0, 64, AdvFlagBegin|AdvFlagResponse, 0, 'b')...),
		frame(0, 2, 4000, 64, 64, AdvFlagAbort|AdvFlagResponse, 0, 0)...))

	f.Fuzz(func(t *testing.T, data []byte) {
		l := newAdvancedLink(&StandardLink{bufferCount: 4}, AdvancedOptions{MaxMessageSize: 4096, MaxReassemblyBytes: 6000})
		buffer := make([]byte, bufferSize)
		const frameLen = 7*8 + 1
		for len(data) >= frameLen {
			var p protocol.Packet
			p.SetOp(protocol.OpConnCopy)
			for i := 0; i < 7; i++ {
				p.SetOperand(i, binary.LittleEndian.Uint64(data[i*8:]))
			}
			for i := range buffer {
				buffer[i] = data[56]
			}
			data = data[frameLen:]

			msg, hdr, outcome, err := l.consumeAdvancedChunk(&p, buffer)
			if err == nil && outcome == chunkComplete && hdr.isAbort() {
				t.Fatalf("complete message carries the abort flag: %+v", hdr)
			}
			if err != nil || outcome != chunkComplete {
				if msg != nil {
					t.Fatalf("message returned with outcome %v, err %v", outcome, err)
				}
				continue
			}
			if uint64(msg.Len()) != p.Operand(2) || uint64(len(msg.Bytes())) != p.Operand(2) {
				t.Fatalf("payload length %d != total size %d", msg.Len(), p.Operand(2))
			}
			if uint64(msg.Len()) > l.maxMessageSize {
				t.Fatalf("payload length %d exceeds MaxMessageSize", msg.Len())
			}
			msg.Release()
		}
		var held uint64
		for key, a := range l.assemblies {
			if a.size > l.maxMessageSize || uint64(a.msg.Len()) != a.size {
				t.Fatalf("assembly %+v holds %d bytes (size %d)", key, a.msg.Len(), a.size)
			}
			held += a.charge()
		}
		if held != l.assemblyBytes || held > l.maxReassemblyBytes {
			t.Fatalf("assemblies charge %d, tracked %d, limit %d", held, l.assemblyBytes, l.maxReassemblyBytes)
		}
		if len(l.rejected) != len(l.rejectedOrder) || len(l.rejected) > maxRejectedMessages {
			t.Fatalf("rejected set %d, order %d", len(l.rejected), len(l.rejectedOrder))
		}
	})
}

// FuzzOpenOverCorruptSuperblock opens a link over a superblock with arbitrary
// contents. Open must either fail cleanly or produce a working link; it must
// never panic or hang.
func FuzzOpenOverCorruptSuperblock(f *testing.F) {
	// The superblock fields by offset: magic 0, state 8, major 16, minor 24,
	// initToken 72, requiredFeatures 88, creatorToken 96.
	word := func(b []byte, off int, v uint64) []byte {
		binary.LittleEndian.PutUint64(b[off:], v)
		return b
	}
	v3 := func(state uint64) []byte {
		b := make([]byte, 128)
		word(b, 0, superblockMagic)
		word(b, 8, state)
		word(b, 16, superblockMajor)
		word(b, 24, superblockMinor)
		return b
	}
	f.Add(make([]byte, 128), uint64(0), uint64(0))
	f.Add(v3(superblockStateReady), uint64(1), uint64(5_000_000))
	f.Add(v3(superblockStateReady), uint64(0), uint64(0))
	// A v2 ("HQQ_STL2") segment and a segment that requires an unknown
	// layout feature.
	f.Add(word(v3(superblockStateReady), 0, 0x4851515f53544c32), uint64(1), uint64(5_000_000))
	f.Add(word(v3(superblockStateReady), 88, 1<<5), uint64(1), uint64(5_000_000))
	// Initializing segments: nothing recorded yet, a live-looking layout and
	// token, and one of another layout version.
	f.Add(word(make([]byte, 128), 8, superblockStateInitializing), uint64(0), uint64(0))
	f.Add(word(v3(superblockStateInitializing), 72, 0x1234), uint64(0), uint64(0))
	f.Add(word(v3(superblockStateInitializing), 16, 2), uint64(0), uint64(0))
	// Empty segments with leftover data in the first page.
	f.Add(word(make([]byte, 128), 96, 5), uint64(0), uint64(0))

	opts := LinkOptions{
		HeartbeatInterval: 5 * time.Millisecond,
		PeerTimeout:       20 * time.Millisecond,
		AttachTimeout:     20 * time.Millisecond,
		initStale:         5 * time.Millisecond, // bound takeover waits
	}
	f.Fuzz(func(t *testing.T, header []byte, token uint64, interval uint64) {
		const bufferCount, bufferSize = 4, 64
		mem := AlignedBuffer(SizeStandardLink(bufferCount, bufferSize))
		copy(mem[:128], header)
		sb := (*standardSuperblock)(unsafe.Pointer(&mem[0]))
		for i := range sb.endpoints {
			if sb.state == superblockStateEmpty {
				break // an empty segment must stay all zero to be claimable
			}
			sb.endpoints[i].token = token
			sb.endpoints[i].interval = interval % uint64(5*time.Millisecond)
			sb.endpoints[i].heartbeat = 1
		}

		link, err := NewStandardLink(mem, bufferCount, bufferSize, opts)
		if err != nil {
			return
		}
		defer link.Close()
		if link.Mode() == LinkModePrimary {
			peer, err := NewStandardLink(mem, bufferCount, bufferSize, opts)
			if err != nil {
				t.Fatalf("attach to freshly initialized segment: %v", err)
			}
			defer peer.Close()
			if _, err := link.Write([]byte("ok")); err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, bufferSize)
			if n, err := peer.Read(buf); err != nil || string(buf[:n]) != "ok" {
				t.Fatalf("Read = %q, %v", buf[:n], err)
			}
		}
	})
}
