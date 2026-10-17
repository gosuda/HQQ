package main

import "testing"

// TestStdOptionsExclusiveFromEnvironment: HQQ_INTEROP_EXCLUSIVE=1 opens the
// Standard endpoints in exclusive mode; any other value leaves the default.
func TestStdOptionsExclusiveFromEnvironment(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"", false},
		{"0", false},
		{"true", false},
		{"1", true},
	} {
		t.Setenv(envExclusive, tc.value)
		if got := stdOptions(config{}).Exclusive; got != tc.want {
			t.Errorf("%s=%q: Exclusive = %v, want %v", envExclusive, tc.value, got, tc.want)
		}
	}
}
