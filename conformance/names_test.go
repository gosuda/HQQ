package conformance

import (
	"errors"
	"os"
	"runtime"
	"testing"

	"gosuda.org/hqq"
)

// TestNameVectors (G-05) checks name validation through CreateStandardLink
// and OpenStandardLink: invalid names fail with ErrInvalidName and create
// nothing; valid names create a Primary that removes its name on close. The
// darwin length vectors (30 bytes valid, 31 bytes invalid, without the
// leading slash) run on macOS only, and the Windows vectors on Windows only.
func TestNameVectors(t *testing.T) {
	m := loadManifest(t)
	for _, nv := range m.Names {
		if nv.Platform == "darwin" && runtime.GOOS != "darwin" {
			continue
		}
		if nv.Platform == "windows" && runtime.GOOS != "windows" {
			continue
		}
		t.Run(nameLabel(nv.Name, nv.Valid), func(t *testing.T) {
			opts := testOpts()
			if !nv.Valid {
				link, err := hqq.CreateStandardLink(nv.Name, sbCount, sbSize, 0o600, opts)
				if link != nil {
					_ = link.Close()
					t.Fatalf("CreateStandardLink(%q) created a link", nv.Name)
				}
				if !errors.Is(err, hqq.ErrInvalidName) {
					t.Fatalf("CreateStandardLink(%q) = %v, want ErrInvalidName", nv.Name, err)
				}
				if _, err := hqq.OpenStandardLink(nv.Name, sbCount, sbSize, opts); !errors.Is(err, hqq.ErrInvalidName) {
					t.Fatalf("OpenStandardLink(%q) = %v, want ErrInvalidName", nv.Name, err)
				}
				return
			}
			link, err := hqq.CreateStandardLink(nv.Name, sbCount, sbSize, 0o600, opts)
			if err != nil {
				t.Fatalf("CreateStandardLink(%q): %v", nv.Name, err)
			}
			if link.Mode() != hqq.LinkModePrimary {
				t.Errorf("CreateStandardLink(%q) mode %v, want primary", nv.Name, link.Mode())
			}
			if err := link.Close(); err != nil {
				t.Fatalf("close %q: %v", nv.Name, err)
			}
			// The creator's close removes the name (SPEC 11.4).
			if _, err := hqq.OpenStandardLink(nv.Name, sbCount, sbSize, opts); !errors.Is(err, os.ErrNotExist) {
				_ = hqq.Unlink(nv.Name)
				t.Errorf("after close, OpenStandardLink(%q) = %v, want the name removed", nv.Name, err)
			}
		})
	}
}

// nameLabel is a subtest name for a name vector.
func nameLabel(name string, valid bool) string {
	if !valid {
		return "invalid-" + sanitize(name)
	}
	return "valid-" + sanitize(name)
}

// sanitize keeps subtest names printable.
func sanitize(s string) string {
	if s == "" {
		return "empty"
	}
	out := make([]byte, 0, len(s))
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}
