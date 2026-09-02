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

//go:build integration

package main

// Adversarial tests for the filesystem jail — the claim (README, "What's
// isolated") that only the target project directory is ever visible inside
// the container, and that nothing an agent does inside the container can
// reach or persist outside of it.
//
// These need a working Docker daemon, so they're gated behind the
// "integration" build tag and skip (rather than fail) if Docker isn't
// reachable:
//
//	go test -tags=integration -run TestFilesystemJail ./...
//
// They deliberately don't go through the full smith-jail image build
// (BuildImage), which needs network access for apt/npm and takes minutes —
// the property under test is the mount topology docker.go's
// buildDockerRunArgs sets up (one bind mount for the project directory, one
// named volume for the home directory), which is independent of whatever
// tooling later gets layered into the image. A small stock image is enough
// to exercise it, and keeps this fast.
//
// Every Docker resource these tests create is randomly named
// ("smithjail-fjail-test-...") and torn down in t.Cleanup, so they can never
// collide with or disturb a real smith-jail session on the machine running
// them.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const jailTestImage = "debian:trixie-slim" // matches Config's own default BaseImage

// dockerRunTimeout bounds a single "docker run" invocation. Without it, a
// stuck daemon or registry pull hangs the *exec.Cmd's Wait() forever and the
// whole test binary eventually dies with a 10-minute panic instead of a
// readable failure pointing at the actual command that stalled.
const dockerRunTimeout = 60 * time.Second

// requireDocker skips the test if Docker isn't installed and reachable, and
// ensures jailTestImage is pulled so later docker run calls in the test body
// don't block on an implicit pull. CheckDocker only pings the daemon; it
// says nothing about whether the daemon can actually reach a registry, so
// the pull is attempted separately and bounded with its own timeout.
func requireDocker(t *testing.T) {
	t.Helper()
	if err := CheckDocker(); err != nil {
		t.Skipf("Docker not available, skipping filesystem jail test: %v", err)
	}
	ensureJailTestImage(t)
}

var (
	jailTestImageOnce sync.Once
	jailTestImageErr  error
)

// ensureJailTestImage pulls jailTestImage once per test run (cached across
// subtests via sync.Once) with a bounded timeout, skipping the calling test
// if the image can't be obtained — e.g. no network reachable to the
// registry — rather than letting a later "docker run" hang indefinitely.
func ensureJailTestImage(t *testing.T) {
	t.Helper()
	jailTestImageOnce.Do(func() {
		if err := exec.Command("docker", "image", "inspect", jailTestImage).Run(); err == nil {
			return // already cached locally
		}
		ctx, cancel := context.WithTimeout(context.Background(), dockerRunTimeout)
		defer cancel()
		out, err := exec.CommandContext(ctx, "docker", "pull", jailTestImage).CombinedOutput()
		if err != nil {
			jailTestImageErr = fmt.Errorf("pulling %s: %w\n%s", jailTestImage, err, out)
		}
	})
	if jailTestImageErr != nil {
		t.Skipf("could not obtain test image, skipping filesystem jail test: %v", jailTestImageErr)
	}
}

// newProjectDir creates a temp directory suitable for bind-mounting into the
// container as /workspace.
//
// Under rootless Docker, container "root" is remapped via /etc/subuid to an
// unprivileged host UID with no DAC-override, so every directory on the path
// down to the mount needs to be at least world-traversable or the mount is
// simply unreadable from inside the container ("Permission denied").
// t.TempDir() defaults each directory it hands out to 0755, but nests it
// inside a per-test parent directory (shared across all of that test's
// TempDir calls) that the testing package creates with os.MkdirTemp's
// default 0700 — chmod-ing the leaf doesn't help if an ancestor still blocks
// traversal. Making our own directory straight under os.TempDir() (already
// world-traversable, e.g. /tmp at 1777) sidesteps that shared parent
// entirely.
func newProjectDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "smithjail-fjail-project-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// testVolumeName returns a randomly-suffixed, obviously-test-scoped Docker
// volume name and registers its removal on test cleanup.
func testVolumeName(t *testing.T) string {
	t.Helper()
	id, err := newSessionID()
	if err != nil {
		t.Fatal(err)
	}
	name := "smithjail-fjail-test-" + id
	t.Cleanup(func() {
		_ = exec.Command("docker", "volume", "rm", "-f", name).Run()
	})
	return name
}

