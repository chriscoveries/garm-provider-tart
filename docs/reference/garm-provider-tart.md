# garm-provider-tart — reference

This page is the exhaustive, dry specification of the provider's behaviour as implemented. It documents
what the code does, not what it could do. For how to operate the fleet, see
[docs/how-to/macos-runners.md](../how-to/macos-runners.md); for why the design is shaped this way, see
[docs/adr/](../adr/).

## Provider ID

A `provider_id` is `<host>/<vm-name>`, where `<host>` is the `name` of a `[[hosts]]` entry in the
provider config and `<vm-name>` is the Tart VM name. Example: `mac-mini-01/garm-abc123`.

- The provider splits an instance ID on the first `<host>/` prefix that matches a configured host name.
- A bare instance name (no `<host>/` prefix) is accepted and routed to the first configured host. GARM
  sends the bare name when a create failed before a provider ID existed.

## Configuration

GARM passes the config path in `GARM_PROVIDER_CONFIG_FILE`. The file is TOML. Every key is read from
`internal/provider/config.go`; defaults are applied in `Validate`, except `ssh_bin`, which is resolved in
`internal/provider/ssh.go` `NewClient` (PATH lookup with a `/usr/bin/ssh` fallback).

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `ssh_user` | string | required | unprivileged account used to SSH to each host |
| `ssh_key` | string | empty | path to the private key for `ssh_user`; empty means the SSH agent |
| `known_hosts` | string | empty | path to a `known_hosts` file; empty disables host-key verification (development only) |
| `ssh_bin` | string | `ssh` | the SSH client binary (overridable for the break-glass wrapper and tests) |
| `hostctl_bin` | string | `/usr/local/bin/tart-hostctl` | the `tart-hostctl` binary path on each host |
| `create_timeout_seconds` | int | `900` | end-to-end bound for one `CreateInstance` call |
| `command_timeout_seconds` | int | `30` | bound for one SSH command on a host |
| `probe_timeout_seconds` | int | `3` | bound for one host `status` probe (the whole probe pass runs in parallel) |
| `slots_per_host` | int | `1` | informational planning hint; the host enforces actual capacity via `TART_FLEET_SLOTS_PER_HOST`. Values above `2` are rejected |
| `image` | string | empty | golden image used when the bootstrap payload carries none |
| `default_cpu` | int | `4` | guest CPU count when not overridden |
| `default_memory_mb` | int | `8192` | guest memory when not overridden |
| `default_disk_gb` | int | `50` | guest disk when not overridden |

Each host is a `[[hosts]]` block:

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `name` | string | required | stable identifier recorded in provider IDs; must be unique |
| `address` | string | required | SSH target, host name or IP |
| `port` | int | `22` | SSH port |

Validation fails (exit code `1`) when there are no hosts, `ssh_user` is empty, `slots_per_host` exceeds
`2`, a host lacks `name` or `address`, or two hosts share a `name`.

Minimal config (see `config.example.toml`):

```toml
ssh_user = "garm"
ssh_key = "/etc/garm/id_ed25519"
slots_per_host = 2
image = "ghcr.io/cirruslabs/macos-tahoe-xcode@sha256:<digest>"

[[hosts]]
name = "mac-mini-01"
address = "10.20.0.11"
```

## `extra_specs` schema

The provider reads the `extra_specs` object from the `CreateInstance` stdin `BootstrapInstance`.
The rendered `runner_install_template` is streamed directly into the guest through hostctl's
`bootstrap` verb after the VM reaches ready. The script is absent from SSH command arguments
and host registry records.

| Key | Type | Meaning |
| --- | --- | --- |
| `host` | string | pin placement to a configured host `name` |
| `cpu` | int | per-VM CPU override (falls back to `default_cpu`) |
| `mem` | int | per-VM memory override, in MB (falls back to `default_memory_mb`) |
| `disk` | int | per-VM disk override, in GB (falls back to `default_disk_gb`) |
| `runner_install_template` | string | GARM's rendered runner-install wrapper, executed inside the guest |

