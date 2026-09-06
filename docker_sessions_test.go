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
	"testing"

	"github.com/docker/docker/api/types"
)

func containerFixture(name string, created int64, labels map[string]string) types.Container {
	return types.Container{
		Names:   []string{"/" + name},
		Created: created,
		Labels:  labels,
	}
}

func TestActiveSessionsFromContainersDedupesRunAndShell(t *testing.T) {
	containers := []types.Container{
		containerFixture("smithjail-claude-abc123", 1000,
			map[string]string{"smithjail.agent": "claude", "smithjail.project": "/home/me/proj"}),
		containerFixture("smithjail-claude-abc123-shell", 2000,
			map[string]string{"smithjail.agent": "claude", "smithjail.project": "/home/me/proj"}),
	}

	sessions := activeSessionsFromContainers(containers)

	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1 (deduped): %+v", len(sessions), sessions)
	}
	if sessions[0].Since.Unix() != 1000 {
		t.Errorf("Since = %v, want the earlier of the two start times", sessions[0].Since)
	}
}

func TestActiveSessionsFromContainersExcludesProxy(t *testing.T) {
	containers := []types.Container{
		containerFixture("smithjail-claude-proxy-deadbeef", 1000,
			map[string]string{
				"smithjail.agent": "claude", "smithjail.project": "/home/me/proj",
				"smithjail.role": "proxy",
			}),
	}

	sessions := activeSessionsFromContainers(containers)
	if len(sessions) != 0 {
		t.Fatalf("proxy sidecar should never appear as a session, got %+v", sessions)
	}
}

func TestActiveSessionsFromContainersExcludesOllamaSidecar(t *testing.T) {
	containers := []types.Container{
		containerFixture(OllamaContainerName, 1000, map[string]string{
			"smithjail.agent": "hermes-local", "smithjail.project": "/whatever",
		}),
	}

	sessions := activeSessionsFromContainers(containers)
	if len(sessions) != 0 {
		t.Fatalf("Ollama sidecar should never appear as a session, got %+v", sessions)
	}
}

func TestActiveSessionsFromContainersSkipsUnlabelledOrUnknownAgent(t *testing.T) {
	containers := []types.Container{
		containerFixture("smithjail-claude-nolabels", 1000, nil),
		containerFixture("smithjail-mystery-xyz", 1000,
			map[string]string{"smithjail.agent": "not-a-real-agent", "smithjail.project": "/x"}),
	}

	sessions := activeSessionsFromContainers(containers)
	if len(sessions) != 0 {
		t.Fatalf("unlabelled/unresolvable containers should be skipped, got %+v", sessions)
	}
}

func TestActiveSessionsFromContainersDistinctProjectsBothAppear(t *testing.T) {
	containers := []types.Container{
		containerFixture("smithjail-claude-aaa", 1000,
			map[string]string{"smithjail.agent": "claude", "smithjail.project": "/proj/a"}),
		containerFixture("smithjail-claude-bbb", 2000,
			map[string]string{"smithjail.agent": "claude", "smithjail.project": "/proj/b"}),
	}

	sessions := activeSessionsFromContainers(containers)
	if len(sessions) != 2 {
		t.Fatalf("got %d sessions, want 2 distinct projects: %+v", len(sessions), sessions)
	}
}

func TestListActiveSessionsSortsMostRecentFirst(t *testing.T) {
	containers := []types.Container{
		containerFixture("smithjail-claude-old", 1000,
			map[string]string{"smithjail.agent": "claude", "smithjail.project": "/proj/old"}),
		containerFixture("smithjail-gemini-new", 2000,
			map[string]string{"smithjail.agent": "gemini", "smithjail.project": "/proj/new"}),
	}

	sessions := activeSessionsFromContainers(containers)
	sortSessionsByRecency(sessions)

	if len(sessions) != 2 || sessions[0].Dir != "/proj/new" {
		t.Fatalf("expected the more recently started session first, got %+v", sessions)
	}
}
