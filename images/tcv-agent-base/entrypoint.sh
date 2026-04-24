#!/usr/bin/env bash
set -euo pipefail

[ $# -gt 0 ] || { echo "no command provided"; exit 2; }

# Ensure PATH includes agent CLI bin (installed to /opt to survive home mounts)
export PATH="/opt/agent-cli/bin:/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/bin:/sbin:/bin:${PATH:-}"
export HOME="/home/agent"

# Ensure /home/agent exists and is writable (for tmpfs mounts)
if [ ! -d "/home/agent" ]; then
  mkdir -p /home/agent 2>/dev/null || true
fi

# Use full path for known agent commands (script -c uses /bin/sh which doesn't inherit PATH)
case "$1" in
  claude) set -- "/opt/agent-cli/bin/claude" "${@:2}" ;;
  codex)  set -- "/opt/agent-cli/bin/codex" "${@:2}" ;;
  aider)  set -- "/opt/agent-cli/bin/aider" "${@:2}" ;;
esac

exec "$@"
