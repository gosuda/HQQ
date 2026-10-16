package conformance

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Generate regenerates the golden vectors under outDir: manifest.json plus
// the binary images in images/<page>/, rings/<page>/ and, for the host page
// family only, sequences/<page>/. Everything is built in memory first, so a
// failed generation leaves outDir untouched. The output is deterministic: no
// clock and no unseeded randomness; live Go runs are normalized (see
// Manifest.Normalized).
//
// Before writing, the generator removes the directories and the manifest it
// owns (images/, rings/, sequences/<host page>/ and manifest.json), so stale
// files disappear. Other sequence families and nothing outside outDir are
// touched.
func Generate(outDir string) error {
	files, m, err := buildVectors()
	if err != nil {
		return err
	}
	manifest, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	manifest = append(manifest, '\n')

	host := strconv.Itoa(os.Getpagesize())
	for _, owned := range []string{"images", "rings", filepath.Join("sequences", host)} {
		if err := os.RemoveAll(filepath.Join(outDir, owned)); err != nil {
			return err
		}
	}
	if err := os.Remove(filepath.Join(outDir, "manifest.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	for _, p := range paths {
		full := filepath.Join(outDir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, files[p], 0o644); err != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(outDir, "manifest.json"), manifest, 0o644)
}

// buildVectors builds every vector file (relative slash path -> bytes) and
// the manifest in memory. It runs the Go implementation (host page family)
// and fails if the layout model disagrees with it.
func buildVectors() (map[string][]byte, *Manifest, error) {
	files := make(map[string][]byte)
	m := &Manifest{
		Schema:        ManifestSchema,
		Protocol:      "1.1",
		SegmentLayout: "HQQ_STL3",
		RingLayout:    RingVersion,
		PageFamilies:  slices.Clone(PageFamilies),
		Normalized:    normalizationRules(),
		Packets:       packetVectors(),
		Layouts:       layoutVectors(),
		Geometry:      geometryVectors(),
		Names:         nameVectors(),
		ErrorCodes:    errorCodeVectors(),
		Payloads:      payloadVectors(),
	}
	var err error
	if m.Superblocks, err = superblockVectors(files); err != nil {
		return nil, nil, err
	}
	if m.Rings, err = ringVectors(files); err != nil {
		return nil, nil, err
	}
	if m.Frames, err = frameVectors(); err != nil {
		return nil, nil, err
	}
	if err := goldenSequences(files); err != nil {
		return nil, nil, err
	}
	return files, m, nil
}

// PageFamilies are the page sizes for which superblock and ring images are
// generated. They do not depend on the host.
var PageFamilies = []int{4096, 16384}

// ManifestSchema is the schema version of manifest.json. It changes when a
// field is removed or changes meaning.
const ManifestSchema = 1

// Manifest is the root of manifest.json. Fields are written in struct order;
// the file is 2-space indented with a trailing newline. It does not depend on
// the host: the golden sequences, which need a Go run, are in
// sequences/<page>/sequence.json (GoldenSequence) instead.
//
// Binary files are referenced by the vectors that use them, as slash paths
// relative to the vectors directory.
type Manifest struct {
	// Schema is ManifestSchema.
	Schema int `json:"schema"`
	// Protocol is the Advanced protocol version of the frame vectors ("1.1").
	Protocol string `json:"protocol"`
	// SegmentLayout is the magic name of the segment layout ("HQQ_STL3").
	SegmentLayout string `json:"segment_layout"`
	// RingLayout is the ring layout version (2).
	RingLayout int `json:"ring_layout"`
	// PageFamilies lists the page sizes of superblock and ring images.
	PageFamilies []int `json:"page_families"`
	// Normalized lists the rules that replace the live words of Go-run
	// images, with the word each rule applies to. Model images are built
	// with the replacement values already in place.
	Normalized []Normalization `json:"normalized"`
	// Packets are the packet byte vectors (G-02): the nine vectors of
	// constants.json (Source "constants.json") and the extra frames
	// (Source "extra").
	Packets []PacketVector `json:"packets"`
	// Layouts are region-offset vectors (G-03): the twelve constants.json
	// geometries (Source "constants.json") and extra geometries.
	Layouts []LayoutVector `json:"layouts"`
	// Geometry are geometry validity vectors (G-04).
	Geometry []GeometryVector `json:"geometry"`
	// Names are shared-memory name vectors (G-05).
	Names []NameVector `json:"names"`
	// ErrorCodes maps wire error codes to canonical error kinds (G-11).
	ErrorCodes []ErrorCodeVector `json:"error_codes"`
	// Payloads are deterministic payload vectors (INTEROP_CLI section 5).
	Payloads []PayloadVector `json:"payloads"`
	// Superblocks are the superblock state cases (G-08), one entry per case
	// and page family.
	Superblocks []SuperblockCase `json:"superblocks"`
	// Rings are the ring image cases (G-07), one entry per case and page
	// family.
	Rings []RingCase `json:"rings"`
	// Frames are the rows of the Advanced frame matrix (G-10).
	Frames []FrameRow `json:"advanced_frames"`
}

// Normalization documents one normalization rule of Go-run images.
type Normalization struct {
	// Word is the superblock or endpoint word the rule applies to, named as
	// in SPEC 3.1 (for example "endpoints[0].token").
	Word string `json:"word"`
	// Rule is the replacement: an owner token becomes a fixed owner token
	// (neither 0 nor 1 is replaced); a nonzero pid or heartbeat becomes 0.
	Rule string `json:"rule"`
}

// normalizationRules lists the rules of normalizeSegment.
func normalizationRules() []Normalization {
	return []Normalization{
		{"endpoints[0].token", fmt.Sprintf("owner token -> %#x (0 and 1 kept)", normPrimaryToken)},
		{"endpoints[1].token", fmt.Sprintf("owner token -> %#x (0 and 1 kept)", normSecondaryToken)},
		{"endpoints[0].pid", "nonzero -> 0"},
		{"endpoints[1].pid", "nonzero -> 0"},
		{"endpoints[0].heartbeat", "nonzero -> 0"},
		{"endpoints[1].heartbeat", "nonzero -> 0"},
		{"initToken", fmt.Sprintf("nonzero -> %#x", normInitToken)},
		{"initHeartbeat", "nonzero -> 0"},
		{"creatorToken", fmt.Sprintf("nonzero -> %#x", normCreatorToken)},
	}
}

// Hex64 is a 64-bit word that JSON carries as a "0x..." string. Positions
// and words above 2^53 are written this way (as constants.json does), so
// that a reader with 53-bit numbers gets them exactly.
type Hex64 uint64

// MarshalJSON writes v as a lowercase "0x" hex string.
func (v Hex64) MarshalJSON() ([]byte, error) {
	return json.Marshal(fmt.Sprintf("%#x", uint64(v)))
}

// UnmarshalJSON reads a "0x..." string or a JSON number.
func (v *Hex64) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		var n uint64
		if err := json.Unmarshal(b, &n); err != nil {
			return fmt.Errorf("conformance: Hex64 %s: %w", b, err)
		}
		*v = Hex64(n)
		return nil
	}
	n, err := strconv.ParseUint(strings.TrimPrefix(s, "0x"), 16, 64)
	if err != nil {
		return fmt.Errorf("conformance: Hex64 %q: %w", s, err)
	}
	*v = Hex64(n)
	return nil
}

