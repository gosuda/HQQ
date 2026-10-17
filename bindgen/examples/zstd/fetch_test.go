package zstd_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// repoRoot is the repository root, two levels above bindgen/examples/zstd.
const repoRoot = "../../.."

// pinnedVersion reads version= from third_party/zstd/VERSION.
func pinnedVersion(t testing.TB) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot, "third_party", "zstd", "VERSION"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, "version="); ok {
			return v
		}
	}
	t.Fatal("third_party/zstd/VERSION has no version= line")
	return ""
}

func TestPinFilesAreConsistent(t *testing.T) {
	pins := filepath.Join(repoRoot, "third_party", "zstd")
	version := pinnedVersion(t)
	vdata, err := os.ReadFile(filepath.Join(pins, "VERSION"))
	if err != nil {
		t.Fatal(err)
	}
	archive := "zstd-" + version + ".tar.gz"
	for _, want := range []string{"archive=" + archive, "url=https://github.com/facebook/zstd/releases/download/v" + version + "/" + archive} {
		if !strings.Contains(string(vdata), want+"\n") {
			t.Errorf("VERSION lacks the line %q", want)
		}
	}
	sums, err := os.ReadFile(filepath.Join(pins, "SHA256"))
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(sums))
	if len(fields) != 2 || fields[1] != archive || len(fields[0]) != 64 {
		t.Fatalf("SHA256 must hold one digest for %s, got %q", archive, sums)
	}
	if _, err := hex.DecodeString(fields[0]); err != nil {
		t.Errorf("SHA256 digest is not hex: %v", err)
	}
	lic, err := os.ReadFile(filepath.Join(pins, "LICENSE"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"BSD License", "Redistributions of source code must retain the above copyright notice", "Neither the name Facebook, nor Meta"} {
		if !strings.Contains(string(lic), want) {
			t.Errorf("third_party/zstd/LICENSE lacks %q: it must be the BSD text of the release", want)
		}
	}
}

// fetchFixture is a copy of the script in a scratch repository layout, so that the test can pin its
// own archives: scripts/release/fetch-zstd.sh reads third_party/zstd relative to itself.
type fetchFixture struct {
	root    string
	archive string // name pinned in VERSION
}

func newFetchFixture(t *testing.T, pinnedSum string, license string) *fetchFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fetch-zstd.sh is a POSIX shell script")
	}
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("tar is required")
	}
	root := t.TempDir()
	script, err := os.ReadFile(filepath.Join(repoRoot, "scripts", "release", "fetch-zstd.sh"))
	if err != nil {
		t.Fatal(err)
	}
	write := func(rel string, data string, mode os.FileMode) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), mode); err != nil {
			t.Fatal(err)
		}
	}
	const name = "zstd-9.9.9.tar.gz"
	write("scripts/release/fetch-zstd.sh", string(script), 0o755)
	write("third_party/zstd/VERSION", "version=9.9.9\narchive="+name+"\nurl=https://example.invalid/"+name+"\n", 0o644)
	write("third_party/zstd/SHA256", pinnedSum+"  "+name+"\n", 0o644)
	write("third_party/zstd/LICENSE", license, 0o644)
	return &fetchFixture{root: root, archive: name}
}

