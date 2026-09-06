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
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// screen identifies which view currently owns the keyboard.
type screen int

const (
	// screenDashboard is the sessions overview — the true top level: active
	// sessions, "New session ▸", and agent/directory-independent utilities.
	screenDashboard screen = iota
	// screenNewSession is the submenu everything about configuring/launching
	// a session for the currently selected agent+directory now lives in —
	// what screenDashboard used to be, before it grew a sessions overview.
	screenNewSession
	screenPicker
	screenPackages
	screenSettings
	screenOllamaModel
	screenArtifacts
	screenDockerfile
	screenDoctor
	screenHelp
	screenAgentPicker
)

// imageState is the dashboard's per-agent view of the Docker image, loaded
// asynchronously so a slow or absent daemon never blocks the first paint.
type imageState struct {
	loaded bool
	info   ImageInfo
	err    error

	// Version fields are populated only by an explicit update check.
	installed string
	latest    string
	checked   bool
}

// updateAvailable reports whether a newer agent release exists in the registry.
func (s *imageState) updateAvailable() bool {
	return s.checked && s.installed != "" && s.latest != "" && s.installed != s.latest
}

// rootModel is the top-level Bubble Tea model. It owns shared state (config,
// selected agent, selected directory) and delegates to a sub-model per screen.
type rootModel struct {
	screen        screen
	width, height int

	cfg    *Config
	cfgErr error

	dockerChecked bool
	dockerErr     error

	agentIdx    int
	agentCursor int
	dir         string

	images map[string]*imageState

	picker        pickerModel
	packages      pkgModel
	settings      settingsModel
	modelPicker   modelPickerModel
	artifacts     artifactsModel
	recentsLoaded bool

	// hubCursor is the highlighted row of screenNewSession's always-focused
	// list.
	hubCursor int

	// dashCursor is the highlighted row of screenDashboard's list: active
	// sessions, then "New session ▸", then dashGlobalItems, all in one flat
	// index.
	dashCursor     int
	sessions       []activeSession
	sessionsLoaded bool
	sessionsErr    error

	checkingUpdates bool

	doctorChecks []DoctorCheck

	dockerfileContent string
	dockerfileErr     error
	dockerfileScroll  int

	action *tuiAction
	status string
}

// ── Messages ──────────────────────────────────────────────────────────────────

type dockerCheckedMsg struct{ err error }

type imageInfoMsg struct {
	agent string
	info  ImageInfo
	err   error
}

type recentsLoadedMsg struct{ items []recentProject }

// dirChosenMsg is emitted by the picker when the user commits a directory.
type dirChosenMsg struct{ dir string }

// configReloadedMsg carries a config re-read after an editor saved changes.
type configReloadedMsg struct {
	cfg *Config
	err error
}

type versionsMsg struct {
	agent     string
	installed string
	latest    string
}

// activeSessionsMsg carries a fresh ListActiveSessions() result.
type activeSessionsMsg struct {
	sessions []activeSession
	err      error
}

// activeSessionsTickMsg fires the sessions overview's periodic refresh.
type activeSessionsTickMsg struct{}

// ── Construction ──────────────────────────────────────────────────────────────

func newRootModel() rootModel {
	m := rootModel{
		screen: screenDashboard,
		images: map[string]*imageState{},
	}

	if wd, err := os.Getwd(); err == nil {
		m.dir = wd
	} else {
		m.dir = "/"
	}

	// LoadConfig is used rather than mustLoadConfig: the latter prompts on
	// stdin for missing config files, which the alternate screen cannot host.
	cfg, err := LoadConfig(m.dir)
	if err != nil {
		m.cfgErr = err
	} else {
		m.cfg = cfg
	}

	for _, a := range AllAgents {
		m.images[a.Name] = &imageState{}
	}

	m.picker = newPickerModel(m.dir)
	m.packages = newPkgModel()
	m.settings = newSettingsModel()
	m.modelPicker = newModelPickerModel()
	m.artifacts = newArtifactsModel()
	return m
}

func (m rootModel) agent() *Agent { return AllAgents[m.agentIdx] }

// recentProjectFor reports whether dir has been jailed before, and its record.
func (m rootModel) recentProjectFor(dir string) (recentProject, bool) {
	for _, r := range m.picker.recents {
		if r.Dir == dir {
			return r, true
		}
	}
	return recentProject{}, false
}

// updateDashboard handles a keystroke on the top-level sessions overview:
// active sessions, then "New session ▸", then dashGlobalItems, all under one
// flat cursor. This is the true root screen — esc/q here quits the app,
// unlike screenNewSession's esc/q, which only steps back up to here.
func (m rootModel) updateDashboard(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	total := len(m.sessions) + 1 + len(dashGlobalItems) // +1 for "New session ▸"

	switch msg.String() {
	case "ctrl+c", "q", "esc":
		return m, tea.Quit

	case "up", "k":
		if m.dashCursor > 0 {
			m.dashCursor--
		}
		return m, nil

	case "down", "j":
		if m.dashCursor < total-1 {
			m.dashCursor++
		}
		return m, nil

	case "enter", " ":
		return m.activateDashboard()
	}
	return m, nil
}

