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

// linuxIfnameMax is IFNAMSIZ-1: the kernel rejects a longer interface name, and
// it would do so at container-start time rather than at build time.
const linuxIfnameMax = 15

// TestFindNftFallsBackToSbin guards against a regression that shipped once
// already: nftables packages (including openSUSE's zypper) install nft
// under an sbin directory, which is commonly absent from a regular user's
// $PATH even though root (and sudo) can run it fine. findNft must still
// locate it there instead of reporting "not found".
func TestFindNftFallsBackToSbin(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // a PATH with no nft on it

	fakeSbin := t.TempDir()
	fakeNft := filepath.Join(fakeSbin, "nft")
	if err := os.WriteFile(fakeNft, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}

	orig := nftSbinFallbacks
	nftSbinFallbacks = []string{fakeNft}
	defer func() { nftSbinFallbacks = orig }()

	path, err := findNft()
	if err != nil {
		t.Fatalf("findNft() error = %v, want it to find the sbin fallback", err)
	}
	if path != fakeNft {
		t.Errorf("findNft() = %q, want %q", path, fakeNft)
	}
}

func TestFindNftReportsMissingBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	orig := nftSbinFallbacks
	nftSbinFallbacks = []string{filepath.Join(t.TempDir(), "nft")}
	defer func() { nftSbinFallbacks = orig }()

	if _, err := findNft(); err == nil {
		t.Fatal("findNft() should fail when nft is nowhere to be found")
	}
}

func TestBridgeIfaceNameFitsKernelLimit(t *testing.T) {
	for i := 0; i < 100; i++ {
		id, err := newSessionID()
		if err != nil {
			t.Fatal(err)
		}
		if name := bridgeIfaceName(id); len(name) > linuxIfnameMax {
			t.Fatalf("bridge name %q is %d chars, kernel allows %d",
				name, len(name), linuxIfnameMax)
		}
	}
}

func TestSessionIDsAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id, err := newSessionID()
		if err != nil {
			t.Fatal(err)
		}
		if seen[id] {
			t.Fatalf("duplicate session id %q after %d draws", id, i)
		}
		if !isSessionID(id) {
			t.Fatalf("generated id %q is not recognised as a session id", id)
		}
		seen[id] = true
	}
}

// The fail-open this fixes: two concurrent sessions of one agent used to share
// a table name, so the first to exit deleted the firewall around the second.
func TestConcurrentSessionsGetDistinctKernelNames(t *testing.T) {
	a, err := newSessionID()
	if err != nil {
		t.Fatal(err)
	}
	b, err := newSessionID()
	if err != nil {
		t.Fatal(err)
	}

	if nftTableName(AgentClaude, a) == nftTableName(AgentClaude, b) {
		t.Error("two sessions of the same agent share an nft table name")
	}
	if bridgeIfaceName(a) == bridgeIfaceName(b) {
		t.Error("two sessions share a bridge interface name")
	}
}

func TestBridgeForTableMapsCurrentFormat(t *testing.T) {
	table := nftTableName(AgentClaude, "a1b2c3d4")

	iface, ok := bridgeForTable(table)
	if !ok {
		t.Fatalf("table %q not recognised as ours", table)
	}
	if want := bridgeIfaceName("a1b2c3d4"); iface != want {
		t.Errorf("iface = %q, want %q", iface, want)
	}
}

// Tables written by builds predating session IDs must still be recognised, so
// they can be cleaned up rather than lingering in the ruleset forever.
func TestBridgeForTableMapsLegacyFormat(t *testing.T) {
	iface, ok := bridgeForTable("smithjail-claude-session")
	if !ok {
		t.Fatal("legacy table not recognised")
	}
	if iface != legacyBridgeIface {
		t.Errorf("iface = %q, want %q", iface, legacyBridgeIface)
	}
}

// Anything not clearly ours must be refused. Pruning judges a table only by
// whether its interface exists, so a mis-mapped foreign table would be deleted
// the moment that interface happened to be absent.
func TestBridgeForTableRefusesForeignTables(t *testing.T) {
	for _, table := range []string{
		"filter",
		"nat",
		"inet",
		"my-smithjail-clone",
		"smithjail",                  // prefix alone, no suffix to map
		"smithjail-claude-notahexid", // wrong length
		"smithjail-claude-abcdefgh",  // right length, not hex
		"smithjail-claude-A1B2C3D4",  // uppercase is not what we emit
		"",
	} {
		if iface, ok := bridgeForTable(table); ok {
			t.Errorf("table %q was claimed as ours (mapped to %q)", table, iface)
		}
	}
}

