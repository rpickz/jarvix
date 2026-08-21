package build

import "testing"

func TestVersionDefaultsToDev(t *testing.T) {
	// -ldflags overrides this at build time; unlinked builds must still carry
	// a truthful, non-empty version.
	if Version != "dev" {
		t.Errorf("Version = %q, want the dev default", Version)
	}
}
