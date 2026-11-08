# Run macOS jobs with garm-provider-tart

End-to-end guide: register the template, create a pool, run a first job, plan capacity, and roll back
without orphaning VMs. Field-by-field behaviour is in
[the reference](../reference/garm-provider-tart.md); preparing the Mac minis is in
[macos-runner-host.md](macos-runner-host.md).

## Prerequisites

- A GARM controller on Linux with the `garm-provider-tart` binary installed and registered as an
  external provider (a `[[provider]]` block with `provider_type = "external"`).
- At least one Mac mini prepared by `scripts/mac/host-bootstrap.sh` and passing
  `scripts/mac/host-check.sh --json | jq -e '.ok == true'` (see
  [macos-runner-host.md](macos-runner-host.md)).
- The provider config file with one `[[hosts]]` block per Mac (see the reference).
- `garm-cli` authenticated against the controller, and the repository already registered with GARM.

Get the repository ID you will attach the pool to:

```bash
garm-cli repository list --format json | jq -r '.[] | "\(.name) \(.id)"'
```

## 1. Register the runner-install template

GARM has no darwin OS type, so the macOS runner logic lives in a template registered as `--os-type
linux`. Register it once:

```bash
garm-cli template create \
  --name macos-runner \
  --path internal/macprovider/macos-runner.sh \
  --forge-type github \
  --os-type linux
```

`scripts/mac/render-template.sh` renders the template (JIT and non-JIT) and fails if it leaks a
Linux-only mechanism; run it before registering to prove the template renders.

## 2. Create the pool

`max-runners` must not exceed the fleet capacity (hosts × `2`, the per-host licence cap enforced by
`TART_FLEET_SLOTS_PER_HOST`). The tags list must include
`self-hosted` explicitly: GARM configures runners with `--no-default-labels`, so a missing `self-hosted`
label means no workflow ever matches.

```bash
garm-cli pool add \
  --repo <repo-id> \
  --provider-name tart \
  --image "ghcr.io/cirruslabs/macos-tahoe-xcode@sha256:<digest>" \
  --flavor default \
  --tags "self-hosted,macos,arm64" \
  --runner-install-template macos-runner \
  --os-arch arm64 \
  --os-type linux \
  --enabled \
  --min-idle-runners 1 \
  --max-runners 2 \
  --runner-bootstrap-timeout 15
```

Confirm the pool exists and an idle runner is being provisioned:

```bash
garm-cli pool list --format json | jq '.[] | select(.id == "<pool-id>")'
garm-cli runner list <pool-id> --format json
```

A healthy pool converges to an idle runner whose status is `running` and whose `provider_id` is
`<host>/<vm-name>`. Wait for it — clone plus boot plus readiness probe is on the order of a minute.

## 3. Run a first job

The labels must match the pool tags exactly. A minimal smoke workflow:

```yaml
name: macos-smoke
on: [push]
jobs:
  smoke:
    runs-on: [self-hosted, macos, arm64]
    steps:
      - run: uname -m
      - run: swift --version
```

The job is picked up only by runners whose label set is a superset of `runs-on`. `self-hosted` is the
GitHub-convention label GARM no longer adds for you, which is why the pool tags carry it explicitly.

## 4. Capacity planning

- Each Mac mini runs at most two guests. The host enforces the cap as a hard ceiling: `tart-hostctl`
  rejects `TART_FLEET_SLOTS_PER_HOST` above `2`, and the provider rejects a `slots_per_host` config above
  `2` because the macOS licence allows two guests per host
  ([docs/explanation/macos-runner-isolation.md](../explanation/macos-runner-isolation.md)).
- For one pool, set `--max-runners` to `hosts × 2` so GARM never asks for more than the fleet can hold.
  `--min-idle-runners` trades warm capacity for idle cost; keep it at or below `max-runners`.
- A single-Mac deployment is simplest as one provider and one pool per Mac with `--max-runners 2`. A
  multi-host pool needs `max-runners = hosts × 2` and lets the provider place VMs across the fleet.
- Watch `provider_metric` lines on the provider stderr (GARM logs them): `probe up= full= unknown=`,
  per-host `capacity`, and `unknown_hosts=` tell you whether you are short of slots or short of hosts.

## 5. Roll back

### Drain a host

Draining refuses new creates but leaves running VMs alone — use it before maintenance, a reboot, or
removing a host:

```bash
ssh garm@mac-mini-01 /usr/local/bin/tart-hostctl drain
```

Wait for running VMs to finish their jobs, then delete the runners:

```bash
garm-cli runner list <pool-id> --format json | jq -r '.[].name' | while read -r n; do
  garm-cli runner delete "$n"
done
```

### Delete the pool

GARM refuses to delete a pool that still has runners, so the runners go first. Deleting the pool also
drives `RemoveAllInstances`, which deletes every VM carrying this controller's ID as a safety net:

```bash
garm-cli pool delete <pool-id>
```

### Remove the provider

Remove the `[[provider]]` block from the GARM config and restart the controller. The provider itself
holds no state, so nothing on the controller needs cleaning.

### Verify no orphaned VMs

On each host, confirm the registry and Tart agree that nothing is left:

```bash
ssh garm@mac-mini-01 '/usr/local/bin/tart-hostctl status --json' | jq '.capacity'
ssh garm@mac-mini-01 'tart list --source local --format json' | jq length
```

Both the registry `instances` and the remaining local Tart VMs should be reviewed. Tracked stale creates
can be reaped automatically, and deleting tombstones retain their slot until absence is verified.
Untracked VMs are quarantined for manual ownership review, and the host never deletes them automatically.
A re-image of the host (`/var/db/tart-fleet` and `~/.tart` go with the image) permanently removes those VMs.

## Add or replace a Mac mini

1. Bootstrap the new Mac with the same `HOST_NAME` you want in `provider_id`s
   ([macos-runner-host.md](macos-runner-host.md)), and verify it with `host-check.sh`.
2. Append its `[[hosts]]` block to the provider config (the bootstrap writes a ready-to-copy fragment
   at `/etc/garm-provider-tart.toml` on the host).
3. To replace a host: drain the old one, delete its VMs and its `[[hosts]]` block, then add the new
   block under the same name if you want provider IDs to stay stable. The registry is per host, so
   nothing else moves.
