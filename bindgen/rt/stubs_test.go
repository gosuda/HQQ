package rt

import "encoding/binary"

// The stubs below are hand-written copies of the code the Go emitter generates
// for microlib.idl (BINDGEN.md 9.4). They use only the exported API of rt, so
// the tests exercise the surface the emitter compiles against.

var hqqLE = binary.LittleEndian

type counterH struct{ Handle }

type stubClient struct{ c *Conn }

type addFuture struct {
	c    *stubClient
	call *Call
	id   uint32
}

func (c *stubClient) Add(a, b int32) (int32, error) {
	f, err := c.AddAsync(a, b)
	if err != nil {
		return 0, err
	}
	return f.Wait()
}

func (c *stubClient) AddAsync(a, b int32) (addFuture, error) {
	e, err := c.c.BeginRequest(opAdd, 8, 4)
	if err != nil {
		return addFuture{}, err
	}
	body := e.Body()
	hqqLE.PutUint32(body[0:], uint32(a))
	hqqLE.PutUint32(body[4:], uint32(b))
	call, err := c.c.Submit(e)
	if err != nil {
		return addFuture{}, err
	}
	return addFuture{c, call, call.ID()}, nil
}

func (f addFuture) Wait() (int32, error) {
	d, err := f.c.c.Wait(f.call, f.id)
	if err != nil {
		return 0, err
	}
	defer d.Release()
	if len(d.Body) != 4 || d.Fail {
		return 0, d.Malformed()
	}
	return int32(hqqLE.Uint32(d.Body[0:])), nil
}

// CounterNew: @error(null), RESP_BOUND 4.
func (c *stubClient) CounterNew(start int64) (counterH, error) {
	e, err := c.c.BeginRequest(opCounterNew, 8, 4)
	if err != nil {
		return counterH{}, err
	}
	hqqLE.PutUint64(e.Body(), uint64(start))
	call, err := c.c.Submit(e)
	if err != nil {
		return counterH{}, err
	}
	d, err := c.c.Wait(call, call.ID())
	if err != nil {
		return counterH{}, err
	}
	defer d.Release()
	if len(d.Body) != 4 || d.Detail {
		return counterH{}, d.Malformed()
	}
	id := hqqLE.Uint32(d.Body[0:])
	if (id == 0) != d.Fail {
		return counterH{}, d.Malformed()
	}
	if d.Fail {
		return counterH{}, d.LibError("ml_counter_new", 0, 4)
	}
	return counterH{c.c.NewHandle(id)}, nil
}

func (c *stubClient) CounterAdd(h counterH, delta int64) (int64, error) {
	if err := c.c.CheckHandle(h.Handle, false); err != nil {
		return 0, err
	}
	e, err := c.c.BeginRequest(opCounterAdd, 16, 16+DetailReserve)
	if err != nil {
		return 0, err
	}
	body := e.Body()
	hqqLE.PutUint32(body[0:], h.ID())
	clear(body[4:8])
	hqqLE.PutUint64(body[8:], uint64(delta))
	call, err := c.c.Submit(e)
	if err != nil {
		return 0, err
	}
	d, err := c.c.Wait(call, call.ID())
	if err != nil {
		return 0, err
	}
	defer d.Release()
	b := d.Body
	if len(b) < 16 {
		return 0, d.Malformed()
	}
	rc := int32(hqqLE.Uint32(b[0:]))
	if (rc == 0) == d.Fail {
		return 0, d.Malformed()
	}
	if d.Fail {
		return 0, d.LibError("ml_counter_add", int64(rc), 16)
	}
	if len(b) != 16 || d.Detail {
		return 0, d.Malformed()
	}
	return int64(hqqLE.Uint64(b[8:])), nil
}

// CounterFree is the destroy function of Counter: void, empty response.
func (c *stubClient) CounterFree(h counterH) error {
	if err := c.c.CheckHandle(h.Handle, false); err != nil {
		return err
	}
	e, err := c.c.BeginRequest(opCounterFree, 4, 0)
	if err != nil {
		return err
	}
	hqqLE.PutUint32(e.Body(), h.ID())
	call, err := c.c.Submit(e)
	if err != nil {
		return err
	}
	d, err := c.c.Wait(call, call.ID())
	if err != nil {
		return err
	}
	defer d.Release()
	if len(d.Body) != 0 || d.Fail || d.Detail {
		return d.Malformed()
	}
	return nil
}

func (c *stubClient) CursorOpen(h counterH, step int32) (counterH, error) {
	if err := c.c.CheckHandle(h.Handle, false); err != nil {
		return counterH{}, err
	}
	e, err := c.c.BeginRequest(opCursorOpen, 8, 4)
	if err != nil {
		return counterH{}, err
	}
	body := e.Body()
	hqqLE.PutUint32(body[0:], h.ID())
	hqqLE.PutUint32(body[4:], uint32(step))
	call, err := c.c.Submit(e)
	if err != nil {
		return counterH{}, err
	}
	d, err := c.c.Wait(call, call.ID())
	if err != nil {
		return counterH{}, err
	}
	defer d.Release()
	if len(d.Body) != 4 || d.Detail {
		return counterH{}, d.Malformed()
	}
	id := hqqLE.Uint32(d.Body[0:])
	if (id == 0) != d.Fail {
		return counterH{}, d.Malformed()
	}
	if d.Fail {
		return counterH{}, d.LibError("ml_cursor_open", 0, 4)
	}
	return counterH{c.c.NewHandle(id)}, nil
}

