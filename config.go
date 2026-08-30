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
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const envTemplateJail = `# =============================================================================
# smith-jail configuration
# $XDG_CONFIG_HOME/smith-jail/smith-jail.env
# =============================================================================

# ── Container resource limits ─────────────────────────────────────────────────
# JAIL_MEM_LIMIT=8g
# JAIL_CPU_LIMIT=2.0

# ── Base Image ────────────────────────────────────────────────────────────────
# The Docker image used as the base for the agent's environment.
# Must be Debian-based (uses apt-get).
# JAIL_BASE_IMAGE=debian:trixie-slim

# ── Extra Debian packages to install in the container ─────────────────────────
# Space-separated list of apt package names.
# The image rebuilds automatically when this list changes.
# JAIL_INIT_PACKAGES="python3 python3-pip gcc g++ make cargo nano jq golang default-jdk maven"

# ── Skip permission prompts (autonomous mode) ─────────────────────────────────
# When true, the agent runs without asking permission for each action.
# Safe inside the jail — the container is the real security boundary.
# JAIL_SKIP_PERMISSIONS=false

# ── Run container as root ─────────────────────────────────────────────────────
# When true, allows apt-get and other root operations inside the container.
# Filesystem isolation (only /workspace mounted) still applies.
# JAIL_RUN_AS_ROOT=false

# ── Passwordless sudo for the agent user ──────────────────────────────────────
# When true, the agent user gets passwordless sudo inside the container.
# Lets the agent run apt-get install and other root commands without running
# the entire session as root. Has no effect when JAIL_RUN_AS_ROOT=true.
# JAIL_SUDO=false

# ── Per-session startup script ────────────────────────────────────────────────
# $XDG_CONFIG_HOME/smith-jail/start.sh runs inside the container on every
# session start, before the agent launches. Use it to set environment
# variables, run git fetch, print project info, etc.
# Create the file to activate it — it is not created by default.

# ── Network jail ──────────────────────────────────────────────────────────────
# When true, the container is restricted to only reach the agent's API.
# All other outbound connections are dropped at the kernel level via nftables.
# Requires NET_ADMIN capability:
#   sudo setcap cap_net_admin+ep $(which smith-jail)
# JAIL_NETWORK_JAIL=false

# Space-separated list of additional hosts to allow through the network jail.
# JAIL_NETWORK_ALLOW="github.com registry.npmjs.org"

# Path to a file containing one hostname per line (comments with # supported).
# JAIL_NETWORK_ALLOW_FILE=~/.config/smith-jail/allowed-hosts.txt

# ── Auto-approve ──────────────────────────────────────────────────────────────
# JAIL_AUTO_APPROVE=false
`

const envTemplateClaude = `# =============================================================================
# Claude Code credentials
# $XDG_CONFIG_HOME/smith-jail/claude.env
# =============================================================================

# ── OAuth credentials ─────────────────────────────────────────────────────────
# Only needed if you moved your ~/.claude directory from its default location.
# CLAUDE_CREDS=/home/user/.claude/.credentials.json
# CLAUDE_CONFIG_DIR=/home/user/.claude
`

const envTemplateGemini = `# =============================================================================
# Gemini CLI credentials
# $XDG_CONFIG_HOME/smith-jail/gemini.env
# =============================================================================

# ── API Key authentication ────────────────────────────────────────────────────
# Set this to use API key auth instead of OAuth (Sign in with Google).
# Get your key at: https://aistudio.google.com/apikey
# GEMINI_API_KEY=your-api-key-here

# ── OAuth credentials directory ───────────────────────────────────────────────
# Only needed if you moved your ~/.gemini directory from its default location.
# For OAuth to work inside the container, run "gemini" once on the host first
# to cache tokens in ~/.gemini/tokens.json.
# GEMINI_CONFIG_DIR=/home/user/.gemini
`