// activateDashboard performs whatever the highlighted top-level row
// promises: open netview for a session, enter the New Session submenu, or
// hand off to activateHub for a global row.
func (m rootModel) activateDashboard() (tea.Model, tea.Cmd) {
	switch {
	case m.dashCursor < len(m.sessions):
		s := m.sessions[m.dashCursor]
		m.action = &tuiAction{Kind: actionNetView, Agent: s.Agent, Dir: s.Dir}
		return m, tea.Quit

	case m.dashCursor == len(m.sessions):
		m.hubCursor = 0
		m.screen = screenNewSession
		return m, nil

	default:
		row := dashGlobalItems[m.dashCursor-len(m.sessions)-1].row
		return m.activateHub(row)
	}
}

// enterDashboard returns to the top-level sessions overview and kicks off a
// fresh, immediate refresh (plus restarting the periodic tick) rather than
// waiting out whatever's left of the previous refresh interval.
func (m rootModel) enterDashboard() (tea.Model, tea.Cmd) {
	m.screen = screenDashboard
	return m, tea.Batch(activeSessionsCmd(), activeSessionsTickCmd())
}

// updateNewSessionMenu handles a keystroke on the "New session" submenu's
// always-focused list — Run, Shell, and everything that configures a
// session for the currently selected agent+directory. Esc/q steps back up
// to the dashboard rather than quitting (ctrl+c still quits outright).
func (m rootModel) updateNewSessionMenu(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit

	case "esc", "q":
		return m.enterDashboard()

	case "up", "k":
		if m.hubCursor > 0 {
			m.hubCursor--
		}
		return m, nil

	case "down", "j":
		if m.hubCursor < len(newSessionItems)-1 {
			m.hubCursor++
		}
		return m, nil

	case "enter", " ":
		return m.activateHub(newSessionItems[m.hubCursor].row)
	}
	return m, nil
}

// activateHub performs whatever row promises — shared by the New Session
// submenu (Run/Shell/Change agent/Change project/Settings/Ollama
// model/Dockerfile) and the dashboard's global rows (Docker
// artifacts/Check updates/Doctor/Help/Quit); each caller only ever passes a
// row it actually owns.
func (m rootModel) activateHub(row hubRow) (tea.Model, tea.Cmd) {
	switch row {
	case rowRun:
		return m.launch(actionRun)
	case rowShell:
		return m.launch(actionShell)
	case rowChangeAgent:
		m.agentCursor = m.agentIdx
		m.screen = screenAgentPicker
		return m, nil
	case rowChangeProject:
		m.picker = m.picker.reset(m.dir)
		m.screen = screenPicker
		return m, nil
	case rowSettings:
		m.settings = m.settings.reset(m.cfg)
		m.screen = screenSettings
		return m, nil
	case rowOllamaModel:
		m.modelPicker = m.modelPicker.reset(m.cfg)
		m.screen = screenOllamaModel
		return m, ollamaModelsCmd()
	case rowArtifacts:
		return m.openArtifacts()
	case rowDockerfile:
		return m.openDockerfile()
	case rowCheckUpdates:
		return m.triggerUpdates()
	case rowDoctor:
		m.doctorChecks = RunDoctorChecks(m.cfg)
		m.screen = screenDoctor
		return m, nil
	case rowHelp:
		m.screen = screenHelp
		return m, nil
	case rowQuit:
		return m, tea.Quit
	}
	return m, nil
}

// updateAgentPicker handles a keystroke on the agent-selection screen: an
// always-focused list, same shape as the New Session submenu, just scoped to
// AllAgents. Reached only from within New Session, so it steps back there.
func (m rootModel) updateAgentPicker(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit

	case "esc", "q":
		m.screen = screenNewSession
		return m, nil

	case "up", "k":
		if m.agentCursor > 0 {
			m.agentCursor--
		}
		return m, nil

	case "down", "j":
		if m.agentCursor < len(AllAgents)-1 {
			m.agentCursor++
		}
		return m, nil

	case "enter", " ":
		m.agentIdx = m.agentCursor
		m.status = ""
		m.screen = screenNewSession
		return m, nil
	}
	return m, nil
}

// openArtifacts is shared by the "a" accelerator and the Artifacts menu row.
func (m rootModel) openArtifacts() (tea.Model, tea.Cmd) {
	if m.dockerErr != nil {
		m.status = "Docker is unavailable — no artifacts to show."
		return m, nil
	}
	m.artifacts = m.artifacts.reset()
	m.screen = screenArtifacts
	return m, loadArtifactsCmd()
}

// openDockerfile is shared by the Dockerfile menu row: renders the Dockerfile
// that would be built for the current agent/project. Unlike most other rows,
// this needs only cfg, not a working Docker daemon — it never runs "docker
// build", just writes the same build context BuildImage would and reads it
// back.
func (m rootModel) openDockerfile() (tea.Model, tea.Cmd) {
	if m.cfg == nil {
		m.status = "Config is unavailable — cannot generate a Dockerfile."
		return m, nil
	}
	m.dockerfileContent, m.dockerfileErr = m.cfg.GeneratedDockerfile(m.agent())
	m.dockerfileScroll = 0
	m.screen = screenDockerfile
	return m, nil
}

