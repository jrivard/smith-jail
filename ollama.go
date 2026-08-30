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
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
)

const (
	// OllamaContainerName is the sidecar's fixed container name. There is
	// exactly one, shared by every hermes-local session — unlike agent
	// containers, it is not scoped per project and not torn down on exit.
	OllamaContainerName = "smithjail-ollama"

	// OllamaNetworkName is a persistent, user-defined bridge network that
	// exists solely so the sidecar can be reached by container name.
	// Docker's default "bridge" network (what non-jailed agent sessions use)
	// has no embedded DNS, and --network-jail sessions get a fresh,
	// differently-addressed bridge every run — neither gives hermes-local a
	// stable address to point at. This network is created once and reused.
	OllamaNetworkName = "smithjail-ollama-net"

	// OllamaVolumeName holds Ollama's model cache, independent of the
	// container's own lifecycle — stopping/recreating the sidecar never
	// re-downloads models.
	OllamaVolumeName = "smithjail-ollama-models"

	// ollamaContainerPort is the port Ollama listens on inside the sidecar.
	ollamaContainerPort = "11434"
)

// OllamaKnownModels are model tags verified by hand against Ollama's
// /v1/chat/completions endpoint to return real, structured tool_calls — the
// bar Hermes needs for agentic tool use. Offered first in the TUI's model
// picker; anything else can still be typed in, but treat it as unverified
// until checked the same way (see envTemplateOllama's note on hermes3/hermes4
// and qwen2.5-coder, which look tool-capable but aren't).
var OllamaKnownModels = []string{"llama3.1:8b"}

// OllamaStatus reports whether the sidecar container has ever been created
// and, if so, whether it's currently running.
func OllamaStatus() (exists, running bool, err error) {
	cli, err := dockerClient()
	if err != nil {
		return false, false, err
	}
	defer cli.Close()

	inspect, err := cli.ContainerInspect(context.Background(), OllamaContainerName)
	if err != nil {
		if client.IsErrNotFound(err) {
			return false, false, nil
		}
		return false, false, err
	}
	return true, inspect.State != nil && inspect.State.Running, nil
}

// EnsureOllamaNetwork creates the sidecar's dedicated bridge network if it
// doesn't already exist. Unlike the per-session jail networks in network.go,
// this one is permanent.
func EnsureOllamaNetwork() error {
	cli, err := dockerClient()
	if err != nil {
		return err
	}
	defer cli.Close()

	_, err = cli.NetworkInspect(context.Background(), OllamaNetworkName, network.InspectOptions{})
	if err == nil {
		return nil
	}
	if !client.IsErrNotFound(err) {
		return fmt.Errorf("inspecting Ollama network: %w", err)
	}

	_, err = cli.NetworkCreate(context.Background(), OllamaNetworkName, network.CreateOptions{
		Driver: "bridge",
		Labels: map[string]string{"smithjail.managed": "true", "smithjail.component": "ollama"},
	})
	if err != nil {
		return fmt.Errorf("creating Ollama network: %w", err)
	}
	return nil
}

// ConnectOllamaToNetwork attaches the sidecar to an additional network — used
// so a --network-jail session's own dedicated bridge can resolve the sidecar
// by name too. This is a second, independent network attachment: it doesn't
// touch that jail's egress allowlist, which filters only on the jail's own
// bridge interface. Already-connected is not an error.
func ConnectOllamaToNetwork(networkName string) error {
	if networkName == "" || networkName == "bridge" || networkName == OllamaNetworkName {
		return nil
	}
	cli, err := dockerClient()
	if err != nil {
		return err
	}
	defer cli.Close()

	err = cli.NetworkConnect(context.Background(), networkName, OllamaContainerName, nil)
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		return fmt.Errorf("connecting Ollama sidecar to %s: %w", networkName, err)
	}
	return nil
}

// ensureOllamaVolume creates the model-cache volume if it doesn't exist yet.
func ensureOllamaVolume() error {
	cli, err := dockerClient()
	if err != nil {
		return err
	}
	defer cli.Close()

	_, err = cli.VolumeInspect(context.Background(), OllamaVolumeName)
	if err == nil {
		return nil
	}
	if !client.IsErrNotFound(err) {
		return fmt.Errorf("inspecting Ollama volume: %w", err)
	}

	_, err = cli.VolumeCreate(context.Background(), volume.CreateOptions{
		Name:   OllamaVolumeName,
		Labels: map[string]string{"smithjail.managed": "true", "smithjail.component": "ollama"},
	})
	if err != nil {
		return fmt.Errorf("creating Ollama volume: %w", err)
	}
	return nil
}

