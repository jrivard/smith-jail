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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
)

// dockerClient creates and returns a Docker client.
func dockerClient() (*client.Client, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("cannot connect to Docker: %w\n  Is Docker running? Try: sudo systemctl start docker", err)
	}
	return cli, nil
}

// ImageExists returns true if the image built from cfg's effective settings
// exists locally.
func ImageExists(agent *Agent, cfg *Config) (bool, error) {
	cli, err := dockerClient()
	if err != nil {
		return false, err
	}
	defer cli.Close()

	_, _, err = cli.ImageInspectWithRaw(context.Background(), agent.ImageRef(cfg))
	if err != nil {
		if client.IsErrNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// AnyAgentImageExists reports whether at least one image (any tag) has ever
// been built for this agent, regardless of which project's configuration
// produced it. Used only to phrase EnsureImage's confirmation prompt.
func AnyAgentImageExists(agent *Agent) bool {
	imgs, err := ListAgentImages(agent)
	return err == nil && len(imgs) > 0
}

// InstalledVersion returns the agent version installed in the image built
// from cfg's effective settings.
func InstalledVersion(agent *Agent, cfg *Config) string {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx,
		"docker", "run", "--rm",
		"--entrypoint", agent.BinaryName,
		agent.ImageRef(cfg), "--version",
	).Output()
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(out))
	// Find the first token that looks like a version number (digits or 'v'+digits).
	for _, part := range strings.Fields(line) {
		v := strings.TrimPrefix(part, "v")
		if len(v) > 0 && v[0] >= '0' && v[0] <= '9' {
			return v
		}
	}
	return ""
}

// LatestVersion fetches the latest version of the agent from the npm registry.
func LatestVersion(agent *Agent) string {
	if agent.NPMPackage == "" {
		return ""
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	url := "https://registry.npmjs.org/" + agent.NPMPackage + "/latest"
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()

	var result struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return ""
	}
	return result.Version
}

// BuildImage builds the image for cfg's effective settings, tagged
// content-addressed by agent.ImageRef(cfg).
func BuildImage(agent *Agent, cfg *Config, noCache bool) error {
	if err := cfg.WriteBuildContext(agent); err != nil {
		return fmt.Errorf("preparing build context: %w", err)
	}

	args := []string{"build", "--progress", "plain"}
	if noCache {
		args = append(args, "--no-cache")
	}
	args = append(args,
		"--build-arg", fmt.Sprintf("HOST_UID=%d", cfg.UID),
		"--build-arg", fmt.Sprintf("HOST_GID=%d", cfg.GID),
		"-t", agent.ImageRef(cfg),
		"-f", filepath.Join(cfg.BuildDir, agent.Name+"-Dockerfile"),
		cfg.BuildDir,
	)

	cmd := exec.Command("docker", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker build failed: %w", err)
	}

	return nil
}

// RemoveImage removes the image built from cfg's effective settings. Other
// tags in the same repository (other projects' configurations) are untouched.
func RemoveImage(agent *Agent, cfg *Config) {
	cli, err := dockerClient()
	if err != nil {
		return
	}
	defer cli.Close()

	ref := agent.ImageRef(cfg)
	_, err = cli.ImageRemove(context.Background(), ref,
		image.RemoveOptions{Force: true, PruneChildren: true})
	if err == nil {
		printOK("Image removed: " + ref)
	}
}

// ListAgentImages returns every local image tagged under agent's repository
// — one entry per distinct effective configuration ever built for it.
func ListAgentImages(agent *Agent) ([]image.Summary, error) {
	cli, err := dockerClient()
	if err != nil {
		return nil, err
	}
	defer cli.Close()

	f := filters.NewArgs()
	f.Add("reference", agent.ImageName()+":*")

	return cli.ImageList(context.Background(), image.ListOptions{Filters: f})
}

// RemoveAllAgentImages removes every tag in the agent's repository.
func RemoveAllAgentImages(agent *Agent) {
	cli, err := dockerClient()
	if err != nil {
		return
	}
	defer cli.Close()

	imgs, err := ListAgentImages(agent)
	if err != nil {
		return
	}
	for _, img := range imgs {
		for _, tag := range img.RepoTags {
			_, err := cli.ImageRemove(context.Background(), tag,
				image.RemoveOptions{Force: true, PruneChildren: true})
			if err == nil {
				printOK("Image removed: " + tag)
			}
		}
	}
}

