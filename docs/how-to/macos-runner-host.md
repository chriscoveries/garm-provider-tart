# Prepare a macOS runner host

How to make a fresh Mac mini ready for the `garm-provider-tart` provider in one idempotent pass, and
keep its golden image current. The provider does not provision hosts: it assumes a prepared host and
fails loudly otherwise. This page is the operator runbook; the design reference is the
`garm-provider-tart` README and the provider contract.

## Prerequisites

| Item | Requirement |
| --- | --- |
| Hardware | Apple Silicon Mac mini (arm64 guests only) |
| OS | macOS 26, up to date at image-build time (automatic updates are then disabled by the bootstrap) |
| Network | Wired Ethernet, static IP or DHCP reservation, reachable from the GARM controller on TCP 22 |
| Outbound | HTTPS to `github.com`, `ghcr.io`, and the GARM callback/metadata URLs |
| Disk | ≥ 25 GB per golden image plus ~40 GB per live VM; keep 20 % free |
| Tooling | Homebrew installed; an admin account with passwordless `sudo` (the bootstrap runs unprivileged and escalates only the steps that need root) |

The controller's public SSH key and two root-only password files must already exist and be referenced
by path — the scripts never read a secret off the command line and never write one:

| File | Contents | Mode |
| --- | --- | --- |
| `GARM_SSH_KEY` (default `/etc/garm/id_ed25519.pub`) | the controller's public key | 0644 |
| `GARM_PW_FILE` (default `/var/root/.garm-pw`) | the `garm` user's login password (for auto-login) | 0600 root |
| `KEYCHAIN_PW_FILE` (default `/var/root/.keychain-pw`) | the same login password (for the keychain daemon) | 0600 root |

On a single-user host `GARM_PW_FILE` and `KEYCHAIN_PW_FILE` are normally the same file.

## Bootstrap

```bash
# Preview what would change (no side effects):
scripts/mac/host-bootstrap.sh --dry-run

# Apply (as the admin user — NOT under sudo; see below):
scripts/mac/host-bootstrap.sh
```

Run the bootstrap as the admin user with passwordless `sudo` — **not** under
`sudo scripts/mac/host-bootstrap.sh`. Homebrew refuses to run as root, and root's default `PATH` omits
`/opt/homebrew/bin`, so a root shell breaks the Tart install step. The privileged steps (user,
Remote Login, auto-login, LaunchDaemons, `pmset`, updates, the config fragment) escalate with `sudo`
internally; only the Homebrew install runs as the invoking admin. Image management runs under the
`garm` account with `sudo -H -u garm`, which keeps Tart's per-user image cache visible to hostctl.
The scripts fail naming the step when passwordless
`sudo` is unavailable.

Each of the eleven steps is check-then-act, so re-running is safe and makes no changes:

1. create the `garm` user and append the controller's authorized key (existing keys are preserved);
2. enable Remote Login (SSH) and restrict it to `garm` (plus the operator) via `com.apple.access_ssh`;
3. enable auto-login for `garm` (GUI session + unlocked login keychain);
4. install and load the keychain-unlock LaunchDaemon;
5. `pmset -a sleep 0 disablesleep 1 autorestart 1`;
6. disable automatic OS updates;
7. install Tart (Homebrew);
8. pull the pinned golden image into the `garm` account's Tart cache;
9. create `/var/db/tart-fleet` mode 0700, owned by `garm`, for hostctl's registry and lock;
10. install a `tart prune` LaunchDaemon that prunes the `garm` account's caches only;
11. write `/etc/garm-provider-tart.toml` — a `[[hosts]]` fragment to copy into the controller config.

Configuration is via environment variables (see `scripts/mac/lib.sh` for defaults); the ones you will
normally set are `HOST_NAME`, `HOST_ADDRESS`, `IMAGE`, `GARM_USER`, and the three paths above.

### Boot ordering

macOS 15+ refuses to start VMs while the login keychain is locked. The keychain is unlocked two ways,
belt and braces: auto-login (step 3) and a root LaunchDaemon (step 4) that runs
`security unlock-keychain` at load. A LaunchDaemon is loaded by launchd at boot before any user
session, so the keychain is unlocked before GARM reaches the host over SSH and starts a VM.

### Security notes

- Remote Login is restricted to `garm` (plus the operator account that ran the bootstrap) via the
  `com.apple.access_ssh` group. `host-check.sh` asserts the restriction, not just that SSH is on.
- The keychain password never reaches a process command line: the unlock daemon reads it from the
  root-only `KEYCHAIN_PW_FILE` and passes it to `security unlock-keychain` on stdin, never via `-p`.
- Inside the guest, the runner-install template passes GARM's instance token to `curl` through a
  0600 header file (`-H @file`) and writes JIT credential files 0600 owner-only, so neither the
  token nor the JIT RSA key material is world-readable or `ps`-visible. The non-JIT (registration
  token) path reads the token from a 0600 file at run time and deletes it immediately after
  `config.sh`; `config.sh` itself only accepts the token via `--token`, so it is briefly visible on
  that one process's argv — a documented residual risk. Prefer the JIT path (the default), which
  uses no registration token at all.

## Verify

```bash
scripts/mac/host-check.sh --json | jq -e '.ok == true'
```

`host-check.sh` is read-only and re-runs the same checks the bootstrap uses; a healthy host reports
`"ok": true` with every step `"ok": true`. It never changes anything, so `--dry-run` is accepted for
symmetry.

## Image refresh

Keep the golden image pinned and current (pull, verify, prune stale caches):

```bash
scripts/mac/image-refresh.sh --image ghcr.io/cirruslabs/macos-tahoe-xcode@sha256:…   # digest-pinned
scripts/mac/image-refresh.sh --image ghcr.io/cirruslabs/macos-tahoe-xcode:26         # tag → pinned via crane
```

`image-refresh.sh` pins the image by digest (resolving a bare tag with `crane digest`), verifies it
appears in the `garm` account's `tart list`, and prunes that account's OCI/IPSW caches. It never prunes VMs, and it refuses to prune
while a VM is running (the image is then in use).

## Failure → cause

| Symptom | Cause |
| --- | --- |
| host is `unknown`, no runners | SSH key, `garm` user, Remote Login, or firewall |
| `create` refused: keychain locked | auto-login disabled, or unlock daemon not loaded |
| `tart: command not found` | Tart not installed, or not on `garm`'s `PATH` |
| `tart clone` fails | image not pulled / digest missing |
| job dies, then host vanishes | `pmset` sleep not disabled |

## Cleanup and teardown

The bootstrap writes only configuration and an obfuscated auto-login blob (`/etc/kcpassword`); it
never deletes VMs, images, or user data. To drain a host before maintenance, use the hostctl
`drain` verb, not these scripts. Re-imaging a host removes `/var/db/tart-fleet` and `~/.tart`, which
is correct — GARM re-creates VMs from the pool.
