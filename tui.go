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
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-isatty"
)

// actionKind identifies what the user asked for before the TUI exited.
type actionKind int

const (
	actionNone actionKind = iota
	actionRun
	actionShell
)

// tuiAction is a request the TUI hands back to cmdTUI once it has released the
// terminal. Nothing is executed while the alternate screen is active — a jailed
// session needs the real terminal for "docker run -it".
type tuiAction struct {
	Kind  actionKind
	Agent *Agent
	Dir   string
}

// stdoutIsTTY reports whether taking over the screen is possible. When
// smith-jail is piped or redirected, callers fall back to the help text.
func stdoutIsTTY() bool {
	fd := os.Stdout.Fd()
	return isatty.IsTerminal(fd) || isatty.IsCygwinTerminal(fd)
}

// cmdTUI runs the interactive interface, then performs whatever the user
// selected. The selected action is dispatched through the ordinary CLI command
// path, so a TUI launch behaves identically to typing the command by hand.
func cmdTUI() {
	action, err := runTUI()
	if err != nil {
		die("TUI error: " + err.Error())
	}
	if action == nil || action.Kind == actionNone {
		return
	}

	rememberProject(action.Dir)

	// The TUI is fully torn down at this point: stdout is the real terminal
	// again, and the prompts inside cmdRun/cmdShell work as they always do.
	switch action.Kind {
	case actionRun:
		cmdRun(action.Agent, action.Dir, &InvokeOptions{}, nil)
	case actionShell:
		cmdShell(action.Agent, action.Dir, &InvokeOptions{}, nil)
	}
}

// runTUI drives the Bubble Tea program and returns the requested action, or
// nil if the user quit without launching anything.
func runTUI() (*tuiAction, error) {
	p := tea.NewProgram(newRootModel(), tea.WithAltScreen())
	final, err := p.Run()
	if err != nil {
		return nil, err
	}
	m, ok := final.(rootModel)
	if !ok {
		return nil, nil
	}
	return m.action, nil
}

// ── Recent projects ───────────────────────────────────────────────────────────

// recentProject is a directory the user has previously jailed.
type recentProject struct {
	Dir  string    `json:"dir"`
	Used time.Time `json:"used"`
}

const maxRecentProjects = 20

// recentsFile returns the path of the recent-projects cache. It lives beside
// the build cache so "clean" semantics stay unsurprising. The directory is
// resolved the same way LoadConfig resolves Config.BuildDir, but without its
// side effects — this is called on a hot path and only needs a path.
func recentsFile() string {
	xdgCache := os.Getenv("XDG_CACHE_HOME")
	if xdgCache == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		xdgCache = filepath.Join(home, ".cache")
	}
	return filepath.Join(xdgCache, "smith-jail", "recent.json")
}

// loadRecentProjects returns known project directories, most recent first.
// Docker labels are the primary source — every container and volume smith-jail
// creates is tagged with smithjail.project — with the on-disk cache filling in
// projects whose resources have since been cleaned.
func loadRecentProjects() []recentProject {
	seen := map[string]recentProject{}

	if path := recentsFile(); path != "" {
		if data, err := os.ReadFile(path); err == nil {
			var cached []recentProject
			if json.Unmarshal(data, &cached) == nil {
				for _, r := range cached {
					seen[r.Dir] = r
				}
			}
		}
	}

	containers, volumes := ListAllResources(nil)
	for _, c := range containers {
		if dir := c.Labels["smithjail.project"]; dir != "" {
			if _, ok := seen[dir]; !ok {
				seen[dir] = recentProject{Dir: dir, Used: time.Unix(c.Created, 0)}
			}
		}
	}
	for _, v := range volumes {
		if dir := v.Labels["smithjail.project"]; dir != "" {
			if _, ok := seen[dir]; !ok {
				seen[dir] = recentProject{Dir: dir}
			}
		}
	}

	out := make([]recentProject, 0, len(seen))
	for _, r := range seen {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Used.Equal(out[j].Used) {
			return out[i].Dir < out[j].Dir
		}
		return out[i].Used.After(out[j].Used)
	})
	if len(out) > maxRecentProjects {
		out = out[:maxRecentProjects]
	}
	return out
}

// rememberProject records a directory as most recently used. Failures are
// silent: the recents list is a convenience, never a prerequisite.
func rememberProject(dir string) {
	path := recentsFile()
	if path == "" || dir == "" {
		return
	}

	var list []recentProject
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &list)
	}

	out := []recentProject{{Dir: dir, Used: time.Now()}}
	for _, r := range list {
		if r.Dir != dir {
			out = append(out, r)
		}
	}
	if len(out) > maxRecentProjects {
		out = out[:maxRecentProjects]
	}

	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return
	}
	if os.MkdirAll(filepath.Dir(path), 0755) != nil {
		return
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, data, 0644) != nil {
		return
	}
	_ = os.Rename(tmp, path)
}
