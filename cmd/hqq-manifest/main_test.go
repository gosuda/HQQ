package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// call runs the tool and returns its exit status and combined output.
func call(args ...string) (int, string) {
	var out, errOut bytes.Buffer
	code := run(args, &out, &errOut)
	return code, out.String() + errOut.String()
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestEndToEnd(t *testing.T) {
	dir := t.TempDir()
	priv := filepath.Join(dir, "release.key")
	pub := filepath.Join(dir, "release.pub")
	art := filepath.Join(dir, "artifacts")
	if err := os.Mkdir(art, 0o755); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(dir, "manifest.json")
	sigPath := manifestPath + ".sig"

	if code, out := call("keygen", "-priv", priv, "-pub", pub); code != 0 || !strings.Contains(out, "public key: ") {
		t.Fatalf("keygen: %d %s", code, out)
	}
	if fi, err := os.Stat(priv); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("private key mode: %v %v", fi, err)
	}
	if code, _ := call("keygen", "-priv", priv, "-pub", pub); code == 0 {
		t.Fatal("keygen overwrote an existing key")
	}

	mustWrite(t, filepath.Join(art, "hqq-test_linux_amd64"), "linux-bytes")
	mustWrite(t, filepath.Join(art, "hqq-test_windows_amd64.exe"), "windows-bytes")
	mustWrite(t, filepath.Join(art, "unrelated.txt"), "ignored")

	if code, out := call("gen", "-dir", art, "-name", "hqq-test", "-release", "v1", "-license", "MIT", "-out", manifestPath); code != 0 {
		t.Fatalf("gen: %d %s", code, out)
	}
	if code, out := call("sign", "-key", priv, "-in", manifestPath); code != 0 {
		t.Fatalf("sign: %d %s", code, out)
	}
	if _, err := os.Stat(sigPath); err != nil {
		t.Fatalf("signature not written: %v", err)
	}

	code, out := call("verify", "-pub", pub, "-in", manifestPath, "-sig", sigPath, "-dir", art)
	if code != 0 || !strings.Contains(out, "ok: hqq-test v1, 2 files") {
		t.Fatalf("verify: %d %s", code, out)
	}

	// Tampered artifact: the signature still holds, the -dir check must fail.
	mustWrite(t, filepath.Join(art, "hqq-test_linux_amd64"), "linux-bytez")
	if code, _ := call("verify", "-pub", pub, "-in", manifestPath, "-sig", sigPath, "-dir", art); code != 1 {
		t.Fatalf("tampered artifact: exit %d, want 1", code)
	}
	// Without -dir the manifest alone still verifies.
	if code, _ := call("verify", "-pub", pub, "-in", manifestPath, "-sig", sigPath); code != 0 {
		t.Fatalf("manifest-only verify: exit %d", code)
	}

	// Tampered manifest: signature must fail.
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, manifestPath, strings.Replace(string(data), `"v1"`, `"v2"`, 1))
	if code, out := call("verify", "-pub", pub, "-in", manifestPath, "-sig", sigPath); code != 1 || !strings.Contains(out, "bad signature") {
		t.Fatalf("tampered manifest: exit %d, output %q", code, out)
	}
	mustWrite(t, manifestPath, string(data))

	// Signing from an environment variable (the CI secret path).
	keyText, err := os.ReadFile(priv)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HQQ_TEST_RELEASE_KEY", strings.TrimSpace(string(keyText)))
	envSig := filepath.Join(dir, "env.sig")
	if code, out := call("sign", "-key-env", "HQQ_TEST_RELEASE_KEY", "-in", manifestPath, "-out", envSig); code != 0 {
		t.Fatalf("sign -key-env: %d %s", code, out)
	}
	if code, _ := call("verify", "-pub", pub, "-in", manifestPath, "-sig", envSig); code != 0 {
		t.Fatalf("env-signed manifest did not verify: exit %d", code)
	}

	// hash prints sha256sum-style output.
	want := sha256.Sum256([]byte("linux-bytez"))
	code, out = call("hash", filepath.Join(art, "hqq-test_linux_amd64"))
	if code != 0 || out != hex.EncodeToString(want[:])+"  "+filepath.Join(art, "hqq-test_linux_amd64")+"\n" {
		t.Fatalf("hash output %q (exit %d)", out, code)
	}
}

func TestUsageErrors(t *testing.T) {
	cases := [][]string{
		{},
		{"bogus"},
		{"gen"},
		{"keygen", "-priv", "x"},
		{"sign", "-in", "m", "-key", "a", "-key-env", "b"},
		{"sign", "-in", "m"},
		{"verify", "-pub", "p"},
		{"hash"},
		{"hash", "--no-such-flag"},
		{"keygen", "-priv", "a", "-pub", "b", "extra"},
	}
	for _, args := range cases {
		if code, out := call(args...); code != 2 {
			t.Errorf("%v: exit %d, want 2 (%s)", args, code, out)
		}
	}
	if code, _ := call("help"); code != 0 {
		t.Errorf("help: exit %d", code)
	}
}

func TestGenRejectsAmbiguousArtifacts(t *testing.T) {
	art := t.TempDir()
	mustWrite(t, filepath.Join(art, "hqq-test_linux_amd64"), "a")
	mustWrite(t, filepath.Join(art, "hqq-test_linux_amd64.exe"), "b")
	out := filepath.Join(t.TempDir(), "m.json")
	if code, msg := call("gen", "-dir", art, "-name", "hqq-test", "-release", "v1", "-out", out); code == 0 {
		t.Fatalf("duplicate platform accepted: %s", msg)
	}
}
