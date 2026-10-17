package secureexec

import (
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHashFileMatchesSHA256(t *testing.T) {
	data := []byte("hello, secureexec")
	path := filepath.Join(t.TempDir(), "data")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	got, size, err := HashFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := sha256.Sum256(data); got != Hash(want) {
		t.Fatalf("HashFile = %s, want %x", got, want)
	}
	if size != int64(len(data)) {
		t.Fatalf("size = %d, want %d", size, len(data))
	}
	if _, _, err := HashFile(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("HashFile of a missing file succeeded")
	}
}

func TestParseHash(t *testing.T) {
	sum := sha256.Sum256([]byte("x"))
	text := Hash(sum).String()
	if len(text) != 64 || strings.ToLower(text) != text {
		t.Fatalf("String() = %q, want 64 lowercase hex digits", text)
	}
	got, err := ParseHash(text)
	if err != nil || got != Hash(sum) {
		t.Fatalf("ParseHash round trip: %v, %s", err, got)
	}
	if got, err := ParseHash(strings.ToUpper(text)); err != nil || got != Hash(sum) {
		t.Fatalf("ParseHash upper-case: %v", err)
	}
	for _, bad := range []string{"", text[:63], text + "0", strings.Repeat("g", 64)} {
		if _, err := ParseHash(bad); err == nil {
			t.Errorf("ParseHash(%q) succeeded", bad)
		}
	}
}

func TestHashJSON(t *testing.T) {
	sum := sha256.Sum256([]byte("json"))
	type wrap struct {
		H Hash `json:"h"`
	}
	b, err := json.Marshal(wrap{H: Hash(sum)})
	if err != nil {
		t.Fatal(err)
	}
	var back wrap
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.H != Hash(sum) {
		t.Fatalf("round trip = %s", back.H)
	}
	if err := json.Unmarshal([]byte(`{"h":"nope"}`), &back); err == nil {
		t.Fatal("bad digest accepted")
	}
}
