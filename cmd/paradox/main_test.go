package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Rumeasiyan/paradox"
)

func TestVersionFlagPrintsVersionFile(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"-version"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
	if got := strings.TrimSpace(out.String()); got != paradox.Version {
		t.Fatalf("printed %q, want %q", got, paradox.Version)
	}
}

func TestNoFlagsPrintsUsageAndFails(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(nil, &out, &errOut); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "-version") {
		t.Fatalf("usage missing -version: %q", errOut.String())
	}
}

func TestUnknownFlagFails(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"-nope"}, &out, &errOut); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
}
