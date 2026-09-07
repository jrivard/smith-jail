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
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// errAborted is returned when the user declines a confirmation prompt.
var errAborted = errors.New("aborted by user")

const helpText = `smith-jail — Docker jail launcher for AI coding agents

Usage:
  smith-jail claude       <command> [flags] [args]
  smith-jail gemini       <command> [flags] [args]
  smith-jail codex        <command> [flags] [args]
  smith-jail hermes       <command> [flags] [args]
  smith-jail hermes-local <command> [flags] [args]
  smith-jail ollama       <up|down|status|pull [model]>
  smith-jail show
  smith-jail doctor
  smith-jail tui
  smith-jail version
  smith-jail help

Running smith-jail with no arguments opens the interactive interface.

Commands (per agent):
  run        [flags] <dir>          Launch the agent in the given directory
  shell      [flags] <dir>          Open a shell in the container
  setup      [flags] [dir] [args]   Run the agent's own setup/login inside the container (default dir: cwd)
  netlog     [flags] [dir]          Print a project's network jail activity log (default dir: cwd)
  netview    [dir]                  Live TUI view of a project's network jail activity (default dir: cwd)
  dockerfile [dir]                  Print the generated Dockerfile for dir's configuration (default: cwd)
  rebuild    [--yes] [dir]          Rebuild the Docker image for dir's configuration (default: cwd)
  clean      [dir]                  Remove containers/volumes for a project, or all images/containers for this agent

Flags (netlog):
  --follow                Keep tailing new events (like tail -f)
  --blocked-only          Show only blocked/failed events

netlog and netview read a project's persisted network activity log — every
DNS query and TCP connection the network jail's proxy sidecar saw, allowed
or blocked — from disk, independent of whether a session is currently
running. Since smith-jail's own terminal is occupied for the whole run of
"run"/"shell", run these in a second terminal or tmux pane to watch a live
session, e.g.:
  smith-jail claude netview .
  smith-jail claude netlog --follow --blocked-only .

Flags (run, shell, and setup):
  --network-jail          Restrict outbound network to the agent's API only (Linux only)
  --allow host1,host2     Additional hosts to allow (comma or space separated)
  --allow-file <path>     File of additional allowed hosts, one per line
  --yes, -y               Auto-approve all creation prompts

Example — authenticate Hermes without installing it on the host:
  smith-jail hermes setup . --portal

hermes-local runs the same Hermes binary against a local Ollama model
instead of the cloud. It shares an always-on sidecar container
(smithjail-ollama, managed by the "ollama" command above) across every
project — "run"/"shell" offer to start it if it isn't already, and offer
to stop it on exit (default: leave it running).

Config:
  $XDG_CONFIG_HOME/smith-jail/smith-jail.env   — jail settings (limits, packages, network)
  $XDG_CONFIG_HOME/smith-jail/claude.env       — Claude credentials (CLAUDE_CONFIG_DIR)
  $XDG_CONFIG_HOME/smith-jail/gemini.env       — Gemini credentials (GEMINI_API_KEY)
  $XDG_CONFIG_HOME/smith-jail/codex.env        — Codex credentials (OPENAI_API_KEY)
  $XDG_CONFIG_HOME/smith-jail/hermes.env       — Hermes credentials (HERMES_CONFIG_DIR)
  $XDG_CONFIG_HOME/smith-jail/hermes-local.env — Hermes (local Ollama) credentials
  $XDG_CONFIG_HOME/smith-jail/ollama.env       — Ollama sidecar settings (image, GPU passthrough)
  $XDG_CONFIG_HOME/smith-jail/init.sh          — runs once at image build time
  $XDG_CONFIG_HOME/smith-jail/start.sh         — runs on every container start
  $XDG_CONFIG_HOME/smith-jail/allowed-hosts.txt — default network allow list
  (XDG_CONFIG_HOME defaults to ~/.config if unset)
  JAIL_AUTO_APPROVE=true                       — always skip confirmation prompts

Per-project overrides — any smith-jail.env setting (base image, packages,
limits, etc.) can be overridden for one project. Precedence, lowest to
highest:
  smith-jail.env (global)
  $XDG_CONFIG_HOME/smith-jail/projects/<hash>.env — private, per project directory
  process environment
  Edit it from the TUI's Settings/Packages screens (tab switches scope),
  or hand-edit the file directly. There is deliberately no project-directory
  config file smith-jail reads automatically — this stays machine-local so
  cloning a repo and running smith-jail in it can never grant itself root,
  sudo, or disable the network jail.
`

