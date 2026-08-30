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
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
)

// artifactTab selects which class of Docker object the browser is showing.
type artifactTab int

const (
	tabImages artifactTab = iota
	tabContainers
	tabVolumes
	tabNetworks
)

var artifactTabs = []struct {
	tab   artifactTab
	label string
}{
	{tabImages, "Images"},
	{tabContainers, "Containers"},
	{tabVolumes, "Volumes"},
	{tabNetworks, "Networks"},
}

// artifactRow is one selectable Docker object, flattened for display.
type artifactRow struct {
	ID      string // what removal is keyed on
	Name    string
	State   string
	Agent   string
	Project string
	Detail  string
	Missing bool // listed for context but nothing to remove (e.g. unbuilt image)

	Orphan       bool
	OrphanReason string
}

// artifactSet is a full snapshot across all four tabs.
type artifactSet struct {
	images     []artifactRow
	containers []artifactRow
	volumes    []artifactRow
	networks   []artifactRow
	err        error
}

// artifactsModel is the Docker artifact browser: inspect what smith-jail has
// created, select across tabs, and remove it.
type artifactsModel struct {
	width, height int

	set    artifactSet
	loaded bool

	tab    artifactTab
	cursor map[artifactTab]int
	offset map[artifactTab]int

	// selected is keyed by tab then row ID so a multi-tab selection survives
	// tab switching — cleaning a project usually spans containers and volumes.
	selected map[artifactTab]map[string]bool

	confirming bool
	working    bool

	status    string
	failed    bool
	cancelled bool
}

func newArtifactsModel() artifactsModel {
	return artifactsModel{
		cursor:   map[artifactTab]int{},
		offset:   map[artifactTab]int{},
		selected: map[artifactTab]map[string]bool{},
	}
}

func (m artifactsModel) reset() artifactsModel {
	m.loaded = false
	m.set = artifactSet{}
	m.cursor = map[artifactTab]int{}
	m.offset = map[artifactTab]int{}
	m.selected = map[artifactTab]map[string]bool{}
	m.confirming, m.working = false, false
	m.status, m.failed, m.cancelled = "", false, false
	return m
}

func (m artifactsModel) setSize(w, h int) artifactsModel {
	m.width, m.height = w, h
	return m
}

func (m artifactsModel) rows() []artifactRow { return m.rowsFor(m.tab) }

func (m artifactsModel) rowsFor(tab artifactTab) []artifactRow {
	switch tab {
	case tabImages:
		return m.set.images
	case tabContainers:
		return m.set.containers
	case tabVolumes:
		return m.set.volumes
	default:
		return m.set.networks
	}
}

func (m artifactsModel) visibleRows() int {
	if m.height <= 0 {
		return 10
	}
	return max(3, m.height-16)
}

// selectionCount totals selected rows across every tab.
func (m artifactsModel) selectionCount() int {
	n := 0
	for _, ids := range m.selected {
		n += len(ids)
	}
	return n
}

// orphanCount totals removable orphans across every tab.
func (m artifactsModel) orphanCount() int {
	n := 0
	for _, t := range artifactTabs {
		for _, row := range m.rowsFor(t.tab) {
			if row.Orphan && !row.Missing {
				n++
			}
		}
	}
	return n
}

