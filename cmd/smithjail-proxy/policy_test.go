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
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestAllowsHostMatchesCaseAndTrailingDot(t *testing.T) {
	p, err := NewPolicy([]string{"api.anthropic.com"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{
		"api.anthropic.com",
		"api.anthropic.com.", // DNS wire-format queries end in a dot
		"API.ANTHROPIC.COM",
		"API.Anthropic.Com.",
	} {
		if !p.AllowsHost(name) {
			t.Errorf("AllowsHost(%q) = false, want true", name)
		}
	}

	if p.AllowsHost("evil.com") {
		t.Error("AllowsHost(\"evil.com\") = true, want false")
	}
}

func TestAllowsIPUnknownByDefault(t *testing.T) {
	p, err := NewPolicy([]string{"api.anthropic.com"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := p.AllowsIP(net.ParseIP("93.184.216.34")); ok {
		t.Error("an IP nothing resolved to and no CIDR covers should not be allowed")
	}
}

// This is the mechanism the DNS forwarder and TCP relay share: a resolved
// name becomes a live-allowed IP the relay can check without ever needing
// to sniff SNI or lie in DNS answers.
func TestLearnMakesIPAllowed(t *testing.T) {
	p, err := NewPolicy([]string{"api.anthropic.com"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	ip := net.ParseIP("160.79.104.10")
	p.Learn("api.anthropic.com.", ip)

	host, ok := p.AllowsIP(ip)
	if !ok {
		t.Fatal("AllowsIP should allow an IP just learned for an allowed host")
	}
	if host != "api.anthropic.com" {
		t.Errorf("host = %q, want %q", host, "api.anthropic.com")
	}
}

func TestAllowsIPStaticCIDR(t *testing.T) {
	p, err := NewPolicy(nil, []string{"140.82.112.0/20"})
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := p.AllowsIP(net.ParseIP("140.82.112.3")); !ok {
		t.Error("IP inside the allowed CIDR should be allowed")
	}
	if _, ok := p.AllowsIP(net.ParseIP("8.8.8.8")); ok {
		t.Error("IP outside every allowed CIDR should not be allowed")
	}
}

func TestNewPolicyRejectsBadCIDR(t *testing.T) {
	if _, err := NewPolicy(nil, []string{"not-a-cidr"}); err == nil {
		t.Fatal("expected an error for a malformed CIDR")
	}
}

func TestLoadPolicyFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.json")
	content := `{"allowedHosts":["api.anthropic.com","claude.ai"],"allowedCIDRs":["10.0.0.0/8"]}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	p, err := LoadPolicy(path)
	if err != nil {
		t.Fatal(err)
	}
	if !p.AllowsHost("claude.ai") {
		t.Error("claude.ai should be allowed from the loaded policy file")
	}
	if _, ok := p.AllowsIP(net.ParseIP("10.1.2.3")); !ok {
		t.Error("10.1.2.3 should be allowed via the loaded CIDR")
	}
}

func TestLoadPolicyMissingFile(t *testing.T) {
	if _, err := LoadPolicy("/nonexistent/policy.json"); err == nil {
		t.Fatal("expected an error for a missing policy file")
	}
}
