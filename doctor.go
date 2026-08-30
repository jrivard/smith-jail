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
	"runtime"
	"syscall"
	"time"

	"github.com/docker/docker/client"
)

// checkStatus is the outcome of one doctor check.
type checkStatus int

const (
	statusOK checkStatus = iota
	statusWarn
	statusFail
)

// DoctorCheck is one diagnostic result: what was checked, how it came out,
// a human-readable detail, and — for anything short of statusOK — a
// suggested fix.
type DoctorCheck struct {
	Name   string
	Status checkStatus
	Detail string
	Fix    string
}

// RunDoctorChecks inspects the host for everything smith-jail depends on:
// Docker itself, the optional network-jail toolchain, and the config and
// credential directories LoadConfig manages. cfg may be nil if config
// loading itself failed, in which case only Docker checks run.
func RunDoctorChecks(cfg *Config) []DoctorCheck {
	var checks []DoctorCheck

	checks = append(checks, checkDockerBinary())
	dockerUp := checkDockerDaemon()
	checks = append(checks, dockerUp)

	checks = append(checks, checkNftBinary())
	checks = append(checks, checkNetAdminCapability())
	checks = append(checks, checkSELinuxStatus())

	if cfg != nil {
		checks = append(checks, checkDirWritable("Config directory", cfg.UserConfigDir))
		checks = append(checks, checkDirWritable("Build/cache directory", cfg.BuildDir))
		checks = append(checks, checkDiskSpace(cfg.BuildDir))
		checks = append(checks, checkCredentialDir("Claude credentials (~/.claude)", cfg.ClaudeConfig, cfg))
		checks = append(checks, checkCredentialDir("Gemini credentials (~/.gemini)", cfg.GeminiConfig, cfg))
		checks = append(checks, checkCredentialDir("Codex credentials (~/.codex)", cfg.CodexConfig, cfg))
		checks = append(checks, checkCredentialDir("Hermes credentials (~/.hermes)", cfg.HermesConfig, cfg))
		checks = append(checks, checkCredentialDir("Hermes-local credentials (~/.hermes-local)", cfg.HermesLocalConfig, cfg))
		checks = append(checks, checkOllamaSidecar())
	}

	return checks
}

// checkOllamaSidecar reports the hermes-local sidecar's state. Not running
// is not a failure — "run"/"shell" offer to start it on demand — but it's
// worth surfacing so "why is hermes-local hanging" has an obvious answer.
func checkOllamaSidecar() DoctorCheck {
	exists, running, err := OllamaStatus()
	if err != nil {
		return DoctorCheck{Name: "Ollama sidecar (hermes-local)", Status: statusWarn, Detail: err.Error()}
	}
	switch {
	case running:
		return DoctorCheck{Name: "Ollama sidecar (hermes-local)", Status: statusOK, Detail: "running"}
	case exists:
		return DoctorCheck{Name: "Ollama sidecar (hermes-local)", Status: statusOK, Detail: "stopped — will be offered on next hermes-local run"}
	default:
		return DoctorCheck{Name: "Ollama sidecar (hermes-local)", Status: statusOK, Detail: "not created yet — will be offered on first hermes-local run"}
	}
}

// AnyFailed reports whether any check in the list failed outright.
func AnyFailed(checks []DoctorCheck) bool {
	for _, c := range checks {
		if c.Status == statusFail {
			return true
		}
	}
	return false
}

func checkDockerBinary() DoctorCheck {
	path, err := exec.LookPath("docker")
	if err != nil {
		return DoctorCheck{
			Name:   "Docker binary",
			Status: statusFail,
			Detail: "docker not found in PATH",
			Fix:    "sudo zypper install docker && sudo systemctl enable --now docker",
		}
	}
	return DoctorCheck{Name: "Docker binary", Status: statusOK, Detail: path}
}

