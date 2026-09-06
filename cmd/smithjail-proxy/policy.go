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
	"net"
	"os"
	"strings"
	"sync"

	"github.com/jrivard/smith-jail/internal/proxyproto"
)

// Policy is the live network allow-list the DNS forwarder and TCP relay both
// consult. Hostnames are checked as configured; every IP a DNS lookup
// resolves for an allowed hostname is learned into a live set, so the TCP
// relay's IP check stays accurate even when a CDN/API endpoint's address
// rotates mid-session — unlike smith-jail's old host-nft design, which
// resolved every allowed host once at jail startup and never revisited it.
type Policy struct {
	mu          sync.RWMutex
	hosts       map[string]bool
	staticCIDRs []*net.IPNet
	dynamicIPs  map[string]string // ip.String() -> hostname it was resolved for
}

// LoadPolicy reads a proxyproto.PolicyConfig JSON file from disk.
func LoadPolicy(path string) (*Policy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg proxyproto.PolicyConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return NewPolicy(cfg.AllowedHosts, cfg.AllowedCIDRs)
}

// NewPolicy builds a Policy from an explicit host and CIDR allow-list.
func NewPolicy(hosts, cidrs []string) (*Policy, error) {
	p := &Policy{
		hosts:      make(map[string]bool, len(hosts)),
		dynamicIPs: make(map[string]string),
	}
	for _, h := range hosts {
		if h = normalizeHost(h); h != "" {
			p.hosts[h] = true
		}
	}
	for _, c := range cidrs {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		_, ipnet, err := net.ParseCIDR(c)
		if err != nil {
			return nil, err
		}
		p.staticCIDRs = append(p.staticCIDRs, ipnet)
	}
	return p, nil
}

// normalizeHost makes DNS wire-format names ("api.anthropic.com.") and
// config-file names ("api.anthropic.com") compare equal, case-insensitively.
func normalizeHost(h string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), "."))
}

// AllowsHost reports whether name is on the allow-list.
func (p *Policy) AllowsHost(name string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.hosts[normalizeHost(name)]
}

// Learn records that host resolved to ip, so a later connection carrying
// only the IP (never the name — SO_ORIGINAL_DST has no hostname) can still
// be attributed and checked by AllowsIP.
func (p *Policy) Learn(host string, ip net.IP) {
	if ip == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dynamicIPs[ip.String()] = normalizeHost(host)
}

// AllowsIP reports whether ip may be relayed to, and the hostname it was
// last resolved for, if any is known. This is the TCP relay's independent
// backstop check: a connection can't bypass policy by skipping DNS (a
// hardcoded IP, an address cached from a previous run, a resolver that
// ignores the container's configured nameserver) because nothing routes
// anywhere except through this relay, and this relay checks every
// destination against either a live DNS-learned mapping or a static CIDR.
func (p *Policy) AllowsIP(ip net.IP) (host string, ok bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if host, ok := p.dynamicIPs[ip.String()]; ok {
		return host, true
	}
	for _, cidr := range p.staticCIDRs {
		if cidr.Contains(ip) {
			return "", true
		}
	}
	return "", false
}
