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
  migrate/        v2 migrate: checks, conversion of v1 state, rollback
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

## 3. Risks

- **The `host` runtime has no containment boundary.** Repave and reset must
  stop pods, unmount volumes and remove state explicitly. Mistakes leave stuck
  mounts or orphaned processes. This area needs the most end-to-end coverage.
- **Workloads see the real host** on the `host` runtime ([node runtime, section 5.2](03-node-runtime.md#52-host)).
- **Plugins run consumer code on every host.** Each runs only with the
  SHA-256 pinned in the configuration, from the descriptor that was checked,
  in a sandboxed unit as an unprivileged user unless it declares more, with a
  clean environment and an audit line per call. A plugin that declares
  `root: true` or capabilities is trusted with them
  ([extensions, section 3](02-extensions.md#3-plugin-protocol)).
- **A plugin's shared libraries are not pinned.** Only the executable is.
  Statically linked plugins avoid this, and `v2 preflight` warns about the
  others.
- **Agent and plugin versions could skew.** Plugins change only with an
  AgentUpgrade, which stages and rolls them back with the binary, and the
  daemon checks each plugin with `describe` before it reports ready
  ([lifecycles, section 5](05-lifecycles.md#5-agentupgrade)).
- **Experimental code ships in the production binary.** It is reachable only
  through `v2` commands.
- **Copied code drifts from v1.** The list in [v1 compatibility, section 3](08-v1-compatibility.md#3-code-copied-from-v1) is the mitigation.
- **Migration takes over running production nodes.** A bug could disrupt a
  node that was working. Before the commit, v2 changes nothing of v1's, so a
  failure returns the host to whichever v1 binary ran it. The commit is short,
  local and always finished. The rollback window lasts until the first repave,
  and the migration suite runs every phase, interrupted and failed, from every
  supported v1 release
  ([migrating from v1](10-migration-from-v1.md), [testing, section 4](11-testing.md#4-migration-suite)).
- **v1's on-host formats differ between releases.** The conversion reads what
  each supported release writes. The migration test matrix sets the oldest
  supported v1 release, and an older host is refused before anything changes
  ([migrating from v1, section 2](10-migration-from-v1.md#2-release-plan-and-cutover)).

## 4. Production readiness

v2 is built to production standard from its first release. The experimental
releases mark only what may still change: the configuration format, the plugin
protocol and the on-host layout. Nothing in this design is simplified on the
grounds that v2 is experimental.

The first official v2 release replaces v1 on every host
([migrating from v1, section 2](10-migration-from-v1.md#2-release-plan-and-cutover)), so it is cut only when all of these hold. The
release's tracking issue lists each one with links to the evidence.

| Criterion | Met when |
|---|---|
| Validation tasks | V1 to V6 have prototypes, their results are written back into this design, and the design has changed wherever a result required it ([testing, section 7](11-testing.md#7-validation-tasks)) |
| CI | The v2 jobs have been required on every pull request that touches the agent since the first v2 code merged, and pass nightly on the full host matrix for both runtimes ([testing, section 6](11-testing.md#6-ci)) |
| Migration matrix | The cutover matrix passes against the release builds, from every v1 release back to the oldest supported one, together with every interrupted and failed variant. The release notes name the oldest supported v1 release ([testing, section 4](11-testing.md#4-migration-suite)). |
| Capability probes | Every host in the matrix gives the expected probe results for both runtimes, and a host that fails a probe is refused with the probe in the error ([node runtime, section 5.3](03-node-runtime.md#53-host-capabilities)) |
| Documentation | The public site under `docs/` covers the configuration format, writing plugins (the protocol, permissions and plugin manifests), the AgentUpgrade parameters, the two runtimes and their capability probes, what v2 creates on a host, and migrating and rolling back. The experimental warning and the `v2` command group's "unstable" label are removed in the same release. |