// homeVolumeName returns the Docker volume name for a project's home directory.
func homeVolumeName(agent *Agent, hash string) string {
	return agent.ContainerPrefix() + "-home-" + hash
}

// EnsureHomeVolume creates the per-project home volume if it doesn't already exist.
func EnsureHomeVolume(agent *Agent, cfg *Config, hash, dir string) error {
	cli, err := dockerClient()
	if err != nil {
		return err
	}
	defer cli.Close()

	name := homeVolumeName(agent, hash)
	_, err = cli.VolumeInspect(context.Background(), name)
	if err == nil {
		return nil // already exists
	}
	if !client.IsErrNotFound(err) {
		return fmt.Errorf("inspecting home volume: %w", err)
	}

	if !confirm(fmt.Sprintf("create Docker volume %q for project home directory", name), cfg.AutoApprove) {
		return errAborted
	}

	_, err = cli.VolumeCreate(context.Background(), volume.CreateOptions{
		Name: name,
		Labels: map[string]string{
			"smithjail.project": dir,
			"smithjail.agent":   agent.Name,
		},
	})
	if err != nil {
		return fmt.Errorf("creating home volume %s: %w", name, err)
	}
	return nil
}

// RunAgent runs an agent session in a container, waiting for it to finish.
// It returns the container's exit code and any execution error.
// extraArgs are appended to the agent binary's command line inside the container.
func RunAgent(agent *Agent, cfg *Config, dir, networkArg string, extraArgs []string) (int, error) {
	hash := projectHash(dir)

	if err := EnsureHomeVolume(agent, cfg, hash, dir); err != nil {
		return -1, err
	}

	args := buildDockerRunArgs(agent, cfg, dir, hash, hash, networkArg)
	args = append(args, agent.ImageRef(cfg))

	if cfg.SkipPermissions {
		args = append(args, agent.SkipPermsFlag)
	}

	args = append(args, extraArgs...)

	return runDockerWait(args)
}

// RunShell runs a bash shell in a new container for the given directory.
// extraArgs are appended to the bash command line inside the container.
func RunShell(agent *Agent, cfg *Config, dir, networkArg string, extraArgs []string) (int, error) {
	hash := projectHash(dir)

	if err := EnsureHomeVolume(agent, cfg, hash, dir); err != nil {
		return -1, err
	}

	args := buildDockerRunArgs(agent, cfg, dir, hash+"-shell", hash, networkArg)
	args = append(args, "--entrypoint", "/bin/bash")
	args = append(args, agent.ImageRef(cfg))
	args = append(args, extraArgs...)

	return runDockerWait(args)
}

// RunSetup runs the agent binary's own "setup" subcommand inside a
// throwaway container — e.g. "hermes setup --portal" — instead of the
// agent's normal entrypoint. It uses the same credential-dir bind mounts as
// run/shell, so anything the setup flow writes (tokens, config) lands in
// the same host-side directory the agent reads from on every later run.
func RunSetup(agent *Agent, cfg *Config, dir, networkArg string, setupArgs []string) (int, error) {
	hash := projectHash(dir)

	if err := EnsureHomeVolume(agent, cfg, hash, dir); err != nil {
		return -1, err
	}

	args := buildDockerRunArgs(agent, cfg, dir, hash+"-setup", hash, networkArg)
	args = append(args, "--entrypoint", agent.BinaryName)
	args = append(args, agent.ImageRef(cfg))
	args = append(args, "setup")
	args = append(args, setupArgs...)

	return runDockerWait(args)
}

// ListProjectResources returns all containers, volumes, and networks for the
// given agent and project hash. If agent is nil, returns resources for all agents.
func ListProjectResources(agent *Agent, hash string) (containers []types.Container, volumes []*volume.Volume, nets []NetworkItem) {
	allContainers, allVolumes := ListAllResources(agent)
	for _, c := range allContainers {
		name := strings.TrimPrefix(c.Names[0], "/")
		if strings.Contains(name, hash) {
			containers = append(containers, c)
		}
	}
	for _, v := range allVolumes {
		if strings.Contains(v.Name, hash) {
			volumes = append(volumes, v)
		}
	}
	if allNets, err := ListNetworks(agent); err == nil {
		for _, n := range allNets {
			if strings.Contains(n.Name, hash) {
				nets = append(nets, n)
			}
		}
	}
	return
}

