package tui

import (
	"context"
	"math"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sroberts/outpost/internal/backup"
	"github.com/sroberts/outpost/internal/ipc"
	"github.com/sroberts/outpost/internal/stats"
	"github.com/sroberts/outpost/internal/supervisor"
)

// fullClient is a daemon with everything the dashboard can use: usage,
// secrets, tokens and the user and service operations.
type fullClient struct {
	*extrasClient
	usage   map[string]stats.Usage
	secrets []ipc.SecretInfo
	tokens  []ipc.TokenInfo
	removed []string
	ops     []string
}

func newSecretsClient() *fullClient {
	x := newExtras()
	cpu := make([]float64, stats.RecentSlots)
	for i := range cpu {
		cpu[i] = float64(i%10) / 20
	}
	cpu[0] = math.NaN()
	day := make([]float64, stats.Buckets)
	for i := range day {
		day[i] = float64(i)
	}
	return &fullClient{
		extrasClient: x,
		usage: map[string]stats.Usage{
			"notes": {Up: true, Measured: true, CPU: 0.25, RSS: 48 << 20, RecentCPU: cpu, RecentRSS: cpu,
				CPUSeries: day, RSSSeries: day, StorageSeries: day, Storage: 3 << 30, StorageAt: time.Now()},
		},
		secrets: []ipc.SecretInfo{{Name: "db-pass", Env: "DB_PASS", Services: []string{"notes"}, Revision: 2}},
		tokens:  []ipc.TokenInfo{{ID: 7, Username: "alice", Name: "ci"}},
	}
}

func (c *fullClient) Stats(string) (map[string]stats.Usage, error) { return c.usage, nil }

func (c *fullClient) SecretList() ([]ipc.SecretInfo, error) { return c.secrets, nil }
func (c *fullClient) SecretSet(n, e, v string) (string, error) {
	c.ops = append(c.ops, "set:"+n+":"+e)
	return "set " + n, nil
}
func (c *fullClient) SecretRemove(n string) (string, error) {
	c.removed = append(c.removed, n)
	return "deleted " + n, nil
}
func (c *fullClient) SecretBind(n, s string) (string, error) {
	c.ops = append(c.ops, "bind:"+n+":"+s)
	return "bound", nil
}
func (c *fullClient) SecretUnbind(n, s string) (string, error) {
	c.ops = append(c.ops, "unbind:"+n+":"+s)
	return "unbound", nil
}

func (c *fullClient) TokenCreate(r ipc.TokenRequest) (string, ipc.TokenInfo, string, error) {
	c.ops = append(c.ops, "token:"+r.Username)
	return "opat_secret", ipc.TokenInfo{ID: 8, Username: r.Username}, "", nil
}
func (c *fullClient) TokenList(string) ([]ipc.TokenInfo, error)    { return c.tokens, nil }
func (c *fullClient) TokenRevoke(id int64) (string, error)          { return "revoked", nil }
func (c *fullClient) TokenClients(string) ([]ipc.ClientInfo, error) { return nil, nil }
func (c *fullClient) TokenRevokeClient(int64) (string, error)       { return "disconnected", nil }

func (c *fullClient) record(op string) (string, error) {
	c.ops = append(c.ops, op)
	return op, nil
}
func (c *fullClient) UserAdd(r ipc.UserRequest) (string, error) { return c.record("add:" + r.Username) }
func (c *fullClient) UserPasswd(u, p string) (string, error)    { return c.record("passwd:" + u) }
func (c *fullClient) UserRemove(u string) (string, error)       { return c.record("rm:" + u) }
func (c *fullClient) UserGroups(u string, a, r []string) (string, error) {
	return c.record("groups:" + u)
}
func (c *fullClient) UserDisable(u string) (string, error)       { return c.record("disable:" + u) }
func (c *fullClient) UserEnable(u string) (string, error)        { return c.record("enable:" + u) }
func (c *fullClient) UserAdmin(u string, a bool) (string, error) { return c.record("admin:" + u) }
func (c *fullClient) UserTailnet(u, l string) (string, error)    { return c.record("tailnet:" + u) }
func (c *fullClient) AuthRotateKey() (string, error)             { return c.record("rotate") }

func (c *fullClient) Forget(n string) (string, error) { return c.record("forget:" + n) }
func (c *fullClient) Apply() ([]supervisor.ServiceStatus, error) {
	_, _ = c.record("apply")
	return c.services, nil
}
func (c *fullClient) Backup(n string) ([]backup.Archive, string, error) {
	msg, err := c.record("backup:" + n)
	return nil, msg, err
}
func (c *fullClient) Backups(n string) ([]backup.Archive, error) {
	return []backup.Archive{{Service: n, Timestamp: "20261101T000000Z", CreatedAt: time.Now().Add(-2 * time.Hour)}}, nil
}
func (c *fullClient) Restore(n, ts string) (string, error) { return c.record("restore:" + n) }

// loadedFull is a dashboard on the services view with everything loaded.
func loadedFull(t *testing.T, c *fullClient) Model {
	t.Helper()
	m := New(c, nil)
	m.width, m.height = 100, 40
	return drain(t, m, tea.Batch(m.fetchServices(), m.fetchGateway(), m.fetchUsage()))
}

// streamClient is a full client whose service list is pushed.
type streamClient struct {
	*fullClient
	ch chan []supervisor.ServiceStatus
}

func (c *streamClient) Watch(context.Context) (<-chan []supervisor.ServiceStatus, error) {
	return c.ch, nil
}
