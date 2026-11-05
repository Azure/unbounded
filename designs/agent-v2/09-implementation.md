# Unbounded Agent v2: Implementation

Part of the [Unbounded Agent v2 design](README.md). The README has the status,
repository baseline, citation conventions and terms.

## 1. Packages

```
cmd/agent/internal/cmd/
  commands_v1.go  //go:build !agentv2: registers v1's commands, as cmd.go:30-39 does today
  commands_v2.go  //go:build agentv2: registers internal/agentv2/cli's commands instead
internal/agentv2/
  cli/            cobra commands and the experimental warning
  config/         AgentConfiguration v1alpha1, defaults, validation, v1 JSON conversion
  nodespec/       NodeSpec and Resolve
  bundle/         NodeBundle, Layout, Render (pure), templates
  runtime/        NodeRuntime interface, types, errors, registry
    runtimetest/  fake node runtime and conformance suite
    host/         the host node runtime, including cleanup (07-host-runtime-cleanup.md)
    nspawn/       the nspawn-container node runtime
  agentruntime/   AgentRuntime interface, the supported node runtimes of each, registry
    agentruntimetest/  conformance suite for every agent runtime
    host/         blue/green binaries, the plugin store and transient-unit plugin executor
    image/        shared by the image agent runtimes: units by profile, selection file,
                  host-exec, AgentUpgrade by image, image cleanup, the in-container plugin executor
    containerd/   image store, command lines and probes for containerd
    systemdnspawn/ image store, command lines and probes for systemd-nspawn
  ociimage/       pull by digest and unpack, copied from pkg/agent with two fixes (08 section 3)
  hostprep/       host preparation tasks, requirement merging, the undo log
  hostnet/        host network reset, used for both runtimes
  hostfs/         every host file access, rooted at / under the host agent runtime and /host under the image agent runtimes
  artifacts/      cache, download, verification, offline sources
  state/          install record, node state, locks
  bootstrap/      install check and stages
  nodemanager/    Ensure, Apply, Reboot, Reset, Recover, Decide
  control/        goal sources and status reporters (machina, static, plugin), the MachineOperation source
  credentials/    bootstrap (static, tpm, plugin), kubelet, daemon (csr, kubeconfig, plugin)
  daemon/         controller wiring, startup gate, operation handlers, unit assets
  agentupgrade/   MachineOperation and CLI upgrade over pkg/agent/agentbinary and agentruntime
  migrate/        migration run by the daemon, migrate --rollback and --adopt-machine
  plugin/         protocol types, the executor interface, describe negotiation
  hooks/          event runner
  extensions/     validation, resolution, distributions, merge into bundle and hostprep
  preflight/      check composition
images/agent-v2/  the agent image
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
- **Plugins run consumer code on every host.** Under the `host` agent runtime each
  runs only with the SHA-256 of its distribution, from a root-owned store, in a
  sandboxed transient unit as an unprivileged user of its own unless it
  declares more, with a clean environment and an audit line per call. Under
  an image agent runtime the consumer builds the image, so the image digest is the
  trust boundary: plugins run inside the agent's container as unprivileged
  UIDs of their own, with no file system sandbox. A plugin that declares
  `root: true` or capabilities is trusted with them
  ([extensions, section 3](02-extensions.md#3-plugin-protocol), [agent runtime, section 6](12-agent-runtime.md#6-plugins)).
- **OS packages can use the node's paths or unit names.** v2 does not control
  the OS images it runs on. The `host` runtime binds each slot's state over the
  standard paths and never deletes what is underneath, so a package's files
  are safe, but a package upgrade writes into the slot's state, and a package
  that ships `containerd.service` restarts the node's containerd (validation
  task V10). These hosts are accepted with a warning
  ([host runtime cleanup, section 3.1](07-host-runtime-cleanup.md#31-packages-that-use-the-nodes-paths-or-unit-names)); validation task V7 shows how common they are.
- **Only statically linked plugins run.** The pin covers only the executable,
  so a dynamically linked plugin would load unpinned libraries from the host.
  Consumers whose plugins are dynamically linked today must rebuild them.
- **Agent and plugin versions could skew.** Plugins change only with an
  AgentUpgrade, which stages and rolls them back with the binary or image, and
  the daemon checks each plugin with `describe` before it reports ready
  ([lifecycles, section 5](05-lifecycles.md#5-agentupgrade)).
- **The image agent runtimes are tested on Azure Container Linux only.** The
  `containerd` agent runtime needs the host's containerd and, on ACL, ACL's
  SELinux rule and labels, in 3.0.20261026 and later. The `systemd-nspawn`
  agent runtime is meant for other container operating systems, but has run
  only on ACL; each new target needs its own validation run
  ([agent runtime, sections 4.2 and 10](12-agent-runtime.md#42-systemd-nspawn)). The capability probes refuse a host
  without what they need.
- **Each agent runtime multiplies the test matrix.** Every one runs the v1
  suites and the agent runtime conformance suite, so a new agent runtime adds
  CI time as well as code ([agent runtime, section 4.3](12-agent-runtime.md#43-adding-an-agent-runtime)).
- **An agent in a container could write into its container instead of the
  host.** All host file access goes through `hostfs` under `/host`, unit tests
  fail any write outside it, the end-to-end suite checks that the `containerd`
  daemon container's writable layer stays empty, and `systemd-nspawn` runs it
  read-only ([agent runtime, section 5](12-agent-runtime.md#5-reaching-the-host)).
- **Each release has two builds.** An operator could install the wrong one.
  The v2 build on a v1 host migrates it, which is what the v2 build is for. A
  v1 build on a v2 host is rejected before the switch, and the v1 build's
  own commands refuse to act on a v2 host. A build tag rather than a linker flag selects the
  commands, so a mistake fails to compile instead of being ignored, and v1's
  build contains none of v2's code. CI builds and tests both
  ([testing, section 6](11-testing.md#6-ci)).
- **Copied code drifts from v1.** The list in [v1 compatibility, section 3](08-v1-compatibility.md#3-code-copied-from-v1) is the mitigation.
- **Migration takes over running production nodes.** A bug could disrupt a
  node that was working. Before the commit, v2 changes nothing of v1's, so a
  failure returns the host to whichever v1 binary ran it. The commit is short,
  local and always finished. Rollback on the host is possible until the first repave,
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
| Validation tasks | V1 to V15 have prototypes, their results are written back into this design, and the design has changed wherever a result required it ([testing, section 7](11-testing.md#7-validation-tasks)) |
| CI | The v2 jobs have been required on every pull request that touches the agent since the first v2 code merged, and pass nightly on the full host matrix for both runtimes ([testing, section 6](11-testing.md#6-ci)) |
| Migration matrix | The cutover matrix passes against the release builds, from every v1 release back to the oldest supported one, together with every interrupted and failed variant. The release notes name the oldest supported v1 release ([testing, section 4](11-testing.md#4-migration-suite)). |
| Capability probes | Every host in the matrix gives the expected probe results for both node runtimes and every agent runtime, and a host that fails a probe is refused with the probe in the error ([node runtime, section 5.3](03-node-runtime.md#53-host-capabilities)) |
| Image agent runtimes | The v1 suites pass under each image agent runtime on Azure Container Linux with SELinux enforcing, using the published agent image, and nothing is written into the daemon's container ([agent runtime](12-agent-runtime.md)) |
| Documentation | The public site under `docs/` covers the configuration format, writing plugins (the protocol, permissions and distribution manifests), the AgentUpgrade parameters, the two node runtimes and their capability probes, the agent runtimes and theirs, what v2 creates on a host, and migrating and rolling back. The same release drops the v2 build's experimental warning, publishes the v2 build under the plain artifact name, and stops publishing the v1 build. |
