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

// Adversarial tests for the network jail's core claim: that a container
// joined to it can reach only what's on the allow-list, by name or by IP,
// regardless of which process or tool inside the container is asking.
//
// These need a working Docker daemon with internet access (the proxy and
// netsetup images are built from embedded source the first time this runs,
// which downloads a Go base image and compiles — expect the first
// invocation to take a couple of minutes; later runs reuse the cached,
// content-addressed image):
//
//	go test -tags=integration -run TestNetworkJail ./...
//
// Every Docker resource is randomly named ("smithjail-njail-test-...") and
// torn down in t.Cleanup, mirroring sandbox_filesystem_integration_test.go.

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// networkTestImage is deliberately not jailTestImage (debian:trixie-slim,
// used by the filesystem jail tests): these tests need wget and nslookup
// available out of the box to probe the network from inside the container,
// and busybox bundles both without an apt-get that would itself need to
// cross the jail to run.
const networkTestImage = "busybox:stable"

var (
	networkTestImageOnce sync.Once
	networkTestImageErr  error
)

// requireDockerForNetworkTest is requireDocker plus pulling networkTestImage
// instead of jailTestImage.
func requireDockerForNetworkTest(t *testing.T) {
	t.Helper()
	if err := CheckDocker(); err != nil {
		t.Skipf("Docker not available, skipping network jail test: %v", err)
	}
	networkTestImageOnce.Do(func() {
		if err := exec.Command("docker", "image", "inspect", networkTestImage).Run(); err == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), dockerRunTimeout)
		defer cancel()
		out, err := exec.CommandContext(ctx, "docker", "pull", networkTestImage).CombinedOutput()
		if err != nil {
			networkTestImageErr = fmt.Errorf("pulling %s: %w\n%s", networkTestImage, err, out)
		}
	})
	if networkTestImageErr != nil {
		t.Skipf("could not obtain test image, skipping network jail test: %v", networkTestImageErr)
	}
}

// startJailedTestContainer starts a throwaway, long-lived container joined
// to jail's proxy namespace, running as a shell that just idles — tests
// then `docker exec` probes into it, the same way a real interactive agent
// session would be probed via ExecShell.
func startJailedTestContainer(t *testing.T, jail *NetworkJail) string {
	t.Helper()

	id, err := newSessionID()
	if err != nil {
		t.Fatal(err)
	}
	name := "smithjail-njail-test-" + id
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", name).Run()
	})

	ctx, cancel := context.WithTimeout(context.Background(), dockerRunTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, "docker", "run", "-d",
		"--name", name,
		"--network", jail.NetworkArg(),
		networkTestImage,
		"sleep", "300",
	).CombinedOutput()
	if err != nil {
		t.Fatalf("starting jailed test container: %v\n%s", err, out)
	}
	return name
}

func execInJailedContainer(t *testing.T, containerName, shCmd string) (string, error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), dockerRunTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, "docker", "exec", containerName, "sh", "-c", shCmd).CombinedOutput()
	return string(out), err
}

// newTestJail starts a real proxy sidecar + netsetup helper allowing only
// allowedHost, and registers its teardown. It returns the project directory
// too, since that (plus the agent) is what netLogPath keys the persisted
// event log on.
func newTestJail(t *testing.T, allowedHost string) (*NetworkJail, string) {
	t.Helper()

	dir := newProjectDir(t)
	cfg := &Config{AutoApprove: true, NetworkAllowHosts: []string{allowedHost}}
	opts := &InvokeOptions{AutoApprove: true}

	jail, err := NewNetworkJail(AgentClaude, cfg, opts, dir)
	if err != nil {
		t.Fatalf("NewNetworkJail: %v", err)
	}
	t.Cleanup(func() { _ = jail.Cleanup() })
	return jail, dir
}

func TestNetworkJailAllowsListedHost(t *testing.T) {
	requireDockerForNetworkTest(t)

	jail, _ := newTestJail(t, "example.com")
	c := startJailedTestContainer(t, jail)

	out, err := execInJailedContainer(t, c, "wget -q -O- -T 10 http://example.com/ | head -c 15")
	if err != nil {
		t.Fatalf("expected the allowed host to be reachable, got error: %v\n%s", err, out)
	}
}

