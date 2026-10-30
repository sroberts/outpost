package ipc

import (
	"os"
	"testing"
	"time"
)

// A client waiting out a restart compares these, so two runs must differ even
// when the kernel hands the new one the old one's PID.
func TestIdentity(t *testing.T) {
	before := time.Now()
	c := serveWith(t, func(*Server) {})
	id, err := c.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if id.PID != os.Getpid() {
		t.Errorf("pid = %d; want %d", id.PID, os.Getpid())
	}
	if id.Version == "" {
		t.Error("no version")
	}
	if id.Started.Before(before.Add(-time.Second)) || id.Started.After(time.Now()) {
		t.Errorf("started = %v", id.Started)
	}

	other := serveWith(t, func(*Server) {})
	id2, err := other.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if id2.Started.Equal(id.Started) {
		t.Errorf("two servers report the same start time, %v", id.Started)
	}
}
