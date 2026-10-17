package gen

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"gosuda.org/hqq/bindgen/idl"
)

var updateCGolden = flag.Bool("update-c", false, "rewrite the golden C files under testdata/c/golden")

// repoRoot is the module root, found from the package directory.
func repoRoot(t testing.TB) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
		if d == filepath.Dir(d) {
			t.Fatal("go.mod not found above " + dir)
		}
	}
}

// parseSpec parses an IDL file given relative to the repository root.
func parseSpec(t testing.TB, rel string) *idl.Spec {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	spec, err := idl.Parse(filepath.Base(rel), data, idl.Options{})
	if err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	return spec
}

// readFile reads a file given relative to the repository root.
func readFile(t testing.TB, rel string) (string, error) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), filepath.FromSlash(rel)))
	return string(data), err
}

// parseText parses an IDL text.
func parseText(t testing.TB, src string) *idl.Spec {
	t.Helper()
	spec, err := idl.Parse("test.idl", []byte(src), idl.Options{})
	if err != nil {
		t.Fatalf("parse: %v\n%s", err, src)
	}
	return spec
}

// genC runs the emitter and returns the files by name.
func genC(t testing.TB, spec *idl.Spec, source string) map[string][]byte {
	t.Helper()
	files, err := C(spec, COptions{Source: source})
	if err != nil {
		t.Fatalf("C: %v", err)
	}
	out := map[string][]byte{}
	for _, f := range files {
		out[f.Name] = f.Data
	}
	return out
}

// writeFiles writes generated files into dir.
func writeFiles(t testing.TB, dir string, files map[string][]byte) {
	t.Helper()
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// ---- the C toolchain --------------------------------------------------------------------------------

// strictFlags are the flags of 11.2 for generated C.
var strictFlags = []string{"-std=c11", "-Wall", "-Wextra", "-Wpedantic", "-Werror"}

func needCC(t testing.TB) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("skipped: the C server targets POSIX (BINDGEN.md 1.2)")
	}
	name := "cc"
	if env := strings.Fields(os.Getenv("CC")); len(env) == 1 {
		name = env[0] // CMake honors CC too, so the library and the glue use one compiler
	}
	cc, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("skipped: no C compiler (%s) on PATH", name)
	}
	return cc
}

// runCmd runs a command and fails the test with its output.
func runCmd(t testing.TB, dir string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out.String())
	}
	return out.String()
}

var (
	sanOnce sync.Once
	sanFlag []string
)

// sanitizerFlags are the flags for ASan and UBSan, or nil (with a log line) if the compiler cannot build them.
func sanitizerFlags(t testing.TB) []string {
	t.Helper()
	cc := needCC(t)
	sanOnce.Do(func() {
		dir, err := os.MkdirTemp("", "hqq-san-probe")
		if err != nil {
			return
		}
		defer os.RemoveAll(dir)
		src := filepath.Join(dir, "p.c")
		if os.WriteFile(src, []byte("int main(void) { return 0; }\n"), 0o644) != nil {
			return
		}
		flags := []string{"-g", "-O1", "-fsanitize=address,undefined", "-fno-sanitize-recover=undefined", "-fno-omit-frame-pointer"}
		cmd := exec.Command(cc, append(append([]string{}, flags...), "-o", filepath.Join(dir, "p"), src)...)
		if cmd.Run() == nil && exec.Command(filepath.Join(dir, "p")).Run() == nil {
			sanFlag = flags
		}
	})
	if sanFlag == nil {
		t.Log("the C compiler cannot build ASan+UBSan programs here; running without them")
	}
	return sanFlag
}

var (
	libMu    sync.Mutex
	libCache = map[bool]string{}
)