const envTemplateCodex = `# =============================================================================
# Codex CLI credentials
# $XDG_CONFIG_HOME/smith-jail/codex.env
# =============================================================================

# ── API Key authentication ────────────────────────────────────────────────────
# Set this to use API key auth instead of OAuth (Sign in with ChatGPT).
# Get your key at: https://platform.openai.com/api-keys
# OPENAI_API_KEY=your-api-key-here

# ── Config directory ───────────────────────────────────────────────────────────
# Only needed if you moved your ~/.codex directory from its default location.
# CODEX_CONFIG_DIR=/home/user/.codex
`

const envTemplateHermes = `# =============================================================================
# Hermes Agent credentials
# $XDG_CONFIG_HOME/smith-jail/hermes.env
# =============================================================================

# ── Authenticating ────────────────────────────────────────────────────────────
# Run "smith-jail hermes setup <dir> --portal" to log in — it runs Hermes's
# own setup wizard inside the container and writes tokens to ~/.hermes below,
# so you never need Hermes installed on the host.

# ── OAuth credentials directory ───────────────────────────────────────────────
# Only needed if you moved your ~/.hermes directory from its default location.
# HERMES_CONFIG_DIR=/home/user/.hermes
`

const envTemplateHermesLocal = `# =============================================================================
# Hermes Agent (local Ollama) credentials
# $XDG_CONFIG_HOME/smith-jail/hermes-local.env
# =============================================================================

# ── A separate persona from "hermes" ──────────────────────────────────────────
# hermes-local keeps its own config.yaml under ~/.hermes-local, independent of
# the regular hermes agent's ~/.hermes — so switching between a cloud model
# and a local one isn't a matter of fighting over which is "default".
#
# Run "smith-jail hermes-local setup <dir> --portal" to log in the first
# time. smith-jail seeds this directory's config.yaml with a model: block
# pointing at the sidecar the first time it's missing, so no manual
# "hermes model" step is needed — see ollama.env for the model tag and the
# sidecar itself (image, GPU passthrough, etc).

# ── Config directory ───────────────────────────────────────────────────────────
# Only needed if you moved your ~/.hermes-local directory from its default location.
# HERMES_LOCAL_CONFIG_DIR=/home/user/.hermes-local
`

const envTemplateOllama = `# =============================================================================
# Ollama sidecar (used by "hermes-local")
# $XDG_CONFIG_HOME/smith-jail/ollama.env
# =============================================================================
#
# smith-jail manages one long-lived Ollama container (smithjail-ollama),
# shared by every hermes-local session — it's started/stopped around
# sessions on request, not rebuilt per project the way agent images are.
#
# Defaults below are CPU-only so "smith-jail hermes-local run" works with
# zero setup on any machine. Uncomment the GPU section once you've confirmed
# that works and want to try hardware acceleration.

# ── Image ────────────────────────────────────────────────────────────────────
# OLLAMA_IMAGE=ollama/ollama

# ── Model context length ────────────────────────────────────────────────────
# Hermes needs at least 64k tokens of context for reliable tool use — most
# servers default much lower.
# OLLAMA_CONTEXT_LENGTH=65536

# ── Host port ────────────────────────────────────────────────────────────────
# Published to 127.0.0.1 only, for "docker exec smithjail-ollama ollama pull"
# or poking the API directly. hermes-local itself reaches the sidecar over
# its own Docker network, not this port.
# OLLAMA_PORT=11434

# ── Default model ───────────────────────────────────────────────────────────
# "hermes-local run" pulls this into the sidecar automatically if it isn't
# already there (prompting first — skip with --yes/-y), and it's written into
# ~/.hermes-local/config.yaml the first time that file is created (see
# ollama.go's SeedHermesLocalConfig) so hermes-local talks to the sidecar with
# zero manual "hermes model" setup. Default is Llama 3.1 (8B) — verified by
# hand against Ollama's /v1/chat/completions endpoint to return a properly
# structured tool_calls response (not just tool-call-shaped text in
# "content", which is what several other "tool-capable" models did in
# practice). Two things NOT to use here despite looking like reasonable
# picks:
#   - Nous's own hermes3/hermes4 tags: Ollama lists "tools" as a capability,
#     but Hermes Agent refuses them outright at startup ("NOT agentic...
#     lack tool-calling capabilities required for agent workflows").
#   - qwen2.5-coder:7b: emits tool-call-shaped JSON, but Ollama (tested on
#     0.32.5) never lifts it into the tool_calls field — it arrives as plain
#     text content that Hermes can't act on.
# If you swap models, verify with a raw curl against
# http://127.0.0.1:${OLLAMA_PORT:-11434}/v1/chat/completions with a "tools"
# array before wiring it into Hermes — don't assume "tools capability" in
# "ollama show" means it actually works end to end.
# OLLAMA_MODEL=llama3.1:8b

# ── GPU passthrough (disabled by default — CPU only) ────────────────────────
# Space-separated lists, same convention as JAIL_NETWORK_ALLOW.
#
# Example for AMD ROCm, e.g. RDNA3 integrated GPUs like the Radeon 780M
# (gfx1103, not officially supported — the HSA_OVERRIDE_GFX_VERSION below
# presents it as the supported gfx1102):
#   OLLAMA_IMAGE=ollama/ollama:rocm
#   OLLAMA_DEVICES="/dev/kfd /dev/dri"
#   OLLAMA_GROUP_ADD="video render"
#   OLLAMA_EXTRA_ENV="HSA_OVERRIDE_GFX_VERSION=11.0.2 OLLAMA_IGPU_ENABLE=1"
# OLLAMA_DEVICES=
# OLLAMA_GROUP_ADD=
# OLLAMA_EXTRA_ENV=
`

