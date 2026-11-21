# Managed ACR authentication and node isolation alignment

Status: draft
Scope: `internal/registryauth`, `internal/operator/components/gantry`, `pkg/agent/isolation`
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
  `<root> = /host`) is the *host* path, which has no such unit. Its
  `chroot /host systemctl restart kubelet.service` reaches the host's systemd, not the
  machine's. Its `/host/proc` scan sees the host's PID namespace, not the machine's, so it
  never finds the in-machine kubelet.
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

### 3.1 The mechanism, restated

On the failing nspawn nodes the installer can issue a restart (the log shows the kubelet
stopping and starting) but can never confirm the managed flags took effect, because its
detection (`RunningKubeletArgs` on `/host/proc`) is blind to the in-machine kubelet. It
therefore restarts on every 30 s tick forever. This is a detection/verification failure, not
an "installer cannot reach the node at all" failure.

An earlier diagnosis (in the issue filed for this work) claimed the failing nodes were
"almost certainly none mode (host kubelet)" and that a pure-nspawn node "never takes
ownership and fails with no restart." That is contradicted by the report: the kubelet *is*
restarted on the failing nodes. The correct statement is the one in section 2.1: the
installer's host-`kubelet.service`-plus-`chroot`-systemctl model is not any Flex isolation
mode, and on nspawn nodes in particular the restart reaches a kubelet that the host `/proc`
scan cannot see.

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

- The agent gains a registry-auth reconciler that, per active node slot:
  - reads the policy (delivered by the operator as a file or a CR the agent watches),
  - writes the credential-provider binary and config into the node stack via
    `Backend.Exec` (so the path is correct per mode: machine rootfs, host unit dir, or
    container rootfs),
  - applies the kubelet flags through the mode's unit mechanism (the drop-in the installer
    writes today, placed where the mode's kubelet unit lives),
  - `Backend.Restart`s the node stack,
  - verifies the flags via `Backend.Status` / `Backend.Exec` reading the kubelet's active
    args, and
  - reports a node condition (or a file the operator reads back) with the outcome.
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

The open question in section 8 (how the policy is delivered to the agent and how state is
read back) resolves in favor of the existing agent pull-and-report model: the policy is a
field the agent already pulls, and the state is a condition on `Machine.Status` the agent
already writes. A dedicated per-node watch or a control-plane polling loop over nodes would
both violate the scaling constraint and are explicitly out of scope.

## 7. Relationship to the working patch

The working patch (PR #1201) is still worth merging as a bounded, safe improvement to the
*current* architecture: it stops the unbounded restart loop (ask 2), adds the supported
disable (ask 3), and makes the installer fail closed and diagnosable rather than flap. It is
not the north star, and its probe's "nspawn" claim should be corrected to match section 4.
The target design in section 6 is the follow-up that actually makes managed ACR
authentication work on every isolation mode and removes the DaemonSet-does-node-work
anti-pattern.

## 8. Constraints and open questions

Constraints:

- **Scale:** clusters can have up to 10k nodes. The design must not add per-node watches or
  excessive control-plane calls for a status check. Section 6.4 shows the target design
  satisfies this: it removes the per-node installer DaemonSet and reports registry-auth
  state through the agent's existing `Machine.Status` path, so the new control-plane
  surface is one condition on `Machine.Status` plus one policy field the agent already
  pulls, not a new per-node watch or a polling loop over nodes.

Open questions:

- Whether the registry-auth goal state should be a first-class agent goal state (like the
  existing node-stack goals) or a lighter sidecar reconciler.
- The exact shape of the Site-level `RegistryAuthFailed` aggregation (count of nodes
  not-ready, dominant reason, threshold) and on which operator tick it is computed so it
  stays bounded at 10k nodes.
- The `none` mode's unit naming (`unbounded-kubelet.service` vs the installer's
  `kubelet.service`) confirms the installer was never aligned to the agent's units; the
  target design makes this a non-issue by going through the backend, but the drop-in path
  must be validated per mode.
- Whether `container` mode (experimental) should be in scope for the north star now or be a
  later increment behind the existing `ContainerIsolationEnabled` gate.
