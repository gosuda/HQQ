package shm

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestSecurityDescriptorSDDL(t *testing.T) {
	const sid = "S-1-5-21-1-2-3-1001"
	tests := []struct {
		mode    os.FileMode
		section string
		sem     string
	}{
		// 0o600: owner reads and writes, nobody else.
		{0o600, "D:P(A;;0xf001f;;;SY)(A;;0xf001f;;;" + sid + ")", "D:P(A;;0x1f0003;;;SY)(A;;0x1f0003;;;" + sid + ")"},
		// 0o444: everybody may read; no write right is granted anywhere.
		{0o444, "D:P(A;;0xf001f;;;SY)(A;;0x20005;;;" + sid + ")(A;;0x20005;;;WD)",
			"D:P(A;;0x1f0003;;;SY)(A;;0x120001;;;" + sid + ")(A;;0x120001;;;WD)"},
		// 0o606: other gets read and write, group bits do not matter.
		{0o666, "D:P(A;;0xf001f;;;SY)(A;;0xf001f;;;" + sid + ")(A;;0x20007;;;WD)",
			"D:P(A;;0x1f0003;;;SY)(A;;0x1f0003;;;" + sid + ")(A;;0x120003;;;WD)"},
		{0o060, "D:P(A;;0xf001f;;;SY)", "D:P(A;;0x1f0003;;;SY)"},
		// 0: only SYSTEM (and the creator's own handles).
		{0, "D:P(A;;0xf001f;;;SY)", "D:P(A;;0x1f0003;;;SY)"},
		// Non-permission bits are ignored.
		{os.ModeSetuid | 0o600, "D:P(A;;0xf001f;;;SY)(A;;0xf001f;;;" + sid + ")", "D:P(A;;0x1f0003;;;SY)(A;;0x1f0003;;;" + sid + ")"},
	}
	for _, tc := range tests {
		if got := securityDescriptorSDDL(tc.mode, sid, true); got != tc.section {
			t.Errorf("section SDDL for %#o = %q, want %q", tc.mode, got, tc.section)
		}
		if got := securityDescriptorSDDL(tc.mode, sid, false); got != tc.sem {
			t.Errorf("semaphore SDDL for %#o = %q, want %q", tc.mode, got, tc.sem)
		}
	}
}

func TestWindowsSharedMemoryName(t *testing.T) {
	for name, want := range map[string]string{
		"/seg":         `Local\seg`,
		"seg":          `Local\seg`,
		`Global\seg`:   `Global\seg`,
		`local\seg`:    `Local\seg`,
		`Local\a.b-c_`: `Local\a.b-c_`,
	} {
		got, err := windowsSharedMemoryName(name)
		if err != nil || got != want {
			t.Errorf("windowsSharedMemoryName(%q) = %q, %v; want %q", name, got, err, want)
		}
	}

	// The longest name whose semaphore names still fit.
	longest := maxObjectName - len(semaphoreSuffixes[0]) - len(`Local\`)
	if _, err := windowsSharedMemoryName(strings.Repeat("a", longest)); err != nil {
		t.Fatalf("name of %d characters rejected: %v", longest, err)
	}
	_, err := windowsSharedMemoryName(strings.Repeat("a", longest+1))
	if !errors.Is(err, ErrInvalidName) {
		t.Fatalf("name of %d characters = %v, want ErrInvalidName", longest+1, err)
	}
	// Characters outside the BMP count as two UTF-16 units.
	_, err = windowsSharedMemoryName(strings.Repeat("\U0001F600", longest/2+1))
	if !errors.Is(err, ErrInvalidName) {
		t.Fatalf("overlong astral name = %v, want ErrInvalidName", err)
	}
}

func TestSemaphoreSuffixesAreDistinct(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range semaphoreSuffixes {
		if s == "" || seen[s] {
			t.Fatalf("suffix %q is empty or duplicated", s)
		}
		seen[s] = true
	}
}
