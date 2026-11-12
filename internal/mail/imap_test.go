package mail

import (
	"bytes"
	"context"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/backend/memory"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-imap/server"
)

// The real session against go-imap's in-memory server: searching, reading
// recipient headers and whole messages, without marking anything read.
// Labelling is Gmail's own extension, which this server doesn't have.
func TestIMAPSession_AgainstAServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New(memory.New())
	srv.AllowInsecureAuth = true
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	// The memory backend starts with one message; add one routed to us.
	seed, err := client.Dial(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Login("username", "password"); err != nil {
		t.Fatal(err)
	}
	raw := newsletter("me+yarr@gmail.com", "hello")
	if err := seed.Append("INBOX", nil, time.Now(), bytes.NewBufferString(raw)); err != nil {
		t.Fatal(err)
	}
	_ = seed.Logout()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := login(context.Background(), conn, "username", "wrong"); err == nil || !strings.Contains(err.Error(), "app password") {
		t.Errorf("a wrong password = %v; want the login error", err)
	}
	conn, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := login(context.Background(), conn, "username", "password")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.close() }()
	validity, err := sess.inbox()
	if err != nil || validity == 0 {
		t.Fatalf("inbox = %d, %v", validity, err)
	}
	uids, err := sess.search(0, time.Now().Add(-firstLook))
	if err != nil || len(uids) == 0 {
		t.Fatalf("search = %v, %v", uids, err)
	}
	last := slices.Max(uids)
	if after, err := sess.search(last, time.Time{}); err != nil || len(after) != 0 {
		t.Errorf("search after the newest = %v, %v; want nothing, though n:* matches it", after, err)
	}
	heads, err := sess.headers([]uint32{last})
	if err != nil {
		t.Fatal(err)
	}
	if got := recipients(heads[last]); len(got) == 0 || got[0] != "me+yarr@gmail.com" {
		t.Errorf("recipients = %q", got)
	}
	if strings.Contains(string(heads[last]), "Subject") {
		t.Errorf("headers fetched more than the recipients: %q", heads[last])
	}
	bodies, err := sess.bodies([]uint32{last})
	if err != nil {
		t.Fatal(err)
	}
	m, err := parse(bodies[last])
	if err != nil || m.Subject != "hello" || m.Text != "Hello in text." {
		t.Fatalf("parsed %+v, %v", m, err)
	}

	// Nothing was marked read.
	set := new(imap.SeqSet)
	set.AddNum(last)
	ch := make(chan *imap.Message, 1)
	if err := sess.(*imapSession).c.UidFetch(set, []imap.FetchItem{imap.FetchFlags}, ch); err != nil {
		t.Fatal(err)
	}
	if msg := <-ch; msg == nil || slices.Contains(msg.Flags, imap.SeenFlag) {
		t.Errorf("reading marked the message read: %v", msg)
	}
}
