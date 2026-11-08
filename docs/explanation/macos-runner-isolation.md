# macOS runner isolation: VMs, bare metal, and the licence cap

Why macOS runners are VMs on Apple Silicon rather than bare-metal machines, and what the macOS licence
permits.

## VM vs bare metal

Tart runs guests on `Virtualization.framework`, Apple Silicon's hypervisor. The trade-offs for CI:

| Concern | Tart VM | Bare-metal Mac mini |
| --- | --- | --- |
| Guest OS | arm64 macOS only; no Intel guests. x86_64 binaries run under Rosetta 2 (CLI only). | native macOS |
| GPU | paravirtual GPU — Metal and the iOS simulator work | full discrete GPU (for GPU-specific or `eGPU` jobs) |
| USB / peripherals | no USB passthrough | full passthrough (iOS device testing, hardware-in-the-loop) |
| Clean room | Clone from a golden image; `--stacked` is available on macOS 27+ | manual re-image or MDM; slow to reset |
| Isolation | each job gets a fresh VM; a broken job does not poison the next | shared machine; state leaks between jobs |
| Density | two guests per host (licence-capped) | one runner per machine |
| Provisioning | clone + boot + bootstrap, tens of seconds | image and enroll a physical machine |

The fleet uses VMs because clean-room isolation and near-instant clones are worth more than GPU or USB
passthrough for the target workloads. Jobs that need a discrete GPU, USB devices, or an Intel build
environment must run on bare metal, not on Tart VMs.

### Networking

Tart's default networking is vmnet shared NAT: guests sit on `192.168.64.0/24` behind the Mac and are
not reachable from the Linux controller. The provider therefore reaches a guest only from its host —
`tart ip` for the address and `tart exec` for commands. Bridged networking (`en0`) is the alternative
when a guest must be reachable from outside the host.

## The licence cap

The macOS licence grants a limited right to run additional macOS instances in virtual operating system
environments. Apple's Software License Agreement for macOS, Section 2B(iii), grants the right

> to install, use and run up to two (2) additional copies or instances of the Apple Software, or any
> prior macOS or OS X operating system software or subsequent release of the Apple Software, within
> virtual operating system environments on each Apple-branded computer you own or control that is
> already running the Apple Software, for purposes of: (a) software development; (b) testing during
> software development; (c) using macOS Server; or (d) personal, non-commercial use.

The same section states that, except as expressly permitted elsewhere,

> the grant set forth in Section 2B(iii) above does not permit you to use the virtualized copies or
> instances of the Apple Software in connection with service bureau, time-sharing, terminal sharing,
> relay service or other similar types of services.

Consequences for this provider:

- **Two guests per host, maximum.** `tart-hostctl` enforces the cap as a hard ceiling (it rejects
  `TART_FLEET_SLOTS_PER_HOST` above `2`), and the provider rejects `slots_per_host` above `2` as a config
  guard. A third guest is a licence breach, not a tuning choice.
- **Permitted purposes.** The fleet is for our own software development and testing during development.
- **Out of scope.** Offering the runners as a hosted or shared CI service to third parties is explicitly
  excluded — this is not a service bureau.

This page paraphrases the macOS Software License Agreement for macOS 26 (Tahoe). The authoritative text
is Apple's, at <https://www.apple.com/legal/sla/>; if the licence you accepted at purchase differs, it
governs.
