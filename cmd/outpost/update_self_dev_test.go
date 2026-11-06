package main

import (
	"context"
	"strings"
	"testing"

	"github.com/sroberts/outpost/internal/version"
)

func TestMadeFrom(t *testing.T) {
	for build, want := range map[string]string{
		"v0.2.0-30-g6c3ba82 (6c3ba8213936)":   "v0.2.0",
		"v0.2.0-30-g6c3ba82":                  "v0.2.0",
		"v1.0.0-rc.1-2-gabc1234 (abc1234def)": "v1.0.0-rc.1",
		// Not made cleanly from a release.
		"v0.2.0-30-g6c3ba82-dirty":                "",
		"v0.2.0-30-g6c3ba82 (6c3ba8213936-dirty)": "",
		"v0.2.1-0.20261103193908-d8c2f6b3afa5":    "",
		"v0.2.0 (6c3ba8213936)":                   "",
		"dev":                                     "",
		"6c3ba82":                                 "",
		"nightly-3-gabc1234":                      "",
		"":                                        "",
	} {
		got, ok := madeFrom(build)
		if got != want || ok != (want != "") {
			t.Errorf("madeFrom(%q) = %q, %v; want %q", build, got, ok, want)
		}
	}
}

// describedBuild makes the fixture's running build one `make build` stamped
// between tags, made from base.
func describedBuild(f *selfFixture, base string) string {
	v := base + "-30-g6c3ba82"
	selfBuild = func() version.Info { return version.Info{Version: v, Commit: "6c3ba8213936abcd"} }
	return v
}

// A clean build made from v1.0.0 doesn't have v1.1.0 in it, so v1.1.0 is an
// update, installed like any other.
func TestUpdateSelf_DevBuildMadeFromAnOlderReleaseUpdates(t *testing.T) {
	f := newSelfFixture(t)
	f.installJob()
	dev := describedBuild(f, "v1.0.0")
	f.ping.start(dev + " (6c3ba8213936)")
	newFakeOutpostReleases(t, "v1.1.0", map[string]selfRelease{"v1.1.0": {binary: outpostScript("v1.1.0")}})

	out, _, err := f.update("--check")
	if err != nil || !strings.Contains(out, "made from v1.0.0; v1.1.0 available") || !strings.Contains(out, "`outpost update --self`") {
		t.Errorf("--check: %v\n%s", err, out)
	}
	f.untouched()

	out, errOut, err := f.update()
	if err != nil {
		t.Fatalf("update --self: %v\n%s%s", err, out, errOut)
	}
	if got := readFile(t, f.bin); got != string(outpostScript("v1.1.0")) {
		t.Errorf("binary is now:\n%s", got)
	}
	if !strings.Contains(out, "Outpost is running v1.1.0 (was "+dev+")") {
		t.Errorf("output:\n%s", out)
	}
}

// One made from the latest release, or a later one, already has everything
// in it: nothing is installed without --to or --force.
func TestUpdateSelf_DevBuildMadeFromTheLatestChangesNothing(t *testing.T) {
	for _, base := range []string{"v1.1.0", "v1.2.0"} {
		f := newSelfFixture(t)
		f.installJob()
		describedBuild(f, base)
		newFakeOutpostReleases(t, "v1.1.0", map[string]selfRelease{"v1.1.0": {binary: outpostScript("v1.1.0")}})

		_, _, err := f.update()
		if err == nil || !strings.Contains(err.Error(), "made from "+base) || !strings.Contains(err.Error(), "--to") {
			t.Errorf("%s: err = %v", base, err)
		}
		f.untouched()

		out, _, err := f.update("--check")
		if err != nil || !strings.Contains(out, "made from "+base+", no older than the latest release, v1.1.0") {
			t.Errorf("%s --check: %v\n%s", base, err, out)
		}
		f.untouched()
	}
}

// The Updates page offers a later release to a clean build between tags, and
// says why it offers nothing when the build was made from the latest.
func TestSelfWebLatest_DevBuildMadeFromARelease(t *testing.T) {
	f := newSelfFixture(t)
	f.installJob()
	stubJobLabel(t, testLabel)
	describedBuild(f, "v1.0.0")
	newFakeOutpostReleases(t, "v1.1.0", map[string]selfRelease{"v1.1.0": {binary: outpostScript("v1.1.0")}})

	w := f.web()
	got, err := w.Latest(context.Background())
	if err != nil || !got.Newer || got.Latest != "v1.1.0" {
		t.Errorf("made from v1.0.0: %+v %v", got, err)
	}
	if r := w.Refusal(); r != "" {
		t.Errorf("refused: %s", r)
	}

	describedBuild(f, "v1.1.0")
	got, err = w.Latest(context.Background())
	if err != nil || got.Newer || !strings.Contains(got.Note, "made from v1.1.0") {
		t.Errorf("made from v1.1.0: %+v %v", got, err)
	}

	// A modified tree is still in no release's order.
	selfBuild = func() version.Info {
		return version.Info{Version: "v1.0.0-30-g6c3ba82", Commit: "6c3ba8213936", Dirty: true}
	}
	got, err = w.Latest(context.Background())
	if err != nil || got.Newer || !strings.Contains(got.Note, "development build") {
		t.Errorf("dirty: %+v %v", got, err)
	}
	if r := w.Refusal(); !strings.Contains(r, "development build") {
		t.Errorf("dirty build not refused: %q", r)
	}
}
