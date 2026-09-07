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
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func entrypointScript(agent *Agent) string {
	return fmt.Sprintf(`#!/bin/bash
# BASH_ENV makes every non-interactive bash subprocess source this file automatically.
# Agent tool calls run in non-interactive shells, so PATH changes written here
# (e.g. after installing rust, java, node) persist across sessions and context resets.
export BASH_ENV="$HOME/.agent_env"
touch "$HOME/.agent_env" 2>/dev/null || true
if [[ -f /etc/smith-jail/start.sh ]]; then
    source /etc/smith-jail/start.sh
fi
exec %s "$@"
`, agent.BinaryName)
}

// WriteBuildContext writes the Dockerfile and supporting files to the build dir.
func (c *Config) WriteBuildContext(agent *Agent) error {
	epPath := filepath.Join(c.BuildDir, agent.Name+"-entrypoint.sh")
	if err := writeExecutable(epPath, entrypointScript(agent)); err != nil {
		return fmt.Errorf("writing entrypoint.sh: %w", err)
	}

	// Copy init.sh if present (shared across agents)
	if _, err := os.Stat(c.InitScript); err == nil {
		if err := copyExecutable(c.InitScript, filepath.Join(c.BuildDir, agent.Name+"-init.sh")); err != nil {
			return fmt.Errorf("copying init.sh: %w", err)
		}
	} else {
		_ = os.Remove(filepath.Join(c.BuildDir, agent.Name+"-init.sh"))
	}

	// Copy start.sh if present (shared across agents)
	if _, err := os.Stat(c.StartScript); err == nil {
		if err := copyExecutable(c.StartScript, filepath.Join(c.BuildDir, agent.Name+"-start.sh")); err != nil {
			return fmt.Errorf("copying start.sh: %w", err)
		}
	} else {
		_ = os.Remove(filepath.Join(c.BuildDir, agent.Name+"-start.sh"))
	}

	df := c.generateDockerfile(agent)
	dfPath := filepath.Join(c.BuildDir, agent.Name+"-Dockerfile")
	return os.WriteFile(dfPath, []byte(df), 0644)
}

// GeneratedDockerfile returns the Dockerfile that a build would use for
// agent under cfg's effective configuration, without building an image.
//
// It goes through WriteBuildContext rather than calling generateDockerfile
// directly: generateDockerfile's init.sh/start.sh sections depend on those
// scripts already being copied into BuildDir, so this needs to be the same
// path BuildImage takes to stay accurate.
func (c *Config) GeneratedDockerfile(agent *Agent) (string, error) {
	if err := c.WriteBuildContext(agent); err != nil {
		return "", fmt.Errorf("preparing build context: %w", err)
	}
	dfPath := filepath.Join(c.BuildDir, agent.Name+"-Dockerfile")
	df, err := os.ReadFile(dfPath)
	if err != nil {
		return "", fmt.Errorf("reading generated Dockerfile: %w", err)
	}
	return string(df), nil
}

