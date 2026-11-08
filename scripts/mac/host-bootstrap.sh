#!/usr/bin/env bash
# Idempotent host bootstrap for a garm-provider-tart Mac mini.
#
# Makes a fresh host ready for the provider in one pass:
#   1. create the garm user and install the controller's authorized key;
#   2. enable Remote Login (SSH) and restrict it to garm (+ the operator);
#   3. enable auto-login for the garm user (GUI session + login keychain);
#   4. install and load the keychain-unlock LaunchDaemon (boot ordering: a
#      LaunchDaemon loads before any user session, so the keychain is unlocked
#      before the provider can start a VM);
#   5. `pmset -a sleep 0 disablesleep 1 autorestart 1`;
#   6. disable automatic OS updates;
#   7. install Tart (Homebrew);
#   8. pull the pinned golden image as the garm user;
#   9. create the owner-only host registry for tart-hostctl;
#  10. install a `tart prune` LaunchDaemon for the garm user's caches;
#  11. write the /etc/garm-provider-tart.toml fragment for this host.
#
# Every step is check-then-act: a second run makes no changes and exits 0.
# `--dry-run` prints the actions and changes nothing. A step that cannot be
# applied fails with a message naming the step, not a stack trace.
#
# No secret is written by this script. The SSH public key, the auto-login
# password and the keychain password are referenced by path (GARM_SSH_KEY,
# GARM_PW_FILE, KEYCHAIN_PW_FILE); only the OS-standard obfuscated kcpassword
# blob is generated from the password, never the plaintext.
#
# Usage:
#   scripts/mac/host-bootstrap.sh            # apply everything (as the admin user)
#   scripts/mac/host-bootstrap.sh --dry-run  # print actions, change nothing
#
# Run as the admin user with passwordless sudo — NOT under `sudo`. Homebrew
# refuses to run as root, and root's default PATH omits /opt/homebrew/bin, so a
# root shell breaks the unprivileged Tart install. Privileged steps use sudo
# internally (require_sudo).
#
# Configuration is via environment variables; see scripts/mac/lib.sh.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROG="host-bootstrap"
# shellcheck source=scripts/mac/lib.sh
. "$SCRIPT_DIR/lib.sh"

usage() {
  cat <<EOF
Usage: $PROG [--dry-run]

Idempotent one-pass setup of a Mac mini for the garm-provider-tart provider.

Options:
  --dry-run   print the actions that would be taken and change nothing

Configuration (environment, with defaults from scripts/mac/lib.sh):
  GARM_USER, GARM_SSH_KEY, GARM_PW_FILE, KEYCHAIN_PW_FILE, IMAGE,
  HOST_NAME, HOST_ADDRESS, PRUNE_OLDER_THAN, LAUNCHD_DIR, KCPASSWORD_PATH,
  GARM_CONFIG_PATH, WRAPPER_PATH, TART_FLEET_ROOT
EOF
}

DRY_RUN=0
for arg in "$@"; do
  case "$arg" in
    --dry-run) DRY_RUN=1 ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown argument: $arg" ;;
  esac
done

# --- apply functions (each guarded, each names its step on failure) ---------

apply_user() {
  local tmp
  require_sudo "user"
  [ -r "$GARM_SSH_KEY" ] || die "user: SSH public key not found at $GARM_SSH_KEY (it is referenced by path, not embedded)"
  if ! id "$GARM_USER" >/dev/null 2>&1; then
    sudo sysadminctl -addUser "$GARM_USER" -fullName "GARM provider" -shell /bin/bash -home "$GARM_HOME" >/dev/null \
      || die "user: failed to create user $GARM_USER"
  fi
  sudo install -d -m 700 -o "$GARM_USER" -g staff "$GARM_HOME/.ssh" \
    || die "user: failed to create $GARM_HOME/.ssh"
  # Append the controller key only when the exact line is absent. authorized_keys
  # is read-modify-written via a temp file, never replaced, so any pre-existing
  # keys survive (attack #4: no destructive scope).
  if ! sudo -n grep -qxF "$(cat "$GARM_SSH_KEY")" "$AUTH_KEYS" 2>/dev/null; then
    tmp="$(mktemp)"
    { sudo -n cat "$AUTH_KEYS" 2>/dev/null || :; cat "$GARM_SSH_KEY"; } >"$tmp"
    sudo install -m 600 -o "$GARM_USER" -g staff "$tmp" "$AUTH_KEYS" \
      || { rm -f "$tmp"; die "user: failed to install authorized key"; }
    rm -f "$tmp"
  fi
}

apply_remote_login() {
  require_sudo "remote_login"
  local operator
  operator="$(id -un)"
  sudo systemsetup -setremotelogin on >/dev/null \
    || die "remote_login: failed to enable Remote Login"
  # Restrict SSH to garm (and the operator) via com.apple.access_ssh: once the
  # group has members, only those members (plus root) may log in. An absent or
  # empty group means every local user can SSH.
  sudo dseditgroup -o create -r "Remote Login access" com.apple.access_ssh >/dev/null 2>&1 || true
  sudo dseditgroup -o edit -a "$GARM_USER" -t user com.apple.access_ssh >/dev/null \
    || die "remote_login: failed to grant $GARM_USER SSH access"
  if [ -n "$operator" ] && [ "$operator" != "root" ] && [ "$operator" != "$GARM_USER" ]; then
    sudo dseditgroup -o edit -a "$operator" -t user com.apple.access_ssh >/dev/null \
      || die "remote_login: failed to grant $operator SSH access"
  fi
}

