# Agent e2e: local and CI

`e2e.py` defines shared `setup`, `lifecycle`, `configuration`, `fresh-bootstrap`,
`bootstrap-recovery`, and `migration` suites. Run
`e2e.py list-suite --suite lifecycle` to inspect the exact sequence.
The host lifecycle includes existing upgrade/rollback, reset/reinstall, and repave
operations plus an unassisted host reboot with fresh node identity and workload/DNS.
An AgentUpgrade must restart the daemon in place: the same process, running the
new binary.
Persistent DNS failure is fatal after bounded convergence retries.

```sh
HOST_BASE_OS=ubuntu2404 E2E_SUITE=lifecycle KEEP_ENV=1 \
  bash hack/agent/e2e-kind/run-local.sh
```

Default local execution runs setup, lifecycle, fresh-instance bootstrap, then
configuration scenarios. Fresh-instance bootstrap is a new VM installation, not
an interrupted-bootstrap recovery assertion.
Focused configuration creates only the bridge rather than a colliding default VM.
Use matching cluster/VM/subnet variables when invoking commands or cleanup on a
preserved environment. Same-disk reinstall checks host boot identity.

## Azure Container Linux (immutable hosts)

`HOST_BASE_OS=acl` boots an immutable host: `/usr` is a read-only dm-verity
image with no package manager, so nothing is installed at boot and the image
must already carry what the agent needs. It does.

`/usr/local` is a real directory inside that read-only `/usr` rather than a
symlink to somewhere writable, so an agent released before the host root cannot
be installed there. The current agent installs under `/opt/unbounded/agent` on
every host, which is on the writable root filesystem here.

Provisioning is Ignition rather than cloud-init, which inverts the usual order.
An Ignition config is applied before the host boots and has to carry the
bootstrap token and the API server address, so `create-vm` acquires the image
and stops; `run-agent` renders the config and launches the VM. On first boot
nothing is delivered over SSH: Ignition places the binary and the agent config,
and a first-boot unit runs preflight and bootstrap. Ignition runs only on first
boot, so the lifecycle's reinstall on the same disk copies the same binary,
config and unit over SSH instead. The VM fetches the config and the binary from
the runner, at `IGNITION_SERVE_BASE` when set and otherwise the bridge
gateway.

The image comes from one of two sources, chosen by `ACL_IMAGE_SOURCE`:

- `gallery`, the default, is the Azure build of the image, published to the
  shared compute gallery image `ACL_IMAGE_GALLERY_IMAGE` (default
  `/SharedGalleries/b3e01d89-bd55-414f-bbb4-cdfeb2628caa-ACL/Images/acl-1es-eval`).
  A gallery image cannot be downloaded as such, so the harness exports it: it
  creates a temporary managed disk from the version in
  `ACL_IMAGE_RESOURCE_GROUP`, reads the disk's written pages (about 850 MiB of
  32.5 GiB) through a ten-minute read SAS into a sparse VHD, revokes the SAS,
  deletes the disk, and converts the raw disk inside the VHD to qcow2 at its
  exact size. The disk is deleted whether or not the export worked, and each
  export first deletes any export disk older than six hours, which a canceled
  job leaves behind; a disk whose age cannot be read is left alone. The version
  is the gallery's latest unless `ACL_IMAGE_VERSION` pins one, and the disk has
  to be made in `ACL_IMAGE_GALLERY_LOCATION` (default `westus2`), the one region
  the image is replicated to. `ACL_IMAGE_SUBSCRIPTION` overrides az's default
  subscription. The gallery publishes no digest; the qcow2's sha256 is logged.
  The SAS is on no command line and in no error message, and is masked in the
  log, its signature on its own too.
- `manifest` is the generic build, resolved from the manifest published
  alongside it in a storage account that disables anonymous access and shared
  keys alike, and fetched with an Azure login. `ACL_IMAGE_MANIFEST_URL` reads a
  different manifest. `ACL_IMAGE_URL`, `ACL_IMAGE_SHA256` and
  `ACL_IMAGE_BUILD_ID`, set together, pin a build and skip the manifest;
  `ACL_IMAGE_BUILD_ID` alone fails the run unless the manifest publishes that
  build.

