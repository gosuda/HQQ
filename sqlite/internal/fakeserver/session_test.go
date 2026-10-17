package fakeserver

import (
	"context"
	"errors"
	"testing"

	"gosuda.org/hqq/sqlite/internal/wire"
)

// TestOpenChecksVersionFirst checks PROTOCOL.md section 6.1: the major version
// is read before anything else, so a body of another major version is refused
// with VERSION even when the rest of it would not parse.
func TestOpenChecksVersionFirst(t *testing.T) {
	s := &session{srv: &Server{opts: Options{MaxSessions: 1}}}
	garbage := []byte{2, 0, 0xff, 0xff, 0xff}
	_, err := s.openSession(context.Background(), garbage)
	var pe *wire.ProtocolError
	if !errors.As(err, &pe) || pe.Code != wire.ProtoVersion {
		t.Fatalf("err = %v, want VERSION", err)
	}
	if s.open {
		t.Fatal("session opened despite the version error")
	}
	// A short body is not a version: it is refused as a version error too.
	_, err = s.openSession(context.Background(), []byte{1})
	if !errors.As(err, &pe) || pe.Code != wire.ProtoVersion {
		t.Fatalf("short body err = %v, want VERSION", err)
	}
}
