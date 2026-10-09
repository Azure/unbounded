# Agent e2e: local and CI

`e2e.py` defines shared `setup`, `lifecycle`, `configuration`, `fresh-bootstrap`,
`bootstrap-recovery`, and `migration` suites. Run
`e2e.py list-suite --suite lifecycle` to inspect the exact sequence.
The host lifecycle includes existing upgrade/rollback, reset/reinstall, and repave
operations plus an unassisted host reboot with fresh node identity and workload/DNS.
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

The gallery export needs, for the identity az is logged in as, on the resource
group: `Microsoft.Compute/disks/read`, `write`, `delete`,
`beginGetAccess/action` and `endGetAccess/action`, and read access to the
shared gallery image. The identity has to be in a tenant the gallery is shared
with. Grant no more than that: any pull request from this repository can log in
as it. A resource group that holds only the export disks, with a custom role
carrying exactly those actions, keeps a pull request from reading or deleting
any other disk.

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
root, `LEGACY_AGENT_VERSION` (default `v0.8.0`), fetched from its GitHub release
by the install script. v0.10.0 is the last such release and lays the host out
the same way; the default moves to it once it is published. Before installing
it, the suite stages a file under `/opt/unbounded/images`, as a host keeping a
local OCI layout beside the host root would, and checks it is untouched after
the link, the move and reset. An AgentUpgrade to this build must link
`/opt/unbounded/agent` to `/usr/local` and leave that release's layout and units
as they were, because the older release is now last-good and a rollback needs
them. The host then reboots, returns to the older release, and upgrades to this
build again, staying linked throughout. The next upgrade leaves no older release
in either slot, and the daemon it starts must copy the files into a real
`/opt/unbounded/agent`, point the units at them, and restart itself from there,
and the restarted daemon must remove them from `/usr/local`. The moved host
reboots, then resets, which must leave neither root behind. The older release
cannot be installed on an immutable host, so the suite needs a cloud-init host:

```sh
HOST_BASE_OS=ubuntu2404 E2E_SUITE=migration KEEP_ENV=1 \
  bash hack/agent/e2e-kind/run-local.sh
```
