#!/usr/bin/env bash
# Register the macOS runner-install template with GARM.
#
# The template is registered as --os-type linux (GARM's params.OSType has no
# darwin value); the macOS-specific logic lives entirely inside the template,
# and the pool attaches it with --runner-install-template <name>.
#
# Usage:
#   scripts/mac/register-template.sh [name] [forge-type]
#
#   name        template name (default: macos-runner)
#   forge-type  github or gitea (default: github)
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
template="$repo_root/internal/macprovider/macos-runner.sh"

name="${1:-macos-runner}"
forge_type="${2:-github}"

case "$forge_type" in
  github|gitea) ;;
  *)
    echo "register-template: unsupported forge type '$forge_type' (want github or gitea)" >&2
    exit 2
    ;;
esac

if [ ! -f "$template" ]; then
  echo "register-template: template not found at $template" >&2
  exit 1
fi

# Fail fast on a template that does not render (JIT and non-JIT) or leaks a
# Linux-only mechanism, before handing it to GARM.
if ! "$repo_root/scripts/mac/render-template.sh" >/dev/null; then
  echo "register-template: template failed to render; not registering" >&2
  exit 1
fi

if ! command -v garm-cli >/dev/null 2>&1; then
  echo "register-template: garm-cli not found on PATH" >&2
  exit 1
fi

garm-cli template create \
  --name "$name" \
  --path "$template" \
  --forge-type "$forge_type" \
  --os-type linux
