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
- The agent runtime conformance suite, which every agent runtime passes
  ([agent runtime, section 4.3](12-agent-runtime.md#43-adding-an-agent-runtime)). Against a fake host: its units and the command line
  for each profile, the selection file, pulls by digest, image cleanup, and
  refusing a node runtime it does not support. On a real host in CI
  (section 6): the checks validation task V14 prototyped, namely each
  profile's SELinux domain and capabilities, `host-exec` mounts and
  `systemctl`, readiness and recovery, containers from one image running at
  once, every process ending with its unit, the plugin executor, and an
  installed image keeping its file owners, modes and capabilities. A fake
  agent runtime serves the `daemon` and `agentupgrade` tests.
- `ociimage`: pulls by digest and by tag; owners, setuid modes, file
  capabilities and later layers' deletions kept; a completion marker, and an
  unpack interrupted at any point starting again.
- One test per row of both `Decide` tables, with each trigger: a deletion
  event, a missing Node with a saved UID, a Node with a new UID, a Node that
  has never registered, which must not count as deleted, and the goal version
  recorded on a check whose settings match.
- `Recover` after a simulated crash between every two steps of a repave,
  including after `Repave` is saved but before the bundle is rendered.
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
  a crash, the return to v1 after a failure in each step before the commit,
  `--rollback` before and after the first repave and after a crash at each of
  its steps, and the conversion of v1's applied config, credentials, Machine
  status and host preparation. The `agent:` line of `version` for the v1 build, the v2
  build and an older v1 release.
- The in-container plugin executor of the image agent runtimes: the plugin runs as its
  plugin's UID with only its declared capabilities, its state and cache
  directories are owned by that UID, its credential directory is private to
  the call and removed after it, it runs as PID 1 of a PID namespace of its
  own so a timeout ends everything it started, and it dies with the daemon.
- Plugin protocol tests with a reference plugin: timeouts, errors, oversized
  output and ownership checks. The executor refuses an executable whose SHA-256
  does not match its distribution, a `#!` script, any other file that is not
  ELF, a dynamically linked executable, and a store with a parent directory
  that group or others can write. The transient unit's properties for each
  combination of `permissions` and `credentials`, the clean environment,
  `plugins[].env` refusing `PATH` and `LD_` names, and an audit line that never
  contains the request or response.
- Distributions in AgentUpgrade: staging from a manifest, copying the running
  distribution without one, and refusing a manifest with a wrong digest, with
  a plugin or extension added or dropped, or with a changed permission,
  credential, shared host path or cleanup declaration. Extension content from
  a new distribution reaching the node at the next provision and not before.
  `start` with changed plugins or extensions on an installed host is refused,
  and with the original file after an upgrade is not.
- Every host task run against a temporary `hostfs` root, failing on any write
  outside it ([agent runtime, section 5](12-agent-runtime.md#5-reaching-the-host)).
- State files: a write interrupted at every step leaves the old file or the
  new one, never `StateCorrupt`. Fingerprint fixtures with literal values, so
  a field added to the install-time settings cannot change the fingerprint of
  an existing install.
- Extension templates: rendering on both runtimes, where
  `KubeletKubeconfigPath` differs; a misspelled variable rejected when the
  configuration loads; an `APIServer` replaced by the bootstrap credentials
  provider showing up at the next provision; content without `template` left
  untouched.
- `agentDependencies`: the daemon drop-in's content, a warning for a unit that
  does not exist, and the drop-in removed by reset.
- MachineOperation handling without the CRD: the daemon starts, reports that it
  is waiting, keeps reconciling the node, and starts the watch once a fake API
  server begins serving the type.
- Host cleanup with a fake `SystemOps`: order of operations, `ErrNodeBusy` on a
  hung process, `ErrMountsRemain` when a process outside the node holds a
  per-slot path, the safe remover refusing to cross a bind mount inside a
  slot's state or to follow a symlink, the `active` link replaced in one
  rename, crash replay at every step of a slot change, Teardown without an
  installed record, and Teardown removing only the mount points the runtime
  created.
- Per-slot state: `Provision` with new content gives the slot new, empty state
  directories, and with the same digest leaves them as they are; content that
  existed at a standard path before the install is byte-identical after
  Teardown; a package that owns files in a node path, or ships a unit with a
  node unit's name, is reported as a warning.

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
| `AGENT_PROFILE` | `v1`, `v2`: which build of the change under test the harness installs | `v1` |
| `NODE_RUNTIME` | `nspawn-container`, `host` (only with `v2`, and never on `acl`) | `nspawn-container` |
| `AGENT_RUNTIME` | `host`; `containerd` (only with `v2`, `nspawn-container` and `HOST_BASE_OS=acl`); `systemd-nspawn` (only with `v2` and `nspawn-container`): installs the agent as a binary, or as the agent image built from the change under test and run by the host's containerd or by systemd-nspawn. For `systemd-nspawn` the harness also makes and serves the image's root file system archive. | `host` |

A profile object holds everything that differs between them, so the suites
themselves do not change:

- **Build.** For `v2`, the harness installs the v2 build. The generated
  bootstrap script and the Ignition unit run `start` and `preflight`
  (`cmd/kubectl-unbounded/app/ignition.go:188-189`), which are the v2 build's
  own commands, so the harness does not rewrite them. It still owns the
  `version` and `agent-upgrade` commands it runs on the host.
- **Configuration.** The harness generates the configuration with
  `kubectl unbounded machine manual-bootstrap`, passing `--node-runtime` and
  `--agent-runtime` for the runtimes under test ([CLI and configuration, section 2](01-cli-and-configuration.md#choosing-the-node-runtime)). v2 converts the v1 JSON
  the script carries.
- **State.** For `v2`, the harness reads install and node state from
  `status --output json`, not from v2's files
  ([CLI and configuration, section 1](01-cli-and-configuration.md#1-cli)).
- **Running a command inside the node.** `machinectl shell` (`e2e.py:637-644`)
  for `nspawn-container`; SSH to the host for `host`.
- **Names and paths:** daemon units, the reset leftover list, and the fake
  failing agent used by the rollback test.

### 2.3 Swapped checks

| Check | v1 | v2, `nspawn-container` | v2, `host` |
|---|---|---|---|
| Daemon unit and binary layout (`e2e.py:1407-1444,5615-5649`) | `unbounded-agent-daemon.service` runs `<current> daemon` | Unchanged: the same unit runs `<current> daemon`, which in the v2 build is v2's daemon, and the binary layout is the same | Same as `nspawn-container` |
| Install record (`e2e.py:3737-3797,6480`) | `install-state.json`: phase and install ID | `status`: install phase and install ID | Same |
| Applied config unchanged by a resumed bootstrap (`e2e.py:3762-3803`) | Hash of `kube1-applied-config.json` | `status`: digest of the applied settings | Same |
| Failure injected during bootstrap (`e2e.py:3735-3741`) | Blocks v1's daemon recovery script | Blocks v2's daemon recovery script | Same |
| The node's distribution matches the host's (`e2e.py:715-731`) | `/var/lib/machines/<m>/etc/os-release` | Unchanged | Not applicable: the node is the host |
| Extra host mounts and devices (`e2e.py:4150-4234`) | `.nspawn` binds and `DeviceAllow` | Unchanged | Not applicable: these settings are rejected for `host` ([CLI and configuration, section 2](01-cli-and-configuration.md#2-configuration)), so the scenario runs only on `nspawn-container` |
| A device that appears at boot is usable in the node (`e2e.py:4312-4394`) | `.nspawn` and override rewritten | Unchanged | The device is read from the host directly |
| Boot wiring (`e2e.py:5599-5607`) | The override runs `nspawn-lifecycle post-start` | The override runs `boot-hook post-start` | Not applicable |
| A repave switches machines (`e2e.py:734-747,6077-6091`) | `kube1` to `kube2`, with a bpffs sentinel | Unchanged: slots `a` and `b` are `kube1` and `kube2` | `status`: the enabled slot switches |
| NodeReboot (`e2e.py:5415-5452`) | New Node boot ID and a `Rebooted` event | Unchanged | Unchanged ([node runtime, section 5.2](03-node-runtime.md#52-host)) |
| AgentUpgrade rollback (`e2e.py:1515`) | The fake failing agent answers `version` | It also prints `agent: v2`, so the test still reaches the failing-daemon path | Same |
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
cloud-init state and SSH host keys. The allowlist also covers `sysctl` keys
that change on their own, without the agent: for example `kernel.random.uuid`,
which is different on every read, `kernel.random.entropy_avail`,
`kernel.ns_last_pid`, `fs.dentry-state`, `fs.inode-nr`, `fs.inode-state` and
`fs.file-nr`. Without it, the comparison fails on every run. Installed packages
are listed in the reset report and allowed. This tests [host changes, section 3](06-host-changes.md#3-host-preparation-and-its-reversal) and
[host runtime cleanup](07-host-runtime-cleanup.md) directly.

On a host migrated from v1, the difference must be exactly the v1 host
preparation changes that the reset reports as left in place
([migrating from v1, section 7](10-migration-from-v1.md#7-converting-v1s-state)).

## 4. Migration suite

A new `migration-v2` suite follows the existing host-root `migration` suite
(`e2e.py:6504-6509`). Migrating early and the cutover are the same step, an
upgrade to the v2 build ([migrating from v1, section 2.1](10-migration-from-v1.md#21-how-a-host-migrates)), so one suite covers
both:

1. Stage host files, as the existing suite does, and take the host snapshot.
2. Install v1: the release in `LEGACY_AGENT_VERSION`, or the v1 build of the
   change under test. Record a checksum of every v1 file and unit, and of the
   v1 binary.
3. Wait for the node, run the workload, and record the Node UID, the machine's
   PID, and the workload pods' UIDs and restart counts.
4. Migrate with an AgentUpgrade to the v2 build of the change under test. A
   variant runs `agent-upgrade` with the v2 build on the host instead.
5. Check that:
   - the operation is `Complete`, and was published only after the v2 daemon
     reported ready;
   - the Node UID, machine PID, pod UIDs and restart counts are unchanged;
   - v1's detection files are gone ([v1 compatibility, section 4](08-v1-compatibility.md#4-existing-packages-used-as-libraries)), so v1
     `start` refuses the host;
   - `last-good` names the v2 binary, and the backup holds the v1 binary;
   - `status` shows a complete install on `nspawn-container`, with the
     running machine as the enabled slot;
   - the staged files are untouched.
6. Reboot the host. The node comes back through v2's boot wiring.
7. Run the v1 lifecycle steps under the `v2` profile: NodeReboot,
   AgentUpgrade in place, AgentUpgrade rollback, the workload and a repave.
   After the repave, the v1 backup is gone and `migrate --rollback` is
   refused.
8. Reset, and compare with the snapshot (section 3).

Variants:

| Variant | What it checks |
|---|---|
| Release matrix | Step 2 runs once for each v1 release from the oldest supported one, at least v0.13.0, to the latest. The oldest release in this matrix is the oldest supported v1 release. A host on v0.12.x is refused before anything changes ([migrating from v1, section 2](10-migration-from-v1.md#2-release-plan-and-cutover)). |
| Interrupted | For each phase in [migrating from v1, section 6](10-migration-from-v1.md#6-steps), and each step of the commit and of the rollback, a helper on the host watches the migration record and kills the migrating process, or reboots the host, when the step is reached. The node must keep running, or boot with one complete machine override, and the migration or rollback must then finish on its own. During a rollback, a reboot before v1's daemon unit is back must start v2, never v1, and a finished rollback must leave `last-good` naming the v1 binary. This is done from outside, without failure points in the product, as the existing bootstrap-recovery test does (`e2e.py:3732-3741`). |
| Failed | In each step before the commit, a v2 path is made unwritable, or the checks before the commit are made to fail, including with the API server unreachable. The daemon returns `current` to the v1 binary, v1's daemon runs, v1's files match their checksums, the node is unchanged, and the operation is `Failed` with reason `DaemonFailed` and v2's error as its message. |
| Stale preparation | Make v2's daemon crash repeatedly while the record is `Preparing`, so v1's recovery unit returns the host to v1 with the record left behind. Delete the Node so v1 repaves into the other machine, then upgrade to the v2 build again. v2 discards the old record and staged files, runs the checks again, and adopts the machine that is now running ([migrating from v1, section 6.3](10-migration-from-v1.md#63-after-a-crash-or-reboot)). |
| Refused | An upgrade to the v2 build changes nothing of v1's on a v1 host with an unfinished install, with two applied configs, or on a release older than the oldest supported one, and the operation is `Failed`. The v2 build's `start` on a v1 host is refused. |
| Rollback | Migrate, then `migrate --rollback`. v1's daemon runs, v1's files and the v1 binary match the checksums from step 2, `current` and `last-good` name that binary, and the node is unchanged. The v1 NodeReboot and AgentUpgrade steps pass. Migrating again succeeds. A second variant makes the restored v1 daemon fail repeatedly: the recovery unit restarts v1, never v2. |
| v1 build refused | After migrating, an AgentUpgrade to a v1 build, and one to the v1 release the host ran before, are rejected before the switch, both inside the rollback window and after a repave |
| Crash loop after the commit | After migrating, make the v2 daemon fail repeatedly, then do the same after a repave. Each time, the recovery unit restores a v2 binary, never the v1 one, and the daemon runs again once the fault is removed ([migrating from v1, section 2](10-migration-from-v1.md#2-release-plan-and-cutover)). `migrate --rollback` still works while the daemon cannot start. |
| Readiness timeout | After migrating, make an AgentUpgrade's new daemon hang before it reports ready. The start times out three times, the recovery unit restores the previous binary, and the operation is `Failed` ([lifecycles, section 5](05-lifecycles.md#5-agentupgrade)). |
| API server unreachable | After migrating, block the API server and restart the daemon: it reports ready and catches up once the API server is back, with no binary rollback. Repeat right after an AgentUpgrade: the new binary is rolled back and the operation is `Failed` once the API server is back. |
| Limited mode | After migrating, make the node state disagree with the host and restart the daemon: it reports ready, shows `AgentHealthy=False` with `StateMismatch`, changes nothing on a Node deletion, and runs an AgentReset. Repeat right after an AgentUpgrade: the new binary is rolled back. |
| To `host` | After migrating, set the Machine's `spec.agent.nodeRuntime` to `host` and check that `ReinstallPending` is `True` with reason `NodeRuntimeChanged`. Then `reset`, bootstrap again with `manual-bootstrap --node-runtime host`, check that `ReinstallPending` is `False` and `status.agent.nodeRuntime` is `host`, and run the lifecycle suite under `NODE_RUNTIME=host`. On every host but `acl`, where the `host` runtime is not supported. |
| Immutable host | The cluster-started migration on an Ignition host (`HOST_BASE_OS=acl`), which needs no access to the host |
| Adopted machine | `migrate --adopt-machine` on hosts built by a pinned AKS Flex Node release, not a fixture that mimics one: once on Ubuntu with the host install, after a Flex repave so that its `config.json` no longer names the running Kubernetes version, and once on ACL with the container install of Azure/AKSFlexNode#370. Each is started from the cluster with a Flex AgentUpgrade to a bridge ([migrating from v1, section 9.2](10-migration-from-v1.md#92-starting-it-a-bridge)). The checks warn that the host's kubelet will be masked; the host's containerd keeps running; Flex's units and replaced units are backed up and removed; the node keeps running; `migrate --rollback` restores Flex and runs its `handBack`; a configuration whose Kubernetes version differs from the machine's is refused; and `reset` reverses what v2 changed. |

The suite runs against the v2 build of each change, and nightly also against
the published v2 build of each release, because a build from source cannot
prove that the released binary behaves the same.

## 5. Further end-to-end tests

These cover what v1 never had, so the v1 suites cannot:

- A `host` workload set with tmpfs, projected and hostPath volumes, a CSI-style
  bind mount, hostPort pods and unbounded-net, followed by restart, repave and
  reset. Assert no leftovers and that host data mounted into pods survives.
- The same on a host with its own nftables firewall, to verify the firewall
  returns after reset.
- Repave with `retainContainerImages: true`: images present, no containers or
  sandboxes, kubelet starts cleanly.
- A Node deleted, with drift, while the daemon is stopped: the daemon must
  repave when it starts. In a second run, the host also reboots before the
  daemon starts, so kubelet has registered a new Node: the daemon must repave
  because the UID changed. In a third, nothing drifted: the daemon must not
  repave, and must record the new Node's UID.
- On a Machine whose `configurationRef` names the version the node was built
  from, `status.configuration` and `RepavePending=False` are set shortly after
  the daemon starts, without a repave.
- A daemon restart after it saw a Node deleted, but before the new slot is
  rendered: the repave must finish without another deletion.
- GPU coverage on `hack/agent/azure-gpu-vm`.
- A sample Flex-like plugin as a fixture, statically linked, with the
  following checks:
  - Run with default permissions, it cannot read `/etc/unbounded/agent-v2`,
    can write only its own state and cache directories, which persist across
    calls, reads a root-only secret through its declared credential, cannot
    see host processes, and runs as a dynamic user with no capabilities.
  - It serves as kubelet's exec credential: `Provision` copies it into the
    node, and after an upgrade that replaces it, and after a rollback, the
    running node's copy matches the running distribution without a repave.
    kubelet keeps renewing its credentials while the daemon is stopped.
  - An AgentUpgrade with a distribution manifest updates it with the binary,
    and under an image agent runtime an upgrade to a new image does the same. A second
    upgrade, whose plugin no longer answers a method the configuration uses,
    never reports ready, and the recovery unit restores the previous agent and
    plugin together.
  - A plugin changed in place in the store fails every call with a digest
    error, and the audit log records it.
- A reset with a plugin status reporter and an `agent.reset.after` hook: the
  hook runs and the result is reported before the plugin store is removed.

## 6. CI

| When | Runs |
|---|---|
| Pull requests that touch `internal/agentv2`, `cmd/agent`, `pkg/agent` or `hack/agent/e2e-kind` | Both builds are built, and the agent image from the v2 build. The existing v1 jobs, unchanged, against the v1 build. On ubuntu2404: the v1 suites against the v2 build (`AGENT_PROFILE=v2`) for both node runtimes, and the `migration-v2` suite (section 4). On the `acl` entry, where its credentials are configured: the lifecycle suite under `AGENT_RUNTIME=containerd` and `AGENT_RUNTIME=systemd-nspawn`. All of these are required to merge, from the first pull request that adds v2 code. |
| Nightly | The v1 suites under `AGENT_PROFILE=v2` across the full host matrix (`.github/workflows/agent-e2e-kind.yaml:98-116`) for both node runtimes, and under `AGENT_RUNTIME=containerd` and `AGENT_RUNTIME=systemd-nspawn` on `acl` with SELinux enforcing; the agent runtime conformance suite on a real host for each agent runtime (section 1); the configuration suite for both node runtimes; every `migration-v2` variant; all against both the change's v2 build and image and the published ones of each release; and section 5. After each `containerd` run, the daemon container's writable layer must be empty; `systemd-nspawn` runs it read-only ([agent runtime, section 5](12-agent-runtime.md#5-reaching-the-host)). |

The v2 jobs are required from the start, not only once v2 is official. The
experimental releases are what operators migrate early, and v2 code that
breaks a v1 job, or a v2 job, would otherwise reach them.

Each job stays within the matrix's existing time limits
(`agent-e2e-kind.yaml:98-104`).

Every host in the matrix, including the immutable-host (`acl`) entry, runs both
node runtimes' and every agent runtime's capability probes ([node runtime, section 5.3](03-node-runtime.md#53-host-capabilities)) and checks each result
against what is expected for that host. A runtime's lifecycle suites run on a
host only where its probes pass; where they fail, the job checks that `start`
refuses with the failing probe in its error. On `acl`, the `host` runtime's
probes must fail, on the host's containerd, and both image agent runtimes' must pass.

From the first official v2 release, the v1 build is no longer built or
published, so the `AGENT_PROFILE=v1` jobs are removed. The v2 jobs stay
required and replace them, and `AGENT_PROFILE` defaults to `v2`. The
`migration-v2` suite then installs v1 only from `LEGACY_AGENT_VERSION`
(section 4).

## 7. Validation tasks

Each needs a prototype before the feature it covers is built:

| ID | Task |
|---|---|
| V1 | Every DaemonSet and component we ship works on a `host` node: unbounded-net, gantry, racer, kube-proxy, the NVIDIA device plugin and node-problem-detector. **Changed by V10:** kubelet keeps its default cgroup root, so pods are under the usual `kubepods.slice` and the original concern, components that assume it, no longer applies. |
| V2 | The image-preserving containerd reset ([host runtime cleanup, section 8](07-host-runtime-cleanup.md#8-slot-changes)) against containerd 2.1.8 (`pkg/agent/goalstates/constants.go:121`), including the CRI plugin's state directory layout. |
| V3 | The minimal NVIDIA Container Toolkit binary set for CDI mode, and how the toolkit finds its configuration in the slot directory without editing the host's. |
| V4 | The SELinux labels the `host` runtime gives binaries in the slot directories on hosts with an enforcing policy, such as Azure Linux 3, so the exec capability probe passes ([node runtime, section 5.3](03-node-runtime.md#53-host-capabilities)). |
| V5 | The oneshot prerequisites and pre-start boot hook run once per node start, and only then: after a host reboot, on `systemctl restart systemd-nspawn@<m>`, and on a `host` `Start` or `Restart`. On `host`, killing kubelet or containerd so that systemd restarts it must not run the nftables flush or the pre-start boot hook ([node runtime, section 4](03-node-runtime.md#4-host-boot-and-boot-units)). |
| V6 | On `host`, kubelet reports the boot ID mounted over `/proc/sys/kernel/random/boot_id` for `kubelet.service`, a NodeReboot produces a new Node boot ID and a `Rebooted` event, a kubelet that systemd restarts on its own keeps the same boot ID, nothing else on the node depends on kubelet's boot ID matching the kernel's, and secret, projected and in-memory `emptyDir` volumes have their contents inside containers. **Prototype result:** with a shared host root, a unit given `BindReadOnlyPaths=` over the boot ID kept its tmpfs mounts from reaching the host, so containerd would have bound empty directories; the wrapper in [node runtime, section 5.2](03-node-runtime.md#52-host) gave the unit the fake boot ID, left the host's unchanged, and let mounts and unmounts reach the host, on systemd 249, 252 and 255. The kubelet and pod checks remain. |
| V7 | For every image in the host matrix, which of the `host` runtime's node paths ([host runtime cleanup, section 4](07-host-runtime-cleanup.md#4-node-paths)) exist on a fresh host, what is in them, and which packages own files there or ship `containerd.service` or `kubelet.service`. The result says how often the warnings in [host runtime cleanup, section 3.1](07-host-runtime-cleanup.md#31-packages-that-use-the-nodes-paths-or-unit-names) appear. |
| V8 | The `host` agent runtime's plugin executor ([extensions, section 3](02-extensions.md#3-plugin-protocol)) gives every sandbox property it promises, called directly and from a separate mount namespace through `chroot`. **Prototype result:** on Ubuntu 22.04, Ubuntu 24.04 and AlmaLinux 9 (systemd 249, 255 and 252) in containers, and on an Ubuntu 24.04 VM: a stable dynamic UID; no capabilities and an empty bounding set; `no_new_privs`; the agent's paths hidden; a root-only file unreadable and `ReadWritePaths=` on a root-owned directory not writable for a dynamic user; state and cache that persist across calls; `LoadCredential=` delivering a root-only secret (on the VM; systemd in a container did not create credential directories); host processes hidden; a private `/tmp`; no leaked environment; a timeout ending the call; a daemonized child ending with it; a store under an inaccessible path refusing to run; a root plugin with exactly its declared capability and write path. 47 to 69 ms per call. On ACL 3.0.20261026 with SELinux enforcing, every property held for calls from the host and from a privileged container running as `spc_t` through `chroot /host`; the store under `/opt` is labeled `usr_t`; about 95 ms per call from the host. |
| V9 | How a daemon that runs in a container through `ctr run` reports readiness to its unit ([agent runtime, section 3.1](12-agent-runtime.md#31-readiness)). **Prototype result:** with the container in a scope of its own, as Azure/AKSFlexNode#370 runs it, `Type=notify` timed out because systemd ignores the notification. A readiness file waited for in `ExecStartPost=` worked on systemd 249 and 255, and a daemon that never became ready tripped the start limit and the recovery unit, leaving no container. Putting the container's cgroup under the unit worked only with `DelegateSubgroup=` (systemd 254 and later); without it runc could not enter the cgroup. On ACL 3.0.20261026 with SELinux enforcing the results were the same: `Type=notify` timed out, the readiness file and `DelegateSubgroup=supervisor` both worked, and recovery ran for a daemon that never became ready. The design uses the readiness file and never `sd_notify` ([control and state, section 5](04-control-and-state.md#5-daemon-reconcile)). |
| V10 | The `host` runtime's per-slot node state ([host runtime cleanup, section 3](07-host-runtime-cleanup.md#3-node-state-per-slot)). **Prototype result:** on an Ubuntu 24.04 VM with kubelet 1.33.13 and containerd 2.1.8, standalone with static pods: `/var/lib/kubelet` was a shared mount; in-memory `emptyDir` and downward API volumes reached their containers; a pod's Bidirectional mount under `/var/lib/kubelet/plugins` reached the host; kubelet reported node and image file system statistics; a process outside the node holding `/var/lib/kubelet` kept its mount, and once it exited, stopping the mount units left no mount under any node path; a slot change gave kubelet empty state and the old slot's tree held no mounts and was removed; a reboot right after the `active` link changed brought the node back from the slot it named; after Teardown, existing `/opt/cni/bin` and `/etc/cni/net.d` content was byte-identical, `dpkg --verify` was unchanged and the mount points the units created were gone. Two results changed the design: kubelet refused `cgroupRoot: /unboundednode` for a missing `hugetlb` controller, and reinstalling Ubuntu's `containerd` package restarted the node's containerd through its maintainer scripts. |
| V11 | The `containerd` agent runtime end to end on Azure Container Linux with SELinux enforcing: bootstrap, repave, NodeReboot, AgentUpgrade and its rollback, reset and rejoin, with the daemon container's writable layer empty afterwards ([agent runtime, section 5](12-agent-runtime.md#5-reaching-the-host)). Needs the v2 implementation; Azure/AKSFlexNode#370's harness shows the same model working for the Flex agent. **Prototype result, in part:** on ACL 3.0.20261026 with SELinux enforcing, from a privileged `ctr run` container running as `spc_t`, `host-exec` as [agent runtime, section 5](12-agent-runtime.md#5-reaching-the-host) gives it worked: a mount through `chroot /host` and `nsenter` reached the host, and `systemctl` through the system bus ran `reset-failed` on a unit backed by a unit file, `daemon-reload` and `start`, with no denials. |
| V12 | Which processes may run `systemctl reset-failed` on Azure Container Linux, which recovery and upgrades use to clear the daemon unit's start limit (Azure/AKSFlexNode#385). **Prototype result:** on ACL 3.0.20261026 with SELinux enforcing, a host process, running as `kernel_t` like every host process there, was denied for units backed by a unit file in `/etc/systemd/system`: the denial is `{ reload }` on `tclass=service`, from `bus_unit_method_reset_failed`, whether the file was `unlabeled_t`, as written, or relabeled with `restorecon` to the policy's `systemd_unit_t`. The same call was allowed for transient units, and from a privileged container running as `spc_t` for either. The image agent runtimes therefore avoid the limit; the `host` agent runtime on ACL needs an ACL policy change ([node runtime, section 5.3](03-node-runtime.md#53-host-capabilities), [agent runtime, section 10](12-agent-runtime.md#10-host-requirements)). |
| V13 | The in-container plugin executor of the image agent runtimes ([agent runtime, section 6](12-agent-runtime.md#6-plugins)) on Azure Container Linux with SELinux enforcing, from the daemon's privileged container: the UID and capability drop, state and cache directories under `/host` owned by the plugin's UID, the private credential directory, a timeout ending everything the plugin started, the plugin dying with the daemon, and an unprivileged plugin unable to read the agent's credentials under `/host`. **Prototype result:** on ACL 3.0.20261026 with SELinux enforcing, from a privileged container run by the host's containerd: the plugin ran as its own UID with no effective capabilities, or exactly `CAP_NET_ADMIN` as an ambient capability when declared, and with `no_new_privs` already set; it could not read the agent's configuration or a root-only consumer file under `/host`, could read a world-readable one, and could not write a root-owned directory; its credential arrived; its state directory on the host was owned by its UID and persisted; its environment held only the five variables; a timeout ended the call within 3 seconds; with `PR_SET_PDEATHSIG` and a PID namespace it died with the executor. Without a PID namespace, a child it daemonized outlived the call, so the executor gives each plugin one. Host processes are visible to it. |
| V14 | The `systemd-nspawn` agent runtime end to end on Azure Container Linux with SELinux enforcing, as V11 does for `containerd`: bootstrap from a root file system archive, repave, NodeReboot, AgentUpgrade and its rollback, reset and rejoin ([agent runtime, section 4.2](12-agent-runtime.md#42-systemd-nspawn)), and then the same on the first other operating system it targets. Needs the v2 implementation. **Prototype results,** on ACL 3.0.20261026, systemd 255, with a stand-in agent and image. *Running the image:* with systemd-nspawn's defaults the payload ran as `spc_t`, in a private PID namespace, on the host's network, and lacked `CAP_NET_ADMIN` until `--capability=all`; `--register=no` kept it out of `machinectl list`; `--notify-ready=yes` with `Type=notify`, the readiness file, recovery after a daemon that never became ready, the end of every process when the unit stopped, and the in-container plugin executor all worked; a start took about 140 ms, against about 630 ms for `ctr run`. *Installing and upgrading:* Ignition's `verification.hash` and a bootstrap unit with `curl`, `sha256sum -c` and `tar` each installed a 28 MB root file system archive, and each refused a wrong hash, leaving nothing behind; `start` then pulled the current image by digest and started the daemon, in about 2.6 s in all; the daemon upgraded itself by digest in about 1.8 s and kept the old image, and refused a digest the registry did not have; reset kept the install image, so joining again needed only the registry. Owners, setuid modes, file capabilities and later layers' deletions survived `podman export` with `tar`, and the agent's own unpack. Trees the agent unpacked were `var_lib_t` and ran as `kernel_t` until `--selinux-context=` gave `spc_t`, which held after `restorecon`. A writable container was refused while another ran from the same tree; read-only ones shared it, each with a scope of its own. A read-only image needs `/host`, `/etc/resolv.conf` and `/etc/localtime` to exist, and a `/etc/resolv.conf` made by a `RUN` step is not kept in the image. `machinectl pull-tar` worked, but checks only against the server's own checksum file. *Reaching the host:* `host-exec` as [agent runtime, section 5](12-agent-runtime.md#5-reaching-the-host) gives it worked from the `systemd-nspawn` container, including `reset-failed` through the system bus, with no denials; systemd's private socket refused it. Boot hooks ran as `systemd-nspawn` containers around a node machine while the daemon ran from the same image: the pre-start hook as its own unit, and the post-start hook from the machine unit's `ExecStartPost=` with `--keep-unit`, under the machine unit's device policy. With `--no-new-privileges=yes` the container still ran as `spc_t`, with `no_new_privs` set, and `host-exec` still worked. |
| V15 | v2's copy of the OCI unpack against v1's ([v1 compatibility, section 2](08-v1-compatibility.md#2-behavior-changes-from-v1)): every node image the repository builds (`images/agent-*`) unpacked both ways and compared. The trees may differ only in owners, the `user.rootlesscontainers` xattr and device nodes, and a node started from v2's unpack passes the lifecycle suite. **Prototype result:** on a test image, v1's `DownloadRootFS` failed for a reference by digest with `tag "sha256:..." not found in OCI layout`, and by tag it gave root a file the image gives UID 1234, keeping 1234 only in `user.rootlesscontainers`. Pulling under a fixed local tag and unpacking without rootless mode unpacked the reference by digest, and kept that owner, a setuid mode, a file capability and a later layer's deletion. |
