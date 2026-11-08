# garm-provider-tart

A [GARM](https://github.com/cloudbase/garm) external provider that manages ephemeral macOS VMs on
Apple Silicon Mac minis with [Tart](https://tart.run).

Each GARM runner maps to one Tart VM: clone from a golden image, boot headless, wait for the guest,
run the runner bootstrap, and delete the VM when the job is done. The provider speaks the standard
GARM external-provider CLI contract (`CreateInstance`, `DeleteInstance`, `GetInstance`, `ListInstances`,
`RemoveAllInstances`, `Start`, `Stop`), so GARM treats it like any other provider.

## Status

The host-owned registry, the idempotent Tart verb set, fleet placement (T5), and the guest bootstrap
template (T3) are implemented and unit-tested with stub `tart`/`ssh` binaries. `StartInstance` and
`StopInstance` are recognised but not implemented yet (they return exit 1); nothing here runs on real
hardware.

## Architecture

Truth lives on the Mac, not on the GARM controller. Each host runs `tart-hostctl`, a small CLI that
owns a per-instance registry under `/var/db/tart-fleet/instances/<name>.json` and serialises every
mutation with `flock /var/db/tart-fleet/lock`. The Linux provider is a stateless adapter that runs
one `tart-hostctl` verb over SSH and maps the result to GARM's view. A controller rebuild, a second
controller, or a manual `tart delete` can therefore never double-book or strand a slot.

```text
GARM (Linux) --ssh--> tart-hostctl (macOS) --> tart + /var/db/tart-fleet/
```

## Why a separate provider

GARM has no macOS support: its OS types are `linux` and `windows`, its bootstrap assumes systemd, and
it ships no darwin build. The provider therefore

- runs on the Linux GARM controller and drives Tart on each Mac mini over SSH,
- selects the `osx`/`arm64` runner tool from the bootstrap payload itself,
- runs the runner install script that GARM injects into `extra_specs["runner_install_template"]`
  inside the guest (launchd, JIT credentials, no `systemd`),
- caps each host at two macOS guests, which is the limit in the macOS licence agreement.

## Layout

```text
cmd/garm-provider-tart/   GARM command dispatch (external-provider CLI contract)
cmd/tart-hostctl/         host-side control plane, installed at /usr/local/bin/tart-hostctl
internal/hostctl/         registry, state machine, Tart verbs, adoption, locking
internal/provider/        config, dispatch, operations, SSH transport
testdata/bin/             stub tart and ssh binaries used by the tests
scripts/mac/              host bootstrap, image refresh, host check, fake-GARM harness
docs/how-to/              operator runbooks (host preparation, ...)
test/contract/            pinned GARM contract fixtures, replay + canary tests
scripts/contract/         contract capture driver (builds upstream GARM and records the contract)
```

## Requirements

- GARM on Linux (no darwin builds are published).
- Apple Silicon Mac minis running macOS 26, reachable over SSH.
- `tart-hostctl` installed at `/usr/local/bin/tart-hostctl` on each host.
- Tart 2.33 or newer, a golden image in an OCI registry, and an unlocked `login.keychain` on each host.

On macOS 26, the provider uses ordinary Tart clones. On macOS 27+ with a compatible Tart build,
set `TART_FLEET_STACKED_CLONES=1` on the host to opt in to layered clones. Tart currently has
no VM label setter; controller ownership is persisted in the hostctl registry, and any unrecorded
VM is quarantined for operator review.

Host preparation (user, Remote Login, auto-login, keychain daemon, power/update settings, Tart, golden
image, prune timer) is a one-time privileged step: `scripts/mac/host-bootstrap.sh` (see
`docs/how-to/macos-runner-host.md`). The provider never provisions hosts.

## Licence

TBD.

## Development

```bash
go build ./... && go vet ./... && gofmt -l .
go test -race ./internal/hostctl/... ./internal/provider/... -count=1
go test -race ./internal/hostctl/... -run 'Concurrency|Crash|Adopt' -count=20
make integration            # real GARM + fake hosts + Gitea (Docker; GARM_TEST_COMPOSE=podman-compose for rootless)
make contract               # replay the pinned GARM contract + canary shape diff
make contract-capture GARM_REF=<sha>   # regenerate the pinned contract fixtures
shellcheck -S warning $(find scripts -name '*.sh' 2>/dev/null)
npx --yes markdownlint-cli2 $(find . -name '*.md' -not -path './.git/*')
```

`make integration` drives a real GARM controller against the real provider binary, two fake hosts
(real `tart-hostctl` + stub `tart` + real `sshd`) and a real Gitea forge, entirely on Docker — no Apple
hardware, no GitHub tokens. See `test/integration/` and `.github/workflows/integration.yml`.

`make contract` pins the GARM external-provider contract we do not own: the operation names, the env
vars, the `BootstrapInstance` shape on stdin, and the `ProviderInstance` shape on stdout. `TestReplay`
runs the provider against the captured fixtures and asserts the exit code, stdout JSON validity and
every required field; `TestCanaryDrift` (driven nightly by `.github/workflows/contract-canary.yml`)
builds GARM at `main`, re-captures, and diffs the shape against the pinned fixtures, failing with a
readable diff on drift. Regenerate the pin after an intentional upstream change with
`make contract-capture GARM_REF=<sha>`. See `test/contract/`.

CI runs the same commands (`.github/workflows/ci.yml`, job `ci`). `main` is protected: every change lands
through a pull request with a green `ci` check.

## Usage

GARM execs the provider binary once per operation, with `GARM_COMMAND` selecting it:

| `GARM_COMMAND` | stdout | notes |
| --- | --- | --- |
| `CreateInstance` | one `ProviderInstance` | reads `BootstrapInstance` JSON on stdin |
| `GetInstance` | one `ProviderInstance` | `GARM_INSTANCE_ID` |
| `ListInstances` | JSON array (`[]` when empty) | fan-out across hosts, deduplicated |
| `DeleteInstance` | nothing | no-op when the VM is already gone |
| `RemoveAllInstances` | nothing | fan-out across hosts, best-effort |
| `StartInstance`, `StopInstance` | nothing | not implemented (exit 1) |

`provider_id` is `<host>/<vm-name>`. Diagnostics go to stderr; stdout carries JSON only.
Exit codes: `0` success, `1` operation/configuration failure, `2` usage error.

The hostctl verb set (each JSON on stdout, diagnostics on stderr, exit `0/1/2`):

```text
status [--json]                                   host health, keychain, disk, capacity
create --name --image --cpu --mem --disk --controller --owner [--host]
get --name
list [--controller --owner]
delete --name                                     proves release before returning
bootstrap --name --script -                       runs the script in the guest
drain / undrain                                   refuse / allow new creates
```

Minimal config (see `config.example.toml`):

```toml
ssh_user = "garm"
ssh_key = "/etc/garm/id_ed25519"
slots_per_host = 2
image = "ghcr.io/cirruslabs/macos-tahoe-xcode@sha256:…"

[[hosts]]
name = "mac-mini-01"
address = "10.20.0.11"
```

`slots_per_host` is an informational planning hint — the host enforces the two-guest licence cap via
`TART_FLEET_SLOTS_PER_HOST` — and values above 2 are rejected.

`scripts/mac/fake-garm.sh` exercises the provider contract end to end on Linux with the ssh stub:

```bash
scripts/mac/fake-garm.sh create | jq -e '.provider_id and .status=="running"'
scripts/mac/fake-garm.sh list   | jq -e 'type=="array"'
```
