#!/usr/bin/env bash
# shellcheck shell=bash
# Shared configuration, helpers and read-only checks for the macOS host scripts.
# Sourced by host-bootstrap.sh, host-check.sh and image-refresh.sh.
#
# The check_* functions are the single source of truth for "is this host step
# already done". host-bootstrap.sh uses them to skip already-applied steps
# (idempotency); host-check.sh uses them to build the read-only health report.
# They are all read-only and never prompt for a password (sudo -n).
#
# Design notes (see /home/ubuntu/tmp/garm-macos/DESIGN.md for the contract):
#   * one unprivileged service user (garm) accepts SSH and runs tart-hostctl/tart;
#   * macOS 15+ refuses to start VMs while the login keychain is locked, so the
#     host unlocks it at boot via auto-login and a root LaunchDaemon (belt and
#     braces) — the daemon loads at boot before the provider can start a VM;
#   * no secret is ever written by these scripts: the SSH public key, the
#     auto-login password and the keychain password are all referenced by path.

# --- configuration (all overridable via the environment) --------------------

GARM_USER="${GARM_USER:-garm}"
GARM_HOME="${GARM_HOME:-/Users/$GARM_USER}"
GARM_SSH_KEY="${GARM_SSH_KEY:-/etc/garm/id_ed25519.pub}"
GARM_PW_FILE="${GARM_PW_FILE:-/var/root/.garm-pw}"
KEYCHAIN_PW_FILE="${KEYCHAIN_PW_FILE:-/var/root/.keychain-pw}"
IMAGE="${IMAGE:-ghcr.io/cirruslabs/macos-tahoe-xcode:26}"
HOST_NAME="${HOST_NAME:-$(hostname)}"
HOST_ADDRESS="${HOST_ADDRESS:-}"
PRUNE_OLDER_THAN="${PRUNE_OLDER_THAN:-14}"
LAUNCHD_DIR="${LAUNCHD_DIR:-/Library/LaunchDaemons}"
WRAPPER_PATH="${WRAPPER_PATH:-/usr/local/bin/garm-keychain-unlock}"
KCPASSWORD_PATH="${KCPASSWORD_PATH:-/etc/kcpassword}"
GARM_CONFIG_PATH="${GARM_CONFIG_PATH:-/etc/garm-provider-tart.toml}"
TART_FLEET_ROOT="${TART_FLEET_ROOT:-/var/db/tart-fleet}"
KEYCHAIN_DAEMON_LABEL="${KEYCHAIN_DAEMON_LABEL:-com.garm.keychain-unlock}"
PRUNE_DAEMON_LABEL="${PRUNE_DAEMON_LABEL:-com.garm.tart-prune}"

AUTH_KEYS="$GARM_HOME/.ssh/authorized_keys"

# --- helpers ----------------------------------------------------------------

die() { echo "${PROG:-garm-mac}: $1" >&2; exit 1; }
log() { echo "${PROG:-garm-mac}: $1"; }

# require_command <bin> <step> — fail naming the step when a tool is missing.
require_command() {
  command -v "$1" >/dev/null 2>&1 || die "$2: $1 not found on PATH"
}

# require_sudo <step> — fail naming the step when root/sudo is unavailable.
require_sudo() {
  [ "$(id -u)" = "0" ] && return 0
  sudo -n true 2>/dev/null || die "$1: sudo required (run as root or with passwordless sudo)"
}

# resolve_bin <name> — echo the absolute path to a tool: first via PATH, then in
# the Homebrew locations (Apple Silicon /opt/homebrew/bin, then Intel
# /usr/local/bin). These scripts run over SSH where PATH is minimal, and root's
# default PATH omits /opt/homebrew/bin, so a bare `command -v` is not enough.
# Emits nothing and returns non-zero when the tool is absent.
resolve_bin() {
  local name="$1" p
  if command -v "$name" >/dev/null 2>&1; then
    command -v "$name"
    return 0
  fi
  for p in "/opt/homebrew/bin/$name" "/usr/local/bin/$name"; do
    if [ -x "$p" ]; then printf '%s\n' "$p"; return 0; fi
  done
  return 1
}

# json_string escapes a value for embedding in a JSON string. Pure bash for
# macOS /bin/bash 3.2 (no jq, no associative arrays).
json_string() {
  local value="$1"
  value=${value//\\/\\\\}
  value=${value//\"/\\\"}
  value=${value//$'\n'/\\n}
  value=${value//$'\r'/\\r}
  value=${value//$'\t'/\\t}
  printf '%s' "$value"
}

# tart_has_image <ref> — true when `tart list` shows an image whose name
# contains the ref (the digest when @sha256:-pinned, else the full name).
tart_as_runner() {
  local tart_bin
  tart_bin="$(resolve_bin tart)" || return 127
  # Tart's image and VM stores default to ~/.tart: use the same account as
  # the SSH-invoked tart-hostctl for every list, pull, and prune operation.
  sudo -n -H -u "$GARM_USER" "$tart_bin" "$@"
}

tart_has_image() {
  local ref="$1" needle="${1##*@}"
  [ "$needle" = "$ref" ] && needle="$ref"
  tart_as_runner list --format json 2>/dev/null | grep -qF "$needle"
}

# vm_running — true when any VM is in a running state.
vm_running() {
  tart_as_runner list --format json 2>/dev/null | grep -Eq '"State"[^,]*"running"'
}

# kcpassword_bytes <password> — encode the auto-login password using the classic
# 11-byte XOR key, NUL-padded to 12 bytes. Pure bash (macOS /bin/bash 3.2). The
# password is ASCII; a single-byte encoding is the operating system's own
# convention. The result is the obfuscated /etc/kcpassword form, not plaintext.
kcpassword_bytes() {
  local pw="$1" c ord out="" k i
  for ((i = 0; i < 12; i++)); do
    c="${pw:i:1}"
    if [ -z "$c" ]; then
      ord=0
    else
      ord=$(printf '%d' "'$c")
    fi
    case $((i % 11)) in
      0) k=125 ;; 1) k=137 ;; 2) k=82 ;; 3) k=35 ;; 4) k=210 ;;
      5) k=188 ;; 6) k=221 ;; 7) k=234 ;; 8) k=163 ;; 9) k=185 ;; 10) k=31 ;;
    esac
    printf -v c '\\%03o' "$((ord ^ k))"
    out+="$c"
  done
  printf '%b' "$out"
}

