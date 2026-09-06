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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"

	"github.com/jrivard/smith-jail/internal/proxyproto"
)

const sessionIDLen = 8

// NetworkJail is the per-session network sandbox: a plain, WAN-reachable
// Docker network that only two containers ever join — a proxy sidecar that
// owns the network namespace, and (via `--network container:<proxy>`) the
// agent container itself. A one-shot helper installs nft rules *inside that
// namespace* (never the host's) that redirect the namespace's outbound
// :80/:443/:53 traffic into the proxy, which decides — by consulting a live
// allow-list — whether to relay each connection or refuse it.
//
// This replaces smith-jail's earlier design, which wrote nft rules into the
// *host's* namespace and required a standing CAP_NET_ADMIN grant on the
// smith-jail binary itself. Nothing here holds that capability except the
// netsetup helper, briefly, for the one call that installs the rules — see
// runNetsetup.
type NetworkJail struct {
	sessionID          string
	networkID          string
	networkName        string
	proxyContainerID   string
	proxyContainerName string
	policyFilePath     string // temp host file, bind-mounted into the proxy; removed on Cleanup
	allowedHosts       []string
}

// newSessionID returns a random token that makes every jail's container and
// network names unique, so concurrent sessions never collide.
func newSessionID() (string, error) {
	buf := make([]byte, sessionIDLen/2)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating session id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// InvokeOptions holds per-invocation flags that affect network jail behaviour.
type InvokeOptions struct {
	NetworkJail     bool
	ExtraAllowHosts []string // from --allow flag
	AllowFile       string   // from --allow-file flag
	AutoApprove     bool     // from --yes/-y flag
}

// NewNetworkJail creates the session's proxy sidecar, network, and nft
// rules, and blocks until the proxy is confirmed listening.
func NewNetworkJail(agent *Agent, cfg *Config, opts *InvokeOptions, dir string) (*NetworkJail, error) {
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("--network-jail requires Linux (nftables + CAP_NET_ADMIN in an ephemeral helper) — not supported on %s", runtime.GOOS)
	}
	if err := requireNftRedirModule(); err != nil {
		return nil, err
	}

	sessionID, err := newSessionID()
	if err != nil {
		return nil, err
	}

	hosts, err := effectiveAllowedHosts(agent, cfg, opts)
	if err != nil {
		return nil, err
	}

	hash := projectHash(dir)
	j := &NetworkJail{
		sessionID:          sessionID,
		networkName:        agent.ContainerPrefix() + "-net-" + hash,
		proxyContainerName: agent.ContainerPrefix() + "-proxy-" + sessionID,
		allowedHosts:       hosts,
	}

	proxyTag, netsetupTag, err := EnsureProxyImages()
	if err != nil {
		return nil, fmt.Errorf("preparing network jail images: %w", err)
	}

	networkID, err := createDockerNetwork(j.networkName, dir, agent.Name)
	if err != nil {
		return nil, fmt.Errorf("creating Docker network: %w", err)
	}
	j.networkID = networkID

	policyPath, err := writePolicyFile(hosts)
	if err != nil {
		_ = j.Cleanup()
		return nil, fmt.Errorf("writing network policy: %w", err)
	}
	j.policyFilePath = policyPath

	eventLogPath, err := netLogPath(agent, dir)
	if err != nil {
		_ = j.Cleanup()
		return nil, fmt.Errorf("preparing network event log: %w", err)
	}
	if err := ensureFileExists(eventLogPath); err != nil {
		_ = j.Cleanup()
		return nil, fmt.Errorf("creating network event log %s: %w", eventLogPath, err)
	}

	printInfo(fmt.Sprintf("Starting network proxy (allowed hosts: %d)...", len(hosts)))
	proxyID, err := startProxyContainer(j.networkName, j.proxyContainerName, policyPath, eventLogPath, proxyTag, agent, dir)
	if err != nil {
		_ = j.Cleanup()
		return nil, fmt.Errorf("starting proxy sidecar: %w", err)
	}
	j.proxyContainerID = proxyID

	if err := waitProxyReady(j.proxyContainerName, 15*time.Second); err != nil {
		_ = j.Cleanup()
		return nil, fmt.Errorf("proxy sidecar: %w", err)
	}

	if err := runNetsetup(j.proxyContainerName, netsetupTag, nftRules()); err != nil {
		_ = j.Cleanup()
		return nil, fmt.Errorf("installing network jail rules: %w", err)
	}

	printOK(fmt.Sprintf("Network jail active — proxy: %s, allowed hosts: %d", j.proxyContainerName, len(hosts)))
	return j, nil
}