func main() {
	if len(os.Args) < 2 {
		// Interactive terminal: open the TUI. Otherwise (pipes, redirects, CI)
		// keep the original behaviour of printing help.
		if stdoutIsTTY() {
			cmdTUI()
			return
		}
		fmt.Print(helpText)
		os.Exit(0)
	}

	command := os.Args[1]

	switch command {
	case "help", "--help", "-h":
		fmt.Print(helpText)

	case "tui":
		cmdTUI()

	case "claude":
		cmdAgent(AgentClaude, os.Args[2:])

	case "gemini":
		cmdAgent(AgentGemini, os.Args[2:])

	case "codex":
		cmdAgent(AgentCodex, os.Args[2:])

	case "hermes":
		cmdAgent(AgentHermes, os.Args[2:])

	case "hermes-local":
		cmdAgent(AgentHermesLocal, os.Args[2:])

	case "ollama":
		cmdOllama(os.Args[2:])

	case "show":
		cmdShow()

	case "doctor":
		cmdDoctor()

	case "version", "--version", "-v":
		cmdVersion()

	case "run", "shell", "setup", "dockerfile", "rebuild", "clean":
		die(fmt.Sprintf("'%s' needs an agent: smith-jail <claude|gemini|codex|hermes|hermes-local> %s [args]", command, command))

	default:
		die(fmt.Sprintf("Unknown command: %s\n  Run: smith-jail help", command))
	}
}

// cmdAgent dispatches agent-specific subcommands.
func cmdAgent(agent *Agent, args []string) {
	if len(args) == 0 {
		fmt.Print(helpText)
		os.Exit(0)
	}

	switch args[0] {
	case "run":
		opts, dir, extraArgs := parseRunFlags(args[1:], agent.Name+" run")
		cmdRun(agent, dir, opts, extraArgs)

	case "shell":
		opts, dir, extraArgs := parseRunFlags(args[1:], agent.Name+" shell")
		cmdShell(agent, dir, opts, extraArgs)

	case "setup":
		opts, dir, extraArgs := parseSetupFlags(args[1:], agent.Name+" setup")
		cmdSetup(agent, dir, opts, extraArgs)

	case "netlog":
		follow, blockedOnly, dir := parseNetLogFlags(args[1:], agent.Name+" netlog")
		cmdNetLog(agent, dir, follow, blockedOnly)

	case "netview":
		dir := "."
		if len(args) > 1 {
			dir = args[1]
		}
		cmdNetView(agent, dir)

	case "dockerfile":
		cmdDockerfile(agent, args[1:])

	case "rebuild":
		cmdRebuild(agent, args[1:])

	case "clean":
		if len(args) > 1 {
			cmdCleanProject(agent, args[1])
		} else {
			cmdCleanAgent(agent)
		}

	default:
		die(fmt.Sprintf("Unknown %s command: %s\n  Run: smith-jail help", agent.Name, args[0]))
	}
}

// reorderKnownFlags moves any occurrence of a flag defined on fs — along
// with its value, wherever it appears in args — to the front, preserving
// the relative order of everything else. This makes fs.Parse order-
// independent: Go's flag package otherwise stops parsing at the first
// argument that doesn't look like a flag, so e.g. "run <dir> --network-jail"
// would leave --network-jail in the leftover positional args instead of
// being recognized, and it would silently pass through to the wrapped
// agent's own CLI instead of configuring smith-jail. A bare "--" ends the
// scan and everything from it onward (including the "--" itself) is left
// untouched, so it still works as an escape hatch for literally passing a
// smith-jail-flag-shaped argument through to the agent.
func reorderKnownFlags(fs *flag.FlagSet, args []string) []string {
	var known, rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			rest = append(rest, args[i:]...)
			break
		}

		name := ""
		switch {
		case strings.HasPrefix(a, "--"):
			name = a[2:]
		case strings.HasPrefix(a, "-") && len(a) > 1:
			name = a[1:]
		default:
			rest = append(rest, a)
			continue
		}
		hasValue := false
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			name = name[:eq]
			hasValue = true
		}

		f := fs.Lookup(name)
		if f == nil {
			rest = append(rest, a)
			continue
		}
		known = append(known, a)

		isBool := false
		if b, ok := f.Value.(interface{ IsBoolFlag() bool }); ok {
			isBool = b.IsBoolFlag()
		}
		if !hasValue && !isBool && i+1 < len(args) {
			i++
			known = append(known, args[i])
		}
	}
	return append(known, rest...)
}

// parseCommonFlags parses the flags shared by run, shell, and setup, and
// returns whatever positional arguments are left for the caller to interpret.
func parseCommonFlags(args []string, cmd string) (*InvokeOptions, []string) {
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)

	networkJail := fs.Bool("network-jail", false, "Restrict outbound network to agent API only (Linux only)")
	allowHosts := fs.String("allow", "", "Comma/space-separated list of additional allowed hosts")
	allowFile := fs.String("allow-file", "", "File of additional allowed hosts (one per line)")
	yes := fs.Bool("yes", false, "Auto-approve all creation prompts")
	fs.BoolVar(yes, "y", false, "Auto-approve all creation prompts (short form)")

	// Don't call os.Exit here: flag.ExitOnError already does, with the right
	// code — 0 for -h/--help, 2 for a genuine parse error. Exiting 0
	// unconditionally from here would mask real usage mistakes from scripts
	// checking the exit status.
	fs.Usage = func() {
		fmt.Print(helpText)
	}
	_ = fs.Parse(reorderKnownFlags(fs, args))

	opts := &InvokeOptions{
		NetworkJail: *networkJail,
		AllowFile:   *allowFile,
		AutoApprove: *yes,
	}

	if *allowHosts != "" {
		raw := strings.ReplaceAll(*allowHosts, ",", " ")
		for _, h := range strings.Fields(raw) {
			opts.ExtraAllowHosts = append(opts.ExtraAllowHosts, h)
		}
	}

	return opts, fs.Args()
}

