package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"gosuda.org/hqq"
	"gosuda.org/hqq/conformance"
	"gosuda.org/hqq/internal/shm"
)

// X-01: create, open, exchange, close. The creator writes Payload(seed, 0, size),
// the opener verifies it and replies with Payload(seed+1, 0, size). The side that
// closes second sees the peer close (io.EOF after the buffered data is consumed).
// The creator's close removes the name; an opener's close does not.
func init() {
	register(&scenario{
		ID: "X-01", Title: "create, open, exchange, close", Kind: "standard",
		Platforms: []string{"any"}, TimeoutS: 30,
		Variants: []variant{
			{Name: "creator-closes-first", Params: params{BufferSize: 256, Messages: 1},
				Steps: []step{{Roles: []string{"creator", "opener"}, Expect: map[string]string{"creator": "ok", "opener": "ok"}}}},
			{Name: "opener-closes-first", Params: params{BufferSize: 256, Messages: 1},
				Steps: []step{{Roles: []string{"creator", "opener"}, Expect: map[string]string{"creator": "ok", "opener": "ok"}}}},
		},
		Run: runX01,
	})
}

func runX01(e *env) error {
	size, seed := e.cfg.Size, e.cfg.Seed
	creatorFirst := e.cfg.Variant == "creator-closes-first"
	var (
		link *hqq.NamedStandardLink
		err  error
	)
	if e.cfg.Role == "creator" {
		link, err = stdCreateSignal(e)
	} else {
		link, err = stdOpenAfter(e)
	}
	if err != nil {
		return err
	}
	defer func() { _ = link.Close() }()
	stdBind(e, link.StandardLink)

	buf := make([]byte, size)
	if e.cfg.Role == "creator" {
		if _, err := link.Write(conformance.Payload(seed, 0, size)); err != nil {
			return err
		}
		e.res.Sent++
		if _, err := io.ReadFull(link, buf); err != nil {
			return err
		}
		e.res.Received++
		e.check("creator receives the reply intact", conformance.VerifyPayload(seed+1, 0, buf) == nil, "")
	} else {
		if _, err := io.ReadFull(link, buf); err != nil {
			return err
		}
		e.res.Received++
		e.check("opener receives the message intact", conformance.VerifyPayload(seed, 0, buf) == nil, "")
		if _, err := link.Write(conformance.Payload(seed+1, 0, size)); err != nil {
			return err
		}
		e.res.Sent++
	}
	if err := e.barrier("replied"); err != nil {
		return err
	}

	if (e.cfg.Role == "creator") == creatorFirst {
		// This side closes first.
		if err := link.Close(); err != nil {
			return err
		}
		if e.cfg.Role == "creator" {
			probe, err := hqq.OpenStandardLink(e.cfg.Name, e.cfg.BufferCount, e.cfg.BufferSize, stdOptions(e.cfg))
			if probe != nil {
				_ = probe.Close()
			}
			// A Windows name lives while any handle to it is open, and the
			// opener still holds its endpoint here: the name is there and busy.
			if shm.NamesPersist {
				e.checkErr("creator close removes the name", err, os.ErrNotExist)
			} else {
				e.checkErr("creator close keeps the name while the opener holds it (Windows)", err, hqq.ErrLinkBusy)
			}
		} else {
			probe, err := hqq.OpenStandardLink(e.cfg.Name, e.cfg.BufferCount, e.cfg.BufferSize, stdOptions(e.cfg))
			if probe != nil {
				_ = probe.Close()
			}
			e.checkErr("opener close leaves the name in place", err, hqq.ErrLinkBusy)
		}
		return e.barrier("closed")
	}

	// The other side waits for the peer's close, then sees the end of stream.
	_, err = link.Read(buf)
	e.checkErr("peer close is seen as io.EOF after buffered data", err, io.EOF)
	if err := e.barrier("closed"); err != nil {
		return err
	}
	if e.cfg.Role == "creator" {
		// The opener closed first; the creator's close must now remove the name.
		if err := link.Close(); err != nil {
			return err
		}
		probe, err := hqq.OpenStandardLink(e.cfg.Name, e.cfg.BufferCount, e.cfg.BufferSize, stdOptions(e.cfg))
		if probe != nil {
			_ = probe.Close()
		}
		e.checkErr("creator close removes the name", err, os.ErrNotExist)
	}
	return nil
}

