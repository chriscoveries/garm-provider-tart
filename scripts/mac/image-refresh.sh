#!/usr/bin/env bash
# Keep the host's golden image current: pin by digest, pull, verify, and prune
# stale caches. Never prunes VMs, and refuses to prune while a VM is running
# (the image is then in use).
#
# The provider clones VMs from the golden image. This script is the only thing
# that touches the image on the host, and it is deliberately conservative:
#   * the image is pinned by digest so a moved tag cannot silently change what
#     the fleet clones;
#   * `tart prune --entries caches` reclaims OCI/IPSW download caches only —
#     `--entries vms` is never issued (it would delete live VMs);
#   * when any VM is running, pruning is refused rather than risk reclaiming a
#     layer a live VM still depends on.
#
# Usage:
#   scripts/mac/image-refresh.sh --image ghcr.io/cirruslabs/macos-tahoe-xcode@sha256:...
#   scripts/mac/image-refresh.sh --dry-run
#
# IMAGE may be a digest-pinned reference (preferred) or a bare tag, in which
# case `crane digest` is used to resolve the pin (CRANE, default `crane`).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROG="image-refresh"
# shellcheck source=scripts/mac/lib.sh
. "$SCRIPT_DIR/lib.sh"

CRANE="${CRANE:-crane}"

usage() {
  cat <<EOF
Usage: $PROG [--dry-run] [--image <ref>]

Pins the golden image by digest, pulls it, verifies it appears in tart list,
and prunes stale caches (never VMs). Refuses to prune while a VM is running.

Environment: IMAGE, PRUNE_OLDER_THAN, CRANE, GARM_USER.
EOF
}

DRY_RUN=0
while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) DRY_RUN=1 ;;
    --image) IMAGE="$2"; shift ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
  shift
done

# pin_image <ref> -> digest-pinned ref. Fails naming the step when a bare tag
# cannot be resolved (no crane / no network).
pin_image() {
  case "$1" in
    *@sha256:*) printf '%s\n' "$1" ;;
    *)
      command -v "$CRANE" >/dev/null 2>&1 \
        || die "pin: $1 is not digest-pinned and $CRANE is unavailable to resolve it (set IMAGE to a @sha256: reference)"
      local digest
      digest="$("$CRANE" digest "$1" 2>/dev/null)" || die "pin: failed to resolve digest for $1"
      [ -n "$digest" ] || die "pin: empty digest resolved for $1"
      printf '%s@%s\n' "$1" "$digest"
      ;;
  esac
}

main() {
  if [ "$DRY_RUN" -eq 1 ]; then
    log "would pin $IMAGE by digest"
    log "would pull $IMAGE"
    log "would verify $IMAGE in tart list"
    log "would prune caches older than ${PRUNE_OLDER_THAN}d (never VMs)"
    return 0
  fi

  require_sudo "pull"

  local pinned
  pinned="$(pin_image "$IMAGE")"
  log "pin: $pinned"

  log "pull: $pinned"
  tart_as_runner pull "$pinned" || die "pull: failed to pull $pinned as $GARM_USER"

  log "verify: $pinned"
  tart_has_image "$pinned" || die "verify: $pinned not present in tart list after pull"

  if vm_running; then
    log "prune: skipped — a VM is running (refusing to prune while an image is in use)"
  else
    log "prune: caches older than ${PRUNE_OLDER_THAN}d"
    tart_as_runner prune --entries caches --older-than "$PRUNE_OLDER_THAN" || die "prune: failed"
  fi
}

main
