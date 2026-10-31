package config

import (
	"os"
	"strings"
	"testing"
)

// The settings table in the contract must list every variable with the
// default the code uses, so the published document cannot drift.
func TestContractListsEverySettingWithItsDefault(t *testing.T) {
	doc, err := os.ReadFile("../../docs/contract.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(doc)
	for name, def := range Defaults {
		row := ""
		for _, line := range strings.Split(text, "\n") {
			if strings.HasPrefix(line, "| `"+name+"` |") {
				row = line
				break
			}
		}
		if row == "" {
			t.Errorf("%s is missing from the settings table in docs/contract.md", name)
			continue
		}
		cells := strings.Split(row, "|")
		got := strings.TrimSpace(cells[2])
		want := "`" + def + "`"
		if def == "" {
			want = "empty"
		}
		if name == "PARADOX_PRIVATE_KEY" {
			want = "none, required"
		}
		if got != want {
			t.Errorf("%s: contract says default %s, code says %s", name, got, want)
		}
	}
}