// X-02: geometry mismatch. The opener uses another buffer count or size than
// the creator; it must fail with ErrInvalidSize (a *GeometryError when the
// layouts are the same size).
func init() {
	geo := func(name string, args []string) variant {
		return variant{Name: name, Params: params{BufferCount: 4, BufferSize: 256},
			Steps: []step{{Roles: []string{"creator", "opener"},
				Expect: map[string]string{"creator": "ok", "opener": "ok"},
				Args:   map[string][]string{"opener": args}}}}
	}
	register(&scenario{
		ID: "X-02", Title: "geometry mismatch (count, size)", Kind: "standard",
		Platforms: []string{"any"}, TimeoutS: 30,
		Variants: []variant{
			geo("count-double", []string{"--buffer-count", "8"}),
			geo("size-double", []string{"--buffer-size", "512"}),
			geo("count-half", []string{"--buffer-count", "2"}),
			geo("size-half", []string{"--buffer-size", "128"}),
		},
		Run: runX02,
	})
}

func runX02(e *env) error {
	if e.cfg.Role == "creator" {
		link, err := stdCreateSignal(e)
		if err != nil {
			return err
		}
		defer func() { _ = link.Close() }()
		e.check("creator creates the segment", true, "")
		return e.barrier("checked")
	}
	if err := e.barrier("created"); err != nil {
		return err
	}
	link, err := stdOpen(e)
	if link != nil {
		_ = link.Close()
	}
	e.checkErr("mismatched geometry fails with ErrInvalidSize", err, hqq.ErrInvalidSize)
	var geoErr *hqq.GeometryError
	e.meta("geometry_error", errors.As(err, &geoErr))
	return e.barrier("checked")
}

// X-03: third endpoint, re-join after own close, creator vs live segment.
// While the creator and the opener are both attached: a creator's create fails
// with ErrLinkBusy, a second opener handle (third endpoint) fails with
// ErrLinkBusy. After the opener closes its handle, re-opening still fails with
// ErrLinkBusy because the record stays detached until re-initialization.
func init() {
	register(&scenario{
		ID: "X-03", Title: "third endpoint, re-join after own close, creator vs live segment", Kind: "standard",
		Platforms: []string{"any"}, TimeoutS: 30,
		Variants: []variant{
			{Name: "default", Params: params{BufferCount: 4, BufferSize: 256},
				Steps: []step{{Roles: []string{"creator", "opener"}, Expect: map[string]string{"creator": "ok", "opener": "ok"}}}},
		},
		Run: runX03,
	})
}

func runX03(e *env) error {
	if e.cfg.Role == "creator" {
		link, err := stdCreateSignal(e)
		if err != nil {
			return err
		}
		defer func() { _ = link.Close() }()
		stdBind(e, link.StandardLink)
		if err := e.barrier("joined"); err != nil {
			return err
		}
		again, err := stdCreate(e)
		if again != nil {
			_ = again.Close()
		}
		e.checkErr("creator on a live name fails with ErrLinkBusy", err, hqq.ErrLinkBusy)
		if err := e.barrier("creator-checked"); err != nil {
			return err
		}
		return e.barrier("reopen-done")
	}

	first, err := stdOpenAfter(e)
	if err != nil {
		return err
	}
	stdBind(e, first.StandardLink)
	if err := e.barrier("joined"); err != nil {
		_ = first.Close()
		return err
	}
	third, err := stdOpen(e)
	if third != nil {
		_ = third.Close()
	}
	e.checkErr("third endpoint fails with ErrLinkBusy", err, hqq.ErrLinkBusy)
	if err := e.barrier("creator-checked"); err != nil {
		_ = first.Close()
		return err
	}
	if err := first.Close(); err != nil {
		return err
	}
	again, err := stdOpen(e)
	if again != nil {
		_ = again.Close()
	}
	e.checkErr("re-join after own close fails with ErrLinkBusy", err, hqq.ErrLinkBusy)
	return e.barrier("reopen-done")
}

// X-10: safe unlink. The creator's name is removed with hqq.Unlink and replaced
// by another object (internal/shm). Closing the creator must not remove the
// replacement. Unlink of a missing name is a no-op returning nil. On Windows a
// name cannot be replaced while a handle holds it, so the scenario does not
// apply there.
func init() {
	register(&scenario{
		ID: "X-10", Title: "safe unlink: replaced name survives the creator's close", Kind: "standard",
		Platforms: []string{"linux", "darwin"}, TimeoutS: 30,
		Variants: []variant{
			{Name: "default", Params: params{BufferCount: 4, BufferSize: 256},
				Steps: []step{{Roles: []string{"creator"}, Expect: map[string]string{"creator": "ok"}}}},
		},
		Run: runX10,
	})
}

