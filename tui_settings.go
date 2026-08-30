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
	"os"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// fieldKind distinguishes a toggle from a free-text setting.
type fieldKind int

const (
	fieldBool fieldKind = iota
	fieldText
)

// settingsScope selects which layer of the JAIL_* config the screen reads
// from and saves to: the global file, or the current project's override.
type settingsScope int

const (
	scopeGlobal settingsScope = iota
	scopeProject
)

// settingField is one editable line in the active scope's env file.
type settingField struct {
	Key     string
	Label   string
	Kind    fieldKind
	Help    string
	Value   string
	Saved   string
	Present bool // key literally exists in the active scope's file
	// SavedPresent is Present as of the last load/save — dirty() compares
	// against both, so clearing an override (Present flips without Value
	// changing) counts as a pending edit too.
	SavedPresent bool
	Rebuild      bool // whether changing this invalidates the build hash
}

func (f settingField) dirty() bool { return f.Value != f.Saved || f.Present != f.SavedPresent }

// settingsModel edits the non-package settings, either in smith-jail.env
// (global) or the current project's override file (see settingsScope).
type settingsModel struct {
	width, height int

	cfg    *Config
	scope  settingsScope
	fields []settingField
	cursor int

	editing bool
	input   textinput.Model

	status       string
	failed       bool
	cancelled    bool
	confirmed    bool
	openPackages bool // set by activate() when the Packages row is chosen
}

func newSettingsModel() settingsModel {
	ti := textinput.New()
	ti.CharLimit = 512
	return settingsModel{input: ti}
}

// reset rebuilds the field list from the given config, always starting on
// the Global scope.
func (m settingsModel) reset(cfg *Config) settingsModel {
	m.cfg = cfg
	m.scope = scopeGlobal
	m.cursor = 0
	m.editing = false
	m.input.Blur()
	m.status, m.failed, m.cancelled, m.confirmed, m.openPackages = "", false, false, false, false
	m.fields = nil

	if cfg == nil {
		return m
	}
	m.fields = buildSettingFields(cfg, m.scope)
	return m
}

// buildSettingFields reads the active scope's file (global smith-jail.env,
// or the current project's override) and builds one field per JAIL_* key.
// A key absent from that file seeds Value from cfg's fully merged value —
// a sensible starting point to edit, and in Project scope, exactly what
// the field would inherit if left alone — but Present stays false, so
// dirty()/rendering/save can tell "inherited" apart from "explicitly set
// to this value here".
func buildSettingFields(cfg *Config, scope settingsScope) []settingField {
	file := cfg.EnvFile
	if scope == scopeProject {
		file = cfg.ProjectScopeFile()
	}
	raw := RawJailValues(file)

	var fields []settingField
	add := func(key, label string, kind fieldKind, fallback, help string, rebuild bool) {
		value, present := raw[key]
		if !present {
			value = fallback
		}
		fields = append(fields, settingField{
			Key: key, Label: label, Kind: kind,
			Value: value, Saved: value,
			Present: present, SavedPresent: present,
			Help: help, Rebuild: rebuild,
		})
	}

	add("JAIL_BASE_IMAGE", "Base image", fieldText, cfg.BaseImage,
		"Debian-based image the agent environment is built from", true)
	add("JAIL_MEM_LIMIT", "Memory limit", fieldText, cfg.MemLimit,
		"container memory cap, e.g. 8g — exit 137 means this was hit", false)
	add("JAIL_CPU_LIMIT", "CPU limit", fieldText, cfg.CPULimit,
		"container CPU cap, e.g. 2.0", false)
	add("JAIL_SKIP_PERMISSIONS", "Skip permissions", fieldBool, boolValue(cfg.SkipPermissions),
		"run the agent autonomously — safe inside the jail", false)
	add("JAIL_RUN_AS_ROOT", "Run as root", fieldBool, boolValue(cfg.RunAsRoot),
		"run the whole session as root inside the container", true)
	add("JAIL_SUDO", "Passwordless sudo", fieldBool, boolValue(cfg.Sudo),
		"let the agent user sudo — ignored when running as root", true)
	add("JAIL_NETWORK_JAIL", "Network jail", fieldBool, boolValue(cfg.NetworkJailEnabled),
		"restrict outbound traffic to the agent API (Linux only, needs CAP_NET_ADMIN)", false)
	add("JAIL_NETWORK_ALLOW", "Extra allowed hosts", fieldText, strings.Join(cfg.NetworkAllowHosts, " "),
		"space-separated hosts permitted through the network jail", false)
	add("JAIL_AUTO_APPROVE", "Auto-approve", fieldBool, boolValue(cfg.AutoApprove),
		"skip confirmation prompts for image and volume creation", false)

	return fields
}

