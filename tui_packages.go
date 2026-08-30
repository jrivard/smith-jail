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
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// pkgModel edits JAIL_INIT_PACKAGES — the apt packages baked into every agent
// image — in either Global or Project scope (see settingsScope). Because the
// list feeds Config.BuildHash, the screen also reports which images the
// pending edit would invalidate.
type pkgModel struct {
	width, height int

	cfg   *Config
	scope settingsScope

	original []string
	pkgs     []string
	// present/originalPresent track whether JAIL_INIT_PACKAGES literally
	// exists in the active scope's file, independent of pkgs' contents —
	// same purpose as settingField.Present/SavedPresent.
	present         bool
	originalPresent bool
	cursor          int
	offset          int

	adding bool
	input  textinput.Model

	status    string
	failed    bool
	confirmed bool // an esc with unsaved changes has already been refused once
	cancelled bool
}

func newPkgModel() pkgModel {
	ti := textinput.New()
	ti.Prompt = "add: "
	ti.Placeholder = "package name (space-separated for several)"
	ti.CharLimit = 512

	return pkgModel{input: ti}
}

// reset reloads the screen from the given config, always starting on the
// Global scope.
func (m pkgModel) reset(cfg *Config) pkgModel {
	m.cfg = cfg
	m.scope = scopeGlobal
	m = m.loadScope()
	m.cursor, m.offset = 0, 0
	m.adding = false
	m.input.Blur()
	m.input.SetValue("")
	m.status, m.failed, m.confirmed, m.cancelled = "", false, false, false
	return m
}

// loadScope (re-)reads the package list from the active scope's file.
// Callers switching scope must guard on dirty() first — this discards
// whatever is currently in progress.
func (m pkgModel) loadScope() pkgModel {
	m.present, m.originalPresent = false, false
	m.original, m.pkgs = nil, nil
	if m.cfg == nil {
		return m
	}
	raw := RawJailValues(m.scopeFile())
	value, present := raw["JAIL_INIT_PACKAGES"]
	if !present {
		// Not overridden at this layer — seed from the fully merged value,
		// which in Project scope is exactly what's being inherited.
		value = m.cfg.InitPackages
	}
	m.present, m.originalPresent = present, present
	m.original = splitPackages(value)
	m.pkgs = append([]string(nil), m.original...)
	return m
}

// scopeFile returns the file the active scope reads from and saves to.
func (m pkgModel) scopeFile() string {
	if m.cfg == nil {
		return ""
	}
	if m.scope == scopeProject {
		return m.cfg.ProjectScopeFile()
	}
	return m.cfg.EnvFile
}

func (m pkgModel) setSize(w, h int) pkgModel {
	m.width, m.height = w, h
	m.input.Width = max(20, w-14)
	return m
}

// dirty reports whether the in-memory list (or its override status) differs
// from what is on disk.
func (m pkgModel) dirty() bool {
	if m.present != m.originalPresent {
		return true
	}
	if len(m.pkgs) != len(m.original) {
		return true
	}
	for i := range m.pkgs {
		if m.pkgs[i] != m.original[i] {
			return true
		}
	}
	return false
}

func (m pkgModel) visibleRows() int {
	if m.height <= 0 {
		return 10
	}
	return max(3, m.height-16)
}

func (m pkgModel) Update(msg tea.KeyMsg) (pkgModel, tea.Cmd) {
	if m.adding {
		return m.updateAdding(msg)
	}

	// Any action other than a repeated esc re-arms the discard guard.
	if key := msg.String(); key != "esc" && key != "q" {
		m.confirmed = false
	}

	switch msg.String() {
	case "esc", "q":
		// Refuse the first exit so an accidental esc cannot silently drop
		// edits; a second press confirms.
		if m.dirty() && !m.confirmed {
			m.status = "Unsaved changes — press esc again to discard, or s to save."
			m.failed = true
			m.confirmed = true
			return m, nil
		}
		m.cancelled = true
		return m, nil

	case "ctrl+c":
		return m, tea.Quit

	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
		return m.clampOffset(), nil

	case "down", "j":
		if m.cursor < len(m.pkgs)-1 {
			m.cursor++
		}
		return m.clampOffset(), nil

	case "home", "g":
		m.cursor, m.offset = 0, 0
		return m, nil

	case "end", "G":
		m.cursor = max(0, len(m.pkgs)-1)
		return m.clampOffset(), nil

	case "a", "+", "i":
		m.adding = true
		m.status, m.failed = "", false
		m.input.SetValue("")
		return m, m.input.Focus()

	case "d", "x", "delete":
		if m.cursor < len(m.pkgs) {
			m.pkgs = append(m.pkgs[:m.cursor], m.pkgs[m.cursor+1:]...)
			if m.cursor >= len(m.pkgs) {
				m.cursor = max(0, len(m.pkgs)-1)
			}
			m.present = true
			m.status, m.failed = "", false
		}
		return m.clampOffset(), nil

	case "u":
		m.pkgs = append([]string(nil), m.original...)
		m.present = m.originalPresent
		m.cursor, m.offset = 0, 0
		m.status, m.failed = "Reverted to the saved list.", false
		return m, nil

	case "s":
		return m.save()

	case "tab":
		return m.toggleScope()

	case "c":
		return m.clearOverride()
	}

	return m, nil
}