// triggerUpdates is shared by the "u" accelerator and the Check updates menu
// row.
func (m rootModel) triggerUpdates() (tea.Model, tea.Cmd) {
	if m.dockerErr != nil {
		m.status = "Docker is unavailable — cannot check versions."
		return m, nil
	}
	if m.cfg == nil {
		m.status = "Config is unavailable — cannot check versions."
		return m, nil
	}
	if m.checkingUpdates {
		return m, nil
	}
	m.checkingUpdates = true
	m.status = ""
	cmds := make([]tea.Cmd, 0, len(AllAgents))
	for _, a := range AllAgents {
		m.images[a.Name].checked = false
		cmds = append(cmds, versionsCmd(a, m.cfg))
	}
	return m, tea.Batch(cmds...)
}

func (m rootModel) Init() tea.Cmd {
	return tea.Batch(checkDockerCmd(), loadRecentsCmd(), activeSessionsCmd(), activeSessionsTickCmd())
}

// ── Commands ──────────────────────────────────────────────────────────────────

func checkDockerCmd() tea.Cmd {
	return func() tea.Msg { return dockerCheckedMsg{err: CheckDocker()} }
}

func activeSessionsCmd() tea.Cmd {
	return func() tea.Msg {
		sessions, err := ListActiveSessions()
		return activeSessionsMsg{sessions: sessions, err: err}
	}
}

// activeSessionsRefreshInterval bounds how quickly the sessions overview
// notices a session starting or exiting. A live human is watching a list,
// not a monitoring pipeline, so a few seconds' staleness is unnoticeable
// and keeps this from hammering the Docker daemon.
const activeSessionsRefreshInterval = 3 * time.Second

func activeSessionsTickCmd() tea.Cmd {
	return tea.Tick(activeSessionsRefreshInterval, func(time.Time) tea.Msg { return activeSessionsTickMsg{} })
}

func imageInfoCmd(a *Agent, cfg *Config) tea.Cmd {
	return func() tea.Msg {
		info, err := GetImageInfo(a, cfg)
		return imageInfoMsg{agent: a.Name, info: info, err: err}
	}
}

func loadRecentsCmd() tea.Cmd {
	return func() tea.Msg { return recentsLoadedMsg{items: loadRecentProjects()} }
}

func reloadConfigCmd(dir string) tea.Cmd {
	return func() tea.Msg {
		cfg, err := LoadConfig(dir)
		return configReloadedMsg{cfg: cfg, err: err}
	}
}

// versionsCmd compares the agent version baked into the image against the
// registry. This is deliberately on-demand rather than automatic: reading the
// installed version starts a throwaway container per agent, which is far too
// costly to pay on every launch of the interface.
func versionsCmd(a *Agent, cfg *Config) tea.Cmd {
	return func() tea.Msg {
		return versionsMsg{
			agent:     a.Name,
			installed: InstalledVersion(a, cfg),
			latest:    LatestVersion(a),
		}
	}
}

// ── Update ────────────────────────────────────────────────────────────────────