func TestNetworkJailBlocksUnlistedHostByName(t *testing.T) {
	requireDockerForNetworkTest(t)

	jail, _ := newTestJail(t, "example.com")
	c := startJailedTestContainer(t, jail)

	out, err := execInJailedContainer(t, c, "nslookup not-on-the-allowlist.invalid")
	if err == nil {
		t.Fatalf("expected DNS resolution of an unlisted host to fail, got:\n%s", out)
	}
}

func TestNetworkJailBlocksRawIPBypass(t *testing.T) {
	requireDockerForNetworkTest(t)

	jail, _ := newTestJail(t, "example.com")
	c := startJailedTestContainer(t, jail)

	// 1.1.1.1 is never on the allow-list and is dialled directly by IP,
	// bypassing DNS entirely — this is exactly the bypass a DNS-only
	// allow-list can't stop, and what SO_ORIGINAL_DST-based enforcement at
	// the TCP layer exists to close.
	out, err := execInJailedContainer(t, c, "wget -q -O- -T 8 http://1.1.1.1/")
	if err == nil {
		t.Fatalf("expected a direct connection to a disallowed raw IP to fail, got:\n%s", out)
	}
}

// The whole point of moving enforcement into the network namespace instead
// of the agent's own HTTP client is that *every* process in the container is
// covered, not just the agent binary — wget standing in for apt/npm/pip/git.
func TestNetworkJailCoversNonAgentTools(t *testing.T) {
	requireDockerForNetworkTest(t)

	jail, _ := newTestJail(t, "example.com")
	c := startJailedTestContainer(t, jail)

	allowed, err := execInJailedContainer(t, c, "wget -q -O- -T 10 http://example.com/ | head -c 15")
	if err != nil {
		t.Fatalf("wget to the allowed host should succeed: %v\n%s", err, allowed)
	}

	blocked, err := execInJailedContainer(t, c, "wget -q -O- -T 8 http://evil.invalid.example/")
	if err == nil {
		t.Fatalf("wget to an unlisted host should fail, got:\n%s", blocked)
	}
	if strings.TrimSpace(blocked) == strings.TrimSpace(allowed) {
		t.Fatal("blocked and allowed responses were identical — the probe itself is broken")
	}
}

// The persisted event log is the whole point of netlog/netview being able
// to show anything after a session ends — this proves it actually survives
// and reflects reality, not just that the proxy's stdout does (which
// disappears with the container at Cleanup).
func TestNetworkJailPersistsEventLog(t *testing.T) {
	requireDockerForNetworkTest(t)

	jail, dir := newTestJail(t, "example.com")
	c := startJailedTestContainer(t, jail)

	if _, err := execInJailedContainer(t, c, "wget -q -O- -T 10 http://example.com/"); err != nil {
		t.Fatalf("wget to the allowed host should succeed: %v", err)
	}
	if _, err := execInJailedContainer(t, c, "wget -q -O- -T 8 http://evil.invalid.example/"); err == nil {
		t.Fatal("wget to an unlisted host should fail")
	}

	path, err := netLogPath(AgentClaude, dir)
	if err != nil {
		t.Fatal(err)
	}

	// The proxy writes asynchronously per-connection; give it a moment
	// rather than racing the two wget calls above against the file.
	deadline := time.Now().Add(10 * time.Second)
	var haveAllowed, haveBlocked bool
	for time.Now().Before(deadline) && !(haveAllowed && haveBlocked) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		events, _ := tailEvents(ctx, path, false)
		for e := range events {
			if e.Action == "allowed" {
				haveAllowed = true
			}
			if e.Action == "blocked" {
				haveBlocked = true
			}
		}
		cancel()
		if !(haveAllowed && haveBlocked) {
			time.Sleep(300 * time.Millisecond)
		}
	}

	if !haveAllowed {
		t.Error("persisted event log never recorded an allowed connection")
	}
	if !haveBlocked {
		t.Error("persisted event log never recorded a blocked connection")
	}
}