// PacketVector is one packet byte vector (G-02). Operands are the operand
// words 0..len-1; the words after them are zero. Hex is the 64-byte packet.
type PacketVector struct {
	// Name identifies the vector ("standard_copy", "conn_copy_abort", ...).
	Name string `json:"name"`
	// Source is "constants.json" for packet_vectors of the spec, or "extra".
	Source string `json:"source"`
	// Opcode is the value of word 0 (the low byte).
	Opcode uint8 `json:"opcode"`
	// Operands are operand words 0..len-1.
	Operands []uint64 `json:"operands"`
	// Hex is the 64-byte packet, little-endian words.
	Hex string `json:"hex"`
}

// LayoutVector is one region-offset vector (G-03). Offsets follow SPEC 2.1.
type LayoutVector struct {
	// Source is "constants.json" for segment.layout_vectors, or "extra".
	Source      string `json:"source"`
	PageSize    int    `json:"page_size"`
	BufferCount int    `json:"buffer_count"`
	BufferSize  int    `json:"buffer_size"`
	Ring0       int    `json:"ring0"`
	Buffers0    int    `json:"buffers0"`
	Ring1       int    `json:"ring1"`
	Buffers1    int    `json:"buffers1"`
	Total       int    `json:"total"`
}

// GeometryVector is one geometry validity vector (G-04).
type GeometryVector struct {
	BufferCount int  `json:"buffer_count"`
	BufferSize  int  `json:"buffer_size"`
	Valid       bool `json:"valid"`
	// Reason names the violated rule of SPEC 2.2 (empty when valid).
	Reason string `json:"reason"`
}

