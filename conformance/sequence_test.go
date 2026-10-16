package conformance

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"gosuda.org/hqq"
)

// TestGoldenSequence (G-06) replays the six steps of the golden sequence on
// caller memory and compares every normalized snapshot with the committed
// bytes, the normalized word list and the epoch. Only the host page family
// has a sequence; without one the test is skipped.
func TestGoldenSequence(t *testing.T) {
	page := hostPage()
	dir := filepath.Join(vectorsDir, "sequences", strconv.Itoa(page))
	raw, err := os.ReadFile(filepath.Join(dir, "sequence.json"))
	if errors.Is(err, fs.ErrNotExist) {
		t.Skipf("no golden sequence for page family %d under %s", page, dir)
	}
	if err != nil {
		t.Fatal(err)
	}
	var seq GoldenSequence
	if err := json.Unmarshal(raw, &seq); err != nil {
		t.Fatalf("decode sequence.json: %v", err)
	}
	if seq.PageSize != page {
		t.Fatalf("sequence page size %d, host %d", seq.PageSize, page)
	}
	total := int(hqq.SizeStandardLink(seq.BufferCount, seq.BufferSize))
	if total != seq.LayoutSize {
		t.Fatalf("layout size %d, sequence says %d", total, seq.LayoutSize)
	}
	if len(seq.Steps) != len(goldenStepNames) {
		t.Fatalf("sequence has %d steps, want %d", len(seq.Steps), len(goldenStepNames))
	}
	mem := hqq.AlignedBuffer(uintptr(total))
	err = runGoldenSequence(mem, seq.BufferCount, seq.BufferSize, func(step int) error {
		st := seq.Steps[step]
		want := readVector(t, st.File)
		got := append([]byte(nil), mem...)
		words := normalizeSegment(got)
		if !bytes.Equal(got, want) {
			t.Errorf("step %s: snapshot differs from %s at byte %d", st.Name, st.File, firstDiffOutside(got, want, 0, 0))
		}
		if !slices.Equal(words, st.Normalized) {
			t.Errorf("step %s: normalized words %v, want %v", st.Name, words, st.Normalized)
		}
		if ep := binary.LittleEndian.Uint64(got[SBEpoch:]); ep != st.Epoch {
			t.Errorf("step %s: epoch %d, want %d", st.Name, ep, st.Epoch)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