// runtimeLib builds the C library of the repository with CMake and returns the path of libhqq.a. The build
// directory is HQQ_BINDGEN_GEN_CLIB (a directory that contains a built libhqq.a) or a directory under the
// temporary directory. With sanitize the library is built with ASan and UBSan.
func runtimeLib(t testing.TB, sanitize bool) string {
	t.Helper()
	needCC(t)
	root := repoRoot(t)
	if _, err := os.Stat(filepath.Join(root, "c", "src", "hqq_rpc.c")); err != nil {
		t.Skip("skipped: the C runtime c/src/hqq_rpc.c does not exist")
	}
	if dir := os.Getenv("HQQ_BINDGEN_GEN_CLIB"); dir != "" {
		lib := filepath.Join(dir, "libhqq.a")
		if _, err := os.Stat(lib); err != nil {
			t.Skipf("skipped: HQQ_BINDGEN_GEN_CLIB=%s has no libhqq.a", dir)
		}
		return lib
	}
	libMu.Lock()
	defer libMu.Unlock()
	if lib, ok := libCache[sanitize]; ok {
		return lib
	}
	cmake, err := exec.LookPath("cmake")
	if err != nil {
		t.Skip("skipped: cmake not found, cannot build the C runtime")
	}
	ninja, err := exec.LookPath("ninja")
	if err != nil {
		t.Skip("skipped: ninja not found, cannot build the C runtime")
	}
	_ = ninja
	name := "hqqc-emit"
	args := []string{"-S", filepath.Join(root, "c"), "-G", "Ninja", "-DCMAKE_BUILD_TYPE=Debug",
		"-DHQQ_BUILD_TESTS=OFF", "-DHQQ_BUILD_STRESS=OFF", "-DHQQ_BUILD_CXX_CHECK=OFF"}
	if sanitize {
		name += "-san"
		args = append(args, "-DHQQ_SANITIZE=address+undefined")
	}
	dir := filepath.Join(os.TempDir(), name)
	runCmd(t, root, cmake, append(args, "-B", dir)...)
	runCmd(t, root, cmake, "--build", dir, "--target", "hqq")
	lib := filepath.Join(dir, "libhqq.a")
	if _, err := os.Stat(lib); err != nil {
		t.Fatalf("the C library build produced no %s", lib)
	}
	libCache[sanitize] = lib
	return lib
}

// buildProgram compiles the glue sources strictly, the driver with relaxed flags, and links them with the
// runtime. It returns the path of the executable. sources are paths; defines are -D options.
func buildProgram(t testing.TB, dir string, includes []string, defines []string, strict, relaxed []string) string {
	t.Helper()
	cc := needCC(t)
	san := sanitizerFlags(t)
	lib := runtimeLib(t, san != nil)
	root := repoRoot(t)

	common := append([]string{}, san...)
	if san == nil {
		common = []string{"-g", "-O1"}
	}
	common = append(common, "-I"+filepath.Join(root, "c", "include"), "-I"+dir)
	for _, inc := range includes {
		common = append(common, "-I"+inc)
	}
	for _, d := range defines {
		common = append(common, "-D"+d)
	}
	var objs []string
	build := func(src string, flags []string) {
		obj := filepath.Join(dir, strings.TrimSuffix(filepath.Base(src), ".c")+".o")
		args := append(append(append([]string{}, flags...), common...), "-c", src, "-o", obj)
		runCmd(t, dir, cc, args...)
		objs = append(objs, obj)
	}
	for _, s := range strict {
		build(s, strictFlags)
	}
	for _, s := range relaxed {
		build(s, []string{"-std=c11", "-Wall", "-Wextra"})
	}
	exe := filepath.Join(dir, "prog")
	args := append(append([]string{}, san...), objs...)
	args = append(args, lib, "-lpthread", "-o", exe)
	runCmd(t, dir, cc, args...)
	return exe
}

// ---- frames -------------------------------------------------------------------------------------------

// frame is a decoded server frame.
type frame struct {
	kind, flags byte
	op          uint16
	id, total   uint32
	aux         uint32
	body        []byte
}

func le32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