// NameVector is one name vector (G-05). Platform is "any" for the rules of
// SPEC 11.1, "darwin" for the macOS limit (30 bytes without the leading
// slash, 31 with it) or "windows" for the vectors of constants.json
// platform.windows.
type NameVector struct {
	// Name is the name as given to CreateStandardLink.
	Name string `json:"name"`
	// Platform is "any", "darwin" or "windows".
	Platform string `json:"platform"`
	// Valid is false when CreateStandardLink must return ErrInvalidName.
	Valid bool `json:"valid"`
	// Object is the kernel object name of a valid Windows name.
	Object string `json:"object,omitempty"`
	// Source is "constants.json" or "extra".
	Source string `json:"source"`
}

// ErrorCodeVector maps one wire error code to the canonical error kind of
// INTEROP_CLI section 4.1 (G-11). Unknown codes map to ErrProtocol.
type ErrorCodeVector struct {
	// Name is the code's name in constants.json, or "unknown".
	Name string `json:"name"`
	// Code is the wire value (OpError operand 1).
	Code uint64 `json:"code"`
	// ErrorKind is the canonical error_kind the code surfaces as.
	ErrorKind string `json:"error_kind"`
	// Source is "constants.json", "protocol_1_1" (PROTOCOL_1_1_DRAFT section
	// 7) or "extra".
	Source string `json:"source"`
}

// PayloadVector is one payload vector (INTEROP_CLI section 5). Payloads of up
// to 64 bytes carry Hex; larger ones carry the SHA-256 of the bytes and their
// Length.
type PayloadVector struct {
	Seed uint64 `json:"seed"`
	Seq  uint64 `json:"seq"`
	Size int    `json:"size"`
	// Hex is the payload as hex, for sizes up to 64 bytes.
	Hex string `json:"hex,omitempty"`
	// SHA256 is the hex digest of the payload, for larger sizes.
	SHA256 string `json:"sha256,omitempty"`
	// Length is the payload length, set with SHA256.
	Length int `json:"length,omitempty"`
}

// RoleOutcome is the expected result of one attach role on a superblock case.
type RoleOutcome struct {
	// Result is "primary" or "secondary" when the attach succeeds, else empty.
	Result string `json:"result"`
	// Error is the canonical error_kind when the attach fails, else empty.
	Error string `json:"error"`
	// Writes is what the attach writes to the segment: "none" (the bytes are
	// identical afterwards), "join" (only the secondary endpoint record
	// changes) or "init" (the superblock is re-initialized and Ready).
	Writes string `json:"writes"`
}

// SuperblockCase is one superblock state (G-08). File holds the image: the
// first page of the segment (the superblock page), or, when Full is set, the
// whole segment with formatted rings. Bytes of the segment past File are zero.
// Creator, Opener and InMemory are the expected outcomes of the attach paths:
// CreateStandardLink, OpenStandardLink and NewStandardLink on caller memory.
type SuperblockCase struct {
	// ID is the case identifier.
	ID string `json:"id"`
	// Title describes the state.
	Title string `json:"title"`
	// PageSize is the page family of the image (the file is PageSize bytes
	// long unless Full is set). Only images of the host family run on a host.
	PageSize int `json:"page_size"`
	// RecordedPageSize is the page size written in the superblock; it equals
	// PageSize except for the recorded-page case.
	RecordedPageSize int `json:"recorded_page_size"`
	// File is the image path relative to the vectors directory.
	File string `json:"file"`
	// FileBytes is the length of File.
	FileBytes int `json:"file_bytes"`
	// Full is true when File is a whole segment (rings formatted).
	Full bool `json:"full"`
	// SegmentBytes is the layout total of the image geometry (SPEC 2.1); a
	// named object is created with max(SegmentBytes, caller total) bytes.
	SegmentBytes int `json:"segment_bytes"`
	// BufferCount and BufferSize are the geometry recorded in the image.
	BufferCount int `json:"buffer_count"`
	BufferSize  int `json:"buffer_size"`
	// CallerBufferCount and CallerBufferSize are the geometry of the attach
	// calls (equal to the image geometry except for the mismatch cases).
	CallerBufferCount int `json:"caller_buffer_count"`
	CallerBufferSize  int `json:"caller_buffer_size"`
	// Normalized names the words of the segment that carry normalized values
	// (owner tokens, creator and init tokens, pids, heartbeats).
	Normalized []string `json:"normalized"`
	// Creator, Opener and InMemory are the expected outcomes per role.
	Creator  RoleOutcome `json:"creator"`
	Opener   RoleOutcome `json:"opener"`
	InMemory RoleOutcome `json:"inmemory"`
}

