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
- **Plugins run as root.** Path and ownership checks are the main defense.
- **Agent and plugin versions can skew.** Only protocol negotiation guards
  against it.
- **Experimental code ships in the production binary.** It is reachable only
  through `v2` commands.
- **Copied code drifts from v1.** The list in [v1 compatibility, section 3](08-v1-compatibility.md#3-code-copied-from-v1) is the mitigation.
- **Migration takes over running production nodes.** A bug could disrupt a
  node that was working. Automatic rollback before commit, the rollback window
  until the first repave, and the migration suite are the mitigations
  ([migrating from v1](10-migration-from-v1.md), [testing, section 4](11-testing.md#4-migration-suite)).
