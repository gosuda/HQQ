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
		l := &AdvancedLink{
			assemblies:         make(map[advancedMessageKey]*advancedAssembly),
			maxMessageSize:     4096,
			maxReassemblyBytes: 6000,
		}
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

			msg, outcome, err := l.consumeAdvancedChunk(&p, buffer)
			if err == nil && outcome == chunkComplete && msg.IsAbort() {
				t.Fatalf("complete message carries the abort flag: %+v", msg)
			}
			if err != nil || outcome != chunkComplete {
				continue
			}
			if uint64(len(msg.Payload)) != p.Operand(2) {
				t.Fatalf("payload length %d != total size %d", len(msg.Payload), p.Operand(2))
			}
			if uint64(len(msg.Payload)) > l.maxMessageSize {
				t.Fatalf("payload length %d exceeds MaxMessageSize", len(msg.Payload))
			}
		}
		var held uint64
		for key, a := range l.assemblies {
			if uint64(len(a.data)) > l.maxMessageSize {
				t.Fatalf("assembly %+v holds %d bytes", key, len(a.data))
			}
			held += a.charge
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
	f.Add(make([]byte, 128), uint64(0), uint64(0))
	ready := make([]byte, 128)
	binary.LittleEndian.PutUint64(ready[0:], superblockMagic)
	binary.LittleEndian.PutUint64(ready[8:], superblockStateReady)
	f.Add(ready, uint64(1), uint64(5_000_000))
	initializing := make([]byte, 128)
	binary.LittleEndian.PutUint64(initializing[8:], superblockStateInitializing)
	f.Add(initializing, uint64(0), uint64(0))

	opts := LinkOptions{
		HeartbeatInterval: 5 * time.Millisecond,
		PeerTimeout:       20 * time.Millisecond,
		AttachTimeout:     20 * time.Millisecond,
	}
	f.Fuzz(func(t *testing.T, header []byte, token uint64, interval uint64) {
		const bufferCount, bufferSize = 4, 64
		mem := AlignedBuffer(SizeStandardLink(bufferCount, bufferSize))
		copy(mem[:128], header)
		sb := (*standardSuperblock)(unsafe.Pointer(&mem[0]))
		sb.initHeartbeat = uint64(time.Now().UnixNano()) // bound takeover waits
		for i := range sb.endpoints {
			sb.endpoints[i].token = token
			sb.endpoints[i].interval = interval % uint64(5*time.Millisecond)
			sb.endpoints[i].heartbeat = 1
		}

		link, err := OpenStandardLinkMemory(mem, bufferCount, bufferSize, opts)
		if err != nil {
			return
		}
		defer link.Close()
		if link.Mode() == LinkModePrimary {
			peer, err := OpenStandardLinkMemory(mem, bufferCount, bufferSize, opts)
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