func putLE32(b []byte, v uint32) {
	b[0], b[1], b[2], b[3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
}

// parseFrames splits the output of one exchange into frames.
func parseFrames(t testing.TB, b []byte) []frame {
	t.Helper()
	var fs []frame
	for len(b) > 0 {
		if len(b) < 16 {
			t.Fatalf("short frame: %x", b)
		}
		f := frame{kind: b[0], flags: b[1], op: uint16(b[2]) | uint16(b[3])<<8, id: le32(b[4:]), total: le32(b[8:]), aux: le32(b[12:])}
		n := int(f.total)
		if f.kind == 3 || n > len(b)-16 {
			n = len(b) - 16
		}
		f.body = append([]byte(nil), b[16:16+n]...)
		fs = append(fs, f)
		b = b[16+n:]
	}
	return fs
}

// requestFrame is a REQUEST frame with the given body.
func requestFrame(op uint16, id uint32, body []byte) []byte {
	f := make([]byte, 16+len(body))
	f[0] = 1
	f[2], f[3] = byte(op), byte(op>>8)
	putLE32(f[4:], id)
	putLE32(f[8:], uint32(len(body)))
	copy(f[16:], body)
	return f
}

// helloFrame is the HELLO of 4.11 for a digest and geometry.
func helloFrame(digest [32]byte, bufSize, bufCount uint32) []byte {
	f := make([]byte, 16+64)
	f[0] = 4
	putLE32(f[8:], 64)
	copy(f[16:], "HQBG")
	f[20] = 1
	putLE32(f[28:], bufSize)
	putLE32(f[32:], bufCount)
	putLE32(f[36:], bufCount)
	putLE32(f[40:], 1<<24)
	copy(f[48:], digest[:])
	return f
}

// session runs a driver program: it sends each line and returns the lines it prints.
type session struct {
	t    testing.TB
	exe  string
	args []string
}

// run sends the frames (hex) one per line and returns the responses: the exchange status and the bytes,
// and the trailer fields.
func (s *session) run(frames ...[]byte) (res []exchangeResult, trailer string) {
	s.t.Helper()
	var in bytes.Buffer
	for _, f := range frames {
		if len(f) > 0 && f[0] == '#' { // a driver command such as "#last 4294967294"
			in.Write(f)
		} else {
			in.WriteString(hex.EncodeToString(f))
		}
		in.WriteByte('\n')
	}
	cmd := exec.Command(s.exe, s.args...)
	cmd.Stdin = &in
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		s.t.Fatalf("%s: %v\nstdout:\n%s\nstderr:\n%s", s.exe, err, out.String(), errb.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != len(frames)+1 {
		s.t.Fatalf("driver printed %d lines for %d frames:\n%s\nstderr:\n%s", len(lines), len(frames), out.String(), errb.String())
	}
	for _, l := range lines[:len(frames)] {
		var r exchangeResult
		parts := strings.SplitN(l, " ", 2)
		if _, err := parseUint(parts[0], &r.status); err != nil {
			s.t.Fatalf("bad driver line %q", l)
		}
		if len(parts) == 2 {
			b, err := hex.DecodeString(parts[1])
			if err != nil {
				s.t.Fatalf("bad driver line %q: %v", l, err)
			}
			r.out = b
		}
		res = append(res, r)
	}
	return res, lines[len(frames)]
}

type exchangeResult struct {
	status uint
	out    []byte
}

func parseUint(s string, v *uint) (int, error) {
	var n uint
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, os.ErrInvalid
		}
		n = n*10 + uint(c-'0')
	}
	*v = n
	return len(s), nil
}

// vectorFrame is one frame of testdata/vectors.json.
type vectorFrame struct {
	Name string `json:"name"`
	ID   uint32 `json:"id"`
	Dir  string `json:"dir"`
	Hex  string `json:"hex"`
}

func (f vectorFrame) bytes(t testing.TB) []byte {
	b, err := hex.DecodeString(f.Hex)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type vectorSet struct {
	BufferSize int `json:"buffer_size"`
	Geometry   struct{ BufferSize, BufferCount int }
	Frames     []vectorFrame `json:"frames"`
}

func loadVectors(t testing.TB) map[string]vectorSet {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "bindgen", "testdata", "vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	out := map[string]vectorSet{}
	for k, v := range raw {
		var s struct {
			BufferSize int `json:"buffer_size"`
			Geometry   struct {
				BufferSize  int `json:"buffer_size"`
				BufferCount int `json:"buffer_count"`
			} `json:"geometry"`
			Frames []vectorFrame `json:"frames"`
		}
		if err := json.Unmarshal(v, &s); err != nil {
			t.Fatalf("vectors.json %s: %v", k, err)
		}
		vs := vectorSet{BufferSize: s.BufferSize, Frames: s.Frames}
		vs.Geometry.BufferSize, vs.Geometry.BufferCount = s.Geometry.BufferSize, s.Geometry.BufferCount
		out[k] = vs
	}
	return out
}
