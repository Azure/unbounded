# Managed ACR authentication and node isolation alignment

Status: implemented
Scope: `internal/registryauth`, `internal/operator/components/gantry`, `pkg/agent/isolation`

Sections 1-5 describe the starting state on main (the DaemonSet installer and its
misalignment with the three isolation modes). Section 6 is the target design that
is now implemented: the policy is delivered to the agent as
`Machine.spec.registryAuth`, the kubelet configuration and bounded restart live in
the agent's isolation backend, and readiness is read back from
`Machine.Status` conditions. Section 7 records the relationship to the working
patch (PR #1201), which the implemented design supersedes.
North star: managed ACR authentication (`attach-acr --auth-mode aks-identity-binding`) must work on every isolation mode the agent supports, and the failure modes observed on air-gapped nspawn nodes must be diagnosed and bounded rather than flapping forever.

## 1. What is implemented today on main

Managed ACR authentication was introduced in `3b19476fc` (#1037) and is the feature behind
`kubectl unbounded gantry attach-acr --auth-mode aks-identity-binding`. It has two halves:

### 1.1 The Azure + Kubernetes control plane (works)

`cmd/kubectl-unbounded/app/gantry_attach_acr.go` and `gantry_acr_azure.go` create, for the
selected AKS cluster:

- a Gantry-owned user-assigned managed identity (UAMI) named `gantry-pull-<6hex>`,
- an AKS identity binding for that UAMI,
- a cross-subscription `AcrPull` role assignment on the ACR scope,
- the `node-auth.yaml` policy in the `gantry-config` ConfigMap, switched to
  `mode: aks-identity-binding`, and the upstream registry in `config.yaml`.

`internal/operator/components/gantry/registry_auth.go` and `registry_auth_audience.go` then
reconcile the Kubernetes-side objects the kubelet token audience needs: the `gantry-acr-auth`
ServiceAccount, Role, RoleBinding, and the ServiceAccount-token audience. This half is
topology-independent and is not the subject of this document.

### 1.2 The node-side installer (the part that does not align)

The operator creates a DaemonSet `gantry-acr-auth` (one pod per Flex node). Each pod runs
`unbounded-operator registry-auth-node`, i.e. `internal/registryauth/node.go`'s `RunNode`.
The pod spec (`registry_auth.go:registryAuthObjects`) is:

- `HostPID: true`, `HostNetwork: true`,
- host `/` bind-mounted at `/host` (the pod's "node root"),
- `Capabilities.Add: [SYS_CHROOT]`, running as root.

The installer's model of the node is hard-coded in `internal/registryauth`:

- unit path: `<root>/etc/systemd/system/kubelet.service` (`node.go:unitPath`),
- detection: scan `<root>/proc/*/comm == kubelet` and read `cmdline`
  (`node.go:RunningKubeletArgs`),
- restart: `chroot <root> /usr/bin/systemctl restart kubelet.service`
  (`node.go:RunNode`'s `HostCommand`),
- the kubelet unit must contain `$KUBELET_NODE_CONFIG_ARGS` and must not already use an
  external credential provider (`install.go:Reconcile`).

So the installer is a k8s pod that reaches into the host, assumes the kubelet is a plain
`kubelet.service` unit visible in the host `/proc`, and restarts it with `chroot systemctl`.
That model is correct for a stock AKS VMSS node, and it is **none of the three isolation
modes** the agent actually runs Flex nodes with.

## 2. The three isolation modes, and where the kubelet lives in each

The agent's isolation backend (`pkg/agent/isolation/isolation.go`) names the modes:

| Mode | Constant | Where the node stack (kubelet) runs | Kubelet unit | How the agent reaches it |
|------|----------|-------------------------------------|--------------|--------------------------|
| nspawn | `isolation.ModeNSpawn` | a systemd-nspawn machine at `/var/lib/machines/<slot>` | `kubelet.service` *inside the machine* | `systemd-run --machine=<slot>` / `machinectl` |
| none | `isolation.ModeNone` | the host, as agent-owned systemd units | `unbounded-kubelet.service` (agent-owned name) | `systemctl` on the host |
| container | `isolation.ModeContainer` (experimental) | a privileged OCI container supervised by an agent unit | `kubelet.service` *inside the container* | `<runtime> exec <container>` |

The agent already abstracts all three behind one seam, the `isolation.Backend`
interface (`isolation.go:254`):

```go
type Backend interface {
    Mode() Mode
    Prepare(ctx context.Context, n NodeSpec) (NodeRoot, error)
    Start(ctx context.Context, r SlotRef) error
    Stop(ctx context.Context, r SlotRef) error
    Restart(ctx context.Context, r SlotRef) error
    Destroy(ctx context.Context, r SlotRef) error
    Status(ctx context.Context, r SlotRef) (Status, error)
    Exec(ctx context.Context, r SlotRef, cmd ExecSpec) (ExecResult, error)
    Logs(ctx context.Context, r SlotRef, opts LogOptions) (io.ReadCloser, error)
}
```

Every method the installer needs to operate a kubelet correctly, regardless of mode, already
exists on this interface: `Restart` (never touches the host, per its doc comment), `Status`,
`Exec` (run a command in the node stack), `Logs`, and `Mode`. The agent wires each mode's
backend in one composition table (`cmd/agent/internal/cmd/modes.go`).

### 2.1 Why the installer's model matches none of the modes

- **nspawn:** the kubelet is at
  `/var/lib/machines/<slot>/etc/systemd/system/kubelet.service`, in the machine's own PID
  namespace. The installer's `<root>/etc/systemd/system/kubelet.service` (with
  `<root> = /host`) is the *host* path, which has no such unit, so the unit it edits (or
  refuses to find) is not the machine's unit. Its `chroot /host systemctl restart
  kubelet.service` reaches the host's systemd, not the machine's.

  Its `/host/proc` scan, however, is **not** categorically blind to the in-machine kubelet:
  a PID namespace is nested, so an ancestor namespace can see the descendants in it, and the
  machine's kubelet does appear in the host's `/proc`. The observed failure is therefore not
  "the scan can never see the in-machine kubelet" but an *observed verification failure*: on
  the reported nodes the installer issued a restart but never confirmed the managed flags
  took effect, and kept restarting on every tick. Two confounds make the diagnosis
  uncertain rather than certain, and both are named here instead of asserted:

  1. the installer pod runs with `HostPID: true` and a host `/` bind mount; the `/proc` it
     actually sees is whichever PID namespace it shares with the runtime that scheduled it.
     On an nspawn node that namespace may be the nspawn runtime's view rather than the
     physical agent host's, so which PIDs the scan enumerates depends on that namespace
     (a hypothesis to confirm per node, not a fact established by the report); and
  2. even when the in-machine kubelet's `/proc` entry is visible, the installer matches on
     `comm == kubelet` and reads a `cmdline` that is the machine's, but it restarts through
     the host unit, so a visible process does not prove the restart reached the right
     systemd.

  The honest statement is the observed one: the installer restarted a kubelet it could not
  verify, and the diagnostics it logs (the `/proc` enumeration and the unit path it tried)
  are the evidence for which confound applies on a given node. Section 3.1 restates this as
  an observed verification failure with the diagnostics to disambiguate it.
- **none:** the kubelet is `unbounded-kubelet.service` on the host. The installer looks for
  `kubelet.service` (the distro name), which is not the agent-owned unit. The agent's own
  code maps the user-facing name to the agent unit (`none/agentUnit`, `none/backend.go:600`),
  confirming the two names are distinct.
- **container:** the kubelet is inside a container reached via `<runtime> exec`. The
  installer has no container-runtime path at all.

A stock VMSS node (host `kubelet.service`, visible in host `/proc`, restartable via
`chroot systemctl`) is the only topology the installer actually matches, and it is not the
topology the DaemonSet is scheduled for (`nodeSelector: kubernetes.io/os=linux` plus the
Flex node label).

## 3. What Marcello's report shows, and where the diagnosis landed

The first end-to-end run of managed mode on on-prem, air-gapped nspawn nodes is
[Azure/AKSFlexNode#249](https://github.com/Azure/AKSFlexNode/issues/249). Environment:
unbounded v0.26.0, AKS 1.37.0 (private), AKS Flex Node v0.3.0 with nspawn workers and
offline bootstrap, 4 on-prem Flex nodes, ACR in another subscription of the same tenant.

What worked:
- the UAMI, identity binding, and cross-subscription `AcrPull` grant were created;
- on 2 of the 4 nodes the installer configured the kubelet (the managed flags were present)
  and the node became Ready.

What did not:
- on the other 2 nodes the `gantry-acr-auth` pod stays NotReady with
  `reason="no running kubelet found; waiting before configuring node authentication"`, while
  the kubelet **is** reconfigured and then restarted every 30 s with no backoff
  (~56 restarts in 28 minutes).

The three asks:
1. reliable kubelet detection for nspawn-based Flex nodes, or logging that shows how
   detection fails;
2. backoff / retry cap instead of an unbounded kubelet restart loop, with a failure
   condition on the Site;
3. a supported, documented way to disable managed mode and clean up the node configuration.

### 3.1 The mechanism, restated as an observed verification failure

On the failing nspawn nodes the installer can issue a restart (the log shows the kubelet
stopping and starting) but can never confirm the managed flags took effect, so it restarts
on every 30 s tick forever. This is an *observed verification failure*, not a proven
topological fact: the report shows the restart loop, but it does not by itself prove which
of the section-2.1 confounds (the installer's namespace view of `/proc`, or restarting the
host unit rather than the machine's) is the one that applies on those nodes. The two are
distinguished by the diagnostics the installer should log and the ones the target design
makes unneeded:

- the list of PIDs the installer actually enumerated from its `/proc` (which namespace it
  was in, and whether the in-machine kubelet's PID was among them),
- the unit path it read and edited (the host `kubelet.service`, which on an nspawn node is
  not the machine's unit),
- the restart command it ran and through which systemd it ran it (the host `systemctl`, not
  the machine's), and
- the `cmdline` of the kubelet it *believed* it had verified, if any.

With those four, a node's failure is attributed to a named cause instead of a guess.

An earlier diagnosis (in the issue filed for this work) claimed the failing nodes were
"almost certainly none mode (host kubelet)" and that a pure-nspawn node "never takes
ownership and fails with no restart." That is contradicted by the report: the kubelet *is*
restarted on the failing nodes. The correct statement is the one in section 2.1: the
installer's host-`kubelet.service`-plus-`chroot`-systemctl model is not any Flex isolation
mode, and on nspawn nodes in particular it restarts through the host's systemd a unit that
is not the machine's. The `/proc`-blindness claim in the earlier text is retracted in favor
of the observed, diagnosable failure above; it is retained only as the *possible* confound
that the diagnostics exist to rule in or out.

## 4. What a follow-up patch changed, and what it still gets wrong

The working patch (branch `fix/gantry-acr-nspawn-detection-detach`, PR #1201) addresses the
asks as follows:

- ask 2 (backoff / retry cap / failure condition): kubelet restarts are now gated by a
  per-policy-revision budget of 3 with exponential backoff (2m, 4m, ... capped at 30m),
  persisted on the node at `/opt/unbounded/registry-auth/.restart-backoff` and keyed by the
  policy revision. The attempt is charged before the disruptive command runs, in both the
  managed and teardown paths. On exhaustion the installer reports
  `ErrRestartBudgetExhausted`. The operator surfaces a distinct `GantryReady` reason
  (`RegistryAuthFailed`). This directly answers ask 2 and the backoff half of ask 1.
- ask 3 (supported disable): a new `kubectl unbounded gantry detach-acr` command reverts
  `node-auth.yaml` to `mode: external`, waits for the operator to converge the external
  policy, removes the Gantry-owned Kubernetes grants, and retains the Azure identity /
  binding / `AcrPull` grant by default (`--remove-azure` deletes them, with ownership and
  cluster/kubeconfig validation). This answers ask 3.
- ask 1 (detection / logging): a one-shot `probeNodeLayout` at startup, plus richer proc and
  unit diagnostics in the log.

The target implementation provides its own `kubectl unbounded gantry detach-acr` that
reverts `node-auth.yaml` to `mode: external`; the operator then clears
`Machine.spec.registryAuth` and the agent removes the managed credential provider, so the
supported-disable path is the same policy field used by `attach-acr` rather than a separate
DaemonSet teardown. `attach-acr` likewise converges by reading the node-reported
`Machine.Status` condition instead of polling the installer DaemonSet.

The part that still gets ask 1 wrong is the probe's *claim*. It tests for the absence of a
host `kubelet.service` unit and labels that "the nspawn case," returning
`ErrUnsupportedKubeletLayout`. But Marcello's failing nodes *have* a reachable kubelet that
the installer can restart; the host unit being present or absent is not the condition that
distinguishes his failure from a working node. The probe therefore does not actually detect
the nspawn failure shape, and its docs/issue text overstate what it proves. The genuine
detection fix for ask 1 is mode-aware (below), and until that lands the probe should be
described honestly as "detects the topology the installer cannot reach" rather than "the
nspawn case."

## 5. The north star and why the current architecture cannot reach it

The north star is: **managed ACR authentication works on every isolation mode** the agent
supports, and the failures are diagnosed and bounded.

The current architecture cannot reach that north star for a structural reason: the
installer is a k8s DaemonSet that performs node-level kubelet mutation through a hard-coded,
topology-blind model. The correct operator of a kubelet, in every isolation mode, is the
agent's `isolation.Backend`, which already runs on the node, already knows the mode, and
already has `Restart` / `Status` / `Exec` / `Logs` / `Mode`.

A k8s DaemonSet is the wrong place for node-level configuration in general, and it is
strictly worse here because the agent already has the right abstraction sitting unused.
The two are also misaligned in control: the DaemonSet's readiness is a pod-level `/readyz`
signal that does not reflect whether the kubelet (the actual target) was configured, while
the agent's `Backend.Status` does reflect the node stack it operates.

## 6. Target design

Move the registry-auth kubelet configuration from the DaemonSet installer into the agent's
isolation backend, and shrink the operator's role to policy and state.

### 6.1 Ownership split

- **Agent (node-side):** owns "configure the kubelet to use the managed credential
  provider, verify it, and report state," done through `isolation.Backend` so all three
  modes work by construction. The agent already runs on every Flex node and already knows
  the mode; it adds a registry-auth goal state driven by the policy.
- **Operator (control plane):** owns "reconcile the policy (the `node-auth.yaml` content) to
  each node, and read back the node-reported state." It no longer creates a DaemonSet that
  restarts kubelets.
- **CLI:** `attach-acr` / `detach-acr` unchanged in user-facing shape; they still set the
  policy and the Azure-side objects.

### 6.2 Concretely

- The agent gains a registry-auth reconciler (a dedicated controller over the local
  `Machine`) that, per active node slot:
  - reads the policy from `Machine.spec.registryAuth` (an operator-set field; the
    operator writes it via an optimistic merge patch so it does not clobber the other
    spec fields machina owns),
  - downloads the stock ACR credential-provider binary into the node stack
    (`/opt/unbounded/registry-auth/bin`, pinned per-arch SHA256, like the other node
    binaries; it is a large upstream artifact, so it is not embedded in the agent). The
    download honors the node's configured source, including an offline artifact mirror
    (`DownloadOverrides.ACRProvider`), because the provider is no longer supplied by an
    installer image and air-gapped nspawn nodes are the motivating environment. The
    offline bundle is resolved **lazily**, only when the provider binary is actually
    missing and a download is needed: a node in steady state (provider already in place)
    or converging to an external policy does not require the bootstrap source to remain
    available, so verification and teardown do not couple to it;
  - writes the credential-provider and azure config generated from the policy into the
    node stack through the mode-aware node root (so the path is correct per mode: machine
    rootfs for nspawn, the host for none, container rootfs for container),
  - toggles the managed image credential provider flags in the kubelet's **existing**
    node-config drop-in (`$KUBELET_NODE_CONFIG_ARGS`, the same place the supported
    `Kubelet.ImageCredentialProvider` configuration lands) rather than a separate
    ExecStart-restating drop-in: it adds the two provider flags if absent, removes them
    for an external policy, preserves the other effective node-config args
    (hostname-override, node-ip, labels) and any other drop-ins, and refuses to add a flag
    whose path differs from an existing provider so an external provider configured
    elsewhere is not silently overridden,
  - daemon-reloads the mode's systemd manager (mode-correct) and verifies the reload
    succeeded before the disruptive restart, then restarts only the kubelet unit
    (mode-correct unit name: `unbounded-kubelet.service` for `none`, `kubelet.service` for
    `nspawn`/`container`), within a bounded, persisted budget charged before the
    disruptive restart,
  - tracks the pending restart work durably (independent of the on-disk bytes) so a pass
    that wrote the files then crashed, or a restart that failed, still retries on the next
    pass, and clears it only when the kubelet is verified running the expected flags,
  - verifies the flags by resolving the unit's MainPID in the node's namespace, reading
    that process's `/proc/<pid>/cmdline`, and checking the exact argv tokens (a missing
    MainPID or unreadable cmdline is an unknown state reported as a failure, not as
    proven cleanup; for an external policy a running kubelet with no managed tokens is the
    proven-external state), and
  - reports the `Machine.Status` `RegistryAuth` condition (`Configured`/`External` when
    verified True; a terminal failure otherwise; the condition is only True when the
    flags are actually verified present, with `ObservedGeneration` set so the operator can
    tell the node acknowledged the current policy generation).
- The `Backend.Restart` contract ("restart the node stack only, never the host") is exactly
  the safety property the installer's `chroot systemctl restart kubelet` was approximating
  and getting wrong on nspawn.
- The restart budget/backoff from the working patch is retained, but it bounds the *agent's*
  restarts, which are now mode-correct, and its persisted state lives where the agent keeps
  node state rather than in a DaemonSet's bind mount.
- The `gantry-acr-auth` DaemonSet is retired. The operator's `planRegistryAuth` keeps
  reconciling the ServiceAccount / Role / RoleBinding / audience (topology-independent) and
  changes from "ensure a DaemonSet exists" to "ensure the policy is delivered and read back."

### 6.3 Why this satisfies the north star

- **All isolation modes:** each mode's backend already knows where its kubelet is and how to
  reach it, so "configure the kubelet" is the same code path in all three modes, with no
  special-casing of nspawn.
- **Marcello's asks:**
  - ask 1 (detection / logging): the agent reads the kubelet's real state through
    `Backend`/`Backend.Status`, so detection is correct per mode, and `Backend.Logs` gives
    the exact failure diagnostics instead of a blind host-`/proc` scan.
  - ask 2 (backoff / failure condition): retained and now bounded against a mode-correct
    restart; the operator's `RegistryAuthFailed` condition reads back node-reported state
    instead of a pod-level signal.
  - ask 3 (supported disable): `detach-acr` sets the policy to external; the agent's
    reconciler removes the flags and reports convergence, with the same ownership-aware
    cleanup the patch already implements.

### 6.4 Control-plane scaling at 10k nodes

The design must not add per-node watches or excessive control-plane calls. It does not,
because it reuses the status path the agent already runs and removes the per-node DaemonSet
the current design adds.

What the current design does at scale:

- One DaemonSet whose spec the operator reconciles (topology-independent, one object).
- The kubelet-scheduling machinery that places and monitors **one installer pod per node**
  (10k pods at 10k nodes), each with a readiness probe firing every 5 s. The operator's
  `RegistryAuthFailed` signal is an aggregate of those pod statuses.
- The Azure/K8s audience RBAC, which is topology-independent.

What the target design does at scale:

- The installer DaemonSet is retired, so the 10k per-node pods and their probes are gone.
- The agent, which already runs on every node and already writes `Machine.Status` (see
  `cmd/agent/internal/daemon/controller_node.go` and the `MachineStatus.Conditions` /
  `Isolation` fields in `api/machina/v1alpha3/machine_types.go`), gains a registry-auth
  condition on that same status. The agent already polls the control plane for its desired
  configuration (the repave / `MachineConfiguration` path in `controller_node.go`); it is
  the same pull-based agent that reports state back. No new per-node watch is introduced;
  the node reports one condition alongside the conditions it already reports.
- The operator/machina reads back `Machine.Status` for the registry-auth condition. This is
  the same mechanism machina already uses to watch `Machine` objects
  (`cmd/machina/machina/controller/machine_controller.go:808`), so it is an incremental
  field on an existing watch, not a new one. The Site-level `RegistryAuthFailed` condition
  becomes a bounded aggregation over the nodes' reported conditions (for example, count of
  nodes reporting not-ready and the reason), computed on the operator's existing reconcile
  tick rather than on every node event.

Net: the target design *reduces* control-plane load at 10k nodes by removing the per-node
installer pods and their probes, and reports registry-auth state through the agent's
existing `Machine.Status` path. The only new control-plane surface is one condition on
`Machine.Status` and one policy field the agent already polls, both of which are
amortized over the agent's existing control-plane interaction rather than adding new
per-node watches.

How the policy is delivered to the agent and how state is read back resolves as follows.
The policy is `Machine.spec.registryAuth`, a field the operator writes and the agent's
registry-auth reconciler watches on its own local `Machine` (a per-agent watch of a single
object, plus a bounded periodic requeue that keeps the condition fresh and enforces the
restart backoff). This is a watch the agent already has over its own `Machine`, not a new
per-node fan-out on the control plane. The state is the `Machine.Status` `RegistryAuth`
condition the agent writes alongside the conditions it already reports. The operator folds
the fleet of conditions into `GantryReady` on its existing reconcile tick (a bounded list,
no per-node events). A control-plane polling loop over nodes would violate the scaling
constraint and is explicitly out of scope.

## 7. Relationship to the working patch

The working patch (PR #1201) was a bounded, safe improvement to the *previous*
DaemonSet-installer architecture: it stopped the unbounded restart loop (ask 2), added the
supported disable (ask 3), and made the installer fail closed and diagnosable rather than
flap. That patch is now superseded: the target design in section 6 (delivered to the agent
as `Machine.spec.registryAuth`, applied through the agent's isolation backend, and read back
from `Machine.Status`) is the architecture that is implemented, and it removes the
DaemonSet-does-node-work anti-pattern entirely. PR #1201's probe "nspawn" claim is
corrected in section 4, and its fixes (bounded restart, fail-closed verification,
detach) are re-expressed in the agent-backed reconciler rather than carried in the
installer.

## 8. Constraints and open questions

Constraints:

- **Scale:** clusters can have up to 10k nodes. The design must not add per-node watches or
  excessive control-plane calls for a status check. Section 6.4 shows the target design
  satisfies this: it removes the per-node installer DaemonSet and reports registry-auth
  state through the agent's existing `Machine.Status` path, so the new control-plane
  surface is one condition on `Machine.Status` plus one policy field the agent already
  pulls, not a new per-node watch or a polling loop over nodes.

Open questions (resolved in the implementation):

- **Reconciler shape:** implemented as a dedicated, separate controller over the local
  `Machine` (its own watch of `spec.registryAuth` plus a bounded 5m periodic requeue), not
  folded into the serialized repave/machine-op setup path. It re-resolves the **mode-aware**
  node-stack root for the active slot (host for `none`, container rootfs for `container`,
  the machine directory for `nspawn`) so the provider files and the node-config drop-in land
  on the correct filesystem in every mode. The offline-artifact download override is
  resolved **lazily, only when the provider binary is actually missing** (so a steady-state
  or external-policy pass does not require the bootstrap source to remain available), and
  when resolved it wires `DownloadOverrides.ACRProvider` so the provider binary downloads
  from the offline mirror instead of the public upstream release.
- **Operator triggers:** the operator enqueues a registry-auth reconcile on Machine
  create/delete and on a change to the policy, the association (`spec.kubernetes.nodeRef.name`,
  which the delivery plan indexes via `Machine.NodeName()`), or the reported condition; and
  on Node membership changes only (a node becoming or stopping being an eligible Linux AKS
  Flex node), not on status-only Node updates. At 10k nodes a NodeReady heartbeat or
  unrelated label churn therefore does not fan out into a whole-fleet pass.
- **Aggregation shape:** `GantryReady` reports `RegistryAuthFailed` when any eligible node
  reports a terminal failure (the message carries the count and the first failing node), and
  `RegistryAuthInstalling` while nodes have not yet reported. It is computed on the operator's
  reconcile tick as a bounded list over the eligible nodes' `Machine.Status` conditions, not on
  per-node events.
- **Unit naming:** the restart and the node-config drop-in it edits use the mode-correct
  unit name (`unbounded-kubelet.service` for `none`, `kubelet.service` for
  `nspawn`/`container`), and the drop-in is edited in that unit's existing
  `20-node-config.conf` (where `$KUBELET_NODE_CONFIG_ARGS` lives), so the misalignment
  between the installer's hard-coded `kubelet.service` and the agent's units is gone by
  construction and the managed flags share the same effective-args path the supported
  `Kubelet.ImageCredentialProvider` configuration already uses.
- **Container mode:** `nspawn` and `container` share the in-machine kubelet and unit
  (`kubelet.service`), so they are both in scope behind the same isolation backend; `none` is
  the host-level path. The reconciler resolves the active slot's root, so it does not need a
  per-mode `ContainerIsolationEnabled` gate to reach the right filesystem.
