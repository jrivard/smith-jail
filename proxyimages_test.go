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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The whole point of content-addressed image tags is that they're stable
// across runs (so a cached image is actually reused) and change when the
// embedded source does (so a code change never silently runs stale cached
// behaviour).
func TestProxyImageTagIsStable(t *testing.T) {
	a, err := proxyImageTag()
	if err != nil {
		t.Fatal(err)
	}
	b, err := proxyImageTag()
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("proxyImageTag() is not stable: %q != %q", a, b)
	}
	if !strings.HasPrefix(a, proxyImageRepo+":") {
		t.Errorf("tag %q missing expected repo prefix %q", a, proxyImageRepo+":")
	}
}

func TestProxyAndNetsetupTagsDiffer(t *testing.T) {
	proxyTag, err := proxyImageTag()
	if err != nil {
		t.Fatal(err)
	}
	netsetupTag, err := netsetupImageTag()
	if err != nil {
		t.Fatal(err)
	}
	if proxyTag == netsetupTag {
		t.Errorf("proxy and netsetup images hashed to the same tag: %q", proxyTag)
	}
}

func TestMaterializeFSWritesExpectedFiles(t *testing.T) {
	dir := t.TempDir()
	if err := materializeFS(netsetupSource, dir); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "static-dockerfile", "netsetup", "Dockerfile")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("materialized Dockerfile not found: %v", err)
	}
	if !strings.Contains(string(data), "nftables") {
		t.Errorf("materialized netsetup Dockerfile looks wrong:\n%s", data)
	}
}

func TestMaterializeFSWritesProxySource(t *testing.T) {
	dir := t.TempDir()
	if err := materializeFS(proxySource, dir); err != nil {
		t.Fatal(err)
	}

	for _, rel := range []string{
		"go.mod",
		"go.sum",
		filepath.Join("cmd", "smithjail-proxy", "main.go"),
		filepath.Join("internal", "proxyproto", "proxyproto.go"),
	} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Errorf("expected materialized file %q: %v", rel, err)
		}
	}
}