// ExecShell execs into an already-running container.
func ExecShell(containerName string) error {
	return execDocker([]string{"exec", "-it", containerName, "/bin/bash"})
}

// FindRunningContainer returns the name of a running container for the given agent and dir hash.
func FindRunningContainer(agent *Agent, dirHash string) string {
	cli, err := dockerClient()
	if err != nil {
		return ""
	}
	defer cli.Close()

	f := filters.NewArgs()
	f.Add("name", agent.ContainerPrefix()+"-"+dirHash)
	f.Add("status", "running")

	containers, err := cli.ContainerList(context.Background(),
		container.ListOptions{Filters: f})
	if err != nil || len(containers) == 0 {
		return ""
	}
	return strings.TrimPrefix(containers[0].Names[0], "/")
}

// ListAllResources returns all containers and volumes for the given agent.
// If agent is nil, returns resources for all agents.
func ListAllResources(agent *Agent) (containers []types.Container, volumes []*volume.Volume) {
	cli, err := dockerClient()
	if err != nil {
		return
	}
	defer cli.Close()

	namePrefix := "smithjail-"
	volPrefix := "smithjail-"
	if agent != nil {
		namePrefix = agent.ContainerPrefix() + "-"
		volPrefix = agent.ContainerPrefix() + "-home-"
	}

	f := filters.NewArgs()
	f.Add("name", namePrefix)
	containers, _ = cli.ContainerList(context.Background(),
		container.ListOptions{All: true, Filters: f})

	vf := filters.NewArgs()
	vf.Add("name", volPrefix)
	vols, err := cli.VolumeList(context.Background(), volume.ListOptions{Filters: vf})
	if err == nil {
		volumes = vols.Volumes
	}

	return
}

// printResourceList prints one line per container and volume, indented by
// indent. Shared by the clean commands and the post-run exit summary.
func printResourceList(indent string, containers []types.Container, volumes []*volume.Volume) {
	for _, c := range containers {
		name := strings.TrimPrefix(c.Names[0], "/")
		fmt.Printf("%scontainer: %-50s  [%s]\n", indent, name, c.State)
	}
	for _, v := range volumes {
		fmt.Printf("%svolume:    %s\n", indent, v.Name)
	}
}

// RemoveContainerList force-removes the given containers.
func RemoveContainerList(containers []types.Container) {
	cli, err := dockerClient()
	if err != nil {
		return
	}
	defer cli.Close()

	for _, c := range containers {
		_ = cli.ContainerRemove(context.Background(), c.ID,
			container.RemoveOptions{Force: true})
	}
	if len(containers) > 0 {
		printOK(fmt.Sprintf("Removed %d container(s).", len(containers)))
	}
}

// RemoveVolumeList removes the given volumes.
func RemoveVolumeList(volumes []*volume.Volume) {
	cli, err := dockerClient()
	if err != nil {
		return
	}
	defer cli.Close()

	for _, v := range volumes {
		_ = cli.VolumeRemove(context.Background(), v.Name, true)
	}
	if len(volumes) > 0 {
		printOK(fmt.Sprintf("Removed %d home volume(s).", len(volumes)))
	}
}

// StreamBuildOutput streams docker build output to stdout line by line.
func StreamBuildOutput(r io.Reader) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fmt.Println(scanner.Text())
	}
}

// ── SELinux bind-mount labelling ──────────────────────────────────────────────

// mountLabel returns the suffix to append to a bind mount so the container can
// access it under SELinux. On systems where SELinux is not active it returns an
// empty string, leaving mount specs unchanged.
//
// The behaviour can be overridden with JAIL_SELINUX:
//
//	off / false / 0   never label
//	on / true / 1 / z label shared (":z") — the default when SELinux is active
//	private / Z       label private (":Z") — one container only, not recommended
//	auto (unset)      detect from /sys/fs/selinux/enforce
//
// ":z" (shared) is the default because smith-jail runs more than one container
// against the same credential directory — the agent session and "shell". ":Z"
// stamps a unique MCS category on the files, so the second container would be
// denied access to what the first one relabelled.
func mountLabel() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("JAIL_SELINUX"))) {
	case "off", "false", "0", "no", "none":
		return ""
	case "on", "true", "1", "yes", "z", "shared":
		return ":z"
	case "private", "zz":
		return ":Z"
	case "", "auto":
		// fall through to detection
	default:
		printWarn("Unrecognised JAIL_SELINUX value; falling back to auto-detection.")
	}

	if selinuxEnforcing() {
		return ":z"
	}
	return ""
}

