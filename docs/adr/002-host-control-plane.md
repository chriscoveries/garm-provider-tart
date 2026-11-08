# ADR 002 — Host-owned control plane: SSH, host agents, or a controller

Status: accepted
Date: 2026-10-28

## Context

GARM runs on Linux and must create and destroy macOS VMs on a fleet of Apple Silicon Mac minis, two
guests per host. An earlier design assumed the provider SSHes into each Mac, runs `tart`, and keeps slot
state in a file on the GARM controller. This record decides where truth lives and what transport the
provider uses to reach it.

## Decision

**No separate Tart controller. SSH now, with the instance registry and capacity owned by the host.**
GARM already owns desired state — pools, instance records, retries, status. A controller tier would
duplicate that state and drift from it.

The provider is a stateless adapter that runs one idempotent `tart-hostctl` verb over SSH per operation.
Each Mac owns a per-instance registry under `/var/db/tart-fleet/instances/<name>.json` and serialises
every mutation with `flock /var/db/tart-fleet/lock` held for the whole verb. The provider keeps no state
of its own, so a controller rebuild, a second controller, or a manual `tart delete` cannot double-book
or strand a slot.

The transport and the correctness model are deliberately separated so the same verb surface can move to
HTTP later without a rewrite:

1. host-side registry + stateless host CLI over SSH (now);
2. the same verbs over HTTP from a host agent, when the fleet or the latency budget justifies a daemon;
3. a shared allocator only when more than one pool or provider shares the same Macs.

## Options

| | A. SSH, state on controller | B. host agent, provider as scheduler | C. provider → controller → agents |
| --- | --- | --- | --- |
| Truth for "which VMs exist" | controller file | host | controller DB |
| Survives controller rebuild | no | yes | yes |
| Mutual exclusion | flock on controller (correct only while one controller runs) | agent event loop | controller DB |
| Host reboot → VMs | invisible; leaked slots | adopted into registry, capacity honest | same as B |
| New moving parts | none | daemon, mTLS, cert lifecycle, upgrade program | agent + controller + DB |
| Cost at 2–10 Macs | low | high | highest |

## What was actually wrong, independent of transport

Three defects, none of them about SSH versus HTTP:

1. **Controller-local state.** A state file on the GARM host cannot be authoritative. Lose the host, run
   a second controller, or delete a VM by hand, and the provider either double-books a slot or strands
   one.
2. **No distinct "unknown".** A probe that times out is not "empty" and not "full". Collapsing the two
   double-books on one side and strands the fleet on the other.
3. **Tart state conflated with guest usability.** `tart run` returning does not mean the guest is usable.
   Returning "running" at that point lets GARM mark a half-bootstrapped VM healthy.

## Design that fixes them

- **Host-owned registry.** One JSON record per instance, plus an `adopted` marker for VMs found in
  `tart list` that have no record. `flock` on the host, held for the whole verb.
- **State machine.** `cloning → running → ready → bootstrapping → idle` for create,
  `deleting → gone` for delete, and `failed` for any live state. `ready` means `tart exec <name> true`
  answered, not that `tart run` returned.
- **Tri-state host health.** Every host is classified `up`, `full`, or `unknown`. `unknown` is dropped
  from placement; all-`unknown` is a distinct `no_reachable_host` fault, never `capacity`.
- **Placement without a scheduler.** Probe all hosts in parallel, pick the least-loaded `up` host, wait
  for capacity when only `full` hosts remain, and fail with a distinct fault when nothing is reachable.
- **Deletion proves release.** `delete` returns success only when the VM is absent from `tart list` and
  the record is gone; an unreachable host yields a hard error so GARM retries, and the tombstone keeps
  the slot counted until the VM is confirmed gone.
- **Adoption with an owner token.** VMs found after a reboot are adopted only if they carry this
  controller's label; unverified VMs are quarantined until manually resolved.

## Security controls (from the adversarial review)

1. No generic exec: `bootstrap` runs one fixed argv (`/bin/bash <script>`) as the runner user; names and
   image references are validated against a narrow pattern.
2. The runner token never persists: written to a 0600 temp file and removed after the run, never in a
   label, log, or error string.
3. mTLS only when the agent exists; no bearer fallback, and the listener rejects the guest subnet.
4. Fail closed on cert, allowlist, or clock error.
5. SSH stays as break-glass, with `authorized_keys` restricted to a command wrapper.

## Adversarial record

**Reviewer 1 (systems).** Verdict: *agent, no controller*. "Fleet state derivable from `GET /v1/vms`"
fails under partition — unreachable reads as empty and double-books. Idempotency on the instance name
hides a half-bootstrapped VM: "the agent knows Tart lifecycle state; it does not know guest usability."
Non-negotiable: make slot release provable and synchronous in both directions.

**Reviewer 2 (security/operability).** Verdict: *not at 10 Macs* — "you trade ~500 lines of ssh for a few
thousand lines of privileged networked service, cert lifecycle and a daemon patch program." Flips at
25–40 hosts or when real multi-tenant scoping is needed. Objections: the exec endpoint is RCE for any
certificate holder; guests share L2 with the host; one shared client cert destroys the "narrow verbs"
claim; adoption "resurrects instances GARM has terminally failed."

**Resolution.** They disagree only on *when* the daemon pays for itself, not on *what* is wrong. Both
defects are state-ownership defects, so the design fixes them without a daemon: host registry, tri-state
host health, provable delete, owner-token adoption. The SSH phase is correct now, and the agent phase is
a pure latency/observability upgrade with an unchanged verb surface.

## Current implementation status

The host-owned registry and the idempotent verb set are implemented (`cmd/tart-hostctl`,
`internal/hostctl`) and unit-tested against stub `tart`/`ssh`. The provider side (parallel probing,
tri-state classification, place-and-wait) is implemented in `internal/provider`. The host agent daemon
is future work and has not been built.