func (m rootModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.picker = m.picker.setSize(msg.Width, msg.Height)
		m.packages = m.packages.setSize(msg.Width, msg.Height)
		m.settings = m.settings.setSize(msg.Width, msg.Height)
		m.modelPicker = m.modelPicker.setSize(msg.Width, msg.Height)
		m.artifacts = m.artifacts.setSize(msg.Width, msg.Height)
		return m, nil

	case dockerCheckedMsg:
		m.dockerChecked = true
		m.dockerErr = msg.err
		if msg.err != nil || m.cfg == nil {
			return m, nil
		}
		cmds := make([]tea.Cmd, 0, len(AllAgents))
		for _, a := range AllAgents {
			cmds = append(cmds, imageInfoCmd(a, m.cfg))
		}
		return m, tea.Batch(cmds...)

	case imageInfoMsg:
		if st, ok := m.images[msg.agent]; ok {
			st.loaded, st.info, st.err = true, msg.info, msg.err
		}
		return m, nil

	case recentsLoadedMsg:
		m.picker = m.picker.setRecents(msg.items)
		m.recentsLoaded = true
		return m, nil

	case activeSessionsMsg:
		m.sessions = msg.sessions
		m.sessionsErr = msg.err
		m.sessionsLoaded = true
		if m.dashCursor > len(m.sessions)+1+len(dashGlobalItems)-1 {
			m.dashCursor = 0
		}
		return m, nil

	case activeSessionsTickMsg:
		if m.screen != screenDashboard {
			return m, nil
		}
		return m, tea.Batch(activeSessionsCmd(), activeSessionsTickCmd())

	case dirChosenMsg:
		m.dir = msg.dir
		m.screen = screenNewSession
		m.status = ""
		// Project overrides are keyed by directory, so switching projects
		// requires re-layering config, not just remembering the new path.
		return m, reloadConfigCmd(msg.dir)

	case configReloadedMsg:
		if msg.err != nil {
			m.cfgErr = msg.err
			return m, nil
		}
		m.cfg, m.cfgErr = msg.cfg, nil
		// A saved edit can change every image's rebuild verdict, so re-read
		// image state rather than leaving a stale "ready" badge on screen.
		if m.dockerErr == nil {
			cmds := make([]tea.Cmd, 0, len(AllAgents))
			for _, a := range AllAgents {
				cmds = append(cmds, imageInfoCmd(a, m.cfg))
			}
			return m, tea.Batch(cmds...)
		}
		return m, nil

	case versionsMsg:
		if st, ok := m.images[msg.agent]; ok {
			st.installed, st.latest, st.checked = msg.installed, msg.latest, true
		}
		m.checkingUpdates = false
		for _, a := range AllAgents {
			if st := m.images[a.Name]; st != nil && !st.checked {
				m.checkingUpdates = true
			}
		}
		return m, nil

	case ollamaModelsLoadedMsg:
		m.modelPicker.loaded = true
		m.modelPicker.loadErr = msg.err
		m.modelPicker.pulled = msg.models
		m.modelPicker.items = m.modelPicker.buildItems()
		return m, nil

	case artifactsLoadedMsg:
		m.artifacts.set = msg.set
		m.artifacts.loaded = true
		return m, nil

	case artifactsRemovedMsg:
		m.artifacts.working = false
		m.artifacts.selected = map[artifactTab]map[string]bool{}
		if msg.err != nil {
			m.artifacts.status = fmt.Sprintf("Removed %d, but: %v", msg.removed, msg.err)
			m.artifacts.failed = true
		} else {
			m.artifacts.status = fmt.Sprintf("Removed %d object(s).", msg.removed)
			m.artifacts.failed = false
		}
		// Removing an image changes the dashboard's build state too.
		cmds := []tea.Cmd{loadArtifactsCmd()}
		if m.dockerErr == nil && m.cfg != nil {
			for _, a := range AllAgents {
				cmds = append(cmds, imageInfoCmd(a, m.cfg))
			}
		}
		return m, tea.Batch(cmds...)

	case tea.KeyMsg:
		return m.routeKey(msg)
	}

	return m, nil
}

// routeKey dispatches a keypress to the focused screen.
//
// Bubble Tea coalesces every rune available in a single read into one KeyRunes
// message, so holding j for autorepeat — or pasting — arrives as a single key
// named "jjj" that matches no binding. Splitting it back into one message per
// rune keeps held keys working and costs text inputs nothing but an extra
// update per character.
func (m rootModel) routeKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.Type != tea.KeyRunes || len(msg.Runes) <= 1 {
		return m.routeKeyOne(msg)
	}

	var model tea.Model = m
	cmds := make([]tea.Cmd, 0, len(msg.Runes))
	for _, r := range msg.Runes {
		single := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}, Alt: msg.Alt}

		next, ok := model.(rootModel)
		if !ok {
			break
		}
		var cmd tea.Cmd
		model, cmd = next.routeKeyOne(single)
		cmds = append(cmds, cmd)
	}
	return model, tea.Batch(cmds...)
}

// routeKeyOne handles exactly one keystroke and the uniform "child asked to go
// back" transition.
func (m rootModel) routeKeyOne(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.screen {
	case screenDashboard:
		return m.updateDashboard(msg)

	case screenNewSession:
		return m.updateNewSessionMenu(msg)

	case screenPicker:
		p, cmd := m.picker.Update(msg)
		m.picker = p
		if p.cancelled {
			m.picker.cancelled = false
			m.screen = screenNewSession
		}
		return m, cmd

	case screenPackages:
		p, cmd := m.packages.Update(msg)
		m.packages = p
		if p.cancelled {
			// Packages is only reached from within Settings now, so "back"
			// returns there rather than all the way out to New Session.
			m.packages.cancelled = false
			m.screen = screenSettings
		}
		return m, cmd

	case screenSettings:
		s, cmd := m.settings.Update(msg)
		m.settings = s
		if s.openPackages {
			s.openPackages = false
			m.settings = s
			m.packages = m.packages.reset(m.cfg)
			m.screen = screenPackages
			return m, nil
		}
		if s.cancelled {
			m.settings.cancelled = false
			m.screen = screenNewSession
		}
		return m, cmd

	case screenOllamaModel:
		p, cmd := m.modelPicker.Update(msg)
		m.modelPicker = p
		if p.cancelled {
			m.modelPicker.cancelled = false
			m.screen = screenNewSession
		}
		return m, cmd

	case screenArtifacts:
		a, cmd := m.artifacts.Update(msg)
		m.artifacts = a
		if a.cancelled {
			m.artifacts.cancelled = false
			return m.enterDashboard()
		}
		return m, cmd

	case screenDockerfile:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "up", "k":
			if m.dockerfileScroll > 0 {
				m.dockerfileScroll--
			}
			return m, nil
		case "down", "j":
			m.dockerfileScroll++
			return m, nil
		case "pgup":
			m.dockerfileScroll -= m.dockerfileVisibleLines()
			if m.dockerfileScroll < 0 {
				m.dockerfileScroll = 0
			}
			return m, nil
		case "pgdown":
			m.dockerfileScroll += m.dockerfileVisibleLines()
			return m, nil
		default:
			m.screen = screenNewSession
			return m, nil
		}

	case screenDoctor:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		return m.enterDashboard()

	case screenHelp:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		return m.enterDashboard()

	case screenAgentPicker:
		return m.updateAgentPicker(msg)
	}

	return m.updateDashboard(msg)
}

