# Unified Machine and Node Topology

## Decision Summary

Give Unbounded Machines and their Kubernetes Nodes the same topology vocabulary
for label selectors. Operators assign topology on Machine metadata.
`machina-controller` continuously projects it onto the bound Node, without a
restart. Machine metadata is the sole authority.

| Concept | Meaning | Canonical label |
|---|---|---|
| Data Center | One physical facility | `topology.unbounded-cloud.io/datacenter` |
| Rack | A physical equipment rack within one Data Center | `topology.unbounded-cloud.io/rack` |
| Fabric | An operator-declared high-speed compute interconnect domain within one Data Center | `topology.unbounded-cloud.io/fabric` |

Rack and Fabric are independent memberships, not levels of one hierarchy.
A Rack may contain Machines from different Fabrics; a Fabric may span Racks
within its Data Center. A Machine has at most one value for each concept.
Networking Site membership remains separate. These labels neither replace
`unbounded-cloud.io/site` nor map automatically to Kubernetes region/zone labels.

## Scope and Non-goals

Deliver the label contract, admission validation, live projection, a
`TopologySynced` Machine condition, documentation, and selector examples.
This document specifies desired behavior, not existing implementation.

Do not add update scheduling, batching, concurrency limits, disruption
guarantees, multi-Fabric membership, topology CRDs/registries, inventory
integration, physical discovery, or manual overrides. Additional admission
restrictions on the projector's Node-write permissions are separate hardening.
Selecting a topology domain does not itself implement an update safety policy.

## Label Values

Operators assign stable Data Center IDs unique across the clusters using this
scheme. Rack and Fabric IDs are globally unique by qualification with that
Data Center. Machines sharing an ID express membership, not a uniqueness error.
Admission validates declared consistency, not physical identity or existence.

| Label | Value |
|---|---|
| Data Center | `<datacenter>`, for example `dc-a` |
| Rack | `<datacenter>.<local-id>`, for example `dc-a.rack-01` |
| Fabric | `<datacenter>.<local-id>`, for example `dc-a.compute-01` |

Each segment must match `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`: lowercase letters,
digits, and internal hyphens; a single letter or digit is valid. Qualified
values have exactly one dot, with a Data Center prefix equal to the Machine's
Data Center label. Every complete value is nonempty and at most 63 characters.
A 62- or 63-character Data Center ID is valid alone but cannot accommodate a
qualified Rack or Fabric. Do not shorten or normalize values automatically.

All labels are optional. Rack or Fabric requires Data Center, but neither
requires the other. Absence means unknown or not applicable; a present key with
`""` is invalid. Do not synthesize values such as `unknown` or infer placement
from Site, region, or zone. Missing topology does not prevent a Machine running.

| Assignment | Result |
|---|---|
| No topology labels | Valid |
| Data Center alone, or with only Rack or only Fabric | Valid |
| `datacenter=d`, `rack=d.r`, `fabric=d.f` | Valid single-character segments |
| Rack or Fabric without Data Center | Invalid |
| `datacenter=dc-a`, `rack=dc-b.rack-01` | Invalid prefix |
| `datacenter=DC-A`, `rack=dc-a.`, or `rack=dc-a.r.1` | Invalid grammar |
| A present topology key with an empty value | Invalid, not omission |
| Complete value of 63 characters / 64 characters | Valid if otherwise well formed / invalid |

Changing Data Center must update or remove dependent Rack and Fabric labels in
the same API request so the resulting assignment remains valid.

## Admission and Compatibility

