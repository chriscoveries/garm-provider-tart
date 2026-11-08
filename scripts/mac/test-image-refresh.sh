#!/usr/bin/env bash
# Test image-refresh.sh against the stub tart/crane on Linux (no Mac, no Tart):
# digest pinning, pull + verify + prune when idle, refuse-to-prune while a VM is
# running, and bare-tag resolution via crane.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
scripts_dir="$repo_root/scripts/mac"
stub_dir="$scripts_dir/testdata/bin"
SYS_PATH="$PATH"

workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT

state="$workdir/state"
mkdir -p "$state"

export BOOTSTRAP_STUB_STATE="$state"
export STUB_LOG="$workdir/stubs.log"
export IMAGE="ghcr.io/cirruslabs/macos-tahoe-xcode@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
export TART_STUB_IMAGE="$IMAGE"

refresh() { PATH="$stub_dir:$SYS_PATH" "$scripts_dir/image-refresh.sh" "$@"; }

# Tart is already installed on the host (the bootstrap ran).
: >"$state/tart.installed"

# --- dry-run prints actions, changes nothing ---
refresh --dry-run >"$workdir/dry.txt" 2>&1
grep -q 'would pull' "$workdir/dry.txt" || { echo "dry-run missing pull action"; cat "$workdir/dry.txt"; exit 1; }

# --- idle host: pull + verify + prune ---
refresh >"$workdir/run.txt" 2>&1
grep -q 'pin: ' "$workdir/run.txt" || { echo "missing pin line"; cat "$workdir/run.txt"; exit 1; }
grep -q 'prune: caches' "$workdir/run.txt" || { echo "expected prune when idle"; cat "$workdir/run.txt"; exit 1; }
[ -e "$state/tart.pruned" ] || { echo "prune did not run"; exit 1; }
grep -qF 'sudo -n -H -u garm' "$workdir/stubs.log" \
  || { echo "refresh operated on the caller's cache rather than the garm account"; exit 1; }

# --- running VM: prune refused ---
rm -f "$state/tart.pruned"
TART_STUB_RUNNING=1 refresh >"$workdir/run-running.txt" 2>&1
grep -q 'prune: skipped' "$workdir/run-running.txt" || { echo "expected prune refusal while running"; cat "$workdir/run-running.txt"; exit 1; }
[ ! -e "$state/tart.pruned" ] || { echo "prune ran while a VM was running"; exit 1; }

# --- bare tag resolves to a digest pin via crane ---
rm -f "$state/tart.pruned"
refresh --image "ghcr.io/cirruslabs/macos-tahoe-xcode:26" >"$workdir/tag.txt" 2>&1
grep -q 'pin: .*@sha256:' "$workdir/tag.txt" || { echo "bare tag was not pinned by digest"; cat "$workdir/tag.txt"; exit 1; }

echo "test-image-refresh: ok"
