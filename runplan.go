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

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/volume"
)

// RunPlan summarizes everything a run/shell/setup invocation would need to
// do to stand up a session — build an image, create a home volume, start the
// network jail, start the Ollama sidecar — gathered up front (read-only, no
// side effects) so it can be shown to the user as a single summary with one
// confirmation, instead of prompting separately for each resource as
// EnsureImage/EnsureHomeVolume/EnsureOllama* create it.
type RunPlan struct {
	agent *Agent
	cfg   *Config
	dir   string

	imageExists      bool
	imageRef         string
	imageBuildReason string

	homeVolumeExists bool
	homeVolumeName   string

	networkJail      bool
	networkName      string
	allowedHostCount int

	checkOllama      bool
	ollamaRunning    bool
	ollamaAction     string // "start" or "restart"; set when !ollamaRunning
	ollamaModel      string
	ollamaModelReady bool

	existingContainers []types.Container
	existingVolumes    []*volume.Volume
	existingNetworks   []NetworkItem
}

// buildRunPlan gathers a RunPlan without creating or starting anything.
// checkOllama should be true for commands that will call
// prepareOllamaSidecar (run, shell) and false for ones that won't (setup).
func buildRunPlan(agent *Agent, cfg *Config, opts *InvokeOptions, dir string, checkOllama bool) (*RunPlan, error) {
	hash := projectHash(dir)
	p := &RunPlan{
		agent:    agent,
		cfg:      cfg,
		dir:      dir,
		imageRef: agent.ImageRef(cfg),
	}

	exists, err := ImageExists(agent, cfg)
	if err != nil {
		return nil, fmt.Errorf("checking image: %w", err)
	}
	p.imageExists = exists
	if !exists {
		p.imageBuildReason = "first time"
		if AnyAgentImageExists(agent) {
			p.imageBuildReason = "new configuration for this project"
		}
	}

	p.homeVolumeName = homeVolumeName(agent, hash)
	volExists, err := homeVolumeExists(p.homeVolumeName)
	if err != nil {
		return nil, fmt.Errorf("checking home volume: %w", err)
	}
	p.homeVolumeExists = volExists

	if cfg.NetworkJailEnabled {
		p.networkJail = true
		p.networkName = agent.ContainerPrefix() + "-net-" + hash
		hosts, err := effectiveAllowedHosts(agent, cfg, opts)
		if err != nil {
			return nil, fmt.Errorf("computing allowed hosts: %w", err)
		}
		p.allowedHostCount = len(hosts)
	}

	if checkOllama && agent.Name == "hermes-local" {
		p.checkOllama = true
		exists, running, err := OllamaStatus()
		if err != nil {
			return nil, fmt.Errorf("checking Ollama sidecar: %w", err)
		}
		p.ollamaRunning = running
		if !running {
			p.ollamaAction = "start"
			if exists {
				p.ollamaAction = "restart"
			}
		}
		p.ollamaModel = cfg.OllamaModel
		if running && cfg.OllamaModel != "" {
			if present, err := ollamaModelPresent(cfg.OllamaModel); err == nil {
				p.ollamaModelReady = present
			}
		}
	}

	p.existingContainers, p.existingVolumes, p.existingNetworks = ListProjectResources(agent, hash)

	return p, nil
}

// needsConfirmation reports whether this plan would create or start
// anything. A fully warm session — image built, volume present, no jail,
// Ollama already serving the right model — has nothing to confirm.
func (p *RunPlan) needsConfirmation() bool {
	if !p.imageExists || !p.homeVolumeExists || p.networkJail {
		return true
	}
	if p.checkOllama && (!p.ollamaRunning || (p.ollamaModel != "" && !p.ollamaModelReady)) {
		return true
	}
	return false
}

// printRunPlan prints the plan's actions-needed summary and the project's
// currently existing resources.
func printRunPlan(p *RunPlan) {
	fmt.Println()
	printHeader("Run Plan", colorBlue)
	fmt.Println()
	fmt.Printf("  %sProject :%s %s%s%s\n", colorBold, colorReset, colorCyan, p.dir, colorReset)
	fmt.Printf("  %sAgent   :%s %s\n", colorBold, colorReset, p.agent.DisplayName)
	fmt.Println()

	line := func(label string, needsAction bool, detail string) {
		color := colorDim
		if needsAction {
			color = colorYellow
		}
		fmt.Printf("  %s%-9s:%s %s%s%s\n", colorBold, label, colorReset, color, detail, colorReset)
	}

	if p.imageExists {
		line("Image", false, fmt.Sprintf("%s (already built)", p.imageRef))
	} else {
		line("Image", true, fmt.Sprintf("build %s (%s)", p.imageRef, p.imageBuildReason))
	}

	if p.homeVolumeExists {
		line("Volume", false, fmt.Sprintf("%s (already exists)", p.homeVolumeName))
	} else {
		line("Volume", true, fmt.Sprintf("create %s", p.homeVolumeName))
	}

	if p.networkJail {
		line("Network", true, fmt.Sprintf("jailed — create network %s + proxy sidecar (allowed hosts: %d)", p.networkName, p.allowedHostCount))
	} else {
		line("Network", false, "bridge (unrestricted)")
	}

	if p.checkOllama {
		if p.ollamaRunning {
			line("Ollama", false, "sidecar already running")
		} else {
			line("Ollama", true, p.ollamaAction+" sidecar")
		}
		if p.ollamaModel != "" {
			if p.ollamaModelReady {
				line("Model", false, fmt.Sprintf("%s (already pulled)", p.ollamaModel))
			} else {
				line("Model", true, fmt.Sprintf("pull %s (one-time download)", p.ollamaModel))
			}
		}
	}

	fmt.Println()
	fmt.Printf("  %sExisting resources for this project:%s\n", colorBold, colorReset)
	if len(p.existingContainers) == 0 && len(p.existingVolumes) == 0 && len(p.existingNetworks) == 0 {
		fmt.Println("    none")
	} else {
		printResourceList("    ", p.existingContainers, p.existingVolumes)
		for _, n := range p.existingNetworks {
			fmt.Printf("    network:   %s\n", n.Name)
		}
	}
	fmt.Println()
}

// confirmRunPlan prints the plan and asks once whether to proceed, replacing
// the separate confirmations that EnsureImage, EnsureHomeVolume, and the
// Ollama helpers would otherwise each show. --yes/-y (cfg.AutoApprove)
// bypasses the prompt entirely; once the user has approved here, cfg is
// flipped to auto-approve for the rest of the invocation so none of those
// per-resource confirmations fire again for actions already listed above.
func confirmRunPlan(p *RunPlan) bool {
	printRunPlan(p)
	if !p.needsConfirmation() {
		return true
	}
	if !confirmDefault("proceed with the actions above", p.cfg.AutoApprove, true) {
		return false
	}
	p.cfg.AutoApprove = true
	return true
}
