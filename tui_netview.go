// Copyright 2026 Jason D. Rivard <code@jrivard.org>
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/jrivard/smith-jail/internal/proxyproto"
)

// netViewMaxEvents bounds memory for a long-running netview: only the most
// recent events are kept, which is all a live tail ever needs — full
// history is what netlog (streaming straight to the terminal's own
// scrollback) is for.
const netViewMaxEvents = 5000

// netViewEventMsg/netViewErrMsg are how tailEvents' channels reach the
// Bubble Tea program: a goroutine in cmdNetView reads them and calls
// Program.Send, the documented way to feed external events into a running
// program (see charm's "send messages" docs) — this file never touches the
// channels directly.
type netViewEventMsg proxyproto.Event
type netViewErrMsg struct{ err error }

type netViewModel struct {
	agent *Agent
	dir   string

	width, height int

	events                     []proxyproto.Event
	allowedCount, blockedCount int
	blockedOnly                bool
	noActiveSession            bool
	errMsg                     string
}

func newNetViewModel(agent *Agent, dir string, noActiveSession bool) netViewModel {
	return netViewModel{agent: agent, dir: dir, noActiveSession: noActiveSession}
}

func (m netViewModel) Init() tea.Cmd { return nil }

func (m netViewModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case netViewEventMsg:
		e := proxyproto.Event(msg)
		if e.Blocked() {
			m.blockedCount++
		} else {
			m.allowedCount++
		}
		m.events = append(m.events, e)
		if len(m.events) > netViewMaxEvents {
			m.events = m.events[len(m.events)-netViewMaxEvents:]
		}
		return m, nil

	case netViewErrMsg:
		m.errMsg = msg.err.Error()
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			return m, tea.Quit
		case "b":
			m.blockedOnly = !m.blockedOnly
			return m, nil
		}
	}
	return m, nil
}

func (m netViewModel) filteredEvents() []proxyproto.Event {
	if !m.blockedOnly {
		return m.events
	}
	filtered := make([]proxyproto.Event, 0, len(m.events))
	for _, e := range m.events {
		if e.Blocked() {
			filtered = append(filtered, e)
		}
	}
	return filtered
}

func (m netViewModel) View() string {
	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	title := titleStyle.Render(fmt.Sprintf("Network Activity — %s (%s)", m.agent.DisplayName, m.dir))
	if m.blockedOnly {
		title += lipgloss.NewStyle().Foreground(lipgloss.Color("11")).Render("  [blocked only]")
	}
	if m.noActiveSession {
		title += lipgloss.NewStyle().Foreground(lipgloss.Color("11")).Render("  [no active session — showing recorded history only]")
	}

	width := m.width
	if width <= 0 {
		width = 80
	}
	rule := strings.Repeat("─", width)

	counts := fmt.Sprintf("%sallowed:%s %d    %sblocked/failed:%s %d",
		colorGreen, colorReset, m.allowedCount, colorRed, colorReset, m.blockedCount)

	rows := m.filteredEvents()
	viewportHeight := m.height - 7 // title, rule, counts, blank, footer, rule, margin
	if viewportHeight < 1 {
		viewportHeight = 1
	}
	start := 0
	if len(rows) > viewportHeight {
		start = len(rows) - viewportHeight
	}

	var body strings.Builder
	if len(rows) == 0 {
		body.WriteString(colorDim + "No events yet — waiting for network activity." + colorReset)
	}
	for _, e := range rows[start:] {
		body.WriteString(formatEvent(e))
		body.WriteString("\n")
	}

	footer := lipgloss.NewStyle().Faint(true).Render("q quit · b toggle blocked-only")
	if m.errMsg != "" {
		footer = colorRed + m.errMsg + colorReset + "   " + footer
	}

	return fmt.Sprintf("%s\n%s\n%s\n\n%s\n%s\n%s", title, rule, counts, body.String(), rule, footer)
}

// cmdNetView opens a live TUI tail of a project's network activity log —
// meant to run in a second terminal/tmux pane alongside "run"/"shell",
// which occupy smith-jail's own terminal for the whole session.
func cmdNetView(agent *Agent, rawDir string) {
	dir := resolveDirOnly(rawDir)

	path, err := netLogPath(agent, dir)
	if err != nil {
		die("Resolving network log path: " + err.Error())
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		printInfo("No network activity recorded yet for this project — run a session with the network jail enabled first (it's on by default on Linux; see --no-network-jail).")
		return
	}

	noActiveSession := FindRunningContainer(agent, projectHash(dir)) == ""

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := tea.NewProgram(newNetViewModel(agent, dir, noActiveSession), tea.WithAltScreen())

	events, errCh := tailEvents(ctx, path, true)
	go func() {
		for e := range events {
			p.Send(netViewEventMsg(e))
		}
	}()
	go func() {
		if err := <-errCh; err != nil {
			p.Send(netViewErrMsg{err})
		}
	}()

	if _, err := p.Run(); err != nil {
		die("netview: " + err.Error())
	}
}
