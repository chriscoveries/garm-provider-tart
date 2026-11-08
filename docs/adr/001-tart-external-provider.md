# ADR 001 — Tart, a thin external provider, and a GARM template for macOS runners

Status: accepted
Date: 2026-10-28

## Context

We need self-hosted GitHub Actions runners on macOS. The fleet is Apple Silicon Mac minis
(arm64 only), the orchestrator is [GARM](https://github.com/cloudbase/garm), and the controller runs
on Linux. GARM publishes no darwin build, understands only `linux` and `windows` OS types, and its
built-in runner bootstrap assumes systemd and a Linux-only dependency step. macOS therefore cannot be
treated as just another GARM OS type; the provider and the guest bootstrap both need to be built.

## Decision

Drive macOS runners with **Tart** VMs on Apple Silicon, expose them to GARM through a **thin external
provider** (a Go binary that speaks the GARM external-provider CLI contract and drives Tart over SSH),
and run the guest bootstrap through a **custom GARM runner-install template** registered with
`--os-type linux`.

- **Tart** is the VM layer: `Virtualization.framework` on Apple Silicon, ordinary clones on macOS 26,
  optional `--stacked` clones on macOS 27+, OCI golden images (`ghcr.io/cirruslabs/macos-*-xcode`), and headless
  `tart run --no-graphics` with a guest agent that supports `tart exec` and `tart ip`.
- **The provider is thin and stateless.** It execs once per GARM operation, maps the operation to a
  `tart-hostctl` verb over SSH, and holds no state of its own. Truth lives on the Mac (see
  [ADR 002](../adr/002-host-control-plane.md)). No daemon, no controller tier.
- **The template carries the macOS-specific logic.** GARM's OS-type gate rejects anything but
  `linux`/`windows`, so the pool is created `--os-type linux` and the provider returns an
  `os_type: linux` instance whose `os_name` is `macos`. The rendered runner-install template does the
  work the Linux bootstrap would do: it uses launchd via `./svc.sh install|start`, fetches JIT or
  registration credentials from the metadata service, and never touches systemd or the Linux-only
  dependency step. It is registered with `garm-cli template create --os-type linux` and attached to the
  pool with `--runner-install-template`.

## Alternatives considered

| Option | Why rejected |
| --- | --- |
| **Anka** (Veertu) | Commercial licence and a proprietary controller/registry tier; heavier than a CLI VM tool. The cost and extra moving parts buy nothing GARM does not already provide. |
| **Orka** (MacStadium) | Kubernetes-based Mac orchestration platform; requires a dedicated Orka cluster on Apple hardware and a commercial relationship. A full second orchestrator beside GARM duplicates desired state. |
| **Lume** | An open-source `Virtualization.framework` CLI in the same space as Tart, but younger: no comparable ecosystem of published golden images, no `--stacked` clone density, and less headless-CI tooling. Tart's Cirrus-published images and guest agent decide it. |
| **VMware Fusion** | Licensing for a fleet is ambiguous; no OCI golden-image pull, no copy-on-write clone density, and a heavier hypervisor stack. Apple Silicon guests are arm64-only regardless of hypervisor, so Fusion adds friction without adding capability. |

Tart was selected because it is free for internal use (FSL-1.1-ALv2, converting to Apache-2.0 two years
after each release), supports efficient `--stacked` clones on macOS 27+, pulls golden images straight from an OCI
registry, and exposes exactly the headless primitives a CI provider needs.

## Consequences

- The provider must cap each host at **two** guests: the macOS licence allows at most two additional
  macOS instances per Apple-branded host (see
  [docs/explanation/macos-runner-isolation.md](../explanation/macos-runner-isolation.md)). A third
  guest is a licence breach, not a tuning choice.
- Pools are created `--os-type linux`, so pool tags must list `self-hosted` explicitly — GARM configures
  runners with `--no-default-labels`.
- The controller stays on Linux and reaches each Mac over SSH; guests sit behind NAT and are only
  reached from the host (`tart ip`, `tart exec`).
- The provider only runs the injected runner-install template in the guest; it never provisions hosts,
  installs Tart, or manages the keychain.