// StartOllamaContainer creates (if needed) and starts the sidecar, built
// from cfg's Ollama* settings. If the container already exists but is
// stopped, it's started in place rather than recreated — its image and
// device config only change via "smith-jail ollama recreate".
func StartOllamaContainer(cfg *Config) error {
	if err := EnsureOllamaNetwork(); err != nil {
		return err
	}
	if err := ensureOllamaVolume(); err != nil {
		return err
	}

	exists, running, err := OllamaStatus()
	if err != nil {
		return err
	}
	if running {
		return nil
	}

	cli, err := dockerClient()
	if err != nil {
		return err
	}
	defer cli.Close()

	if exists {
		return cli.ContainerStart(context.Background(), OllamaContainerName, container.StartOptions{})
	}

	image := cfg.OllamaImage
	if image == "" {
		image = "ollama/ollama"
	}
	if err := ensureOllamaImagePulled(cli, image); err != nil {
		return err
	}

	resp, err := cli.ContainerCreate(context.Background(),
		ollamaContainerConfig(cfg),
		ollamaHostConfig(cfg),
		&network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				OllamaNetworkName: {},
			},
		},
		nil, OllamaContainerName)
	if err != nil {
		return fmt.Errorf("creating Ollama container: %w", err)
	}

	return cli.ContainerStart(context.Background(), resp.ID, container.StartOptions{})
}

// ensureOllamaImagePulled pulls image if it isn't already present locally.
// Unlike the agent images — built locally from a generated Dockerfile —
// the Ollama sidecar uses a public image, and unlike "docker run",
// ContainerCreate does not pull automatically.
func ensureOllamaImagePulled(cli *client.Client, image string) error {
	_, _, err := cli.ImageInspectWithRaw(context.Background(), image)
	if err == nil {
		return nil
	}
	if !client.IsErrNotFound(err) {
		return fmt.Errorf("inspecting Ollama image: %w", err)
	}

	printInfo("Pulling " + image + " (first time only)...")
	cmd := exec.Command("docker", "pull", image)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker pull %s failed: %w", image, err)
	}
	return nil
}

func ollamaContainerConfig(cfg *Config) *container.Config {
	image := cfg.OllamaImage
	if image == "" {
		image = "ollama/ollama"
	}

	env := []string{"OLLAMA_HOST=0.0.0.0:" + ollamaContainerPort}
	if cfg.OllamaContextLength != "" {
		env = append(env, "OLLAMA_CONTEXT_LENGTH="+cfg.OllamaContextLength)
	}
	env = append(env, cfg.OllamaExtraEnv...)

	natPort, _ := nat.NewPort("tcp", ollamaContainerPort)

	return &container.Config{
		Image:        image,
		Env:          env,
		ExposedPorts: nat.PortSet{natPort: struct{}{}},
		Labels:       map[string]string{"smithjail.managed": "true", "smithjail.component": "ollama"},
	}
}

func ollamaHostConfig(cfg *Config) *container.HostConfig {
	port := cfg.OllamaPort
	if port == "" {
		port = ollamaContainerPort
	}
	natPort, _ := nat.NewPort("tcp", ollamaContainerPort)

	var devices []container.DeviceMapping
	for _, d := range cfg.OllamaDevices {
		devices = append(devices, container.DeviceMapping{
			PathOnHost:        d,
			PathInContainer:   d,
			CgroupPermissions: "rwm",
		})
	}

	return &container.HostConfig{
		Binds: []string{OllamaVolumeName + ":/root/.ollama"},
		// Published to loopback only, for "docker exec ... ollama pull" or
		// curl-ing the API from the host — not needed for hermes-local
		// itself, which reaches the sidecar over OllamaNetworkName by name.
		PortBindings: nat.PortMap{natPort: []nat.PortBinding{{HostIP: "127.0.0.1", HostPort: port}}},
		Resources:    container.Resources{Devices: devices},
		GroupAdd:     cfg.OllamaGroupAdd,
		RestartPolicy: container.RestartPolicy{
			Name: container.RestartPolicyUnlessStopped,
		},
	}
}

// StopOllamaContainer stops the sidecar without removing it — the model
// cache volume and container config persist, so the next start is fast.
func StopOllamaContainer() error {
	cli, err := dockerClient()
	if err != nil {
		return err
	}
	defer cli.Close()

	timeout := 10
	return cli.ContainerStop(context.Background(), OllamaContainerName, container.StopOptions{Timeout: &timeout})
}

// EnsureOllamaRunning is the run-time hook for hermes-local: if the sidecar
// isn't already running, offer to start it. Declining aborts the session —
// there is no point launching Hermes with nothing to talk to.
func EnsureOllamaRunning(cfg *Config) error {
	exists, running, err := OllamaStatus()
	if err != nil {
		return err
	}
	if running {
		printOK("Ollama sidecar already running.")
		return nil
	}

	action := "start"
	if exists {
		action = "restart"
	}
	if !confirm(fmt.Sprintf("%s the Ollama sidecar container (%s)", action, OllamaContainerName), cfg.AutoApprove) {
		return errAborted
	}

	printInfo("Starting Ollama sidecar...")
	if err := StartOllamaContainer(cfg); err != nil {
		return fmt.Errorf("starting Ollama sidecar: %w", err)
	}
	printOK("Ollama sidecar running — reachable at http://" + OllamaContainerName + ":" + ollamaContainerPort)
	return nil
}

