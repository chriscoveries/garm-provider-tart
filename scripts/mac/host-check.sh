#!/usr/bin/env bash
# Read-only health report for a prepared garm-provider-tart Mac host.
#
# Re-runs the same checks host-bootstrap.sh uses (scripts/mac/lib.sh) and emits
# a per-step verdict. It never changes anything: --dry-run is accepted for
# symmetry but has no effect. Used by the real-hardware witness (T7) and the
# fleet sweep to confirm a host is ready before it joins a pool.
#
# Output:
#   scripts/mac/host-check.sh --json       one JSON object with a "steps" array
#   scripts/mac/host-check.sh              human-readable table
#
# The exit code is always 0; read the "ok" field for the verdict (a healthy
# host has "ok": true and every step "ok": true).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROG="host-check"
# shellcheck source=scripts/mac/lib.sh
. "$SCRIPT_DIR/lib.sh"

usage() {
  cat <<EOF
Usage: $PROG [--json] [--dry-run]

Read-only health report for a prepared Mac host. --json emits one JSON object
with a "steps" array; without it a human-readable table is printed.
EOF
}

JSON=0
for arg in "$@"; do
  case "$arg" in
    --json) JSON=1 ;;
    --dry-run) : ;;   # accepted for symmetry; host-check never changes anything
    -h|--help) usage; exit 0 ;;
    *) die "unknown argument: $arg" ;;
  esac
done

NAMES=()
OKS=()
DETAILS=()
ALL_OK="true"

add_step() {
  local name="$1" ok="$2" detail="$3"
  NAMES+=("$name")
  OKS+=("$ok")
  DETAILS+=("$detail")
  [ "$ok" = "true" ] || ALL_OK="false"
}

run_check() {
  local name="$1" detail="$2"
  if "check_$name"; then
    add_step "$name" true "$detail"
  else
    add_step "$name" false "$detail"
  fi
}

run_check user            "garm user exists and authorized key is installed"
run_check remote_login    "Remote Login (SSH) enabled and restricted to $GARM_USER"
run_check auto_login      "auto-login enabled for $GARM_USER"
run_check keychain_daemon "login keychain unlocked at boot via LaunchDaemon"
run_check power           "sleep disabled (pmset sleep 0, disablesleep 1)"
run_check updates         "automatic OS updates disabled"
run_check tart            "Tart installed"
run_check image           "golden image $IMAGE present"
run_check registry        "owner-only tart-hostctl registry writable by $GARM_USER"
run_check prune_timer     "tart prune timer installed"
run_check config          "provider config fragment present"

emit_report() {
  local i status body=""
  if [ "$JSON" -eq 1 ]; then
    for ((i = 0; i < ${#NAMES[@]}; i++)); do
      [ -n "$body" ] && body="$body,"
      body="$body{\"name\":\"${NAMES[i]}\",\"ok\":${OKS[i]},\"detail\":\"$(json_string "${DETAILS[i]}")\"}"
    done
    printf '{"host":"%s","ok":%s,"steps":[%s]}\n' "$HOST_NAME" "$ALL_OK" "$body"
  else
    printf 'host: %s\n' "$HOST_NAME"
    printf '%-14s %-6s %s\n' "STEP" "OK" "DETAIL"
    for ((i = 0; i < ${#NAMES[@]}; i++)); do
      if [ "${OKS[i]}" = "true" ]; then status="ok"; else status="FAIL"; fi
      printf '%-14s %-6s %s\n' "${NAMES[i]}" "$status" "${DETAILS[i]}"
    done
    if [ "$ALL_OK" = "true" ]; then
      printf 'overall: ok\n'
    else
      printf 'overall: FAIL\n'
    fi
  fi
}

emit_report
