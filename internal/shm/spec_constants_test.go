package shm

// Executable cross-check between spec/constants.json and the Windows naming
// rules of winnames.go (pure logic, so it runs on every platform).

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestSpecWindowsNaming(t *testing.T) {
	raw, err := os.ReadFile("../../spec/constants.json")
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Platform struct {
			Windows struct {
				DefaultPrefix   string   `json:"default_prefix"`
				MaxObjectName   int      `json:"max_object_name_utf16"`
				SemaphoreCount  int      `json:"semaphore_count"`
				SemaphoreSuffix []string `json:"semaphore_suffixes"`
				NameVectors     []struct {
					Name   string `json:"name"`
					Object string `json:"object"`
				} `json:"name_vectors"`
				InvalidNames []string `json:"invalid_name_vectors"`
			} `json:"windows"`
		} `json:"platform"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	w := c.Platform.Windows
	if got := SemaphoreCount; got != w.SemaphoreCount {
		t.Fatalf("semaphore count = %d, spec says %d", got, w.SemaphoreCount)
	}
	if got := len(semaphoreSuffixes); got != w.SemaphoreCount {
		t.Fatalf("number of semaphore suffixes = %d, spec says %d", got, w.SemaphoreCount)
	}
	for i, s := range w.SemaphoreSuffix {
		if semaphoreSuffixes[i] != s {
			t.Errorf("semaphore suffix %d = %q, spec says %q", i, semaphoreSuffixes[i], s)
		}
	}
	if maxObjectName != w.MaxObjectName {
		t.Errorf("maxObjectName = %d, spec says %d", maxObjectName, w.MaxObjectName)
	}
	for _, v := range w.NameVectors {
		got, err := windowsSharedMemoryName(v.Name)
		if err != nil || got != v.Object {
			t.Errorf("windowsSharedMemoryName(%q) = %q, %v; spec says %q", v.Name, got, err, v.Object)
		}
	}
	for _, name := range w.InvalidNames {
		if _, err := windowsSharedMemoryName(name); !errors.Is(err, ErrInvalidName) {
			t.Errorf("windowsSharedMemoryName(%q) = %v, want ErrInvalidName", name, err)
		}
	}
	// Length rule: the longest semaphore name must fit the kernel limit.
	longest := 0
	for _, s := range w.SemaphoreSuffix {
		longest = max(longest, len(s))
	}
	fit := w.MaxObjectName - longest - len(w.DefaultPrefix)
	if _, err := windowsSharedMemoryName(strings.Repeat("x", fit)); err != nil {
		t.Errorf("a %d-character name must be valid: %v", fit, err)
	}
	if _, err := windowsSharedMemoryName(strings.Repeat("x", fit+1)); !errors.Is(err, ErrInvalidName) {
		t.Errorf("a %d-character name must be invalid, got %v", fit+1, err)
	}
}
