package ipc

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Backup and restore move a service's data; everything else queries in-memory
// state. Only the first kind may take minutes, and the budgets must say so.
func TestTimeout_TransfersGetTheLongerBudget(t *testing.T) {
	for _, cmd := range []string{CmdBackup, CmdRestore} {
		if got := Timeout(cmd); got != TransferTimeout {
			t.Errorf("Timeout(%q) = %s; want the transfer budget %s", cmd, got, TransferTimeout)
		}
	}
	for _, cmd := range []string{CmdList, CmdStatus, CmdStart, CmdStop, CmdRestart, CmdApply, CmdEvents, CmdPing, CmdBackups} {
		if got := Timeout(cmd); got != RequestTimeout {
			t.Errorf("Timeout(%q) = %s; want the ordinary budget %s", cmd, got, RequestTimeout)
		}
	}
}

// silentDaemon accepts connections and never replies, so every request hits
// the client's deadline.
func silentDaemon(t *testing.T) string {
	t.Helper()
	path := filepath.Join(shortDir(t), "s.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close(); os.Remove(path) })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Hold it open without answering.
			t.Cleanup(func() { conn.Close() })
		}
	}()
	return path
}

// A slow backup must not be reported as a failure. The archive may well be
// written after the client gives up, and telling the operator it failed is
// something they will act on.
func TestClient_SlowBackupSaysItMayStillBeRunning(t *testing.T) {
	c := NewClient(silentDaemon(t))
	c.timeout = 150 * time.Millisecond

	_, _, err := c.Backup("alpha")
	if err == nil {
		t.Fatal("Backup against a silent daemon: want an error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "may still be working") {
		t.Errorf("error does not say the work may still be running: %q", msg)
	}
	if !strings.Contains(msg, "outpost logs alpha --events") {
		t.Errorf("error does not point at where the result will show up: %q", msg)
	}
}

// An ordinary command timing out means something is actually wrong, and must
// not be dressed up as "probably fine".
func TestClient_SlowOrdinaryCommandIsReportedPlainly(t *testing.T) {
	c := NewClient(silentDaemon(t))
	c.timeout = 150 * time.Millisecond

	_, err := c.List()
	if err == nil {
		t.Fatal("List against a silent daemon: want an error, got nil")
	}
	if msg := err.Error(); strings.Contains(msg, "may still be working") {
		t.Errorf("a wedged daemon was excused as slow work: %q", msg)
	}
}