// Config holds all runtime configuration.
type Config struct {
	// Paths
	UserConfigDir string
	EnvFile       string // smith-jail.env
	ClaudeEnvFile string // claude.env
	GeminiEnvFile string // gemini.env
	InitScript    string
	StartScript   string
	BuildDir      string

	// Base image
	BaseImage string // JAIL_BASE_IMAGE

	// Claude credentials
	ClaudeConfig string // ~/.claude
	ClaudeJSON   string // ~/.claude.json

	// Gemini credentials
	GeminiConfig string // ~/.gemini
	GeminiAPIKey string // GEMINI_API_KEY (empty = use OAuth)

	// Codex credentials
	CodexConfig  string // ~/.codex
	OpenAIAPIKey string // OPENAI_API_KEY (empty = use OAuth)
	CodexEnvFile string // codex.env

	// Hermes credentials
	HermesConfig  string // ~/.hermes
	HermesEnvFile string // hermes.env

	// Hermes-local credentials — deliberately separate from HermesConfig,
	// see envTemplateHermesLocal.
	HermesLocalConfig  string // ~/.hermes-local
	HermesLocalEnvFile string // hermes-local.env

	// Ollama sidecar settings (used by hermes-local; see ollama.go)
	OllamaEnvFile       string
	OllamaImage         string   // OLLAMA_IMAGE
	OllamaDevices       []string // OLLAMA_DEVICES
	OllamaGroupAdd      []string // OLLAMA_GROUP_ADD
	OllamaExtraEnv      []string // OLLAMA_EXTRA_ENV
	OllamaPort          string   // OLLAMA_PORT
	OllamaContextLength string   // OLLAMA_CONTEXT_LENGTH
	OllamaModel         string   // OLLAMA_MODEL

	// Container settings
	MemLimit        string
	CPULimit        string
	InitPackages    string
	SkipPermissions bool
	RunAsRoot       bool
	Sudo            bool

	// Network jail
	NetworkJailEnabled bool
	NetworkAllowHosts  []string
	NetworkAllowFile   string

	// Approval
	AutoApprove bool

	// Host identity
	UID int
	GID int

	// Project layering — resolved once LoadConfig is given a directory.
	// Precedence (lowest to highest): built-in defaults < smith-jail.env
	// (global) < ProjectStoreFile (private, TUI-managed, keyed by path) <
	// process environment.
	//
	// There is deliberately no project-directory config file committed to
	// the repo. Settings here can enable root, passwordless sudo, or
	// disable the network jail — letting a
	// project directory itself supply any of that would mean cloning a
	// hostile repo and running smith-jail in it silently escalates
	// privileges or turns off the sandbox's own protections. Project-scope
	// settings are user-controlled and machine-local only.
	ProjectDir        string          // directory config was resolved for
	ProjectStoreFile  string          // UserConfigDir/projects/<hash>.env
	ProjectOverridden map[string]bool // JAIL_* keys set by the project store
}