When GARM supplies no install template, the provider logs a warning and returns the ready VM
for manual provisioning. Attach the macOS template to the pool for working GitHub runners.

```json
{ "host": "mac-mini-01", "cpu": 4, "mem": 8192, "disk": 50 }
```

## Operations

GARM execs the provider binary once per operation and selects it with `GARM_COMMAND`. Standard output
carries JSON only; diagnostics and metrics go to standard error.

| `GARM_COMMAND` | reads | stdin | stdout |
| --- | --- | --- | --- |
| `CreateInstance` | `GARM_PROVIDER_CONFIG_FILE`, `GARM_CONTROLLER_ID` | one `BootstrapInstance` | one `ProviderInstance` |
| `GetInstance` | `GARM_INSTANCE_ID` | none | one `ProviderInstance` |
| `ListInstances` | `GARM_POOL_ID` | none | JSON array (`[]` when empty) |
| `DeleteInstance` | `GARM_INSTANCE_ID` | none | nothing |
| `RemoveAllInstances` | `GARM_CONTROLLER_ID` | none | nothing |
| `StartInstance` | `GARM_INSTANCE_ID` | none | nothing (not implemented) |
| `StopInstance` | `GARM_INSTANCE_ID` | none | nothing (not implemented) |

GARM also sets `GARM_INTERFACE_VERSION` and `GARM_POOL_EXTRASPECS`; this provider does not read them.

- `CreateInstance` places the VM (see "Placement and tri-state host health"), runs the hostctl `create`
  verb, and prints the mapped `ProviderInstance`. A placement fault is printed as an errored
  `ProviderInstance`; a transport or Tart failure is a hard error (exit `1`).
- `ListInstances` fans out to every host, filters by this controller and the pool (`owner`), deduplicates
  by `provider_id`, and always prints an array (never `null`). Unreachable hosts are skipped with a
  warning, never treated as empty.
- `DeleteInstance` is a no-op (exit `0`) when the VM is already gone, and a hard error when the host is
  unreachable.
- `RemoveAllInstances` deletes every VM carrying this controller's ID on every host, best-effort, and
  reports failures to stderr; it returns exit `0`.
- `StartInstance` and `StopInstance` are not implemented and return exit `1`.

### Exit codes

| Code | Meaning |
| --- | --- |
| `0` | success (including a no-op delete and a best-effort `RemoveAllInstances`) |
| `1` | operation or configuration failure |
| `2` | usage error: `GARM_COMMAND` unset or not a known command |

## JSON shapes

### ProviderInstance (stdout)

```json
{
  "provider_id": "mac-mini-01/garm-abc123",
  "name": "garm-abc123",
  "os_type": "linux",
  "os_name": "macos",
  "os_version": "26",
  "os_arch": "arm64",
  "status": "running",
  "addresses": [{ "address": "192.168.64.10", "type": "private" }]
}
```

- `os_type` is always `linux`: GARM's OS-type model has no darwin value, so macOS is signalled via
  `os_name`/`os_version`/`os_arch`.
- `addresses` is present only when the guest IP is known; the type is `private`.
- `provider_fault` (base64-encoded `[]byte`) is present on error: `status: error` with the fault name
  and message, or a `running` record whose readiness probe failed.

### Status mapping

The hostctl state is mapped to GARM's instance status:

| hostctl state | GARM status |
| --- | --- |
| `ready`, `bootstrapping`, `idle`, `adopted` | `running` |
| `cloning` | `creating` |
| `running`, `failed` | `error` |
| `deleting` | `deleting` |
| anything else | `unknown` |

### CreateInstance stdin (BootstrapInstance)

