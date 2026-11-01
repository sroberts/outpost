package main

import (
	"strings"
	"testing"
)

// A job installed under another label is found with --label, as the
// no-job refusal says it is, rather than being impossible to update.
func TestUpdateSelf_LabelFindsAJobUnderAnotherLabel(t *testing.T) {
	f := newSelfFixture(t)
	f.cfg.Label = testLabel + "-other"
	f.installJob()
	f.ping.start(oldBuild + " (0123456789ab)")
	newFakeOutpostReleases(t, "v1.1.0", map[string]selfRelease{"v1.1.0": {binary: outpostScript("v1.1.0")}})

	_, _, err := f.update()
	if err == nil || !strings.Contains(err.Error(), "--label") {
		t.Fatalf("without --label: err = %v; want the no-job refusal pointing at --label", err)
	}
	f.untouched()

	out, errOut, err := f.update("--label", f.cfg.Label)
	if err != nil {
		t.Fatalf("update --self --label: %v\n%s%s", err, out, errOut)
	}
	if !strings.Contains(out, "Outpost is running v1.1.0") {
		t.Errorf("output:\n%s", out)
	}
	if got := readFile(t, f.bin); got != string(outpostScript("v1.1.0")) {
		t.Errorf("binary is now:\n%s", got)
	}
}

// --label is the daemon's job, so it means nothing for a service update.
func TestUpdate_LabelNeedsSelf(t *testing.T) {
	_, _, err := runCLI(t, shortHome(t), "update", "svc", "--label", "x")
	if err == nil || !strings.Contains(err.Error(), "--self") {
		t.Errorf("err = %v; want --label refused without --self", err)
	}
}