// ProjectScopeFile returns the file a project-scope edit should be written
// to: the private per-project store. It is always machine-local — never a
// file inside the project directory itself (see the comment on
// ProjectOverridden above).
func (c *Config) ProjectScopeFile() string {
	return c.ProjectStoreFile
}

// LoadConfig resolves all paths and loads all env files, then layers
// project-specific overrides for dir on top (see ProjectStoreFile on
// Config).
func LoadConfig(dir string) (*Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("cannot determine home directory: %w", err)
	}

	xdgConfig := os.Getenv("XDG_CONFIG_HOME")
	if xdgConfig == "" {
		xdgConfig = filepath.Join(home, ".config")
	}

	xdgCache := os.Getenv("XDG_CACHE_HOME")
	if xdgCache == "" {
		xdgCache = filepath.Join(home, ".cache")
	}

	cfg := &Config{
		UserConfigDir:     filepath.Join(xdgConfig, "smith-jail"),
		BuildDir:          filepath.Join(xdgCache, "smith-jail"),
		ClaudeConfig:      filepath.Join(home, ".claude"),
		ClaudeJSON:        filepath.Join(home, ".claude.json"),
		GeminiConfig:      filepath.Join(home, ".gemini"),
		CodexConfig:       filepath.Join(home, ".codex"),
		HermesConfig:      filepath.Join(home, ".hermes"),
		HermesLocalConfig: filepath.Join(home, ".hermes-local"),
		// Defaults
		BaseImage:           "debian:trixie-slim",
		MemLimit:            "8g",
		CPULimit:            "2.0",
		InitPackages:        "curl gcc build-essential make",
		UID:                 os.Getuid(),
		GID:                 os.Getgid(),
		OllamaImage:         "ollama/ollama",
		OllamaPort:          "11434",
		OllamaContextLength: "65536",
		OllamaModel:         "llama3.1:8b",
	}

	cfg.EnvFile = filepath.Join(cfg.UserConfigDir, "smith-jail.env")
	cfg.ClaudeEnvFile = filepath.Join(cfg.UserConfigDir, "claude.env")
	cfg.GeminiEnvFile = filepath.Join(cfg.UserConfigDir, "gemini.env")
	cfg.CodexEnvFile = filepath.Join(cfg.UserConfigDir, "codex.env")
	cfg.HermesEnvFile = filepath.Join(cfg.UserConfigDir, "hermes.env")
	cfg.HermesLocalEnvFile = filepath.Join(cfg.UserConfigDir, "hermes-local.env")
	cfg.OllamaEnvFile = filepath.Join(cfg.UserConfigDir, "ollama.env")
	cfg.InitScript = filepath.Join(cfg.UserConfigDir, "init.sh")
	cfg.StartScript = filepath.Join(cfg.UserConfigDir, "start.sh")

	if err := cfg.loadJailEnvFile(); err != nil {
		return nil, err
	}
	if err := cfg.loadClaudeEnvFile(); err != nil {
		return nil, err
	}
	if err := cfg.loadGeminiEnvFile(); err != nil {
		return nil, err
	}
	if err := cfg.loadCodexEnvFile(); err != nil {
		return nil, err
	}
	if err := cfg.loadHermesEnvFile(); err != nil {
		return nil, err
	}
	if err := cfg.loadHermesLocalEnvFile(); err != nil {
		return nil, err
	}
	if err := cfg.loadOllamaEnvFile(); err != nil {
		return nil, err
	}
	if err := cfg.loadProjectOverrides(dir); err != nil {
		return nil, err
	}

	cfg.applyEnvOverrides()

	if err := os.MkdirAll(cfg.BuildDir, 0755); err != nil {
		return nil, fmt.Errorf("cannot create build dir: %w", err)
	}

	// Ensure .claude.json exists so the Claude bind mount doesn't fail.
	f, err := os.OpenFile(cfg.ClaudeJSON, os.O_CREATE|os.O_WRONLY, 0600)
	if err == nil {
		f.Close()
	}

	return cfg, nil
}