// selinuxEnforcing reports whether SELinux is loaded and in enforcing mode.
// A missing selinuxfs means SELinux is not enabled on this host; permissive
// mode logs denials but still allows access, so no relabelling is required.
func selinuxEnforcing() bool {
	b, err := os.ReadFile("/sys/fs/selinux/enforce")
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(b)) == "1"
}

// labelledMount joins a host path and a container path into a bind mount spec,
// appending the SELinux label when one is in effect.
//
// Relabelling is recursive and destructive, so it is refused for paths where
// that would be a bad idea — "/" or a whole home directory. In those cases the
// mount is returned unlabelled with a warning rather than silently relabelling
// thousands of unrelated files.
func labelledMount(hostPath, containerPath, label string) string {
	spec := hostPath + ":" + containerPath
	if label == "" {
		return spec
	}
	if unsafeToRelabel(hostPath) {
		printWarn(fmt.Sprintf(
			"Refusing to SELinux-relabel %s (too broad); mounting unlabelled. "+
				"Expect permission denials — mount a narrower directory.", hostPath))
		return spec
	}
	return spec + label
}

// unsafeToRelabel reports whether a recursive SELinux relabel of the given path
// would affect far more of the filesystem than intended.
func unsafeToRelabel(path string) bool {
	clean := filepath.Clean(path)
	if clean == "/" || clean == "." {
		return true
	}
	if home, err := os.UserHomeDir(); err == nil && filepath.Clean(home) == clean {
		return true
	}
	switch clean {
	case "/home", "/root", "/usr", "/etc", "/var", "/opt", "/srv", "/tmp", "/mnt", "/media":
		return true
	}
	return false
}

// ── Host credential directories ───────────────────────────────────────────────

// ensureHostDir creates a host directory that is about to be bind-mounted.
//
// This matters more than it looks: if the source path of a bind mount does not
// exist, the Docker daemon creates it, and the daemon runs as root — so the
// directory lands as root:root and the unprivileged "agent" user inside the
// container cannot write to it. For an agent's credential directory that shows
// up as EACCES on every write and a login prompt on every single run.
func ensureHostDir(path string, cfg *Config) {
	if err := os.MkdirAll(path, 0700); err != nil {
		printWarn(fmt.Sprintf("Cannot create %s: %v", path, err))
		return
	}
	warnIfNotOwned(path, cfg)
}

// warnIfNotOwned checks that a host path is owned by the user the container
// runs as, and explains how to repair it if not. It cannot fix the ownership
// itself — that needs root.
func warnIfNotOwned(path string, cfg *Config) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return
	}
	if int(st.Uid) == cfg.UID && int(st.Gid) == cfg.GID {
		return
	}
	printWarn(fmt.Sprintf(
		"%s is owned by uid %d:%d but the container runs as %d:%d — writes will fail.",
		path, st.Uid, st.Gid, cfg.UID, cfg.GID))
	printInfo(fmt.Sprintf("Fix with: sudo chown -R %d:%d %s", cfg.UID, cfg.GID, path))
}

// ── Internal helpers ──────────────────────────────────────────────────────────

