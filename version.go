// Package paradox holds what the whole module shares. Today that is only
// the version, read from the VERSION file so it has one source of truth.
package paradox

import (
	_ "embed"
	"strings"
)

//go:embed VERSION
var versionFile string

// Version is the release version from the VERSION file, for example "0.2.0".
var Version = strings.TrimSpace(versionFile)
