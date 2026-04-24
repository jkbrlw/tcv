# TCV

Run AI coding agents (Claude Code, Codex, Gemini) in sandboxed containers with network policy enforcement.

Each agent session gets its own Podman/Docker container with controlled internet access, resource limits, and project isolation. TCV manages the full lifecycle: image building, proxy setup, session start/stop, log tailing, and crash recovery.

## Quick Start

```bash
git clone https://github.com/yourusername/tcv.git
cd tcv
make install        # Build + install to /usr/local/bin
make build-base     # Build the base agent container image
make build-proxy    # Build the egress proxy image
```

Then for any project:

```bash
cd ~/projects/myapp
tcv init                # Create .tcv.json config
tcv proxy start         # Start the network filtering proxy
tcv claude              # Launch Claude Code in a container
```

## Make Targets

| Target | Description |
|--------|-------------|
| `make build` | Build the `tcv` binary to `cli/tcv` |
| `make install` | Build and install to `/usr/local/bin/tcv` |
| `make build-base` | Build `tcv-agent-base` container image (Debian + Node + agent CLIs) |
| `make build-proxy` | Build `tcv-egress` container image (filtering HTTP proxy) |
| `make build-images` | Build all images (base + proxy + any custom images in `images-custom/`) |
| `make clean` | Remove build artifacts |

## Dependencies

| Dependency | Version | Purpose |
|-----------|---------|---------|
| **Go** | 1.21+ | Build the CLI binary |
| **Podman** or **Docker** | Podman 4+ / Docker 24+ | Container runtime |

You also need at least one API key, set in your shell profile or an env file:

| Variable | Agent |
|----------|-------|
| `ANTHROPIC_API_KEY` | Claude Code |
| `OPENAI_API_KEY` | Codex |
| Google OAuth (interactive) | Gemini |

## Installation

### From Source

```bash
git clone https://github.com/yourusername/tcv.git
cd tcv
make install
```

This builds a single Go binary (one dependency: `golang.org/x/term`) and copies it to `/usr/local/bin/tcv`.

### Set TCV_ROOT

TCV needs to find the container image definitions and baseline network policy. Set `TCV_ROOT` to the repo checkout:

```bash
# Add to your shell profile
export TCV_ROOT=/path/to/tcv
```

If unset, it defaults to `/usr/local/share/tcv`. You can symlink the repo there instead:

```bash
sudo ln -s /path/to/tcv /usr/local/share/tcv
```

### Agent Configuration (Optional)

TCV loads agent configs from (in priority order):

1. **Global**: `$TCV_ROOT/config/agents.json` (ships with the repo)
2. **User**: `~/.config/tcv/agents.json` (your overrides)

The defaults work out of the box. To customize agent flags, git identity, or history mounts:

```bash
mkdir -p ~/.config/tcv
cp config/agents.json ~/.config/tcv/agents.json
```

## Setup

### 1. Build Container Images

```bash
make build-base     # Required — base image with Node.js + agent CLIs
make build-proxy    # Required — egress network proxy
make build-images   # Optional — also builds any custom images in images-custom/
```

### 2. Start the Egress Proxy

```bash
tcv proxy start
```

The proxy filters outbound traffic per project. Core hosts (GitHub, npm, Anthropic, OpenAI, Google) are always allowed. Skip it with `--no-proxy` for unfiltered access.

### 3. Initialize a Project

```bash
cd ~/projects/myapp
tcv init
```

This creates `.tcv.json` with auto-detected project type. You can also pass a path:

```bash
tcv init ~/projects/myapp
```

### 4. Run an Agent

**All commands default to the current directory.** You can `cd` into any project and run commands directly — no path argument needed:

```bash
cd ~/projects/myapp
tcv claude              # Start Claude Code
tcv status              # Check if container is running
tcv logs -f             # Follow live output
tcv stop                # Stop the session
```

Or pass a path explicitly:

```bash
tcv claude ~/projects/myapp
tcv stop ~/projects/myapp
```

## Usage

```
tcv <command> [options] [project-dir]
```

**All commands default to the current working directory** when no `project-dir` is given. `cd` into your project and run commands directly.