func (m artifactsModel) Update(msg tea.KeyMsg) (artifactsModel, tea.Cmd) {
	if m.working {
		// Removal is in flight; swallow input rather than queue surprises.
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		return m, nil
	}
	if m.confirming {
		return m.updateConfirming(msg)
	}

	switch msg.String() {
	case "esc", "q":
		m.cancelled = true
		return m, nil

	case "ctrl+c":
		return m, tea.Quit

	case "tab", "right", "l":
		m.tab = artifactTab((int(m.tab) + 1) % len(artifactTabs))
		m.status = ""
		return m, nil

	case "shift+tab", "left", "h":
		m.tab = artifactTab((int(m.tab) - 1 + len(artifactTabs)) % len(artifactTabs))
		m.status = ""
		return m, nil

	case "up", "k":
		if c := m.cursor[m.tab]; c > 0 {
			m.cursor[m.tab] = c - 1
		}
		return m.clampOffset(), nil

	case "down", "j":
		if c := m.cursor[m.tab]; c < len(m.rows())-1 {
			m.cursor[m.tab] = c + 1
		}
		return m.clampOffset(), nil

	case "home", "g":
		m.cursor[m.tab], m.offset[m.tab] = 0, 0
		return m, nil

	case "end", "G":
		m.cursor[m.tab] = max(0, len(m.rows())-1)
		return m.clampOffset(), nil

	case " ":
		return m.toggleSelection(), nil

	case "A":
		return m.selectAllOnTab(), nil

	case "N":
		m.selected = map[artifactTab]map[string]bool{}
		m.status = ""
		return m, nil

	case "o":
		// Purge: replace the selection with every orphan across every tab and
		// go straight to the confirmation, which lists each one and why.
		n := m.orphanCount()
		if n == 0 {
			m.status, m.failed = "No orphaned artifacts found.", false
			return m, nil
		}
		m.selected = map[artifactTab]map[string]bool{}
		for _, t := range artifactTabs {
			for _, row := range m.rowsFor(t.tab) {
				if row.Orphan && !row.Missing {
					if m.selected[t.tab] == nil {
						m.selected[t.tab] = map[string]bool{}
					}
					m.selected[t.tab][row.ID] = true
				}
			}
		}
		m.confirming = true
		return m, nil

	case "r":
		m.loaded = false
		m.status = ""
		return m, loadArtifactsCmd()

	case "d", "x", "delete":
		if m.selectionCount() == 0 {
			// Nothing ticked: fall back to the row under the cursor, which is
			// what "delete" means everywhere else.
			m = m.toggleSelection()
		}
		if m.selectionCount() == 0 {
			m.status, m.failed = "Nothing selected.", false
			return m, nil
		}
		m.confirming = true
		return m, nil
	}

	return m, nil
}

func (m artifactsModel) updateConfirming(msg tea.KeyMsg) (artifactsModel, tea.Cmd) {
	switch msg.String() {
	case "y", "Y":
		m.confirming = false
		m.working = true
		m.status, m.failed = "Removing…", false
		return m, removeArtifactsCmd(m.selectionByTab())
	case "ctrl+c":
		return m, tea.Quit
	default:
		m.confirming = false
		m.status, m.failed = "Cancelled.", false
		return m, nil
	}
}

func (m artifactsModel) toggleSelection() artifactsModel {
	rows := m.rows()
	c := m.cursor[m.tab]
	if c >= len(rows) {
		return m
	}
	row := rows[c]
	if row.Missing {
		m.status, m.failed = "Nothing to remove for that entry.", false
		return m
	}

	if m.selected[m.tab] == nil {
		m.selected[m.tab] = map[string]bool{}
	}
	if m.selected[m.tab][row.ID] {
		delete(m.selected[m.tab], row.ID)
	} else {
		m.selected[m.tab][row.ID] = true
	}
	m.status = ""
	return m
}

func (m artifactsModel) selectAllOnTab() artifactsModel {
	if m.selected[m.tab] == nil {
		m.selected[m.tab] = map[string]bool{}
	}
	for _, row := range m.rows() {
		if !row.Missing {
			m.selected[m.tab][row.ID] = true
		}
	}
	m.status = ""
	return m
}

// selectionByTab flattens the selection into the shape the remover wants.
func (m artifactsModel) selectionByTab() map[artifactTab][]string {
	out := map[artifactTab][]string{}
	for tab, ids := range m.selected {
		for id := range ids {
			out[tab] = append(out[tab], id)
		}
	}
	return out
}

