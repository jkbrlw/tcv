# tcv-agent-base

Base container image for AI agent sessions (Claude Code, Codex).

## What's Included

- **Debian Bookworm slim** base
- **Node.js 20** with npm
- **Core utilities**: git, jq, curl, wget, tmux, build-essential, openssh-client
- **AI CLIs**: @anthropic-ai/claude-code, @openai/codex
- **Security**: Non-root sandbox user (UID 10001), git push protection for main/master/develop

## Building

```bash
cd tcv-infra
make build-agent-base

# Or manually:
podman build -t localhost/tcv-agent-base:latest images/tcv-agent-base/
```

## Extending for Project-Specific Tooling

Create a new Containerfile in `images-custom/`:

```dockerfile
# images-custom/agent-laravel-vcs/Containerfile
FROM localhost/tcv-agent-base:latest

USER root

# Add PHP and Composer
RUN apt-get update && apt-get install -y --no-install-recommends \
      php8.2-cli php8.2-mbstring php8.2-xml php8.2-curl \
  && apt-get clean && rm -rf /var/lib/apt/lists/*

COPY --from=composer:2 /usr/bin/composer /usr/local/bin/composer

USER 10001:10001
```

```dockerfile
# images-custom/agent-go-nuxt-vcs/Containerfile
FROM localhost/tcv-agent-base:latest

USER root

# Add Go
ARG GO_VERSION=1.23.4
RUN curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" -o go.tar.gz \
  && tar -C /usr/local -xzf go.tar.gz \
  && rm go.tar.gz

ENV PATH="/usr/local/go/bin:/home/sandbox/go/bin:${PATH}"
ENV GOPATH="/home/sandbox/go"

USER 10001:10001

# Add Nuxt CLI
RUN npm install -g nuxi
```

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `NPM_CONFIG_PREFIX` | `/home/sandbox/.npm-global` | User npm install location |
| `PATH` | includes npm-global/bin | Ensures npm binaries are available |

## Security Notes

- Runs as non-root user `sandbox` (UID 10001)
- Git wrapper blocks pushes to `main`, `master`, `develop` branches
- Git wrapper blocks `request-pull` and `remote set-url --push`