// parseEnvFile reads a key=value env file and returns the parsed map.
// Lines starting with # and blank lines are ignored. Returns nil if the file
// does not exist.
func parseEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cannot open %s: %w", path, err)
	}
	defer f.Close()

	env := make(map[string]string)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		env[strings.TrimSpace(parts[0])] = stripQuotes(strings.TrimSpace(parts[1]))
	}
	return env, scanner.Err()
}

func stripQuotes(s string) string {
	if len(s) >= 2 && ((s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'')) {
		return s[1 : len(s)-1]
	}
	return s
}

func (c *Config) loadJailEnvFile() error {
	env, err := parseEnvFile(c.EnvFile)
	if err != nil || env == nil {
		return err
	}
	c.applyJailEnv(env, nil)
	return nil
}

// applyJailEnv sets the JAIL_*-derived fields found in env. seen, if
// non-nil, is marked for every key actually present in env — used to track
// which keys a project-level layer overrides.
func (c *Config) applyJailEnv(env map[string]string, seen map[string]bool) {
	mark := func(key string) {
		if seen != nil {
			seen[key] = true
		}
	}
	if v, ok := env["JAIL_MEM_LIMIT"]; ok {
		c.MemLimit = v
		mark("JAIL_MEM_LIMIT")
	}
	if v, ok := env["JAIL_CPU_LIMIT"]; ok {
		c.CPULimit = v
		mark("JAIL_CPU_LIMIT")
	}
	if v, ok := env["JAIL_BASE_IMAGE"]; ok {
		c.BaseImage = v
		mark("JAIL_BASE_IMAGE")
	}
	if v, ok := env["JAIL_INIT_PACKAGES"]; ok {
		c.InitPackages = v
		mark("JAIL_INIT_PACKAGES")
	}
	if v, ok := env["JAIL_SKIP_PERMISSIONS"]; ok {
		c.SkipPermissions, _ = strconv.ParseBool(v)
		mark("JAIL_SKIP_PERMISSIONS")
	}
	if v, ok := env["JAIL_RUN_AS_ROOT"]; ok {
		c.RunAsRoot, _ = strconv.ParseBool(v)
		mark("JAIL_RUN_AS_ROOT")
	}
	if v, ok := env["JAIL_SUDO"]; ok {
		c.Sudo, _ = strconv.ParseBool(v)
		mark("JAIL_SUDO")
	}
	if v, ok := env["JAIL_NETWORK_JAIL"]; ok {
		c.NetworkJailEnabled, _ = strconv.ParseBool(v)
		mark("JAIL_NETWORK_JAIL")
	}
	if v, ok := env["JAIL_NETWORK_ALLOW"]; ok {
		c.NetworkAllowHosts = nil
		for _, h := range strings.Fields(v) {
			c.NetworkAllowHosts = append(c.NetworkAllowHosts, h)
		}
		mark("JAIL_NETWORK_ALLOW")
	}
	if v, ok := env["JAIL_NETWORK_ALLOW_FILE"]; ok {
		c.NetworkAllowFile = v
		mark("JAIL_NETWORK_ALLOW_FILE")
	}
	if v, ok := env["JAIL_AUTO_APPROVE"]; ok {
		c.AutoApprove, _ = strconv.ParseBool(v)
		mark("JAIL_AUTO_APPROVE")
	}
}

// loadProjectOverrides layers the private per-project store on top of
// whatever loadJailEnvFile already applied. See the comment on
// ProjectOverridden (Config) for why there is no project-directory file in
// this chain.
func (c *Config) loadProjectOverrides(dir string) error {
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	c.ProjectDir = dir
	c.ProjectStoreFile = filepath.Join(c.UserConfigDir, "projects", projectHash(dir)+".env")
	c.ProjectOverridden = map[string]bool{}

	store, err := parseEnvFile(c.ProjectStoreFile)
	if err != nil {
		return err
	}
	if store != nil {
		c.applyJailEnv(store, c.ProjectOverridden)
	}

	return nil
}

