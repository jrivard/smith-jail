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
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jrivard/smith-jail/internal/proxyproto"
)

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
		seen[id] = true
	}
}

// The nft ruleset is what actually makes this a jail: every byte of it is
// applied inside the shared namespace by the netsetup helper, so a typo
// here silently means "nothing is redirected" or "the proxy redirects its
// own traffic into itself".
func TestNftRulesRedirectAndExemptProxyUID(t *testing.T) {
	rules := nftRules()

	uid := strconv.Itoa(proxyproto.ProxyUID)
	for _, want := range []string{
		"meta skuid " + uid + " return",
		"meta skuid " + uid + " accept",
		"udp dport 53 redirect to :" + proxyproto.DNSPort,
		"tcp dport 80 redirect to :" + proxyproto.HTTPPort,
		"tcp dport 443 redirect to :" + proxyproto.TLSPort,
		"policy drop",
		"ip daddr 127.0.0.1 accept",
	} {
		if !strings.Contains(rules, want) {
			t.Errorf("rules missing %q:\n%s", want, rules)
		}
	}

	// The filter chain must not depend on "oif lo" to recognize redirected
	// traffic — for a TCP SYN specifically, oif can still reflect the
	// pre-redirect route at the point this same-hook, later-priority chain
	// evaluates it, so that match silently fails to catch genuinely
	// redirected TCP connections (they fall through to the default-drop
	// policy) even though the identical UDP case works fine. See nftRules'
	// doc comment for how this was actually diagnosed.
	if strings.Contains(rules, `oif "lo"`) {
		t.Errorf("rules must match redirected traffic by ip daddr, not oif \"lo\" (silently drops TCP):\n%s", rules)
	}
}

func TestEffectiveAllowedHostsAssemblesAllSources(t *testing.T) {
	dir := t.TempDir()
	allowFile := filepath.Join(dir, "allow.txt")
	if err := os.WriteFile(allowFile, []byte("# comment\nfrom-file.example.com\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{NetworkAllowHosts: []string{"from-config.example.com"}}
	opts := &InvokeOptions{
		ExtraAllowHosts: []string{"from-flag.example.com"},
		AllowFile:       allowFile,
	}

	hosts, err := effectiveAllowedHosts(AgentClaude, cfg, opts)
	if err != nil {
		t.Fatal(err)
	}

	want := append([]string{}, AgentClaude.DefaultAllowed...)
	want = append(want, "from-config.example.com", "from-flag.example.com", "from-file.example.com")

	if len(hosts) != len(want) {
		t.Fatalf("hosts = %v, want %v", hosts, want)
	}
	for i := range want {
		if hosts[i] != want[i] {
			t.Errorf("hosts[%d] = %q, want %q", i, hosts[i], want[i])
		}
	}
}

// hermes-local's Ollama sidecar is reachable by container name only because
// its name is on the allow-list too — without this, the proxy's DNS
// forwarder would NXDOMAIN it like any other unlisted host, breaking the
// sidecar even though the network topology permits it.
func TestEffectiveAllowedHostsIncludesOllamaForHermesLocal(t *testing.T) {
	cfg := &Config{}
	opts := &InvokeOptions{}

	hosts, err := effectiveAllowedHosts(AgentHermesLocal, cfg, opts)
	if err != nil {
		t.Fatal(err)
	}

	found := false
	for _, h := range hosts {
		if h == OllamaContainerName {
			found = true
		}
	}
	if !found {
		t.Errorf("hosts %v does not include the Ollama sidecar's container name", hosts)
	}
}

func TestEffectiveAllowedHostsMissingAllowFile(t *testing.T) {
	cfg := &Config{}
	opts := &InvokeOptions{AllowFile: "/nonexistent/allow.txt"}

	if _, err := effectiveAllowedHosts(AgentClaude, cfg, opts); err == nil {
		t.Fatal("expected an error for a missing --allow-file")
	}
}

func TestWritePolicyFileRoundTrips(t *testing.T) {
	hosts := []string{"api.anthropic.com", "claude.ai"}
	path, err := writePolicyFile(hosts)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg proxyproto.PolicyConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.AllowedHosts) != len(hosts) {
		t.Fatalf("AllowedHosts = %v, want %v", cfg.AllowedHosts, hosts)
	}
	for i := range hosts {
		if cfg.AllowedHosts[i] != hosts[i] {
			t.Errorf("AllowedHosts[%d] = %q, want %q", i, cfg.AllowedHosts[i], hosts[i])
		}
	}
}

func TestNetworkJailNetworkArgJoinsProxyNamespace(t *testing.T) {
	j := &NetworkJail{proxyContainerName: "smithjail-claude-proxy-a1b2c3d4"}

	if got, want := j.NetworkArg(), "container:smithjail-claude-proxy-a1b2c3d4"; got != want {
		t.Errorf("NetworkArg() = %q, want %q", got, want)
	}
	if got, want := j.NetworkName(), ""; got != want {
		t.Errorf("NetworkName() = %q, want %q (unset in this test)", got, want)
	}
}