Either way the image file in the VM directory is named for its build,
`acl-<build>.qcow2` or `acl-gallery-<version>.qcow2`, so a newer one is picked
up without a code change and never masked by an earlier run's file. CI exports
or downloads it on every run and never puts it in the Actions cache, which a
pull request from a fork can restore; the export takes about two minutes. Within
a job, the fresh-bootstrap suite reuses the file the lifecycle suite made.
`e2e.py resolve-host-image` prints the settings that pin the current build; CI
runs it once per job. Settings that belong to the other source fail the run
rather than being ignored.

The gallery export needs, for the identity az is logged in as:

- on the resource group: `Microsoft.Compute/disks/read`, `write`, `delete`,
  `beginGetAccess/action` and `endGetAccess/action`, and
  `Microsoft.Resources/subscriptions/resourceGroups/read`;
- on the subscription: `Microsoft.Compute/locations/sharedGalleries/read`,
  `.../images/read` and `.../images/versions/read`, which resolve the version
  and let a disk be made from it. They read only galleries shared with the
  subscription, not any resource in it.

Reader on the subscription is not needed, nor are the built-in disk and
snapshot roles, which also grant storage accounts, snapshots and virtual
machines. The identity has to be in a tenant the gallery is shared with. Grant
no more than that: any pull request from this repository can log in as it. Two
custom roles carrying exactly those actions, the first assigned on a resource
group that holds only the export disks, keep a pull request from reading or
deleting any other disk.

`HOST_IMAGE_PATH` boots a local file with no Azure login at all, whichever the
source:

```sh
HOST_BASE_OS=acl E2E_SUITE=lifecycle HOST_IMAGE_PATH="$PWD/acl.qcow2" \
  bash hack/agent/e2e-kind/run-local.sh
```

For the manifest source, an image URL from the manifest or a pin has to be
https on `*.blob.core.windows.net`, because the storage token is sent to it,
and its sha256 has to be 64 hex characters. An image already in the VM
directory is checked against the sha256 again, and downloaded again if it does
not match. The gallery source has no URL or digest to check: its version is
checked as a plain name, and an existing export is reused by that name.

The configuration suite does not run on this host: its scenarios supply their
own agent, and the Ignition path only boots the agent it staged itself. The
default local run skips it there.

Running this locally needs `ovmf` and `qemu-nbd` in addition to the usual
prerequisites. The host boots through its own UEFI bootloader, and the Ignition
config URL is appended to the first boot's kernel command line by patching the
first-boot UKI addon on the EFI system partition; see `ukiboot.py` for why the
boot chain is extended rather than replaced. ignition-quench deletes that addon
after the first boot, so later boots, including the suite's reboots, run on the
image's own command line, with networking from the units Ignition wrote.

In CI this entry is skipped unless a federated Azure login is configured, and
on pull requests from forks, because GitHub withholds secrets from
fork-triggered workflows. It is left out of the matrix rather than added and
failed, so it appears on its own once all three of the selected source's
secrets exist: `ACL_GALLERY_CLIENT_ID`, `ACL_GALLERY_TENANT_ID` and
`ACL_GALLERY_SUBSCRIPTION_ID` for the gallery, which CI uses, or
`ACL_IMAGE_CLIENT_ID`, `ACL_IMAGE_TENANT_ID` and `ACL_IMAGE_SUBSCRIPTION_ID`
for the manifest. A secret missing from the selected set never falls back to
the other set. The workflow sets `ACL_IMAGE_SOURCE` and
`ACL_IMAGE_RESOURCE_GROUP`. They have to be repository secrets rather than
environment ones: the `azure-ci` environment requires a reviewer, which would
put a manual approval in front of every pull request.

Each identity's federated credentials have to trust the subject of every
trigger that adds the entry: pull requests, pushes to `main` and to `release-*`
branches, and manual runs on the branches they run on. Each is configured for
pull requests from this repository and pushes to `main`, so a `release-*` push
or a manual run elsewhere fails at the Azure login rather than being skipped,
until it has a federated credential of its own.

Every other host downloads from a public mirror and runs normally in all of
these cases.

Cloud-init preparation is fail-fast, with the success marker last. EL10 hosts
install `kernel-modules-extra-$(uname -r)` and load the netfilter modules required
by kube-proxy. Completion and marker are verified before bootstrap. Fedora's
specifically observed early hostname warning can be accepted only after completion,
without fatal errors, and after both static and runtime hostname are verified.

