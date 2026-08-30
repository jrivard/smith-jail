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
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// modelPickerModel picks which Ollama model tag hermes-local points at —
// OLLAMA_MODEL in ollama.env. Unlike settingsModel/pkgModel, there's no
// Global/Project scope toggle here: the sidecar and its model are shared
// across every hermes-local session, not layered per project (see
// ollama.go's OllamaContainerName doc).
type modelPickerModel struct {
	width, height int

	cfg *Config

	current string // cfg.OllamaModel as of the last load/save
	pulled  []string
	loaded  bool
	loadErr error

	items  []string // current + OllamaKnownModels + pulled, deduplicated
	cursor int

	adding bool
	input  textinput.Model

	status    string
	failed    bool
	cancelled bool
}

func newModelPickerModel() modelPickerModel {
	ti := textinput.New()
	ti.Prompt = "model tag: "
	ti.Placeholder = "e.g. qwen3:8b"
	ti.CharLimit = 128
	return modelPickerModel{input: ti}
}

// reset rebuilds the item list from cfg's current model. The caller is
// responsible for kicking off ollamaModelsCmd to learn what's actually
// pulled — this only seeds from cfg and OllamaKnownModels, both instant.
func (m modelPickerModel) reset(cfg *Config) modelPickerModel {
	m.cfg = cfg
	m.cursor = 0
	m.adding = false
	m.input.Blur()
	m.input.SetValue("")
	m.status, m.failed, m.cancelled = "", false, false
	m.loaded, m.loadErr, m.pulled = false, nil, nil
	m.current = ""
	if cfg != nil {
		m.current = cfg.OllamaModel
	}
	m.items = m.buildItems()
	return m
}

// buildItems merges the current model, the built-in known-good suggestions,
// and whatever's actually pulled into the sidecar into one deduplicated,
// ordered list.
func (m modelPickerModel) buildItems() []string {
	var items []string
	seen := map[string]bool{}
	add := func(tag string) {
		if tag == "" || seen[tag] {
			return
		}
		seen[tag] = true
		items = append(items, tag)
	}
	add(m.current)
	for _, tag := range OllamaKnownModels {
		add(tag)
	}
	for _, tag := range m.pulled {
		add(tag)
	}
	return items
}

func (m modelPickerModel) setSize(w, h int) modelPickerModel {
	m.width, m.height = w, h
	m.input.Width = max(20, w-24)
	return m
}

// rowCount is the number of selectable rows: one per item, plus a trailing
// "type a custom tag" row.
func (m modelPickerModel) rowCount() int { return len(m.items) + 1 }

func (m modelPickerModel) onCustomRow() bool { return m.cursor == len(m.items) }

// ── Messages / commands ─────────────────────────────────────────────────────

type ollamaModelsLoadedMsg struct {
	models []string
	err    error
}

func ollamaModelsCmd() tea.Cmd {
	return func() tea.Msg {
		models, err := OllamaModelsPulled()
		return ollamaModelsLoadedMsg{models: models, err: err}
	}
}

// ── Update ────────────────────────────────────────────────────────────────────

func (m modelPickerModel) Update(msg tea.KeyMsg) (modelPickerModel, tea.Cmd) {
	if m.adding {
		return m.updateAdding(msg)
	}

	switch msg.String() {
	case "esc", "q":
		m.cancelled = true
		return m, nil

	case "ctrl+c":
		return m, tea.Quit

	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
		return m, nil

	case "down", "j":
		if m.cursor < m.rowCount()-1 {
			m.cursor++
		}
		return m, nil

	case "enter", " ":
		return m.activate()
	}

	return m, nil
}

func (m modelPickerModel) activate() (modelPickerModel, tea.Cmd) {
	m.status, m.failed = "", false

	if m.onCustomRow() {
		m.adding = true
		m.input.SetValue("")
		return m, m.input.Focus()
	}

	return m.selectModel(m.items[m.cursor])
}