func (m artifactsModel) clampOffset() artifactsModel {
	rows := m.visibleRows()
	c, off := m.cursor[m.tab], m.offset[m.tab]
	if c < off {
		off = c
	}
	if c >= off+rows {
		off = c - rows + 1
	}
	if off < 0 {
		off = 0
	}
	m.offset[m.tab] = off
	return m
}

// ── Data loading ──────────────────────────────────────────────────────────────

type artifactsLoadedMsg struct{ set artifactSet }

type artifactsRemovedMsg struct {
	removed int
	err     error
}

func loadArtifactsCmd() tea.Cmd {
	return func() tea.Msg { return artifactsLoadedMsg{set: loadArtifacts()} }
}

// loadArtifacts gathers every smith-jail-managed Docker object. It reuses the
// same listing helpers the CLI uses, so the browser and "smith-jail show" can
// never disagree about what exists.
func loadArtifacts() artifactSet {
	var set artifactSet

	// One row per (agent, effective configuration) tag, since base
	// image/packages are project-specific now and a single agent can have
	// several images — one per distinct project configuration.
	for _, a := range AllAgents {
		imgs, err := ListAgentImages(a)
		switch {
		case err != nil:
			set.images = append(set.images, artifactRow{
				ID: a.ImageName(), Name: a.ImageName(), Agent: a.Name,
				State: "error", Missing: true, Detail: err.Error(),
			})
		case len(imgs) == 0:
			set.images = append(set.images, artifactRow{
				ID: a.ImageName(), Name: a.ImageName(), Agent: a.Name,
				State: "not built", Missing: true,
				Detail: "runs: smith-jail " + a.Name + " rebuild",
			})
		default:
			for _, img := range imgs {
				tags := img.RepoTags
				if len(tags) == 0 {
					tags = []string{a.ImageName() + ":<none>"}
				}
				detail := formatSize(img.Size) + " · " + time.Unix(img.Created, 0).Format("2006-01-02")
				for _, tag := range tags {
					set.images = append(set.images, artifactRow{
						ID: tag, Name: tag, Agent: a.Name,
						State: "built", Detail: detail,
					})
				}
			}
		}
	}
	// Managed-looking images belonging to no current agent — left behind when
	// an agent is renamed or dropped from the tool.
	for _, tag := range listStrayImages() {
		set.images = append(set.images, artifactRow{ID: tag, Name: tag, State: "built"})
	}
	set.images = append(set.images, listDanglingImages()...)

	containers, volumes := ListAllResources(nil)
	for _, c := range containers {
		name := c.ID
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		set.containers = append(set.containers, artifactRow{
			ID:      c.ID,
			Name:    name,
			State:   c.State,
			Agent:   c.Labels["smithjail.agent"],
			Project: c.Labels["smithjail.project"],
			Detail:  c.Status,
		})
	}
	for _, v := range volumes {
		set.volumes = append(set.volumes, artifactRow{
			ID:      v.Name,
			Name:    v.Name,
			Agent:   v.Labels["smithjail.agent"],
			Project: v.Labels["smithjail.project"],
		})
	}

	nets, err := ListNetworks(nil)
	if err != nil {
		set.err = err
	}
	for _, n := range nets {
		set.networks = append(set.networks, artifactRow{
			ID:      n.Name,
			Name:    n.Name,
			Agent:   n.Agent,
			Project: n.Project,
		})
	}

	markOrphans(&set)
	return set
}