// effectiveAllowedHosts assembles the session's hostname allow-list: agent
// defaults + project config + this invocation's flags. hermes-local also
// implicitly allows its Ollama sidecar's container name, so the sidecar
// stays reachable by name even with the jail active — see
// proxyproto.UpstreamDNSDefault for why the proxy's DNS forwarder can
// resolve it at all.
func effectiveAllowedHosts(agent *Agent, cfg *Config, opts *InvokeOptions) ([]string, error) {
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

	if agent.Name == "hermes-local" {
		hosts = append(hosts, OllamaContainerName)
	}

	return hosts, nil
}

// NetworkArg returns the docker --network value the agent container (and
// nothing else) should be created with: joining the proxy's namespace
// entirely, rather than getting its own network attachment.
func (j *NetworkJail) NetworkArg() string {
	return "container:" + j.proxyContainerName
}

// NetworkName returns the actual underlying Docker network — needed by
// callers (the hermes-local Ollama sidecar) that must genuinely join a
// network rather than share a container's namespace via NetworkArg.
func (j *NetworkJail) NetworkName() string {
	return j.networkName
}

// Cleanup removes the proxy sidecar, its policy file, and the session
// network. The agent container removes itself (it's started with --rm); the
// netsetup helper already removed itself after installing the rules.
func (j *NetworkJail) Cleanup() error {
	var errs []string

	if j.proxyContainerID != "" || j.proxyContainerName != "" {
		if err := removeContainerByName(j.proxyContainerName); err != nil {
			errs = append(errs, fmt.Sprintf("removing proxy sidecar: %v", err))
		} else {
			printOK("Network proxy sidecar removed.")
		}
	}

	if j.policyFilePath != "" {
		_ = os.Remove(j.policyFilePath)
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

// ── nft rules ─────────────────────────────────────────────────────────────

// requireNftRedirModule fails fast — before any images are built or Docker
// resources are created — if the kernel's nft_redir module isn't loaded or
// built in. It can only auto-load from the host's own network namespace,
// not the isolated one runNetsetup's rules run inside, so on a host that
// hasn't loaded it since boot, the jail would otherwise fail deep into
// setup (after building images, creating the network, and starting the
// proxy sidecar) with a cryptic nft parse error instead of a clear message
// up front. See checkNftRedirModule (doctor.go) for the equivalent
// informational check.
//
// If the check itself is inconclusive (neither /proc/modules nor
// modules.builtin could be read), it doesn't block — an unusual host layout
// isn't grounds to refuse a run that might well succeed.
func requireNftRedirModule() error {
	loaded, loadedErr := kernelModuleLoaded("nft_redir")
	if loadedErr == nil && loaded {
		return nil
	}
	builtin, builtinErr := kernelModuleBuiltin("nft_redir")
	if builtinErr == nil && builtin {
		return nil
	}
	if loadedErr != nil && builtinErr != nil {
		return nil
	}

	return fmt.Errorf("kernel module nft_redir is not loaded\n\n" +
		"--network-jail's nftables rules run inside an isolated per-session network\n" +
		"namespace, and the kernel only auto-loads nft modules for requests made from\n" +
		"the host's own namespace — so this needs to be loaded once per boot before\n" +
		"the jail can start.\n\n" +
		"Run:\n" +
		"    sudo modprobe nft_redir\n")
}

// nftRules renders the ruleset the netsetup helper applies inside the
// shared network namespace. It's entirely static — no per-session bridge
// name or resolved IP set to bake in, unlike the host-nft design this
// replaces — because enforcement no longer depends on the identity of any
// interface, only on which local UID the traffic belongs to.
//
// Ordering matters: the nat/output hook (priority -100) runs before the
// filter/output hook (priority 0), so by the time the filter chain sees a
// redirected packet, its destination has already been rewritten to
// loopback. That's why the filter chain doesn't need to separately allow
// ports 80/443/53 — anything that reached them was redirected to loopback
// already, and everything else is refused by the default policy.
//
// The filter chain matches the redirected traffic by destination address
// (ip daddr 127.0.0.1), not by output interface (oif "lo"). They sound
// equivalent — a packet redirected to loopback should surely have oif
// "lo" — but they aren't: address rewriting is part of the NAT verdict
// itself and is visible immediately to every later rule in the same hook,
// while for a TCP SYN specifically, oif can still reflect the pre-redirect
// route (e.g. eth0) at the point the filter chain — same hook, later
// priority — evaluates it, so "oif lo accept" silently fails to match and
// the packet falls through to the default-drop policy even though it was
// genuinely redirected. UDP doesn't hit this (its oif does update in time),
// which is why DNS worked throughout while TCP silently died — traced by
// hand, painfully, since this drop happens before conntrack even confirms
// the entry, so there's nothing to see in "cat /proc/net/nf_conntrack" or a
// packet capture: it just looks like the SYN vanishes.
//
// Family is "ip", not "inet": the dual-stack "inet" family only grew NAT
// support (redirect/dnat/snat) in later kernels and its availability is
// inconsistent, while "ip" has had it since nftables' earliest releases. The
// jail's Docker network already disables IPv6 (see createDockerNetwork), so
// there's no dual-stack requirement to justify "inet"'s narrower kernel
// support here.
func nftRules() string {
	return fmt.Sprintf(`table ip smithjail {
  chain output {
    type nat hook output priority -100; policy accept;
    meta skuid %d return
    udp dport 53 redirect to :%s
    tcp dport 80 redirect to :%s
    tcp dport 443 redirect to :%s
  }

  chain block {
    type filter hook output priority 0; policy drop;
    meta skuid %d accept
    ip daddr 127.0.0.1 accept
    ct state established,related accept
  }
}
`, proxyproto.ProxyUID, proxyproto.DNSPort, proxyproto.HTTPPort, proxyproto.TLSPort, proxyproto.ProxyUID)
}

// ── policy file ───────────────────────────────────────────────────────────

// writePolicyFile writes the effective allow-list to a temp file, bind-mounted
// read-only into the proxy sidecar at proxyproto.PolicyFilePath.
func writePolicyFile(hosts []string) (string, error) {
	f, err := os.CreateTemp("", "smithjail-policy-*.json")
	if err != nil {
		return "", err
	}
	defer f.Close()

	// The proxy sidecar reads this bind-mounted file as UID 5353, not the
	// host user running smith-jail (often root), so it must be
	// world-readable — the default 0600 from CreateTemp leaves it
	// unreadable to the container.
	if err := f.Chmod(0o644); err != nil {
		os.Remove(f.Name())
		return "", err
	}

	cfg := proxyproto.PolicyConfig{AllowedHosts: hosts}
	if err := json.NewEncoder(f).Encode(cfg); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// ── Docker helpers ───────────────────────────────────────────────────────

// createDockerNetwork creates the session's bridge network: a plain,
// WAN-reachable network like an unjailed session would use — enforcement
// comes entirely from the nft rules inside the shared namespace, not from
// any network-level isolation, so there's nothing special about this
// network beyond being scoped to one session. IPv6 is disabled: the relay's
// SO_ORIGINAL_DST handling only covers IPv4 for now.
func createDockerNetwork(name, dir, agentName string) (networkID string, err error) {
	cli, err := dockerClient()
	if err != nil {
		return "", err
	}
	defer cli.Close()

	_ = removeDockerNetworkByName(cli, name)

	ipv6 := false
	resp, err := cli.NetworkCreate(context.Background(), name, network.CreateOptions{
		Driver:     "bridge",
		EnableIPv6: &ipv6,
		Labels: map[string]string{
			"smithjail.project": dir,
			"smithjail.agent":   agentName,
		},
	})
	if err != nil {
		return "", err
	}
	return resp.ID, nil
}

func removeDockerNetwork(networkID string) error {
	cli, err := dockerClient()
	if err != nil {
		return err
	}
	defer cli.Close()
	return cli.NetworkRemove(context.Background(), networkID)
}

func removeDockerNetworkByName(cli dockerNetworkRemover, name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return cli.NetworkRemove(ctx, name)
}

// dockerNetworkRemover is the one *client.Client method removeDockerNetworkByName
// needs, kept narrow so it doesn't need to import the client package just
// for a type name here.
type dockerNetworkRemover interface {
	NetworkRemove(ctx context.Context, network string) error
}

// startProxyContainer starts the session's proxy sidecar, detached, on
// networkName, with the policy file bind-mounted read-only.
func startProxyContainer(networkName, containerName, policyPath, eventLogPath, proxyTag string, agent *Agent, dir string) (containerID string, err error) {
	args := []string{
		"run", "-d",
		"--name", containerName,
		"--network", networkName,
		"--label", "smithjail.proxy=" + containerName,
		"--label", "smithjail.role=proxy",
		"--label", "smithjail.project=" + dir,
		"--label", "smithjail.agent=" + agent.Name,
		"--volume", labelledMount(policyPath, proxyproto.PolicyFilePath, mountLabel(), "ro"),
		"--volume", labelledMount(eventLogPath, proxyproto.EventLogPath, mountLabel()),
		"--env", proxyproto.EventLogEnv + "=" + proxyproto.EventLogPath,
		proxyTag,
	}
	out, err := exec.Command("docker", args...).Output()
	if err != nil {
		return "", dockerRunError(err)
	}
	return strings.TrimSpace(string(out)), nil
}

// waitProxyReady polls the proxy's logs for proxyproto.ReadyMarker so the
// netsetup helper never installs redirect rules before anything is
// listening for the traffic they redirect.
func waitProxyReady(containerName string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		out, _ := exec.Command("docker", "logs", containerName).CombinedOutput()
		if strings.Contains(string(out), proxyproto.ReadyMarker) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("proxy sidecar did not report ready within %s (last logs:\n%s)", timeout, out)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// runNetsetup runs the one-shot rule-installation helper: it joins the
// proxy's network namespace and nothing else's, is granted NET_ADMIN only
// for this single invocation, and is removed the moment it exits.
func runNetsetup(proxyContainerName, netsetupTag, rules string) error {
	cmd := exec.Command("docker", "run", "--rm", "-i",
		"--network", "container:"+proxyContainerName,
		"--cap-add", "NET_ADMIN",
		netsetupTag,
	)
	cmd.Stdin = strings.NewReader(rules)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "Could not process rule") && strings.Contains(string(out), "redirect") {
			return fmt.Errorf("%w\n%s\nThe kernel's nft_redir module is likely not loaded — it can only "+
				"auto-load from the host's own network namespace, not this rule's isolated one. "+
				"Run: sudo modprobe nft_redir (see `smith-jail doctor`)", err, out)
		}
		return fmt.Errorf("%w\n%s", err, out)
	}
	return nil
}

func removeContainerByName(name string) error {
	cli, err := dockerClient()
	if err != nil {
		return err
	}
	defer cli.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return cli.ContainerRemove(ctx, name, container.RemoveOptions{Force: true})
}

func dockerRunError(err error) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return fmt.Errorf("docker run failed: %s", strings.TrimSpace(string(exitErr.Stderr)))
	}
	return err
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
