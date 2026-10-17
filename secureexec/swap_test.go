//go:build unix

package secureexec

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type mechCase struct {
	name string
	opts []Option
}

// swapCases are the mechanisms exercised by the attack tests. "default" is
// whatever Verify picks on this host (memfd on Linux, vault elsewhere).
func swapCases(t *testing.T) []mechCase {
	return []mechCase{
		{"default", nil},
		{"vault", []Option{WithMechanism(MechanismVault), WithVaultDir(filepath.Join(t.TempDir(), "vaults"))}},
	}
}

// overwriteInPlace truncates the inode that path currently names and writes
// data into it (attack 2: in-place overwrite).
func overwriteInPlace(t *testing.T, path string, data []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(data, 0); err != nil {
		t.Fatal(err)
	}
}

// renameOver replaces the directory entry at path with the file at evil
// (attack 1: path swap).
func renameOver(t *testing.T, evil, path string) {
	t.Helper()
	tmp := path + ".attacker"
	if err := os.Link(evil, tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

// TestSwapAfterVerifyRunsVerifiedBytes verifies a copy of the victim, then
// (as another user would) rewrites the inode in place, truncates it, and
// renames an evil executable over the path. The started child must still be
// the verified victim.
func TestSwapAfterVerifyRunsVerifiedBytes(t *testing.T) {
	evil := helperBinary(t, "EVIL-SWAP")
	for _, tc := range swapCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			src, want := victimCopy(t, "VICTIM-SWAP", dir)
			evilCopy := filepath.Join(dir, "evil")
			copyExecutable(t, evil, evilCopy)
			evilBytes, err := os.ReadFile(evilCopy)
			if err != nil {
				t.Fatal(err)
			}

			v, err := Verify(src, want, tc.opts...)
			if err != nil {
				t.Fatal(err)
			}
			defer v.Close()

			overwriteInPlace(t, src, evilBytes)
			renameOver(t, evilCopy, src)
			if fileHash(t, src) == want {
				t.Fatal("attack did not change the path; test is not exercising the swap")
			}

			got := runVerified(t, v, "arg")
			if len(got) == 0 || got[0] != "VICTIM-SWAP" {
				t.Fatalf("(%s) child ran %q; want the verified victim", v.Mechanism(), got)
			}
		})
	}
}

// TestSwapRaceDuringExec keeps an attacker rewriting and renaming the path
// while the verified object is executed. Whenever Verify succeeds, the child
// must run the verified bytes.
func TestSwapRaceDuringExec(t *testing.T) {
	victim := helperBinary(t, "VICTIM-RACE")
	evil := helperBinary(t, "EVIL-RACE")
	for _, tc := range swapCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "tool")
			want := fileHash(t, victim)
			evilCopy := filepath.Join(dir, "evil")
			copyExecutable(t, evil, evilCopy)
			evilBytes, err := os.ReadFile(evilCopy)
			if err != nil {
				t.Fatal(err)
			}

			for i := range 20 {
				// A fresh inode at src holding the victim bytes.
				copyExecutable(t, victim, src+".next")
				if err := os.Rename(src+".next", src); err != nil {
					t.Fatal(err)
				}
				v, err := Verify(src, want, tc.opts...)
				if err != nil {
					t.Fatalf("round %d: Verify: %v", i, err)
				}

				stop := make(chan struct{})
				done := make(chan struct{})
				go attack(src, evilCopy, evilBytes, stop, done)

				got := runQuiet(v, "r")
				close(stop)
				<-done
				if err := v.Close(); err != nil {
					t.Fatalf("round %d: Close: %v", i, err)
				}
				if len(got) == 0 || got[0] != "VICTIM-RACE" {
					t.Fatalf("round %d (%s): child ran %q; want the verified victim", i, v.Mechanism(), got)
				}
			}
		})
	}
}

// attack rewrites src in place and swaps in the evil executable until stop is
// closed. Errors are ignored: the attacker only needs to win some races.
func attack(src, evilCopy string, evilBytes []byte, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	for {
		select {
		case <-stop:
			return
		default:
		}
		if f, err := os.OpenFile(src, os.O_WRONLY, 0); err == nil {
			_ = f.Truncate(0)
			_, _ = f.WriteAt(evilBytes, 0)
			_ = f.Close()
		}
		if os.Link(evilCopy, src+".race") == nil {
			_ = os.Rename(src+".race", src)
		}
		time.Sleep(20 * time.Microsecond)
	}
}

// runQuiet runs v and returns its output lines; a failed run yields nil so
// the caller reports the outcome.
func runQuiet(v *Verified, args ...string) []string {
	var buf bytes.Buffer
	cmd := v.Command(context.Background(), args...)
	cmd.Stdout = &buf
	if err := cmd.Run(); err != nil {
		return nil
	}
	return splitLines(buf.String())
}
