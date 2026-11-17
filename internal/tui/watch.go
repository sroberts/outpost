package tui

import (
	"context"
	"sort"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sroberts/outpost/internal/supervisor"
)

// Watcher streams the service list as it changes, so the dashboard shows a
// crash or a restart the moment the daemon sees it rather than on its next
// poll. *ipc.Client satisfies it; a client without it, or a daemon too old
// to stream, is polled as before.
type Watcher interface {
	Watch(ctx context.Context) (<-chan []supervisor.ServiceStatus, error)
}

// watchRetryTicks is how many refresh ticks pass between attempts to
// subscribe while the dashboard is polling instead.
const watchRetryTicks = 15

type watchStartedMsg struct {
	ch  <-chan []supervisor.ServiceStatus
	err error
}

// watchClosedMsg is the stream ending: the daemon went away or restarted.
type watchClosedMsg struct{}

// startWatch subscribes to the service list.
func (m Model) startWatch() tea.Cmd {
	w, ok := m.client.(Watcher)
	if !ok || m.ctx == nil {
		return nil
	}
	ctx, client := m.ctx, m.client
	return func() tea.Msg {
		if !client.Available() {
			return watchStartedMsg{err: errNoDaemon}
		}
		ch, err := w.Watch(ctx)
		return watchStartedMsg{ch: ch, err: err}
	}
}

// nextFrame waits for the stream's next service list and hands it over as an
// ordinary servicesMsg, so pushed and polled lists take one path.
func nextFrame(ch <-chan []supervisor.ServiceStatus) tea.Cmd {
	return func() tea.Msg {
		list, ok := <-ch
		if !ok {
			return watchClosedMsg{}
		}
		sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
		return servicesMsg{services: list, pushed: true}
	}
}

// updateWatch handles the stream's messages; ok is false for any other.
func (m Model) updateWatch(msg tea.Msg) (Model, tea.Cmd, bool) {
	switch msg := msg.(type) {
	case watchStartedMsg:
		if msg.err != nil {
			// Polling carries on; try again in a while, in case the daemon
			// was restarted onto a build that streams.
			m.watching, m.watchCh, m.watchRetry = false, nil, watchRetryTicks
			return m, nil, true
		}
		m.watching, m.watchCh = true, msg.ch
		return m, nextFrame(msg.ch), true
	case watchClosedMsg:
		m.watching, m.watchCh, m.watchRetry = false, nil, 1
		// Poll at once, so a daemon that has gone shows as gone now.
		return m, m.fetchServices(), true
	}
	return m, nil, false
}

// watchOnTick is what a tick does about the stream: nothing while it runs,
// and now and then another attempt while it doesn't.
func (m Model) watchOnTick() (Model, tea.Cmd) {
	if m.watching {
		return m, nil
	}
	if _, ok := m.client.(Watcher); !ok {
		return m, nil
	}
	if m.watchRetry > 0 {
		m.watchRetry--
		return m, nil
	}
	m.watchRetry = watchRetryTicks
	return m, m.startWatch()
}