// checkDockerDaemon pings the Docker daemon and, on success, reports its
// version — a permission error here (the current user isn't in the docker
// group) looks different from "daemon not running", so it gets its own fix.
func checkDockerDaemon() DoctorCheck {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return DoctorCheck{
			Name:   "Docker daemon",
			Status: statusFail,
			Detail: err.Error(),
			Fix:    "sudo systemctl start docker",
		}
	}
	defer cli.Close()

	ping, err := cli.Ping(ctx)
	if err != nil {
		detail := err.Error()
		fix := "sudo systemctl start docker"
		if os.IsPermission(err) {
			fix = "sudo usermod -aG docker $USER   (then log out and back in)"
		}
		return DoctorCheck{Name: "Docker daemon", Status: statusFail, Detail: detail, Fix: fix}
	}

	detail := "reachable"
	if ping.APIVersion != "" {
		detail = fmt.Sprintf("reachable (API %s)", ping.APIVersion)
	}
	return DoctorCheck{Name: "Docker daemon", Status: statusOK, Detail: detail}
}

// checkNftBinary and checkNetAdminCapability are only needed for
// --network-jail, so their absence is a warning rather than a failure —
// everything else in smith-jail works fine without them. On non-Linux hosts
// --network-jail is unavailable outright (nftables is a Linux kernel
// feature), so these are informational rather than something to fix.
func checkNftBinary() DoctorCheck {
	if runtime.GOOS != "linux" {
		return DoctorCheck{Name: "nftables (--network-jail)", Status: statusOK, Detail: "not applicable on " + runtime.GOOS + " — --network-jail is Linux-only"}
	}
	path, err := findNft()
	if err != nil {
		return DoctorCheck{
			Name:   "nftables (--network-jail)",
			Status: statusWarn,
			Detail: "nft not found in PATH or common sbin locations",
			Fix:    "sudo zypper install nftables",
		}
	}
	return DoctorCheck{Name: "nftables (--network-jail)", Status: statusOK, Detail: path}
}

func checkNetAdminCapability() DoctorCheck {
	if runtime.GOOS != "linux" {
		return DoctorCheck{Name: "NET_ADMIN capability (--network-jail)", Status: statusOK, Detail: "not applicable on " + runtime.GOOS}
	}
	if _, err := findNft(); err != nil {
		return DoctorCheck{
			Name:   "NET_ADMIN capability (--network-jail)",
			Status: statusWarn,
			Detail: "cannot check without nft installed",
		}
	}
	if err := checkNetAdmin(); err != nil {
		return DoctorCheck{
			Name:   "NET_ADMIN capability (--network-jail)",
			Status: statusWarn,
			Detail: "missing — nft cannot manage firewall rules as this user",
			Fix:    "sudo setcap cap_net_admin+ep $(which smith-jail)",
		}
	}
	return DoctorCheck{Name: "NET_ADMIN capability (--network-jail)", Status: statusOK, Detail: "granted"}
}

// checkSELinuxStatus is informational only: enforcing, permissive, and
// disabled are all supported (see mountLabel), so nothing here can fail.
func checkSELinuxStatus() DoctorCheck {
	if runtime.GOOS != "linux" {
		return DoctorCheck{Name: "SELinux", Status: statusOK, Detail: "not applicable on " + runtime.GOOS}
	}
	b, err := os.ReadFile("/sys/fs/selinux/enforce")
	if err != nil {
		return DoctorCheck{Name: "SELinux", Status: statusOK, Detail: "not present on this host"}
	}
	mode := "permissive"
	if string(b) == "1" {
		mode = "enforcing"
	}
	return DoctorCheck{
		Name:   "SELinux",
		Status: statusOK,
		Detail: mode + " — bind mounts will be labelled automatically",
	}
}