// listDanglingImages returns untagged smith-jail images — the superseded layers
// a rebuild leaves behind, which are usually the largest thing to reclaim.
//
// The smithjail.managed label is what makes this safe: it is the only way to
// tell our untagged layer from any other project's build cache. Images built
// before labelling was added carry no marker and are deliberately not reported,
// since guessing would risk deleting something that is not ours.
func listDanglingImages() []artifactRow {
	cli, err := dockerClient()
	if err != nil {
		return nil
	}
	defer cli.Close()

	f := filters.NewArgs()
	f.Add("dangling", "true")
	f.Add("label", "smithjail.managed=true")

	imgs, err := cli.ImageList(context.Background(), image.ListOptions{Filters: f})
	if err != nil {
		return nil
	}

	rows := make([]artifactRow, 0, len(imgs))
	for _, img := range imgs {
		row := artifactRow{
			ID:           img.ID,
			Name:         "<untagged> " + shortImageID(img.ID),
			State:        "untagged",
			Agent:        img.Labels["smithjail.agent"],
			Detail:       formatSize(img.Size),
			Orphan:       true,
			OrphanReason: orphanDanglingImage,
		}
		if img.Created > 0 {
			row.Detail += " · " + time.Unix(img.Created, 0).Format("2006-01-02")
		}
		rows = append(rows, row)
	}
	return rows
}

// shortImageID trims the sha256: prefix Docker reports on image IDs.
func shortImageID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// listStrayImages returns smithjail-prefixed image tags that no current agent
// claims — left behind when an agent is renamed or dropped from the tool.
func listStrayImages() []string {
	cli, err := dockerClient()
	if err != nil {
		return nil
	}
	defer cli.Close()

	f := filters.NewArgs()
	f.Add("reference", "smithjail-*")

	imgs, err := cli.ImageList(context.Background(), image.ListOptions{Filters: f})
	if err != nil {
		return nil
	}

	known := map[string]bool{}
	for _, a := range AllAgents {
		known[a.ImageName()] = true
	}

	var out []string
	for _, img := range imgs {
		for _, tag := range img.RepoTags {
			name, _, _ := strings.Cut(tag, ":")
			if !known[name] {
				out = append(out, tag)
			}
		}
	}
	return out
}

// ── Orphan detection ──────────────────────────────────────────────────────────

// Orphan reasons, in the wording shown to the user.
const (
	orphanProjectGone   = "project directory no longer exists"
	orphanUnknownAgent  = "no such agent in this version of smith-jail"
	orphanIdleNetwork   = "session network with nothing attached"
	orphanDanglingImage = "untagged layer superseded by a rebuild"
)

// orphanScan is the snapshot every row is judged against, gathered once so the
// verdicts on one screen are mutually consistent.
type orphanScan struct {
	projectGone   map[string]bool // path -> confirmed absent
	activeProject map[string]bool // path -> has a running container
	netAttached   map[string]int  // network name -> attached containers
	knownAgent    map[string]bool
}

// markOrphans flags resources that nothing can reach any more.
//
// Detection is deliberately one-sided: a resource is only called an orphan when
// there is positive evidence it is stranded. Anything ambiguous — an unreadable
// project path, an unlabelled resource, a network Docker declined to inspect —
// is left alone, because the cost of a false positive here is destroying an
// agent's accumulated home state.
func markOrphans(set *artifactSet) {
	markOrphansWith(set, networkAttachments(set.networks))
}

// markOrphansWith is markOrphans with the network attachment counts supplied,
// so the verdict logic can be exercised without a Docker daemon.
func markOrphansWith(set *artifactSet, netAttached map[string]int) {
	scan := newOrphanScan(set, netAttached)

	for i, row := range set.containers {
		// A running container is by definition still in use, whatever its
		// labels say.
		if row.State == "running" {
			continue
		}
		if reason := scan.verdict(row); reason != "" {
			set.containers[i].Orphan = true
			set.containers[i].OrphanReason = reason
		}
	}

	for i, row := range set.volumes {
		if reason := scan.verdict(row); reason != "" {
			set.volumes[i].Orphan = true
			set.volumes[i].OrphanReason = reason
		}
	}

	for i, row := range set.networks {
		reason := scan.verdict(row)
		if reason == "" {
			// Networks exist only for the duration of a jailed session, so one
			// with no attachments outlived the run that created it — usually a
			// session killed before its deferred cleanup could fire.
			if n, known := scan.netAttached[row.Name]; known && n == 0 {
				reason = orphanIdleNetwork
			}
		}
		if reason != "" {
			set.networks[i].Orphan = true
			set.networks[i].OrphanReason = reason
		}
	}

	for i, row := range set.images {
		if row.Orphan {
			continue // already judged at collection time (dangling layers)
		}
		if !row.Missing && !scan.knownAgent[row.Agent] {
			set.images[i].Orphan = true
			set.images[i].OrphanReason = orphanUnknownAgent
		}
	}
}