GARM writes a `BootstrapInstance` object to stdin. The provider reads only `name`, `image`, `pool_id`
(used as the record's owner) and `extra_specs`; the remaining fields (`tools`, `repo_url`, `callback-url`,
`metadata-url`, `instance-token`, `labels`, `os_type`, `arch`, `flavor`, and so on) are consumed by the
runner-install template, not by this binary.

## State machine

The host record moves through the lifecycle in `internal/hostctl/state.go`:

```text
create:  cloning → running → ready → bootstrapping → idle
delete:  deleting → gone
failure: any live state → failed
```

- `cloning` → `running` happens when `tart run` returns.
- `running` → `ready` happens only when `tart exec <name> true` succeeds.
- `ready` → `bootstrapping` → `idle` is driven by the hostctl `bootstrap` verb; `ready` may also move
  straight to `idle` (see `legalTransitions` in `internal/hostctl/state.go`).
- `deleting` → `gone` is a tombstone transition; `gone` is never persisted, it is the state of an absent
  VM and an absent record.
- Reconciliation adds two boot-time outcomes: `adopted` (a VM in `tart list` carrying this controller's
  label but no record) and `quarantined` (an unlabelled VM awaiting the quarantine TTL).
- `deleting` is reachable from any live state.

## Slot semantics and capacity

A host counts its licence slots via `capacity { total, used, adopted, quarantined }`:

- `total` is the host's `TART_FLEET_SLOTS_PER_HOST`, a hard ceiling of `2` (values above `2` are
  rejected). The provider's `slots_per_host` config key is an informational planning hint and does not
  itself set this value.
- `used` counts every live lifecycle record, including `deleting` tombstones.
- `adopted` counts VMs carrying this controller's label but no record.
- `quarantined` counts unlabelled VMs awaiting the quarantine TTL.

`create` is refused with a `capacity` fault when `used + adopted >= total`. A tombstone keeps its slot
counted until the VM is confirmed absent from `tart list`, so a failed delete can never double-book a
slot.

## Placement and tri-state host health

Before a create, the provider probes every host in parallel (bounded by `probe_timeout_seconds`) and
classifies each as one of three states:

| class | condition |
| --- | --- |
| `up` | host answers, not drained, keychain unlocked, at least one free slot |
| `full` | host answers but is drained, or the keychain is locked, or no free slot |
| `unknown` | host unreachable (timeout, auth failure) or its `status` is unreadable |

Placement rules:

- a pinned host (`extra_specs.host`) is used when it is `up`; a pinned `full` host means wait, a pinned
  `unknown` host means no-reachable;
- otherwise the least-loaded `up` host wins (most free slots, then least recently used, then name order);
- `unknown` hosts are dropped from placement — "unknown" is never treated as "empty";
- if every host is `unknown`, the create fails with a `no_reachable_host` fault;
- if some host is `full` but none is `up`, the provider waits for a free slot and re-probes.

## Timeouts

| Bound | Value | Source |
| --- | --- | --- |
| `CreateInstance` end to end | `create_timeout_seconds` (default `900`) | provider config |
| capacity wait before a create | `create_timeout_seconds − 120s` | `defaultWaitMargin` |
| capacity re-probe interval | 5–10 s, jittered | `jitterInterval` |
| one SSH command | `command_timeout_seconds` (default `30`) | provider config |
| guest runner bootstrap | remaining `create_timeout_seconds`, host hard cap 15m | provider and hostctl |
| one host status probe | `probe_timeout_seconds` (default `3`) | provider config |
| SSH connection | `ConnectTimeout=10`, `BatchMode=yes` | `internal/provider/ssh.go` |
| readiness probe (`tart exec true`) | `60s` | `TART_FLEET_PROBE_TIMEOUT_SECONDS` |
| stale create reclaim | `5m` | `TART_FLEET_STALE_CREATE_TIMEOUT` |
| quarantine TTL | `24h` | `TART_FLEET_QUARANTINE_TTL_HOURS` |
| record lease TTL | `900s` (informational) | `defaultLeaseTTLSeconds` |