func (c *Config) generateDockerfile(agent *Agent) string {
	var b strings.Builder

	b.WriteString("FROM " + c.BaseImage + "\n\n")

	switch agent.Name {
	case "claude":
		c.writeClaudeInstall(&b)
	case "gemini":
		c.writeGeminiInstall(&b)
	case "codex":
		c.writeCodexInstall(&b)
	case "hermes", "hermes-local":
		c.writeHermesInstall(&b)
	}

	c.writeProjectPackages(&b)

	// User creation
	b.WriteString("ARG HOST_UID=1000\n")
	b.WriteString("ARG HOST_GID=1000\n\n")
	b.WriteString("RUN groupadd -g \"${HOST_GID}\" agent && \\\n")
	b.WriteString("    useradd -u \"${HOST_UID}\" -g agent -m -s /bin/bash -d /home/agent agent\n\n")

	// Passwordless sudo
	if c.Sudo && !c.RunAsRoot {
		b.WriteString("RUN echo 'agent ALL=(ALL) NOPASSWD: ALL' > /etc/sudoers.d/agent \\\n")
		b.WriteString("    && chmod 440 /etc/sudoers.d/agent\n\n")
	}

	// init.sh if present
	initDst := agent.Name + "-init.sh"
	if _, err := os.Stat(filepath.Join(c.BuildDir, initDst)); err == nil {
		b.WriteString(fmt.Sprintf("COPY %s /tmp/init.sh\n", initDst))
		b.WriteString("RUN chmod +x /tmp/init.sh && /tmp/init.sh && rm /tmp/init.sh\n\n")
	}

	// start.sh
	startDst := agent.Name + "-start.sh"
	b.WriteString("RUN mkdir -p /etc/smith-jail\n")
	if _, err := os.Stat(filepath.Join(c.BuildDir, startDst)); err == nil {
		b.WriteString(fmt.Sprintf("COPY %s /etc/smith-jail/start.sh\n\n", startDst))
	} else {
		b.WriteString("\n")
	}

	// Workspace and agent credential dir
	credDir := c.agentCredDir(agent)
	b.WriteString(fmt.Sprintf("RUN mkdir -p /workspace /home/agent/%s \\\n", credDir))
	b.WriteString("    && chown -R agent:agent /workspace /home/agent\n\n")

	// Entrypoint
	epDst := agent.Name + "-entrypoint.sh"
	b.WriteString(fmt.Sprintf("COPY %s /usr/local/bin/entrypoint.sh\n\n", epDst))

	if c.RunAsRoot {
		b.WriteString(fmt.Sprintf("# Running as root (JAIL_RUN_AS_ROOT=true)\n"))
	} else {
		b.WriteString("USER agent\n")
	}

	b.WriteString("WORKDIR /workspace\n\n")
	b.WriteString("ENTRYPOINT [\"/usr/local/bin/entrypoint.sh\"]\n")
	b.WriteString("CMD []\n")

	c.writeLabels(&b, agent)

	return b.String()
}

// writeLabels stamps the image with its identity.
//
// These are per-agent, not per-project: one image serves every directory the
// agent is ever run in, so a project label here would record whichever project
// happened to trigger the build and be wrong for all the others. Project
// ownership belongs on the containers, volumes, and networks, which already
// carry smithjail.project.
//
// Placement at the very end is deliberate. LABEL writes a metadata-only layer,
// and build-hash changes on every configuration change — anywhere higher up and
// each such change would invalidate the cached apt and npm layers below it,
// turning a cheap rebuild into a full one.
func (c *Config) writeLabels(b *strings.Builder, agent *Agent) {
	b.WriteString("\n# Identity — smithjail.managed marks this image as ours even once it is\n")
	b.WriteString("# untagged by a later rebuild.\n")
	b.WriteString("LABEL smithjail.managed=\"true\" \\\n")
	b.WriteString(fmt.Sprintf("      smithjail.agent=%q \\\n", agent.Name))
	b.WriteString(fmt.Sprintf("      smithjail.base-image=%q \\\n", c.BaseImage))
	b.WriteString(fmt.Sprintf("      smithjail.build-hash=%q\n", c.BuildHash(agent)))
}

// writeCacheBustArg declares the build arg that lets BuildImage force a
// cache miss on the very next RUN instruction (the agent CLI fetch/install)
// without invalidating the layers above it (base image, apt-get packages).
// Those RUN commands' text never changes even when the upstream install
// script or npm package does, so Docker would otherwise cache them forever;
// referencing the arg's value inside that one RUN ties its cache key to
// whatever BuildImage passes via --build-arg. A default build (no
// --build-arg passed) always resolves to "0", so it stays cached.
func (c *Config) writeCacheBustArg(b *strings.Builder) {
	b.WriteString("ARG AGENT_CACHE_BUST=0\n")
}