// parseOllamaListOutput extracts model tags (the first column) from "ollama
// list" output, skipping the header row.
func parseOllamaListOutput(out []byte) []string {
	lines := strings.Split(string(out), "\n")
	if len(lines) > 0 {
		lines = lines[1:] // header row
	}
	var models []string
	for _, line := range lines {
		if fields := strings.Fields(line); len(fields) > 0 {
			models = append(models, fields[0])
		}
	}
	return models
}

// ollamaModelPresent reports whether model has already been pulled into the
// sidecar, by asking Ollama itself rather than guessing from the volume —
// "ollama list" is authoritative and doesn't need the registry.
func ollamaModelPresent(model string) (bool, error) {
	out, err := exec.Command("docker", "exec", OllamaContainerName, "ollama", "list").Output()
	if err != nil {
		return false, fmt.Errorf("listing sidecar models: %w", err)
	}
	for _, m := range parseOllamaListOutput(out) {
		if m == model {
			return true, nil
		}
	}
	return false, nil
}

// OllamaModelsPulled lists every model tag currently in the sidecar, for the
// TUI's model picker. Unlike ollamaModelPresent — used on the pull path,
// where the sidecar is already known to be running — this is called
// opportunistically whenever the picker opens, so a stopped or never-created
// sidecar is reported back as an empty list rather than an error: the picker
// still has OllamaKnownModels and the current selection to show regardless.
func OllamaModelsPulled() ([]string, error) {
	_, running, err := OllamaStatus()
	if err != nil || !running {
		return nil, err
	}
	out, err := exec.Command("docker", "exec", OllamaContainerName, "ollama", "list").Output()
	if err != nil {
		return nil, fmt.Errorf("listing sidecar models: %w", err)
	}
	return parseOllamaListOutput(out), nil
}

// EnsureOllamaModelPulled pulls model into the sidecar if it isn't already
// cached. The sidecar must already be running — pulling shells out to
// "docker exec ... ollama pull", same mechanism as "ollama pull" run by hand,
// just without requiring Ollama on the host. Declining the confirmation
// aborts the caller, same convention as EnsureOllamaRunning.
func EnsureOllamaModelPulled(cfg *Config, model string) error {
	if model == "" {
		return nil
	}
	present, err := ollamaModelPresent(model)
	if err != nil {
		return err
	}
	if present {
		return nil
	}

	if !confirm(fmt.Sprintf("pull Ollama model %s into the sidecar (one-time download)", model), cfg.AutoApprove) {
		return errAborted
	}

	printInfo("Pulling " + model + " into the sidecar (first time only)...")
	cmd := exec.Command("docker", "exec", OllamaContainerName, "ollama", "pull", model)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pulling model %s: %w", model, err)
	}
	printOK("Model " + model + " ready.")
	return nil
}

// SeedHermesLocalConfig writes a starter ~/.hermes-local/config.yaml pointing
// Hermes at the Ollama sidecar, but only if the file doesn't already exist —
// so a config produced by "hermes model" or hand-editing is never clobbered.
// This is what lets "hermes-local run" reach the sidecar with zero manual
// setup instead of requiring "hermes model → Custom endpoint" every session.
func SeedHermesLocalConfig(cfg *Config) error {
	ensureHostDir(cfg.HermesLocalConfig, cfg)

	content := fmt.Sprintf(`# Written by smith-jail so hermes-local talks to the Ollama sidecar with
# zero manual setup. Safe to edit — smith-jail only writes this file once,
# the first time it's missing. Change the model with "hermes model" or
# "hermes config set model.default <tag>"; pull new tags first with
# "docker exec %s ollama pull <tag>".
#
# context_length is just the value Hermes displays; ollama_num_ctx is what
# actually controls how much context Ollama allocates at runtime for the
# model — without it Ollama silently loads a much smaller default (32768)
# regardless of context_length or the sidecar's OLLAMA_CONTEXT_LENGTH, and
# Hermes then refuses tool use for being under its 64k minimum. Both must
# match to avoid that mismatch.
model:
  default: %s
  provider: custom
  base_url: http://%s:%s/v1
  context_length: %s
  ollama_num_ctx: %s
`, OllamaContainerName, cfg.OllamaModel, OllamaContainerName, ollamaContainerPort, cfg.OllamaContextLength, cfg.OllamaContextLength)

	path := filepath.Join(cfg.HermesLocalConfig, "config.yaml")
	if err := writeFileIfAbsent(path, content); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// MaybeStopOllama is the exit-time hook: ask whether to stop the sidecar.
// Defaults to No — it's meant to stay warm across sessions so models aren't
// reloaded from disk on every run. Under AutoApprove there's no prompt at
// all: the default (leave it running) is what non-interactive mode gets.
func MaybeStopOllama(cfg *Config) {
	if cfg.AutoApprove {
		return
	}
	_, running, err := OllamaStatus()
	if err != nil || !running {
		return
	}
	fmt.Println()
	if !readYesNo("  Stop the Ollama sidecar container? [y/N] ", false) {
		printInfo("Leaving Ollama sidecar running.")
		return
	}
	if err := StopOllamaContainer(); err != nil {
		printWarn("Could not stop Ollama sidecar: " + err.Error())
		return
	}
	printOK("Ollama sidecar stopped.")
}