// toggleScope switches between Global and Project scope, reloading the list
// from the newly active file. Refuses while dirty so in-progress edits
// can't be silently dropped by switching away from them.
func (m pkgModel) toggleScope() (pkgModel, tea.Cmd) {
	if m.dirty() {
		m.status = "Unsaved changes — save (s) or revert (u) before switching scope."
		m.failed = true
		return m, nil
	}
	if m.scope == scopeGlobal {
		m.scope = scopeProject
	} else {
		m.scope = scopeGlobal
	}
	m = m.loadScope()
	m.cursor, m.offset = 0, 0
	m.status, m.failed = "", false
	return m, nil
}

// clearOverride removes the Project-scope override entirely, returning the
// list to inheriting from Global/the built-in default.
func (m pkgModel) clearOverride() (pkgModel, tea.Cmd) {
	if m.scope != scopeProject || !m.present {
		return m, nil
	}
	m.present = false
	m.status, m.failed = "", false
	return m, nil
}

func (m pkgModel) updateAdding(msg tea.KeyMsg) (pkgModel, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.adding = false
		m.input.Blur()
		return m, nil

	case "enter":
		// Accept several names at once: typing a line copied from a Dockerfile
		// is the common case.
		added := 0
		for _, p := range splitPackages(m.input.Value()) {
			if !containsString(m.pkgs, p) {
				m.pkgs = append(m.pkgs, p)
				added++
			}
		}
		m.adding = false
		m.input.Blur()
		m.input.SetValue("")
		if added > 0 {
			m.cursor = len(m.pkgs) - 1
			m.present = true
			m.status, m.failed = "", false
		} else {
			m.status, m.failed = "Already in the list.", false
		}
		return m.clampOffset(), nil
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// save writes the list back to the active scope's file and asks the root
// model to reload.
func (m pkgModel) save() (pkgModel, tea.Cmd) {
	if m.cfg == nil {
		m.status, m.failed = "No config loaded — cannot save.", true
		return m, nil
	}
	if !m.dirty() {
		m.status, m.failed = "No changes to save.", false
		return m, nil
	}

	target := m.scopeFile()

	// The global file ships with documentation worth writing under; a
	// project file is plain key=value and needs no template.
	if m.scope == scopeGlobal {
		if err := m.cfg.WriteEnvTemplates(); err != nil {
			m.status, m.failed = "Could not create config files: "+err.Error(), true
			return m, nil
		}
	}

	if m.present {
		value := strings.Join(m.pkgs, " ")
		if err := SetEnvValues(target, map[string]string{"JAIL_INIT_PACKAGES": value}); err != nil {
			m.status, m.failed = "Save failed: "+err.Error(), true
			return m, nil
		}
	} else if err := UnsetEnvValues(target, "JAIL_INIT_PACKAGES"); err != nil {
		m.status, m.failed = "Save failed: "+err.Error(), true
		return m, nil
	}

	m.original = append([]string(nil), m.pkgs...)
	m.originalPresent = m.present
	m.status, m.failed = "Saved to "+target, false
	return m, reloadConfigCmd(m.cfg.ProjectDir)
}

func (m pkgModel) clampOffset() pkgModel {
	rows := m.visibleRows()
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+rows {
		m.offset = m.cursor - rows + 1
	}
	if m.offset < 0 {
		m.offset = 0
	}
	return m
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// ── View ──────────────────────────────────────────────────────────────────────

func (m pkgModel) View() string {
	var b strings.Builder

	b.WriteString(styleTitle.Render("Container packages"))
	b.WriteString("\n")
	b.WriteString(styleSubtitle.Render("  " + m.scopeDescription()))
	b.WriteString("\n\n")

	b.WriteString(m.renderList())
	b.WriteString("\n\n")

	if m.adding {
		b.WriteString("  " + m.input.View())
		b.WriteString("\n\n")
	}

	if note := m.scopeNote(); note != "" {
		b.WriteString(styleMuted.Render("  " + note))
		b.WriteString("\n")
	}
	b.WriteString(m.renderImpact())
	b.WriteString("\n")

	if m.status != "" {
		style := styleMuted
		if m.failed {
			style = styleErr
		} else if m.dirty() {
			style = styleWarn
		}
		b.WriteString("\n")
		b.WriteString(style.Render("  " + m.status))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	switch {
	case m.adding:
		b.WriteString(helpLine("enter", "add", "esc", "cancel"))
	case m.scope == scopeProject:
		b.WriteString(helpLine(
			"a", "add",
			"d", "remove",
			"c", "clear override",
			"tab", "scope",
			"u", "revert",
			"s", "save",
			"esc", "back",
		))
	default:
		b.WriteString(helpLine(
			"a", "add",
			"d", "remove",
			"tab", "scope",
			"u", "revert",
			"s", "save",
			"esc", "back",
		))
	}

	return b.String()
}

// scopeDescription names the active scope and the file it reads from and
// saves to.
func (m pkgModel) scopeDescription() string {
	if m.cfg == nil {
		return "apt packages installed into every agent image (JAIL_INIT_PACKAGES)"
	}
	if m.scope == scopeGlobal {
		return "editing: Global (" + m.cfg.EnvFile + ")"
	}
	return "editing: Project (" + m.scopeFile() + ")"
}

// scopeNote flags that the list shown is inherited rather than an explicit
// override for this project, so editing it is understood to create one.
func (m pkgModel) scopeNote() string {
	if m.cfg == nil || m.scope != scopeProject || m.present {
		return ""
	}
	return "Inherited from Global/default — editing creates a project-specific override."
}

func (m pkgModel) renderList() string {
	if len(m.pkgs) == 0 {
		return styleMuted.Render("  no packages — press a to add one")
	}

	width := 60
	if m.width > 0 {
		width = max(30, m.width-4)
	}

	rows := m.visibleRows()
	end := min(m.offset+rows, len(m.pkgs))

	var b strings.Builder
	for i := m.offset; i < end; i++ {
		selected := i == m.cursor && !m.adding
		isNew := !containsString(m.original, m.pkgs[i])

		if selected {
			line := "  " + m.pkgs[i]
			if isNew {
				line += "  + new"
			}
			b.WriteString(highlightRow(width, line))
		} else {
			line := styleRow.Render("  " + m.pkgs[i])
			if isNew {
				line += styleOK.Render("  + new")
			}
			b.WriteString(line)
		}
		b.WriteString("\n")
	}

	// Removals are invisible in a list of what remains, so name them.
	var removed []string
	for _, p := range m.original {
		if !containsString(m.pkgs, p) {
			removed = append(removed, p)
		}
	}
	for _, p := range removed {
		b.WriteString(styleErr.Render("  − " + p))
		b.WriteString("\n")
	}

	if end < len(m.pkgs) || m.offset > 0 {
		b.WriteString(styleMuted.Render(fmt.Sprintf("  %d–%d of %d", m.offset+1, end, len(m.pkgs))))
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderImpact spells out the cost of the pending edit: which agents would
// need a new (or newly-shared) image tag built for this project.
func (m pkgModel) renderImpact() string {
	if m.cfg == nil {
		return ""
	}
	if !m.dirty() {
		return styleMuted.Render("  No pending changes.")
	}

	probe := *m.cfg
	probe.InitPackages = strings.Join(m.pkgs, " ")

	var affected []string
	for _, a := range AllAgents {
		if probe.BuildHash(a) != m.cfg.BuildHash(a) {
			affected = append(affected, a.Name)
		}
	}

	if len(affected) == 0 {
		return styleMuted.Render("  Saving this will not trigger a new build.")
	}
	return styleWarn.Render("  On save, next launch builds a new image tag for: " +
		strings.Join(affected, ", "))
}