// RawJailValues returns the JAIL_* keys exactly as written in path, with no
// layering or defaults applied. Used by the Settings/Packages screens to
// edit one scope (global or project) at a time.
func RawJailValues(path string) map[string]string {
	env, err := parseEnvFile(path)
	if err != nil || env == nil {
		return map[string]string{}
	}
	return env
}

func (c *Config) loadClaudeEnvFile() error {
	env, err := parseEnvFile(c.ClaudeEnvFile)
	if err != nil || env == nil {
		return err
	}
	if v, ok := env["CLAUDE_CONFIG_DIR"]; ok {
		c.ClaudeConfig = v
		c.ClaudeJSON = filepath.Join(filepath.Dir(v), ".claude.json")
	}
	// CLAUDE_CREDS is informational; CLAUDE_CONFIG_DIR is the authoritative override.
	return nil
}

func (c *Config) loadGeminiEnvFile() error {
	env, err := parseEnvFile(c.GeminiEnvFile)
	if err != nil || env == nil {
		return err
	}
	if v, ok := env["GEMINI_API_KEY"]; ok {
		c.GeminiAPIKey = v
	}
	if v, ok := env["GEMINI_CONFIG_DIR"]; ok {
		c.GeminiConfig = v
	}
	return nil
}

func (c *Config) loadCodexEnvFile() error {
	env, err := parseEnvFile(c.CodexEnvFile)
	if err != nil || env == nil {
		return err
	}
	if v, ok := env["OPENAI_API_KEY"]; ok {
		c.OpenAIAPIKey = v
	}
	if v, ok := env["CODEX_CONFIG_DIR"]; ok {
		c.CodexConfig = v
	}
	return nil
}

func (c *Config) loadHermesEnvFile() error {
	env, err := parseEnvFile(c.HermesEnvFile)
	if err != nil || env == nil {
		return err
	}
	if v, ok := env["HERMES_CONFIG_DIR"]; ok {
		c.HermesConfig = v
	}
	return nil
}

func (c *Config) loadHermesLocalEnvFile() error {
	env, err := parseEnvFile(c.HermesLocalEnvFile)
	if err != nil || env == nil {
		return err
	}
	if v, ok := env["HERMES_LOCAL_CONFIG_DIR"]; ok {
		c.HermesLocalConfig = v
	}
	return nil
}

func (c *Config) loadOllamaEnvFile() error {
	env, err := parseEnvFile(c.OllamaEnvFile)
	if err != nil || env == nil {
		return err
	}
	c.applyOllamaEnv(env)
	return nil
}

// applyOllamaEnv sets the OLLAMA_*-derived fields found in env. Shared by
// the ollama.env loader and applyEnvOverrides so process environment
// variables and the config file use identical parsing.
func (c *Config) applyOllamaEnv(env map[string]string) {
	if v, ok := env["OLLAMA_IMAGE"]; ok {
		c.OllamaImage = v
	}
	if v, ok := env["OLLAMA_DEVICES"]; ok {
		c.OllamaDevices = strings.Fields(v)
	}
	if v, ok := env["OLLAMA_GROUP_ADD"]; ok {
		c.OllamaGroupAdd = strings.Fields(v)
	}
	if v, ok := env["OLLAMA_EXTRA_ENV"]; ok {
		c.OllamaExtraEnv = strings.Fields(v)
	}
	if v, ok := env["OLLAMA_PORT"]; ok {
		c.OllamaPort = v
	}
	if v, ok := env["OLLAMA_CONTEXT_LENGTH"]; ok {
		c.OllamaContextLength = v
	}
	if v, ok := env["OLLAMA_MODEL"]; ok {
		c.OllamaModel = v
	}
}