### Session Commands

| Command | Description |
|---------|-------------|
| `tcv claude` | Start a Claude Code session |
| `tcv codex` | Start a Codex session |
| `tcv gemini` | Start a Gemini session |

### Management Commands

| Command | Description |
|---------|-------------|
| `tcv stop` | Gracefully stop the session container |
| `tcv kill` | Force-kill the session container |
| `tcv status` | Check container state (running/stopped/exited) |
| `tcv logs` | Tail session output logs |
| `tcv attach` | Attach to tmux inside a running container |
| `tcv reconnect` | Reconnect to a crashed session (restarts if dead) |

### Setup Commands

| Command | Description |
|---------|-------------|
| `tcv init` | Initialize project with `.tcv.json` |
| `tcv reload` | Push updated `.tcv.json` to the running proxy |
| `tcv baseline` | View or update the proxy's baseline network policy |
| `tcv proxy start\|stop\|status` | Manage the egress proxy container |
| `tcv build [image]` | Build container images |

### Session Options

```
-d, --project-dir      Path to project directory (default: current dir)
-n, --project-name     Logical project name (default: directory name)
    --headless         Run in headless mode (tmux + script PTY wrapper)
    --batch            Run non-interactively with a prompt, exit when done
    --prompt "..."     Prompt string for batch mode (requires --batch)
    --timeout 10m      Session timeout (kills container after duration)
    --no-proxy         Bypass egress proxy (direct internet access)
    --env-file .env    Load extra environment variables into the container
    --policy file.json Override the policy file path
```

### Examples

```bash
# Typical workflow — cd into your project, run everything from there
cd ~/projects/myapp
tcv init                                # One-time setup
tcv claude                              # Start Claude
tcv logs -f                             # Follow output in another terminal
tcv stop                                # Done for now

# Batch automation (non-interactive)
cd ~/projects/api
tcv claude --batch --prompt "Fix the failing tests in auth_test.go"
tcv codex --batch --prompt "Add unit tests for user_service.go"

# Or specify paths explicitly
tcv claude ~/projects/myapp
tcv status ~/projects/myapp
tcv stop ~/projects/myapp

# Debugging
tcv status                              # Is the container running?
tcv attach                              # Drop into tmux (Ctrl-b d to detach)
tcv reconnect                           # Recover from terminal crash

# Image management
tcv build --list                        # See available images
tcv build tcv-agent-base                # Rebuild the base image
tcv build --all                         # Build everything

# Network policy
tcv proxy start                         # Start the egress proxy
tcv reload                              # Reload .tcv.json after editing allow_hosts
tcv baseline --reload                   # Push updated baseline to proxy
```

## Project Configuration

Each project needs a `.tcv.json` in its root. Create one with `tcv init` or write it by hand.

### Minimal

```json
{
  "project_name": "myapp",
  "image_type": "tcv-agent-base"
}
```

### Full

```json
{
  "project_name": "myapp",
  "image_type": "agent-laravel-vcs",
  "resources": {
    "memory": "2g",
    "cpus": "2",
    "pids_limit": "256"
  },
  "mounts": [
    { "source": "/path/on/host", "destination": "/path/in/container", "mode": "ro" }
  ],
  "local_domains": ["myapp.local", "api.myapp.local"],
  "local_ports": [8000, 5173, 3306],
  "network": {
    "allow_hosts": [
      "api.myservice.com:443",
      "registry.npmjs.org:443"
    ]
  },
  "hooks": {
    "session.started": {
      "command": "curl -sf http://my-server/api/sessions -d @-",
      "timeout": 10
    },
    "session.stopped": {
      "command": "echo stopped >> /tmp/tcv.log"
    }
  }
}
```

### Resource Limits

Defaults if not specified:

| Resource | Default | Notes |
|----------|---------|-------|
| Memory | 1 GB | Increase for projects with heavy builds |
| CPUs | 1 | |
| PIDs | 128 | Increase for Node.js builds (many child processes) |
| tmpfs | 1 GB at /tmp | noexec, nosuid |

### Network Policy

