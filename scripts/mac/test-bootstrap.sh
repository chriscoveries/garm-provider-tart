#!/usr/bin/env bash
# Idempotency test for host-bootstrap.sh, driven by the stub binaries under
# testdata/bin on Linux (no Mac, no Tart). Exercises the dry-run path (no
# changes), a first apply, a second run that must apply nothing, and the
# host-check.sh --json report. Negative paths: missing sudo and missing
# Homebrew each fail naming the step.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
scripts_dir="$repo_root/scripts/mac"
stub_dir="$scripts_dir/testdata/bin"
SYS_PATH="$PATH"

workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT

state="$workdir/state"
mkdir -p "$workdir/Users" "$workdir/root" "$workdir/etc"

IMAGE="ghcr.io/cirruslabs/macos-tahoe-xcode@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

# Fake inputs, all referenced by path (no secrets embedded anywhere).
printf 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKE garm-test\n' >"$workdir/id_ed25519.pub"
printf 'runner-password\n' >"$workdir/root/garm-pw"
printf 'runner-password\n' >"$workdir/root/keychain-pw"

export BOOTSTRAP_STUB_STATE="$state"
export STUB_LOG="$workdir/stubs.log"
export GARM_USER="garm"
export GARM_HOME="$workdir/Users/garm"
export GARM_SSH_KEY="$workdir/id_ed25519.pub"
export GARM_PW_FILE="$workdir/root/garm-pw"
export KEYCHAIN_PW_FILE="$workdir/root/keychain-pw"
export IMAGE="$IMAGE"
export TART_STUB_IMAGE="$IMAGE"
export HOST_NAME="mac-mini-test"
export HOST_ADDRESS="10.20.0.11"
export LAUNCHD_DIR="$workdir/Library/LaunchDaemons"
export KCPASSWORD_PATH="$workdir/etc/kcpassword"
export GARM_CONFIG_PATH="$workdir/etc/garm-provider-tart.toml"
export TART_FLEET_ROOT="$workdir/var/db/tart-fleet"
export WRAPPER_PATH="$workdir/usr/local/bin/garm-keychain-unlock"

bootstrap() { PATH="$stub_dir:$SYS_PATH" "$scripts_dir/host-bootstrap.sh" "$@"; }
host_check() { PATH="$stub_dir:$SYS_PATH" "$scripts_dir/host-check.sh" "$@"; }

# A pre-existing operator key the bootstrap must preserve: authorized_keys is
# appended, never replaced (attack #4: no destructive scope).
mkdir -p "$workdir/Users/garm/.ssh"
printf '%s\n' 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEXISTINGEXISTINGEXISTINGEXISTINGEXISTING operator@old' \
  >"$workdir/Users/garm/.ssh/authorized_keys"

# --- dry-run: prints actions, changes nothing ---
bootstrap --dry-run >"$workdir/dry-run.txt" 2>&1
would="$(grep -c '^host-bootstrap: would ' "$workdir/dry-run.txt" || true)"
[ "$would" -gt 0 ] || { echo "dry-run emitted no 'would' actions"; cat "$workdir/dry-run.txt"; exit 1; }
if [ -d "$state" ] && [ -n "$(find "$state" -type f 2>/dev/null)" ]; then
  echo "dry-run mutated stub state"; find "$state" -type f; exit 1
fi
echo "dry-run: $would actions, state untouched"

# --- first run: applies every pending step ---
bootstrap >"$workdir/run1.txt" 2>&1
apply1="$(grep -c '^host-bootstrap: apply ' "$workdir/run1.txt" || true)"
[ "$apply1" -eq "$would" ] || { echo "first run applied $apply1, expected $would"; cat "$workdir/run1.txt"; exit 1; }

# --- the pre-existing key survived the append, and the controller key landed ---
grep -qxF 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEXISTINGEXISTINGEXISTINGEXISTINGEXISTING operator@old' \
  "$workdir/Users/garm/.ssh/authorized_keys" \
  || { echo "pre-existing authorized key was dropped"; cat "$workdir/Users/garm/.ssh/authorized_keys"; exit 1; }
grep -qxF "$(cat "$workdir/id_ed25519.pub")" "$workdir/Users/garm/.ssh/authorized_keys" \
  || { echo "controller key was not appended"; cat "$workdir/Users/garm/.ssh/authorized_keys"; exit 1; }