func runX10(e *env) error {
	name := e.cfg.Name
	link, err := stdCreate(e)
	if err != nil {
		return err
	}
	defer func() { _ = link.Close() }()

	e.checkErr("unlink of the live name returns nil", hqq.Unlink(name), nil)
	_, err = hqq.OpenStandardLink(name, e.cfg.BufferCount, e.cfg.BufferSize, stdOptions(e.cfg))
	e.checkErr("name is gone after unlink", err, os.ErrNotExist)

	page := os.Getpagesize()
	repl, err := shm.OpenSharedMemory(name, 2*page, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("creating the replacement object: %w", err)
	}
	_ = repl.Close()

	e.checkErr("creator close returns nil while the name was replaced", link.Close(), nil)
	probe, err := shm.OpenSharedMemory(name, 0, os.O_RDWR, 0)
	if err != nil {
		e.check("replacement survives the creator's close", false, err.Error())
	} else {
		_ = probe.Close()
		e.check("replacement survives the creator's close", true, "")
	}
	e.checkErr("unlink removes the replacement", hqq.Unlink(name), nil)
	// SPEC 11.4: an endpoint's Unlink of a missing name is a no-op returning
	// success. The package-level Unlink reports the missing name (ENOENT).
	gone, err := stdCreate(e)
	if err != nil {
		return err
	}
	e.checkErr("package Unlink removes the live name", hqq.Unlink(name), nil)
	e.checkErr("endpoint Unlink of a missing name is a no-op", gone.Unlink(), nil)
	e.checkErr("close after the name went away returns nil", gone.Close(), nil)
	return nil
}

// X-11: invalid names. Create and open must fail with ErrInvalidName for every
// invalid name of the platform; a valid 30-byte name works and is removed.
func init() {
	register(&scenario{
		ID: "X-11", Title: "invalid names per platform, long names on macOS", Kind: "standard",
		Platforms: []string{"any"}, TimeoutS: 30,
		Variants: []variant{
			{Name: "default", Params: params{BufferCount: 4, BufferSize: 256},
				Steps: []step{{Roles: []string{"creator"}, Expect: map[string]string{"creator": "ok"}}}},
		},
		Run: runX11,
	})
}

func runX11(e *env) error {
	invalid := []string{"", "/", "a/b", ".", "..", "a\x00b", strings.Repeat("a", 256)}
	switch runtime.GOOS {
	case "darwin":
		// The whole name including the leading slash is limited to 31 bytes.
		invalid = append(invalid, strings.Repeat("a", 31))
	case "windows":
		invalid = append(invalid, `Global\`)
	}
	opts := stdOptions(e.cfg)
	for _, name := range invalid {
		_, err := hqq.CreateStandardLink(name, e.cfg.BufferCount, e.cfg.BufferSize, 0o600, opts)
		e.checkErr(fmt.Sprintf("create %q -> ErrInvalidName", stdShorten(name)), err, hqq.ErrInvalidName)
		_, err = hqq.OpenStandardLink(name, e.cfg.BufferCount, e.cfg.BufferSize, opts)
		e.checkErr(fmt.Sprintf("open %q -> ErrInvalidName", stdShorten(name)), err, hqq.ErrInvalidName)
	}

	prefix := e.cfg.Name
	if len(prefix) > 20 {
		prefix = prefix[:20]
	}
	valid := (prefix + "-x11-000000000000000000000000000000")[:30]
	link, err := hqq.CreateStandardLink(valid, e.cfg.BufferCount, e.cfg.BufferSize, 0o600, opts)
	if err != nil {
		e.check("valid 30-byte name is created", false, err.Error())
		return nil
	}
	e.check("valid 30-byte name is created", true, "")
	e.checkErr("valid 30-byte name is removed on close", link.Close(), nil)
	_, err = hqq.OpenStandardLink(valid, e.cfg.BufferCount, e.cfg.BufferSize, opts)
	e.checkErr("valid 30-byte name is gone after close", err, os.ErrNotExist)
	return nil
}

// stdShorten keeps check names readable for long invalid names.
func stdShorten(s string) string {
	if len(s) > 40 {
		return fmt.Sprintf("%s...(%d bytes)", s[:40], len(s))
	}
	return s
}
