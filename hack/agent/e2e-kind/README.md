# Agent e2e: local and CI

`e2e.py` defines shared `setup`, `lifecycle`, `configuration`, and `fresh-bootstrap`
suites. Run `e2e.py list-suite --suite lifecycle` to inspect the exact sequence.
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

Because `/usr/local` is a real directory inside that read-only `/usr` rather
than a symlink to somewhere writable, the agent is installed under
`/opt/unbounded` instead. The harness passes that prefix to
`manual-bootstrap --host-prefix` and asserts against it throughout, including
the reset cleanup.

Provisioning is Ignition rather than cloud-init, which inverts the usual order.
An Ignition config is applied before the host boots and has to carry the
bootstrap token and the API server address, so `create-vm` acquires the image
and stops; `run-agent` renders the config and launches the VM. Nothing is
delivered over SSH: Ignition places the binary and the agent config, and a
first-boot unit runs preflight and bootstrap.

The image is resolved from the manifest published alongside it, so a refreshed
build is picked up without a code change. It is fetched with a federated Azure
login, because the storage account holding it disables anonymous access and
shared keys alike. The image can be chosen in other ways:

- `ACL_IMAGE_MANIFEST_URL` reads a different manifest.
- `ACL_IMAGE_URL`, `ACL_IMAGE_SHA256` and `ACL_IMAGE_BUILD_ID`, set together,
  pin a build and skip the manifest. `e2e.py resolve-host-image` prints them for
  the current build; CI runs it once per job.
- `ACL_IMAGE_BUILD_ID` alone fails the run unless the manifest publishes that
  build.
- `HOST_IMAGE_PATH` boots a local file with no Azure login at all:

```sh
HOST_BASE_OS=acl HOST_IMAGE_PATH="$PWD/acl.qcow2" \
  bash hack/agent/e2e-kind/run-local.sh
```

Running this locally needs `ovmf` and `qemu-nbd` in addition to the usual
prerequisites. The host boots through its own UEFI bootloader, and the Ignition
config URL is appended to the kernel command line by patching a UKI addon on
the EFI system partition; see `ukiboot.py` for why the boot chain is extended
rather than replaced.

In CI this entry is skipped unless a federated Azure login is configured, and
on pull requests from forks, because GitHub withholds secrets from
fork-triggered workflows. It is left out of the matrix rather than added and
failed, so it appears on its own once `ACL_IMAGE_CLIENT_ID`,
`ACL_IMAGE_TENANT_ID` and `ACL_IMAGE_SUBSCRIPTION_ID` exist as repository
secrets. They have to be repository secrets rather than environment ones: the
`azure-ci` environment requires a reviewer, which would put a manual approval
in front of every pull request.

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