# render_config_fragment — the /etc/garm-provider-tart.toml fragment for this
# host: a single [[hosts]] block the operator copies into the controller config.
# Contains no secrets (the SSH key is referenced by path on the controller).
render_config_fragment() {
  cat <<EOF
# garm-provider-tart host fragment — append this [[hosts]] block to the
# controller's provider config. Written by host-bootstrap.sh; contains no
# secrets (the SSH key is referenced by path on the controller).
[[hosts]]
name = "$HOST_NAME"
address = "$HOST_ADDRESS"
EOF
}

# --- read-only checks (idempotency predicates) ------------------------------

check_user() {
  id "$GARM_USER" >/dev/null 2>&1 || return 1
  [ -f "$AUTH_KEYS" ] || return 1
  [ -r "$GARM_SSH_KEY" ] || return 1
  sudo -n grep -qxF "$(cat "$GARM_SSH_KEY")" "$AUTH_KEYS" 2>/dev/null
}

check_remote_login() {
  sudo -n systemsetup -getremotelogin 2>/dev/null | grep -q "Remote Login: On" || return 1
  # SSH must be restricted to garm via com.apple.access_ssh, not open to every
  # local user. checkmember prints "yes ... is a member of ..." when garm is in
  # the group; an absent group or missing membership prints "no".
  sudo -n dseditgroup -o checkmember -m "$GARM_USER" com.apple.access_ssh 2>/dev/null | grep -q "is a member of"
}

check_auto_login() {
  [ "$(sudo -n defaults read /Library/Preferences/com.apple.loginwindow autoLoginUser 2>/dev/null)" = "$GARM_USER" ] || return 1
  [ -e "$KCPASSWORD_PATH" ] || return 1
}

check_keychain_daemon() {
  [ -f "$LAUNCHD_DIR/$KEYCHAIN_DAEMON_LABEL.plist" ] || return 1
  launchctl list 2>/dev/null | grep -qF "$KEYCHAIN_DAEMON_LABEL"
}

check_power() {
  local out
  out="$(sudo -n pmset -g 2>/dev/null || true)"
  printf '%s\n' "$out" | grep -Eq '^[[:space:]]*sleep[[:space:]]+0([[:space:]]|$)' || return 1
  printf '%s\n' "$out" | grep -Eq '^[[:space:]]*disablesleep[[:space:]]+1([[:space:]]|$)' || return 1
  printf '%s\n' "$out" | grep -Eq '^[[:space:]]*autorestart[[:space:]]+1([[:space:]]|$)' || return 1
}

check_updates() {
  sudo -n softwareupdate --schedule 2>/dev/null | grep -qi "Automatic check is off" || return 1
  [ "$(sudo -n defaults read /Library/Preferences/com.apple.SoftwareUpdate AutomaticCheckEnabled 2>/dev/null)" = "false" ] || return 1
  [ "$(sudo -n defaults read /Library/Preferences/com.apple.SoftwareUpdate AutomaticDownload 2>/dev/null)" = "false" ] || return 1
  [ "$(sudo -n defaults read /Library/Preferences/com.apple.SoftwareUpdate CriticalUpdateInstall 2>/dev/null)" = "false" ] || return 1
}

check_tart() {
  command -v tart >/dev/null 2>&1 && tart --version >/dev/null 2>&1
}

check_image() {
  tart_has_image "$IMAGE"
}

check_registry() {
  [ -d "$TART_FLEET_ROOT" ] && [ ! -L "$TART_FLEET_ROOT" ] || return 1
  local mode
  case "$(uname -s)" in
    Darwin) mode="$(stat -f '%Lp' "$TART_FLEET_ROOT" 2>/dev/null)" ;;
    *) mode="$(stat -c '%a' "$TART_FLEET_ROOT" 2>/dev/null)" ;;
  esac
  [ "$mode" = "700" ] || return 1
  sudo -n -H -u "$GARM_USER" test -w "$TART_FLEET_ROOT" 2>/dev/null
}

check_prune_timer() {
  [ -f "$LAUNCHD_DIR/$PRUNE_DAEMON_LABEL.plist" ] || return 1
  launchctl list 2>/dev/null | grep -qF "$PRUNE_DAEMON_LABEL"
}

check_config() {
  [ -f "$GARM_CONFIG_PATH" ] || return 1
  [ "$(cat "$GARM_CONFIG_PATH")" = "$(render_config_fragment)" ]
}