Configuration scenarios run at most two guests concurrently by default
(`CONFIG_SCENARIO_WORKERS`). Successful guests have logs captured before stopping;
their disks remain until cleanup. Failed batches preserve guests and prevent
additional batches from consuming memory. Commands have bounded execution and
CI's monitor records host resources before the suite deadline, leaving time for
diagnostic collection and upload. These tests exercise main's existing lifecycle;
they do not assert resumable bootstrap or introduce new recovery operations.

## Host root migration

The `migration` suite starts from a host installed by a release before the host
root, `LEGACY_AGENT_VERSION` (default `v0.12.0`, the latest published release),
fetched from its GitHub release by the install script. Its layout under
`/usr/local` is the one v0.8.0, the earlier default, has. Before installing it,
the suite stages a file under `/opt/unbounded/images`, as a host keeping a local
OCI layout beside the host root would, and checks it is untouched after the
link, the move and reset. An AgentUpgrade to this build must link
`/opt/unbounded/agent` to `/usr/local` and leave that release's layout and units
as they were, because the older release is now last-good and a rollback needs
them. The host then reboots, returns to the older release, and upgrades to this
build again, staying linked throughout. The next upgrade leaves no older release
in either slot, and the daemon it starts must copy the files into a real
`/opt/unbounded/agent`, point the units at them, and restart itself from there
in place, and the restarted daemon must remove them from `/usr/local`. The
moved host reboots, then resets, which must leave neither root behind. The
older release cannot be installed on an immutable host, so the suite needs a
cloud-init host:

```sh
HOST_BASE_OS=ubuntu2404 E2E_SUITE=migration KEEP_ENV=1 \
  bash hack/agent/e2e-kind/run-local.sh
```

## Isolation modes

The agent runs the node stack in one of two isolation modes: `nspawn` (the
default) and `none`. `ISOLATION_MODE` selects the mode under test and adds
`--isolation <mode>` to `kubectl unbounded machine manual-bootstrap`. Left
unset, no flag is passed and the existing suites behave exactly as before.

Every isolation suite is listed here. "Mode" is the value of `ISOLATION_MODE` to
run it with; the suites that change the mode start in the default mode and move
the node themselves. A change is declared the way an operator does it: the
harness patches `spec.isolation` on the Machine, checks that nothing moves and
`IsolationDrift` shows the declared mode as not yet applied, then deletes the
Node, which triggers the repave that applies it. Every change between `nspawn`
and `none` is a reprovision (the host is reset and bootstrapped again), and the
agent only reprovisions a Machine annotated
`unbounded-cloud.io/allow-reprovision: "true"`; without it the change is
reported `Failed` with reason `ReprovisionNotAllowed` and the repave goes ahead
in the current mode. The harness sets the annotation for the changes it expects
to succeed and removes it afterward. It does not choose the strategy: it asserts
the one `status.isolationChange` records.

| Suite | Mode | Covers |
|---|---|---|
| `isolation-operations` | `nspawn`, `none` or unset | The full operation set in one mode: join, status, preflight, `unbounded-agent exec` and `logs`, soft reboot (`NodeReboot`, which must restart the kubelet of the node stack), host-driven and operation-driven `AgentUpgrade` with rollback, repave, host reboot, `AgentReset`, and reinstall on the same disk. |
| `isolation-reset` | `nspawn`, `none` or unset | `unbounded-agent reset` run on the host removes the node stack of the mode, the agent's files and units, and leaves no reprovision handoff (unit, staged agent or staged configuration with the bootstrap token). |
| `isolation-reset-after-reprovision` | unset | The same reset on a node that reached `none` by a reprovision, after checking the handoff was cleaned up once the change succeeded. |
| `isolation-matrix` | unset | Every ordered pair of modes (`nspawn` -> `none` -> `nspawn`), by reprovision. After each change it also runs a soft reboot and a host reboot (and the `none` preflight), so a node that was changed behaves like one that was bootstrapped, and checks no reprovision handoff is left. |
| `isolation-drift` | unset | The `IsolationDrift` states: an empty spec declares nothing (`NoModeDeclared`), a declared unapplied mode shows `AppliedModeDiffers` while nothing moves, withdrawing it clears the drift, and a change refused for want of the reprovision opt-in shows `ChangeRefused` and is not retried. A change that succeeds ends at `AppliedModeMatches`; `isolation-matrix` asserts that after every change. |
| `isolation-rollback` | `nspawn`, `none` or unset | A change that cannot go ahead leaves the node in its mode: declaring the applied mode moves nothing and records nothing, and a change that needs a reprovision is refused without the opt-in (`ReprovisionNotAllowed`) while the repave brings the node back in its mode, with no handoff staged and no retry. |