// scopeFile returns the file the active scope reads from and saves to.
func (m settingsModel) scopeFile() string {
	if m.cfg == nil {
		return ""
	}
	if m.scope == scopeProject {
		return m.cfg.ProjectScopeFile()
	}
	return m.cfg.EnvFile
}

// rowCount is the number of selectable rows: one virtual "Packages" row
// (cursor 0) in front of the real settings fields.
func (m settingsModel) rowCount() int { return len(m.fields) + 1 }

func (m settingsModel) onPackagesRow() bool { return m.cursor == 0 }

// field returns the settingField under the cursor. Callers must guard with
// onPackagesRow first — there is no field at cursor 0.
func (m settingsModel) field() settingField { return m.fields[m.cursor-1] }

func boolValue(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func (m settingsModel) setSize(w, h int) settingsModel {
	m.width, m.height = w, h
	m.input.Width = max(20, w-24)
	return m
}

func (m settingsModel) dirty() bool {
	for _, f := range m.fields {
		if f.dirty() {
			return true
		}
	}
	return false
}

func (m settingsModel) Update(msg tea.KeyMsg) (settingsModel, tea.Cmd) {
	if m.editing {
		return m.updateEditing(msg)
	}

	// Any action other than a repeated esc re-arms the discard guard.
	if key := msg.String(); key != "esc" && key != "q" {
		m.confirmed = false
	}

	switch msg.String() {
	case "esc", "q":
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
		m.confirmed = false
		return m, nil

	case "down", "j":
		if m.cursor < m.rowCount()-1 {
			m.cursor++
		}
		m.confirmed = false
		return m, nil

	case " ", "enter":
		return m.activate()

	case "u":
		for i := range m.fields {
			m.fields[i].Value = m.fields[i].Saved
			m.fields[i].Present = m.fields[i].SavedPresent
		}
		m.status, m.failed, m.confirmed = "Reverted to the saved settings.", false, false
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

// toggleScope switches between Global and Project scope, reloading fields
// from the newly active file. Refuses while dirty so an in-progress edit
// can't be silently dropped by switching away from it.
func (m settingsModel) toggleScope() (settingsModel, tea.Cmd) {
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
	m.cursor = 0
	m.status, m.failed = "", false
	if m.cfg != nil {
		m.fields = buildSettingFields(m.cfg, m.scope)
	}
	return m, nil
}

// clearOverride removes a field's Project-scope override, returning it to
// inheriting from Global/the built-in default. Only meaningful in Project
// scope, and only once the field actually has an override to remove.
func (m settingsModel) clearOverride() (settingsModel, tea.Cmd) {
	if m.scope != scopeProject || m.onPackagesRow() || !m.field().Present {
		return m, nil
	}
	m.fields[m.cursor-1].Present = false
	m.status, m.failed = "", false
	return m, nil
}

// activate toggles a boolean field, opens the editor for a text field, or —
// on the Packages row — asks the root model to switch to the Packages
// screen.
func (m settingsModel) activate() (settingsModel, tea.Cmd) {
	m.status, m.failed, m.confirmed = "", false, false

	if m.onPackagesRow() {
		m.openPackages = true
		return m, nil
	}

	f := m.field()

	if f.Kind == fieldBool {
		m.fields[m.cursor-1] = toggleBoolField(f, m.scope)
		return m, nil
	}

	m.editing = true
	m.input.Prompt = f.Label + ": "
	m.input.SetValue(f.Value)
	m.input.CursorEnd()
	return m, m.input.Focus()
}

// toggleBoolField advances a boolean field. Global scope stays the
// pre-existing plain on/off toggle. Project scope cycles a third state —
// Present false, meaning the key isn't in the project file at all and the
// field inherits whatever Global/the built-in default resolves to.
func toggleBoolField(f settingField, scope settingsScope) settingField {
	if scope == scopeGlobal {
		f.Value, f.Present = boolValue(f.Value != "true"), true
		return f
	}
	switch {
	case !f.Present:
		f.Value, f.Present = "true", true
	case f.Value == "true":
		f.Value = "false"
	default:
		f.Present = false
	}
	return f
}

func (m settingsModel) updateEditing(msg tea.KeyMsg) (settingsModel, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.editing = false
		m.input.Blur()
		return m, nil

	case "enter":
		value := strings.TrimSpace(m.input.Value())
		if err := validateSetting(m.field().Key, value); err != "" {
			m.status, m.failed = err, true
			return m, nil
		}
		m.fields[m.cursor-1].Value = value
		m.fields[m.cursor-1].Present = true
		m.editing = false
		m.input.Blur()
		return m, nil
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	m.status, m.failed = "", false
	return m, cmd
}

// validateSetting rejects values that would fail later inside Docker, where the
// error is far less obvious. It returns an empty string when the value is fine.
func validateSetting(key, value string) string {
	switch key {
	case "JAIL_MEM_LIMIT":
		if value == "" {
			return "Memory limit cannot be empty."
		}
		unit := value[len(value)-1]
		if unit != 'b' && unit != 'k' && unit != 'm' && unit != 'g' {
			return "Memory limit needs a unit suffix, e.g. 8g."
		}
		if _, err := strconv.ParseFloat(value[:len(value)-1], 64); err != nil {
			return "Memory limit must be a number followed by b, k, m, or g."
		}
	case "JAIL_CPU_LIMIT":
		n, err := strconv.ParseFloat(value, 64)
		if err != nil || n <= 0 {
			return "CPU limit must be a positive number, e.g. 2.0."
		}
	case "JAIL_BASE_IMAGE":
		if value == "" {
			return "Base image cannot be empty."
		}
	}
	return ""
}

// save writes every changed field to the active scope's file in a single
// rewrite: explicit values via SetEnvValues, cleared overrides via
// UnsetEnvValues.
func (m settingsModel) save() (settingsModel, tea.Cmd) {
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

	values := map[string]string{}
	var unset []string
	for _, f := range m.fields {
		if !f.dirty() {
			continue
		}
		if f.Present {
			values[f.Key] = f.Value
		} else {
			unset = append(unset, f.Key)
		}
	}

	if len(values) > 0 {
		if err := SetEnvValues(target, values); err != nil {
			m.status, m.failed = "Save failed: "+err.Error(), true
			return m, nil
		}
	}
	if len(unset) > 0 {
		if err := UnsetEnvValues(target, unset...); err != nil {
			m.status, m.failed = "Save failed: "+err.Error(), true
			return m, nil
		}
	}

	for i := range m.fields {
		m.fields[i].Saved = m.fields[i].Value
		m.fields[i].SavedPresent = m.fields[i].Present
	}
	m.status, m.failed, m.confirmed = "Saved to "+target, false, false
	return m, reloadConfigCmd(m.cfg.ProjectDir)
}

// ── View ──────────────────────────────────────────────────────────────────────

func (m settingsModel) View() string {
	var b strings.Builder

	b.WriteString(styleTitle.Render("Settings"))
	b.WriteString("\n")
	b.WriteString(styleSubtitle.Render("  " + m.scopeDescription()))
	b.WriteString("\n\n")

	if len(m.fields) == 0 {
		b.WriteString(styleErr.Render("  Config could not be loaded."))
		b.WriteString("\n\n")
		b.WriteString(helpLine("esc", "back"))
		return b.String()
	}

	b.WriteString(m.renderFields())
	b.WriteString("\n\n")

	if m.editing {
		b.WriteString("  " + m.input.View())
		b.WriteString("\n\n")
	}

	if !m.onPackagesRow() && !m.editing {
		if f := m.field(); f.Help != "" {
			b.WriteString(styleMuted.Render("  " + f.Help))
			b.WriteString("\n")
		}
	}
	if note := m.overrideNote(); note != "" {
		b.WriteString(styleWarn.Render("  " + note))
		b.WriteString("\n")
	}
	if impact := m.renderImpact(); impact != "" {
		b.WriteString(impact)
		b.WriteString("\n")
	}

	if m.status != "" {
		style := styleMuted
		if m.failed {
			style = styleErr
		}
		b.WriteString("\n" + style.Render("  "+m.status) + "\n")
	}

	b.WriteString("\n")
	switch {
	case m.editing:
		b.WriteString(helpLine("enter", "accept", "esc", "cancel"))
	case m.scope == scopeProject:
		b.WriteString(helpLine(
			"space", "toggle/edit",
			"c", "clear override",
			"tab", "scope",
			"u", "revert",
			"s", "save",
			"esc", "back",
		))
	default:
		b.WriteString(helpLine(
			"space", "toggle/edit",
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
func (m settingsModel) scopeDescription() string {
	if m.cfg == nil {
		return "jail configuration"
	}
	if m.scope == scopeGlobal {
		return "editing: Global (" + m.cfg.EnvFile + ")"
	}
	return "editing: Project (" + m.scopeFile() + ")"
}

func (m settingsModel) renderFields() string {
	width := 60
	if m.width > 0 {
		width = max(30, m.width-4)
	}

	var b strings.Builder

	packagesLabel := padRight("Packages ▸", 22)
	if m.onPackagesRow() {
		b.WriteString(highlightRow(width, "  "+packagesLabel))
	} else {
		b.WriteString("  " + styleRow.Render(packagesLabel))
	}
	b.WriteString("\n")

	for i, f := range m.fields {
		selected := i+1 == m.cursor
		label := padRight(f.Label, 22)

		inherited := m.scope == scopeProject && !f.Present

		var valuePlain string
		switch {
		case f.Kind == fieldBool:
			valuePlain = map[bool]string{true: "on", false: "off"}[f.Value == "true"]
		case f.Value == "":
			valuePlain = "(unset)"
		default:
			valuePlain = truncate(f.Value, max(10, width-32))
		}
		if inherited {
			valuePlain += "  (inherited)"
		}
		if f.dirty() {
			valuePlain += "  *"
		}

		if selected {
			b.WriteString(highlightRow(width, "  "+label+valuePlain))
		} else {
			var value string
			switch {
			case f.Kind == fieldBool && f.Value == "true":
				value = styleOK.Render("on")
			case f.Kind == fieldBool:
				value = styleMuted.Render("off")
			case f.Value == "":
				value = styleMuted.Render("(unset)")
			default:
				value = styleValue.Render(truncate(f.Value, max(10, width-32)))
			}
			if inherited {
				value += styleMuted.Render("  (inherited)")
			}
			if f.dirty() {
				value += styleWarn.Render("  *")
			}
			b.WriteString("  " + styleRow.Render(label) + value)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// overrideNote warns when something outside the active scope shadows the
// field being edited, so a saved value that appears not to take effect is
// explainable rather than eerie: either a process environment variable
// (always the top layer), or — while editing Global — a project override
// that wins over whatever gets saved here.
func (m settingsModel) overrideNote() string {
	if m.onPackagesRow() {
		return ""
	}
	key := m.field().Key
	if os.Getenv(key) != "" {
		return "Note: " + key + " is set in your environment and overrides this file."
	}
	if m.scope == scopeGlobal && m.cfg != nil && m.cfg.ProjectOverridden[key] {
		return "Note: this project overrides " + key + " — editing here has no effect until that override is cleared (switch to Project scope with tab)."
	}
	return ""
}

// renderImpact reports which images a pending edit would invalidate.
func (m settingsModel) renderImpact() string {
	if m.cfg == nil || !m.dirty() {
		return ""
	}

	probe := *m.cfg
	touched := false
	for _, f := range m.fields {
		if !f.dirty() || !f.Rebuild {
			continue
		}
		touched = true
		switch f.Key {
		case "JAIL_BASE_IMAGE":
			probe.BaseImage = f.Value
		case "JAIL_RUN_AS_ROOT":
			probe.RunAsRoot = f.Value == "true"
		case "JAIL_SUDO":
			probe.Sudo = f.Value == "true"
		}
	}
	if !touched {
		return styleMuted.Render("  Pending changes take effect on the next launch.")
	}

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

// padRight pads to a display width, not a byte count — labels contain arrows
// and box characters whose byte length overstates the columns they occupy.
func padRight(s string, n int) string {
	w := lipgloss.Width(s)
	if w >= n {
		return s
	}
	return s + strings.Repeat(" ", n-w)
}