func buildDockerRunArgs(agent *Agent, cfg *Config, dir, containerSuffix, baseHash, networkArg string) []string {
	label := mountLabel()

	args := []string{
		"run", "-it", "--rm",
		"--name", agent.ContainerPrefix() + "-" + containerSuffix,
		"--label", "smithjail.project=" + dir,
		"--label", "smithjail.agent=" + agent.Name,
		// Named volumes are labelled by Docker automatically; only bind
		// mounts of pre-existing host paths need an explicit label.
		"--volume", homeVolumeName(agent, baseHash) + ":/home/agent",
		"--volume", labelledMount(dir, "/workspace", label),
		"--env", "HOME=/home/agent",
		"--env", "USER=agent",
		"--env", "TERM=xterm-256color",
		"--env", "PATH=/usr/local/bin:/home/agent/.local/bin:/home/agent/.cargo/bin:/home/agent/go/bin:/home/agent/.npm-global/bin:/usr/bin:/usr/sbin:/bin:/sbin",
		"--memory", cfg.MemLimit,
		"--cpus", cfg.CPULimit,
		"--network", networkArg,
	}

	switch agent.Name {
	case "claude":
		// Ensure the host ~/.claude dir exists so Docker doesn't create it as
		// root. Claude Code stores its OAuth credentials here, so if it isn't
		// writable the session cannot persist a login.
		ensureHostDir(cfg.ClaudeConfig, cfg)
		warnIfNotOwned(cfg.ClaudeJSON, cfg)
		args = append(args,
			"--volume", labelledMount(cfg.ClaudeConfig, "/home/agent/.claude", label),
			"--volume", labelledMount(cfg.ClaudeJSON, "/home/agent/.claude.json", label),
			"--env", "ANTHROPIC_API_KEY=",
		)
	case "gemini":
		// Ensure the host ~/.gemini dir exists so the bind mount doesn't fail.
		ensureHostDir(cfg.GeminiConfig, cfg)
		args = append(args,
			"--volume", labelledMount(cfg.GeminiConfig, "/home/agent/.gemini", label),
		)
		if cfg.GeminiAPIKey != "" {
			args = append(args, "--env", "GEMINI_API_KEY="+cfg.GeminiAPIKey)
		}
	case "codex":
		// Ensure the host ~/.codex dir exists so the bind mount doesn't fail.
		ensureHostDir(cfg.CodexConfig, cfg)
		args = append(args,
			"--volume", labelledMount(cfg.CodexConfig, "/home/agent/.codex", label),
		)
		if cfg.OpenAIAPIKey != "" {
			args = append(args, "--env", "OPENAI_API_KEY="+cfg.OpenAIAPIKey)
		}
	case "hermes":
		// Ensure the host ~/.hermes dir exists so the bind mount doesn't fail.
		ensureHostDir(cfg.HermesConfig, cfg)
		args = append(args,
			"--volume", labelledMount(cfg.HermesConfig, "/home/agent/.hermes", label),
		)
	case "hermes-local":
		// Hermes always reads/writes $HOME/.hermes (i.e. /home/agent/.hermes
		// in the container) — there's no env var to redirect it. So despite
		// the host-side directory being named ~/.hermes-local (kept separate
		// from cfg.HermesConfig so its "local Ollama" model default never
		// fights with the cloud persona's), it must be bind-mounted AT
		// /home/agent/.hermes for Hermes to actually see it. This is safe
		// because hermes-local already gets its own isolated per-project
		// home volume (see homeVolumeName/agent.ContainerPrefix), so this
		// never collides with the plain "hermes" agent's own ~/.hermes.
		ensureHostDir(cfg.HermesLocalConfig, cfg)
		args = append(args,
			"--volume", labelledMount(cfg.HermesLocalConfig, "/home/agent/.hermes", label),
		)
	}

	return args
}

// execDocker replaces the current process with docker, inheriting stdio.
func execDocker(args []string) error {
	dockerPath, err := exec.LookPath("docker")
	if err != nil {
		return fmt.Errorf("docker not found in PATH")
	}
	allArgs := append([]string{dockerPath}, args...)
	return syscall.Exec(dockerPath, allArgs, os.Environ())
}

// runDockerWait runs docker as a child process with inherited stdio and waits for it to finish.
func runDockerWait(args []string) (int, error) {
	dockerPath, err := exec.LookPath("docker")
	if err != nil {
		return -1, fmt.Errorf("docker not found in PATH")
	}
	cmd := exec.Command(dockerPath, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err = cmd.Run()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode(), nil
		}
		return -1, err
	}
	return 0, nil
}

// CheckForUpdate checks npm for a newer version of the agent and prompts to rebuild.
func CheckForUpdate(agent *Agent, cfg *Config) {
	installed := InstalledVersion(agent, cfg)
	if installed == "" {
		return
	}

	latest := LatestVersion(agent)
	if latest == "" || installed == latest {
		return
	}

	fmt.Println()
	printWarn(fmt.Sprintf("%s update available: %s → %s", agent.DisplayName, installed, latest))

	var rebuild bool
	if cfg.AutoApprove {
		printInfo(fmt.Sprintf("rebuild Docker image %q (update) [auto-approved]", agent.ImageRef(cfg)))
		rebuild = true
	} else {
		rebuild = readYesNo("  Rebuild image now? [Y/n] ", true)
	}

	if rebuild {
		printInfo("Removing old image...")
		RemoveImage(agent, cfg)
		if err := BuildImage(agent, cfg, true); err != nil {
			printWarn("Rebuild failed: " + err.Error())
		} else {
			printOK("Image updated.")
		}
	} else {
		printInfo(fmt.Sprintf("Skipping. Run: smith-jail %s rebuild", agent.Name))
	}
	fmt.Println()
}