// applyEnvOverrides applies process environment variables on top of config files.
func (c *Config) applyEnvOverrides() {
	if v := os.Getenv("CLAUDE_CONFIG_DIR"); v != "" {
		c.ClaudeConfig = v
		c.ClaudeJSON = filepath.Join(filepath.Dir(v), ".claude.json")
	}
	if v := os.Getenv("GEMINI_API_KEY"); v != "" {
		c.GeminiAPIKey = v
	}
	if v := os.Getenv("GEMINI_CONFIG_DIR"); v != "" {
		c.GeminiConfig = v
	}
	if v := os.Getenv("OPENAI_API_KEY"); v != "" {
		c.OpenAIAPIKey = v
	}
	if v := os.Getenv("CODEX_CONFIG_DIR"); v != "" {
		c.CodexConfig = v
	}
	if v := os.Getenv("HERMES_CONFIG_DIR"); v != "" {
		c.HermesConfig = v
	}
	if v := os.Getenv("HERMES_LOCAL_CONFIG_DIR"); v != "" {
		c.HermesLocalConfig = v
	}
	ollamaEnv := map[string]string{}
	for _, key := range []string{
		"OLLAMA_IMAGE", "OLLAMA_DEVICES", "OLLAMA_GROUP_ADD",
		"OLLAMA_EXTRA_ENV", "OLLAMA_PORT", "OLLAMA_CONTEXT_LENGTH", "OLLAMA_MODEL",
	} {
		if v := os.Getenv(key); v != "" {
			ollamaEnv[key] = v
		}
	}
	c.applyOllamaEnv(ollamaEnv)
	if v := os.Getenv("JAIL_MEM_LIMIT"); v != "" {
		c.MemLimit = v
	}
	if v := os.Getenv("JAIL_CPU_LIMIT"); v != "" {
		c.CPULimit = v
	}
	if v := os.Getenv("JAIL_BASE_IMAGE"); v != "" {
		c.BaseImage = v
	}
	if v := os.Getenv("JAIL_INIT_PACKAGES"); v != "" {
		c.InitPackages = v
	}
	if v := os.Getenv("JAIL_SKIP_PERMISSIONS"); v != "" {
		c.SkipPermissions, _ = strconv.ParseBool(v)
	}
	if v := os.Getenv("JAIL_RUN_AS_ROOT"); v != "" {
		c.RunAsRoot, _ = strconv.ParseBool(v)
	}
	if v := os.Getenv("JAIL_SUDO"); v != "" {
		c.Sudo, _ = strconv.ParseBool(v)
	}
	if v := os.Getenv("JAIL_NETWORK_JAIL"); v != "" {
		c.NetworkJailEnabled, _ = strconv.ParseBool(v)
	}
	if v := os.Getenv("JAIL_NETWORK_ALLOW"); v != "" {
		c.NetworkAllowHosts = nil
		for _, h := range strings.Fields(v) {
			c.NetworkAllowHosts = append(c.NetworkAllowHosts, h)
		}
	}
	if v := os.Getenv("JAIL_NETWORK_ALLOW_FILE"); v != "" {
		c.NetworkAllowFile = v
	}
	if v := os.Getenv("JAIL_AUTO_APPROVE"); v != "" {
		c.AutoApprove, _ = strconv.ParseBool(v)
	}
}

// envTemplateFor returns the default template content for one of Config's
// env file paths, or "" if path doesn't match a known one.
func (c *Config) envTemplateFor(path string) string {
	switch path {
	case c.EnvFile:
		return envTemplateJail
	case c.ClaudeEnvFile:
		return envTemplateClaude
	case c.GeminiEnvFile:
		return envTemplateGemini
	case c.CodexEnvFile:
		return envTemplateCodex
	case c.HermesEnvFile:
		return envTemplateHermes
	case c.HermesLocalEnvFile:
		return envTemplateHermesLocal
	case c.OllamaEnvFile:
		return envTemplateOllama
	default:
		return ""
	}
}

// WriteEnvTemplates writes all default config files for files that don't exist yet.
func (c *Config) WriteEnvTemplates() error {
	if err := os.MkdirAll(c.UserConfigDir, 0700); err != nil {
		return err
	}
	for _, path := range []string{
		c.EnvFile,
		c.ClaudeEnvFile,
		c.GeminiEnvFile,
		c.CodexEnvFile,
		c.HermesEnvFile,
		c.HermesLocalEnvFile,
		c.OllamaEnvFile,
	} {
		if err := writeFileIfAbsent(path, c.envTemplateFor(path)); err != nil {
			return err
		}
	}
	return nil
}