```sh
HOST_BASE_OS=ubuntu2404 ISOLATION_MODE=none E2E_SUITE=isolation-operations KEEP_ENV=1 \
  bash hack/agent/e2e-kind/run-local.sh
HOST_BASE_OS=ubuntu2404 E2E_SUITE=isolation-matrix KEEP_ENV=1 \
  bash hack/agent/e2e-kind/run-local.sh
HOST_BASE_OS=ubuntu2404 E2E_SUITE=isolation-rollback KEEP_ENV=1 \
  bash hack/agent/e2e-kind/run-local.sh
```

The suites need QEMU with KVM, Docker and a Kind cluster, and they run one at a
time: the bridge, tap and veth names, the Kind cluster, the VM and the subnet are
fixed. If your shell predates your membership of the `kvm` group, wrap the
command with `sg kvm -c '...'`.

These suites are not part of the per-change CI e2e workflow: each one creates a
Kind cluster and boots a VM, and the set is too expensive to run on every change.
`isolation-matrix` and `isolation-rollback` run every night in
`.github/workflows/agent-isolation-nightly.yaml` (also started by hand with
`workflow_dispatch`). Run the others by hand with `run-local.sh`, or run the
whole plan with `isolation_plan.py`, when you change the agent's isolation code.

When a step fails, the harness prints the unit state, the journals of the agent
and the node stack, and the state of any mode change and reprovision handoff.
Bootstrap tokens and kubeconfig credentials are redacted from that output.

After a refused change the harness waits `RETRY_SOAK` seconds (default 60) to
prove nothing retries the change without a new trigger. `MIGRATION_TIMEOUT`
(seconds, default 900) bounds how long each mode change may take to reach a
terminal `status.isolationChange`.
NodeConfig variants that check nspawn files (`additional-host-mounts.json`) are
nspawn-only and are not part of the isolation suites.

## Isolation test plan and report

`isolation_plan.py` runs the scenarios in
`designs/agent-isolation-modes-test-plan.md` (every operation in each mode,
every ordered mode transition, drift, a refused change, reset on the host) and
writes a report.
Each scenario is one e2e suite on a fresh VM; a failed scenario does not stop the
run unless `--fail-fast`.

```sh
sudo apt-get install -y qemu-system-x86 qemu-utils genisoimage iptables   # once
sudo usermod -aG kvm "$USER"                  # once; log in again, or wrap with `sg kvm -c`
PATH="$PWD/tmp/bin:$PATH" python3 hack/agent/e2e-kind/isolation_plan.py run
```

| Command | Effect |
|---|---|
| `isolation_plan.py list` | The scenario catalog (IDs, mode, suite). |
| `run --scenario D01 T01` | Run only these scenarios. Also `--category`, `--tag`, `--exclude-tag transition`. |
| `run --dry-run` | Show what would run and what is skipped, without touching anything. |
| `run --no-up` | Use a Kind cluster and bridge you created; create and remove nothing. |
| `run --keep-env` | Leave the cluster, bridge and VM in place afterward. |
| `render <results.json> --html` | Regenerate `report.md` and `report.html` from saved results. |

The run creates the Kind cluster, VM bridge and controllers once (the `setup`
suite through `run-local.sh`), then for each scenario retires the previous VM,
launches a new one, runs `e2e.py run-suite`, and captures a JSON snapshot of the
Machine and node stack. Everything lands in
`tmp/isolation-report/<run-id>/`: `report.md`, `report.html`, `results.json`,
and a log per scenario (`<ID>/suite.log`, plus `<ID>/diagnostics/` for a
failure). Bootstrap tokens are redacted. The report has the environment, a
summary, an operation by mode coverage matrix, a transition matrix and the
detail of each scenario. Scenarios that need other hardware (GPU) or another
host OS (`requires` in the catalog) are reported as skipped with the reason.

Every suite in the table above can also be run directly with `E2E_SUITE` through
`run-local.sh`, with `ISOLATION_MODE` set as the table says.