func TestIsSessionIDRejectsNonHex(t *testing.T) {
	valid := []string{"00000000", "a1b2c3d4", "ffffffff"}
	invalid := []string{"", "a1b2c3d", "a1b2c3d45", "a1b2c3dg", "A1B2C3D4", "-1b2c3d4"}

	for _, s := range valid {
		if !isSessionID(s) {
			t.Errorf("isSessionID(%q) = false, want true", s)
		}
	}
	for _, s := range invalid {
		if isSessionID(s) {
			t.Errorf("isSessionID(%q) = true, want false", s)
		}
	}
}

func TestStaleNftTablesLeavesLiveSessionsAlone(t *testing.T) {
	live := nftTableName(AgentClaude, "aaaaaaaa")
	dead := nftTableName(AgentClaude, "bbbbbbbb")

	up := map[string]bool{bridgeIfaceName("aaaaaaaa"): true}
	exists := func(n string) bool { return up[n] }

	stale := staleNftTables([]string{live, dead}, exists)

	if len(stale) != 1 || stale[0] != dead {
		t.Fatalf("stale = %v, want exactly [%s]", stale, dead)
	}
}

// Deleting a foreign table would tear down firewall rules belonging to
// something else entirely on the user's machine.
func TestStaleNftTablesNeverTouchesForeignTables(t *testing.T) {
	tables := []string{"filter", "nat", "my-smithjail-clone", "smithjail-claude-zzzzzzzz"}
	none := func(string) bool { return false } // nothing is up

	if stale := staleNftTables(tables, none); len(stale) != 0 {
		t.Errorf("stale = %v, want none — no table here is ours", stale)
	}
}

// Upgrade safety: a session started by an older build is still running with the
// fixed bridge, and a newly started session must not delete its rules.
func TestStaleNftTablesSparesRunningLegacySession(t *testing.T) {
	legacy := "smithjail-claude-session"

	up := func(n string) bool { return n == legacyBridgeIface }
	if stale := staleNftTables([]string{legacy}, up); len(stale) != 0 {
		t.Errorf("stale = %v, want none — the legacy session is still live", stale)
	}

	down := func(string) bool { return false }
	if stale := staleNftTables([]string{legacy}, down); len(stale) != 1 {
		t.Errorf("stale = %v, want the abandoned legacy table to be collected", stale)
	}
}

// The rules a jail installs must name its own table and match only its own
// bridge. If either were shared, one session's teardown would drop another
// session's firewall.
func TestNftRulesAreSessionScoped(t *testing.T) {
	id := "a1b2c3d4"
	j := &NetworkJail{
		sessionID:    id,
		bridgeIface:  bridgeIfaceName(id),
		nftTableName: nftTableName(AgentClaude, id),
		resolvedIPs:  []string{"1.2.3.4", "5.6.7.8"},
	}

	rules := j.nftRules()

	if !strings.Contains(rules, "table inet "+nftTableName(AgentClaude, id)) {
		t.Errorf("rules do not declare the session's own table:\n%s", rules)
	}
	if !strings.Contains(rules, `iifname "`+bridgeIfaceName(id)+`"`) {
		t.Errorf("rules do not filter on the session's own bridge:\n%s", rules)
	}
	if strings.Contains(rules, legacyBridgeIface) {
		t.Errorf("rules still reference the shared legacy bridge:\n%s", rules)
	}
	for _, ip := range j.resolvedIPs {
		if !strings.Contains(rules, ip) {
			t.Errorf("allowed IP %s missing from rules:\n%s", ip, rules)
		}
	}
	// The catch-all drop is what makes this a jail rather than a suggestion.
	if !strings.Contains(rules, `iifname "`+bridgeIfaceName(id)+`" drop`) {
		t.Errorf("rules lack the default-drop for the session bridge:\n%s", rules)
	}
}

func TestParseNftTables(t *testing.T) {
	out := `table inet filter
table inet smithjail-claude-a1b2c3d4
table inet nat

garbage line
table ip6 something
`
	got := parseNftTables(out)
	want := []string{"filter", "smithjail-claude-a1b2c3d4", "nat"}

	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// The nft table name feeds straight into a shell-free exec, but it also lands in
// a generated rules file, so it must stay a plain identifier.
func TestNftTableNameIsPlainIdentifier(t *testing.T) {
	id, err := newSessionID()
	if err != nil {
		t.Fatal(err)
	}

	for _, agent := range AllAgents {
		name := nftTableName(agent, id)
		if strings.ContainsAny(name, " \t\n\"'{}();$`\\") {
			t.Errorf("table name %q contains characters that need quoting", name)
		}
		if !strings.HasPrefix(name, nftTablePrefix) {
			t.Errorf("table name %q lost its identifying prefix", name)
		}
		if _, ok := bridgeForTable(name); !ok {
			t.Errorf("table name %q does not round-trip through bridgeForTable", name)
		}
	}
}
