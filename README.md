# TCV CLI

A standalone command-line tool for running AI coding agents (Claude Code, Codex, Gemini) inside sandboxed Podman/Docker containers with network policy enforcement.

Each agent session gets its own container with controlled internet access, resource limits, and project isolation. The CLI manages the full lifecycle: image building, proxy setup, session start/stop, log tailing, and crash recovery.

## Dependencies

### Required

| Dependency | Version | Purpose |
|-----------|---------|---------|
| **Go** | 1.21+ | Build the CLI binary |
| **Podman** or **Docker** | Podman 4+ / Docker 24+ | Container runtime |
| **Node.js** | 20+ | Required inside agent containers (for Claude Code, Codex, Gemini CLIs) |

### API Keys (at least one)

| Variable | Agent |
|----------|-------|
| `ANTHROPIC_API_KEY` | Claude Code |
| `OPENAI_API_KEY` | Codex |
| Google OAuth (interactive) | Gemini |

These are passed into containers automatically. Set them in your shell profile or an env file.

### Optional

| Dependency | Purpose |
|-----------|---------|
| **tmux** | Installed in containers by the base image; used for headless sessions |

## Installation

### Quick Start

```bash
# Clone and build
git clone https://github.com/yourusername/tcv.git
cd tcv/cli
go build -o tcv .

# Install to PATH
sudo cp tcv /usr/local/bin/
```

The CLI is a single Go binary with one dependency (`golang.org/x/term`). The resulting binary is fully self-contained — copy it anywhere.

### Set TCV_ROOT

The CLI needs to know where to find container image definitions and the baseline network policy. Set `TCV_ROOT` to the repo checkout:

```bash
export TCV_ROOT=/path/to/tcv
```

If unset, it defaults to `/usr/local/share/tcv`. You can symlink the repo there:

```bash
sudo ln -s /path/to/tcv /usr/local/share/tcv
```

### Install Agent Configuration

The CLI loads agent mount configs from (in priority order):

1. **Global**: `$TCV_ROOT/config/agents.json`
2. **User**: `~/.config/tcv/agents.json`

Copy the default config to the user location for customization:

```bash
mkdir -p ~/.config/tcv
cp config/agents.json ~/.config/tcv/agents.json
```

This file defines how each agent's config directory (`.claude/`, `.codex/`, `.gemini/`) is mounted into containers, which history files persist between sessions, and the git identity for each agent.

## Setup

### 1. Build Container Images

The base image installs Node.js, Claude Code, Codex, and Gemini CLIs into a Debian container:

```bash
# Build the base image (required)
tcv build tcv-agent-base

# Build the egress proxy (required for network filtering)
tcv build tcv-egress

# Build project-specific images (extend the base with your toolchain)
tcv build --all

# List available images
tcv build --list
```

### 2. Start the Egress Proxy

The egress proxy is a filtering HTTP proxy that controls which hosts agent containers can reach:

```bash
tcv proxy start
tcv proxy status
```

The proxy runs as a Podman container (`tcv-egress`) and filters outbound traffic based on per-project allow lists defined in `.tcv.json`. A baseline policy (`images/tcv-egress/baseline-policy.json`) always permits core AI API hosts (anthropic.com, openai.com, github.com, etc.).

Skip the proxy with `--no-proxy` if you want unfiltered internet access.

### 3. Initialize a Project

```bash
cd ~/projects/myapp
tcv init
```

This creates `.tcv.json` in your project root:

```json
{
  "project_name": "myapp",
  "image_type": "agent-laravel-vcs",
  "resources": {
    "memory": "2g",
    "cpus": "2",
    "pids_limit": "256"
  },
  "mounts": [],
  "local_domains": [],
  "local_ports": [],
  "hooks": {
    "session.started": {
      "command": "echo \"Started $TCV_PROJECT\" >> /tmp/tcv.log",
      "timeout": 5
    }
  }
}
```

The `init` command auto-detects your project type and suggests an appropriate container image.

### 4. Run an Agent

