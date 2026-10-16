package conformance

import (
	"fmt"
	"testing"

	"gosuda.org/hqq"
)

// TestSuperblockImages (G-08) attaches every superblock image of the manifest
// through each of the three attach paths and checks the result, the error
// kind and what was written:
//
//   - creator: CreateStandardLink on a named object holding the image;
//   - opener: OpenStandardLink on the same kind of object;
//   - inmemory: NewStandardLink on caller-owned memory holding the image.
//
// "none" means the segment bytes are identical afterwards; "join" means only
// the secondary endpoint record changed and it now carries a token; "init"
// means the superblock was re-initialized to Ready. Cases of another page
// family are skipped per case.
func TestSuperblockImages(t *testing.T) {
	m := loadManifest(t)
	for _, c := range m.Superblocks {
		t.Run(fmt.Sprintf("%s/page%d", c.ID, c.PageSize), func(t *testing.T) {
			if c.PageSize != hostPage() {
				t.Skipf("image of page family %d; this host uses %d", c.PageSize, hostPage())
			}
			t.Parallel()
			image := readVector(t, c.File)
			if len(image) != c.FileBytes {
				t.Fatalf("%s has %d bytes, manifest says %d", c.File, len(image), c.FileBytes)
			}
			roles := []struct {
				name string
				want RoleOutcome
			}{
				{"creator", c.Creator},
				{"opener", c.Opener},
				{"inmemory", c.InMemory},
			}
			for _, r := range roles {
				t.Run(r.name, func(t *testing.T) {
					t.Parallel()
					runSuperblockRole(t, c, r.name, r.want, image)
				})
			}
		})
	}
}

// runSuperblockRole performs one attach of role on the image and checks it
// against want.
func runSuperblockRole(t *testing.T, c SuperblockCase, role string, want RoleOutcome, image []byte) {
	t.Helper()
	count, size := c.CallerBufferCount, c.CallerBufferSize
	callerTotal := int(hqq.SizeStandardLink(count, size))
	if callerTotal == 0 {
		t.Fatalf("no layout for the caller geometry %d x %d", count, size)
	}
	opts := testOpts()

	var (
		view    []byte // the segment bytes the writes check compares
		before  []byte // the segment bytes before the attach
		mode    hqq.LinkMode
		err     error
		closeFn = func() error { return nil }
	)
	switch role {
	case "inmemory":
		view = hqq.AlignedBuffer(uintptr(callerTotal))
		copy(view, image)
		before = append([]byte(nil), view...)
		var sl *hqq.StandardLink
		sl, err = hqq.NewStandardLink(view, count, size, opts)
		if err == nil {
			mode = sl.Mode()
			closeFn = sl.Close
		}
	case "creator", "opener":
		name := uniqueName()
		objSize := max(c.SegmentBytes, callerTotal)
		view = createNamedObject(t, name, image, objSize)
		before = append([]byte(nil), view...)
		if role == "creator" {
			var nl *hqq.NamedStandardLink
			nl, err = hqq.CreateStandardLink(name, count, size, 0o600, opts)
			if err == nil {
				mode = nl.Mode()
				closeFn = nl.Close
			}
		} else {
			var nl *hqq.NamedStandardLink
			nl, err = hqq.OpenStandardLink(name, count, size, opts)
			if err == nil {
				mode = nl.Mode()
				closeFn = nl.Close
			}
		}
	default:
		t.Fatalf("unknown role %q", role)
	}
	t.Cleanup(func() { _ = closeFn() })

	if want.Error != "" {
		if err == nil {
			t.Fatalf("attach succeeded as %v; want error %s", mode, want.Error)
		}
		if !matchesKind(err, want.Error) {
			t.Fatalf("attach error %v; want %s", err, want.Error)
		}
	} else {
		if err != nil {
			t.Fatalf("attach failed: %v; want %s", err, want.Result)
		}
		wantMode := hqq.LinkModePrimary
		if want.Result == "secondary" {
			wantMode = hqq.LinkModeSecondary
		}
		if mode != wantMode {
			t.Fatalf("attach mode %v; want %s", mode, want.Result)
		}
	}

	checkWrites(t, want.Writes, before, view)
}

// checkWrites compares the segment bytes before and after an attach with the
// expected kind of write (see RoleOutcome.Writes).
func checkWrites(t *testing.T, writes string, before, after []byte) {
	t.Helper()
	switch writes {
	case "none":
		if d := firstDiffOutside(before, after, 0, 0); d >= 0 {
			t.Errorf("attach wrote byte %d; the expected write is none", d)
		}
	case "join":
		lo, hi := SBEndpointSecond, SBEndpointSecond+RecordSize
		if d := firstDiffOutside(before, after, lo, hi); d >= 0 {
			t.Errorf("join wrote byte %d outside the secondary record", d)
		}
		if tok := segmentWord(after, lo+RecToken); tok == TokenUnused || tok == TokenDetached {
			t.Errorf("secondary token after join is %#x; want an owner token", tok)
		}
		if segmentWord(before, lo+RecToken) != TokenUnused {
			t.Errorf("secondary token before join is not zero")
		}
	case "init":
		if got := segmentWord(after, SBMagic); got != MagicSuperblock {
			t.Errorf("magic after init = %#x", got)
		}
		if got := segmentWord(after, SBState); got != StateReady {
			t.Errorf("state after init = %d; want Ready", got)
		}
		if got := segmentWord(after, SBEpoch); got < 1 {
			t.Errorf("epoch after init = %d", got)
		}
		if got := segmentWord(after, SBInitToken); got != TokenUnused {
			t.Errorf("initToken after init = %#x; want 0", got)
		}
	default:
		t.Fatalf("unknown write kind %q", writes)
	}
}
