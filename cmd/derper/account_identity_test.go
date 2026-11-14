package main

import "testing"

// Account evidence is an explicit startup capability and cannot select a
// standalone mode or quietly enable the legacy user limiter.
func TestTailnetAccountMode(t *testing.T) {
	for _, tc := range []struct {
		name, path     string
		enabled, valid bool
	}{
		{"disabled legacy", "", false, true},
		{"disabled tailnet", "rates.json", false, true},
		{"enabled tailnet", "rates.json", true, true},
		{"standalone forbidden", "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateTailnetAccountMode(tc.path, tc.enabled)
			if (err == nil) != tc.valid {
				t.Fatal(err)
			}
		})
	}
}
