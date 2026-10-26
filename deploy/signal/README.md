# Project Signal controller

Project Signal performs one certification run for a newly joined Node or a
small, explicitly selected group of Nodes. The controller snapshots the
approved profile and scripts, reserves and cordons every participant, runs 15
ordered checks through Argo Workflows, persists one immutable evidence
document, and cleans up all temporary workloads.

Project Signal does not integrate with Kueue. Queue admission and repeatable
fleet scheduling are intentionally deferred.

## Components

- `CertificationProfile` is cluster-scoped operator policy.
- `CertificationRun` is a namespaced, immutable execution snapshot.
- `project-signal-controller` admits Nodes, owns reservations and cordons,
  creates the Workflow, persists status and evidence, and performs teardown.
- `project-signal-participant-adapter` creates Pod or MPIJob backends and
  captures typed evidence. Ordinary Pods and completed MPI resources remain
  until controller teardown; the MPI Operator removes still-running workers
  after launcher completion so later sequential checks can use the GPUs.
  On setup, observation, or wall-clock deadline failures, the adapter first
  persists the available evidence and then quiesces uncertain backends.
- `project-signal-participant-adapter` is a fixed WorkflowTemplate used by
  every check. Profiles cannot replace it or pass arbitrary Workflow wiring.

Argo Workflows is required. The Kubeflow MPI Operator is required only when a
profile contains an `MPIJob` check.

## First deployment

Render the manifests with immutable controller and adapter images:

```bash
make signal-manifests \
  SIGNAL_CONTROLLER_IMAGE=<controller-image> \
  SIGNAL_ADAPTER_IMAGE=<adapter-image>@sha256:<digest>
```

Install the rendered CRDs and namespaced resources:

```bash
kubectl apply -f deploy/signal/rendered/crd/
kubectl apply -f deploy/signal/rendered/
```

The install creates:

- the `project-signal` namespace;
- controller and adapter service accounts and RBAC;
- the fixed participant adapter WorkflowTemplate;
- the controller Deployment.

The controller requires cluster permission to get, list, watch, and patch
Nodes because it owns participant reservation markers and cordons.

## CertificationProfile contract

A Profile contains only operator choices that legitimately vary between
certification suites:

| Field | Meaning |
|---|---|
| `spec.participants.count` | Total Nodes selected for a Run |
| `spec.participants.selector` | Labels every selected Node must match |
| `spec.participants.minimumAllocatable` | Admission-time allocatable resource floor |
| `spec.checks` | Exactly 15 ordered checks; position defines R01 through R15 |
| `spec.deadline` | Whole-suite deadline |

The requesting Node is participant zero. The remaining participants are
selected deterministically from currently ready, schedulable, unreserved
Nodes matching the participant selector and allocatable resource floor. Every
check runs on this complete immutable participant set.

Each check contains:

| Field | Meaning |
|---|---|
| `script` | Source ConfigMap name and key; admission snapshots the current bytes and computes their SHA-256 digest |
| `runnerImage` | Digest-pinned check image |
| `backend` | `Pod` or `MPIJob` |
| `backendActiveDeadline` | Backend execution deadline |
| `gpuCount` | GPU request; must be positive for MPIJob |
| `scriptEnvironment` | At most 32 bounded, non-reserved environment values |
| `workloadPolicy` | Narrow container resources and host access policy |

Workload policy is intentionally constrained:

- the only added Linux capability is `IPC_LOCK`;
- host sources must be under `/proc`, `/sys`, or
  `/var/lib/project-signal`;
- host mounts are read-only and their volume names and `Directory` type are
  controller-owned;
- scheduling tolerations are controller-owned;
- arbitrary PodSpec, Workflow, retry, TTL, queue, and cleanup settings are not
  accepted.

Profile edits affect only future Runs.

## Starting one certification

Add the request label to a schedulable Node:

```bash
kubectl label node <node-name> \
  signal.unbounded-cloud.io/certification-request=<profile-name>
```

Admission then:

1. validates the Profile and all 15 checks;
2. selects the requesting Node plus any additional participants;
3. atomically records the Run reservation name, participant Node UID, and
   `spec.unschedulable=true` on each participant;
4. copies all verified scripts into one immutable Run-owned ConfigMap;
5. creates one immutable `CertificationRun`;
6. projects the pending certification state and marks admission ready.

