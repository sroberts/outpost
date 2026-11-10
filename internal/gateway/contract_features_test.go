package gateway

import (
	"regexp"
	"strings"
	"testing"

	"github.com/sroberts/outpost"
)

// specSections names every heading in the contract the way contractFeatures
// does: "N. Title" for a numbered section, and "N. Title" for a subsection at
// any depth too, with N its section's number. Backticks are dropped, since a
// feature's section is plain text.
func specSections(spec string) map[string]bool {
	numbered := regexp.MustCompile(`^## (\d+)\. (.+)$`)
	sub := regexp.MustCompile(`^###+ (.+)$`)
	sections := map[string]bool{}
	current := ""
	for _, line := range strings.Split(spec, "\n") {
		line = strings.ReplaceAll(strings.TrimSpace(line), "`", "")
		if m := numbered.FindStringSubmatch(line); m != nil {
			current = m[1]
			sections[m[1]+". "+m[2]] = true
			continue
		}
		if m := sub.FindStringSubmatch(line); m != nil && current != "" {
			sections[current+". "+m[1]] = true
		}
	}
	return sections
}

// A feature pointing at a heading that has been renamed or renumbered sends a
// reader nowhere.
func TestContractFeatures_NameRealSections(t *testing.T) {
	sections := specSections(outpost.ServiceSpec())
	for _, f := range contractFeatures {
		if !sections[f.Section] {
			t.Errorf("feature %s names section %q, which the contract doesn't have", f.Name, f.Section)
		}
	}
}

// Each "Using ..." section describes something Outpost offers an app, so a
// new one that isn't listed means outpost.json undersells this build, as it
// did when email arrived.
func TestContractFeatures_ListEveryUsingSection(t *testing.T) {
	listed := map[string]bool{}
	for _, f := range contractFeatures {
		listed[f.Section] = true
	}
	using := regexp.MustCompile(`^\d+\. Using `)
	for s := range specSections(outpost.ServiceSpec()) {
		if using.MatchString(s) && !listed[s] {
			t.Errorf("the contract's section %q isn't in contractFeatures", s)
		}
	}
}