// run executes the script with args and returns stdout, stderr and the exit error.
func (f *fetchFixture) run(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	cmd := exec.Command("sh", append([]string{filepath.Join(f.root, "scripts", "release", "fetch-zstd.sh")}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

type member struct{ name, body string }

// makeArchive returns a tar.gz of members under the directory zstd-9.9.9 and its SHA-256.
func makeArchive(t *testing.T, members []member) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, m := range members {
		hdr := &tar.Header{Name: m.name, Mode: 0o644, Size: int64(len(m.body)), Typeflag: tar.TypeReg}
		if strings.HasSuffix(m.name, "/") {
			hdr.Typeflag, hdr.Mode, hdr.Size = tar.TypeDir, 0o755, 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(m.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), hex.EncodeToString(sum[:])
}

const fixtureLicense = "BSD License\nfixture text\n"

func goodMembers() []member {
	return []member{
		{"zstd-9.9.9/", ""},
		{"zstd-9.9.9/LICENSE", fixtureLicense},
		{"zstd-9.9.9/lib/", ""},
		{"zstd-9.9.9/lib/zstd.h", "/* zstd.h */\n"},
		{"zstd-9.9.9/lib/zstd_errors.h", "/* zstd_errors.h */\n"},
		{"zstd-9.9.9/lib/compress/", ""},
		{"zstd-9.9.9/lib/compress/zstd_compress.c", "/* c */\n"},
		{"zstd-9.9.9/lib/decompress/", ""},
		{"zstd-9.9.9/lib/decompress/zstd_decompress.c", "/* d */\n"},
	}
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func notUnpacked(t *testing.T, dest string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dest, "zstd-9.9.9")); !os.IsNotExist(err) {
		t.Errorf("a refused archive left %s/zstd-9.9.9 behind (stat error %v)", dest, err)
	}
}

func TestFetchUnpacksArchiveThatMatchesThePin(t *testing.T) {
	data, sum := makeArchive(t, goodMembers())
	f := newFetchFixture(t, sum, fixtureLicense)
	arc := filepath.Join(t.TempDir(), "copy.tar.gz")
	writeFile(t, arc, data)
	dest := t.TempDir()
	stdout, stderr, err := f.run(t, "-a", arc, "-d", dest)
	if err != nil {
		t.Fatalf("fetch-zstd.sh: %v\n%s", err, stderr)
	}
	dir := strings.TrimSpace(stdout)
	if want := filepath.Join(dest, "zstd-9.9.9"); dir != want {
		t.Errorf("stdout = %q, want only the source directory %q", dir, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "lib", "zstd.h")); err != nil {
		t.Errorf("unpacked tree has no lib/zstd.h: %v", err)
	}
}

func TestFetchRefusesCorruptedArchive(t *testing.T) {
	data, sum := makeArchive(t, goodMembers())
	f := newFetchFixture(t, sum, fixtureLicense)
	corrupted := bytes.Clone(data)
	corrupted[len(corrupted)/2] ^= 0x01
	arc := filepath.Join(t.TempDir(), "corrupted.tar.gz")
	writeFile(t, arc, corrupted)
	dest := t.TempDir()
	stdout, stderr, err := f.run(t, "-a", arc, "-d", dest)
	if err == nil {
		t.Fatalf("a corrupted archive was accepted; stdout %q", stdout)
	}
	if !strings.Contains(stderr, "checksum mismatch") {
		t.Errorf("stderr does not name the checksum mismatch:\n%s", stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q on refusal, want nothing", stdout)
	}
	notUnpacked(t, dest)
}

func TestFetchRefusesArchiveWithAnotherDigestThanThePin(t *testing.T) {
	data, _ := makeArchive(t, goodMembers())
	f := newFetchFixture(t, strings.Repeat("0", 64), fixtureLicense)
	arc := filepath.Join(t.TempDir(), "a.tar.gz")
	writeFile(t, arc, data)
	dest := t.TempDir()
	_, stderr, err := f.run(t, "-a", arc, "-d", dest)
	if err == nil || !strings.Contains(stderr, "checksum mismatch") {
		t.Fatalf("err = %v, stderr:\n%s", err, stderr)
	}
	notUnpacked(t, dest)
}

func TestFetchRefusesLicenseThatDiffersFromThePinnedCopy(t *testing.T) {
	data, sum := makeArchive(t, goodMembers())
	f := newFetchFixture(t, sum, fixtureLicense+"an edit\n")
	arc := filepath.Join(t.TempDir(), "a.tar.gz")
	writeFile(t, arc, data)
	dest := t.TempDir()
	_, stderr, err := f.run(t, "-a", arc, "-d", dest)
	if err == nil || !strings.Contains(stderr, "LICENSE") {
		t.Fatalf("err = %v, stderr:\n%s", err, stderr)
	}
	notUnpacked(t, dest)
}

func TestFetchRefusesMembersThatEscapeTheUnpackDirectory(t *testing.T) {
	data, sum := makeArchive(t, append(goodMembers(), member{"zstd-9.9.9/../../escape.txt", "x"}))
	f := newFetchFixture(t, sum, fixtureLicense)
	arc := filepath.Join(t.TempDir(), "a.tar.gz")
	writeFile(t, arc, data)
	dest := t.TempDir()
	_, stderr, err := f.run(t, "-a", arc, "-d", dest)
	if err == nil || !strings.Contains(stderr, "parent-relative") {
		t.Fatalf("err = %v, stderr:\n%s", err, stderr)
	}
	notUnpacked(t, dest)
}

func TestFetchRefusesArchiveWithoutTheSourceTree(t *testing.T) {
	data, sum := makeArchive(t, []member{{"zstd-9.9.9/", ""}, {"zstd-9.9.9/LICENSE", fixtureLicense}})
	f := newFetchFixture(t, sum, fixtureLicense)
	arc := filepath.Join(t.TempDir(), "a.tar.gz")
	writeFile(t, arc, data)
	dest := t.TempDir()
	_, stderr, err := f.run(t, "-a", arc, "-d", dest)
	if err == nil || !strings.Contains(stderr, "missing lib/") {
		t.Fatalf("err = %v, stderr:\n%s", err, stderr)
	}
	notUnpacked(t, dest)
}

func TestFetchRefusesToUnpackInsideTheRepository(t *testing.T) {
	data, sum := makeArchive(t, goodMembers())
	f := newFetchFixture(t, sum, fixtureLicense)
	arc := filepath.Join(t.TempDir(), "a.tar.gz")
	writeFile(t, arc, data)
	dest := filepath.Join(f.root, "inside")
	_, stderr, err := f.run(t, "-a", arc, "-d", dest)
	if err == nil || !strings.Contains(stderr, "inside the repository") {
		t.Fatalf("err = %v, stderr:\n%s", err, stderr)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Errorf("the refused destination %s was created (stat error %v)", dest, err)
	}
}