Ordinary workloads cannot schedule onto cordoned participants. Signal backend
Pods receive only the fixed GPU toleration and the fixed
`node.kubernetes.io/unschedulable` toleration needed to execute on the Nodes
reserved by this Run. Required Node affinity pins every backend to the admitted
Node name and UID.

## CertificationRun contract

The Run spec snapshots:

- Profile name, UID, and generation;
- ordered participants with Node name, Node UID, and admission-time allocatable
  resources;
- the immutable script bundle name and UID;
- 15 ordered check snapshots;
- the suite deadline;
- monotonic `cancel` and `release` actions.

Participant and check identity is positional. Participant zero is the
requesting Node. Check positions zero through fourteen are R01 through R15.
The controller derives these identities after every API read rather than
serializing duplicate index and ID fields.

Run status contains:

- `observedGeneration`;
- phase and bounded reason;
- Workflow name, UID, and observed phase;
- evidence URI and SHA-256 digest;
- positional check phases, bounded reasons and measurements, backend
  identities, and backend-evidence digest/completeness;
- start and finish timestamps;
- `Cordoned`, `WorkloadCleanupComplete`, and `Released` conditions.

Detailed Pod and MPIJob evidence is retained in the durable evidence document,
not duplicated into CR status.

## Terminal behavior

The controller never releases a participant before evidence is durable and
all Workflow and backend resources are gone.

- `Succeeded`: cleanup completes and participants are released automatically.
- `Failed`, `Inconclusive`, or `Canceled`: cleanup completes, but participants
  remain reserved and cordoned.
- Set `spec.release: true` to release a non-successful Run after inspection.
- Deleting a Run requests teardown. Its finalizer stops and removes owned
  workloads, verifies participant identity and reservation ownership, removes
  owned cordons and reservations, deletes the script bundle, and then permits
  deletion.

Release uncordons a Node only when all of these still match:

- the snapshotted Node UID;
- `signal.unbounded-cloud.io/reserved-by=<run-name>`;
- `signal.unbounded-cloud.io/reserved-node-uid=<node-uid>`.

Identity replacement or ownership loss is skipped during release. Signal
never uncordons or removes markers from a Node it cannot prove it still owns,
but it continues releasing the remaining exact identity matches.

Cancel a live Run:

```bash
kubectl patch certificationrun -n project-signal <run-name> \
  --type=merge -p '{"spec":{"cancel":true}}'
```

Release a retained terminal Run:

```bash
kubectl patch certificationrun -n project-signal <run-name> \
  --type=merge -p '{"spec":{"release":true}}'
```

## Evidence

The evidence document includes:

- Run, Profile, participant, script-bundle, and Workflow identities;
- the immutable check snapshots and script contents;
- aggregate execution state, verdict, reason, phase, and timestamps;
- each positional check's verdict and timing;
- typed Pod or MPIJob evidence, including Pods, containers, states, bounded
  logs, and capture errors;
- backend identities and participant positions;
- a per-check cleanup disposition showing that deletion is deferred to the
  controller.

The adapter persists typed backend evidence and leaves completed resources in
place. MPI worker evidence is captured when still present; a worker may already
be absent because `cleanPodPolicy: Running` releases its GPU after launcher
completion. If an uncertain backend cannot be quiesced, the adapter task fails
and later checks are skipped rather than overlapping execution. The controller
deletes the Workflow and all remaining owned backends only after the final
evidence document is durable.
`WorkloadCleanupComplete=True` is the authoritative record that this
controller-owned cleanup finished.

## Operations

Inspect Runs and lifecycle conditions:

```bash
kubectl get certificationruns -n project-signal
kubectl describe certificationrun -n project-signal <run-name>
```

Inspect the immutable script bundle before terminal cleanup:

```bash
kubectl get configmap -n project-signal <run-name>-scripts -o yaml
```

Controller restarts are safe:

- Workflow creation intent is recorded before creation;
- a missing Workflow after durable intent becomes Inconclusive and is not
  recreated;
- positional identities are re-derived after every Run read;
- persisted terminal evidence can reconstruct compact Run status;
- owned premature uncordons are repaired while a Run is active or retained.

## Non-goals

- Kueue or other queue integration;
- recurring certification or automatic retry;
- arbitrary WorkflowTemplate, PodSpec, toleration, capability, or host-path
  pass-through;
- general-purpose workload scheduling;
- live Node draining or eviction of workloads that existed before the cordon.
