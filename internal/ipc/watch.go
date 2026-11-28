package ipc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"time"

	"github.com/sroberts/outpost/internal/supervisor"
)

const (
	// watchHeartbeat is how long a quiet stream goes without a frame. A
	// client that has gone away is only noticed when a write to it fails,
	// since it closed its side for writing when it sent the request.
	watchHeartbeat = 15 * time.Second
	// watchThrottle spaces out reads of the list. The loop signals a change
	// on every persist, several times a tick with many services, and each
	// read is a turn of the loop.
	watchThrottle = 200 * time.Millisecond
	// watchWriteTimeout bounds one frame, so a client that stops reading
	// doesn't hold a handler forever.
	watchWriteTimeout = 10 * time.Second

	watchServices     = "services"
	watchHeartbeatMsg = "heartbeat"
)

// watch streams the service list over conn: a frame at once, then another
// whenever the list a client would see is different, until ctx ends, the
// server closes, or a write fails.
func (s *Server) watch(ctx context.Context, conn net.Conn) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-s.closing:
			cancel()
		case <-ctx.Done():
		}
	}()
	// A stream has no overall budget; each write gets its own below.
	_ = conn.SetDeadline(time.Time{})

	beat := time.NewTimer(watchHeartbeat)
	defer beat.Stop()
	var last []byte
	var listed time.Time
	heartbeatDue := false
	for {
		if d := time.Until(listed.Add(watchThrottle)); d > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(d):
			}
		}
		// Taken before the read, so a change landing while it runs still
		// wakes the next wait instead of being lost between the two.
		changed := s.sup.Changed()
		lctx, lcancel := context.WithTimeout(ctx, RequestTimeout)
		services, err := s.sup.List(lctx)
		lcancel()
		listed = time.Now()
		if err != nil {
			if ctx.Err() == nil {
				_ = s.writeFrame(conn, errorResponse(err)) // ending anyway; nothing to do if it fails
			}
			return
		}
		sort.Slice(services, func(i, j int) bool { return services[i].Name < services[j].Name })
		frame, err := json.Marshal(Response{OK: true, Message: watchServices, Services: services})
		if err != nil {
			s.lg("err", err).Errorf("outpost: watch: %v", err)
			return
		}
		var werr error
		sent := true
		switch {
		case !bytes.Equal(frame, last):
			werr = s.writeRaw(conn, frame)
			last = frame
		case heartbeatDue:
			werr = s.writeFrame(conn, Response{OK: true, Message: watchHeartbeatMsg})
		default:
			sent = false
		}
		if werr != nil {
			return
		}
		if sent {
			heartbeatDue = false
			if !beat.Stop() {
				select {
				case <-beat.C:
				default:
				}
			}
			beat.Reset(watchHeartbeat)
		}

		select {
		case <-ctx.Done():
			return
		case <-changed:
		case <-beat.C:
			// Read the list again rather than only beating: some of what it
			// reports, a tailnet node's state, changes without the loop
			// signalling it.
			heartbeatDue = true
		}
	}
}

func (s *Server) writeFrame(conn net.Conn, resp Response) error {
	b, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	return s.writeRaw(conn, b)
}

func (s *Server) writeRaw(conn net.Conn, b []byte) error {
	_ = conn.SetWriteDeadline(time.Now().Add(watchWriteTimeout))
	_, err := conn.Write(append(b, '\n'))
	return err
}

// Watch streams the service list: a frame at once, then one whenever it
// changes, until ctx ends or the connection drops, when the channel closes. A
// daemon too old to know watch returns an error, and the caller falls back to
// polling.
//
// The channel holds only the latest list: a reader that falls behind gets the
// newest state, never a backlog of stale ones.
func (c *Client) Watch(ctx context.Context) (<-chan []supervisor.ServiceStatus, error) {
	conn, err := net.DialTimeout("unix", c.path, 2*time.Second)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || isConnRefused(err) {
			return nil, ErrNoDaemon
		}
		return nil, fmt.Errorf("connect to %s: %w", c.path, err)
	}
	body, err := json.Marshal(Request{Command: CmdWatch})
	if err != nil {
		conn.Close()
		return nil, err
	}
	// The first frame is due at once, so it gets an ordinary budget; after
	// it, a quiet stream is normal and has no deadline.
	_ = conn.SetDeadline(time.Now().Add(c.budget(Request{Command: CmdWatch})))
	if _, err := conn.Write(body); err != nil {
		conn.Close()
		return nil, fmt.Errorf("send request: %w", err)
	}
	// The daemon reads the request to EOF.
	if uc, ok := conn.(*net.UnixConn); ok {
		_ = uc.CloseWrite()
	}

	dec := json.NewDecoder(conn)
	var first Response
	if err := dec.Decode(&first); err != nil {
		conn.Close()
		return nil, fmt.Errorf("watch: read response: %w", err)
	}
	if !first.OK {
		conn.Close()
		return nil, errors.New(first.Error)
	}
	_ = conn.SetDeadline(time.Time{})

	ch := make(chan []supervisor.ServiceStatus, 1)
	ch <- servicesOf(first)
	// Closing the conn is what unblocks the decoder below when ctx ends.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	go func() {
		defer close(ch)
		defer conn.Close()
		defer stop()
		for {
			var resp Response
			if err := dec.Decode(&resp); err != nil || !resp.OK || ctx.Err() != nil {
				return
			}
			if resp.Message != watchServices {
				continue
			}
			latest(ch, servicesOf(resp))
		}
	}()
	return ch, nil
}

// servicesOf is a frame's list. Services is omitempty, so no services arrives
// as nil, and a watcher should see an empty list, not a missing one.
func servicesOf(r Response) []supervisor.ServiceStatus {
	if r.Services == nil {
		return []supervisor.ServiceStatus{}
	}
	return r.Services
}

// latest puts v in ch, replacing anything still unread there. Only the
// goroutine that owns ch sends on it, so after the drain the send can't block.
func latest(ch chan []supervisor.ServiceStatus, v []supervisor.ServiceStatus) {
	select {
	case ch <- v:
	default:
		select {
		case <-ch:
		default:
		}
		ch <- v
	}
}
