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

// Package proxyproto holds the constants and on-disk shapes shared between
// smith-jail's host-side network jail (network.go) and the smithjail-proxy
// sidecar it starts (cmd/smithjail-proxy). They're pulled out here — rather
// than literals duplicated on each side — because host and sidecar are
// built as two separate "package main"s and can't otherwise share this
// without drifting apart silently.
package proxyproto

import "time"

const (
	// HTTPPort/TLSPort/DNSPort are the fixed, unprivileged local ports the
	// proxy listens on inside the shared network namespace. The netsetup
	// helper's nft rules redirect the namespace's real :80/:443/:53 traffic
	// to these — see NftRules.
	HTTPPort = "3080"
	TLSPort  = "3443"
	DNSPort  = "3053"

	// ProxyUID is the fixed, non-root UID the proxy image runs as (baked in
	// by static-dockerfile/proxy/Dockerfile). The nft ruleset exempts traffic owned by
	// this UID from redirection, so the proxy's own outbound relay/upstream
	// dials aren't redirected straight back into itself.
	ProxyUID = 5353

	// ReadyMarker is the line the proxy logs once every listener is bound.
	// The host polls container logs for it before running the netsetup
	// helper, so redirected traffic never arrives before anything is
	// listening for it.
	ReadyMarker = "smithjail-proxy: ready"

	// PolicyFileEnv names the env var pointing at the bind-mounted
	// PolicyConfig JSON file; PolicyFilePath is where the host mounts it.
	PolicyFileEnv  = "SMITHJAIL_POLICY_FILE"
	PolicyFilePath = "/etc/smithjail/policy.json"

	// UpstreamDNSEnv optionally overrides the resolver the proxy forwards
	// allowed DNS queries to. The default is Docker's own embedded resolver
	// (reachable at this fixed address from every container on a
	// user-defined network) rather than a public resolver directly: it
	// forwards ordinary internet queries to the host's real upstream same
	// as any resolver would, but it also resolves *other containers on the
	// same session network by name* — needed for hermes-local's Ollama
	// sidecar, which attaches to the jailed session network so the agent
	// can still reach it as "smithjail-ollama" (see ConnectOllamaToNetwork
	// in ollama.go) even with the network jail active.
	UpstreamDNSEnv     = "SMITHJAIL_UPSTREAM_DNS"
	UpstreamDNSDefault = "127.0.0.11:53"

	// EventLogEnv names the env var pointing at a bind-mounted file the
	// proxy should tee its activity log to, in addition to stdout.
	// EventLogPath is where the host mounts it. Unlike PolicyFilePath this
	// mount is read-write and, unlike the container itself, isn't removed
	// when the session ends — see netLogPath in netlog.go.
	EventLogEnv  = "SMITHJAIL_EVENT_LOG"
	EventLogPath = "/var/log/smithjail/events.jsonl"
)

// PolicyConfig is the allow-list the host writes to PolicyFilePath and the
// proxy reads at startup.
type PolicyConfig struct {
	AllowedHosts []string `json:"allowedHosts"`
	AllowedCIDRs []string `json:"allowedCIDRs"`
}

// Event is one line of the proxy's structured activity log — one per DNS
// query and one per TCP connection. Shared between the proxy (which writes
// it, see cmd/smithjail-proxy/log.go) and the host's netlog/netview
// commands (which read it back from the persisted file).
type Event struct {
	Time     time.Time `json:"time"`
	Kind     string    `json:"kind"`             // "dns" or "tcp"
	Action   string    `json:"action"`           // "allowed", "blocked", "error", ...
	Host     string    `json:"host,omitempty"`   // queried or resolved hostname, if known
	DestIP   string    `json:"destIP,omitempty"` // for "tcp" events
	DestPort int       `json:"destPort,omitempty"`
	Error    string    `json:"error,omitempty"`
}

// Blocked reports whether the event represents anything other than a clean
// allow — blocked, refused, or failed. netlog/netview's --blocked-only /
// "b" filter is exactly this.
func (e Event) Blocked() bool {
	return e.Action != "allowed"
}
