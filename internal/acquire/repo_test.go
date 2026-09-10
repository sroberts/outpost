package acquire

import "testing"

func TestParseRepo_AcceptsTheFormsPeopleHaveToHand(t *testing.T) {
	want := Repo{Owner: "sroberts", Name: "plumbline"}
	for _, in := range []string{
		"https://github.com/sroberts/plumbline",
		"https://github.com/sroberts/plumbline/",
		"https://github.com/sroberts/plumbline.git",
		"http://github.com/sroberts/plumbline",
		"https://www.github.com/sroberts/plumbline",
		"github.com/sroberts/plumbline",
		"git@github.com:sroberts/plumbline.git",
		"sroberts/plumbline",
		// Copied from a releases or file page rather than the repo root.
		"https://github.com/sroberts/plumbline/releases/tag/v1.0.0",
		"https://github.com/sroberts/plumbline/blob/main/outpost.toml",
	} {
		got, err := ParseRepo(in)
		if err != nil {
			t.Errorf("ParseRepo(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseRepo(%q) = %+v; want %+v", in, got, want)
		}
	}
}

func TestParseRepo_Rejects(t *testing.T) {
	for _, in := range []string{
		"",
		"   ",
		"plumbline",
		"https://gitlab.com/sroberts/plumbline",
		"https://example.com/sroberts/plumbline",
		"sroberts/",
		"/plumbline",
		"sroberts/plum bline",
		"../../etc/passwd",
	} {
		if got, err := ParseRepo(in); err == nil {
			t.Errorf("ParseRepo(%q) = %+v; want an error", in, got)
		}
	}
}