// launch validates the current selection and, if it holds up, quits the program
// so cmdTUI can run the real command against the restored terminal.
func (m rootModel) launch(kind actionKind) (tea.Model, tea.Cmd) {
	if m.dockerErr != nil {
		m.status = "Docker is unavailable — cannot launch."
		return m, nil
	}
	if fi, err := os.Stat(m.dir); err != nil || !fi.IsDir() {
		m.status = "Not a directory: " + m.dir
		return m, nil
	}
	m.action = &tuiAction{Kind: kind, Agent: m.agent(), Dir: m.dir}
	return m, tea.Quit
}

// ── View ──────────────────────────────────────────────────────────────────────

func (m rootModel) View() string {
	// The picker is a focused modal with its own full styling; every other
	// screen shares one outer padding.
	if m.screen == screenPicker {
		return m.picker.View()
	}

	var body string
	switch m.screen {
	case screenNewSession:
		body = m.viewNewSessionMenu()
	case screenPackages:
		body = m.packages.View()
	case screenSettings:
		body = m.settings.View()
	case screenOllamaModel:
		body = m.modelPicker.View()
	case screenArtifacts:
		body = m.artifacts.View()
	case screenDockerfile:
		body = m.viewDockerfile()
	case screenDoctor:
		body = m.viewDoctor()
	case screenHelp:
		body = m.viewHelp()
	case screenAgentPicker:
		body = m.viewAgentPicker()
	default:
		body = m.viewDashboardBody()
	}
	return lipgloss.NewStyle().Padding(1, 2).Render(body)
}

func (m rootModel) viewHelp() string {
	section := func(title string, pairs ...string) string {
		out := styleCardTitleActive.Render("  "+title) + "\n"
		for i := 0; i+1 < len(pairs); i += 2 {
			out += "    " + styleKey.Render(padRight(pairs[i], 14)) +
				styleValue.Render(pairs[i+1]) + "\n"
		}
		return out
	}

	var b strings.Builder
	b.WriteString(styleTitle.Render("Keys"))
	b.WriteString("\n\n")

	b.WriteString(section("Dashboard",
		"↑ / ↓", "move the highlight (arrow keys or j/k)",
		"enter / space", "activate the highlighted row",
		"esc / q", "quit",
	))
	b.WriteString("\n")
	b.WriteString(section("Lists",
		"j / k", "move (arrow keys work too)",
		"space / enter", "select or toggle",
		"esc", "go back",
	))
	b.WriteString("\n")
	b.WriteString(section("Editors",
		"a", "add a package",
		"d", "remove the highlighted entry",
		"u", "revert unsaved changes",
		"s", "save to smith-jail.env",
	))

	b.WriteString("\n")
	b.WriteString(styleMuted.Render("  Every launch is equivalent to a plain CLI command — the line above the"))
	b.WriteString("\n")
	b.WriteString(styleMuted.Render("  key hints on the dashboard shows exactly which one."))
	b.WriteString("\n\n")
	b.WriteString(helpLine("any key", "back"))

	return b.String()
}

// dockerfileVisibleLines is how many Dockerfile lines fit on screen at once,
// leaving room for the title, path, and footer.
func (m rootModel) dockerfileVisibleLines() int {
	if m.height <= 0 {
		return 20
	}
	return max(5, m.height-10)
}

// viewDockerfile renders the Dockerfile generated for the current
// agent/project, scrolled to m.dockerfileScroll. Any key other than the
// scroll keys dismisses it, same as Doctor and Help.
func (m rootModel) viewDockerfile() string {
	var b strings.Builder
	b.WriteString(styleTitle.Render("Dockerfile — " + m.agent().DisplayName))
	b.WriteString("\n")
	b.WriteString(styleMuted.Render("  " + m.dir))
	b.WriteString("\n\n")

	if m.dockerfileErr != nil {
		b.WriteString(styleErr.Render("  " + m.dockerfileErr.Error()))
		b.WriteString("\n\n")
		b.WriteString(helpLine("any key", "back"))
		return b.String()
	}

	lines := strings.Split(strings.TrimRight(m.dockerfileContent, "\n"), "\n")
	visible := m.dockerfileVisibleLines()
	maxScroll := max(0, len(lines)-visible)
	scroll := min(max(m.dockerfileScroll, 0), maxScroll)
	end := min(scroll+visible, len(lines))

	for _, line := range lines[scroll:end] {
		b.WriteString("  " + styleValue.Render(line) + "\n")
	}

	b.WriteString("\n")
	if len(lines) > visible {
		b.WriteString(styleMuted.Render(fmt.Sprintf("  lines %d-%d of %d", scroll+1, end, len(lines))))
		b.WriteString("\n")
	}
	b.WriteString(helpLine("↑/↓", "scroll", "any other key", "back"))

	return b.String()
}

