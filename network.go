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
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
)

const (
	// Linux caps interface names at 15 characters (IFNAMSIZ - 1), so the bridge
	// name has a tight budget: a 3-character prefix plus an 8-hex session ID
	// comes to 11, leaving headroom.
	bridgeIfacePrefix = "sj-"
	sessionIDLen      = 8

	// legacyBridgeIface is the fixed bridge name used before session IDs
	// existed. Recognised only so stale tables from those builds can be
	// identified; nothing creates it any more.
	legacyBridgeIface = "cj-restricted"

	nftTablePrefix = "smithjail-"
)

// nftSbinFallbacks are searched when "nft" isn't on $PATH. Package managers
// (including openSUSE's zypper) install nft under sbin, which isn't on a
// regular, non-root user's $PATH on most distros — so exec.LookPath alone
// reports "not found" even when nftables is installed and usable via sudo.
var nftSbinFallbacks = []string{"/usr/sbin/nft", "/sbin/nft", "/usr/local/sbin/nft"}

// findNft resolves the nft binary, checking $PATH first and then the usual
// sbin locations. Every nft invocation in this file goes through it instead
// of a bare "nft", which would silently re-fail the same PATH lookup.
func findNft() (string, error) {
	if path, err := exec.LookPath("nft"); err == nil {
		return path, nil
	}
	for _, candidate := range nftSbinFallbacks {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("nft not found in PATH or %s", strings.Join(nftSbinFallbacks, ", "))
}

// NetworkJail manages the per-session Docker network and nft firewall rules.
type NetworkJail struct {
	sessionID    string
	networkID    string
	networkName  string
	bridgeIface  string
	nftTableName string
	resolvedIPs  []string
}

// newSessionID returns a random token that makes every jail's kernel-level
// names unique. Without it the bridge and nft table names collide between
// concurrent sessions: two jails cannot share one bridge interface, and a
// shared nft table means the first session to exit tears down the firewall
// protecting the other.
func newSessionID() (string, error) {
	buf := make([]byte, sessionIDLen/2)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating session id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func bridgeIfaceName(sessionID string) string { return bridgeIfacePrefix + sessionID }

func nftTableName(agent *Agent, sessionID string) string {
	return nftTablePrefix + agent.Name + "-" + sessionID
}

// InvokeOptions holds per-invocation flags that affect network jail behaviour.
type InvokeOptions struct {
	NetworkJail     bool
	ExtraAllowHosts []string // from --allow flag
	AllowFile       string   // from --allow-file flag
	AutoApprove     bool     // from --yes/-y flag
}

// NewNetworkJail creates a Docker network and nft rules for the session.
func NewNetworkJail(agent *Agent, cfg *Config, opts *InvokeOptions, dir string) (*NetworkJail, error) {
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("--network-jail requires Linux (nftables + CAP_NET_ADMIN) — not supported on %s", runtime.GOOS)
	}

	if _, err := findNft(); err != nil {
		return nil, fmt.Errorf("nft not found — install nftables: sudo zypper install nftables")
	}

	if err := checkNetAdmin(); err != nil {
		return nil, fmt.Errorf("NET_ADMIN capability required for network jail\n"+
			"  Grant it once with: sudo setcap cap_net_admin+ep $(which smith-jail)\n"+
			"  Or run: sudo smith-jail %s run --network-jail ...", agent.Name)
	}

	sessionID, err := newSessionID()
	if err != nil {
		return nil, err
	}

	// Rules left by a session that died before its cleanup ran would otherwise
	// accumulate in the kernel ruleset forever.
	pruneStaleNftTables()

	hash := projectHash(dir)
	j := &NetworkJail{
		sessionID:    sessionID,
		networkName:  agent.ContainerPrefix() + "-net-" + hash,
		bridgeIface:  bridgeIfaceName(sessionID),
		nftTableName: nftTableName(agent, sessionID),
	}

	// Build the full allowed host list: agent defaults + config + flags
	hosts := append([]string{}, agent.DefaultAllowed...)
	hosts = append(hosts, cfg.NetworkAllowHosts...)
	hosts = append(hosts, opts.ExtraAllowHosts...)

	if opts.AllowFile != "" {
		extra, err := readAllowFile(opts.AllowFile)
		if err != nil {
			return nil, fmt.Errorf("reading allow file: %w", err)
		}
		hosts = append(hosts, extra...)
	}

	printInfo(fmt.Sprintf("Resolving %d allowed hosts...", len(hosts)))
	ips, err := resolveHosts(hosts)
	if err != nil {
		return nil, fmt.Errorf("resolving allowed hosts: %w", err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("no IPs resolved for allowed hosts — check DNS")
	}
	j.resolvedIPs = ips
	printInfo(fmt.Sprintf("Resolved %d IPs across %d hosts", len(ips), len(hosts)))

	if !confirm(fmt.Sprintf("create Docker network %q", j.networkName), cfg.AutoApprove) {
		return nil, errAborted
	}

	networkID, bridgeIface, err := createDockerNetwork(j.networkName, j.bridgeIface, dir, agent.Name)
	if err != nil {
		return nil, fmt.Errorf("creating Docker network: %w", err)
	}
	j.networkID = networkID
	// Trust what Docker actually created over what was requested.
	j.bridgeIface = bridgeIface

	if err := j.writeNftRules(); err != nil {
		_ = j.Cleanup()
		return nil, fmt.Errorf("writing nft rules: %w", err)
	}

	printOK(fmt.Sprintf("Network jail active — bridge: %s, allowed IPs: %d", bridgeIface, len(ips)))
	return j, nil
}

// DockerNetworkName returns the Docker network name to pass to --network.
func (j *NetworkJail) DockerNetworkName() string {
	return j.networkName
}

// Cleanup removes the nft table and Docker network.
func (j *NetworkJail) Cleanup() error {
	var errs []string

	if err := nftRun("delete", "table", "inet", j.nftTableName); err != nil {
		errs = append(errs, fmt.Sprintf("removing nft table: %v", err))
	} else {
		printOK("Network jail rules removed.")
	}

	if j.networkID != "" {
		if err := removeDockerNetwork(j.networkID); err != nil {
			errs = append(errs, fmt.Sprintf("removing Docker network: %v", err))
		} else {
			printOK("Docker network removed.")
		}
	}

	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// writeNftRules creates an nft table that allows traffic to the resolved IPs
// and drops everything else forwarded through the bridge.
func (j *NetworkJail) writeNftRules() error {
	tmpFile, err := os.CreateTemp("", "smith-jail-nft-*.rules")
	if err != nil {
		return err
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(j.nftRules()); err != nil {
		tmpFile.Close()
		return err
	}
	tmpFile.Close()

	nft, err := findNft()
	if err != nil {
		return err
	}
	cmd := exec.Command(nft, "-f", tmpFile.Name())
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// nftRules renders the ruleset for this session. Both the table name and the
// interface the rules match on are session-scoped, so concurrent jails filter
// independently and tear down independently.
func (j *NetworkJail) nftRules() string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("table inet %s {\n", j.nftTableName))
	sb.WriteString("  set allowed_ips {\n")
	sb.WriteString("    type ipv4_addr\n")
	sb.WriteString("    flags interval\n")
	sb.WriteString("    elements = {")
	sb.WriteString(strings.Join(j.resolvedIPs, ", "))
	sb.WriteString("}\n  }\n\n")

	sb.WriteString("  chain forward {\n")
	sb.WriteString("    type filter hook forward priority 0; policy accept;\n")
	sb.WriteString(fmt.Sprintf("    iifname \"%s\" ct state established,related accept\n", j.bridgeIface))
	sb.WriteString(fmt.Sprintf("    iifname \"%s\" ip daddr @allowed_ips accept\n", j.bridgeIface))
	sb.WriteString(fmt.Sprintf("    iifname \"%s\" drop\n", j.bridgeIface))
	sb.WriteString("  }\n")
	sb.WriteString("}\n")

	return sb.String()
}

// ── Docker network helpers ─────────────────────────────────────────────────

// createDockerNetwork creates the session's bridge network. The bridge
// interface name is supplied by the caller and must be unique per session: the
// kernel allows only one interface of a given name, so a fixed name limited the
// whole machine to a single jailed session at a time.
func createDockerNetwork(name, bridge, dir, agentName string) (networkID, bridgeIface string, err error) {
	cli, err := dockerClient()
	if err != nil {
		return "", "", err
	}
	defer cli.Close()

	_ = removeDockerNetworkByName(cli, name)

	resp, err := cli.NetworkCreate(context.Background(), name, network.CreateOptions{
		Driver: "bridge",
		Labels: map[string]string{
			"smithjail.project": dir,
			"smithjail.agent":   agentName,
		},
		Options: map[string]string{
			"com.docker.network.bridge.name": bridge,
		},
	})
	if err != nil {
		return "", "", err
	}

	nr, err := cli.NetworkInspect(context.Background(), resp.ID, network.InspectOptions{})
	if err != nil {
		return resp.ID, bridge, nil
	}

	iface := bridge
	if v, ok := nr.Options["com.docker.network.bridge.name"]; ok {
		iface = v
	}

	return resp.ID, iface, nil
}

func removeDockerNetwork(networkID string) error {
	cli, err := dockerClient()
	if err != nil {
		return err
	}
	defer cli.Close()
	return cli.NetworkRemove(context.Background(), networkID)
}

func removeDockerNetworkByName(cli *client.Client, name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return cli.NetworkRemove(ctx, name)
}

// ── nft helpers ───────────────────────────────────────────────────────────

func nftRun(args ...string) error {
	nft, err := findNft()
	if err != nil {
		return err
	}
	cmd := exec.Command(nft, args...)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// nftListTables returns the names of all inet tables in the ruleset.
func nftListTables() ([]string, error) {
	nft, err := findNft()
	if err != nil {
		return nil, err
	}
	out, err := exec.Command(nft, "list", "tables", "inet").Output()
	if err != nil {
		return nil, err
	}
	return parseNftTables(string(out)), nil
}

// parseNftTables extracts table names from "nft list tables inet" output.
func parseNftTables(out string) []string {
	var names []string
	for _, line := range strings.Split(out, "\n") {
		// Lines look like: table inet smithjail-claude-a1b2c3d4
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "table" && fields[1] == "inet" {
			names = append(names, fields[2])
		}
	}
	return names
}

// pruneStaleNftTables removes smith-jail firewall rules whose session is gone.
//
// A table is judged solely by whether the bridge interface its rules filter on
// still exists. That is the one signal that cannot produce a false positive: if
// the interface is present the session is live, and the table is left strictly
// alone. Deleting a live table would silently drop the firewall around a
// running agent, so ambiguity always resolves to "leave it".
func pruneStaleNftTables() {
	tables, err := nftListTables()
	if err != nil {
		return
	}

	for _, table := range staleNftTables(tables, ifaceExists) {
		if err := nftRun("delete", "table", "inet", table); err == nil {
			printInfo("Removed stale network jail rules: " + table)
		}
	}
}

// staleNftTables selects the tables that are ours and whose session is gone.
// Everything else — foreign tables, and ours whose bridge is still up — is
// omitted, so the caller can only ever delete rules nothing is relying on.
func staleNftTables(tables []string, exists func(string) bool) []string {
	var stale []string
	for _, table := range tables {
		iface, ok := bridgeForTable(table)
		if !ok {
			continue // not ours: never touch it
		}
		if exists(iface) {
			continue // bridge is up, so the session owning it is still live
		}
		stale = append(stale, table)
	}
	return stale
}

func ifaceExists(name string) bool {
	_, err := net.InterfaceByName(name)
	return err == nil
}

// bridgeForTable maps one of our nft table names back to the bridge interface
// its rules filter on. It reports false for anything not clearly ours.
func bridgeForTable(table string) (string, bool) {
	if !strings.HasPrefix(table, nftTablePrefix) {
		return "", false
	}

	suffix := table[strings.LastIndex(table, "-")+1:]
	if isSessionID(suffix) {
		return bridgeIfaceName(suffix), true
	}
	// Tables from builds predating session IDs were all named "<agent>-session"
	// and shared the one fixed bridge.
	if suffix == "session" {
		return legacyBridgeIface, true
	}
	return "", false
}

func isSessionID(s string) bool {
	if len(s) != sessionIDLen {
		return false
	}
	return strings.Trim(s, "0123456789abcdef") == ""
}

// ── IP resolution ─────────────────────────────────────────────────────────

func resolveHosts(hosts []string) ([]string, error) {
	seen := make(map[string]bool)
	var ips []string

	for _, host := range hosts {
		host = strings.TrimSpace(host)
		if host == "" {
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		addrs, err := net.DefaultResolver.LookupHost(ctx, host)
		cancel()

		if err != nil {
			printWarn(fmt.Sprintf("Cannot resolve %s: %v (skipping)", host, err))
			continue
		}

		for _, addr := range addrs {
			if ip := net.ParseIP(addr); ip != nil && ip.To4() != nil {
				if !seen[addr] {
					seen[addr] = true
					ips = append(ips, addr)
				}
			}
		}
	}

	return ips, nil
}

// ── Allow file ────────────────────────────────────────────────────────────

func readAllowFile(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var hosts []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		hosts = append(hosts, line)
	}
	return hosts, scanner.Err()
}

// ── Capability check ──────────────────────────────────────────────────────

func checkNetAdmin() error {
	nft, err := findNft()
	if err != nil {
		return err
	}
	cmd := exec.Command(nft, "list", "tables")
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("cannot run nft — missing NET_ADMIN capability or not root")
	}
	return nil
}
