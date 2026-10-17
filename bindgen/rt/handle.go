package rt

import "sync/atomic"

// sessionSeq numbers the Conns of the process; 0 is the stamp of no session.
var sessionSeq atomic.Uint64

// Handle is a session-stamped handle id (4.9). The zero value is the nil
// handle.
type Handle struct {
	id   uint32
	sess uint64
}

// ID is the wire id: generation in the upper 16 bits, index in the lower 16.
func (h Handle) ID() uint32 { return h.id }

// IsNil reports whether h is NULL (id 0).
func (h Handle) IsNil() bool { return h.id == 0 }

// NewHandle stamps id with this session. Id 0 gives the nil Handle.
func (c *Conn) NewHandle(id uint32) Handle {
	if id == 0 {
		return Handle{}
	}
	return Handle{id: id, sess: c.sess}
}

// CheckHandle refuses a handle of another session with ErrForeignHandle and a
// nil handle that is not nullable with ErrInvalidArg, before anything is
// sent.
func (c *Conn) CheckHandle(h Handle, nullable bool) error {
	if h.id == 0 {
		if nullable {
			return nil
		}
		return ErrInvalidArg
	}
	if h.sess != c.sess {
		return ErrForeignHandle
	}
	return nil
}
