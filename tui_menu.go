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

// hubRow identifies one row of an always-focused list on the TUI — either
// the "New session" submenu or the dashboard's global rows.
type hubRow int

const (
	rowRun hubRow = iota
	rowShell
	rowChangeAgent
	rowChangeProject
	rowSettings
	rowOllamaModel
	rowDockerfile
	rowArtifacts
	rowCheckUpdates
	rowDoctor
	rowHelp
	rowQuit
)

type hubItem struct {
	row   hubRow
	label string
}

// newSessionItems is the "New session" submenu: everything that configures
// or launches a session for the currently selected agent+directory.
var newSessionItems = []hubItem{
	{rowRun, "Run"},
	{rowShell, "Shell"},
	{rowChangeAgent, "Change agent"},
	{rowChangeProject, "Change project"},
	{rowSettings, "Settings"},
	{rowOllamaModel, "Ollama model"},
	{rowDockerfile, "Dockerfile"},
}

// dashGlobalItems are the dashboard's agent/directory-independent rows,
// reachable from the top level alongside the active sessions list and
// "New session ▸".
var dashGlobalItems = []hubItem{
	{rowArtifacts, "Docker artifacts"},
	{rowCheckUpdates, "Check updates"},
	{rowDoctor, "Doctor"},
	{rowHelp, "Help"},
	{rowQuit, "Quit"},
}

// hubLabelWidth is the fixed column every row's label is padded to, so
// per-row detail (agent name, directory, status) lines up underneath itself.
const hubLabelWidth = 16
