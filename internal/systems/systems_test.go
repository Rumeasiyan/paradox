package systems

import (
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Rumeasiyan/paradox/internal/schedule"
	"github.com/Rumeasiyan/paradox/sign"
)

var opt = Options{
	Defaults:          schedule.Defaults{Timeout: 10 * time.Minute, Retries: 3},
	SablierConfigured: true,
}

func pubKey(t *testing.T) string {
	t.Helper()
	pub, _, err := sign.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return pub
}

func file(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }

func sched(name string) *fstest.MapFile {
	return file("system: " + name + "\njobs:\n  - name: send-reminders\n    every: \"0 7 * * *\"\n")
}

func TestLoadsEverySystemSorted(t *testing.T) {
	dir := fstest.MapFS{
		"zeta/schedules.yaml":    sched("zeta"),
		"zeta/system.pub":        file(pubKey(t)),
		"citadel/schedules.yaml": sched("citadel"),
		"citadel/system.pub":     file(pubKey(t) + "\n" + pubKey(t) + "\n"),
		".DS_Store":              file("junk"),
	}
	got, err := Load(dir, opt)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "citadel" || got[1].Name != "zeta" {
		t.Fatalf("got %d systems: %+v", len(got), got)
	}
	if len(got[0].Keys) != 2 || len(got[1].Keys) != 1 {
		t.Fatalf("keys: %d and %d", len(got[0].Keys), len(got[1].Keys))
	}
	idx := NewIndex(got)
	if len(idx.Keys("citadel")) != 2 || idx.Keys("stranger") != nil {
		t.Fatal("index lookups wrong")
	}
}

func TestFaultsStopTheStartAndAreAllReported(t *testing.T) {
	_, priv, _ := sign.GenerateKey()
	dir := fstest.MapFS{
		"loose.yaml":             file("system: loose\n"),
		"nokey/schedules.yaml":   sched("nokey"),
		"nosched/system.pub":     file(pubKey(t)),
		"renamed/schedules.yaml": sched("other"),
		"renamed/system.pub":     file(pubKey(t)),
		"leaked/schedules.yaml":  sched("leaked"),
		"leaked/system.pub":      file(priv),
		"badyaml/schedules.yaml": file("system: badyaml\nwake: maybe\njobs:\n  - name: j\n"),
		"badyaml/system.pub":     file(pubKey(t)),
	}
	_, err := Load(dir, opt)
	if err == nil {
		t.Fatal("bad folder accepted")
	}
	for _, want := range []string{
		"loose.yaml: unexpected file",
		"nokey/system.pub: missing",
		"nosched/schedules.yaml: missing",
		`renamed/schedules.yaml: system is "other" but the folder is "renamed"`,
		"leaked/system.pub: line 1: this is a private key",
		"badyaml/schedules.yaml:2: wake must be",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in:\n%v", want, err)
		}
	}
	if strings.Contains(err.Error(), strings.TrimPrefix(priv, "ed25519-priv:")) {
		t.Fatal("error repeats a private key")
	}
}

func TestEmptyFolderIsRefused(t *testing.T) {
	_, err := Load(fstest.MapFS{".keep": file("")}, opt)
	if err == nil || !strings.Contains(err.Error(), "no systems") {
		t.Fatalf("got %v", err)
	}
}

func TestSablierWakeNeedsSablierURL(t *testing.T) {
	dir := fstest.MapFS{
		"citadel/schedules.yaml": sched("citadel"),
		"citadel/system.pub":     file(pubKey(t)),
		"site/schedules.yaml":    file("system: site\nwake: none\njobs:\n  - name: j\n"),
		"site/system.pub":        file(pubKey(t)),
	}
	_, err := Load(dir, Options{Defaults: opt.Defaults, SablierConfigured: false})
	if err == nil || !strings.Contains(err.Error(), "citadel/schedules.yaml: wake is sablier but PARADOX_SABLIER_URL is empty") {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(err.Error(), "site/") {
		t.Fatalf("always-on system wrongly refused: %v", err)
	}
}
