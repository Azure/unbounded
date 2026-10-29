# Unbounded Agent v2: Implementation

Part of the [Unbounded Agent v2 design](README.md). The README has the status,
repository baseline, citation conventions and terms.

## 1. Packages

```
cmd/agent/internal/cmd/v2.go     registers the "v2" group, delegates to internal/agentv2/cli
internal/agentv2/
  cli/            cobra commands and the experimental warning
  config/         AgentConfiguration v1alpha1, defaults, validation, v1 JSON conversion
  nodespec/       NodeSpec and Resolve
  bundle/         NodeBundle, Layout, Render (pure), templates
  runtime/        NodeRuntime interface, types, errors, registry
    runtimetest/  fake runtime and conformance suite
    host/         the host runtime, including cleanup (07-host-runtime-cleanup.md)
    nspawn/       the nspawn-container runtime
  hostprep/       host preparation tasks, requirement merging, the undo log
  hostnet/        host network reset, used for both runtimes
  artifacts/      cache, download, verification, offline sources
  state/          install record, node state, locks
  bootstrap/      install check and stages
  nodemanager/    Ensure, Apply, Reboot, Reset, Recover, Decide
  control/        goal sources, status reporters, operation source (machina, static, plugin)
  credentials/    bootstrap (static, tpm, plugin), kubelet, daemon (csr, kubeconfig, plugin)
  daemon/         controller wiring, startup gate, operation handlers, unit assets
  agentupgrade/   MachineOperation and CLI upgrade over pkg/agent/agentbinary
  plugin/         exec client, protocol types, describe negotiation
  hooks/          event runner
  extensions/     validation, resolution, merge into bundle and hostprep
  preflight/      check composition
```

## 2. Export policy

1. Everything starts in `internal/agentv2`.
2. Moving a package to `pkg/agentv2/<area>/<version>` requires a decision
   recorded in a design doc under `designs/`, stating: the consumer and why the binary
   hook points are not enough, the exact API surface, the stability level
   (alpha, beta or stable) and compatibility promise, and the owner.
3. Export types before behavior. The likely first candidates are the plugin
   protocol types (`plugin/v1alpha1`) and the configuration types
   (`config/v1alpha1`), so plugin authors and configuration generators get
   typed structs.
4. The node manager, the runtime implementations and the state store are never
   exported.
5. `pkg/agent` is frozen for v1 and its current consumers; no v2 features land
   there. The one exception is the additive v2 detection check in [v1 compatibility, section 4](08-v1-compatibility.md#4-existing-packages-used-as-libraries).
   `pkg/agent` is deprecated once AKS Flex Node has moved to the binary.

## 3. Testing and validation

Unit and conformance tests:

- The runtime conformance suite, plus a fake runtime for `nodemanager` and
  `daemon` tests.
- One test per row of the `Decide` table, including crash-recovery states.
- Golden tests showing the `nspawn-container` runtime renders kubelet,
  containerd, `.nspawn` and unit-override files identical to v1's goldens
  (`pkg/agent/phases/rootfs/testdata`).
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

End-to-end tests, using the `hack/agent/e2e-kind` and `hack/agent/qemu`
harnesses with `runtime.type` as a test dimension:

- Bootstrap, reboot, repave and reset for both runtimes.
- A `host` workload set with tmpfs, projected and hostPath volumes, a CSI-style
  bind mount, hostPort pods and unbounded-net, followed by restart, repave and
  reset. Assert no leftovers and that host data mounted into pods survives.
- The same on a host with its own nftables firewall, to verify the firewall
  returns after reset.
- Repave with `retainContainerImages: true`: images present, no containers or
  sandboxes, kubelet starts cleanly.
- GPU coverage on `hack/agent/azure-gpu-vm`.
- A sample Flex-like plugin as a fixture.

Validation tasks. Each needs a prototype before the feature it covers is
built:

| ID | Task |
|---|---|
| V1 | Every DaemonSet and component we ship works with kubelet `cgroupRoot: /unboundednode`: unbounded-net, gantry, racer, kube-proxy, the NVIDIA device plugin and node-problem-detector. Nothing may assume `/kubepods.slice`. |
| V2 | The image-preserving containerd reset ([host runtime cleanup, section 8](07-host-runtime-cleanup.md#8-wipe-and-slot-changes)) against containerd 2.1.8 (`pkg/agent/goalstates/constants.go:121`), including the CRI plugin's state directory layout. |
| V3 | The minimal NVIDIA Container Toolkit binary set for CDI mode, and how the toolkit finds its configuration in the slot directory without editing the host's. |
| V4 | SELinux labels for binaries executed from the slot directories on Azure Linux 3. |
| V5 | The oneshot prerequisites and pre-start boot hook run again on every start of the boot unit: after a host reboot, on `systemctl restart systemd-nspawn@<m>`, and when the `host` node units start again after a restart ([node runtime, section 4](03-node-runtime.md#4-host-boot-and-boot-units)). |

## 4. Risks

- **The `host` runtime has no containment boundary.** Repave and reset must
  stop pods, unmount volumes and remove state explicitly. Mistakes leave stuck
  mounts or orphaned processes. This area needs the most end-to-end coverage.
- **Workloads see the real host** on the `host` runtime ([node runtime, section 5.2](03-node-runtime.md#52-host)).
- **Plugins run as root.** Path and ownership checks are the main defense.
- **Agent and plugin versions can skew.** Only protocol negotiation guards
  against it.
- **Experimental code ships in the production binary.** It is reachable only
  through `v2` commands.
- **Copied code drifts from v1.** The list in [v1 compatibility, section 3](08-v1-compatibility.md#3-code-copied-from-v1) is the mitigation.