func newOrphanScan(set *artifactSet, netAttached map[string]int) *orphanScan {
	s := &orphanScan{
		projectGone:   map[string]bool{},
		activeProject: map[string]bool{},
		netAttached:   netAttached,
		knownAgent:    map[string]bool{},
	}

	for _, a := range AllAgents {
		s.knownAgent[a.Name] = true
	}

	for _, c := range set.containers {
		if c.State == "running" && c.Project != "" {
			s.activeProject[c.Project] = true
		}
	}

	paths := map[string]bool{}
	for _, rows := range [][]artifactRow{set.containers, set.volumes, set.networks} {
		for _, r := range rows {
			if r.Project != "" {
				paths[r.Project] = true
			}
		}
	}
	for p := range paths {
		// Only os.IsNotExist counts. A permission error, or a path on an
		// unmounted volume, means "cannot tell" — not "gone".
		if _, err := os.Stat(p); err != nil && os.IsNotExist(err) {
			s.projectGone[p] = true
		}
	}

	return s
}

// verdict returns the orphan reason for a row, or "" if it looks live.
func (s *orphanScan) verdict(row artifactRow) string {
	if row.Agent != "" && !s.knownAgent[row.Agent] {
		return orphanUnknownAgent
	}
	if row.Project == "" {
		return "" // unlabelled: nothing to judge it against
	}
	if s.activeProject[row.Project] {
		return ""
	}
	if s.projectGone[row.Project] {
		return orphanProjectGone
	}
	return ""
}

// networkAttachments counts containers attached to each managed network. A
// network missing from the result is one Docker would not describe, and is
// therefore never judged.
func networkAttachments(rows []artifactRow) map[string]int {
	out := map[string]int{}
	if len(rows) == 0 {
		return out
	}

	cli, err := dockerClient()
	if err != nil {
		return out
	}
	defer cli.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for _, row := range rows {
		info, err := cli.NetworkInspect(ctx, row.ID, network.InspectOptions{})
		if err != nil {
			continue
		}
		out[row.Name] = len(info.Containers)
	}
	return out
}

// ── Removal ───────────────────────────────────────────────────────────────────

// removeArtifactsCmd deletes the selected objects.
//
// The CLI's RemoveContainerList/RemoveVolumeList/RemoveImage helpers are not
// reused here: they report progress by writing to stdout, which would corrupt
// the alternate screen. These variants return errors instead, which is what a
// TUI needs anyway.
func removeArtifactsCmd(selection map[artifactTab][]string) tea.Cmd {
	return func() tea.Msg {
		cli, err := dockerClient()
		if err != nil {
			return artifactsRemovedMsg{err: err}
		}
		defer cli.Close()

		ctx := context.Background()
		removed := 0
		var errs []string

		note := func(kind, id string, err error) {
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s %s: %v", kind, shortID(id), err))
				return
			}
			removed++
		}

		// Containers first: a volume or network still attached to one cannot
		// be removed, and this ordering makes a whole-project sweep succeed in
		// a single pass.
		for _, id := range selection[tabContainers] {
			note("container", id, cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true}))
		}
		for _, name := range selection[tabVolumes] {
			note("volume", name, cli.VolumeRemove(ctx, name, true))
		}
		for _, name := range selection[tabNetworks] {
			note("network", name, cli.NetworkRemove(ctx, name))
		}
		for _, name := range selection[tabImages] {
			_, err := cli.ImageRemove(ctx, name, image.RemoveOptions{Force: true, PruneChildren: true})
			note("image", name, err)
		}

		if len(errs) > 0 {
			return artifactsRemovedMsg{
				removed: removed,
				err:     fmt.Errorf("%s", strings.Join(errs, "; ")),
			}
		}
		return artifactsRemovedMsg{removed: removed}
	}
}

