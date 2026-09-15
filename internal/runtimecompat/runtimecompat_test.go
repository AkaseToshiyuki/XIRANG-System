package runtimecompat

import "testing"

func TestCompatible(t *testing.T) {
	facts := map[string]string{"os": "linux", "arch": "amd64", "libc.version": "2.39", "kernel.version": "6.8.0-40"}
	for name, test := range map[string]struct {
		requirements map[string]string
		want         bool
	}{
		"empty legacy contract": {nil, true},
		"exact and minimum":     {map[string]string{"os": "linux", "libc.version": ">=2.17"}, true},
		"padded version":        {map[string]string{"kernel.version": ">=6.8"}, true},
		"version mismatch":      {map[string]string{"libc.version": ">=2.40"}, false},
		"exact mismatch":        {map[string]string{"arch": "arm64"}, false},
		"missing fact":          {map[string]string{"driver.nvidia": ">=550.0"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			got, _ := Compatible(test.requirements, facts)
			if got != test.want {
				t.Fatalf("Compatible() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestValidateRequirementsRejectsInvalidOrderedVersion(t *testing.T) {
	if err := ValidateRequirements(map[string]string{"libc.version": ">=latest"}); err == nil {
		t.Fatal("expected malformed version requirement to be rejected")
	}
}