## Failure and cleanup behaviour

- **Create, transport/Tart failure.** The provider runs a best-effort cleanup `delete`, then returns a
  hard error; GARM retries the create (up to its own attempt limit). The host-side reaper and adoption
  cover the case where the cleanup delete cannot run because the host is unreachable.
- **Create, placement fault.** `no_reachable_host` or `capacity` is returned as an errored
  `ProviderInstance` with `provider_fault` set, not as a hard error, so GARM can tell the two apart.
- **Delete.** `delete` proves release: stop (bounded) → `tart delete` → confirm absent from `tart list`
  → remove the record → release the slot. Absent VM = no-op success. Unreachable host = hard error so
  GARM retries; the `deleting` tombstone keeps the slot counted.
- **Reaper.** `Reconcile` runs on every `status` and before every `create`: it reaps quarantined VMs past
  the TTL, drops records whose VM vanished (except `deleting` tombstones), reclaims stale
  `cloning`/`running` records left by a crashed create, and adopts or quarantines unrecorded VMs.

## Invariants

1. **The host owns truth.** The registry under `/var/db/tart-fleet/instances/<name>.json` is the source
   of truth for which VMs exist and which slot each occupies. The provider is stateless.
2. **`unknown` is not empty.** A host that cannot be probed is `unknown`, is excluded from placement,
   and — when every host is `unknown` — produces a distinct `no_reachable_host` fault, never `capacity`.
3. **`ready` means the guest answered.** A record reaches `ready` only when `tart exec <name> true`
   succeeds. A `running` record whose probe failed maps to GARM `error` with the `last_error` as
   `provider_fault`.

## Host-side control plane (`tart-hostctl`)

The host binary exposes an idempotent verb set. JSON goes to stdout; a machine-readable fault
`{"fault": "<name>", "error": "<message>"}` goes to stderr on failure. Exit codes match the provider:
`0` success, `1` failure, `2` usage.

| Verb | Effect |
| --- | --- |
| `status [--json]` | reconcile, then report health: `tart_version`, `keychain_unlocked`, `free_disk_bytes`, `drained`, `capacity`, `instances` |
| `create --name --image --cpu --mem --disk --controller [--owner] [--host]` | idempotent on name; clone `--stacked`, label, set resources, boot headless, probe readiness |
| `get --name` | record plus live Tart presence and IP |
| `list [--controller --owner]` | records, excluding quarantined VMs |
| `delete --name` | prove release; no-op when absent |
| `bootstrap --name` | run the supplied script (stdin) in the guest; record `idle` or `failed` |
| `drain` / `undrain` | refuse / allow new creates |

Fault names: `usage`, `capacity`, `keychain`, `drained`, `not_found`, `tart`, `validation`.

The hostctl CLI is configured through environment variables (`cmd/tart-hostctl/main.go`):

| Variable | Default | Meaning |
| --- | --- | --- |
| `TART_FLEET_ROOT` | `/var/db/tart-fleet` | registry root |
| `TART_FLEET_SLOTS_PER_HOST` | `2` | licence cap used by `capacity`; values above `2` are rejected |
| `TART_FLEET_STALE_CREATE_TIMEOUT` | `5m` | stale-create reclaim TTL |
| `TART_FLEET_PROBE_TIMEOUT_SECONDS` | `60` | readiness-probe bound |
| `TART_FLEET_QUARANTINE_TTL_HOURS` | `24` | quarantine TTL |
| `TART_FLEET_HOST` | hostname | host component of provider IDs |
| `TART_FLEET_CONTROLLER_ID` | empty | controller used for adoption |
| `TART_FLEET_TART_BIN` | `tart` | Tart binary |
| `TART_FLEET_SECURITY_BIN` | `security` | keychain probe binary |
| `TART_FLEET_DF_BIN` | `df` | free-disk probe binary |