// parseRunFlags parses flags common to run and shell. The directory is
// required — these commands operate on project files.
func parseRunFlags(args []string, cmd string) (*InvokeOptions, string, []string) {
	opts, remaining := parseCommonFlags(args, cmd)
	if len(remaining) == 0 {
		die(fmt.Sprintf("Usage: smith-jail %s [flags] <directory>", cmd))
	}
	return opts, remaining[0], remaining[1:]
}

// parseSetupFlags parses flags for the setup command. Unlike run/shell, the
// directory is optional and defaults to cwd: setup doesn't touch project
// files, it just needs a project's effective config to pick the right image
// and network settings. Anything after the directory is passed through to
// the agent's own "setup" subcommand (e.g. "--portal").
func parseSetupFlags(args []string, cmd string) (*InvokeOptions, string, []string) {
	opts, remaining := parseCommonFlags(args, cmd)
	if len(remaining) == 0 {
		return opts, ".", nil
	}
	return opts, remaining[0], remaining[1:]
}

// ── Commands ──────────────────────────────────────────────────────────────────

// resolveSession resolves the project directory, loads its effective config,
// merges in this invocation's flags, and verifies Docker is reachable. Shared
// by cmdRun and cmdShell. On any failure it calls die() and never returns.
func resolveSession(agent *Agent, rawDir string, opts *InvokeOptions) (dir string, cfg *Config) {
	dir, err := filepath.Abs(rawDir)
	if err != nil {
		die("Cannot resolve path: " + err.Error())
	}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		die("Directory does not exist: " + dir)
	}

	cfg = mustLoadConfig(dir, agent)
	cfg.AutoApprove = cfg.AutoApprove || opts.AutoApprove

	if opts.NetworkJail {
		cfg.NetworkJailEnabled = true
	}
	if opts.AllowFile == "" && cfg.NetworkAllowFile != "" {
		opts.AllowFile = cfg.NetworkAllowFile
	}

	if err := CheckDocker(); err != nil {
		die(err.Error())
	}
	return dir, cfg
}

// prepareContainer builds the image (if needed) and stands up the network
// jail (if enabled). Shared by cmdRun and cmdShell. cleanup is always safe to
// defer, even when jail setup was skipped or aborted. ok is false if the user
// declined a confirmation prompt — "Aborted." has already been printed, and
// the caller should return without doing anything further. Any other failure
// calls die() and never returns.
func prepareContainer(agent *Agent, cfg *Config, opts *InvokeOptions, dir string) (jail *NetworkJail, cleanup func(), ok bool) {
	cleanup = func() {}

	if err := EnsureImage(agent, cfg); err != nil {
		if errors.Is(err, errAborted) {
			printInfo("Aborted.")
			return nil, cleanup, false
		}
		die("Image build failed: " + err.Error())
	}

	if cfg.NetworkJailEnabled {
		j, err := NewNetworkJail(agent, cfg, opts, dir)
		if err != nil {
			if errors.Is(err, errAborted) {
				printInfo("Aborted.")
				return nil, cleanup, false
			}
			die("Network jail setup failed: " + err.Error())
		}
		jail = j
		cleanup = func() {
			if err := jail.Cleanup(); err != nil {
				printWarn("Network jail cleanup error: " + err.Error())
			}
		}
	}

	return jail, cleanup, true
}

// networkArgFor returns the docker --network value for the agent container:
// when the jail is active, that means joining the proxy sidecar's namespace
// entirely (see NetworkJail.NetworkArg) rather than getting its own network
// attachment. Otherwise it's the default bridge — except for hermes-local,
// which uses its own persistent network instead of the default bridge so it
// can reach the Ollama sidecar by container name (the default "bridge"
// network has no embedded DNS). See ollama.go.
func networkArgFor(agent *Agent, jail *NetworkJail) string {
	if jail != nil {
		return jail.NetworkArg()
	}
	if agent.Name == "hermes-local" {
		return OllamaNetworkName
	}
	return "bridge"
}

