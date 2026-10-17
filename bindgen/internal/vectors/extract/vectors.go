// Package extract reads the golden vectors out of spec/toolchain/BINDGEN.md (10.1): the fenced blocks
// `hex vector=NAME`, `text canon=NAME` and `text digest=NAME`. It renders them as bindgen/testdata/vectors.json
// and as the C header c/tests/generated/rpc_vectors.h.
package extract

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Repository-relative paths of the document, the extracted vectors and the generated C header.
const (
	DocRel     = "spec/toolchain/BINDGEN.md"
	JSONRel    = "bindgen/testdata/vectors.json"
	CHeaderRel = "c/tests/generated/rpc_vectors.h"
)

// Wire constants of spec/toolchain/BINDGEN.md section 4.2 that the sanity checks need.
const (
	frameHeader = 16
	kindCont    = 0x03
)

// Frame directions of the JSON file.
const (
	dirC2S = "c2s"
	dirS2C = "s2c"
)

// Frame is one golden frame of section 4.14.
type Frame struct {
	Name string `json:"name"`
	ID   uint32 `json:"id"`
	Dir  string `json:"dir"`
	Hex  string `json:"hex"`
}

// Geometry is the link geometry the frames of a group were built with.
type Geometry struct {
	BufferSize  int `json:"buffer_size"`
	BufferCount int `json:"buffer_count"`
}

// Service is a worked IDL example (sections 2.7 and 2.8) with its frames.
type Service struct {
	IDL       string    `json:"idl"`
	Canonical string    `json:"canonical"`
	Digest    string    `json:"digest"`
	Geometry  *Geometry `json:"geometry,omitempty"`
	Frames    []Frame   `json:"frames,omitempty"`
}

// ChunkGroup is a frame group that carries its own buffer size (the oversize pair).
type ChunkGroup struct {
	BufferSize int     `json:"buffer_size"`
	Frames     []Frame `json:"frames"`
}

// WrapGroup is the request id wrap pair.
type WrapGroup struct {
	Frames []Frame `json:"frames"`
}

// File is the content of bindgen/testdata/vectors.json.
type File struct {
	Microlib       Service    `json:"microlib"`
	SQLiteLowlevel Service    `json:"sqlite_lowlevel"`
	Chunking       ChunkGroup `json:"chunking"`
	Wrap           WrapGroup  `json:"wrap"`
}

// Default geometry of section 4.14 ("S = 16384 and B = 64 unless stated").
const (
	defaultBufferSize  = 16384
	defaultBufferCount = 64
	chunkBufferSize    = 128
)

var serviceIDL = map[string]string{
	"microlib":        "microlib/microlib.idl",
	"sqlite_lowlevel": "sqlite_lowlevel.idl",
}

// block is one fenced block of the document with a recognized info string.
type block struct {
	kind string // "vector", "canon" or "digest"
	name string
	body string
	line int
}

// scan returns the fenced blocks whose info string is `hex vector=NAME`, `text canon=NAME` or `text digest=NAME`.
func scan(doc []byte) ([]block, error) {
	var blocks []block
	lines := strings.Split(string(doc), "\n")
	for i := 0; i < len(lines); i++ {
		info, ok := strings.CutPrefix(lines[i], "```")
		if !ok || info == "" {
			continue
		}
		start := i
		var body []string
		closed := false
		for i++; i < len(lines); i++ {
			if strings.HasPrefix(lines[i], "```") {
				closed = true
				break
			}
			body = append(body, lines[i])
		}
		if !closed {
			return nil, fmt.Errorf("line %d: unterminated fenced block", start+1)
		}
		lang, label, found := strings.Cut(info, " ")
		if !found {
			continue
		}
		key, name, found := strings.Cut(label, "=")
		if !found {
			continue
		}
		var kind string
		switch {
		case lang == "hex" && key == "vector":
			kind = "vector"
		case lang == "text" && key == "canon":
			kind = "canon"
		case lang == "text" && key == "digest":
			kind = "digest"
		default:
			continue
		}
		if name == "" || strings.ContainsAny(name, " \t") {
			return nil, fmt.Errorf("line %d: bad block name %q", start+1, name)
		}
		blocks = append(blocks, block{kind: kind, name: name, body: strings.Join(body, "\n"), line: start + 1})
	}
	return blocks, nil
}

// Extract builds the vector file from the text of BINDGEN.md and validates it: block names are unique, each
// digest block is the SHA-256 of the canonical block of the same name, every frame is well formed for its
// geometry, and the digest inside the HELLO frames is the microlib digest.
func Extract(doc []byte) (*File, error) {
	blocks, err := scan(doc)
	if err != nil {
		return nil, err
	}
	canon := map[string]string{}
	digest := map[string]string{}
	seen := map[string]bool{}
	var frames []Frame
	for _, b := range blocks {
		id := b.kind + "=" + b.name
		if seen[id] {
			return nil, fmt.Errorf("line %d: duplicate block %s", b.line, id)
		}
		seen[id] = true
		switch b.kind {
		case "canon":
			if !strings.HasSuffix(b.body, "\n") && b.body != "" {
				b.body += "\n"
			}
			canon[b.name] = b.body
		case "digest":
			digest[b.name] = strings.TrimSpace(b.body)
		case "vector":
			f, err := frameOf(b)
			if err != nil {
				return nil, err
			}
			frames = append(frames, f)
		}
	}

	out := &File{}
	services := map[string]*Service{"microlib": &out.Microlib, "sqlite_lowlevel": &out.SQLiteLowlevel}
	for name, svc := range services {
		c, ok := canon[name]
		if !ok {
			return nil, fmt.Errorf("missing block text canon=%s", name)
		}
		d, ok := digest[name]
		if !ok {
			return nil, fmt.Errorf("missing block text digest=%s", name)
		}
		sum := sha256.Sum256([]byte(c))
		if got := hex.EncodeToString(sum[:]); got != d {
			return nil, fmt.Errorf("digest=%s is %s but the canonical text hashes to %s", name, d, got)
		}
		svc.IDL = serviceIDL[name]
		svc.Canonical = c
		svc.Digest = d
	}
	for name := range canon {
		if _, ok := services[name]; !ok {
			return nil, fmt.Errorf("unknown canon block %q", name)
		}
	}
	for name := range digest {
		if _, ok := services[name]; !ok {
			return nil, fmt.Errorf("unknown digest block %q", name)
		}
	}

	out.Microlib.Geometry = &Geometry{BufferSize: defaultBufferSize, BufferCount: defaultBufferCount}
	out.Chunking.BufferSize = chunkBufferSize
	for _, f := range frames {
		switch {
		case strings.HasPrefix(f.Name, "C_"):
			out.Chunking.Frames = append(out.Chunking.Frames, f)
		case strings.HasPrefix(f.Name, "WRAP_"):
			out.Wrap.Frames = append(out.Wrap.Frames, f)
		default:
			out.Microlib.Frames = append(out.Microlib.Frames, f)
		}
	}
	if err := out.validate(); err != nil {
		return nil, err
	}
	return out, nil
}