// RingWord is one expected result of a ring operation. Kind is "message"
// (Hex is the payload of a Read), "empty" (a Read deadline passed with nothing
// to read), "error" (Error is the canonical error kind) or "written" (a Write
// accepted N bytes).
type RingWord struct {
	Kind  string `json:"kind"`
	Hex   string `json:"hex,omitempty"`
	N     int    `json:"n,omitempty"`
	Error string `json:"error,omitempty"`
}

// RingCase is one ring image (G-07). The image is a whole segment of the page
// family with a live Primary (interval 0) and a free Secondary slot. The test
// joins it as Secondary on caller memory and runs Dequeue (successive Reads,
// ring 0, the Primary-to-Secondary direction) and Write (successive Writes,
// ring 1). The payload of Write i is Payload(Seed, i, BufferSize).
type RingCase struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// PageSize is the page family of the image.
	PageSize int `json:"page_size"`
	// File is the image path relative to the vectors directory.
	File        string `json:"file"`
	BufferCount int    `json:"buffer_count"`
	BufferSize  int    `json:"buffer_size"`
	// Seed is the payload seed of the messages of ring 0 (Payload(Seed, pos,
	// BufferSize) is the message at position pos).
	Seed uint64 `json:"seed"`
	// Ring0 and Ring1 describe the cursors and the published or claimed
	// positions of each direction.
	Ring0 RingState `json:"ring0"`
	Ring1 RingState `json:"ring1"`
	// Dequeue lists the expected results of successive Secondary Reads.
	Dequeue []RingWord `json:"dequeue"`
	// Write lists the expected results of successive Secondary Writes.
	Write []RingWord `json:"write,omitempty"`
}

// RingState describes one direction of a ring case. Positions are modulo
// 2^64.
type RingState struct {
	// R is the consumer position and W the producer position.
	R Hex64 `json:"r"`
	W Hex64 `json:"w"`
	// Published lists the positions whose slot sequence is pos+1.
	Published []Hex64 `json:"published"`
	// Claimed lists positions claimed (W advanced) but not published.
	Claimed []Hex64 `json:"claimed,omitempty"`
}

// FrameRow is one row of the Advanced frame matrix (G-10). The test builds a
// fresh in-memory image per row: ring 0 holds Frames in order (the first one
// is OpProtoNegotiate), then the test appends a sentinel when Sentinel is set.
// A Secondary joins in memory and negotiates by consuming the first frame; the
// OpProtoAck it publishes on ring 1 is checked. Then the row's target runs.
// LateFrames, when present, are published into ring 0 after the first request
// handler of a serve row has started (a cancel of a running request).
type FrameRow struct {
	// ID is the row identifier ("G10-01").
	ID string `json:"id"`
	// Title describes the case and the rule it exercises.
	Title string `json:"title"`
	// Target is the API that receives the frames: "recv" (AdvancedLink.Recv),
	// "call" (AdvancedLink.Call of message 1, the first ID of the link, so
	// responses match it) or "serve" (AdvancedLink.Serve with a handler that
	// records requests).
	Target string `json:"target"`
	// MaxReassemblyBytes overrides AdvancedOptions.MaxReassemblyBytes (0 =
	// default).
	MaxReassemblyBytes uint64 `json:"max_reassembly_bytes,omitempty"`
	// Frames are the frames of ring 0; the first is OpProtoNegotiate.
	Frames []Frame `json:"frames"`
	// LateFrames are published after the first request has been handed to
	// the handler (serve rows only).
	LateFrames []Frame `json:"late_frames,omitempty"`
	// Effect is what the frames cause: "deliver" (the Deliver entries arrive
	// in order), "drop" (nothing is delivered; the path survives), "drain"
	// (a refused message: its chunks are discarded; the path survives) or
	// "fail" (the target returns Error).
	Effect string `json:"effect"`
	// FailScope is "path" when the receive path fails (later calls fail with
	// the same error and no sentinel follows) or "call" when only the Call
	// fails and the path survives.
	FailScope string `json:"fail_scope,omitempty"`
	// Deliver lists the expected deliveries, in order, for effect "deliver".
	Deliver []Delivery `json:"deliver,omitempty"`
	// Error is the canonical error kind of a "fail" row ("ResponseError" for
	// an error response, whose status is the aux of the frame).
	Error string `json:"error,omitempty"`
	// Sentinel is true when a sentinel message follows the frames and must
	// be the next delivery: it proves that the path survived the row.
	Sentinel bool `json:"sentinel"`
	// AckFeatures is the feature word of the OpProtoAck the secondary sends
	// (the negotiated set: the requested features it supports).
	AckFeatures uint64 `json:"ack_features"`
}

