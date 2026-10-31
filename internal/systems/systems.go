// Package systems reads the systems folder: one subfolder per system,
// holding its schedules.yaml and system.pub (PX-SCH-01, PX-SYS-01,
// PX-SYS-02). It is read once at start; changes need a restart
// (PX-SYS-05). Any fault stops the start, and every fault is reported.
package systems

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/Rumeasiyan/paradox/internal/schedule"
	"github.com/Rumeasiyan/paradox/sign"
)

// File names inside each system's subfolder.
const (
	ScheduleFile  = "schedules.yaml"
	PublicKeyFile = "system.pub"
)

// System is one system's schedule plus the public keys it signs with.
type System struct {
	schedule.System
	Keys []ed25519.PublicKey // one, or two during a key change (PX-SIG-06)
}

// Options carry what the folder alone cannot say.
type Options struct {
	Defaults schedule.Defaults
	// SablierConfigured is false when PARADOX_SABLIER_URL is empty; then no
	// system may ask to be woken, or its jobs could never run.
	SablierConfigured bool
}

// Load reads every system from dir, which is the only folder PARADOX reads
// (pass os.DirFS(cfg.SystemsDir)). Systems come back sorted by name.
func Load(dir fs.FS, opt Options) ([]System, error) {
	entries, err := fs.ReadDir(dir, ".")
	if err != nil {
		return nil, fmt.Errorf("systems folder: %w", err)
	}
	var out []System
	var errs []error
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue // editor and OS files (.DS_Store, .swp)
		}
		if !e.IsDir() {
			errs = append(errs, fmt.Errorf("%s: unexpected file; each system is a subfolder holding %s and %s", name, ScheduleFile, PublicKeyFile))
			continue
		}
		s, err := loadOne(dir, name, opt)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, s)
	}
	if len(out) == 0 && len(errs) == 0 {
		errs = append(errs, errors.New("systems folder holds no systems; check that it is mounted"))
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func loadOne(dir fs.FS, folder string, opt Options) (System, error) {
	schedPath := folder + "/" + ScheduleFile
	keyPath := folder + "/" + PublicKeyFile
	var errs []error

	var s System
	data, err := fs.ReadFile(dir, schedPath)
	if err != nil {
		errs = append(errs, fmt.Errorf("%s: missing or unreadable", schedPath))
	} else if parsed, err := schedule.Parse(schedPath, data, opt.Defaults); err != nil {
		errs = append(errs, err)
	} else {
		s.System = parsed
		if parsed.Name != folder {
			errs = append(errs, fmt.Errorf("%s: system is %q but the folder is %q; they must match", schedPath, parsed.Name, folder))
		}
		if parsed.Wake == schedule.WakeSablier && !opt.SablierConfigured {
			errs = append(errs, fmt.Errorf("%s: wake is sablier but PARADOX_SABLIER_URL is empty", schedPath))
		}
	}

	keyText, err := fs.ReadFile(dir, keyPath)
	if err != nil {
		errs = append(errs, fmt.Errorf("%s: missing or unreadable", keyPath))
	} else if keys, err := sign.ParsePublicKeys(string(keyText)); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", keyPath, err))
	} else {
		s.Keys = keys
	}

	return s, errors.Join(errs...)
}

// Index finds systems by name, for checking who signed a request.
type Index map[string]System

// NewIndex builds an Index from loaded systems.
func NewIndex(list []System) Index {
	idx := make(Index, len(list))
	for _, s := range list {
		idx[s.Name] = s
	}
	return idx
}

// Keys returns the public keys a system signs with, or nil for an unknown
// system. Its shape fits sign.Verifier.Keys.
func (idx Index) Keys(name string) []ed25519.PublicKey {
	return idx[name].Keys
}