// acquireBuildLock exclusively flocks a per-agent lock file so concurrent
// processes don't race to build the same image.
func acquireBuildLock(agent *Agent, cfg *Config) (func(), error) {
	lockPath := filepath.Join(cfg.BuildDir, agent.Name+".build.lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("cannot open build lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("cannot acquire build lock: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// EnsureImage builds the image for cfg's effective settings if it doesn't
// already exist. Because tags are content-addressed, "doesn't exist" is the
// only rebuild trigger there is — no separate staleness check is needed.
func EnsureImage(agent *Agent, cfg *Config) error {
	exists, err := ImageExists(agent, cfg)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}

	unlock, err := acquireBuildLock(agent, cfg)
	if err != nil {
		return err
	}
	defer unlock()

	// Re-check after acquiring the lock.
	exists, err = ImageExists(agent, cfg)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}

	reason := "first time"
	if AnyAgentImageExists(agent) {
		reason = "new configuration for this project"
	}

	var desc string
	if _, err := os.Stat(cfg.InitScript); err == nil {
		desc = fmt.Sprintf("build Docker image %q (%s, init.sh present, packages: %s)", agent.ImageRef(cfg), reason, cfg.InitPackages)
	} else if cfg.InitPackages != "" {
		desc = fmt.Sprintf("build Docker image %q (%s, packages: %s)", agent.ImageRef(cfg), reason, cfg.InitPackages)
	} else {
		desc = fmt.Sprintf("build Docker image %q (%s)", agent.ImageRef(cfg), reason)
	}

	if !confirm(desc, cfg.AutoApprove) {
		return errAborted
	}

	printInfo("Building image...")

	return BuildImage(agent, cfg, false)
}

// ImageInfo holds display-relevant details about a managed image.
type ImageInfo struct {
	Exists  bool
	Size    int64
	Created string // RFC3339
}

// GetImageInfo returns metadata about the image built from cfg's effective
// settings.
func GetImageInfo(agent *Agent, cfg *Config) (ImageInfo, error) {
	cli, err := dockerClient()
	if err != nil {
		return ImageInfo{}, err
	}
	defer cli.Close()

	inspect, _, err := cli.ImageInspectWithRaw(context.Background(), agent.ImageRef(cfg))
	if err != nil {
		if client.IsErrNotFound(err) {
			return ImageInfo{}, nil
		}
		return ImageInfo{}, err
	}
	return ImageInfo{Exists: true, Size: inspect.Size, Created: inspect.Created}, nil
}

// NetworkItem holds display-relevant details about a managed Docker network.
type NetworkItem struct {
	Name    string
	Project string
	Agent   string
}

// ListNetworks returns all smith-jail Docker networks.
// If agent is nil, returns networks for all agents.
func ListNetworks(agent *Agent) ([]NetworkItem, error) {
	cli, err := dockerClient()
	if err != nil {
		return nil, err
	}
	defer cli.Close()

	prefix := "smithjail"
	if agent != nil {
		prefix = agent.ContainerPrefix()
	}

	f := filters.NewArgs()
	f.Add("name", prefix)
	nets, err := cli.NetworkList(context.Background(), network.ListOptions{Filters: f})
	if err != nil {
		return nil, err
	}
	items := make([]NetworkItem, len(nets))
	for i, n := range nets {
		items[i] = NetworkItem{
			Name:    n.Name,
			Project: n.Labels["smithjail.project"],
			Agent:   n.Labels["smithjail.agent"],
		}
	}
	return items, nil
}

// CheckDocker verifies Docker is available and responding.
func CheckDocker() error {
	if _, err := exec.LookPath("docker"); err != nil {
		return fmt.Errorf("docker not found in PATH\n  Install: sudo zypper install docker && sudo systemctl enable --now docker")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return fmt.Errorf("cannot connect to Docker daemon: %w", err)
	}
	defer cli.Close()
	if _, err := cli.Ping(ctx); err != nil {
		return fmt.Errorf("Docker daemon not responding: %w\n  Try: sudo systemctl start docker", err)
	}
	return nil
}