// prepareOllamaSidecar is the hermes-local-only hook run after the agent's
// own container/network setup: it ensures the Ollama sidecar is running
// (prompting to start it if not), pulls cfg.OllamaModel into it if missing,
// seeds ~/.hermes-local/config.yaml to point at it so no manual "hermes
// model" step is needed, and attaches the sidecar to whatever network this
// session is actually using, so it's reachable by name regardless of
// whether --network-jail is in play. Returns false if the user declined any
// of these steps — the caller should abort without launching the agent. A
// no-op for every other agent.
func prepareOllamaSidecar(agent *Agent, cfg *Config, jail *NetworkJail) bool {
	if agent.Name != "hermes-local" {
		return true
	}
	if err := EnsureOllamaRunning(cfg); err != nil {
		if errors.Is(err, errAborted) {
			printInfo("Aborted.")
			return false
		}
		die("Ollama sidecar setup failed: " + err.Error())
	}
	if err := EnsureOllamaModelPulled(cfg, cfg.OllamaModel); err != nil {
		if errors.Is(err, errAborted) {
			printInfo("Aborted.")
			return false
		}
		die("Pulling Ollama model failed: " + err.Error())
	}
	if err := SeedHermesLocalConfig(cfg); err != nil {
		printWarn("Could not seed hermes-local config.yaml: " + err.Error())
	}
	// Unlike networkArgFor (which the agent container uses to *join* the
	// proxy's namespace via "container:<proxy>"), Ollama needs the real
	// underlying network name — it's a genuinely separate container, not
	// something that can share another container's namespace.
	ollamaNet := OllamaNetworkName
	if jail != nil {
		ollamaNet = jail.NetworkName()
	}
	if err := ConnectOllamaToNetwork(ollamaNet); err != nil {
		printWarn("Could not attach Ollama sidecar to session network: " + err.Error())
	}
	return true
}

// runAndSummarize wires up signal handling around run — which should perform
// the actual docker invocation and block until it completes — then prints
// the exit summary. Shared by cmdRun and cmdShell.
func runAndSummarize(agent *Agent, dir string, cfg *Config, run func() (int, error)) {
	startTime := time.Now()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGHUP)

	exitCode, err := run()
	if errors.Is(err, errAborted) {
		printInfo("Aborted.")
		return
	}

	signal.Stop(sigCh)
	var sig os.Signal
	select {
	case sig = <-sigCh:
	default:
	}

	printExitSummary(agent, dir, startTime, sig, exitCode, cfg)
}

func cmdRun(agent *Agent, rawDir string, opts *InvokeOptions, extraArgs []string) {
	dir, cfg := resolveSession(agent, rawDir, opts)

	if !doctorGate(cfg) {
		printInfo("Aborted.")
		return
	}

	plan, err := buildRunPlan(agent, cfg, opts, dir, true)
	if err != nil {
		die("Building run plan: " + err.Error())
	}
	if !confirmRunPlan(plan) {
		printInfo("Aborted.")
		return
	}

	jail, cleanup, ok := prepareContainer(agent, cfg, opts, dir)
	defer cleanup()
	if !ok {
		return
	}
	if !prepareOllamaSidecar(agent, cfg, jail) {
		return
	}

	// Session banner
	fmt.Println()
	printHeader("Smith Jail", colorBlue)
	fmt.Println()
	fmt.Printf("  %sProject :%s %s%s%s\n", colorBold, colorReset, colorCyan, dir, colorReset)
	fmt.Printf("  %sAgent   :%s %s\n", colorBold, colorReset, agent.DisplayName)
	fmt.Printf("  %sBase    :%s %s\n", colorBold, colorReset, cfg.BaseImage)
	fmt.Printf("  %sAuth    :%s %s\n", colorBold, colorReset, agentAuthDesc(agent, cfg))
	if cfg.SkipPermissions {
		fmt.Printf("  %sPerms   :%s %sskip (autonomous mode)%s\n", colorBold, colorReset, colorYellow, colorReset)
	} else {
		fmt.Printf("  %sPerms   :%s prompt on each action\n", colorBold, colorReset)
	}
	if cfg.RunAsRoot {
		fmt.Printf("  %sUser    :%s %sroot%s\n", colorBold, colorReset, colorYellow, colorReset)
	} else {
		fmt.Printf("  %sUser    :%s agent (unprivileged)\n", colorBold, colorReset)
	}
	if cfg.NetworkJailEnabled {
		fmt.Printf("  %sNetwork :%s %sjailed (agent API only)%s\n", colorBold, colorReset, colorYellow, colorReset)
	} else {
		fmt.Printf("  %sNetwork :%s bridge (unrestricted)\n", colorBold, colorReset)
	}
	fmt.Printf("  %sMemory  :%s %s\n", colorBold, colorReset, cfg.MemLimit)
	fmt.Printf("  %sCPU     :%s %s\n", colorBold, colorReset, cfg.CPULimit)
	fmt.Printf("  %s  /help for commands · Ctrl-C or /exit to quit%s\n", colorDim, colorReset)
	fmt.Println()

	CheckForUpdate(agent, cfg)

	networkArg := networkArgFor(agent, jail)
	runAndSummarize(agent, dir, cfg, func() (int, error) {
		return RunAgent(agent, cfg, dir, networkArg, extraArgs)
	})

	if agent.Name == "hermes-local" {
		MaybeStopOllama(cfg)
	}
}

