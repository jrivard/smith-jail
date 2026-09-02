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

// setupIsolatedHome points HOME/XDG_*_HOME at a scratch directory so
// LoadConfig never reads or writes the real user's config.
func setupIsolatedHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
}

// TestLoadConfigProjectLayering walks the full precedence chain — built-in
// default < global smith-jail.env < private per-project store < process
// environment — checking that each layer wins over everything below it as
// it's introduced.
func TestLoadConfigProjectLayering(t *testing.T) {
	setupIsolatedHome(t)
	projectDir := t.TempDir()

	cfg, err := LoadConfig(projectDir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BaseImage != "debian:trixie-slim" {
		t.Fatalf("built-in default: BaseImage = %q", cfg.BaseImage)
	}

	if err := SetEnvValues(cfg.EnvFile, map[string]string{"JAIL_BASE_IMAGE": "golang:1.22"}); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadConfig(projectDir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BaseImage != "golang:1.22" {
		t.Fatalf("global file: BaseImage = %q, want golang:1.22", cfg.BaseImage)
	}

	if err := SetEnvValues(cfg.ProjectStoreFile, map[string]string{"JAIL_BASE_IMAGE": "rust:1.75"}); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadConfig(projectDir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BaseImage != "rust:1.75" {
		t.Fatalf("private project store: BaseImage = %q, want rust:1.75", cfg.BaseImage)
	}
	if !cfg.ProjectOverridden["JAIL_BASE_IMAGE"] {
		t.Error("ProjectOverridden should record JAIL_BASE_IMAGE once the project store sets it")
	}
	if cfg.ProjectScopeFile() != cfg.ProjectStoreFile {
		t.Errorf("ProjectScopeFile() = %q, want the private store %q",
			cfg.ProjectScopeFile(), cfg.ProjectStoreFile)
	}

	t.Setenv("JAIL_BASE_IMAGE", "python:3.12")
	cfg, err = LoadConfig(projectDir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BaseImage != "python:3.12" {
		t.Fatalf("process environment: BaseImage = %q, want python:3.12", cfg.BaseImage)
	}
}

// hermes-local must default to a config directory distinct from the
// regular hermes agent's, and setting one must never move the other — the
// two personas are meant to keep independent config.yaml files.
func TestHermesLocalConfigDirIsIndependentOfHermes(t *testing.T) {
	setupIsolatedHome(t)
	projectDir := t.TempDir()

	cfg, err := LoadConfig(projectDir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HermesConfig == cfg.HermesLocalConfig {
		t.Fatalf("HermesConfig and HermesLocalConfig both resolved to %q", cfg.HermesConfig)
	}

	t.Setenv("HERMES_CONFIG_DIR", "/tmp/moved-hermes")
	cfg, err = LoadConfig(projectDir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HermesLocalConfig == "/tmp/moved-hermes" {
		t.Error("HERMES_CONFIG_DIR leaked into HermesLocalConfig")
	}
}

// The Ollama sidecar must default to CPU-only — no image override, no
// device passthrough — so a fresh "hermes-local run" works with zero setup
// on any machine, GPU or not.
func TestOllamaDefaultsAreCPUOnly(t *testing.T) {
	setupIsolatedHome(t)
	projectDir := t.TempDir()

	cfg, err := LoadConfig(projectDir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OllamaImage != "ollama/ollama" {
		t.Errorf("OllamaImage = %q, want the plain (non-GPU) ollama/ollama image", cfg.OllamaImage)
	}
	if len(cfg.OllamaDevices) != 0 {
		t.Errorf("OllamaDevices = %v, want none by default", cfg.OllamaDevices)
	}
	if len(cfg.OllamaExtraEnv) != 0 {
		t.Errorf("OllamaExtraEnv = %v, want none by default", cfg.OllamaExtraEnv)
	}
}

// ollama.env must be able to opt into GPU passthrough — the default is
// CPU-only, not a hard limitation.
func TestOllamaEnvFileEnablesGPUPassthrough(t *testing.T) {
	setupIsolatedHome(t)
	projectDir := t.TempDir()

	cfg, err := LoadConfig(projectDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetEnvValues(cfg.OllamaEnvFile, map[string]string{
		"OLLAMA_IMAGE":     "ollama/ollama:rocm",
		"OLLAMA_DEVICES":   "/dev/kfd /dev/dri",
		"OLLAMA_EXTRA_ENV": "HSA_OVERRIDE_GFX_VERSION=11.0.2",
	}); err != nil {
		t.Fatal(err)
	}

	cfg, err = LoadConfig(projectDir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OllamaImage != "ollama/ollama:rocm" {
		t.Errorf("OllamaImage = %q, want ollama/ollama:rocm", cfg.OllamaImage)
	}
	if len(cfg.OllamaDevices) != 2 || cfg.OllamaDevices[0] != "/dev/kfd" || cfg.OllamaDevices[1] != "/dev/dri" {
		t.Errorf("OllamaDevices = %v, want [/dev/kfd /dev/dri]", cfg.OllamaDevices)
	}
	if len(cfg.OllamaExtraEnv) != 1 || cfg.OllamaExtraEnv[0] != "HSA_OVERRIDE_GFX_VERSION=11.0.2" {
		t.Errorf("OllamaExtraEnv = %v, want [HSA_OVERRIDE_GFX_VERSION=11.0.2]", cfg.OllamaExtraEnv)
	}
}

// A project's override must not leak into an unrelated project's config.
func TestLoadConfigProjectOverridesAreIsolatedPerDirectory(t *testing.T) {
	setupIsolatedHome(t)
	projectA := t.TempDir()
	projectB := t.TempDir()

	cfgA, err := LoadConfig(projectA)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetEnvValues(cfgA.ProjectStoreFile, map[string]string{"JAIL_INIT_PACKAGES": "cargo"}); err != nil {
		t.Fatal(err)
	}

	cfgA, err = LoadConfig(projectA)
	if err != nil {
		t.Fatal(err)
	}
	if cfgA.InitPackages != "cargo" {
		t.Fatalf("project A: InitPackages = %q, want cargo", cfgA.InitPackages)
	}

	cfgB, err := LoadConfig(projectB)
	if err != nil {
		t.Fatal(err)
	}
	if cfgB.InitPackages == "cargo" {
		t.Fatalf("project B: unexpectedly inherited project A's override: %q", cfgB.InitPackages)
	}
}

// TestMaliciousProjectDirCannotEscalatePrivileges is the adversarial
// counterpart to TestLoadConfigProjectLayering: it plants files *inside the
// project directory itself* — the one thing an untrusted cloned repo
// controls — at every plausible path an attacker might guess, including the
// real global filename and the real per-project store's relative layout
// (projects/<hash>.env), and with the specific settings that would matter if
// they took effect: running as root, passwordless sudo, and turning off the
// network jail. See the comment on Config.ProjectOverridden for why the real
// lookup chain never reads anything under the project directory. If any of
// this ever got picked up, cloning a hostile repo and running
// "smith-jail run" in it would silently self-escalate.
func TestMaliciousProjectDirCannotEscalatePrivileges(t *testing.T) {
	setupIsolatedHome(t)
	projectDir := t.TempDir()

	baseline, err := LoadConfig(projectDir)
	if err != nil {
		t.Fatal(err)
	}

	dangerous := map[string]string{
		"JAIL_RUN_AS_ROOT":  "true",
		"JAIL_SUDO":         "true",
		"JAIL_NETWORK_JAIL": "false",
		"JAIL_BASE_IMAGE":   "attacker/backdoored-image",
		"JAIL_AUTO_APPROVE": "true",
	}

	// Every path a malicious repo could plausibly plant, all relative to the
	// project directory itself.
	hash := projectHash(projectDir)
	candidatePaths := []string{
		filepath.Join(projectDir, "smith-jail.env"), // guesses the global filename verbatim
		filepath.Join(projectDir, ".config", "smith-jail", "smith-jail.env"),
		filepath.Join(projectDir, "projects", hash+".env"), // guesses the private store's relative layout
		filepath.Join(projectDir, ".config", "smith-jail", "projects", hash+".env"),
		filepath.Join(projectDir, "claude.env"),
	}
	for _, p := range candidatePaths {
		if err := SetEnvValues(p, dangerous); err != nil {
			t.Fatalf("planting %s: %v", p, err)
		}
	}

	cfg, err := LoadConfig(projectDir)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.RunAsRoot {
		t.Error("a file inside the project directory was able to set JAIL_RUN_AS_ROOT")
	}
	if cfg.Sudo {
		t.Error("a file inside the project directory was able to set JAIL_SUDO")
	}
	if cfg.NetworkJailEnabled != baseline.NetworkJailEnabled {
		t.Error("a file inside the project directory was able to change JAIL_NETWORK_JAIL")
	}
	if cfg.BaseImage != baseline.BaseImage {
		t.Errorf("BaseImage = %q, want unchanged default %q", cfg.BaseImage, baseline.BaseImage)
	}
	if cfg.AutoApprove {
		t.Error("a file inside the project directory was able to set JAIL_AUTO_APPROVE")
	}
	if len(cfg.ProjectOverridden) != 0 {
		t.Errorf("ProjectOverridden = %v, want empty — nothing under the project directory should ever be recorded as an override", cfg.ProjectOverridden)
	}
}