// writeClaudeInstall writes the Claude Code install layer.
func (c *Config) writeClaudeInstall(b *strings.Builder) {
	basePkgs := "git curl ca-certificates ripgrep fd-find bash procps less vim-tiny"
	if c.Sudo && !c.RunAsRoot {
		basePkgs += " sudo"
	}

	b.WriteString("RUN apt-get update && apt-get install -y --no-install-recommends \\\n")
	for _, pkg := range strings.Fields(basePkgs) {
		b.WriteString("        " + pkg + " \\\n")
	}
	b.WriteString("    && rm -rf /var/lib/apt/lists/*\n\n")

	b.WriteString("ENV PATH=\"/usr/local/bin:/root/.local/bin:/usr/local/sbin:/usr/sbin:/usr/bin:/sbin:/bin\"\n\n")
	c.writeCacheBustArg(b)
	b.WriteString("RUN : \"${AGENT_CACHE_BUST}\" && mkdir -p /root/.local/bin && \\\n")
	b.WriteString("    curl -fsSL https://claude.ai/install.sh -o /tmp/claude-install.sh && \\\n")
	b.WriteString("    bash /tmp/claude-install.sh && \\\n")
	b.WriteString("    rm /tmp/claude-install.sh && \\\n")
	b.WriteString("    cp /root/.local/bin/claude /usr/local/bin/claude && \\\n")
	b.WriteString("    chmod 755 /usr/local/bin/claude\n\n")
}

// writeGeminiInstall writes the Gemini CLI install layer (requires Node.js).
func (c *Config) writeGeminiInstall(b *strings.Builder) {
	sudoPkg := ""
	if c.Sudo && !c.RunAsRoot {
		sudoPkg = " \\\n        sudo"
	}

	// Bootstrap with curl+ca-certs, then NodeSource, then full package list in one layer.
	b.WriteString("RUN apt-get update && apt-get install -y --no-install-recommends \\\n")
	b.WriteString("        curl ca-certificates gnupg \\\n")
	b.WriteString("    && curl -fsSL https://deb.nodesource.com/setup_lts.x | bash - \\\n")
	b.WriteString("    && apt-get install -y --no-install-recommends \\\n")
	b.WriteString("        nodejs \\\n")
	b.WriteString("        git ripgrep fd-find bash procps less vim-tiny")
	b.WriteString(sudoPkg)
	b.WriteString(" \\\n    && rm -rf /var/lib/apt/lists/*\n\n")

	c.writeCacheBustArg(b)
	b.WriteString("RUN : \"${AGENT_CACHE_BUST}\" && npm install -g @google/gemini-cli\n\n")
}

// writeCodexInstall writes the Codex CLI install layer (requires Node.js).
func (c *Config) writeCodexInstall(b *strings.Builder) {
	sudoPkg := ""
	if c.Sudo && !c.RunAsRoot {
		sudoPkg = " \\\n        sudo"
	}

	// Bootstrap with curl+ca-certs, then NodeSource, then full package list in one layer.
	b.WriteString("RUN apt-get update && apt-get install -y --no-install-recommends \\\n")
	b.WriteString("        curl ca-certificates gnupg \\\n")
	b.WriteString("    && curl -fsSL https://deb.nodesource.com/setup_lts.x | bash - \\\n")
	b.WriteString("    && apt-get install -y --no-install-recommends \\\n")
	b.WriteString("        nodejs \\\n")
	b.WriteString("        git ripgrep fd-find bash procps less vim-tiny")
	b.WriteString(sudoPkg)
	b.WriteString(" \\\n    && rm -rf /var/lib/apt/lists/*\n\n")

	c.writeCacheBustArg(b)
	b.WriteString("RUN : \"${AGENT_CACHE_BUST}\" && npm install -g @openai/codex\n\n")
}

