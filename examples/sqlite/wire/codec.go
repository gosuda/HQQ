package wire

import (
	"encoding/binary"
	"math"
)

// Value is one SQL value. Bytes holds the payload of TEXT and BLOB values and
// aliases the buffer it was decoded from (see Decoder.Value).
type Value struct {
	Type  ValueType
	Int   int64
	Float float64
	Bytes []byte
}

// Integer returns an INTEGER value.
func Integer(v int64) Value { return Value{Type: TypeInteger, Int: v} }

// Float returns a FLOAT value.
func Float(v float64) Value { return Value{Type: TypeFloat, Float: v} }

// Text returns a TEXT value.
func Text(s string) Value { return Value{Type: TypeText, Bytes: []byte(s)} }

// Blob returns a BLOB value. The slice is not copied.
func Blob(b []byte) Value { return Value{Type: TypeBlob, Bytes: b} }

// Null returns a NULL value.
func Null() Value { return Value{Type: TypeNull} }

// Param is one bound parameter of a request (PROTOCOL.md section 3).
type Param struct {
	Ordinal uint32 // 1-based position in the argument list
	Name    string // parameter name without its prefix; empty for positional
	Value   Value
}

// Column is the metadata of one result column.
type Column struct {
	Name     string
	Decltype string // empty when SQLite has no declared type
}

// Encoder appends protocol encodings to a byte slice. The zero value is ready
// to use.
type Encoder struct {
	b []byte
}

// NewEncoder returns an Encoder whose buffer has room for capacity bytes.
func NewEncoder(capacity int) *Encoder {
	return &Encoder{b: make([]byte, 0, capacity)}
}

// Bytes returns the encoded bytes. They alias the encoder's buffer until the
// next write.
func (e *Encoder) Bytes() []byte { return e.b }

// Len returns the number of encoded bytes.
func (e *Encoder) Len() int { return len(e.b) }

// Reset empties the encoder and keeps its buffer.
func (e *Encoder) Reset() { e.b = e.b[:0] }

// U8 appends a u8.
func (e *Encoder) U8(v uint8) { e.b = append(e.b, v) }

// U16 appends a u16.
func (e *Encoder) U16(v uint16) { e.b = binary.LittleEndian.AppendUint16(e.b, v) }

// U32 appends a u32.
func (e *Encoder) U32(v uint32) { e.b = binary.LittleEndian.AppendUint32(e.b, v) }

// U64 appends a u64.
func (e *Encoder) U64(v uint64) { e.b = binary.LittleEndian.AppendUint64(e.b, v) }

// I64 appends an i64.
func (e *Encoder) I64(v int64) { e.U64(uint64(v)) }

// F64 appends an f64 (IEEE 754 binary64 bit pattern).
func (e *Encoder) F64(v float64) { e.U64(math.Float64bits(v)) }

// String appends a string: u32 length, then the bytes.
func (e *Encoder) String(s string) {
	e.U32(uint32(len(s)))
	e.b = append(e.b, s...)
}

// Blob appends a byte string: u32 length, then the bytes. It has the same
// layout as String.
func (e *Encoder) Blob(b []byte) {
	e.U32(uint32(len(b)))
	e.b = append(e.b, b...)
}

// Value appends a Value (PROTOCOL.md section 3). It panics on an invalid
// ValueType, which is a programming error.
func (e *Encoder) Value(v Value) {
	switch v.Type {
	case TypeInteger:
		e.U8(uint8(TypeInteger))
		e.I64(v.Int)
	case TypeFloat:
		e.U8(uint8(TypeFloat))
		e.F64(v.Float)
	case TypeText:
		e.U8(uint8(TypeText))
		e.Blob(v.Bytes)
	case TypeBlob:
		e.U8(uint8(TypeBlob))
		e.Blob(v.Bytes)
	case TypeNull:
		e.U8(uint8(TypeNull))
	default:
		panic("wire: invalid value type " + v.Type.String())
	}
}

// Params appends a Params block.
func (e *Encoder) Params(ps []Param) {
	e.U32(uint32(len(ps)))
	for _, p := range ps {
		e.U32(p.Ordinal)
		e.String(p.Name)
		e.Value(p.Value)
	}
}

// Columns appends a Columns block.
func (e *Encoder) Columns(cols []Column) {
	e.U32(uint32(len(cols)))
	for _, c := range cols {
		e.String(c.Name)
		e.String(c.Decltype)
	}
}

// BatchStart appends the placeholder of a batch row count and returns its
// position. Call BatchEnd with the same position when the rows are written.
func (e *Encoder) BatchStart() int {
	pos := len(e.b)
	e.U32(0)
	return pos
}

// BatchEnd stores the row count of a batch started at pos.
func (e *Encoder) BatchEnd(pos int, nrows uint32) {
	binary.LittleEndian.PutUint32(e.b[pos:], nrows)
}

// PatchU8 overwrites the u8 at pos, which must already be encoded. It is used
// for header fields that are known only after the batch is written.
func (e *Encoder) PatchU8(pos int, v uint8) { e.b[pos] = v }

// PatchU32 overwrites the u32 at pos, which must already be encoded.
func (e *Encoder) PatchU32(pos int, v uint32) { binary.LittleEndian.PutUint32(e.b[pos:], v) }

// Decoder reads protocol encodings from a byte slice. Errors are sticky: after
// the first failure every read returns a zero value and Err reports the
// failure. Byte and string results alias the input unless documented
// otherwise.
type Decoder struct {
	b   []byte
	err error
}