// frameOf decodes a vector block. The hex lines are concatenated; the direction comes from the name and the
// id from the frame header.
func frameOf(b block) (Frame, error) {
	var sb strings.Builder
	for _, l := range strings.Split(b.body, "\n") {
		sb.WriteString(strings.TrimSpace(l))
	}
	h := strings.ToLower(sb.String())
	raw, err := hex.DecodeString(h)
	if err != nil {
		return Frame{}, fmt.Errorf("line %d: vector %s: %w", b.line, b.name, err)
	}
	if len(raw) < frameHeader {
		return Frame{}, fmt.Errorf("line %d: vector %s: %d bytes, shorter than the header", b.line, b.name, len(raw))
	}
	dir, err := dirOf(b.name)
	if err != nil {
		return Frame{}, fmt.Errorf("line %d: %w", b.line, err)
	}
	return Frame{Name: b.name, ID: binary.LittleEndian.Uint32(raw[4:8]), Dir: dir, Hex: h}, nil
}

// dirOf derives the direction from the vector name: a request, a HELLO or a CLOSE goes client to server.
func dirOf(name string) (string, error) {
	switch {
	case name == "HELLO", name == "CLOSE", strings.HasSuffix(name, "_REQ"), strings.Contains(name, "_REQ_"):
		return dirC2S, nil
	case name == "HELLO_REPLY", name == "HELLO_REPLY_DIGEST", name == "CLOSE_ACK",
		strings.HasSuffix(name, "_RESP"), strings.Contains(name, "_RESP_"):
		return dirS2C, nil
	}
	return "", fmt.Errorf("cannot derive the direction of vector %q", name)
}

// validate checks the frames against their geometry (4.4 rule 4 and 4.8) and the HELLO digests against 2.7.
func (f *File) validate() error {
	groups := []struct {
		name   string
		size   int
		frames []Frame
	}{
		{"microlib", f.Microlib.Geometry.BufferSize, f.Microlib.Frames},
		{"chunking", f.Chunking.BufferSize, f.Chunking.Frames},
		{"wrap", defaultBufferSize, f.Wrap.Frames},
	}
	for _, g := range groups {
		if len(g.frames) == 0 {
			return fmt.Errorf("group %s has no frames", g.name)
		}
		chunk := g.size - frameHeader
		names := map[string]bool{}
		for _, fr := range g.frames {
			if names[fr.Name] {
				return fmt.Errorf("duplicate vector %s", fr.Name)
			}
			names[fr.Name] = true
			raw, _ := hex.DecodeString(fr.Hex)
			if err := checkFrame(raw, chunk); err != nil {
				return fmt.Errorf("vector %s: %w", fr.Name, err)
			}
		}
	}
	want, err := hex.DecodeString(f.Microlib.Digest)
	if err != nil || len(want) != sha256.Size {
		return errors.New("microlib digest is not 32 bytes of hex")
	}
	for _, fr := range f.Microlib.Frames {
		switch fr.Name {
		case "HELLO", "HELLO_REPLY", "HELLO_REPLY_DIGEST":
			raw, _ := hex.DecodeString(fr.Hex)
			if len(raw) != frameHeader+64 || !bytes.Equal(raw[len(raw)-sha256.Size:], want) {
				return fmt.Errorf("vector %s does not end with the microlib digest", fr.Name)
			}
		}
	}
	return nil
}

// checkFrame applies the length rules of 4.4 and 4.8 to one frame.
func checkFrame(raw []byte, chunk int) error {
	kind := raw[0]
	total := int64(binary.LittleEndian.Uint32(raw[8:12]))
	aux := int64(binary.LittleEndian.Uint32(raw[12:16]))
	body := int64(len(raw) - frameHeader)
	switch kind {
	case kindCont:
		if want := min(total-aux, int64(chunk)); body != want {
			return fmt.Errorf("CONT carries %d body bytes, want %d", body, want)
		}
	default:
		if want := min(total, int64(chunk)); body != want {
			return fmt.Errorf("kind %#x carries %d body bytes, want %d", kind, body, want)
		}
	}
	return nil
}

// Marshal renders the file as the committed JSON: two-space indent and a final newline.
func (f *File) Marshal() ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(f); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Load parses a committed vectors.json.
func Load(data []byte) (*File, error) {
	var f File
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, err
	}
	return &f, nil
}