```bash
# Interactive session (attaches to terminal)
tcv claude
tcv codex ~/projects/myapp
tcv gemini .

# Headless (runs in background with tmux)
tcv claude --headless

# Batch mode (non-interactive, runs prompt and exits)
tcv claude --batch --prompt "Fix the failing tests in auth_test.go"
```

## Usage

```
tcv <command> [options] [project-dir]
```

### Session Commands

| Command | Description |
|---------|-------------|
| `tcv claude [dir]` | Start a Claude Code session |
| `tcv codex [dir]` | Start a Codex session |
| `tcv gemini [dir]` | Start a Gemini session |

### Management Commands

| Command | Description |
|---------|-------------|
| `tcv stop [dir]` | Gracefully stop the session container |
| `tcv kill [dir]` | Force-kill the session container |
| `tcv status [dir]` | Check container state (running/stopped/exited) |
| `tcv logs [dir]` | Tail session output logs |
| `tcv attach [dir]` | Attach to tmux inside a running container |
| `tcv reconnect [dir]` | Reconnect to a crashed session (restarts if dead) |

### Setup Commands

| Command | Description |
|---------|-------------|
| `tcv init [dir]` | Initialize project with `.tcv.json` |
| `tcv reload [dir]` | Push updated `.tcv.json` to the running proxy |
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
# Day-to-day usage
tcv claude                          # Start Claude in current directory
tcv codex ~/projects/api            # Start Codex in a specific project
tcv stop                            # Stop the running session
tcv logs -f                         # Follow live output

# Batch automation
tcv claude --batch --prompt "Refactor the auth module to use JWT"
tcv codex --batch --prompt "Add unit tests for user_service.go"

# Debugging
tcv status                          # Is the container running?
tcv attach                          # Drop into tmux (Ctrl-b d to detach)
tcv reconnect                       # Recover from terminal crash

# Image management
tcv build --list                    # See available images
tcv build tcv-agent-base        # Rebuild the base image
tcv build --all                     # Build everything

# Network policy
tcv proxy start                     # Start the egress proxy
tcv reload                          # Reload .tcv.json after editing
tcv baseline --reload               # Push updated baseline to proxy
```

## Project Configuration

Each project needs a `.tcv.json` in its root. Create one with `tcv init` or write it by hand.

### Minimal Configuration

```json
{
  "project_name": "myapp",
  "image_type": "tcv-agent-base"
}
```

### Full Configuration

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
  "mcp_host": { "url": "http://mcp-server.local:3847" },
  "preview": {
    "Frontend": "https://myapp.example.com",
    "API": "https://api.myapp.example.com"
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

Defaults if not specified in `.tcv.json`:

| Resource | Default | Notes |
|----------|---------|-------|
| Memory | 1 GB | Increase for projects with heavy builds |
| CPUs | 1 | |
| PIDs | 128 | Increase for Node.js builds (many child processes) |
| tmpfs | 1 GB at /tmp | noexec, nosuid |

### Network Policy

The `network` section in `.tcv.json` controls which hosts the agent can reach through the egress proxy:

```json
{
  "network": {
    "allow_hosts": [
      "github.com:443",
      "api.myservice.com:443",
      "registry.npmjs.org:443"
    ]
  }
}
```

Core AI/dev hosts (GitHub, npm, Anthropic, OpenAI, Google) are always allowed via the baseline policy.

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

Hooks are shell commands that run at session lifecycle events. They receive session metadata as JSON on stdin and as environment variables. Hooks run asynchronously and never block the CLI.

### Configuration

**User-level** (`~/.config/tcv/hooks.json`) -- applies to all projects:

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

**Project-level** (`.tcv.json` `hooks` field) -- overrides user-level for that project:

```json
{
  "project_name": "myapp",
  "image_type": "tcv-agent-base",
  "hooks": {
    "session.started": {
      "command": "echo $TCV_SESSION_ID >> /tmp/sessions.log"
    }
  }
}
```

### Events

| Event | Fires When |
|-------|-----------|
| `session.started` | Container starts. For interactive sessions, fires before the terminal attaches. For headless/batch, fires after container status is known. |
| `session.stopped` | Interactive session exits (user quit or agent finished). Does not fire for headless sessions since the container keeps running. |

### Hook Payload

**stdin** receives the full event as JSON:

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

**Environment variables** are also set:

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

### Example: Slack Notification

```json
{
  "session.started": {
    "command": "jq -n --arg text \"Agent $TCV_AGENT_TYPE started on $TCV_PROJECT ($TCV_GIT_BRANCH)\" '{text: $text}' | curl -sf -X POST https://hooks.slack.com/services/T.../B.../xxx -d @-",
    "timeout": 10
  }
}
```

### Example: POST to an API

Register sessions with your own dashboard or monitoring system:

```json
{
  "session.started": {
    "command": "curl -sf -X POST https://my-dashboard.example.com/api/sessions -H 'Content-Type: application/json' -d @-",
    "timeout": 10
  }
}
```

The full session JSON is piped to stdin, so `-d @-` sends it as the POST body.

## Agent Configuration

The `agents.json` file defines how each AI agent's config and history directories are mounted into containers.

### Default Agents

| Agent | CLI Command | Config Dir | History Persisted |
|-------|------------|------------|------------------|
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

Add tools to be mounted read-only into every container:

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

Session files older than 7 days are cleaned up automatically on each `tcv claude/codex/gemini` start.

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `TCV_ROOT` | `/usr/local/share/tcv` | Path to tcv repo (images, config, baseline policy) |
| `TCV_PROXY_PORT` | `8080` | Egress proxy listen port |
| `TCV_CONTAINER_RUNTIME` | auto-detect | Force `podman` or `docker` |
| `ANTHROPIC_API_KEY` | | API key for Claude Code |
| `OPENAI_API_KEY` | | API key for Codex |
| `XDG_CONFIG_HOME` | `~/.config` | User config directory |

## Troubleshooting

### Container won't start

```bash
# Check if the image exists
tcv build --list
podman images | grep agent

