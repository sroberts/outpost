package gateway

import (
	"context"
	"net"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/sroberts/outpost/internal/stats"
)

// Stats is where the gateway reads a service's usage from.
type Stats interface {
	Usage(service string) (stats.Usage, bool)
}

// trafficMeter counts the bytes the proxy exchanges with each service. It
// counts at the connection rather than per request, so websockets and other
// upgraded connections, and every proxy path, are counted the same way;
// headers count too, since they cross the wire like anything else.
//
// Counters are kept per port, the one thing a dial knows, and put to names
// only when read, through the route table. A port's counter lives as long as
// the gateway; there are only as many as ports ever proxied to.
type trafficMeter struct {
	mu     sync.Mutex
	byPort map[int]*byteCounter
}

type byteCounter struct {
	in  atomic.Uint64 // written to the service
	out atomic.Uint64 // read from it
}

func (m *trafficMeter) counter(port int) *byteCounter {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.byPort == nil {
		m.byPort = map[int]*byteCounter{}
	}
	c := m.byPort[port]
	if c == nil {
		c = &byteCounter{}
		m.byPort[port] = c
	}
	return c
}

type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// dial wraps a dialer so every connection it makes is counted.
func (m *trafficMeter) dial(next dialFunc) dialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := next(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		_, p, splitErr := net.SplitHostPort(addr)
		port, atoiErr := strconv.Atoi(p)
		if splitErr != nil || atoiErr != nil {
			return conn, nil // the proxy always dials host:port; nothing to count against otherwise
		}
		return &countConn{Conn: conn, c: m.counter(port)}, nil
	}
}

func (m *trafficMeter) snapshot() map[int]stats.Bytes {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[int]stats.Bytes, len(m.byPort))
	for port, c := range m.byPort {
		out[port] = stats.Bytes{In: c.in.Load(), Out: c.out.Load(), Source: port}
	}
	return out
}

// countConn is a connection to a service that counts what crosses it.
type countConn struct {
	net.Conn
	c *byteCounter
}

func (c *countConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.c.out.Add(uint64(max(n, 0)))
	return n, err
}

func (c *countConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.c.in.Add(uint64(max(n, 0)))
	return n, err
}

// Traffic is the bytes carried to and from each routed service since the
// gateway started. A port that two routes share can't be told apart, so it
// is left out rather than put down to either.
func (g *Gateway) Traffic() map[string]stats.Bytes {
	names := map[int]string{}
	shared := map[int]bool{}
	for _, rt := range g.routes.all() {
		if _, dup := names[rt.Port]; dup {
			shared[rt.Port] = true
		}
		names[rt.Port] = rt.Name
	}
	out := map[string]stats.Bytes{}
	for port, b := range g.traffic.snapshot() {
		if name, ok := names[port]; ok && !shared[port] {
			out[name] = b
		}
	}
	return out
}