func writeFileIfAbsent(path, content string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if os.IsExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(content)
	return err
}

// envFileSpec pairs a config file's path with its display label.
type envFileSpec struct{ path, label string }

// agentEnvFile returns the path and label of agent's own credentials file,
// e.g. AgentCodex -> codex.env. Every agent in AllAgents has one.
func (c *Config) agentEnvFile(agent *Agent) envFileSpec {
	switch agent.Name {
	case "claude":
		return envFileSpec{c.ClaudeEnvFile, "claude.env         — Claude credentials"}
	case "gemini":
		return envFileSpec{c.GeminiEnvFile, "gemini.env         — Gemini credentials"}
	case "codex":
		return envFileSpec{c.CodexEnvFile, "codex.env          — Codex credentials"}
	case "hermes":
		return envFileSpec{c.HermesEnvFile, "hermes.env         — Hermes credentials"}
	case "hermes-local":
		return envFileSpec{c.HermesLocalEnvFile, "hermes-local.env   — Hermes (local Ollama) credentials"}
	default:
		return envFileSpec{}
	}
}

// PromptCreateEnvFiles asks the user to create any missing config files on
// first run, scoped to what this invocation actually needs: the shared jail
// settings file, plus agent's own credentials file (and, for hermes-local,
// the Ollama sidecar settings it depends on). agent may be nil for commands
// that don't target a specific agent (e.g. "smith-jail ollama"), in which
// case only the jail settings file is offered.
func (c *Config) PromptCreateEnvFiles(agent *Agent) {
	missing := []envFileSpec{
		{c.EnvFile, "smith-jail.env  — jail settings"},
	}
	if agent != nil {
		missing = append(missing, c.agentEnvFile(agent))
		if agent.Name == "hermes-local" {
			missing = append(missing, envFileSpec{c.OllamaEnvFile, "ollama.env         — Ollama sidecar settings"})
		}
	}

	var toCreate []envFileSpec
	for _, m := range missing {
		if !fileExists(m.path) {
			toCreate = append(toCreate, m)
		}
	}
	if len(toCreate) == 0 {
		return
	}
	missing = toCreate

	var proceed bool
	if c.AutoApprove {
		printInfo(fmt.Sprintf("create config files in %s [auto-approved]", c.UserConfigDir))
		proceed = true
	} else {
		fmt.Println()
		printHeader("Smith Jail — First Run", colorBlue)
		fmt.Println()
		fmt.Println("  No config files found. May I create defaults in:")
		fmt.Printf("  %s%s%s\n", colorCyan, c.UserConfigDir, colorReset)
		for _, m := range missing {
			fmt.Printf("    %s%s%s\n", colorDim, m.label, colorReset)
		}
		fmt.Println()
		fmt.Print("  Create them? [Y/n] ")

		reader := bufio.NewReader(os.Stdin)
		answer, _ := reader.ReadString('\n')
		answer = strings.TrimSpace(strings.ToLower(answer))
		proceed = answer == "" || answer == "y" || answer == "yes"
	}

	if proceed {
		if err := os.MkdirAll(c.UserConfigDir, 0700); err != nil {
			printWarn(fmt.Sprintf("Could not create config files: %v", err))
		} else {
			for _, m := range missing {
				if err := writeFileIfAbsent(m.path, c.envTemplateFor(m.path)); err != nil {
					printWarn(fmt.Sprintf("Could not create %s: %v", m.path, err))
					continue
				}
				printOK("Created " + m.path)
			}
			if !c.AutoApprove {
				fmt.Printf("  %sEdit them any time to change settings.%s\n", colorDim, colorReset)
			}
		}
	} else {
		printInfo("Skipping. Using built-in defaults.")
	}
	if !c.AutoApprove {
		fmt.Println()
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
