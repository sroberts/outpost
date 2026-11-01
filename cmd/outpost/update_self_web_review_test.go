package main

import (
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/autostart"
)

// After a reboot a recorded PID may be someone else's; an update older than
// any helper runs is abandoned whatever that process is.
func TestSelfStatus_AbandonedOnceTooOldEvenWithALivePID(t *testing.T) {
	now := time.Now()
	st := selfStatus{Phase: selfWaiting, HelperPID: os.Getpid(), Started: now.Add(-selfLongest() - time.Minute)}
	if !st.abandoned(now) {
		t.Error("an update older than any helper runs is still live")
	}
	st.Started = now.Add(-selfLongest() + time.Minute)
	if st.abandoned(now) {
		t.Error("an update within its time, with its helper alive, is abandoned")
	}
}

// A daemon that is up but not answering isn't left for an init system that
// won't restart it; only no daemon at all is.
func TestSignalRestart_NoAnswerIsNotNoDaemon(t *testing.T) {
	launchd, err := autostart.For("darwin")
	if err != nil {
		t.Fatal(err)
	}
	job := daemonJob{mgr: launchd, cfg: autostart.Config{Label: testLabel}}
	var signalled []int
	prev := signalDaemon
	t.Cleanup(func() { signalDaemon = prev })
	signalDaemon = func(pid int) error { signalled = append(signalled, pid); return nil }

	home := shortHome(t)
	a := &app{home: home, out: io.Discard, err: io.Discard}
	if err := a.signalRestart(job); err != nil {
		t.Errorf("no daemon at all: %v", err)
	}

	// The pidfile names a live process (this one) that the socket doesn't answer for.
	if err := os.WriteFile(a.pidPath(), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	err = a.signalRestart(job)
	if err == nil || !strings.Contains(err.Error(), "the daemon (pid "+strconv.Itoa(os.Getpid())+") isn't answering, so it wasn't restarted") {
		t.Errorf("a daemon not answering: %v", err)
	}
	if len(signalled) != 0 {
		t.Errorf("signalled %v; only a PID the socket reports may be", signalled)
	}
}

func TestCgroupLabel(t *testing.T) {
	for _, c := range []struct{ name, cgroup, want string }{
		{"v2", "0::/user.slice/user-1000.slice/user@1000.service/app.slice/outpost.service\n", "outpost"},
		{"hybrid, unified at the root", "12:pids:/user.slice\n1:name=systemd:/user.slice/user-1000.slice/user@1000.service/com.example.outpost.service\n0::/\n",
			"com.example.outpost"},
		{"v1 only", "12:pids:/user.slice\n2:cpu,cpuacct:/user.slice\n1:name=systemd:/system.slice/outpost.service\n", "outpost"},
		{"hybrid", "11:memory:/user.slice\n1:name=systemd:/user.slice/user@1000.service/outpost.service\n0::/user.slice/user@1000.service/outpost.service\n", "outpost"},
		{"v2 first", "0::/system.slice/outpost.service\n1:name=systemd:/system.slice/outpost.service\n", "outpost"},
		{"no service", "0::/user.slice/user-1000.slice/session-2.scope\n", ""},
		{"nothing", "", ""},
	} {
		if got := cgroupLabel(c.cgroup); got != c.want {
			t.Errorf("%s: %q; want %q", c.name, got, c.want)
		}
	}
}

func TestSelfWeb_SystemdRunPassesOnProxiesAndCertificates(t *testing.T) {
	mgr, err := autostart.For("linux")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range helperPassOn {
		t.Setenv(name, "")
	}
	t.Setenv("HTTPS_PROXY", "http://proxy:3128")
	t.Setenv("no_proxy", "localhost")
	t.Setenv("SSL_CERT_FILE", "/etc/ca.pem")
	t.Setenv("TS_AUTHKEY", "tskey-a")
	w := &selfWeb{a: &app{home: "/srv/outpost"}}
	got, err := w.helperArgv(daemonJob{mgr: mgr, cfg: autostart.Config{Label: "outpost"}}, "/b/outpost")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--setenv=HTTPS_PROXY", "--setenv=no_proxy", "--setenv=SSL_CERT_FILE"} {
		if !slices.Contains(got, want) {
			t.Errorf("argv lacks %s: %q", want, got)
		}
	}
	for _, arg := range got {
		if strings.Contains(arg, "TS_") || strings.Contains(arg, "HTTP_PROXY") || strings.Contains(arg, "GITHUB_TOKEN") {
			t.Errorf("argv passes on %s, which isn't set or mustn't be", arg)
		}
	}
}

// An outcome first read after a day is recorded once, and then not shown.
func TestSelfWebStatus_OldUnrecordedOutcomeIsAnnouncedThenHidden(t *testing.T) {
	home := t.TempDir()
	w := &selfWeb{a: &app{home: home, out: io.Discard, err: io.Discard}}
	_ = writeSelfStatus(home, selfStatus{Phase: selfDone, To: "v1.1.0", Started: time.Now().Add(-26 * time.Hour), Finished: time.Now().Add(-25 * time.Hour)})
	st, ok := w.Status()
	if ok || !st.Announce || st.To != "v1.1.0" {
		t.Errorf("first read = %+v %v; want announced and not shown", st, ok)
	}
	if st, ok := w.Status(); ok || st.Announce {
		t.Errorf("second read = %+v %v", st, ok)
	}
}
