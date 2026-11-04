# Project Signal controller

Project Signal performs one certification run for a newly joined Node or a
small, explicitly selected group of Nodes. The controller snapshots the
approved Profile, referenced Suite, and scripts, reserves and cordons every
participant, runs the ordered check suite through Argo Workflows, persists one
immutable evidence document, and cleans up all temporary workloads.

Project Signal does not integrate with Kueue. Queue admission and repeatable
fleet scheduling are intentionally deferred.

## Components

- `CertificationSuite` is the cluster-scoped executable policy: it selects an
  immutable script bundle and defines the ordered checks.
- `CertificationProfile` is cluster-scoped admission policy: it defines
  participant selection and references one Suite.
- `CertificationRun` is a namespaced, immutable execution snapshot.
- `project-signal-controller` admits Nodes, owns reservations and cordons,
  creates the Workflow, persists status and evidence, and performs teardown.
- `project-signal-participant-adapter` creates Pod or MPIJob backends and
  captures typed evidence. Ordinary completed Pods remain until controller
  teardown. The adapter retains MPI workers long enough to prove their Node
  identities, persists each check result in an immutable ConfigMap, and then
  deletes the MPIJob before the next sequential check so its GPUs are released.
  On setup, observation, or wall-clock deadline failures, the adapter first
  persists the available evidence and then quiesces uncertain backends.
- `project-signal-participant-adapter` is a fixed WorkflowTemplate used by
  every check. Profiles and Suites cannot replace it or pass arbitrary
  Workflow wiring.

Argo Workflows is required. The Kubeflow MPI Operator is required only when a
referenced Suite contains an `MPIJob` check.

## First deployment

Render the manifests with immutable controller, adapter, and certification
runner images:

```bash
make signal-manifests \
  SIGNAL_CONTROLLER_IMAGE=<controller-image>@sha256:<digest> \
  SIGNAL_ADAPTER_IMAGE=<adapter-image>@sha256:<digest> \
  SIGNAL_CERTIFICATION_RUNNER_IMAGE=<runner-image>@sha256:<digest>
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
Nodes because it owns participant reservation markers and cordons. It also
watches Profiles and Suites so changes can trigger admission reconciliation.
The deployment-owned runner image is copied into every immutable Run check
snapshot. Profile and Suite authors cannot replace it.

## CertificationSuite contract

A Suite contains the executable choices that vary between certification test
plans:

| Field | Meaning |
|---|---|
| `spec.scriptBundle.name` | Immutable source ConfigMap containing the selected scripts |
| `spec.scriptBundle.digest` | Aggregate SHA-256 digest of the selected script contents in check order |
| `spec.checks` | Between 1 and 256 ordered checks; position defines the stable check ID starting at R01 |

The source ConfigMap must set `immutable: true` and must not use `binaryData`.
The aggregate digest is derived by computing the algorithm-qualified SHA-256
digest of each selected script's exact bytes, writing those digest strings
with one newline after each string in Suite check order, and computing the
SHA-256 digest of that manifest. Certification admission rejects the request
if the current source bytes do not match the pinned aggregate digest.
Unreferenced ConfigMap keys are not part of the digest because they cannot
execute in that Suite.

Each check contains:

| Field | Meaning |
|---|---|
| `scriptKey` | Key in the Suite's `spec.scriptBundle.name`; admission snapshots the verified bytes and records their SHA-256 digest |
| `backend` | `Pod` or `MPIJob` |
| `backendActiveDeadline` | Backend execution deadline |
| `gpuCount` | GPU request; must be positive for MPIJob |
| `workloadPolicy` | Narrow container resources and host access policy |

Workload policy is intentionally constrained:

- `ipcLock` is the only supported added Linux capability;
- host sources must be under `/proc`, `/sys`, or
  `/var/lib/project-signal`;
- host mounts are read-only and their volume names and `Directory` type are
  controller-owned;
- scheduling tolerations are controller-owned;
- arbitrary PodSpec, Workflow, retry, TTL, queue, and cleanup settings are not
  accepted.

Suite edits affect only future Runs.

## CertificationProfile contract

A Profile contains the participant policy and references one Suite:

| Field | Meaning |
|---|---|
| `spec.participants.count` | Total Nodes selected for a Run |
| `spec.participants.selector` | Labels every selected Node must match |
| `spec.suite.name` | Cluster-scoped Suite resolved and pinned at admission |

The requesting Node is participant zero. The remaining participants are
selected deterministically from currently ready, schedulable, unreserved
Nodes matching the participant selector and the resource floor derived from
the largest per-Node check request. Every Suite check runs on this complete
immutable participant set. Profile edits affect only future Runs.

For example, define the executable Suite separately from the participant
Profile:

```yaml
apiVersion: signal.unbounded-cloud.io/v1alpha1
kind: CertificationSuite
metadata:
  name: standard
