package paradox

import (
	"regexp"
	"testing"
)

func TestVersionIsSemantic(t *testing.T) {
	// MAJOR.MINOR.PATCH only: AGENTS.md forbids a fourth part or a build suffix.
	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(Version) {
		t.Fatalf("VERSION holds %q, want MAJOR.MINOR.PATCH", Version)
	}
}