// printShellBanner explains what the interactive shell about to start is —
// attaching to a container already running, or a fresh one just for this
// shell — and how to leave it, since nothing else prints once bash takes
// over the terminal.
func printShellBanner(agent *Agent, cfg *Config, dir string, attaching bool) {
	clearScreen()
	printHeader("Smith Jail — Shell", colorBlue)
	fmt.Println()
	fmt.Printf("  %sProject :%s %s%s%s\n", colorBold, colorReset, colorCyan, dir, colorReset)
	fmt.Printf("  %sAgent   :%s %s\n", colorBold, colorReset, agent.DisplayName)
	fmt.Printf("  %sBase    :%s %s\n", colorBold, colorReset, cfg.BaseImage)
	if attaching {
		fmt.Printf("  %sMode    :%s attaching to the agent's already-running container\n", colorBold, colorReset)
	} else {
		fmt.Printf("  %sMode    :%s starting a fresh container for this project\n", colorBold, colorReset)
	}
	fmt.Println()
	fmt.Println("  You're getting an interactive bash shell inside the container —")
	fmt.Println("  the same filesystem and network the agent itself runs in.")
	fmt.Println()
	fmt.Printf("  %sType 'exit' or press Ctrl-D to leave.%s Smith-jail exits as soon\n", colorBold, colorReset)
	fmt.Println("  as the shell does, returning you to your regular terminal.")
	fmt.Println()
}

func cmdShell(agent *Agent, rawDir string, opts *InvokeOptions, extraArgs []string) {
	dir, cfg := resolveSession(agent, rawDir, opts)

	hash := projectHash(dir)

	if running := FindRunningContainer(agent, hash); running != "" {
		printShellBanner(agent, cfg, dir, true)
		if err := ExecShell(running); err != nil {
			die(err.Error())
		}
		return
	}

	fmt.Println()
	printWarn("No running container found for: " + dir)

	plan, err := buildRunPlan(agent, cfg, opts, dir, true)
	if err != nil {
		die("Building run plan: " + err.Error())
	}
	if !confirmRunPlan(plan) {
		printInfo("Aborted.")
		return
	}

	jail, cleanup, ok := prepareContainer(agent, cfg, opts, dir)
	defer cleanup()
	if !ok {
		return
	}
	if !prepareOllamaSidecar(agent, cfg, jail) {
		return
	}

	networkArg := networkArgFor(agent, jail)

	printShellBanner(agent, cfg, dir, false)
	runAndSummarize(agent, dir, cfg, func() (int, error) {
		return RunShell(agent, cfg, dir, networkArg, extraArgs)
	})

	if agent.Name == "hermes-local" {
		MaybeStopOllama(cfg)
	}
}

// cmdSetup runs the agent binary's own "setup" subcommand inside a
// throwaway container — e.g. "hermes setup --portal" — so credential
// login/OAuth happens without ever installing the agent on the host. It
// uses the project's normal network mode (jailed or bridge), same as
// run/shell; if an agent's login flow needs a browser loopback callback
// that jailed networking can't reach, rerun with --allow added for
// whatever host that flow calls out to.
func cmdSetup(agent *Agent, rawDir string, opts *InvokeOptions, extraArgs []string) {
	dir, cfg := resolveSession(agent, rawDir, opts)

	plan, err := buildRunPlan(agent, cfg, opts, dir, false)
	if err != nil {
		die("Building run plan: " + err.Error())
	}
	if !confirmRunPlan(plan) {
		printInfo("Aborted.")
		return
	}

	jail, cleanup, ok := prepareContainer(agent, cfg, opts, dir)
	defer cleanup()
	if !ok {
		return
	}

	networkArg := networkArgFor(agent, jail)

	fmt.Println()
	printHeader("Smith Jail — Setup", colorBlue)
	fmt.Println()
	fmt.Printf("  %sAgent   :%s %s\n", colorBold, colorReset, agent.DisplayName)
	fmt.Printf("  %sCommand :%s %s setup %s\n", colorBold, colorReset, agent.BinaryName, strings.Join(extraArgs, " "))
	fmt.Println()

	runAndSummarize(agent, dir, cfg, func() (int, error) {
		return RunSetup(agent, cfg, dir, networkArg, extraArgs)
	})
}

// cmdDockerfile prints the Dockerfile that would be used to build dir's
// effective image, without building it — the same build context prep as
// "rebuild", minus the docker build/confirm step.
func cmdDockerfile(agent *Agent, args []string) {
	fs := flag.NewFlagSet("dockerfile", flag.ExitOnError)
	fs.Usage = func() { fmt.Print(helpText) }
	_ = fs.Parse(args)

	rawDir := "."
	if remaining := fs.Args(); len(remaining) > 0 {
		rawDir = remaining[0]
	}
	dir, err := filepath.Abs(rawDir)
	if err != nil {
		die("Cannot resolve path: " + err.Error())
	}

	cfg := mustLoadConfig(dir, agent)

	df, err := cfg.GeneratedDockerfile(agent)
	if err != nil {
		die("Generating Dockerfile: " + err.Error())
	}

	fmt.Printf("# %s\n# dir: %s\n\n", agent.ImageRef(cfg), dir)
	fmt.Print(df)
}