// checkDirWritable ensures a directory smith-jail needs to write into
// exists (creating it if not, mirroring what LoadConfig itself does) and is
// actually writable by this process.
func checkDirWritable(name, path string) DoctorCheck {
	if err := os.MkdirAll(path, 0755); err != nil {
		return DoctorCheck{
			Name:   name,
			Status: statusFail,
			Detail: fmt.Sprintf("%s: cannot create: %v", path, err),
		}
	}
	probe := filepath.Join(path, ".smith-jail-doctor-probe")
	if err := os.WriteFile(probe, []byte("x"), 0600); err != nil {
		return DoctorCheck{
			Name:   name,
			Status: statusFail,
			Detail: fmt.Sprintf("%s: not writable: %v", path, err),
			Fix:    fmt.Sprintf("sudo chown -R $USER %s", path),
		}
	}
	_ = os.Remove(probe)
	return DoctorCheck{Name: name, Status: statusOK, Detail: path}
}

// checkDiskSpace warns below 2GB free — comfortably less than a single
// Debian-based agent image with build tooling needs.
func checkDiskSpace(path string) DoctorCheck {
	const minFreeBytes = 2 << 30 // 2 GiB

	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return DoctorCheck{Name: "Disk space (build dir)", Status: statusWarn, Detail: "cannot stat: " + err.Error()}
	}
	free := stat.Bavail * uint64(stat.Bsize)
	detail := formatSize(int64(free)) + " free"
	if free < minFreeBytes {
		return DoctorCheck{
			Name:   "Disk space (build dir)",
			Status: statusWarn,
			Detail: detail,
			Fix:    "docker builder prune   (or free up space on " + path + ")",
		}
	}
	return DoctorCheck{Name: "Disk space (build dir)", Status: statusOK, Detail: detail}
}

// checkCredentialDir reports whether an agent's credential directory exists
// and, if so, whether it's owned by the UID/GID the container runs as —
// mismatched ownership is exactly the EACCES-on-every-write failure mode
// warnIfNotOwned guards against at run time.
func checkCredentialDir(name, path string, cfg *Config) DoctorCheck {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return DoctorCheck{Name: name, Status: statusOK, Detail: "not created yet — will be created on first run"}
	}
	if err != nil {
		return DoctorCheck{Name: name, Status: statusWarn, Detail: err.Error()}
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return DoctorCheck{Name: name, Status: statusOK, Detail: path}
	}
	if int(st.Uid) != cfg.UID || int(st.Gid) != cfg.GID {
		return DoctorCheck{
			Name:   name,
			Status: statusWarn,
			Detail: fmt.Sprintf("owned by uid %d:%d, container runs as %d:%d", st.Uid, st.Gid, cfg.UID, cfg.GID),
			Fix:    fmt.Sprintf("sudo chown -R %d:%d %s", cfg.UID, cfg.GID, path),
		}
	}
	return DoctorCheck{Name: name, Status: statusOK, Detail: path}
}

// ── CLI ─────────────────────────────────────────────────────────────────────

func cmdDoctor() {
	cfg, err := LoadConfig(".")
	if err != nil {
		printWarn("Could not load config: " + err.Error())
	}

	fmt.Println()
	printHeader("Smith Jail — Doctor", colorBlue)
	fmt.Println()

	checks := RunDoctorChecks(cfg)
	for _, c := range checks {
		printDoctorCheck(c)
	}

	fmt.Println()
	if AnyFailed(checks) {
		printWarn("One or more checks failed — see fixes above.")
		os.Exit(1)
	}
	printOK("All checks passed.")
}

func printDoctorCheck(c DoctorCheck) {
	switch c.Status {
	case statusOK:
		fmt.Printf("%s✔%s %-38s %s\n", colorGreen, colorReset, c.Name, c.Detail)
	case statusWarn:
		fmt.Printf("%s⚠%s  %-38s %s\n", colorYellow, colorReset, c.Name, c.Detail)
	case statusFail:
		fmt.Printf("%s✖%s %-38s %s\n", colorRed, colorReset, c.Name, c.Detail)
	}
	if c.Fix != "" {
		fmt.Printf("     %sFix: %s%s\n", colorDim, c.Fix, colorReset)
	}
}
