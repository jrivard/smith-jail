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
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"

	"github.com/docker/docker/client"
)

// proxySource embeds everything static-dockerfile/proxy/Dockerfile's build needs to
// compile cmd/smithjail-proxy: the module's dependency manifest and the two
// packages that binary actually imports (not the rest of this repo's own
// "package main", which cmd/smithjail-proxy never depends on). Embedding it
// means smith-jail — distributed as a single static binary — never needs a
// source checkout on the machine it runs on to build its own sidecar image.
//
//go:embed go.mod go.sum
//go:embed static-dockerfile/proxy/Dockerfile
//go:embed cmd/smithjail-proxy
//go:embed internal/proxyproto
var proxySource embed.FS

// netsetupSource embeds the one-shot netns rule-setup helper's Dockerfile.
// It has no source of its own to compile (see static-dockerfile/netsetup/Dockerfile) —
// just nftables layered onto a small base image.
//
//go:embed static-dockerfile/netsetup/Dockerfile
var netsetupSource embed.FS

const (
	proxyImageRepo    = "smithjail-proxy"
	netsetupImageRepo = "smithjail-netsetup"
)

// proxyImageTag and netsetupImageTag are content-addressed from the
// embedded source, the same principle Agent.ImageRef uses for agent images:
// a smith-jail binary built from unchanged proxy source always resolves to
// the same tag (so unrelated sessions share one cached image), and any
// change to the proxy's own code yields a new tag automatically instead of
// silently running stale cached code.
func proxyImageTag() (string, error) {
	hash, err := hashEmbeddedFS(proxySource)
	if err != nil {
		return "", err
	}
	return proxyImageRepo + ":" + hash, nil
}

func netsetupImageTag() (string, error) {
	hash, err := hashEmbeddedFS(netsetupSource)
	if err != nil {
		return "", err
	}
	return netsetupImageRepo + ":" + hash, nil
}

func hashEmbeddedFS(fsys embed.FS) (string, error) {
	var paths []string
	if err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			paths = append(paths, path)
		}
		return nil
	}); err != nil {
		return "", err
	}
	sort.Strings(paths) // WalkDir is already lexical, but don't depend on it silently

	h := sha256.New()
	for _, p := range paths {
		data, err := fsys.ReadFile(p)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", p, len(data))
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil))[:12], nil
}

// localImageExists checks by exact ref, unlike ImageExists (docker.go) which
// is agent-config-specific.
func localImageExists(ref string) (bool, error) {
	cli, err := dockerClient()
	if err != nil {
		return false, err
	}
	defer cli.Close()

	_, _, err = cli.ImageInspectWithRaw(context.Background(), ref)
	if err != nil {
		if client.IsErrNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// materializeFS writes an embedded filesystem out to dir, preserving
// structure, so it can be handed to `docker build` as a real build context.
func materializeFS(fsys embed.FS, dir string) error {
	return fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(dir, path)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := fsys.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

// buildEmbeddedImage materializes fsys into a temp build context and runs
// `docker build`, the same shell-out BuildImage (docker.go) already uses
// for agent images.
func buildEmbeddedImage(fsys embed.FS, dockerfileRelPath, tag string) error {
	buildDir, err := os.MkdirTemp("", "smithjail-imgbuild-*")
	if err != nil {
		return fmt.Errorf("preparing build context: %w", err)
	}
	defer os.RemoveAll(buildDir)

	if err := materializeFS(fsys, buildDir); err != nil {
		return fmt.Errorf("writing build context: %w", err)
	}

	cmd := exec.Command("docker", "build",
		"-t", tag,
		"-f", filepath.Join(buildDir, dockerfileRelPath),
		buildDir,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker build failed: %w", err)
	}
	return nil
}

// EnsureProxyImages builds the proxy sidecar and netsetup helper images if
// they aren't already cached locally, and returns their tags. Both are
// content-addressed and never published — see proxyImageTag/netsetupImageTag.
func EnsureProxyImages() (proxyTag, netsetupTag string, err error) {
	proxyTag, err = proxyImageTag()
	if err != nil {
		return "", "", fmt.Errorf("hashing proxy source: %w", err)
	}
	netsetupTag, err = netsetupImageTag()
	if err != nil {
		return "", "", fmt.Errorf("hashing netsetup source: %w", err)
	}

	if exists, err := localImageExists(proxyTag); err != nil {
		return "", "", err
	} else if !exists {
		printInfo("Building network proxy image (first time or updated source)...")
		if err := buildEmbeddedImage(proxySource, "static-dockerfile/proxy/Dockerfile", proxyTag); err != nil {
			return "", "", fmt.Errorf("building %s: %w", proxyImageRepo, err)
		}
		printOK("Network proxy image built: " + proxyTag)
	}

	if exists, err := localImageExists(netsetupTag); err != nil {
		return "", "", err
	} else if !exists {
		printInfo("Building network setup helper image...")
		if err := buildEmbeddedImage(netsetupSource, "static-dockerfile/netsetup/Dockerfile", netsetupTag); err != nil {
			return "", "", fmt.Errorf("building %s: %w", netsetupImageRepo, err)
		}
		printOK("Network setup helper image built: " + netsetupTag)
	}

	return proxyTag, netsetupTag, nil
}