# Check if proxy is running (required unless --no-proxy)
tcv proxy status

# Try without proxy to isolate the issue
tcv claude --no-proxy
```

### Session crashes

```bash
# Check container state
tcv status

# Check for OOM kill
podman inspect $(cat .container_id) --format '{{.State.OOMKilled}} {{.State.Status}}'

# Increase memory in .tcv.json
# "resources": { "memory": "4g", "pids_limit": "512" }

# Reconnect (preserves Claude history for /resume)
tcv reconnect
```

### Agent can't reach a host

```bash
# Check current policy
cat .tcv.json | jq '.network'

# Add the host
# "network": { "allow_hosts": ["api.example.com:443"] }

# Reload without restarting
tcv reload

# Check baseline policy
tcv baseline
```

### "Text file busy" on install

```bash
# Stop any running sessions first
tcv stop

# Then install
sudo rm -f /usr/local/bin/tcv
sudo cp cli/tcv /usr/local/bin/tcv
```

## Architecture

The CLI is a single-file Go binary (`cli/main.go`, ~3500 lines) with one external dependency (`golang.org/x/term`). It makes zero HTTP calls — all external integration is handled through configurable lifecycle hooks.

```
tcv CLI
    │
    ├── reads .tcv.json          (per-project config)
    ├── reads ~/.config/tcv/     (user config: agents.json, hooks.json)
    ├── reads $TCV_ROOT/         (image definitions, baseline policy)
    │
    ├── manages Podman/Docker        (container lifecycle)
    ├── manages egress proxy         (network filtering)
    ├── writes .tcv/sessions/    (session logs, metadata)
    │
    └── fires lifecycle hooks        (shell commands, async, non-blocking)
```

### Repository Structure

```
cli/                         # Go source code
config/
  agents.json                # Agent mount/history configuration
  hooks.json.example         # Example lifecycle hooks
images/
  tcv-agent-base/        # Base container image (Debian + Node + agent CLIs)
  tcv-egress/            # Egress proxy image (Node.js HTTP proxy)
images-custom/               # Project-specific images (extend the base)
```

## Contributing

Contributions are welcome. Please open an issue to discuss before submitting large changes.

When adding a new agent, add its config to `config/agents.json` and document it in this README.

## License

MIT
