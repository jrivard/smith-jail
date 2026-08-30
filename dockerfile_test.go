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
	"strings"
	"testing"
)

func testConfig(t *testing.T) *Config {
	t.Helper()
	dir := t.TempDir()
	return &Config{
		BuildDir:     dir,
		BaseImage:    "debian:trixie-slim",
		InitPackages: "curl gcc",
		InitScript:   dir + "/init.sh",
		StartScript:  dir + "/start.sh",
	}
}

func TestGeneratedDockerfileCarriesIdentityLabels(t *testing.T) {
	cfg := testConfig(t)
	df := cfg.generateDockerfile(AgentClaude)

	for _, want := range []string{
		`smithjail.managed="true"`,
		`smithjail.agent="claude"`,
		`smithjail.base-image="debian:trixie-slim"`,
		`smithjail.build-hash="` + cfg.BuildHash(AgentClaude) + `"`,
	} {
		if !strings.Contains(df, want) {
			t.Errorf("Dockerfile missing %s:\n%s", want, df)
		}
	}
}

// The label block must stay below every RUN/COPY layer. build-hash changes on
// every configuration change, so a LABEL placed higher would invalidate the
// cached apt and npm layers and turn each cheap rebuild into a full one.
func TestLabelsComeAfterAllBuildLayers(t *testing.T) {
	cfg := testConfig(t)
	df := cfg.generateDockerfile(AgentClaude)

	label := strings.Index(df, "LABEL smithjail.managed")
	if label < 0 {
		t.Fatal("no LABEL instruction found")
	}

	for _, instr := range []string{"RUN ", "COPY ", "FROM ", "USER ", "ENTRYPOINT "} {
		if last := strings.LastIndex(df, instr); last > label {
			t.Errorf("%q appears after the LABEL block; labels must come last", instr)
		}
	}
}

// Labels are metadata about the build, not an input to it. If they fed the hash
// the tool would demand a rebuild of every existing image on upgrade.
func TestLabelsDoNotAffectBuildHash(t *testing.T) {
	cfg := testConfig(t)

	before := cfg.BuildHash(AgentClaude)
	df := cfg.generateDockerfile(AgentClaude)
	after := cfg.BuildHash(AgentClaude)

	if before != after {
		t.Errorf("BuildHash changed across Dockerfile generation: %s -> %s", before, after)
	}
	if !strings.Contains(df, before) {
		t.Error("the stamped build-hash should be the one BuildHash reports")
	}
}

// The base install layer (agent binary + fixed OS packages) must be
// byte-identical regardless of a project's extra packages, so Docker's
// local build cache reuses it across every project that shares a base
// image — only the final, project-specific packages layer should vary.
func TestBaseInstallLayerIsIndependentOfProjectPackages(t *testing.T) {
	install := map[string]func(*Config, *strings.Builder){
		"claude":       (*Config).writeClaudeInstall,
		"gemini":       (*Config).writeGeminiInstall,
		"codex":        (*Config).writeCodexInstall,
		"hermes":       (*Config).writeHermesInstall,
		"hermes-local": (*Config).writeHermesInstall,
	}
	for name, fn := range install {
		withPkgs := testConfig(t)
		withPkgs.InitPackages = "cargo nodejs"

		withoutPkgs := testConfig(t)
		withoutPkgs.InitPackages = ""

		var bWith, bWithout strings.Builder
		fn(withPkgs, &bWith)
		fn(withoutPkgs, &bWithout)

		if bWith.String() != bWithout.String() {
			t.Errorf("%s: base install layer changed when InitPackages was set — cache would not be shared:\nwith:\n%s\nwithout:\n%s",
				name, bWith.String(), bWithout.String())
		}
	}
}

// An empty JAIL_INIT_PACKAGES must not emit an empty/no-op apt-get layer.
func TestNoProjectPackagesLayerWhenEmpty(t *testing.T) {
	cfg := testConfig(t)
	cfg.InitPackages = ""

	var b strings.Builder
	cfg.writeProjectPackages(&b)
	if b.Len() != 0 {
		t.Errorf("expected no output when InitPackages is empty, got:\n%s", b.String())
	}
}

// The project-packages layer must actually list the project's packages when
// present, so the split doesn't silently drop them.
func TestProjectPackagesLayerListsPackages(t *testing.T) {
	cfg := testConfig(t)
	cfg.InitPackages = "cargo nodejs"

	var b strings.Builder
	cfg.writeProjectPackages(&b)
	out := b.String()
	for _, pkg := range []string{"cargo", "nodejs"} {
		if !strings.Contains(out, pkg) {
			t.Errorf("project packages layer missing %q:\n%s", pkg, out)
		}
	}
}

// hermes-local shares the hermes agent's install layer, binary, and
// in-container credential path (Hermes always reads/writes $HOME/.hermes,
// with no override) — but must never share its actual config, since that's
// the whole point of giving it a separate config.yaml. Separation instead
// comes from distinct host-side directories bind-mounted over that same
// in-container path (see TestHermesLocalConfigDirIsIndependentOfHermes in
// config_test.go) plus distinct per-project home volumes, so the two never
// collide in practice.
func TestHermesLocalSharesCredentialDirPath(t *testing.T) {
	cfg := testConfig(t)

	if got := cfg.agentCredDir(AgentHermes); got != ".hermes" {
		t.Errorf("hermes credDir = %q, want .hermes", got)
	}
	if got := cfg.agentCredDir(AgentHermesLocal); got != ".hermes" {
		t.Errorf("hermes-local credDir = %q, want .hermes", got)
	}
}

func TestEveryAgentGetsItsOwnAgentLabel(t *testing.T) {
	cfg := testConfig(t)

	for _, agent := range AllAgents {
		df := cfg.generateDockerfile(agent)
		want := `smithjail.agent="` + agent.Name + `"`
		if !strings.Contains(df, want) {
			t.Errorf("%s: Dockerfile missing %s", agent.Name, want)
		}
		// An image serves every project, so it must never claim to own one.
		if strings.Contains(df, "smithjail.project") {
			t.Errorf("%s: image labelled with a project it does not exclusively belong to", agent.Name)
		}
	}
}