Use an `admissionregistration.k8s.io/v1` ValidatingAdmissionPolicy and binding
with `failurePolicy: Fail` and `validationActions: [Deny]`. CRD CEL cannot
inspect `metadata.labels`; CLI checks alone are insufficient.
This feature requires a compatible API server serving the stable VAP API,
[available since Kubernetes 1.30](https://kubernetes.io/docs/reference/access-authn-authz/validating-admission-policy/),
with VAP admission enabled.

Enforce Machine metadata validity and reserve the three canonical keys in:

- `Machine.spec.kubernetes.nodeLabels`
- `MachineConfiguration.spec.template.kubernetes.nodeLabels`
- `MachineConfigurationVersion.spec.template.kubernetes.nodeLabels`

New node-label configuration must not contain those keys, even when values
match Machine metadata. Do not restrict unrelated node-label keys.

### Exact legacy allowance

Compare only topology inputs, not the entire label map or object:

1. Let `T` be the presence and values of the three Machine metadata keys.
   Absent and present-with-empty are different. On CREATE, require valid `T`.
   On UPDATE, unchanged `T` may remain invalid; changed `T` must be wholly valid.
2. Independently, for each node-label configuration map, let `R` be its
   reserved-key entries. On CREATE, require empty `R`. On UPDATE, unchanged `R`
   is allowed; changed `R` must be empty.
3. Unrelated labels, annotations, finalizers, spec fields, non-reserved
   node-label entries, and status remain editable when these inputs do not
   change. DELETE is unaffected. Status updates must not be blocked by
   unchanged legacy inputs or become a route for new invalid input.

This is grandfathering, not a general validation bypass. Removing all invalid
inputs is a valid repair. An edit that repairs only part of an invalid `T`
while leaving it invalid must be rejected.

The rule also applies to newly generated configuration-version snapshots:
creating a new snapshot from an unrepaired legacy parent is rejected. Surface
that failure explicitly through the configuration controller's error reporting;
the operator must repair the parent before producing a clean new snapshot.
Existing invalid snapshots remain readable and are handled at consumption
time as described below.

## Projection Contract

### Writer and target

`machina-controller` is the single projection writer and owns `TopologySynced`.
Run projection for every non-deleting Machine, independently of SSH settings
or reachability, host provider, provisioning phase, configuration binding, and
overall readiness. Do not put it behind existing provisioning guards.
Grant the controller `patch` on Nodes; add `update` only if the implementation
needs it. Preserve its existing unrelated permissions.

Resolve the target with `Machine.NodeName()`: nonempty
`spec.kubernetes.nodeRef.name`, otherwise Machine name.
If multiple non-deleting Machines resolve to the same target, none may project;
report `BindingConflict` on the claimants. Do not choose an arbitrary winner.
Nodes with no bound Machine are untouched.

Rebinding stops writes to the former target and reconciles the new target.
Do not add a cleanup finalizer or remove former-target labels on rebinding or
Machine deletion. Deleting Machines do not project. Consequently, former or
orphaned Nodes can retain stale labels; this feature does not promise cleanup
of those Nodes.

### Exact managed state and writes

For valid Machine topology, each of the three Node keys must equal the
Machine's value or be absent when absent on Machine. Remove absent keys even
if kubelet, an operator, or another field manager originally wrote them.
In particular, an unlabeled Machine causes those keys to be removed from its
bound Node, including on the first reconcile after rollout.

Patch only the managed keys, together in one operation, preserving unrelated
labels and fields. Use explicit deletions, such as JSON merge-patch `null`
entries; do not rely on server-side apply omission to delete another manager's
keys. Protect writes against Node replacement and concurrent updates using
the observed UID/resourceVersion. On conflict, disappearance, or replacement,
reread the target and recompute rather than replaying a stale patch.
Issue no Node write if the managed state already matches.

Invalid Machine metadata causes no Node mutation, including no removal of
previously projected keys. Leave their previous values until repair and report
`InvalidTopology`. This preserves stale values deliberately rather than
silently changing selector membership during a validation failure.

### Events and retries

Reconcile initial state and Machine creation, topology edits, binding changes,
and deletion. Watch Node creation, deletion/replacement, and changes to the
three managed keys. A direct Node topology edit is eventually overwritten.

Map Node events to explicit `nodeRef` claimants and valid same-name fallbacks.
Machine binding or claim changes must enqueue peers for both old and new
targets so conflicts are detected and cleared. Do not enqueue topology work
for unrelated Node status heartbeats or loop on the projector's own unchanged
condition writes.

Retry transient failures with controller backoff and actionable errors.
Status writes must update only this condition, preserve phase and other
conditions, and handle concurrent writers without lost updates. No atomic
transaction across Machine and Node is promised.

## Keep Topology Out of Kubelet Configuration

Live projection is the only supported source of these Node keys. Do not copy
Machine topology into `AgentConfig.Kubelet.Labels` or kubelet `--node-labels`.
Per-host agents do not project topology.

Reject reserved keys in new CLI/API/configuration inputs, including machine
registration, manual bootstrap, and configuration creation node-label flags.
Use consistent validation and actionable errors identifying the key and
directing the operator to Machine metadata.

Grandfathering permits storage, not topology authority. When consuming legacy
stored configuration, configuration-version snapshots, or already-applied
agent configuration, exclude reserved keys from effective kubelet labels and
self-registration inputs, and emit actionable warnings. Prevent provider
label injection from reintroducing them. Do not silently adopt or migrate
them into Machine metadata.

Compare topology-neutral effective labels on both sides of configuration drift
checks. A topology-only edit or filtering of legacy reserved entries must not
itself restart kubelet or recreate the Node. Preserve unrelated configuration
behavior.

A newly registered or recreated Node may briefly lack topology labels until
the first projection. Do not add a readiness gate to close this window.

Topology keys remain ordinary Machine labels and may participate in
MachineConfiguration selectors. Existing binding semantics still apply:
selection can assign a configuration to an unbound Machine; it does not
automatically rebind a Machine with an existing configuration reference
(`cmd/machina/machina/controller/machineconfiguration_binding_controller.go:123-132,163-180`).
Projection itself does not mutate configuration references, but a label edit
can affect normal configuration selection for an unbound Machine.

## Projection Status and Consistency

Define `TopologySynced` and reason constants in the Machine API package,
following existing condition conventions.

| Status | Reason | Meaning |
|---|---|---|
| `True` | `Synced` | Valid Machine topology exactly matches the current target's values and absences |
| `False` | `InvalidTopology` | Machine metadata is invalid; leave Node labels unchanged |
| `False` | `NodeNotFound` | The resolved target Node does not exist |
| `False` | `BindingConflict` | Multiple non-deleting Machines resolve to the same target; do not project |
| `False` | `SyncFailed` | A read or patch failed; report the cause and retry |

An existing Node with no managed labels matching an unlabeled Machine is
`Synced`. An absent Node is not success, even for an unlabeled Machine.
If reconciliation cannot read the Machine or update status, surface the API
error rather than claiming success. Do not change overall Machine readiness.

Set `observedGeneration` according to repository conventions, but do not use
it as a topology revision: metadata label edits do not increment generation.
`True` is the last successful asynchronous observation and can remain stale
while an edit or error is being processed. It is not an acknowledgment of a
specific label edit. A successful Node patch also does not make subsequent
Machine and Node reads atomic.

Machine and Node selectors are eventually consistent. API acceptance or
`TopologySynced=True` alone is not proof of complete, current membership for
an update transaction and provides no disruption guarantee.

## Rollout

Ship admission policies/bindings in rendered manifests and operator-managed
installation. Install guards before enabling the upgraded projector; use
explicit operator dependencies rather than relying on manifest name order.
Do not silently enable projection without its required admission guards.

Existing unlabeled objects need no migration, but any pre-existing canonical
Node keys absent on their bound Machine will be removed. Invalid Machine
metadata leaves existing Node labels untouched and reports `InvalidTopology`.
Legacy configuration remains stored but loses authority over these keys at
consumption time. New invalid snapshots fail until their parent is repaired.

Do not automatically rename existing labels, infer placement, import Node
labels into Machine, or add a manual override.

## Examples

Operator-managed labels on a Machine; provisioning fields are omitted here:

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: Machine
metadata:
  name: gpu-worker-01
  labels:
    topology.unbounded-cloud.io/datacenter: dc-a
    topology.unbounded-cloud.io/rack: dc-a.rack-01
    topology.unbounded-cloud.io/fabric: dc-a.compute-01
```

Observed Node after projection, assuming the default same-name binding.
This is output, not a second operator-managed input:

```yaml
apiVersion: v1
kind: Node
metadata:
  name: gpu-worker-01
  labels:
    example.com/team: compute
    topology.unbounded-cloud.io/datacenter: dc-a
    topology.unbounded-cloud.io/rack: dc-a.rack-01
    topology.unbounded-cloud.io/fabric: dc-a.compute-01
```

The unrelated `example.com/team` label is preserved, not projected from Machine.
Select one Rack, or the intersection of one Rack and one Fabric:

```sh
kubectl get machines.unbounded-cloud.io -l 'topology.unbounded-cloud.io/rack=dc-a.rack-01'
kubectl get nodes -l 'topology.unbounded-cloud.io/rack=dc-a.rack-01,topology.unbounded-cloud.io/fabric=dc-a.compute-01'
```

Positive equality selectors exclude objects missing the key. Negative
selectors such as `!topology.unbounded-cloud.io/fabric` or `NotIn` can include
unlabeled objects; they do not prove complete topology. Machine selectors read
the authoritative assignment, while Node selectors can lag.

## Implementation Touchpoints

These are current-code integration constraints, not evidence that the desired
behavior already exists. Verify line references against the implementation
branch before editing.

| Area | Current behavior or integration constraint | Source |
|---|---|---|
| Binding | The shared Node-name helper supports explicit references and same-name fallback | `api/machina/v1alpha3/machine_types.go:280-287` |
| Projection placement | The existing Machine reconcile path has SSH/provisioning guards | `cmd/machina/machina/controller/machine_controller.go:231-255` |
| Node events | Existing event mapping is name-based; the explicit-reference index is available separately | `cmd/machina/machina/controller/machine_controller.go:774-788`; `cmd/machina/machina/controller/manager.go:144-150` |
| Bootstrap | Label construction merges configured node labels, common labels, and provider labels | `internal/provision/agent_config.go:152-165` |
| Recreation | Applying a configuration-version template can replace kubelet's label map | `cmd/agent/internal/daemon/controller_node.go:143-154` |
| Self-registration | Applied kubelet labels are copied into Machine node-label configuration | `cmd/agent/internal/daemon/daemon.go:391-410` |
| Drift | Kubelet label differences participate in configuration drift | `cmd/agent/internal/daemon/nodeoperator.go:168-170` |
| Permissions | Existing machina Node RBAC lacks patch permission | `deploy/machina/02-rbac.yaml.tmpl:78-80` |
| Admission delivery | Metalman bootstrap restrictions provide an existing VAP/Deny-binding pattern | `deploy/machina/06-metalman-rbac.yaml.tmpl:227-277` |

Update controller registration, RBAC source markers/templates, and the machina
operator component along with the projector. Cover CLI input validation,
configuration snapshot admission, legacy consumption, and drift normalization;
changing only bootstrap label construction is insufficient.

## Acceptance Criteria

Use focused tests at the responsible layer. Admission behavior must be exercised
against a real test API server with installed policies/bindings; fake clients
or assertions on schema strings alone do not establish enforcement.

| Area | Required cases and outcome |
|---|---|
| Grammar | Valid partial assignments, single-character segments, empty vs absent, uppercase, malformed dots/hyphens, prefix mismatch, and 63/64-character boundaries behave as specified |
| Parent edits | Data Center reassignment succeeds only with compatible child edits/removals in the same request |
| Legacy metadata | Invalid CREATE and invalid topology edits fail; unchanged invalid topology permits unrelated metadata, spec, finalizer, and status updates; partial invalid repairs fail |
| Reserved configuration | Machine, configuration, and version CREATE reject reserved entries; unchanged legacy entries permit unrelated updates; changing reserved entries requires removing all of them |
| Snapshots | An unrepaired legacy parent cannot produce a new invalid snapshot; the failure is surfaced; a repaired parent produces a clean version; old snapshots remain readable |
| Projection coverage | Non-SSH/provider-managed Machines and non-ready phases reconcile; explicit different-name bindings work; missing Nodes report `NodeNotFound`; recreated Nodes are resynchronized |
| Managed state | Direct edits are reverted; all three keys can be removed regardless of prior writer; empty assignments succeed on existing Nodes; unrelated labels/fields survive |
| Concurrency | Stale UID/resourceVersion writes do not affect replacement Nodes or lose unrelated updates; conflicts retry from fresh state; matching state causes no Node write |
| Bindings and lifecycle | Duplicate targets do not fight; peer requeues resolve conflicts; rebinding/deletion stops old-target writes without cleanup or a new finalizer; unbound Nodes are untouched |
| Invalid state and status | Invalid topology leaves Node labels unchanged; condition reasons and empty/missing-Node cases match the table; other conditions, phase, and readiness are preserved |
| Config isolation | CLI/new inputs reject reserved keys; legacy snapshot/provider/applied-config paths cannot reintroduce them; warnings are visible; self-registration contains no reserved keys |
| No recreation | Topology-only edits and legacy-key normalization do not introduce configuration drift or restart/recreation; unrelated drift behavior remains intact |
| Watch efficiency | Managed-label, binding, deletion and Node-replacement events reconcile; unrelated Node heartbeats and unchanged condition writes do not cause topology write loops |
| Delivery | Rendered manifests and operator installation include admission guards, required Node permissions, and guard-before-projector ordering |