// runInJail runs shCmd inside a throwaway, non-interactive container with
// the project directory bind-mounted at /workspace and the given named
// volume at /home/agent — the same two mount points buildDockerRunArgs sets
// up for a real session. It returns combined stdout+stderr; a non-zero exit
// from shCmd is not itself a test failure, since several of these tests
// expect the probed command to fail.
func runInJail(t *testing.T, projectDir, homeVolume, shCmd string) string {
	t.Helper()

	id, err := newSessionID()
	if err != nil {
		t.Fatal(err)
	}
	containerName := "smithjail-fjail-test-" + id

	// Mirrors buildDockerRunArgs: on an SELinux-enforcing host, a bind mount
	// without a relabel suffix is denied to the container regardless of its
	// DAC permissions — labelledMount is a no-op everywhere else.
	args := []string{
		"run", "--rm", "-i",
		"--name", containerName,
		"--volume", labelledMount(projectDir, "/workspace", mountLabel()),
		"--volume", homeVolume + ":/home/agent",
		jailTestImage,
		"sh", "-c", shCmd,
	}

	ctx, cancel := context.WithTimeout(context.Background(), dockerRunTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "docker", args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	runErr := cmd.Run()

	// Killing the docker CLI client (what CommandContext's timeout does) does
	// not stop the container on the daemon side — it just severs the client
	// connection. --rm only cleans up on the container's own exit, so a
	// hung command inside the container (e.g. one that stalls reading
	// /proc/kcore) would otherwise keep running, and consuming CPU,
	// indefinitely. Force-removing by name here guarantees no orphaned
	// container survives this call, however it ended.
	_ = exec.Command("docker", "rm", "-f", containerName).Run()

	if runErr != nil {
		if ctx.Err() == context.DeadlineExceeded {
			t.Fatalf("docker run did not complete within %s (the command inside the container was hung, and has now been force-killed):\ncmd: docker %s\noutput so far:\n%s",
				dockerRunTimeout, strings.Join(args, " "), out.String())
		}
		if _, ok := runErr.(*exec.ExitError); !ok {
			t.Fatalf("running docker: %v\noutput:\n%s", runErr, out.String())
		}
	}
	return out.String()
}

// TestFilesystemJailHostFilesAreInvisible plants a canary well outside the
// project directory — simulating another project, or a host credential
// directory — and confirms nothing inside the container can see it: only
// the mounted project directory should be reachable at all.
func TestFilesystemJailHostFilesAreInvisible(t *testing.T) {
	requireDocker(t)

	projectDir := newProjectDir(t)
	if err := os.WriteFile(filepath.Join(projectDir, "README.md"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}

	outsideDir := t.TempDir() // stands in for "another project" or a host credential directory
	token := "SMITHJAIL_CANARY_" + mustSessionID(t)
	if err := os.WriteFile(filepath.Join(outsideDir, "secret.txt"), []byte(token), 0644); err != nil {
		t.Fatal(err)
	}

	homeVol := testVolumeName(t)

	// Excludes /proc, /sys, /dev: pseudo-filesystems, not real host files, and
	// grep-ing them can hang — /proc/kcore in particular reports a virtual
	// size in the terabytes, so a naive "grep -r /" can stall for a very long
	// time reading it, which looks exactly like a stuck Docker daemon.
	const grepCmd = "grep -r --exclude-dir=proc --exclude-dir=sys --exclude-dir=dev " + "TOKEN" + " / 2>/dev/null; echo ---; ls /workspace"
	out := runInJail(t, projectDir, homeVol, strings.Replace(grepCmd, "TOKEN", token, 1))
	if strings.Contains(out, token) {
		t.Fatalf("canary planted outside the project directory was visible inside the container:\n%s", out)
	}
	if !strings.Contains(out, "README.md") {
		t.Fatalf("the project directory's own contents were not visible at /workspace:\n%s", out)
	}
}

// TestFilesystemJailSymlinkCannotEscapeMount plants a symlink inside the
// project directory that points at an absolute host path outside it. If the
// jail were just "restrict what paths the agent types", this would leak the
// target's contents. Because it's an actual bind mount, the symlink is
// dereferenced inside the container's own mount namespace, where that host
// path was never mounted at all — so it must simply fail to resolve.
func TestFilesystemJailSymlinkCannotEscapeMount(t *testing.T) {
	requireDocker(t)

	outsideDir := t.TempDir()
	token := "SMITHJAIL_CANARY_" + mustSessionID(t)
	outsideFile := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte(token), 0644); err != nil {
		t.Fatal(err)
	}

	projectDir := newProjectDir(t)
	if err := os.Symlink(outsideFile, filepath.Join(projectDir, "escape")); err != nil {
		t.Fatal(err)
	}

	homeVol := testVolumeName(t)

	out := runInJail(t, projectDir, homeVol, "cat /workspace/escape")
	if strings.Contains(out, token) {
		t.Fatalf("a symlink inside the project directory reached a host path outside it:\n%s", out)
	}
}

// TestFilesystemJailContainerStateDoesNotPersist confirms that persistence
// is scoped exactly to the two mount points (workspace, home volume) and
// nowhere else in the container's filesystem — so nothing a compromised
// agent writes into the container's own rootfs can survive to the next
// session, for this project or any other.
func TestFilesystemJailContainerStateDoesNotPersist(t *testing.T) {
	requireDocker(t)

	projectDir := newProjectDir(t)
	homeVol := testVolumeName(t)

	runInJail(t, projectDir, homeVol,
		"echo canary > /root/outside-marker.txt && echo canary > /home/agent/persisted-marker.txt")

	out := runInJail(t, projectDir, homeVol,
		"cat /root/outside-marker.txt 2>&1; echo ---; cat /home/agent/persisted-marker.txt 2>&1")

	parts := strings.SplitN(out, "---", 2)
	if len(parts) != 2 {
		t.Fatalf("unexpected output shape:\n%s", out)
	}
	rootfsResult, volumeResult := parts[0], parts[1]

	if strings.Contains(rootfsResult, "canary") {
		t.Errorf("a write to the container's own rootfs (outside any mount) persisted to a new container:\n%s", rootfsResult)
	}
	if !strings.Contains(volumeResult, "canary") {
		t.Errorf("a write to the home volume did not persist to a new container using the same volume:\n%s", volumeResult)
	}
}

func mustSessionID(t *testing.T) string {
	t.Helper()
	id, err := newSessionID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
