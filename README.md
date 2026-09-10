# smith-jail

Runs AI coding agents inside a per-project docker container. Only the specified
project directory is mounted in the container.  The host filesystem and other
projects stay invisible to the agent.  Network access is restricted via transparent proxy
to only specified targets, by default those needed by the agent.


Supports:

- [Claude Code](https://claude.ai/code)
- [Gemini CLI](https://github.com/google/gemini-cli)
- [Codex CLI](https://github.com/openai/codex)
- [Hermes Agent](https://hermes-agent.nousresearch.com/), cloud or a local Ollama model

![smith-agent icon](smith-agent-icon.jpg)

---

## Background

Agentic coding tools read and write files, run shell commands, and manage
git. Their built-in permission prompts work, but eventually you
start proceeding without reading.  Also, you're relying on the 
agent itself to not wander outside the project directory.

smith-jail sets up hard boundaries before the agent starts instead of
relying on runtime approvals. Only one directory is mounted into the
container; the agent can't see anything else on the host no matter what it
tries. The container's network can also be jailed to a per-session proxy
that relays only the agent's own API and hosts you've allowed: everything
else, for every process in the container, is refused.

This makes it easy to allow the agent permission to do anything it needs to inside
the jail since it can't break your host.

It's a **reasonable, not foolproof, sandbox**: enough of a boundary to stop
a misbehaving prompt, a jailbroken agent, or a malicious dependency from
casually reaching outside the directory you handed it. It's not a hardened
multi-tenant sandbox and doesn't defend against an attacker who already has
Docker access to your machine; see [Security model](#security-model) for
what is and isn't covered.

---

## Features

- **Filesystem jail**: only the project directory is mounted; the rest of
  the host (other projects, SSH keys, cloud config, browser profiles)
  stays invisible.
- **Network jail, on by default on Linux** (`--no-network-jail` to opt
  out): a per-session proxy sidecar relays only allowed outbound
  connections, by hostname and IP.
- **Multi-agent support**: Claude, Gemini, Codex, Hermes, each with its
  own image tag and credential directory, so personas never collide.
- **Per-project configuration**: base image, packages, resource limits,
  root/sudo, network jail, and more, layered global → per-project →
  environment, with a content-addressed image tag per effective
  configuration.
- **Interactive TUI**: run, shell, switch agent/project, edit settings,
  manage Ollama models, browse Docker artifacts, check for updates, run
  doctor checks without memorizing flags.
- **Resource limits**: memory/CPU caps (8 GB / 2 CPU by default),
  configurable globally or per project.
- **Docker artifact management**: list and remove smith-jail's own
  images, containers, volumes, and networks (`show`, `clean`, or the TUI's
  "Docker artifacts" screen).

---

## How it works

smith-jail is a single static binary using the Docker SDK directly, no
`docker compose`.

On `smith-jail [agent] run`:

1. Reads config from `$XDG_CONFIG_HOME/smith-jail/smith-jail.env`
2. Builds the image if it isn't already current
3. If the network jail is active: creates a dedicated Docker network,
   starts a proxy sidecar on it (building its image from embedded source
   the first time it's needed), and runs a one-shot helper (holding
   `NET_ADMIN` only for that single call) that installs nft rules
   *inside the sidecar's namespace* redirecting outbound :80/:443/:53
   into it
4. Launches the agent container, joined to the sidecar's namespace when
   jailed
5. On exit: removes the proxy sidecar and the session's network

Each project's container is named by the SHA-256 hash of its absolute
path, so two projects with the same directory name never collide.

```
smith-jail claude run /your/project
  │
  ├─ build image if needed (debian:trixie-slim + tools + agent)
  ├─ [optional] create session Docker network
  ├─ [optional] start proxy sidecar on it, wait for it to be listening
  ├─ [optional] run netsetup helper (NET_ADMIN, one call, then exits) to
  │             install nft redirect rules inside the sidecar's namespace
  │
  └─ docker run --rm
         ├─ bind mount: /your/project → /workspace              (rw)
         ├─ volume:     project-home  → /home/agent             (rw)
         ├─ bind mount: agent creds   → /home/agent/.claude etc (rw)
         ├─ Resource limits (8 GB RAM / 2 CPU default)
         └─ network: bridge, or the proxy sidecar's namespace
```

---

## Requirements

- Linux (fully supported) or macOS (best-effort; see [Platform support](#platform-support))
- Docker Engine on Linux; Docker Desktop on macOS
- Go 1.24+ (to build from source)

---

## Platform support

* Linux - smith-jail is developed and tested on opensuse
  * Requires docker to be installed
  * SELinux support
* macOS builds (amd64/arm64) are best-effort
  * **The network jail does NOT work on macOS**
 
Windows is not supported.

---

## Building

```bash
git clone https://github.com/jrivard/smith-jail
cd smith-jail
go build
```

---

## Quick start

```bash
# Open the interactive dashboard (also what a bare `smith-jail` does in a terminal)
smith-jail tui

# Launch Claude Code in a project directory
smith-jail claude run ~/projects/myapp

# Launch Gemini CLI in a project directory
smith-jail gemini run ~/projects/myapp

# Launch Codex CLI in a project directory
smith-jail codex run ~/projects/myapp

# Launch Hermes Agent against the cloud
smith-jail hermes run ~/projects/myapp

# Launch Hermes Agent against a local Ollama model (see "Local models" below)
smith-jail hermes-local run ~/projects/myapp
```

---

## Interactive TUI

Running `smith-jail` with no arguments (or `smith-jail tui`) opens a
full-screen dashboard instead of the CLI form above. It's the same
underlying operations: the dashboard hands off to the ordinary CLI code
path once you've made a selection and the alternate screen is torn down,
so a jailed session still gets a real TTY.

The dashboard opens on a **sessions overview**: every currently running
agent session (refreshed every few seconds), a **New session** submenu,
and a few agent/project-independent utilities:

| Row | Does |
|---|---|
| *(an active session)* | Open a live [`netview`](#network-jail) of that session's network activity |
| New session ▸ | Enter the submenu below to configure/launch a session |
| Docker artifacts | Browse/remove smith-jail's images, containers, volumes, and networks |
| Check updates | Compare each agent's baked-in version against the latest available |
| Doctor | Run the same environment checks as `smith-jail doctor` |
| Help | Show the CLI help text |
| Quit | Exit |

`esc`/`q` on the sessions overview quits, same as the CLI. **New session**
opens a second menu, scoped to the currently selected agent/project;
`esc`/`q` there steps back to the sessions overview:

| Row | Does |
|---|---|
| Run | Launch the current agent in the current project |
| Shell | Open a shell in the current project's container |
| Change agent | Switch between Claude, Gemini, Hermes, Hermes (local) |
| Change project | Pick a directory, including recently-used projects |
| Settings | Edit `smith-jail.env` settings (Global/Project scope via `tab`) |
| Ollama model | Pick or pull the model `hermes-local` points at (see [Local models](#local-models-hermes-local)) |
| Dockerfile | Preview the generated Dockerfile for the current agent/project without building it |

Recently-used projects are tracked in `$XDG_CACHE_HOME/smith-jail/recent.json`,
backed by the `smithjail.project` Docker label on every container/volume
smith-jail creates, so the list survives even if the cache file is lost.

---

## Commands

```
smith-jail claude       <command> [flags] [args]
smith-jail gemini       <command> [flags] [args]
smith-jail hermes       <command> [flags] [args]
smith-jail hermes-local <command> [flags] [args]
smith-jail ollama       <up|down|status|pull [model]>
smith-jail show
smith-jail doctor
smith-jail tui
smith-jail help

Commands (per agent):
  run        [flags] <dir>          Launch the agent in the given directory
  shell      [flags] <dir>          Open a shell in the container
  setup      [flags] [dir] [args]   Run the agent's own setup/login inside the container (default dir: cwd)
  dockerfile [dir]                  Print the generated Dockerfile for dir's configuration (default: cwd)
  rebuild    [--yes] [dir]          Rebuild the image for dir's configuration (default: cwd)
  clean      [dir]                  Remove containers/volumes for a project, or all images/containers for this agent
```

Run `smith-jail --help` for full current options.

### netlog / netview

While running, you can see every DNS query and TCP
connection the proxy sidecar saw, allowed or blocked.  Run these in a second terminal or
tmux pane, since `run`/`shell` occupy smith-jail's own terminal for the
whole session. See [Network jail](#network-jail) for details.

```bash
smith-jail claude netview .                 # live TUI
smith-jail claude netlog --follow .         # plain, pipeable tail
```

## Local models (hermes-local)

`hermes-local` runs the same Hermes Agent binary as `hermes`, against a
local [Ollama](https://ollama.com) model instead of the cloud.   Ollama is run in its 
own managed docker container
(`~/.hermes-local`, independent of `~/.hermes`).  

The Ollama model server itself isn't built or torn down per project like
agent images are; it's one long-lived sidecar container
(`smithjail-ollama`) shared across every `hermes-local` session:

The ollama container can be managed it directly, independent of any session, with
  `smith-jail ollama up|down|status|pull [model]`.

It sits on its own persistent Docker network (`smithjail-ollama-net`) so
  `hermes-local` can reach it at a stable address
  (`http://smithjail-ollama:11434/v1`) regardless of the network jail.

Configure it in `$XDG_CONFIG_HOME/smith-jail/ollama.env`. **Defaults are
CPU-only** (no image override, no device passthrough) so a fresh
`hermes-local run` works with zero setup on any machine. GPU passthrough
is opt-in:

```bash
# AMD ROCm, e.g. RDNA3 integrated GPUs like the Radeon 780M (gfx1103):
# HSA_OVERRIDE_GFX_VERSION presents it as the officially-supported gfx1102.
OLLAMA_IMAGE=ollama/ollama:rocm
OLLAMA_DEVICES="/dev/kfd /dev/dri"
OLLAMA_GROUP_ADD="video render"
OLLAMA_EXTRA_ENV="HSA_OVERRIDE_GFX_VERSION=11.0.2 OLLAMA_IGPU_ENABLE=1"
```

`smith-jail hermes-local run` handles the rest:

- If the configured model isn't pulled into the sidecar yet, it prompts
  to pull it (skip with `--yes`/`-y`); no manual `docker exec` needed.
  Pull ahead of time with `smith-jail ollama pull [model]`.
- It seeds `~/.hermes-local/config.yaml` with a `model:` block pointing
  at the sidecar (`http://smithjail-ollama:11434/v1`, provider `custom`)
  the first time that file doesn't exist; no manual `hermes model →
  Custom endpoint` step. It only writes the file once; edit it (or run
  `hermes model` / `hermes config set`) freely afterward.

The model tag defaults to `llama3.1:8b`, verified by hand against
Ollama's `/v1/chat/completions` endpoint to return a properly structured
`tool_calls` response, which is the part that actually matters. Override
it with `OLLAMA_MODEL` in `ollama.env`.

**"Tool-calling capable" isn't the same as "works through Ollama's
OpenAI-compat endpoint."** Two tempting alternatives that don't work,
found the hard way:

- Nous's own `hermes3`/`hermes4` tags: Ollama lists `tools` as a
  capability for them, but Hermes Agent refuses them outright at startup
  ("NOT agentic... lack tool-calling capabilities required for agent
  workflows"). Same team's model doesn't make it the right pairing.
- `qwen2.5-coder:7b`: emits tool-call-shaped JSON, but on Ollama 0.32.5
  it never lands in the response's `tool_calls` field; it arrives as
  plain text in `content` that Hermes can't act on.

If you swap models, verify it works *before* wiring it into Hermes:

```bash
docker exec smithjail-ollama ollama pull <model>
curl -s http://127.0.0.1:11434/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "<model>",
    "messages": [{"role": "user", "content": "List the files in the current directory using the terminal tool."}],
    "tools": [{"type": "function", "function": {"name": "terminal",
      "description": "Run a shell command",
      "parameters": {"type": "object", "properties": {"command": {"type": "string"}}, "required": ["command"]}}}]
  }' | python3 -m json.tool
```

A working model returns a populated `choices[0].message.tool_calls` array
with the correct function name, not JSON-looking text in `content`.

First-time setup, inside a session:

```bash
smith-jail hermes-local setup ~/projects/myapp --portal   # log in
smith-jail hermes-local run ~/projects/myapp
```

---

## Configuration

Paths follow the [XDG Base Directory specification](https://specifications.freedesktop.org/basedir-spec/latest/).

| Path | Purpose |
|---|---|
| `$XDG_CONFIG_HOME/smith-jail/smith-jail.env` | Global jail settings |
| `$XDG_CONFIG_HOME/smith-jail/claude.env` | Claude-specific settings |
| `$XDG_CONFIG_HOME/smith-jail/gemini.env` | Gemini-specific settings |
| `$XDG_CONFIG_HOME/smith-jail/codex.env` | Codex-specific settings |
| `$XDG_CONFIG_HOME/smith-jail/hermes.env` | Hermes-specific settings |
| `$XDG_CONFIG_HOME/smith-jail/hermes-local.env` | Hermes (local Ollama) credentials |
| `$XDG_CONFIG_HOME/smith-jail/ollama.env` | Ollama sidecar settings (image, GPU passthrough); see [Local models](#local-models-hermes-local) |
| `$XDG_CONFIG_HOME/smith-jail/init.sh` | Optional build-time script |
| `$XDG_CONFIG_HOME/smith-jail/start.sh` | Optional per-session startup script |
| `$XDG_CONFIG_HOME/smith-jail/projects/<hash>.env` | Private, per-project override (see below) |

### Per-project overrides

Every setting in `smith-jail.env` (base image, packages, resource
limits, root/sudo, network jail, auto-approve) can be overridden for one
project. Settings resolve through three layers, lowest to highest
precedence:

```
built-in default
  → smith-jail.env                              (global)
  → $XDG_CONFIG_HOME/smith-jail/projects/<hash>.env   (private, keyed by project path)
  → process environment                          (JAIL_* / CLAUDE_* / GEMINI_* vars)
```

The private per-project store is machine-local and never touches the
project's own files: useful for personal preferences (a bigger memory
limit, say) you don't want to inflict on teammates. It's a plain
`KEY=value` file in the same format as `smith-jail.env`, hand-editable
directly.

**There's deliberately no project-directory config file** read from the
repo itself. These settings can grant the container root, passwordless
sudo, or turn off the network jail: if a file inside the project
directory could set them, cloning an untrusted repo and running it would
let that repo silently escalate its own privileges with no prompt.
Per-project settings therefore always live outside the project tree,
under your own `$XDG_CONFIG_HOME`.

In the TUI, Settings and Packages edit one scope at a time: `tab`
switches between Global and Project. Project scope shows inherited values
muted and marked `(inherited)`; `c` clears an override back to
inheriting. Saving in Project scope always writes to the private
per-project store.

Base image and packages are baked into the image, so each distinct
effective configuration gets its own content-addressed Docker image tag
(two projects with identical settings share one image, no redundant
build). `smith-jail <agent> show` lists every tag built for an agent;
`smith-jail <agent> rebuild [dir]` rebuilds just the one for that
project; `smith-jail <agent> clean` (no directory) removes every tag for
that agent.

### Network jail

The network jail restricts outbound connections from the container to
the agent's API and any hosts you allow: every process in the container
is covered, not just the agent, and it enforces by both hostname and IP
so a hardcoded or cached address can't bypass it.

A dedicated proxy sidecar owns the session's network namespace. A
one-shot helper installs nft rules *inside that namespace* redirecting
outbound :80/:443/:53 into the sidecar, which forwards DNS queries to a
real resolver (truthful answers for allowed hosts, `NXDOMAIN` otherwise)
and relays TCP connections after checking their real destination
(recovered from the kernel via `SO_ORIGINAL_DST`, never by inspecting or
terminating TLS) against the same allow-list.

No standing capability is required on the smith-jail binary or the agent
container: `NET_ADMIN` is granted only to the one-shot helper, for the
single call that installs the rules, in a container smith-jail creates
and removes itself. The sidecar and helper images are built locally the
first time they're needed, from source smith-jail embeds; nothing is
pulled from a registry.

## Security model

### What's isolated

- **Filesystem**: only the target project directory (plus that agent's
  own per-project home volume) is mounted into the container. The rest of
  the host (other projects, SSH keys, cloud credentials, browser
  profiles) is never visible, regardless of what the agent attempts.
- **Network** (on by default on Linux; opt out with `--no-network-jail`):
  outbound connections are relayed through a per-session proxy sidecar
  that only permits the agent's own API and any hosts you explicitly
  allow, by hostname and IP; everything else is refused, for every
  process in the container.
- **Projects from each other**: each project gets its own container
  name, image tag, and home volume, keyed by the SHA-256 hash of its
  absolute path, so one project's agent can't reach another's files or
  credentials.

### Residual risks

- **The network is open on macOS, and anywhere the jail is off.** With
  `--no-network-jail` (or on a platform where it defaults off), the
  container has ordinary outbound access: nothing stops a compromised
  agent from exfiltrating project contents. Leave the jail on for
  anything sensitive.
- **The allowed API endpoint is itself a channel.** The agent's own API
  has to stay reachable for it to function, and traffic to an allowed
  host can carry arbitrary data. The jail stops connections to
  *unexpected* destinations; it can't distinguish legitimate API traffic
  from data smuggled inside it.
- **Only :80/:443/:53 are covered.** A service reachable solely on some
  other port (a raw TCP API, git-over-ssh) isn't reachable through the
  jail at all, allowed or not. Outbound UDP other than DNS (including
  HTTP/3-over-QUIC) is refused outright rather than relayed.
- **Each agent's own credentials are exposed to that agent.** The
  OAuth/config directory for whichever agent you're running
  (`~/.claude`, `~/.gemini`, `~/.hermes`, …) is bind-mounted read-write
  into the container so logins persist across sessions. A compromised
  agent can read, corrupt, or (subject to the network jail) exfiltrate
  those specific tokens, not any other credentials on the host.
- **Docker is not a hardened isolation boundary.** Containers run with
  Docker's default capabilities and seccomp profile: no `--cap-drop`,
  read-only rootfs, or gVisor/Kata-style sandboxing. A kernel
  container-escape exploit remains a residual risk, and
  `JAIL_RUN_AS_ROOT` / `JAIL_SUDO` widen the blast radius if used.
- **Docker access is already host-equivalent access.** Anyone who can run
  `docker` commands can trivially get a root shell on the host: that's
  a property of Docker, not something smith-jail changes. The threat
  model here is a misbehaving *agent*, not a malicious local user.
- **Resource limits are for stability, not security.** The default 8
  GB/2 CPU cap keeps a runaway agent from starving the host; it isn't an
  attack-surface reduction.
- **Nothing vets the agent's output.** The jail protects your host while
  the agent runs, but doesn't inspect what it writes into `/workspace`.
  A jailbroken agent's leftover code will still run if you run it
  yourself later, outside the jail.

If you need stronger guarantees (untrusted multi-tenant workloads,
defense against a local attacker, protection against kernel exploits),
smith-jail alone isn't sufficient.

---

## License

[Apache License 2.0](LICENSE)