func cmdRebuild(agent *Agent, args []string) {
	fs := flag.NewFlagSet("rebuild", flag.ExitOnError)
	yes := fs.Bool("yes", false, "Auto-approve all creation prompts")
	fs.BoolVar(yes, "y", false, "Auto-approve all creation prompts (short form)")
	fs.Usage = func() { fmt.Print(helpText) }
	_ = fs.Parse(args)

	// Rebuild targets one project's effective configuration; default to the
	// current directory like run/shell do.
	rawDir := "."
	if remaining := fs.Args(); len(remaining) > 0 {
		rawDir = remaining[0]
	}
	dir, err := filepath.Abs(rawDir)
	if err != nil {
		die("Cannot resolve path: " + err.Error())
	}

	cfg := mustLoadConfig(dir, agent)
	cfg.AutoApprove = cfg.AutoApprove || *yes

	if err := CheckDocker(); err != nil {
		die(err.Error())
	}

	var desc string
	if _, err := os.Stat(cfg.InitScript); err == nil {
		desc = fmt.Sprintf("rebuild Docker image %q (init.sh present, packages: %s)", agent.ImageRef(cfg), cfg.InitPackages)
	} else if cfg.InitPackages != "" {
		desc = fmt.Sprintf("rebuild Docker image %q (packages: %s)", agent.ImageRef(cfg), cfg.InitPackages)
	} else {
		desc = fmt.Sprintf("rebuild Docker image %q", agent.ImageRef(cfg))
	}

	if !confirm(desc, cfg.AutoApprove) {
		printInfo("Aborted.")
		return
	}

	printInfo("Removing old image...")
	RemoveImage(agent, cfg)

	if err := BuildImage(agent, cfg, true); err != nil {
		die("Build failed: " + err.Error())
	}
	printOK("Done.")
}

func cmdCleanProject(agent *Agent, rawDir string) {
	if err := CheckDocker(); err != nil {
		die(err.Error())
	}

	dir, err := filepath.Abs(rawDir)
	if err != nil {
		die("Cannot resolve path: " + err.Error())
	}

	hash := projectHash(dir)

	fmt.Println()
	printHeader(fmt.Sprintf("Smith Jail — Clean %s Project", agent.DisplayName), colorRed)
	fmt.Println()
	fmt.Printf("  %sAgent  :%s %s\n", colorBold, colorReset, agent.DisplayName)
	fmt.Printf("  %sProject:%s %s%s%s\n", colorBold, colorReset, colorCyan, dir, colorReset)
	fmt.Printf("  %sHash   :%s %s\n", colorBold, colorReset, hash)
	fmt.Println()

	containers, volumes, _ := ListProjectResources(agent, hash)

	if len(containers) == 0 && len(volumes) == 0 {
		printInfo("Nothing found for this project.")
		return
	}

	printResourceList("  ", containers, volumes)

	fmt.Println()
	if !readYesNo("  Remove the above? [y/N] ", false) {
		printInfo("Aborted.")
		return
	}
	fmt.Println()

	RemoveContainerList(containers)
	RemoveVolumeList(volumes)

	fmt.Println()
	printOK("Done.")
	fmt.Println()
}

func cmdCleanAgent(agent *Agent) {
	if err := CheckDocker(); err != nil {
		die(err.Error())
	}

	fmt.Println()
	printHeader(fmt.Sprintf("Smith Jail — Clean All %s", agent.DisplayName), colorRed)
	fmt.Println()

	containers, volumes := ListAllResources(agent)

	if len(containers) == 0 && len(volumes) == 0 {
		printInfo("No containers or volumes found.")
	} else {
		printResourceList("  ", containers, volumes)
	}

	fmt.Println()
	printWarn(fmt.Sprintf("Also removes: every %s image variant (all projects).", agent.ImageName()))
	printWarn("Your project files and credentials are NOT affected.")
	fmt.Println()
	if !readYesNo("  Remove all of the above? [y/N] ", false) {
		printInfo("Aborted.")
		return
	}
	fmt.Println()

	if len(containers) > 0 {
		printInfo("Stopping and removing containers...")
		RemoveContainerList(containers)
	}

	if len(volumes) > 0 {
		printInfo("Removing home volumes...")
		RemoveVolumeList(volumes)
	}

	printInfo("Removing images...")
	RemoveAllAgentImages(agent)

	fmt.Println()
	printOK(fmt.Sprintf("All clean. Run: smith-jail %s rebuild", agent.Name))
	fmt.Println()
	fmt.Printf("  %sTo also clear Docker's global layer cache (affects all projects):%s\n", colorDim, colorReset)
	fmt.Printf("  %s  docker builder prune%s\n", colorDim, colorReset)
	fmt.Println()
}