# Tart storage is per-user. The bootstrap must pull via the garm account and
# give that same account an owner-only registry directory for tart-hostctl.
grep -qF 'sudo -n -H -u garm' "$workdir/stubs.log" \
  || { echo "Tart operations never switched to the garm account"; cat "$workdir/stubs.log"; exit 1; }
[ "$(stat -c '%a' "$TART_FLEET_ROOT")" = "700" ] \
  || { echo "registry directory must be mode 700"; exit 1; }
[ -d "$TART_FLEET_ROOT" ] || { echo "registry directory was not created"; exit 1; }
grep -q '<string>garm</string>' "$LAUNCHD_DIR/com.garm.tart-prune.plist" \
  || { echo "prune daemon did not target garm's Tart cache"; exit 1; }

# --- host-check with stubs: every step ok ---
host_check --json >"$workdir/check.json" 2>&1
jq -e 'has("steps") and .ok == true and ([.steps[].ok] | all)' "$workdir/check.json" >/dev/null \
  || { echo "host-check did not report all-ok after apply"; cat "$workdir/check.json"; exit 1; }

# --- secret hygiene: the password and any private key never land in a written file ---
# The input password files live under $workdir/root (referenced by path), so only
# the script's output locations are scanned.
if grep -Rq "runner-password" "$workdir/Library" "$workdir/etc" "$workdir/usr" "$workdir/Users" 2>/dev/null; then
  echo "secret leak: the password value was written by the script"; exit 1
fi
if grep -Rq "PRIVATE KEY" "$workdir/Library" "$workdir/etc" "$workdir/usr" "$workdir/Users" 2>/dev/null; then
  echo "secret leak: private key material was written by the script"; exit 1
fi

# --- keychain wrapper: password must go over stdin, never via `-p` (argv) ---
if grep -q 'unlock-keychain -p' "$WRAPPER_PATH"; then
  echo "secret leak: keychain wrapper passes the password via -p (ps-visible)"; exit 1
fi
grep -q 'security unlock-keychain' "$WRAPPER_PATH" \
  || { echo "keychain wrapper missing security unlock-keychain"; exit 1; }
grep -q 'printf.*|.*security unlock-keychain' "$WRAPPER_PATH" \
  || { echo "keychain wrapper does not pipe the password on stdin"; exit 1; }
[ "$(stat -c '%a' "$WRAPPER_PATH")" = "700" ] \
  || { echo "keychain wrapper is not mode 700"; exit 1; }

# --- second run: applies nothing (idempotent) ---
bootstrap >"$workdir/run2.txt" 2>&1
apply2="$(grep -c '^host-bootstrap: apply ' "$workdir/run2.txt" || true)"
skip2="$(grep -c '^host-bootstrap: skip ' "$workdir/run2.txt" || true)"
[ "$apply2" -eq 0 ] || { echo "second run re-applied $apply2 step(s); not idempotent"; cat "$workdir/run2.txt"; exit 1; }
[ "$skip2" -eq "$would" ] || { echo "second run skipped $skip2, expected $would"; cat "$workdir/run2.txt"; exit 1; }

# --- negative: missing sudo fails naming the step ---
rm -rf "$state"
if STUB_SUDO_DENY=1 bootstrap >"$workdir/nosudo.txt" 2>&1; then
  echo "expected failure without sudo"; exit 1
fi
grep -q 'host-bootstrap: user: sudo required' "$workdir/nosudo.txt" \
  || { echo "missing sudo did not name the step"; cat "$workdir/nosudo.txt"; exit 1; }

# --- negative: missing Homebrew fails naming the step ---
rm -rf "$state"
nobrew_dir="$workdir/nobrew-bin"
mkdir -p "$nobrew_dir"
for f in "$stub_dir"/*; do
  [ "$(basename "$f")" = "brew" ] && continue
  ln -s "$f" "$nobrew_dir/"
done
if PATH="$nobrew_dir:$SYS_PATH" "$scripts_dir/host-bootstrap.sh" >"$workdir/nobrew.txt" 2>&1; then
  echo "expected failure without Homebrew"; exit 1
fi
grep -q 'host-bootstrap: tart: Homebrew not found' "$workdir/nobrew.txt" \
  || { echo "missing Homebrew did not name the step"; cat "$workdir/nobrew.txt"; exit 1; }

echo "test-bootstrap: ok ($would steps; dry-run clean, apply once, second run empty)"
