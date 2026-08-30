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
	"path/filepath"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// pickerRowKind identifies what a row in the picker list does.
type pickerRowKind int

const (
	prowUseCurrent pickerRowKind = iota
	prowRecent
	prowSelectPath
	prowTypePath
	prowUp
	prowEntry
)

// pickerRow is one selectable row. idx points back into m.recents or
// m.entries for the rows that need it; it is unused otherwise.
type pickerRow struct {
	kind pickerRowKind
	idx  int
}

// pickerMode distinguishes the top-level menu from the directory-browsing
// screen "Select a path" opens.
type pickerMode int

const (
	pickerMenu pickerMode = iota
	pickerBrowse
)

// pickerModel is the directory-selection screen. The top-level menu offers
// the current directory as its own row, a section of previously jailed
// projects, and two escape hatches: browse the filesystem, or type a path
// directly. "Select a path" drops into a scrollable directory browser; esc
// there returns to the menu rather than cancelling the whole picker.
type pickerModel struct {
	width, height int

	cursor int
	offset int
	mode   pickerMode

	recents []recentProject

	// start is the directory the picker was opened on — what "Current
	// directory" in the menu refers to, and where browsing starts from.
	start string

	// Browse state.
	path       string
	entries    []string // subdirectory names of path
	showHidden bool
	browseErr  error

	// Typed-path entry.
	typing bool
	input  textinput.Model

	status    string
	cancelled bool
}

func newPickerModel(start string) pickerModel {
	ti := textinput.New()
	ti.Prompt = "path: "
	ti.Placeholder = "/absolute/or/~/relative/path"
	ti.CharLimit = 4096

	m := pickerModel{start: start, path: start, input: ti}
	return m.readDir(start)
}

// reset returns the picker to its initial state for a fresh visit.
func (m pickerModel) reset(start string) pickerModel {
	m.cursor, m.offset = 0, 0
	m.mode = pickerMenu
	m.typing = false
	m.input.Blur()
	m.input.SetValue("")
	m.status = ""
	m.cancelled = false
	m.start = start
	return m.readDir(start)
}

func (m pickerModel) setSize(w, h int) pickerModel {
	m.width, m.height = w, h
	m.input.Width = max(20, w-16)
	return m
}

func (m pickerModel) setRecents(items []recentProject) pickerModel {
	m.recents = items
	return m
}

// readDir loads the subdirectories of path. A directory that cannot be read is
// reported inline rather than treated as fatal — an unreadable directory is
// still a legitimate place to stop and pick something else.
func (m pickerModel) readDir(path string) pickerModel {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	m.path = abs
	m.entries = nil
	m.browseErr = nil

	items, err := os.ReadDir(abs)
	if err != nil {
		m.browseErr = err
		return m
	}
	for _, it := range items {
		if !it.IsDir() {
			continue
		}
		if !m.showHidden && strings.HasPrefix(it.Name(), ".") {
			continue
		}
		m.entries = append(m.entries, it.Name())
	}
	sort.Strings(m.entries)
	return m
}

// hasUpRow reports whether the browsed path has a distinct parent to offer.
func (m pickerModel) hasUpRow() bool {
	return filepath.Dir(m.path) != m.path
}

// rows lays out every selectable row in the order it is displayed. In the
// menu, that's the current directory, then the recent-projects section, then
// the two escape hatches. In the browser, it's "use this directory", the
// parent (if any), and the subdirectories of the browsed path.
func (m pickerModel) rows() []pickerRow {
	if m.mode == pickerBrowse {
		rows := make([]pickerRow, 0, len(m.entries)+2)
		rows = append(rows, pickerRow{kind: prowUseCurrent})
		if m.hasUpRow() {
			rows = append(rows, pickerRow{kind: prowUp})
		}
		for i := range m.entries {
			rows = append(rows, pickerRow{kind: prowEntry, idx: i})
		}
		return rows
	}

	rows := make([]pickerRow, 0, len(m.recents)+3)
	rows = append(rows, pickerRow{kind: prowUseCurrent})
	for i := range m.recents {
		rows = append(rows, pickerRow{kind: prowRecent, idx: i})
	}
	rows = append(rows, pickerRow{kind: prowSelectPath})
	rows = append(rows, pickerRow{kind: prowTypePath})
	return rows
}

// visibleRows is how many list rows fit on screen, leaving room for the
// chrome above and below the list.
func (m pickerModel) visibleRows() int {
	if m.height <= 0 {
		return 10
	}
	return max(3, m.height-12)
}

