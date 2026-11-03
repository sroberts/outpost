package tsnet

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
)

// Caller is who is on the other end of a tailnet connection, as Tailscale
// says.
type Caller struct {
	// Login is the Tailscale login the device is signed in to.
	Login string
	// Tagged is a device that belongs to tags rather than a person, whose
	// login names nobody.
	Tagged bool
	// Device is its name on the tailnet, for the log.
	Device string
}

// Forwarder carries connections from one tailnet port to the service on
// loopback, byte for byte.
type Forwarder struct {
	// Service is the app the gateway is asked about. Log lines leave it
	// out: the node's output is already prefixed with it.
	Service string
	// Port is the tailnet port, for the log.
	Port int
	// Target is the service's address, 127.0.0.1:<port>.
	Target string
	// Identify says who a remote address is. Needed only with Check.
	Identify func(ctx context.Context, remote string) (Caller, error)
	// Check, when set, is asked about every connection before it reaches
	// the service, and anything but a user name refuses it. Nil when the
	// gateway is off, which leaves the tailnet itself as the boundary, as it
	// is for HTTP then.
	Check func(ctx context.Context, service, login string) (string, error)
	Logf  func(format string, v ...any)
}

// Serve accepts on ln until ctx ends, which closes ln, and reports why it
// stopped when it wasn't that.
func (f *Forwarder) Serve(ctx context.Context, ln net.Listener) error {
	stop := context.AfterFunc(ctx, func() { _ = ln.Close() }) // ending anyway
	defer stop()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		// Open connections end with ctx too, in handle.
		go f.handle(ctx, c)
	}
}

func (f *Forwarder) logf(format string, v ...any) {
	if f.Logf != nil {
		f.Logf(format, v...)
	}
}

// handle admits, or refuses, one connection and then splices it to the
// service until either side is done.
func (f *Forwarder) handle(ctx context.Context, c net.Conn) {
	defer c.Close()
	remote := c.RemoteAddr().String()
	who := remote

	if f.Check != nil {
		askCtx, cancel := context.WithTimeout(ctx, accessTimeout)
		user, why := f.admit(askCtx, remote)
		cancel()
		if why != "" {
			// Closed without a word: whatever the protocol is, the
			// service never saw this caller, so nothing is owed to it.
			f.logf("tcp %d: refused %s: %s", f.Port, remote, why)
			return
		}
		who = user + " from " + remote
	}

	up, err := (&net.Dialer{Timeout: dialTimeout}).DialContext(ctx, "tcp", f.Target)
	if err != nil {
		f.logf("tcp %d: %s: service on %s is not reachable: %v", f.Port, who, f.Target, err)
		return
	}
	defer up.Close()
	f.logf("tcp %d: connected %s", f.Port, who)

	// Stop both copies when the node is going away, as well as when either
	// side hangs up.
	stop := context.AfterFunc(ctx, func() { _ = c.Close(); _ = up.Close() })
	defer stop()
	splice(c, up)
}

// admit returns the user on yes, or why not.
func (f *Forwarder) admit(ctx context.Context, remote string) (user, why string) {
	if f.Identify == nil {
		return "", "the node can't tell who is calling"
	}
	caller, err := f.Identify(ctx, remote)
	if err != nil {
		return "", "can't tell who is calling: " + err.Error()
	}
	if caller.Tagged {
		return "", "device " + caller.Device + " is tagged, so it belongs to no person"
	}
	user, err = f.Check(ctx, f.Service, caller.Login)
	if err != nil {
		if errors.Is(err, ErrAccessRefused) {
			return "", caller.Login + ": " + err.Error()
		}
		return "", caller.Login + ": " + err.Error() + "; refusing rather than letting it through unchecked"
	}
	return user + " (" + caller.Login + ")", ""
}

// splice copies both ways until both are done. When one side finishes
// sending, the other is told so with a half-close where the connection
// supports it, so a protocol that answers after the end of its input still
// gets to.
func splice(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	pipe := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src) // either side hanging up ends the copy; nothing to report
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite() // the other direction may still be talking
		} else {
			_ = dst.Close() // can't half-close, so end it
		}
	}
	go pipe(a, b)
	go pipe(b, a)
	wg.Wait()
}