func (c *stubClient) CursorFree(h counterH) error {
	if err := c.c.CheckHandle(h.Handle, false); err != nil {
		return err
	}
	e, err := c.c.BeginRequest(opCursorFree, 4, 4)
	if err != nil {
		return err
	}
	hqqLE.PutUint32(e.Body(), h.ID())
	call, err := c.c.Submit(e)
	if err != nil {
		return err
	}
	d, err := c.c.Wait(call, call.ID())
	if err != nil {
		return err
	}
	defer d.Release()
	b := d.Body
	if len(b) != 4 || d.Detail {
		return d.Malformed()
	}
	rc := int32(hqqLE.Uint32(b[0:]))
	if (rc == 0) == d.Fail {
		return d.Malformed()
	}
	if d.Fail {
		return d.LibError("ml_cursor_free", int64(rc), 4)
	}
	return nil
}

func (c *stubClient) CounterCheck(h counterH, limit int32) error {
	if err := c.c.CheckHandle(h.Handle, false); err != nil {
		return err
	}
	e, err := c.c.BeginRequest(opCounterCheck, 8, 8+DetailReserve)
	if err != nil {
		return err
	}
	body := e.Body()
	hqqLE.PutUint32(body[0:], h.ID())
	hqqLE.PutUint32(body[4:], uint32(limit))
	call, err := c.c.Submit(e)
	if err != nil {
		return err
	}
	d, err := c.c.Wait(call, call.ID())
	if err != nil {
		return err
	}
	defer d.Release()
	b := d.Body
	if len(b) < 4 {
		return d.Malformed()
	}
	rc := int32(hqqLE.Uint32(b[0:]))
	if (rc == 0) == d.Fail {
		return d.Malformed()
	}
	if d.Fail {
		return d.LibError("ml_counter_check", int64(rc), 4)
	}
	if len(b) != 4 || d.Detail {
		return d.Malformed()
	}
	return nil
}

type xorFuture struct {
	c    *stubClient
	call *Call
	id   uint32
	dst  []byte
}

func (c *stubClient) Xor(src []byte, key uint8, dst []byte) ([]byte, error) {
	f, err := c.XorAsync(src, key, dst)
	if err != nil {
		return nil, err
	}
	return f.Wait()
}

func (c *stubClient) XorAsync(src []byte, key uint8, dst []byte) (xorFuture, error) {
	if len(src) > 65536 {
		return xorFuture{}, ErrInvalidArg
	}
	e, err := c.c.BeginRequest(opXor, 8+BytesWire(len(src)), 8+VarSlack+int64(len(dst)))
	if err != nil {
		return xorFuture{}, err
	}
	body := e.Body()
	body[0] = key
	clear(body[1:4])
	hqqLE.PutUint32(body[4:], uint32(len(dst)))
	PutBytes(body, 8, src)
	call, err := c.c.Submit(e)
	if err != nil {
		return xorFuture{}, err
	}
	return xorFuture{c, call, call.ID(), dst}, nil
}

func (f xorFuture) Wait() ([]byte, error) {
	d, err := f.c.c.Wait(f.call, f.id)
	if err != nil {
		return nil, err
	}
	defer d.Release()
	b := d.Body
	if len(b) < 8 || d.Detail {
		return nil, d.Malformed()
	}
	ret := hqqLE.Uint64(b[0:])
	if d.Fail {
		return nil, d.LibError("ml_xor", int64(ret), 8)
	}
	n, end, err := GetBytesInto(f.dst, b, 8)
	if err != nil || end != len(b) || uint64(n) != ret {
		return nil, d.Malformed()
	}
	return f.dst[:n], nil
}

type cell struct {
	Tag  uint32
	Int  int64
	Text string
}

const (
	cellInt  = 1
	cellText = 2
	cellNull = 3
)

func (c *stubClient) Cells(h counterH, max int32, dst []cell) ([]cell, error) {
	if err := c.c.CheckHandle(h.Handle, false); err != nil {
		return nil, err
	}
	e, err := c.c.BeginRequest(opCells, 8, 8192)
	if err != nil {
		return nil, err
	}
	body := e.Body()
	hqqLE.PutUint32(body[0:], h.ID())
	hqqLE.PutUint32(body[4:], uint32(max))
	call, err := c.c.Submit(e)
	if err != nil {
		return nil, err
	}
	d, err := c.c.Wait(call, call.ID())
	if err != nil {
		return nil, err
	}
	defer d.Release()
	return decodeCells(&d, dst)
}

// decodeCells decodes a Cell[]<=256 response body, the way the generated
// Cells future does.
func decodeCells(d *Dec, dst []cell) ([]cell, error) {
	b := d.Body
	if d.Fail || d.Detail {
		return nil, d.Malformed()
	}
	cnt, off, err := GetCount(b, 0, 256, 4)
	if err != nil {
		return nil, d.Malformed()
	}
	out := dst[:0]
	for i := uint32(0); i < cnt; i++ {
		var v cell
		off = Align(off, 4)
		if off+4 > len(b) {
			return nil, d.Malformed()
		}
		v.Tag = hqqLE.Uint32(b[off:])
		off += 4
		switch v.Tag {
		case cellInt:
			off = Align(off, 8)
			if off+8 > len(b) {
				return nil, d.Malformed()
			}
			v.Int = int64(hqqLE.Uint64(b[off:]))
			off += 8
		case cellText:
			if v.Text, off, err = GetStr(b, off, 16); err != nil {
				return nil, d.Malformed()
			}
		case cellNull:
		default:
			return nil, d.Malformed()
		}
		out = append(out, v)
	}
	if off != len(b) {
		return nil, d.Malformed()
	}
	return out, nil
}