// viewDoctor renders the results of the last Doctor run — a static report,
// dismissed like Help by any key.
func (m rootModel) viewDoctor() string {
	var b strings.Builder
	b.WriteString(styleTitle.Render("Doctor"))
	b.WriteString("\n\n")

	failed := false
	for _, c := range m.doctorChecks {
		var mark, style = "✔", styleOK
		switch c.Status {
		case statusWarn:
			mark, style = "⚠", styleWarn
		case statusFail:
			mark, style = "✖", styleErr
			failed = true
		}
		b.WriteString("  " + style.Render(mark) + " " + padRight(c.Name, 40) + styleMuted.Render(c.Detail))
		b.WriteString("\n")
		if c.Fix != "" {
			b.WriteString("      " + styleMuted.Render("Fix: "+c.Fix))
			b.WriteString("\n")
		}
	}

	b.WriteString("\n")
	if failed {
		b.WriteString(styleErr.Render("  One or more checks failed — see fixes above."))
	} else {
		b.WriteString(styleOK.Render("  All checks passed."))
	}
	b.WriteString("\n\n")
	b.WriteString(helpLine("any key", "back"))

	return b.String()
}

// viewAgentPicker renders the agent-selection screen: one always-focused
// list, same rendering approach as the New Session submenu (renderNewSessionList).
func (m rootModel) viewAgentPicker() string {
	width := 60
	if m.width > 0 {
		width = max(30, m.width-4)
	}

	var b strings.Builder
	b.WriteString(styleTitle.Render("Change agent"))
	b.WriteString("\n\n")

	for i, a := range AllAgents {
		label := a.DisplayName
		if i == m.agentIdx {
			label += "  (current)"
		}

		switch {
		case i == m.agentCursor:
			b.WriteString(highlightRow(width, "  "+label))
		case i == m.agentIdx:
			b.WriteString("  " + styleAccent.Render(label))
		default:
			b.WriteString("  " + styleRow.Render(label))
		}
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(helpLine("↑/↓", "move", "enter", "select", "esc", "cancel"))
	return b.String()
}

// viewDashboardBody renders the top-level sessions overview: active
// sessions, "New session ▸", and agent/directory-independent utilities.
func (m rootModel) viewDashboardBody() string {
	var b strings.Builder

	b.WriteString(styleTitle.Render("smith-jail"))
	b.WriteString("  ")
	b.WriteString(styleSubtitle.Render("Docker jail launcher for AI coding agents"))
	b.WriteString("\n")
	b.WriteString(m.renderDockerStatus())
	b.WriteString("\n\n")

	b.WriteString(m.renderSessionsList())
	b.WriteString("\n\n")

	if m.status != "" {
		b.WriteString(styleWarn.Render("! " + m.status))
		b.WriteString("\n\n")
	}

	b.WriteString(helpLine("↑/↓", "move", "enter", "select", "esc", "quit"))

	return b.String()
}

// viewNewSessionMenu renders the submenu everything about configuring or
// launching a session for the currently selected agent+directory lives in
// — what the dashboard itself used to be before it grew a sessions
// overview.
func (m rootModel) viewNewSessionMenu() string {
	var b strings.Builder

	b.WriteString(styleTitle.Render("New session"))
	b.WriteString("\n\n")

	b.WriteString(m.renderReadyBanner())
	b.WriteString("\n\n")

	b.WriteString(m.renderNewSessionList())
	b.WriteString("\n\n")

	if details := m.renderSessionDetails(); details != "" {
		b.WriteString(details)
		b.WriteString("\n\n")
	}

	if m.status != "" {
		b.WriteString(styleWarn.Render("! " + m.status))
		b.WriteString("\n\n")
	}

	b.WriteString(stylePreview.Render(m.renderPreview()))
	b.WriteString("\n\n")
	b.WriteString(helpLine("↑/↓", "move", "enter", "select", "esc", "back"))

	return b.String()
}

// renderReadyBanner is the dashboard's headline: whatever it says is exactly
// what pressing Enter on "Run" (the default highlight) would do right now.
func (m rootModel) renderReadyBanner() string {
	title, hue := m.bannerTitle(m.agent())

	width := 60
	if m.width > 0 {
		width = min(max(m.width-6, 30), 76)
	}

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(hue).
		Padding(0, 1).
		Width(width)

	sub := truncate(m.dir, max(10, width-2)) + "  ·  " + m.projectStatusPlain()
	content := lipgloss.NewStyle().Bold(true).Foreground(hue).Render(title) +
		"\n" + styleMuted.Render(sub)
	return box.Render(content)
}

// bannerTitle reduces the same readiness checks renderImageStatus makes to a
// single headline and an accent color for the banner's border and title.
func (m rootModel) bannerTitle(a *Agent) (string, lipgloss.AdaptiveColor) {
	if m.dockerErr != nil {
		return "Docker is unavailable", hueErr
	}
	if fi, err := os.Stat(m.dir); err != nil || !fi.IsDir() {
		return "Not a directory: " + truncate(m.dir, 50), hueErr
	}

	switch m.imageStatusPlain(a) {
	case "…", "unknown":
		return "Checking " + a.DisplayName + "…", hueFaint
	case "error":
		return "Image error for " + a.DisplayName, hueErr
	case "will build":
		return "Launch will build " + a.DisplayName, hueWarn
	default:
		return "Ready — " + a.DisplayName, hueOK
	}
}

// renderNewSessionList draws the New Session submenu's always-focused list.
// The highlighted row renders as a solid background bar (highlightRow);
// every other row keeps its normal per-field coloring.
func (m rootModel) renderNewSessionList() string {
	a := m.agent()
	width := 60
	if m.width > 0 {
		width = max(30, m.width-4)
	}

	var b strings.Builder
	for i, it := range newSessionItems {
		label := padRight(it.label, hubLabelWidth)
		var detailPlain, detailStyled string

		switch it.row {
		case rowChangeAgent:
			widget := a.DisplayName + "  ▸"
			detailPlain = widget + "   " + m.imageStatusPlain(a)
			detailStyled = styleAccent.Render(widget) + "   " + m.renderImageStatus(a)

		case rowChangeProject:
			dir := truncate(m.dir, max(10, width-hubLabelWidth-26))
			detailPlain = dir + "   " + m.projectStatusPlain()
			detailStyled = styleAccent.Render(dir) + "   " + m.projectStatusStyled()

		case rowOllamaModel:
			if m.cfg != nil {
				detailPlain = m.cfg.OllamaModel
				detailStyled = styleAccent.Render(m.cfg.OllamaModel)
			}
		}

		if i == m.hubCursor {
			b.WriteString(highlightRow(width, "  "+label+detailPlain))
		} else {
			b.WriteString("  " + styleRow.Render(label) + detailStyled)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderSessionsList draws the dashboard's top-level list: active sessions,
// then "New session ▸", then dashGlobalItems, all under one flat cursor
// (m.dashCursor) — the single entry point into everything the TUI can do.
func (m rootModel) renderSessionsList() string {
	width := 60
	if m.width > 0 {
		width = max(30, m.width-4)
	}

	var b strings.Builder
	b.WriteString(styleCardTitleActive.Render("  Active sessions"))
	b.WriteString("\n")

	switch {
	case !m.sessionsLoaded:
		b.WriteString("  " + styleMuted.Render("checking…") + "\n")
	case m.sessionsErr != nil:
		b.WriteString("  " + styleErr.Render(m.sessionsErr.Error()) + "\n")
	case len(m.sessions) == 0:
		b.WriteString("  " + styleMuted.Render("none running") + "\n")
	default:
		for i, s := range m.sessions {
			label := padRight(s.Agent.DisplayName, hubLabelWidth)
			dir := truncate(s.Dir, max(10, width-hubLabelWidth-22))
			since := formatRuntime(time.Since(s.Since)) + " ago"

			plain := "  " + label + dir + "   " + since
			styled := "  " + styleAccent.Render(label) + styleValue.Render(dir) + "   " + styleMuted.Render(since)

			if i == m.dashCursor {
				b.WriteString(highlightRow(width, plain))
			} else {
				b.WriteString(styled)
			}
			b.WriteString("\n")
		}
	}
	b.WriteString("\n")

	newSessionRow := "  New session ▸"
	if m.dashCursor == len(m.sessions) {
		b.WriteString(highlightRow(width, newSessionRow))
	} else {
		b.WriteString(styleAccent.Render(newSessionRow))
	}
	b.WriteString("\n\n")

	a := m.agent()
	for i, it := range dashGlobalItems {
		idx := len(m.sessions) + 1 + i
		label := padRight(it.label, hubLabelWidth)
		var detailPlain, detailStyled string

		if it.row == rowCheckUpdates {
			vw := max(10, width-hubLabelWidth-2)
			if v := m.versionLinePlain(a, vw); v != "" {
				detailPlain = v
				detailStyled = m.renderVersionLine(a, vw)
			}
		}

		if idx == m.dashCursor {
			b.WriteString(highlightRow(width, "  "+label+detailPlain))
		} else {
			b.WriteString("  " + styleRow.Render(label) + detailStyled)
		}
		b.WriteString("\n")
	}

	return strings.TrimRight(b.String(), "\n")
}

func (m rootModel) renderDockerStatus() string {
	switch {
	case !m.dockerChecked:
		return styleMuted.Render("  checking Docker…")
	case m.dockerErr != nil:
		// CheckDocker embeds multi-line remediation hints; the first line is
		// the diagnosis and the rest is advice, so show both.
		lines := strings.Split(m.dockerErr.Error(), "\n")
		out := styleErr.Render("  Docker: " + lines[0])
		for _, l := range lines[1:] {
			out += "\n" + styleMuted.Render("  "+strings.TrimSpace(l))
		}
		return out
	default:
		return styleOK.Render("  Docker: connected")
	}
}

// sessionField renders one label/value row in the right-hand info column. An
// empty key indents a continuation line under the row above it. The width is
// one wider than the longest label ("Directory") so a full-width label still
// leaves a gap before its value.
func sessionField(k, v string) string {
	return styleLabel.Render(fmt.Sprintf("  %-10s", k)) + v + "\n"
}

// imageStatusPlain reports whether a launch would build or reuse the image
// for the current project — the same decision EnsureImage makes at run
// time. Tags are content-addressed (Config.BuildHash), so there's no
// separate "stale" state to report: st.info reflects exactly the tag this
// project would run, and either it exists or a launch builds it. It is
// plain text so it can be reused both under color (renderImageStatus) and,
// on whichever row currently has the highlight, inside the solid background
// bar that a colored inner span would otherwise break.
func (m rootModel) imageStatusPlain(a *Agent) string {
	st := m.images[a.Name]
	switch {
	case m.dockerErr != nil:
		return "unknown"
	case st == nil || !st.loaded:
		return "…"
	case st.err != nil:
		return "error"
	case !st.info.Exists:
		return "will build"
	default:
		return "ready"
	}
}

func (m rootModel) renderImageStatus(a *Agent) string {
	switch text := m.imageStatusPlain(a); text {
	case "error":
		return styleErr.Render(text)
	case "will build":
		return styleWarn.Render(text)
	case "ready":
		return styleOK.Render(text)
	default:
		return styleMuted.Render(text)
	}
}

// versionLinePlain shows the result of an update check, or an invitation to
// run one. It stays blank-ish until asked because the check is expensive.
func (m rootModel) versionLinePlain(a *Agent, width int) string {
	st := m.images[a.Name]

	switch {
	case st == nil:
		return ""
	case m.checkingUpdates && !st.checked:
		return "checking…"
	case !st.checked:
		return ""
	case st.updateAvailable():
		return truncate("↑ "+st.installed+" → "+st.latest, width)
	case st.installed != "" && st.latest == "":
		// No registry to compare against (Hermes ships no npm package), so
		// report the version without implying it is current.
		return truncate("v"+st.installed, width)
	case st.installed != "":
		return truncate("v"+st.installed+" · latest", width)
	default:
		return "version unknown"
	}
}

func (m rootModel) renderVersionLine(a *Agent, width int) string {
	text := m.versionLinePlain(a, width)
	if text == "" {
		return ""
	}
	if st := m.images[a.Name]; st != nil && st.checked && st.updateAvailable() {
		return styleWarn.Render(text)
	}
	return styleMuted.Render(text)
}

// renderSessionDetails summarises the jail settings the selected launch would
// use. Project and Agent are their own selectors above this, not fields here.
func (m rootModel) renderSessionDetails() string {
	if m.cfgErr != nil {
		return strings.TrimRight(sessionField("Config", styleErr.Render(m.cfgErr.Error())), "\n")
	}
	if m.cfg == nil {
		return ""
	}

	var b strings.Builder
	b.WriteString(sessionField("Base", styleValue.Render(m.cfg.BaseImage)))
	b.WriteString(sessionField("Auth", styleValue.Render(agentAuthDesc(m.agent(), m.cfg))))

	if m.cfg.SkipPermissions {
		b.WriteString(sessionField("Perms", styleWarn.Render("skip (autonomous mode)")))
	} else {
		b.WriteString(sessionField("Perms", styleValue.Render("prompt on each action")))
	}
	if m.cfg.NetworkJailEnabled {
		b.WriteString(sessionField("Network", styleWarn.Render("jailed (agent API only)")))
	} else {
		b.WriteString(sessionField("Network", styleValue.Render("bridge (unrestricted)")))
	}
	b.WriteString(sessionField("Limits", styleValue.Render(m.cfg.MemLimit+" memory · "+m.cfg.CPULimit+" CPU")))

	return strings.TrimRight(b.String(), "\n")
}

// projectStatusPlain flags whether the current directory is a project
// smith-jail has seen before, since "Change project" only helps once the
// user knows there's a choice to make.
func (m rootModel) projectStatusPlain() string {
	if !m.recentsLoaded {
		return "checking history…"
	}
	if r, ok := m.recentProjectFor(m.dir); ok {
		if r.Used.IsZero() {
			return "known project"
		}
		return "last used " + r.Used.Format("2006-01-02")
	}
	return "new project"
}

func (m rootModel) projectStatusStyled() string {
	s := m.projectStatusPlain()
	if s == "new project" {
		return styleWarn.Render(s)
	}
	return styleMuted.Render(s)
}

// renderPreview shows the CLI command the current selection is equivalent to,
// so the TUI teaches the command line rather than hiding it.
func (m rootModel) renderPreview() string {
	cmd := fmt.Sprintf("$ smith-jail %s run %s", m.agent().Name, m.dir)
	if m.width > 0 {
		cmd = truncate(cmd, max(10, m.width-26))
	}
	return cmd
}

// truncate shortens s to at most n display columns, marking elision with "…".
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= n {
		return s
	}
	r := []rune(s)
	if n == 1 {
		return "…"
	}
	return string(r[:n-1]) + "…"
}
