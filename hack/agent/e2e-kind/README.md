# Local and CI agent e2e

`e2e.py` defines the shared suites used by `run-local.sh` and GitHub Actions.
Environment setup differs, but scenario commands and assertions are shared.

```sh
# Full local run: setup, lifecycle, bootstrap recovery, configuration scenarios
HOST_BASE_OS=acl \
HOST_IMAGE_PATH="$PWD/hack/scratch/acl_amd64_qcow2.qcow2" \
./hack/agent/e2e-kind/run-local.sh

# Focused reproduction; preserve the VM and cluster for inspection
HOST_BASE_OS=acl \
HOST_IMAGE_PATH="$PWD/hack/scratch/acl_amd64_qcow2.qcow2" \
E2E_SUITE=bootstrap-recovery KEEP_ENV=1 \
./hack/agent/e2e-kind/run-local.sh

python3 hack/agent/e2e-kind/e2e.py list-suite --suite lifecycle
python3 hack/agent/e2e-kind/e2e.py list-suite --suite configuration
```

Use consistent `KIND_CLUSTER_NAME`, `VM_NAME`, `AGENT_MACHINE_NAME`, and
`HOST_BASE_OS` values when invoking individual commands on a preserved
environment. Run `e2e.py cleanup` with those same values when finished.

## Lifecycle

The lifecycle suite validates join, runtime configuration, pod logs and cluster
DNS, node restart, host-driven and controller-driven agent upgrades, rollback,
unassisted host reboot, reset, reboot of the reset host, same-disk reinstall,
and node repave. DNS failure is fatal. Kind kube-proxy is configured during
setup to reach the API server through an address available to external VMs,
rather than through Docker-only hostname resolution.

On ACL, **initial provisioning uses Ignition**. Explicit post-reset reinstall
uses SSH to deliver only the rendered agent binary, configuration, and bootstrap
unit. It does not rerun Ignition or replace the disk. Host boot identity must
remain unchanged during reinstall. Reset removes the first-boot unit before
deleting its payload so it cannot restart unattended after reset.

## Bootstrap recovery

An HTTP fixture returns a non-retryable 404 for the first actual runc download,
while allowing preflight HEAD requests. The next fetch succeeds from the same
URL. This exercises failure after workspace creation, not just an HTTP client's
retry loop. Ignition's bootstrap service retries automatically; the script path
is invoked again with the identical payload. The suite requires both requests,
a completed installation record, and a healthy node and workload. On Ignition
hosts it also requires an increased bootstrap-service restart count.

This scenario does not establish recovery from a power failure, interrupted
extraction, or interruption during daemon installation/reset. Those require
separate fault-injection scenarios.

## Configuration scenarios

The configuration suite discovers `node-configs/*.json` and runs each on a
separate VM. ACL uses per-scenario Ignition URLs and the same node config
assertions as other hosts. Offline/blocked-egress scenarios obtain their
artifacts from a local registry prepared before guest boot; image-managed hosts
do not receive package installations over SSH. The rootfs distribution can
differ from the host, as in the offline Ubuntu rootfs scenario on ACL.

The ACL base image remains an external input until a CI-accessible image is
published. Local execution uses `HOST_IMAGE_PATH`; CI can fetch the same image,
verify its checksum, and supply that path. QEMU/OVMF testing does not establish
Azure metadata delivery or Secure Boot support.
