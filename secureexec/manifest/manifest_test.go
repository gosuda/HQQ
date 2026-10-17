package manifest

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gosuda.org/hqq/secureexec"
)

func sampleManifest() *Manifest {
	sum := sha256.Sum256([]byte("artifact"))
	return &Manifest{
		Version: Version,
		Name:    "hqq-test",
		Release: "v1.2.3",
		Files: []File{
			{OS: "linux", Arch: "amd64", Name: "hqq-test_linux_amd64", SHA256: secureexec.Hash(sum), Size: 8},
			{OS: "windows", Arch: "amd64", Name: "hqq-test_windows_amd64.exe", SHA256: secureexec.Hash(sum), Size: 8},
		},
		Licenses: []string{"MIT"},
	}
}

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func TestSignVerifyRoundTrip(t *testing.T) {
	pub, priv := newKey(t)
	data, err := Marshal(sampleManifest())
	if err != nil {
		t.Fatal(err)
	}
	sig, err := Sign(data, priv)
	if err != nil {
		t.Fatal(err)
	}
	m, err := Verify(data, sig, pub)
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "hqq-test" || m.Release != "v1.2.3" || len(m.Files) != 2 {
		t.Fatalf("parsed manifest = %+v", m)
	}
	if f, ok := m.Find("linux", "amd64"); !ok || f.Name != "hqq-test_linux_amd64" {
		t.Fatalf("Find(linux, amd64) = %+v, %v", f, ok)
	}
	if _, ok := m.Find("linux", "arm64"); ok {
		t.Fatal("Find returned an entry for an absent platform")
	}
}

func TestVerifyRejectsTamperedManifest(t *testing.T) {
	pub, priv := newKey(t)
	data, _ := Marshal(sampleManifest())
	sig, _ := Sign(data, priv)
	tampered := bytes.Replace(data, []byte(`"v1.2.3"`), []byte(`"v9.9.9"`), 1)
	if bytes.Equal(tampered, data) {
		t.Fatal("tamper did not change the data")
	}
	if _, err := Verify(tampered, sig, pub); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("tampered manifest: err = %v, want ErrBadSignature", err)
	}
}

func TestVerifyRejectsWrongKeyAndMalformedSignature(t *testing.T) {
	_, priv := newKey(t)
	otherPub, _ := newKey(t)
	data, _ := Marshal(sampleManifest())
	sig, _ := Sign(data, priv)
	if _, err := Verify(data, sig, otherPub); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("wrong key: err = %v, want ErrBadSignature", err)
	}
	pub := priv.Public().(ed25519.PublicKey)
	for _, bad := range [][]byte{nil, []byte("zz\n"), []byte("00\n")} {
		if _, err := Verify(data, bad, pub); !errors.Is(err, ErrBadSignature) {
			t.Errorf("malformed %q: err = %v, want ErrBadSignature", bad, err)
		}
	}
	flipped := append([]byte(nil), sig...)
	flipped[0] ^= 1 // still valid hex, but a different signature
	if _, err := Verify(data, flipped, pub); err == nil {
		t.Fatal("altered signature accepted")
	}
	if _, err := Verify(data, sig, pub[:5]); err == nil {
		t.Fatal("short public key accepted")
	}
}

func TestParseRejects(t *testing.T) {
	good, _ := Marshal(sampleManifest())
	cases := map[string]string{
		"unknown version": strings.Replace(string(good), `"version": 1`, `"version": 2`, 1),
		"missing version": `{"name":"x","release":"r","files":[{"os":"linux","arch":"amd64","name":"x_linux_amd64","sha256":"` + strings.Repeat("0", 64) + `","size":1}],"licenses":[]}`,
		"unknown field":   strings.Replace(string(good), `"name": "hqq-test",`, `"name": "hqq-test", "extra": 1,`, 1),
		"trailing data":   string(good) + "{}",
		"bad sha256":      strings.Replace(string(good), sampleHex(), "zz", 1),
		"path in name":    strings.Replace(string(good), `"hqq-test_linux_amd64"`, `"../hqq-test_linux_amd64"`, 1),
		"duplicate arch":  strings.Replace(string(good), `"os": "windows"`, `"os": "linux"`, 1),
		"negative size":   strings.Replace(string(good), `"size": 8`, `"size": -8`, 1),
		"no files":        `{"version":1,"name":"x","release":"r","files":[],"licenses":[]}`,
		"not json":        "version: 1",
	}
	for name, doc := range cases {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: Parse accepted %q", name, doc)
		}
	}
	if _, err := Parse(good); err != nil {
		t.Fatalf("good manifest rejected: %v", err)
	}
}

// sampleHex is the digest text of sampleManifest's first file.
func sampleHex() string {
	return sampleManifest().Files[0].SHA256.String()
}

func TestKeyFormatting(t *testing.T) {
	pub, priv := newKey(t)
	priv2, err := ParsePrivateKey(FormatPrivateKey(priv))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(priv2, priv) {
		t.Fatal("private key round trip changed the key")
	}
	pub2, err := ParsePublicKey(FormatPublicKey(pub))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pub2, pub) {
		t.Fatal("public key round trip changed the key")
	}
	if _, err := ParsePublicKey([]byte("abcd")); err == nil {
		t.Fatal("short public key accepted")
	}
	if _, err := ParsePrivateKey([]byte("not hex")); err == nil {
		t.Fatal("bad private key accepted")
	}
}

func TestScanAndVerifyFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("hqq-test_linux_amd64", "linux-bin")
	write("hqq-test_windows_amd64.exe", "win-bin")
	write("README.md", "ignored: other name")
	write("other_linux_amd64", "ignored: other product")

	m, err := Scan(dir, "hqq-test", "v1", []string{"MIT"})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Files) != 2 {
		t.Fatalf("files = %+v", m.Files)
	}
	if f, ok := m.Find("windows", "amd64"); !ok || f.Name != "hqq-test_windows_amd64.exe" || f.Size != int64(len("win-bin")) {
		t.Fatalf("windows entry = %+v, %v", f, ok)
	}
	if err := VerifyFiles(dir, m); err != nil {
		t.Fatalf("VerifyFiles on untouched artifacts: %v", err)
	}

	write("hqq-test_linux_amd64", "linux-bi!")
	if err := VerifyFiles(dir, m); !errors.Is(err, secureexec.ErrHashMismatch) {
		t.Fatalf("tampered artifact: err = %v, want ErrHashMismatch", err)
	}
	write("hqq-test_linux_amd64", "linux-bin-longer")
	if err := VerifyFiles(dir, m); err == nil {
		t.Fatal("size change not detected")
	}
	if err := os.Remove(filepath.Join(dir, "hqq-test_windows_amd64.exe")); err != nil {
		t.Fatal(err)
	}
	if err := VerifyFiles(dir, m); err == nil {
		t.Fatal("missing artifact not detected")
	}
}

func TestScanRejectsBadNames(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hqq-test_linux"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Scan(dir, "hqq-test", "v1", nil); err == nil {
		t.Fatal("artifact without an arch accepted")
	}
	empty := t.TempDir()
	if _, err := Scan(empty, "hqq-test", "v1", nil); err == nil {
		t.Fatal("empty directory accepted")
	}
}

func TestScanRefusesSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows")
	}
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "hqq-test_linux_amd64")); err != nil {
		t.Fatal(err)
	}
	if _, err := Scan(dir, "hqq-test", "v1", nil); err == nil {
		t.Fatal("symlinked artifact accepted")
	}
}