apply_auto_login() {
  require_sudo "auto_login"
  [ -r "$GARM_PW_FILE" ] || die "auto_login: password file not found at $GARM_PW_FILE (set GARM_PW_FILE to a root-only 0600 file, or enable auto-login via System Settings/MDM)"
  local pw
  pw="$(sudo cat "$GARM_PW_FILE")"
  [ -n "$pw" ] || die "auto_login: password file $GARM_PW_FILE is empty"
  sudo defaults write /Library/Preferences/com.apple.loginwindow autoLoginUser "$GARM_USER" \
    || die "auto_login: failed to set autoLoginUser"
  kcpassword_bytes "$pw" | sudo tee "$KCPASSWORD_PATH" >/dev/null \
    || die "auto_login: failed to write $KCPASSWORD_PATH"
  sudo chmod 600 "$KCPASSWORD_PATH" || die "auto_login: failed to chmod $KCPASSWORD_PATH"
}

apply_keychain_daemon() {
  require_sudo "keychain_daemon"
  "$SCRIPT_DIR/keychain-unlock-daemon.sh" \
    --runner-user "$GARM_USER" \
    --keychain-pw-file "$KEYCHAIN_PW_FILE" \
    --launchd-dir "$LAUNCHD_DIR" \
    --wrapper-path "$WRAPPER_PATH" \
    --label "$KEYCHAIN_DAEMON_LABEL" \
    || die "keychain_daemon: failed to install the unlock LaunchDaemon"
}

apply_power() {
  require_sudo "power"
  sudo pmset -a sleep 0 disablesleep 1 autorestart 1 >/dev/null \
    || die "power: failed to disable sleep"
}

apply_updates() {
  require_sudo "updates"
  sudo softwareupdate --schedule off >/dev/null \
    || die "updates: failed to disable the automatic-update schedule"
  sudo defaults write /Library/Preferences/com.apple.SoftwareUpdate AutomaticCheckEnabled -bool false \
    || die "updates: failed to disable AutomaticCheckEnabled"
  sudo defaults write /Library/Preferences/com.apple.SoftwareUpdate AutomaticDownload -bool false \
    || die "updates: failed to disable AutomaticDownload"
  sudo defaults write /Library/Preferences/com.apple.SoftwareUpdate CriticalUpdateInstall -bool false \
    || die "updates: failed to disable CriticalUpdateInstall"
}

apply_tart() {
  local brew_bin
  brew_bin="$(resolve_bin brew)" || die "tart: Homebrew not found (install Homebrew first, then re-run)"
  "$brew_bin" install openai/tools/tart >/dev/null || die "tart: Homebrew failed to install Tart"
}

apply_image() {
  require_sudo "image"
  tart_as_runner pull "$IMAGE" || die "image: failed to pull $IMAGE as $GARM_USER"
}

apply_registry() {
  require_sudo "registry"
  case "$TART_FLEET_ROOT" in
    /*) ;;
    *) die "registry: TART_FLEET_ROOT must be an absolute path" ;;
  esac
  [ "$TART_FLEET_ROOT" != / ] && [ ! -L "$TART_FLEET_ROOT" ] \
    || die "registry: refusing unsafe path $TART_FLEET_ROOT"
  sudo install -d -m 700 -o "$GARM_USER" -g staff "$TART_FLEET_ROOT" \
    || die "registry: failed to create owner-only $TART_FLEET_ROOT"
}

apply_prune_timer() {
  require_sudo "prune_timer"
  local tart_bin plist
  tart_bin="$(resolve_bin tart)" || die "prune_timer: tart not found (install Tart first)"
  plist="$LAUNCHD_DIR/$PRUNE_DAEMON_LABEL.plist"
  sudo install -d -m 755 "$LAUNCHD_DIR" || die "prune_timer: failed to create $LAUNCHD_DIR"
  sudo tee "$plist" >/dev/null <<EOF || die "prune_timer: failed to write $plist"
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>$PRUNE_DAEMON_LABEL</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/bin/sudo</string>
    <string>-n</string>
    <string>-H</string>
    <string>-u</string>
    <string>$GARM_USER</string>
    <string>$tart_bin</string>
    <string>prune</string>
    <string>--entries</string>
    <string>caches</string>
    <string>--older-than</string>
    <string>$PRUNE_OLDER_THAN</string>
  </array>
  <key>StartCalendarInterval</key>
  <dict>
    <key>Hour</key>
    <integer>4</integer>
    <key>Minute</key>
    <integer>0</integer>
  </dict>
  <key>RunAtLoad</key>
  <false/>
</dict>
</plist>
EOF
  sudo chmod 644 "$plist" || die "prune_timer: failed to chmod $plist"
  sudo launchctl bootstrap system "$plist" 2>/dev/null || sudo launchctl load -w "$plist" \
    || die "prune_timer: failed to load $plist"
}

apply_config() {
  require_sudo "config"
  [ -n "$HOST_ADDRESS" ] || die "config: HOST_ADDRESS is empty (set it to this host's reachable IP/DNS address)"
  sudo tee "$GARM_CONFIG_PATH" >/dev/null <<EOF || die "config: failed to write $GARM_CONFIG_PATH"
$(render_config_fragment)
EOF
}

# --- driver ----------------------------------------------------------------

STEPS="user remote_login auto_login keychain_daemon power updates tart image registry prune_timer config"

run_steps() {
  local name pending=0 skipped=0
  for name in $STEPS; do
    if "check_$name"; then
      log "skip  $name (already configured)"
      skipped=$((skipped + 1))
    else
      pending=$((pending + 1))
      if [ "$DRY_RUN" -eq 1 ]; then
        log "would $name"
      else
        log "apply $name"
        "apply_$name"
      fi
    fi
  done
  if [ "$DRY_RUN" -eq 1 ]; then
    log "summary: $pending would be applied, $skipped already configured"
  else
    log "summary: $pending applied, $skipped already configured"
  fi
}

run_steps