// shortID abbreviates the 64-character hex IDs Docker uses for containers and
// networks. Names — images, volumes — are left intact, since truncating
// "smithjail-cursor:latest" to "smithjail-cu" only obscures which object failed.
func shortID(id string) string {
	if len(id) != 64 || strings.Trim(id, "0123456789abcdef") != "" {
		return id
	}
	return id[:12]
}

// ── View ──────────────────────────────────────────────────────────────────────

func (m artifactsModel) View() string {
	var b strings.Builder

	b.WriteString(styleTitle.Render("Docker artifacts"))
	b.WriteString("\n")
	b.WriteString(styleSubtitle.Render("  images, containers, volumes and networks managed by smith-jail"))
	b.WriteString("\n\n")

	b.WriteString(m.renderTabs())
	b.WriteString("\n\n")

	if m.confirming {
		b.WriteString(m.renderConfirm())
		return b.String()
	}

	b.WriteString(m.renderRows())
	b.WriteString("\n\n")
	b.WriteString(m.renderDetail())
	b.WriteString("\n")

	if n := m.orphanCount(); n > 0 {
		b.WriteString(styleWarn.Render(fmt.Sprintf(
			"  %d orphaned object(s) — press o to review and purge", n)))
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
	b.WriteString(helpLine(
		"space", "select",
		"d", "remove",
		"o", "purge orphans",
		"A", "all",
		"N", "none",
		"r", "refresh",
		"esc", "back",
	))

	return b.String()
}

func (m artifactsModel) renderTabs() string {
	counts := []int{len(m.set.images), len(m.set.containers), len(m.set.volumes), len(m.set.networks)}

	out := "  "
	for i, t := range artifactTabs {
		label := fmt.Sprintf("%s (%d)", t.label, counts[i])
		if n := len(m.selected[t.tab]); n > 0 {
			label += fmt.Sprintf(" ✓%d", n)
		}
		if m.tab == t.tab {
			out += styleRowActive.Render("[ "+label+" ]") + " "
		} else {
			out += styleMuted.Render("  "+label+"  ") + " "
		}
	}
	return out
}

func (m artifactsModel) renderRows() string {
	if !m.loaded {
		return styleMuted.Render("  loading…")
	}
	if m.set.err != nil && m.tab == tabNetworks {
		return styleErr.Render("  could not list networks: " + m.set.err.Error())
	}

	rows := m.rows()
	if len(rows) == 0 {
		return styleMuted.Render("  nothing here")
	}

	visible := m.visibleRows()
	off := m.offset[m.tab]
	end := min(off+visible, len(rows))
	nameW := max(20, min(46, m.width-40))
	rowWidth := 60
	if m.width > 0 {
		rowWidth = max(30, m.width-4)
	}

	var b strings.Builder
	for i := off; i < end; i++ {
		row := rows[i]
		selected := i == m.cursor[m.tab]

		checkPlain := "  "
		if m.selected[m.tab][row.ID] {
			checkPlain = "✓ "
		} else if row.Missing {
			checkPlain = "· "
		}
		name := padRight(truncate(row.Name, nameW), nameW)
		agent := padRight("", 10)
		if row.Agent != "" {
			agent = padRight("["+row.Agent+"]", 10)
		}

		if selected {
			plain := "  " + checkPlain + name
			if row.State != "" {
				plain += "  " + padRight(row.State, 10)
			}
			plain += "  " + agent
			if row.Orphan {
				plain += "  orphan"
			}
			b.WriteString(highlightRow(rowWidth, plain))
		} else {
			check := "  "
			if m.selected[m.tab][row.ID] {
				check = styleOK.Render("✓ ")
			} else if row.Missing {
				check = styleMuted.Render("· ")
			}
			line := "  " + check + styleRow.Render(name)
			if row.State != "" {
				line += "  " + m.stateStyle(row.State).Render(padRight(row.State, 10))
			}
			if row.Agent != "" {
				line += "  " + styleAccent.Render(agent)
			} else {
				line += "  " + agent
			}
			if row.Orphan {
				line += "  " + styleWarn.Render("orphan")
			}
			b.WriteString(line)
		}
		b.WriteString("\n")
	}

	if end < len(rows) || off > 0 {
		b.WriteString(styleMuted.Render(fmt.Sprintf("  %d–%d of %d", off+1, end, len(rows))))
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m artifactsModel) stateStyle(state string) lipgloss.Style {
	switch state {
	case "running", "built":
		return styleOK
	case "error":
		return styleErr
	case "not built":
		return styleMuted
	default:
		return styleWarn
	}
}

// renderDetail expands whatever the cursor is on, since names alone rarely say
// which project a volume belongs to.
func (m artifactsModel) renderDetail() string {
	rows := m.rows()
	c := m.cursor[m.tab]
	if !m.loaded || c >= len(rows) {
		return ""
	}
	row := rows[c]

	var b strings.Builder
	if row.Project != "" {
		b.WriteString(styleLabel.Render("  project  ") +
			styleValue.Render(truncate(row.Project, max(10, m.width-14))) + "\n")
	}
	if row.Detail != "" {
		b.WriteString(styleLabel.Render("  detail   ") +
			styleValue.Render(truncate(row.Detail, max(10, m.width-14))) + "\n")
	}
	if row.Orphan {
		b.WriteString(styleLabel.Render("  orphan   ") +
			styleWarn.Render(truncate(row.OrphanReason, max(10, m.width-14))) + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m artifactsModel) renderConfirm() string {
	var b strings.Builder

	b.WriteString(styleErr.Render("  Remove the following?"))
	b.WriteString("\n\n")

	pathsClaimedGone := map[string]bool{}

	for _, t := range artifactTabs {
		ids := m.selected[t.tab]
		if len(ids) == 0 {
			continue
		}
		for _, row := range m.rowsFor(t.tab) {
			if !ids[row.ID] {
				continue
			}
			b.WriteString(styleValue.Render(fmt.Sprintf("    %-11s %s",
				strings.TrimSuffix(strings.ToLower(t.label), "s")+":",
				truncate(row.Name, max(10, m.width-20)))))
			if row.Orphan {
				b.WriteString(styleWarn.Render("  " + row.OrphanReason))
			}
			b.WriteString("\n")

			if row.Orphan && row.OrphanReason == orphanProjectGone {
				pathsClaimedGone[row.Project] = true
			}
		}
	}

	// The project-gone verdict is the one that can be wrong for a benign
	// reason, so show the paths and say plainly what would make it wrong.
	if len(pathsClaimedGone) > 0 {
		b.WriteString("\n")
		b.WriteString(styleWarn.Render("  Treated as deleted projects:"))
		b.WriteString("\n")
		for p := range pathsClaimedGone {
			b.WriteString(styleMuted.Render("    " + truncate(p, max(10, m.width-8))))
			b.WriteString("\n")
		}
		b.WriteString(styleWarn.Render("  A path on an unmounted drive looks identical — check before purging."))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(styleWarn.Render("  Your project files and credentials are not affected."))
	b.WriteString("\n")
	b.WriteString(styleMuted.Render("  Removing a home volume discards that project's agent state (git config, caches)."))
	b.WriteString("\n\n")
	b.WriteString(helpLine("y", "remove", "any other key", "cancel"))

	return b.String()
}