func cmdShow() {
	if err := CheckDocker(); err != nil {
		die(err.Error())
	}

	fmt.Println()
	printHeader("Smith Jail — Resources", colorBlue)
	fmt.Println()

	// Images — one line per (agent, effective configuration) tag
	fmt.Printf("  %sImages%s\n", colorBold, colorReset)
	for _, agent := range AllAgents {
		imgs, err := ListAgentImages(agent)
		if err != nil {
			fmt.Printf("    %-20s  %s(error: %v)%s\n", agent.ImageName(), colorRed, err, colorReset)
			continue
		}
		if len(imgs) == 0 {
			fmt.Printf("    %-30s  %s(none — run: smith-jail %s rebuild)%s\n",
				agent.ImageName(), colorDim, agent.Name, colorReset)
			continue
		}
		for _, img := range imgs {
			created := time.Unix(img.Created, 0).Format("2006-01-02")
			tags := img.RepoTags
			if len(tags) == 0 {
				tags = []string{agent.ImageName() + ":<none>"}
			}
			for _, tag := range tags {
				fmt.Printf("    %-30s  %s   created %s\n", tag, formatSize(img.Size), created)
			}
		}
	}
	fmt.Println()

	// Containers (all agents)
	containers, volumes := ListAllResources(nil)

	fmt.Printf("  %sContainers%s", colorBold, colorReset)
	if len(containers) == 0 {
		fmt.Printf("   %s(none)%s\n", colorDim, colorReset)
	} else {
		fmt.Printf("   (%d)\n", len(containers))
		for _, c := range containers {
			name := strings.TrimPrefix(c.Names[0], "/")
			stateColor := colorDim
			if c.State == "running" {
				stateColor = colorGreen
			}
			project := c.Labels["smithjail.project"]
			agentName := c.Labels["smithjail.agent"]
			fmt.Printf("    %-50s  %s%s%s", name, stateColor, c.State, colorReset)
			if agentName != "" {
				fmt.Printf("   %s[%s]%s", colorCyan, agentName, colorReset)
			}
			if project != "" {
				fmt.Printf("  %s%s%s", colorDim, project, colorReset)
			}
			fmt.Println()
		}
	}
	fmt.Println()

	fmt.Printf("  %sVolumes%s", colorBold, colorReset)
	if len(volumes) == 0 {
		fmt.Printf("      %s(none)%s\n", colorDim, colorReset)
	} else {
		fmt.Printf("      (%d)\n", len(volumes))
		for _, v := range volumes {
			project := v.Labels["smithjail.project"]
			agentName := v.Labels["smithjail.agent"]
			fmt.Printf("    %-50s", v.Name)
			if agentName != "" {
				fmt.Printf("   %s[%s]%s", colorCyan, agentName, colorReset)
			}
			if project != "" {
				fmt.Printf("  %s%s%s", colorDim, project, colorReset)
			}
			fmt.Println()
		}
	}
	fmt.Println()

	// Networks (all agents)
	nets, err := ListNetworks(nil)
	if err != nil {
		printWarn("Could not list networks: " + err.Error())
	} else {
		fmt.Printf("  %sNetworks%s", colorBold, colorReset)
		if len(nets) == 0 {
			fmt.Printf("     %s(none)%s\n", colorDim, colorReset)
		} else {
			fmt.Printf("     (%d)\n", len(nets))
			for _, n := range nets {
				fmt.Printf("    %-50s", n.Name)
				if n.Agent != "" {
					fmt.Printf("   %s[%s]%s", colorCyan, n.Agent, colorReset)
				}
				fmt.Println()
			}
		}
		fmt.Println()
	}

	// Ollama sidecar (hermes-local) — not project-scoped, so it's reported
	// on its own rather than folded into the containers list above.
	fmt.Printf("  %sOllama sidecar%s  ", colorBold, colorReset)
	if exists, running, err := OllamaStatus(); err == nil {
		switch {
		case running:
			fmt.Printf("%srunning%s\n", colorGreen, colorReset)
		case exists:
			fmt.Printf("%sstopped%s\n", colorDim, colorReset)
		default:
			fmt.Printf("%snot created%s\n", colorDim, colorReset)
		}
	} else {
		fmt.Printf("%s(error: %v)%s\n", colorRed, err, colorReset)
	}
	fmt.Println()
}

func printExitSummary(agent *Agent, dir string, startTime time.Time, sig os.Signal, exitCode int, cfg *Config) {
	hash := projectHash(dir)
	elapsed := time.Since(startTime)

	sigName := "--"
	if sig != nil {
		sigName = sig.String()
	}

	fmt.Println()
	fmt.Printf("  %sExit signal:%s %-12s  %sRunning time:%s %s\n",
		colorBold, colorReset, sigName,
		colorBold, colorReset, formatRuntime(elapsed))
	if exitCode == 137 {
		printWarn("Container was OOM-killed (exit 137) — memory limit exceeded.")
		printWarn(fmt.Sprintf("Current limit: %s. Set JAIL_MEM_LIMIT=16g in smith-jail.env to increase it.", cfg.MemLimit))
	}
	printInfo("Container exited — smith-jail exiting.")

	containers, volumes, nets := ListProjectResources(agent, hash)
	if len(containers) == 0 && len(volumes) == 0 && len(nets) == 0 {
		fmt.Println()
		return
	}

	fmt.Printf("\n  %sPersistent resources for this project:%s\n", colorBold, colorReset)
	printResourceList("    ", containers, volumes)
	for _, n := range nets {
		fmt.Printf("    network:   %s\n", n.Name)
	}
	fmt.Printf("\n  %sRun: smith-jail %s clean %s%s\n", colorDim, agent.Name, dir, colorReset)
	fmt.Println()
}