func (m modelPickerModel) updateAdding(msg tea.KeyMsg) (modelPickerModel, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.adding = false
		m.input.Blur()
		return m, nil

	case "enter":
		tag := strings.TrimSpace(m.input.Value())
		if tag == "" {
			m.status, m.failed = "Model tag cannot be empty.", true
			return m, nil
		}
		m.adding = false
		m.input.Blur()
		return m.selectModel(tag)
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// selectModel writes tag to ollama.env as OLLAMA_MODEL and asks the root
// model to reload config. It doesn't pull the model itself —
// EnsureOllamaModelPulled already handles that (prompting first) the next
// time hermes-local runs.
func (m modelPickerModel) selectModel(tag string) (modelPickerModel, tea.Cmd) {
	if m.cfg == nil {
		m.status, m.failed = "No config loaded — cannot save.", true
		return m, nil
	}
	if tag == m.current {
		m.status, m.failed = tag+" is already the selected model.", false
		return m, nil
	}

	if err := m.cfg.WriteEnvTemplates(); err != nil {
		m.status, m.failed = "Could not create config files: "+err.Error(), true
		return m, nil
	}
	if err := SetEnvValues(m.cfg.OllamaEnvFile, map[string]string{"OLLAMA_MODEL": tag}); err != nil {
		m.status, m.failed = "Save failed: "+err.Error(), true
		return m, nil
	}

	m.current = tag
	m.items = m.buildItems()
	for i, it := range m.items {
		if it == tag {
			m.cursor = i
		}
	}
	m.status, m.failed = "Saved. "+tag+" will be pulled automatically next time hermes-local runs, if it isn't cached already.", false
	return m, reloadConfigCmd(m.cfg.ProjectDir)
}

// ── View ──────────────────────────────────────────────────────────────────────

func (m modelPickerModel) View() string {
	var b strings.Builder

	b.WriteString(styleTitle.Render("Ollama model"))
	b.WriteString("\n")
	b.WriteString(styleSubtitle.Render("  " + m.fileDescription()))
	b.WriteString("\n\n")

	b.WriteString(m.renderList())
	b.WriteString("\n\n")

	if m.adding {
		b.WriteString("  " + m.input.View())
		b.WriteString("\n\n")
	}

	b.WriteString(styleWarn.Render("  Only " + strings.Join(OllamaKnownModels, ", ") +
		" is verified to return real tool_calls for Hermes — anything else may need"))
	b.WriteString("\n")
	b.WriteString(styleWarn.Render("  hand-verifying against /v1/chat/completions before you trust it (see ollama.env)."))
	b.WriteString("\n")

	switch {
	case !m.loaded:
		b.WriteString(styleMuted.Render("  Checking what's already pulled into the sidecar…"))
		b.WriteString("\n")
	case m.loadErr != nil:
		b.WriteString(styleMuted.Render("  Could not check the sidecar's pulled models: " + m.loadErr.Error()))
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
	if m.adding {
		b.WriteString(helpLine("enter", "save", "esc", "cancel"))
	} else {
		b.WriteString(helpLine("↑/↓", "move", "enter", "select", "esc", "back"))
	}

	return b.String()
}

func (m modelPickerModel) fileDescription() string {
	if m.cfg == nil {
		return "OLLAMA_MODEL — hermes-local's default model"
	}
	return "OLLAMA_MODEL, in " + m.cfg.OllamaEnvFile
}

func (m modelPickerModel) renderList() string {
	width := 60
	if m.width > 0 {
		width = max(30, m.width-4)
	}

	var b strings.Builder
	for i, tag := range m.items {
		label := tag
		var flags []string
		if tag == m.current {
			flags = append(flags, "current")
		}
		if containsString(m.pulled, tag) {
			flags = append(flags, "pulled")
		}
		if len(flags) > 0 {
			label += "  (" + strings.Join(flags, ", ") + ")"
		}

		switch {
		case i == m.cursor && !m.adding:
			b.WriteString(highlightRow(width, "  "+label))
		case tag == m.current:
			b.WriteString("  " + styleAccent.Render(label))
		default:
			b.WriteString("  " + styleRow.Render(label))
		}
		b.WriteString("\n")
	}

	customLabel := "Type a custom tag…"
	if m.onCustomRow() && !m.adding {
		b.WriteString(highlightRow(width, "  "+customLabel))
	} else {
		b.WriteString("  " + styleMuted.Render(customLabel))
	}

	return strings.TrimRight(b.String(), "\n")
}
