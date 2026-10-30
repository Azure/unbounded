# Unbounded Agent v2: Testing

Part of the [Unbounded Agent v2 design](README.md). The README has the status,
repository baseline, citation conventions and terms.

This document specifies how v2 is tested. It has two priorities:

- v2 passes the existing v1 end-to-end suites, because v2 should have the same
  outcomes as v1 ([v1 compatibility, section 2](08-v1-compatibility.md#2-behavior-changes-from-v1)).
- A v1 host migrates to v2 without replacing its node, and can roll back
  ([migrating from v1](10-migration-from-v1.md)).

## 1. Unit, conformance and golden tests

- The runtime conformance suite, plus a fake runtime for `nodemanager` and
  `daemon` tests.
- One test per row of the `Decide` table, with each trigger, including a Node
  deleted while the daemon is stopped, which must be ignored.
- `Recover` after a simulated crash between every two steps of a repave,
  including after `Pending` is saved but before the bundle is rendered.
- Golden tests showing the `nspawn-container` runtime renders kubelet,
  containerd, `.nspawn` and unit-override files identical to v1's goldens
  (`pkg/agent/phases/rootfs/testdata`).
- The nspawn pre-start boot hook rewrites the `.nspawn` file and unit override
  when a host device appears, and leaves the bundle digest unchanged.
- The install check: the same accept, refuse and resume cases as v1's
  (`cmd/agent/internal/installstate/store.go:195-225`), including a resumed
  install with a changed label, which keeps the running node and the applied
  settings.
- Converting v1 configurations ([v1 compatibility, section 5](08-v1-compatibility.md#5-converting-a-v1-configuration)): every scenario in
  `hack/agent/e2e-kind/node-configs/` and the configurations
  `kubectl unbounded machine manual-bootstrap` renders for cloud-init and
  Ignition, compared with golden v2 configurations.
- Migration with a fake host: each check that refuses, each phase resumed after
  a crash, automatic rollback after a failure in each phase, `--rollback`
  before and after the first repave, and the conversion of v1's applied config,
  credentials, Machine status and host preparation.
- Plugin protocol tests with a reference plugin: timeouts, errors, oversized
  output and permission checks.
- Host cleanup with a fake `SystemOps`: order of operations, `ErrNodeBusy` on a
  hung process, refusal to cross a bind mount inside `/var/lib/kubelet/pods`,
  refusal when an owned path is replaced by a symlink, keeping
  `preexistingMount` paths mounted, crash replay at every phase of a slot
  change, conservative Teardown without an ownership record, and
  `--force-standard-paths`.
- Adoption: the `Preserve` round-trip including mode, extended attributes and
  SELinux labels; the size cap; rejection of a mount inside an adopted tree;
  `Discard` deleting contents at the first wipe only.

## 2. Running the v1 end-to-end suites on v2

### 2.1 The rule

The v1 suites in `hack/agent/e2e-kind` are the acceptance tests for v2: setup,
lifecycle, configuration, fresh-bootstrap and bootstrap-recovery
(`hack/agent/e2e-kind/e2e.py:6488-6512`). v2 passes them with the same steps
and the same checks through Kubernetes: Node readiness, labels and taints, the
Machine, MachineOperations, workloads, DNS, kube-proxy and how a repave is
triggered.

Some checks read v1's files on the host directly. Those are swapped for v2
equivalents (section 2.3), and nothing else changes. A v1 check that fails on
v2 for any other reason is a v2 bug, or a behavior change that has to be
justified and listed in [v1 compatibility, section 2](08-v1-compatibility.md#2-behavior-changes-from-v1).

### 2.2 Harness changes

The harness gets two settings:

| Setting | Values | Default |
|---|---|---|
| `AGENT_PROFILE` | `v1`, `v2` | `v1` |
| `NODE_RUNTIME` | `nspawn-container`, `host` (only with `v2`) | `nspawn-container` |

A profile object holds everything that differs between them, so the suites
themselves do not change:

- **Commands.** The generated bootstrap script and the Ignition unit run
  `start` and `preflight` (`cmd/kubectl-unbounded/app/ignition.go:188-189`).
  For `v2`, the harness rewrites them to `v2 start` and `v2 preflight`, as it
  already rewrites the script to add preflight (`e2e.py:3709-3719`). It also
  owns the `version` and `agent-upgrade` commands it runs on the host.
- **Configuration.** For `nspawn-container`, the v1 JSON the script carries is
  used as it is, because v2 converts it. For `host`, the harness converts it
  with `v2 convert-config --runtime host`
  ([CLI and configuration, section 1](01-cli-and-configuration.md#1-cli)).
- **State.** For `v2`, the harness reads install and node state from
  `v2 status --output json`, not from v2's files
  ([CLI and configuration, section 1](01-cli-and-configuration.md#1-cli)).
- **Running a command inside the node.** `machinectl shell` (`e2e.py:637-644`)
  for `nspawn-container`; SSH to the host for `host`.
- **Names and paths:** daemon units, the reset leftover list, and the fake
  failing agent used by the rollback test.

### 2.3 Swapped checks

| Check | v1 | v2, `nspawn-container` | v2, `host` |
|---|---|---|---|
| Daemon unit and binary layout (`e2e.py:1407-1444,5615-5649`) | `unbounded-agent-daemon.service` | `unbounded-agent-v2-daemon.service`; the binary layout is the same | Same as `nspawn-container` |
| Install record (`e2e.py:3737-3797,6480`) | `install-state.json`: phase and install ID | `v2 status`: install phase and install ID | Same |
| Applied config unchanged by a resumed bootstrap (`e2e.py:3762-3803`) | Hash of `kube1-applied-config.json` | `v2 status`: digest of the applied settings | Same |
| Failure injected during bootstrap (`e2e.py:3735-3741`) | Blocks v1's daemon recovery script | Blocks v2's daemon recovery script | Same |
| The node's distribution matches the host's (`e2e.py:715-731`) | `/var/lib/machines/<m>/etc/os-release` | Unchanged | Not applicable: the node is the host |
| Extra host mounts and devices (`e2e.py:4150-4234`) | `.nspawn` binds and `DeviceAllow` | Unchanged | Not applicable: these settings are rejected for `host` ([CLI and configuration, section 2](01-cli-and-configuration.md#2-configuration)), so the scenario runs only on `nspawn-container` |
| A device that appears at boot is usable in the node (`e2e.py:4312-4394`) | `.nspawn` and override rewritten | Unchanged | The device is read from the host directly |
| Boot wiring (`e2e.py:5599-5607`) | The override runs `nspawn-lifecycle post-start` | The override runs `v2 boot-hook post-start` | Not applicable |
| A repave switches machines (`e2e.py:734-747,6077-6091`) | `kube1` to `kube2`, with a bpffs sentinel | Unchanged: slots `a` and `b` are `kube1` and `kube2` | `v2 status`: the enabled slot switches |
| NodeReboot (`e2e.py:5415-5452`) | New Node boot ID and a `Rebooted` event | Unchanged | Unchanged ([node runtime, section 5.2](03-node-runtime.md#52-host)) |
| AgentUpgrade rollback (`e2e.py:1515`) | The fake failing agent answers `version` | It also answers `v2 version`, so the test still reaches the failing-daemon path | Same |
| Reset leaves nothing (`e2e.py:5021-5083`) | v1's leftover list | The list in [host changes, section 1](06-host-changes.md#1-what-v2-creates-on-the-host), plus the host snapshot (section 3) | Same, with the `host` runtime's items |

### 2.4 Keeping v1 runs unchanged

The profile is a refactor of the harness, so it must not change v1 runs:

- `e2e.py list-suite` prints the same steps for every suite with
  `AGENT_PROFILE=v1`.
- The harness's own unit tests pass unchanged, including the ones that pin
  suite contents (`hack/agent/e2e-kind/test_reliability.py:19-31`,
  `test_host_root.py:351-386`). New tests cover the profile.
- The v1 CI jobs keep running and stay required.

## 3. Checking reset with a host snapshot

Before installing, and again after reset, the harness records:

- every file under `/etc`, `/var/lib`, `/opt`, `/usr/local` and `/run`, with its
  mode, owner and content hash;
- enabled and masked units;
- `sysctl -a`, `nft list ruleset`, and `iptables-save` and `ip6tables-save` on
  both the nft and legacy backends;
- `ip link`, `ip route`, `ip rule` and the policy routing tables;
- `/etc/fstab` and the pins under `/sys/fs/bpf`.

After a v2 reset, the two snapshots must match apart from an allowlist of
things the agent does not own, such as the journal, package manager caches,
cloud-init state and SSH host keys. Installed packages are listed in the reset
report and allowed. This tests [host changes, section 3](06-host-changes.md#3-host-preparation-and-its-reversal) and
[host runtime cleanup](07-host-runtime-cleanup.md) directly.

On a host migrated from v1, the difference must be exactly the v1 host
preparation changes that the reset reports as left in place
([migrating from v1, section 6](10-migration-from-v1.md#6-converting-v1s-state)).

## 4. Migration suite

A new `migration-v2` suite follows the existing host-root `migration` suite
(`e2e.py:6504-6509`):

1. Stage host files, as the existing suite does, and take the host snapshot.
2. Install v1: the release in `LEGACY_AGENT_VERSION`, or this build's v1
   commands.
3. Wait for the node, run the workload, and record the Node UID, the machine's
   PID, and the workload pods' UIDs and restart counts.
4. Migrate with an AgentUpgrade with `agentMode: v2`. A variant runs
   `v2 migrate` on the host instead.
5. Check that:
   - the operation is `Complete`;
   - the Node UID, machine PID, pod UIDs and restart counts are unchanged;
   - v1's detection files are gone ([v1 compatibility, section 4](08-v1-compatibility.md#4-existing-packages-used-as-libraries)), so v1
     `start` refuses the host;
   - `v2 status` shows a complete install on `nspawn-container`, with the
     running machine as the enabled slot;
   - the staged files are untouched.
6. Reboot the host. The node comes back through v2's boot wiring.
7. Run the v1 lifecycle steps under the `v2` profile: NodeReboot,
   AgentUpgrade in place, AgentUpgrade rollback, the workload and a repave.
   After the repave, the v1 backup is gone and `v2 migrate --rollback`
   refuses.
8. Reset, and compare with the snapshot (section 3).

Variants:

| Variant | What it checks |
|---|---|
| Rollback | Migrate, then `v2 migrate --rollback`. v1's daemon runs, v1's files are back, and the node is unchanged. The v1 NodeReboot and AgentUpgrade steps pass. Migrating again succeeds. |
| Interrupted | For each phase in [migrating from v1, section 5](10-migration-from-v1.md#5-steps), a helper on the host watches the migration record and kills the migrate unit, or reboots the host, when the phase is reached. The migration must then finish on its own. This is done from outside, without failure points in the product, as the existing bootstrap-recovery test does (`e2e.py:3732-3741`). |
| Failed | A v2 path is made unwritable in each phase. The migration must roll back to a working v1 automatically, and the operation must be `Failed` with the reason. |
| Refused | `v2 migrate` on a v1 host with an unfinished install, or with two applied configs, changes nothing. `v2 start` on a v1 host that was not migrated is refused. |
| To `host` | After migrating, `v2 reset`, then `v2 start` with the `host` runtime, then the lifecycle suite under `NODE_RUNTIME=host`. |
| Immutable host | The cluster-started migration on an Ignition host (`HOST_BASE_OS=acl`), which needs no access to the host |

`--adopt-machine` gets its own variant, with a fixture that mimics a
Flex-built host, once it is implemented.

## 5. Further end-to-end tests

These cover what v1 never had, so the v1 suites cannot:

- A `host` workload set with tmpfs, projected and hostPath volumes, a CSI-style
  bind mount, hostPort pods and unbounded-net, followed by restart, repave and
  reset. Assert no leftovers and that host data mounted into pods survives.
- The same on a host with its own nftables firewall, to verify the firewall
  returns after reset.
- Repave with `retainContainerImages: true`: images present, no containers or
  sandboxes, kubelet starts cleanly.
- A Node deleted while the daemon is stopped: the daemon must not repave when
  it starts. Deleting the Node again after kubelet registers it must repave.
- A daemon restart after it saw a Node deleted, but before the new slot is
  rendered: the repave must finish without another deletion.
- GPU coverage on `hack/agent/azure-gpu-vm`.
- A sample Flex-like plugin as a fixture.

## 6. CI

| When | Runs |
|---|---|
| Pull requests that touch `internal/agentv2`, `cmd/agent`, `pkg/agent` or `hack/agent/e2e-kind` | The existing v1 jobs, unchanged. On ubuntu2404: the v1 suites under `AGENT_PROFILE=v2` for both runtimes, and the `migration-v2` suite. |
| Nightly | The v1 suites under `AGENT_PROFILE=v2` across the full host matrix (`.github/workflows/agent-e2e-kind.yaml:98-116`) for both runtimes, the configuration suite for both runtimes, every `migration-v2` variant, and section 5 |

Each job stays within the matrix's existing time limits
(`agent-e2e-kind.yaml:98-104`). The `host` runtime joins the immutable-host
(`acl`) entry only after validation task V7.

## 7. Validation tasks

Each needs a prototype before the feature it covers is built:

| ID | Task |
|---|---|
| V1 | Every DaemonSet and component we ship works with kubelet `cgroupRoot: /unboundednode`: unbounded-net, gantry, racer, kube-proxy, the NVIDIA device plugin and node-problem-detector. Nothing may assume `/kubepods.slice`. |
| V2 | The image-preserving containerd reset ([host runtime cleanup, section 8](07-host-runtime-cleanup.md#8-wipe-and-slot-changes)) against containerd 2.1.8 (`pkg/agent/goalstates/constants.go:121`), including the CRI plugin's state directory layout. |
| V3 | The minimal NVIDIA Container Toolkit binary set for CDI mode, and how the toolkit finds its configuration in the slot directory without editing the host's. |
| V4 | SELinux labels for binaries executed from the slot directories on Azure Linux 3. |
| V5 | The oneshot prerequisites and pre-start boot hook run again on every start of the boot unit: after a host reboot, on `systemctl restart systemd-nspawn@<m>`, and when the `host` node units start again after a restart ([node runtime, section 4](03-node-runtime.md#4-host-boot-and-boot-units)). |
| V6 | On `host`, kubelet reports the boot ID mounted over `/proc/sys/kernel/random/boot_id` for `kubelet.service`, a NodeReboot produces a new Node boot ID and a `Rebooted` event, and nothing else on the node depends on kubelet's boot ID matching the kernel's. |
| V7 | The `host` runtime on an immutable host, where `/usr` is read-only: the slot layout, units and owned paths all live on writable filesystems. |