// agentAuthDesc returns a human-readable auth description for the session banner.
func agentAuthDesc(agent *Agent, cfg *Config) string {
	switch agent.Name {
	case "claude":
		return "claude.ai Pro (OAuth)"
	case "gemini":
		if cfg.GeminiAPIKey != "" {
			return "API key (Google AI Studio)"
		}
		return "OAuth (tokens.json)"
	case "codex":
		if cfg.OpenAIAPIKey != "" {
			return "API key (OpenAI Platform)"
		}
		return "ChatGPT sign-in (OAuth)"
	case "hermes":
		return "Nous Portal (OAuth)"
	case "hermes-local":
		return "Nous Portal (OAuth) — model via Ollama sidecar"
	default:
		return "unknown"
	}
}

// cmdOllama manages the Ollama sidecar directly, independent of any
// hermes-local session — useful for pre-pulling models or checking status
// without launching an agent.
func cmdOllama(args []string) {
	if len(args) == 0 {
		die("Usage: smith-jail ollama <up|down|status|pull [model]>")
	}
	if err := CheckDocker(); err != nil {
		die(err.Error())
	}
	cfg := mustLoadConfig(".", nil)

	switch args[0] {
	case "up":
		printInfo("Starting Ollama sidecar...")
		if err := StartOllamaContainer(cfg); err != nil {
			die(err.Error())
		}
		printOK("Ollama sidecar running — reachable at http://" + OllamaContainerName + ":11434")

	case "down":
		exists, running, err := OllamaStatus()
		if err != nil {
			die(err.Error())
		}
		if !exists || !running {
			printInfo("Ollama sidecar is not running.")
			return
		}
		if err := StopOllamaContainer(); err != nil {
			die(err.Error())
		}
		printOK("Ollama sidecar stopped.")

	case "status":
		exists, running, err := OllamaStatus()
		if err != nil {
			die(err.Error())
		}
		switch {
		case running:
			printOK("Running — " + OllamaContainerName + " (http://" + OllamaContainerName + ":11434 on " + OllamaNetworkName + ")")
		case exists:
			printInfo("Stopped. Run: smith-jail ollama up")
		default:
			printInfo("Not created yet. Run: smith-jail ollama up")
		}

	case "pull":
		model := cfg.OllamaModel
		if len(args) > 1 {
			model = args[1]
		}
		exists, running, err := OllamaStatus()
		if err != nil {
			die(err.Error())
		}
		if !exists || !running {
			die("Ollama sidecar is not running. Run: smith-jail ollama up")
		}
		present, err := ollamaModelPresent(model)
		if err != nil {
			die(err.Error())
		}
		if present {
			printOK(model + " already pulled.")
			return
		}
		if err := EnsureOllamaModelPulled(cfg, model); err != nil {
			if errors.Is(err, errAborted) {
				printInfo("Aborted.")
				return
			}
			die(err.Error())
		}

	default:
		die("Unknown ollama command: " + args[0] + "\n  Run: smith-jail ollama <up|down|status|pull [model]>")
	}
}

func formatRuntime(d time.Duration) string {
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	return fmt.Sprintf("%dh %dm", h, m)
}

func formatSize(bytes int64) string {
	if bytes == 0 {
		return "0 B"
	}
	units := []string{"B", "KB", "MB", "GB"}
	exp := int(math.Log(float64(bytes)) / math.Log(1000))
	if exp >= len(units) {
		exp = len(units) - 1
	}
	val := float64(bytes) / math.Pow(1000, float64(exp))
	if exp == 0 {
		return fmt.Sprintf("%d %s", bytes, units[exp])
	}
	return fmt.Sprintf("%.0f %s", val, units[exp])
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func confirm(action string, autoApprove bool) bool {
	return confirmDefault(action, autoApprove, false)
}

// confirmDefault is confirm with a caller-chosen default for a bare Enter.
func confirmDefault(action string, autoApprove bool, defaultYes bool) bool {
	if autoApprove {
		printInfo(action + " [auto-approved]")
		return true
	}
	yn := "y/N"
	if defaultYes {
		yn = "Y/n"
	}
	prompt := fmt.Sprintf("\n  %sConfirm:%s %s\n  Proceed? [%s] ", colorBold, colorReset, action, yn)
	return readYesNo(prompt, defaultYes)
}

// readYesNo prints prompt, reads a line from stdin, and reports whether it
// was an affirmative answer. A bare Enter (empty answer) resolves to
// defaultYes, matching the [y/N] vs [Y/n] convention shown in the prompt.
func readYesNo(prompt string, defaultYes bool) bool {
	fmt.Print(prompt)
	reader := bufio.NewReader(os.Stdin)
	answer, _ := reader.ReadString('\n')
	answer = strings.TrimSpace(strings.ToLower(answer))
	if answer == "" {
		return defaultYes
	}
	return answer == "y" || answer == "yes"
}

// mustLoadConfig loads dir's effective config and offers to create any
// missing config files, scoped to agent (nil if this invocation isn't
// targeting a specific agent — see PromptCreateEnvFiles).
func mustLoadConfig(dir string, agent *Agent) *Config {
	cfg, err := LoadConfig(dir)
	if err != nil {
		die(err.Error())
	}
	cfg.PromptCreateEnvFiles(agent)
	return cfg
}