The `network.allow_hosts` list in `.tcv.json` controls which hosts the agent can reach. Core AI and dev hosts (GitHub, npm, Anthropic, OpenAI, Google) are always allowed via the baseline policy.

Edit `.tcv.json` and run `tcv reload` to apply changes without restarting the session.

## Container Images

### Base Image

`tcv-agent-base` is a Debian bookworm-slim image with:
- Node.js 20
- Claude Code, Codex, and Gemini CLIs (installed to `/opt/agent-cli/`)
- git, jq, curl, tmux, build-essential
- Non-root `agent` user (uid 10001)

### Custom Images

Create project-specific images in `images-custom/` that extend the base:

```dockerfile
FROM localhost/tcv-agent-base:latest
RUN apt-get update && apt-get install -y php8.3-cli composer
```

Name the directory `agent-<stack>-vcs` (e.g., `agent-laravel-vcs`). Reference it in `.tcv.json` as the `image_type`.

## Lifecycle Hooks

Hooks are shell commands that fire at session lifecycle events. They run asynchronously and never block the CLI.

### Configuration

**User-level** (`~/.config/tcv/hooks.json`) — applies to all projects:

```json
{
  "session.started": {
    "command": "echo \"[$(date)] Started $TCV_AGENT_TYPE on $TCV_PROJECT\" >> /tmp/tcv.log",
    "timeout": 5
  },
  "session.stopped": {
    "command": "echo \"[$(date)] Stopped $TCV_SESSION_ID\" >> /tmp/tcv.log"
  }
}
```

**Project-level** (`.tcv.json` `hooks` field) — overrides user-level per project.

### Events

| Event | Fires When |
|-------|-----------|
| `session.started` | Container starts. Interactive: before terminal attaches. Headless/batch: after container status is known. |
| `session.stopped` | Interactive session exits. Does not fire for headless sessions (container keeps running). |

### Hook Payload

Hooks receive session data two ways:

**stdin** — full JSON:

```json
{
  "event": "session.started",
  "sessionId": "a1b2c3d4-...",
  "project": "myapp",
  "projectPath": "/home/user/projects/myapp",
  "name": "myapp-claude-20260423",
  "agentType": "claude",
  "cmd": "claude --dangerously-skip-permissions",
  "status": "running",
  "containerName": "agent-myapp-12345",
  "gitBranch": "feature/auth",
  "gitCommit": "abc1234",
  "gitDirty": true,
  "timestamp": "2026-04-23T10:30:00Z"
}
```

**Environment variables**:

| Variable | Example |
|----------|---------|
| `TCV_EVENT` | `session.started` |
| `TCV_SESSION_ID` | `a1b2c3d4-...` |
| `TCV_PROJECT` | `myapp` |
| `TCV_PROJECT_PATH` | `/home/user/projects/myapp` |
| `TCV_AGENT_TYPE` | `claude` |
| `TCV_CONTAINER_NAME` | `agent-myapp-12345` |
| `TCV_STATUS` | `running` |
| `TCV_GIT_BRANCH` | `feature/auth` |

### Hook Examples

**Slack notification:**
```json
{
  "session.started": {
    "command": "jq -n --arg text \"Agent $TCV_AGENT_TYPE started on $TCV_PROJECT ($TCV_GIT_BRANCH)\" '{text: $text}' | curl -sf -X POST https://hooks.slack.com/services/T.../B.../xxx -d @-",
    "timeout": 10
  }
}
```

**POST to an API** (stdin JSON piped as request body):
```json
{
  "session.started": {
    "command": "curl -sf -X POST https://my-dashboard.example.com/api/sessions -H 'Content-Type: application/json' -d @-",
    "timeout": 10
  }
}
```

## Agent Configuration

The `agents.json` file defines how each agent's config directory is mounted into containers.

### Default Agents

| Agent | Command | Config Dir | History Persisted |
|-------|---------|------------|------------------|
| Claude | `claude --dangerously-skip-permissions` | `~/.claude/` | `history.jsonl`, `projects/`, `todos/`, `session-env/`, `plans/` |
| Codex | `codex --full-auto` | `~/.codex/` | `history.jsonl`, `sessions/`, `log/` |
| Gemini | `gemini` | `~/.gemini/` | (OAuth creds only) |
| Aider | `aider --yes --no-git` | `~/.aider/` | Chat history, cache |

