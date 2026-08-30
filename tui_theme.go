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

import "github.com/charmbracelet/lipgloss"

// Palette for the TUI. Values are adaptive so the interface stays legible on
// both light and dark terminal backgrounds.
var (
	hueAccent = lipgloss.AdaptiveColor{Light: "27", Dark: "39"}   // blue
	hueOK     = lipgloss.AdaptiveColor{Light: "28", Dark: "42"}   // green
	hueWarn   = lipgloss.AdaptiveColor{Light: "130", Dark: "214"} // amber
	hueErr    = lipgloss.AdaptiveColor{Light: "160", Dark: "203"} // red
	hueText   = lipgloss.AdaptiveColor{Light: "236", Dark: "252"}
	hueMuted  = lipgloss.AdaptiveColor{Light: "245", Dark: "244"}
	hueFaint  = lipgloss.AdaptiveColor{Light: "250", Dark: "238"}
)

var (
	styleTitle = lipgloss.NewStyle().
			Foreground(hueAccent).
			Bold(true)

	styleSubtitle = lipgloss.NewStyle().
			Foreground(hueMuted)

	styleLabel = lipgloss.NewStyle().
			Foreground(hueMuted)

	styleValue = lipgloss.NewStyle().
			Foreground(hueText)

	styleAccent = lipgloss.NewStyle().Foreground(hueAccent)
	styleOK     = lipgloss.NewStyle().Foreground(hueOK)
	styleWarn   = lipgloss.NewStyle().Foreground(hueWarn)
	styleErr    = lipgloss.NewStyle().Foreground(hueErr)
	styleMuted  = lipgloss.NewStyle().Foreground(hueMuted)

	// Agent cards: one bordered box per agent, highlighted when selected.
	styleCard = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(hueFaint).
			Padding(0, 1).
			MarginRight(1).
			Width(26)

	styleCardActive = styleCard.
			BorderForeground(hueAccent)

	styleCardTitle = lipgloss.NewStyle().Bold(true).Foreground(hueMuted)

	styleCardTitleActive = lipgloss.NewStyle().Bold(true).Foreground(hueAccent)

	// Selected row in a list.
	styleRowActive = lipgloss.NewStyle().
			Foreground(hueAccent).
			Bold(true)

	styleRow = lipgloss.NewStyle().Foreground(hueText)

	// styleRowSelected fills the highlighted row with a solid background bar
	// instead of relying on a marker glyph — render the full line through it
	// with .Width(rowWidth) set so the bar spans the row.
	styleRowSelected = lipgloss.NewStyle().
				Background(hueAccent).
				Foreground(lipgloss.AdaptiveColor{Light: "255", Dark: "255"}).
				Bold(true)

	// The command-preview strip and key hints along the bottom.
	stylePreview = lipgloss.NewStyle().
			Foreground(hueOK).
			Border(lipgloss.NormalBorder(), true, false, false, false).
			BorderForeground(hueFaint).
			PaddingTop(1)

	styleHelp = lipgloss.NewStyle().Foreground(hueMuted)

	styleKey = lipgloss.NewStyle().Foreground(hueAccent).Bold(true)
)

// highlightRow renders one row of an always-focused list as the current
// selection: a solid background bar spanning the row's width. plain must be
// plain text with no embedded ANSI styling — an inner style's own reset code
// would cancel the background for whatever follows it on the line, so
// per-field coloring (warn/ok/muted) is dropped for the row that is
// currently highlighted and shown only on the other rows.
func highlightRow(width int, plain string) string {
	return styleRowSelected.Width(max(width, lipgloss.Width(plain))).Render(plain)
}

// helpLine renders alternating key/description pairs as a single hint line.
func helpLine(pairs ...string) string {
	out := ""
	for i := 0; i+1 < len(pairs); i += 2 {
		if i > 0 {
			out += styleHelp.Render("  ·  ")
		}
		out += styleKey.Render(pairs[i]) + styleHelp.Render(" "+pairs[i+1])
	}
	return out
}