// writeHermesInstall writes the Hermes Agent install layer.
// Running the install script as root triggers the FHS layout, placing the
// binary at /usr/local/bin/hermes and code at /usr/local/lib/hermes-agent/,
// so no post-install copy is needed.
func (c *Config) writeHermesInstall(b *strings.Builder) {
	basePkgs := "git curl ca-certificates ripgrep fd-find bash procps less vim-tiny xz-utils"
	if c.Sudo && !c.RunAsRoot {
		basePkgs += " sudo"
	}

	b.WriteString("RUN apt-get update && apt-get install -y --no-install-recommends \\\n")
	for _, pkg := range strings.Fields(basePkgs) {
		b.WriteString("        " + pkg + " \\\n")
	}
	b.WriteString("    && rm -rf /var/lib/apt/lists/*\n\n")

	c.writeCacheBustArg(b)
	b.WriteString("RUN : \"${AGENT_CACHE_BUST}\" && curl -fsSL https://hermes-agent.nousresearch.com/install.sh -o /tmp/hermes-install.sh && \\\n")
	b.WriteString("    bash /tmp/hermes-install.sh && \\\n")
	b.WriteString("    rm /tmp/hermes-install.sh\n\n")
}

// writeProjectPackages installs the project's extra apt packages (JAIL_INIT_PACKAGES)
// as their own layer, separate from the fixed packages writeClaudeInstall (et al.)
// already installed above. Splitting it out means everything above stays
// byte-identical across projects that share a base image and root/sudo
// settings, so Docker's local build cache reuses those layers regardless of
// which project's build triggered them — only this final layer differs.
//
// It runs immediately after the base install, while the image is still
// root, so it needs no USER juggling regardless of JAIL_RUN_AS_ROOT/JAIL_SUDO,
// and stays ahead of init.sh so a project's init script can rely on its
// packages already being present, matching the single-layer behavior this
// replaces.
func (c *Config) writeProjectPackages(b *strings.Builder) {
	if c.InitPackages == "" {
		return
	}
	b.WriteString("RUN apt-get update && apt-get install -y --no-install-recommends \\\n")
	for _, pkg := range strings.Fields(c.InitPackages) {
		b.WriteString("        " + pkg + " \\\n")
	}
	b.WriteString("    && rm -rf /var/lib/apt/lists/*\n\n")
}

// agentCredDir returns the credential directory name inside the container
// home. hermes-local shares ".hermes" with the plain "hermes" agent — Hermes
// itself always reads/writes $HOME/.hermes with no override, so that's the
// path that actually gets used regardless of which host directory smith-jail
// bind-mounts there at runtime (see buildDockerRunArgs). The two agents never
// collide because each gets its own isolated per-project home volume.
func (c *Config) agentCredDir(agent *Agent) string {
	switch agent.Name {
	case "claude":
		return ".claude"
	case "gemini":
		return ".gemini"
	case "codex":
		return ".codex"
	case "hermes", "hermes-local":
		return ".hermes"
	default:
		return "." + agent.Name
	}
}

// BuildHash returns a short hash of all inputs that affect the agent's
// image. It doubles as the image's tag (see Agent.ImageRef): two configs
// that hash the same share one image, so per-project overrides that don't
// actually change the build (e.g. two projects on the same base image and
// packages) never trigger a redundant build.
func (c *Config) BuildHash(agent *Agent) string {
	h := sha256.New()
	h.Write([]byte(agent.Name))
	h.Write([]byte(c.BaseImage))
	h.Write([]byte(c.InitPackages))
	h.Write([]byte(fmt.Sprintf("|root=%v|sudo=%v", c.RunAsRoot, c.Sudo)))

	if data, err := os.ReadFile(c.InitScript); err == nil {
		h.Write(data)
	}
	if data, err := os.ReadFile(c.StartScript); err == nil {
		h.Write(data)
	}

	return fmt.Sprintf("%x", h.Sum(nil))[:16]
}

// ProjectHash returns a short hash of a directory path for use in container names.
func projectHash(dir string) string {
	h := sha256.Sum256([]byte(dir))
	return fmt.Sprintf("%x", h[:])[:12]
}

// ── File helpers ──────────────────────────────────────────────────────────────

func writeExecutable(path, content string) error {
	return os.WriteFile(path, []byte(content), 0755)
}

func copyExecutable(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0755)
}