### Adding a New Agent

Add an entry to `~/.config/tcv/agents.json`:

```json
{
  "agents": {
    "my-agent": {
      "command": "my-agent-cli",
      "args": ["--auto"],
      "git_name": "My Agent",
      "git_email": "agent@example.com",
      "config_dir": ".my-agent",
      "container_path": "/home/agent/.my-agent",
      "history_mounts": ["sessions/", "cache/"]
    }
  }
}
```

### Mounting Host Tools

Mount scripts or binaries read-only into every container:

```json
{
  "tools": [
    {
      "name": "my-script",
      "host_path": "~/bin/my-script.sh",
      "container_path": "/home/agent/bin/my-script.sh"
    }
  ]
}
```

## Session Storage

Sessions are stored locally within each project:

```
myapp/
  .tcv/
    sessions/
      a1b2c3d4-.../
        meta.json       # Session metadata (id, agent, status, timestamps)
        events.jsonl    # Structured event log
        output.log      # Raw terminal output (ANSI codes)
        stdout.log      # Container stdout
        stderr.log      # Container stderr
  .container_id         # Current container name (for stop/status)
  .session_id           # Current session UUID (for reconnect)
  .claude/              # Claude Code's own history (standard location)
  .codex/               # Codex history (standard location)
  .gemini/              # Gemini history (standard location)
```

Session files older than 7 days are cleaned up automatically.

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `TCV_ROOT` | `/usr/local/share/tcv` | Path to the TCV repo (images, config, baseline policy) |
| `TCV_PROXY_PORT` | `8080` | Egress proxy listen port |
| `TCV_CONTAINER_RUNTIME` | auto-detect | Force `podman` or `docker` |
| `ANTHROPIC_API_KEY` | | API key for Claude Code |
| `OPENAI_API_KEY` | | API key for Codex |
| `XDG_CONFIG_HOME` | `~/.config` | User config directory |

## Troubleshooting

### Container won't start

```bash
tcv build --list              # Is the image built?
podman images | grep agent    # Is it visible to podman?
tcv proxy status              # Is the proxy running?
tcv claude --no-proxy         # Try without proxy to isolate
```

### Session crashes or OOM

```bash
tcv status
podman inspect $(cat .container_id) --format '{{.State.OOMKilled}} {{.State.Status}}'
tcv reconnect                 # Restart + reattach (Claude history preserved)
```

Increase resources in `.tcv.json`:
```json
{ "resources": { "memory": "4g", "pids_limit": "512" } }
```

### Agent can't reach a host

```bash
cat .tcv.json | jq '.network'           # Check your allow list
# Add "api.example.com:443" to network.allow_hosts
tcv reload                               # Apply without restarting
```

### "Text file busy" on install

```bash
tcv stop                                 # Stop running sessions first
sudo rm -f /usr/local/bin/tcv && sudo cp cli/tcv /usr/local/bin/tcv
```

## Architecture

```
tcv CLI
    │
    ├── reads .tcv.json              per-project config
    ├── reads ~/.config/tcv/         user config (agents.json, hooks.json)
    ├── reads $TCV_ROOT/             image definitions, baseline policy
    │
    ├── manages Podman/Docker        container lifecycle
    ├── manages tcv-egress           network filtering proxy
    ├── writes .tcv/sessions/        session logs, metadata
    │
    └── fires lifecycle hooks        shell commands, async, non-blocking
```

### Repository Structure

```
cli/                         # Go source code (single file + go.mod)
config/
  agents.json                # Agent mount/history configuration
  hooks.json.example         # Example lifecycle hooks
images/
  tcv-agent-base/            # Base container image (Debian + Node + agent CLIs)
  tcv-egress/                # Egress proxy image (Node.js HTTP proxy)
images-custom/               # Project-specific images (extend the base)
```

## Contributing

Contributions are welcome. Please open an issue to discuss before submitting large changes.

## License

MIT
