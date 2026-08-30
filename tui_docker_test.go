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
	"path/filepath"
	"testing"
)

// gonePath returns a path that is guaranteed not to exist.
func gonePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "deleted-project")
}

func TestOrphanRunningContainerIsNeverOrphaned(t *testing.T) {
	gone := gonePath(t)
	set := artifactSet{
		containers: []artifactRow{
			{ID: "c1", Name: "smithjail-claude-aaa", State: "running", Agent: "claude", Project: gone},
		},
	}

	markOrphansWith(&set, nil)

	if set.containers[0].Orphan {
		t.Error("a running container must never be flagged, whatever its labels say")
	}
}

func TestOrphanExitedContainerWithDeletedProject(t *testing.T) {
	gone := gonePath(t)
	set := artifactSet{
		containers: []artifactRow{
			{ID: "c1", Name: "smithjail-claude-aaa", State: "exited", Agent: "claude", Project: gone},
		},
	}

	markOrphansWith(&set, nil)

	if !set.containers[0].Orphan {
		t.Fatal("exited container for a deleted project should be an orphan")
	}
	if got := set.containers[0].OrphanReason; got != orphanProjectGone {
		t.Errorf("reason = %q, want %q", got, orphanProjectGone)
	}
}

// A volume must survive as long as any container is still running for its
// project, even if the bind-mount path has since vanished underneath it.
func TestOrphanVolumeSparedWhileProjectHasRunningContainer(t *testing.T) {
	gone := gonePath(t)
	set := artifactSet{
		containers: []artifactRow{
			{ID: "c1", Name: "smithjail-claude-aaa", State: "running", Agent: "claude", Project: gone},
		},
		volumes: []artifactRow{
			{ID: "v1", Name: "smithjail-claude-home-aaa", Agent: "claude", Project: gone},
		},
	}

	markOrphansWith(&set, nil)

	if set.volumes[0].Orphan {
		t.Error("volume must be spared while its project still has a running container")
	}
}

func TestOrphanLiveProjectIsNotOrphaned(t *testing.T) {
	live := t.TempDir() // exists
	set := artifactSet{
		volumes: []artifactRow{
			{ID: "v1", Name: "smithjail-claude-home-aaa", Agent: "claude", Project: live},
		},
	}

	markOrphansWith(&set, nil)

	if set.volumes[0].Orphan {
		t.Error("a volume whose project directory exists is not an orphan")
	}
}

// Unlabelled resources predate labelling; there is nothing to judge them
// against, so they must be left alone rather than assumed dead.
func TestOrphanUnlabelledResourceIsLeftAlone(t *testing.T) {
	set := artifactSet{
		volumes: []artifactRow{
			{ID: "v1", Name: "smithjail-claude-home-aaa"},
		},
	}

	markOrphansWith(&set, nil)

	if set.volumes[0].Orphan {
		t.Error("an unlabelled volume must not be flagged")
	}
}

func TestOrphanUnknownAgent(t *testing.T) {
	live := t.TempDir()
	set := artifactSet{
		volumes: []artifactRow{
			{ID: "v1", Name: "smithjail-cursor-home-aaa", Agent: "cursor", Project: live},
		},
	}

	markOrphansWith(&set, nil)

	if !set.volumes[0].Orphan {
		t.Fatal("a resource for an agent this build does not know is an orphan")
	}
	if got := set.volumes[0].OrphanReason; got != orphanUnknownAgent {
		t.Errorf("reason = %q, want %q", got, orphanUnknownAgent)
	}
}

func TestOrphanIdleNetwork(t *testing.T) {
	live := t.TempDir()
	set := artifactSet{
		networks: []artifactRow{
			{ID: "n1", Name: "smithjail-claude-net-aaa", Agent: "claude", Project: live},
			{ID: "n2", Name: "smithjail-claude-net-bbb", Agent: "claude", Project: live},
		},
	}

	markOrphansWith(&set, map[string]int{
		"smithjail-claude-net-aaa": 0, // session ended without cleanup
		"smithjail-claude-net-bbb": 1, // still in use
	})

	if !set.networks[0].Orphan {
		t.Error("a session network with nothing attached should be an orphan")
	}
	if got := set.networks[0].OrphanReason; got != orphanIdleNetwork {
		t.Errorf("reason = %q, want %q", got, orphanIdleNetwork)
	}
	if set.networks[1].Orphan {
		t.Error("a network with an attached container must not be flagged")
	}
}

// A network Docker declined to describe is absent from the attachment map. That
// is "cannot tell", and must not be read as "nothing attached".
func TestOrphanUninspectableNetworkIsNotJudged(t *testing.T) {
	live := t.TempDir()
	set := artifactSet{
		networks: []artifactRow{
			{ID: "n1", Name: "smithjail-claude-net-aaa", Agent: "claude", Project: live},
		},
	}

	markOrphansWith(&set, map[string]int{}) // inspect failed for every network

	if set.networks[0].Orphan {
		t.Error("a network that could not be inspected must not be flagged")
	}
}

func TestOrphanUnbuiltImageIsNotFlagged(t *testing.T) {
	set := artifactSet{
		images: []artifactRow{
			{ID: "smithjail-hermes", Name: "smithjail-hermes", Agent: "hermes",
				State: "not built", Missing: true},
			{ID: "smithjail-claude", Name: "smithjail-claude", Agent: "claude", State: "built"},
		},
	}

	markOrphansWith(&set, nil)

	for _, row := range set.images {
		if row.Orphan {
			t.Errorf("image %q for a known agent must not be flagged", row.Name)
		}
	}
}

func TestOrphanStrayImageIsFlagged(t *testing.T) {
	set := artifactSet{
		images: []artifactRow{
			{ID: "smithjail-cursor:latest", Name: "smithjail-cursor:latest", State: "built"},
		},
	}

	markOrphansWith(&set, nil)

	if !set.images[0].Orphan {
		t.Fatal("a smithjail-prefixed image with no owning agent is an orphan")
	}
	if got := set.images[0].OrphanReason; got != orphanUnknownAgent {
		t.Errorf("reason = %q, want %q", got, orphanUnknownAgent)
	}
}

// Dangling layers are judged when collected, because only the smithjail.managed
// label proves they are ours. The agent-based pass must not overwrite that
// verdict with a weaker one — the label says "claude", which is a known agent.
func TestOrphanDanglingImageVerdictSurvives(t *testing.T) {
	set := artifactSet{
		images: []artifactRow{
			{ID: "sha256:abc", Name: "<untagged> abc", Agent: "claude", State: "untagged",
				Orphan: true, OrphanReason: orphanDanglingImage},
		},
	}

	markOrphansWith(&set, nil)

	if !set.images[0].Orphan {
		t.Fatal("dangling image lost its orphan flag")
	}
	if got := set.images[0].OrphanReason; got != orphanDanglingImage {
		t.Errorf("reason = %q, want %q", got, orphanDanglingImage)
	}
}

// The purge selection must never include rows that have nothing to remove.
func TestOrphanCountExcludesMissingRows(t *testing.T) {
	m := newArtifactsModel()
	m.loaded = true
	m.set = artifactSet{
		images: []artifactRow{
			{ID: "i1", Name: "gone", Orphan: true, Missing: true},
			{ID: "i2", Name: "stray", Orphan: true},
		},
		volumes: []artifactRow{
			{ID: "v1", Name: "vol", Orphan: true},
			{ID: "v2", Name: "live"},
		},
	}

	if got := m.orphanCount(); got != 2 {
		t.Errorf("orphanCount = %d, want 2 (missing rows and live rows excluded)", got)
	}
}