func (m pickerModel) Update(msg tea.KeyMsg) (pickerModel, tea.Cmd) {
	if m.typing {
		return m.updateTyping(msg)
	}

	rows := m.rows()

	switch msg.String() {
	case "esc", "q":
		if m.mode == pickerBrowse {
			m.mode = pickerMenu
			m.cursor, m.offset = 0, 0
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
		if m.cursor < len(rows)-1 {
			m.cursor++
		}
		return m.clampOffset(), nil

	case "home", "g":
		m.cursor, m.offset = 0, 0
		return m, nil

	case "end", "G":
		m.cursor = max(0, len(rows)-1)
		return m.clampOffset(), nil

	case ".":
		if m.mode != pickerBrowse {
			return m, nil
		}
		m.showHidden = !m.showHidden
		m.cursor, m.offset = 0, 0
		return m.readDir(m.path), nil

	case "backspace":
		if m.mode == pickerBrowse && m.hasUpRow() {
			parent := filepath.Dir(m.path)
			m.cursor, m.offset = 0, 0
			return m.readDir(parent), nil
		}
		return m, nil

	case "enter":
		return m.commit(rows)
	}

	return m, nil
}

// commit acts on the row under the cursor.
func (m pickerModel) commit(rows []pickerRow) (pickerModel, tea.Cmd) {
	if m.cursor >= len(rows) {
		return m, nil
	}
	row := rows[m.cursor]

	switch row.kind {
	case prowUseCurrent:
		if m.mode == pickerBrowse {
			return m, chooseDir(m.path)
		}
		return m, chooseDir(m.start)

	case prowRecent:
		return m, chooseDir(m.recents[row.idx].Dir)

	case prowSelectPath:
		m.mode = pickerBrowse
		m.cursor, m.offset = 0, 0
		return m.readDir(m.start), nil

	case prowTypePath:
		m.typing = true
		m.input.SetValue(m.start)
		m.input.CursorEnd()
		return m, m.input.Focus()

	case prowUp:
		m.cursor, m.offset = 0, 0
		return m.readDir(filepath.Dir(m.path)), nil

	case prowEntry:
		target := filepath.Join(m.path, m.entries[row.idx])
		m.cursor, m.offset = 0, 0
		return m.readDir(target), nil
	}
	return m, nil
}

func (m pickerModel) updateTyping(msg tea.KeyMsg) (pickerModel, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.typing = false
		m.input.Blur()
		return m, nil

	case "enter":
		path := expandPath(strings.TrimSpace(m.input.Value()))
		if path == "" {
			m.typing = false
			m.input.Blur()
			return m, nil
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			m.status = "Cannot resolve path: " + err.Error()
			return m, nil
		}
		fi, err := os.Stat(abs)
		if err != nil {
			m.status = "Does not exist: " + abs
			return m, nil
		}
		if !fi.IsDir() {
			m.status = "Not a directory: " + abs
			return m, nil
		}
		m.typing = false
		m.input.Blur()
		return m, chooseDir(abs)
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	m.status = ""
	return m, cmd
}

// clampOffset scrolls the window so the cursor stays visible.
func (m pickerModel) clampOffset() pickerModel {
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

// chooseDir reports a committed selection back to the root model.
func chooseDir(dir string) tea.Cmd {
	return func() tea.Msg { return dirChosenMsg{dir: dir} }
}

// expandPath resolves a leading ~ against the user's home directory.
func expandPath(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
		}
	}
	return p
}

// ── View ──────────────────────────────────────────────────────────────────────

func (m pickerModel) View() string {
	var b strings.Builder

	title := "Select project directory"
	if m.mode == pickerBrowse {
		title = "Select project directory   ·   " + m.path
	}
	b.WriteString(styleTitle.Render(title))
	b.WriteString("\n\n")

	b.WriteString(m.renderRows())
	b.WriteString("\n\n")

	if m.typing {
		b.WriteString("  " + m.input.View())
		b.WriteString("\n\n")
	}
	if m.status != "" {
		b.WriteString(styleWarn.Render("! " + m.status))
		b.WriteString("\n\n")
	}

	b.WriteString(m.renderHelp())
	return lipgloss.NewStyle().Padding(1, 2).Render(b.String())
}

// rowLabel renders one row's plain text (no color — used both for the
// highlighted row's background bar and, unless noted, everywhere else too,
// since these rows carry little semantic color).
func (m pickerModel) rowLabel(row pickerRow, width int) string {
	switch row.kind {
	case prowUseCurrent:
		if m.mode == pickerBrowse {
			return truncate("Use this directory   ·   "+m.path, width)
		}
		return truncate("Current directory   ·   "+m.start, width)

	case prowRecent:
		r := m.recents[row.idx]
		label := r.Dir
		if !r.Used.IsZero() {
			label += "   ·   last used " + r.Used.Format("2006-01-02")
		}
		return truncate(label, width)

	case prowSelectPath:
		return "Select a path…"

	case prowTypePath:
		return "Type a path…"

	case prowUp:
		return truncate(".. ("+filepath.Dir(m.path)+")", width)

	case prowEntry:
		return truncate(m.entries[row.idx]+"/", width)
	}
	return ""
}

// pickerRowGroup buckets menu row kinds into the three visually separated
// groups: the current directory, the recent-projects section, and the two
// escape-hatch actions (which sit together as one group).
func pickerRowGroup(k pickerRowKind) int {
	switch k {
	case prowUseCurrent:
		return 0
	case prowRecent:
		return 1
	default:
		return 2
	}
}

func (m pickerModel) renderRows() string {
	if m.browseErr != nil {
		return styleErr.Render("  cannot read directory: " + m.browseErr.Error())
	}

	rows := m.rows()
	width := 60
	if m.width > 0 {
		width = max(30, m.width-6)
	}

	visible := m.visibleRows()
	end := min(m.offset+visible, len(rows))

	var b strings.Builder
	for i := m.offset; i < end; i++ {
		if m.mode == pickerMenu && i > 0 && pickerRowGroup(rows[i].kind) != pickerRowGroup(rows[i-1].kind) {
			b.WriteString("\n")
			if rows[i].kind == prowRecent {
				b.WriteString(styleMuted.Render("  Recent"))
				b.WriteString("\n")
			}
		}
		label := m.rowLabel(rows[i], width-2)
		if i == m.cursor {
			b.WriteString(highlightRow(width, "  "+label))
		} else {
			b.WriteString("  " + styleRow.Render(label))
		}
		b.WriteString("\n")
	}

	if end < len(rows) || m.offset > 0 {
		b.WriteString(styleMuted.Render(fmt.Sprintf("  %d–%d of %d", m.offset+1, end, len(rows))))
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m pickerModel) renderHelp() string {
	if m.typing {
		return helpLine("enter", "select", "esc", "cancel")
	}
	if m.mode == pickerBrowse {
		return helpLine(
			"enter", "select",
			".", "hidden files",
			"esc", "back to menu",
		)
	}
	return helpLine("enter", "select", "esc", "cancel")
}