// Frame is one packet of a frame row: opcode, operand words and, for frames
// that carry data, the payload stored in the slot buffer.
type Frame struct {
	// Op is the opcode name ("OpProtoNegotiate", "OpConnCopy", "OpCancel",
	// ...), or its hex value for an unknown opcode.
	Op string `json:"op"`
	// Operands are operand words 0..len-1; the others are zero.
	Operands []uint64 `json:"operands"`
	// Payload is the chunk payload (hex), empty for frames without data.
	Payload string `json:"payload,omitempty"`
	// Packet is the 64-byte packet (hex), encoded by internal/protocol.
	Packet string `json:"packet"`
}

// Delivery is one expected delivery of a frame row.
type Delivery struct {
	// Kind is "data" (Recv), "request" (a Serve handler call) or "response"
	// (the result of a Call).
	Kind string `json:"kind"`
	// Payload is the delivered payload (hex). For a request it excludes the
	// metadata block.
	Payload string `json:"payload"`
	// Method is the request's method (aux of its first frame), for requests.
	Method uint64 `json:"method,omitempty"`
	// Caller is Request.Caller (zero when absent).
	Caller uint64 `json:"caller,omitempty"`
	// Trace is Request.Trace as hex ("" when absent).
	Trace string `json:"trace,omitempty"`
}

// GoldenSequence is sequences/<page>/sequence.json (G-06): the snapshots of a
// real Go run on caller memory with heartbeats disabled, normalized. Steps
// run in order; each snapshot is the whole segment.
type GoldenSequence struct {
	// PageSize is the host page size the Go run used.
	PageSize int `json:"page_size"`
	// BufferCount, BufferSize and LayoutSize are the geometry of the run.
	BufferCount int `json:"buffer_count"`
	BufferSize  int `json:"buffer_size"`
	LayoutSize  int `json:"layout_size"`
	// Seed is the payload seed of the one message of the run.
	Seed uint64 `json:"seed"`
	// Steps are the snapshots in order.
	Steps []SequenceStep `json:"steps"`
}

// SequenceStep is one snapshot of a golden sequence.
type SequenceStep struct {
	// Name is the step name ("1-primary-ready", ...). File is the snapshot.
	Name string `json:"name"`
	// Action describes what the run did before the snapshot.
	Action string `json:"action"`
	// File is the snapshot path relative to the vectors directory.
	File string `json:"file"`
	// Epoch is the superblock epoch of the snapshot.
	Epoch uint64 `json:"epoch"`
	// Normalized names the words of the snapshot that held live values and
	// were normalized: the variable words of this step.
	Normalized []string `json:"normalized"`
}

// putFile stores data under the slash path p, refusing a duplicate.
func putFile(files map[string][]byte, p string, data []byte) error {
	if _, dup := files[p]; dup {
		return fmt.Errorf("conformance: duplicate vector file %s", p)
	}
	files[p] = data
	return nil
}

// familyPath returns the slash path of a file of a page family directory.
func familyPath(dir string, page int, name string) string {
	return path.Join(dir, strconv.Itoa(page), name)
}