spec:
  scriptBundle:
    name: standard-scripts
    digest: sha256:<aggregate-digest>
  checks:
    - scriptKey: node-readiness.sh
      backend: Pod
      backendActiveDeadline: 10m
---
apiVersion: signal.unbounded-cloud.io/v1alpha1
kind: CertificationProfile
metadata:
  name: standard-pool
spec:
  participants:
    count: 2
    selector:
      matchLabels:
        signal.unbounded-cloud.io/certification-pool: standard
  suite:
    name: standard
```

## Starting one certification

Add the request label to a schedulable Node:

```bash
kubectl label node <node-name> \
  signal.unbounded-cloud.io/certification-request=<profile-name>
```

Admission then:

1. resolves the Profile's Suite and validates every configured check, source ConfigMap immutability, and the pinned aggregate script digest;
2. selects the requesting Node plus any additional participants;
3. atomically records the Run reservation name, participant Node UID, and
   `spec.unschedulable=true` on each participant;
4. copies all verified scripts into one immutable Run-owned ConfigMap;
5. creates one immutable `CertificationRun`;
6. projects the pending certification state and marks admission ready.

Ordinary workloads cannot schedule onto cordoned participants. Signal backend
Pods receive only the fixed GPU toleration and the fixed
`node.kubernetes.io/unschedulable` toleration needed to execute on the Nodes
reserved by this Run. Ordinary Pod backends are pinned to one admitted Node
name and UID. An MPI check creates one coordinated MPIJob with one launcher and
one worker per participant. Required Node affinity limits workers to the exact
admitted Node names and UIDs, required hostname anti-affinity places one worker
per Node, and evidence maps each scheduled worker back to the immutable
participant identity. The adapter has read-only `get` access to Nodes solely to
record and verify the scheduled Node UID; it cannot mutate Nodes.

## CertificationRun contract

The Run spec snapshots:

- Profile name, UID, and generation;
- Suite name, UID, and generation;
- ordered participants with Node name and Node UID;
- the immutable script bundle name, UID, and aggregate digest;
- the ordered check suite snapshot;
- monotonic `cancel` and `release` actions.

The controller derives the Workflow deadline from the immutable backend
deadlines plus a fixed one-minute evidence reserve for each check.

Participant and check identity is positional. Participant zero is the
requesting Node. Check positions derive stable IDs starting at R01.
The controller derives these identities after every API read rather than
serializing duplicate index and ID fields.

Run status contains:

- `observedGeneration`;
- phase and bounded reason;
- Workflow name, UID, and observed phase;
- evidence URI and SHA-256 digest;
- positional check phases and bounded reasons;
- `Cordoned`, `WorkloadCleanupComplete`, and `Released` conditions.

Measurements, backend identities, backend evidence, and execution timestamps
are retained only in the durable evidence document.

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

- Run, Profile, Suite, participant, script-bundle, and Workflow identities;
- the immutable check snapshots and script contents;
- aggregate execution state, verdict, reason, phase, and timestamps;
- each positional check's verdict and timing;
- typed Pod or MPIJob evidence, including Pods, containers, states, bounded
  logs, and capture errors;
- backend identities and participant positions;
- a per-check cleanup disposition showing that deletion is deferred to the
  controller.

The adapter persists typed backend evidence and leaves completed ordinary Pods
in place. For each check, it creates one immutable result ConfigMap owned by
the Workflow. The controller verifies the ConfigMap's ownership, run and check
identity annotations, content digest, and bounded `result.json` payload before
including the result in the durable evidence document. MPIJobs use
`cleanPodPolicy: None` so the launcher and every worker remain available for
evidence capture. After that evidence is durable, the adapter terminates the
retained MPI workers without their ordinary Pod grace period, deletes the
MPIJob in the foreground, and waits for the complete ownership tree to
disappear before completing the task. If any backend cannot be quiesced, the
adapter task fails and later checks are skipped rather than overlapping
execution. The controller deletes the Workflow, its owned per-check result
ConfigMaps, and all remaining owned backends only after the final evidence
document is durable.
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