// NewDecoder returns a Decoder over b.
func NewDecoder(b []byte) *Decoder { return &Decoder{b: b} }

// Err returns the first error, or nil.
func (d *Decoder) Err() error { return d.err }

// fail records a BAD_REQUEST error unless one is already recorded.
func (d *Decoder) fail(format string, args ...any) {
	d.failCode(ProtoBadRequest, format, args...)
}

// failCode records a protocol error unless one is already recorded.
func (d *Decoder) failCode(code ProtoCode, format string, args ...any) {
	if d.err == nil {
		d.err = Errorf(code, format, args...)
		d.b = nil
	}
}

// take returns the next n bytes, or nil after recording an error.
func (d *Decoder) take(n int) []byte {
	if d.err != nil {
		return nil
	}
	if n < 0 || n > len(d.b) {
		d.fail("truncated message: need %d bytes, have %d", n, len(d.b))
		return nil
	}
	out := d.b[:n:n]
	d.b = d.b[n:]
	return out
}

// U8 reads a u8.
func (d *Decoder) U8() uint8 {
	b := d.take(1)
	if b == nil {
		return 0
	}
	return b[0]
}

// U16 reads a u16.
func (d *Decoder) U16() uint16 {
	b := d.take(2)
	if b == nil {
		return 0
	}
	return binary.LittleEndian.Uint16(b)
}

// U32 reads a u32.
func (d *Decoder) U32() uint32 {
	b := d.take(4)
	if b == nil {
		return 0
	}
	return binary.LittleEndian.Uint32(b)
}

// U64 reads a u64.
func (d *Decoder) U64() uint64 {
	b := d.take(8)
	if b == nil {
		return 0
	}
	return binary.LittleEndian.Uint64(b)
}

// I64 reads an i64.
func (d *Decoder) I64() int64 { return int64(d.U64()) }

// F64 reads an f64.
func (d *Decoder) F64() float64 { return math.Float64frombits(d.U64()) }

// Bytes reads a length-prefixed byte string. The result aliases the input.
func (d *Decoder) Bytes() []byte {
	n := d.U32()
	if d.err != nil {
		return nil
	}
	if uint64(n) > uint64(len(d.b)) {
		d.fail("truncated message: string of %d bytes, have %d", n, len(d.b))
		return nil
	}
	return d.take(int(n))
}

// Name reads a length-prefixed name of at most MaxNameBytes and copies it.
func (d *Decoder) Name() string {
	b := d.Bytes()
	if len(b) > MaxNameBytes {
		d.failCode(ProtoLimit, "name of %d bytes exceeds %d", len(b), MaxNameBytes)
		return ""
	}
	return string(b)
}

// Value reads a Value. TEXT and BLOB payloads alias the input.
func (d *Decoder) Value() Value {
	tag := ValueType(d.U8())
	if d.err != nil {
		return Value{}
	}
	switch tag {
	case TypeInteger:
		return Value{Type: TypeInteger, Int: d.I64()}
	case TypeFloat:
		return Value{Type: TypeFloat, Float: d.F64()}
	case TypeText, TypeBlob:
		return Value{Type: tag, Bytes: d.Bytes()}
	case TypeNull:
		return Value{Type: TypeNull}
	}
	d.fail("invalid value tag %d", uint8(tag))
	return Value{}
}

// Params reads a Params block. Names are copied; values alias the input.
func (d *Decoder) Params() []Param {
	n := d.U32()
	if d.err != nil {
		return nil
	}
	if n > MaxParams {
		d.failCode(ProtoLimit, "%d parameters exceed %d", n, MaxParams)
		return nil
	}
	ps := make([]Param, 0, min(int(n), len(d.b)/5+1))
	seen := make(map[uint32]bool, n)
	for range n {
		var p Param
		p.Ordinal = d.U32()
		p.Name = d.Name()
		p.Value = d.Value()
		if d.err != nil {
			return nil
		}
		if p.Ordinal < 1 || p.Ordinal > MaxParams {
			d.fail("parameter ordinal %d out of range", p.Ordinal)
			return nil
		}
		if seen[p.Ordinal] {
			d.fail("duplicate parameter ordinal %d", p.Ordinal)
			return nil
		}
		seen[p.Ordinal] = true
		ps = append(ps, p)
	}
	return ps
}

// Columns reads a Columns block and copies its strings.
func (d *Decoder) Columns() []Column {
	n := d.U32()
	if d.err != nil {
		return nil
	}
	if n > MaxColumns {
		d.failCode(ProtoLimit, "%d columns exceed %d", n, MaxColumns)
		return nil
	}
	cols := make([]Column, 0, min(int(n), len(d.b)/8+1))
	for range n {
		name := d.Name()
		decl := d.Name()
		if d.err != nil {
			return nil
		}
		cols = append(cols, Column{Name: name, Decltype: decl})
	}
	return cols
}

// Row reads len(dst) values into dst, one row of a batch.
func (d *Decoder) Row(dst []Value) {
	for i := range dst {
		dst[i] = d.Value()
		if d.err != nil {
			return
		}
	}
}

// Finish returns the sticky error, or a BAD_REQUEST error when unread bytes
// remain.
func (d *Decoder) Finish() error {
	if d.err != nil {
		return d.err
	}
	if len(d.b) != 0 {
		return Errorf(ProtoBadRequest, "%d trailing bytes", len(d.b))
	}
	return nil
}
