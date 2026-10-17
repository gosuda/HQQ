package secureexec

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestVerifyRunsVerifiedBytes(t *testing.T) {
	src, want := victimCopy(t, "VICTIM-MATCH", t.TempDir())
	v, err := Verify(src, want)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	t.Logf("mechanism: %s", v.Mechanism())
	if v.Hash() != want {
		t.Fatalf("Hash() = %s, want %s", v.Hash(), want)
	}
	got := runVerified(t, v, "a", "b")
	wantLines := []string{"VICTIM-MATCH", src, "a", "b"}
	if !equalLines(got, wantLines) {
		t.Fatalf("output = %q, want %q", got, wantLines)
	}
}

func TestSpecVerify(t *testing.T) {
	src, want := victimCopy(t, "VICTIM-SPEC", t.TempDir())
	v, err := Spec{Path: src, SHA256: want}.Verify()
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	if err := v.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestVerifyHashMismatch(t *testing.T) {
	src, actual := victimCopy(t, "VICTIM-MISMATCH", t.TempDir())
	var wrong Hash
	copy(wrong[:], actual[:])
	wrong[0] ^= 0x01

	v, err := Verify(src, wrong)
	if err == nil {
		v.Close()
		t.Fatal("Verify accepted a wrong digest")
	}
	if !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("err = %v, want ErrHashMismatch", err)
	}
	var me *HashMismatchError
	if !errors.As(err, &me) {
		t.Fatalf("err = %T, want *HashMismatchError", err)
	}
	if me.Got != actual || me.Want != wrong || me.Path != src {
		t.Fatalf("HashMismatchError = %+v", me)
	}
}

func TestStartMismatchStartsNothing(t *testing.T) {
	src, _ := victimCopy(t, "VICTIM-START-MISMATCH", t.TempDir())
	var out bytes.Buffer
	_, err := Start(context.Background(), src, Hash{}, nil, WithStdio(nil, &out, nil))
	if !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("Start err = %v, want ErrHashMismatch", err)
	}
	if out.Len() != 0 {
		t.Fatalf("child produced output after a mismatch: %q", out.String())
	}
}

func TestStartWaitReleases(t *testing.T) {
	src, want := victimCopy(t, "VICTIM-START", t.TempDir())
	var out bytes.Buffer
	p, err := Start(context.Background(), src, want, []string{"x"}, WithStdio(nil, &out, nil))
	if err != nil {
		t.Fatal(err)
	}
	if p.Mechanism() == "" {
		t.Fatal("empty mechanism")
	}
	state, err := p.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if state.ExitCode() != 0 {
		t.Fatalf("exit code %d", state.ExitCode())
	}
	select {
	case <-p.Done():
	default:
		t.Fatal("Done not closed after Wait")
	}
	if got := splitLines(out.String()); !equalLines(got, []string{"VICTIM-START", src, "x"}) {
		t.Fatalf("output = %q", got)
	}
	if _, err := p.Wait(); err != nil {
		t.Fatalf("second Wait: %v", err)
	}
}

func TestWithEnvAndDirReachChild(t *testing.T) {
	// The helper does not print the environment; this only checks that Env and
	// Dir are accepted and do not break Start.
	src, want := victimCopy(t, "VICTIM-ENV", t.TempDir())
	var out bytes.Buffer
	p, err := Start(context.Background(), src, want, nil,
		WithStdio(nil, &out, nil), WithEnv([]string{"PATH=" + os.Getenv("PATH")}), WithDir(filepath.Dir(src)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Wait(); err != nil {
		t.Fatal(err)
	}
	if got := splitLines(out.String()); len(got) == 0 || got[0] != "VICTIM-ENV" {
		t.Fatalf("output = %q", got)
	}
}

func TestVerifyNotRegular(t *testing.T) {
	dir := t.TempDir()
	_, err := Verify(dir, Hash{})
	if !errors.Is(err, ErrNotRegular) {
		t.Fatalf("Verify(dir) err = %v, want ErrNotRegular", err)
	}
	if _, _, err := HashFile(dir); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("HashFile(dir) err = %v, want ErrNotRegular", err)
	}
}

func TestVerifyMissingFile(t *testing.T) {
	if _, err := Verify(filepath.Join(t.TempDir(), "absent"), Hash{}); err == nil {
		t.Fatal("Verify of a missing file succeeded")
	}
}

func TestVerifyRejectsUnknownMechanism(t *testing.T) {
	src, want := victimCopy(t, "VICTIM-MECH", t.TempDir())
	_, err := Verify(src, want, WithMechanism("bogus"))
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}

func equalLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
