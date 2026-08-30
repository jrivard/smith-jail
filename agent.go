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

// Agent describes a supported coding agent.
type Agent struct {
	Name           string   // subcommand name: "claude", "gemini"
	BinaryName     string   // executable inside the container
	SkipPermsFlag  string   // autonomous-mode flag passed to the agent
	NPMPackage     string   // npm package name for version checks
	DisplayName    string   // human-readable name for UI
	DefaultAllowed []string // default network jail hostnames
}

var (
	AgentClaude = &Agent{
		Name:          "claude",
		BinaryName:    "claude",
		SkipPermsFlag: "--dangerously-skip-permissions",
		NPMPackage:    "@anthropic-ai/claude-code",
		DisplayName:   "Claude Code",
		DefaultAllowed: []string{
			"api.anthropic.com",
			"claude.ai",
			"statsig.anthropic.com",
			"sentry.io",
		},
	}

	AgentGemini = &Agent{
		Name:          "gemini",
		BinaryName:    "gemini",
		SkipPermsFlag: "--yolo",
		NPMPackage:    "@google/gemini-cli",
		DisplayName:   "Gemini CLI",
		DefaultAllowed: []string{
			"generativelanguage.googleapis.com",
			"accounts.google.com",
			"www.googleapis.com",
			"oauth2.googleapis.com",
		},
	}

	AgentCodex = &Agent{
		Name:          "codex",
		BinaryName:    "codex",
		SkipPermsFlag: "--dangerously-bypass-approvals-and-sandbox",
		NPMPackage:    "@openai/codex",
		DisplayName:   "Codex CLI",
		DefaultAllowed: []string{
			"api.openai.com",
			"chatgpt.com",
			"auth.openai.com",
		},
	}

	AgentHermes = &Agent{
		Name:          "hermes",
		BinaryName:    "hermes",
		SkipPermsFlag: "--yolo",
		NPMPackage:    "", // Python-based; version checks are skipped gracefully
		DisplayName:   "Hermes Agent",
		DefaultAllowed: []string{
			"hermes-agent.nousresearch.com",
			"nousresearch.com",
		},
	}

	// AgentHermesLocal is the same Hermes binary as AgentHermes, pointed at
	// a local Ollama sidecar instead of a cloud model. It's a distinct
	// identity — own image tag, own credential dir (~/.hermes-local) — so
	// its config.yaml never fights with the cloud persona's over which
	// model is "default". See ollama.go for the sidecar it depends on.
	AgentHermesLocal = &Agent{
		Name:          "hermes-local",
		BinaryName:    "hermes",
		SkipPermsFlag: "--yolo",
		NPMPackage:    "",
		DisplayName:   "Hermes Agent (local Ollama)",
		DefaultAllowed: []string{
			"hermes-agent.nousresearch.com",
			"nousresearch.com",
		},
	}

	AllAgents = []*Agent{AgentClaude, AgentGemini, AgentCodex, AgentHermes, AgentHermesLocal}
)

// ImageName returns the Docker repository name for this agent. A given
// repository can hold several tags — one per distinct effective
// configuration (see ImageRef) — since base image, packages, and root/sudo
// are now project-specific.
func (a *Agent) ImageName() string {
	return "smithjail-" + a.Name
}

// ImageRef returns the full name:tag for the image built from cfg's
// effective settings. The tag is content-addressed (Config.BuildHash), so
// identical effective configs across different projects automatically share
// one image, and distinct configs never clobber each other.
func (a *Agent) ImageRef(cfg *Config) string {
	return a.ImageName() + ":" + cfg.BuildHash(a)
}

// ContainerPrefix returns the prefix used for container, volume, and network names.
func (a *Agent) ContainerPrefix() string {
	return "smithjail-" + a.Name
}
