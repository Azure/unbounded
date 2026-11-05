# Project Signal controller

Project Signal performs one certification run for a newly joined Node or a
small, explicitly selected group of Nodes. The controller snapshots the
approved Profile, referenced Suite, and scripts, reserves and cordons every
participant, runs the Suite's resolved executable plan through Argo Workflows,
persists one immutable evidence document, and cleans up all temporary
workloads. Certification Policies separately evaluate terminal evidence into
per-Node qualification ledgers. A successful Run alone is not production
eligibility.

Project Signal does not integrate with Kueue. Queue admission and repeatable
fleet scheduling are intentionally deferred.

## Components

- `CertificationSuite` is the cluster-scoped executable policy: it selects an
  immutable script bundle and defines a named ordered executable plan.
- `CertificationProfile` is cluster-scoped admission policy: it defines
  participant selection and references one Suite.
- `CertificationRun` is a namespaced, immutable execution snapshot.
- `CertificationPolicy` is cluster-scoped qualification policy: it defines
  which terminal Runs count, how many are required, how qualification expires,
  and how non-passing evidence affects prior credit.
- `NodeCertification` is the namespaced, controller-owned qualification ledger
  for one exact Node UID and Policy identity.
- `project-signal-controller` admits Nodes, owns reservations and cordons,
  creates the Workflow, persists status and evidence, performs teardown, and
  reconciles qualification ledgers and the optional protected qualification
  gate.
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

Rendering computes the SHA-256 digest of the exact rendered
`project-signal-participant-adapter` WorkflowTemplate bytes and records that
digest in the controller arguments. Supplying
`SIGNAL_WORKFLOW_TEMPLATE_DIGEST=sha256:<digest>` turns that value into an
expected digest and makes rendering fail if the rendered template differs.
Controller, adapter, and runner images must all be immutable digest references.

Install the rendered CRDs and namespaced resources:

```bash
kubectl apply -f deploy/signal/rendered/crd/
kubectl apply -f deploy/signal/rendered/
```

The install creates:

- the `CertificationSuite`, `CertificationProfile`, `CertificationRun`,
  `CertificationPolicy`, and `NodeCertification` CRDs;
- the `project-signal` namespace;
- controller and adapter service accounts and RBAC;
- the fixed participant adapter WorkflowTemplate;
- the controller Deployment.

The controller requires cluster permission to get, list, watch, update, and
patch Nodes because it owns participant reservation markers, cordons, and
qualification-label reconciliation. Exact Node UID checks in the controller
prevent a replacement Node with the same name from inheriting reservation or
qualification state. The controller watches Profiles and Suites for admission
changes, and Policies, NodeCertifications, Runs, Profiles, Suites, and Nodes
for qualification changes. The deployment-owned runner image is copied into
every immutable Run check snapshot. Profile and Suite authors cannot replace
it.

## CertificationSuite contract

A Suite is a named ordered executable plan. It owns the script bundle and
check definitions that admission resolves into a Run:

| Field | Meaning |
|---|---|
| `spec.scriptBundle.name` | Immutable source ConfigMap containing the selected scripts |
| `spec.scriptBundle.digest` | Aggregate SHA-256 digest of the selected script contents in check order |
| `spec.checks` | Ordered executable plan; position defines the stable check ID starting at R01 |

**Implementation and safety limit:** a Suite currently accepts between 1 and
256 checks. This guardrail bounds Kubernetes object growth, CEL evaluation
cost, generated Workflow size, and evidence size. It is not the product
abstraction exposed to operators.

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
- the resolved executable plan snapshot;
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

## Qualification policy and ledger contract

A Suite and Profile describe one execution. They do not describe a recurring
campaign or a production qualification gate. Each request creates one
independent immutable Run. Project Signal has no Campaign CR.

A `CertificationPolicy` evaluates terminal Run evidence into one
`NodeCertification` ledger per exact Node UID. Policy requirements select a
Profile identity, define how participant credit is assigned, require a pass
count, and accept either the Profile's current plan or an explicit set of plan
digests. Optional variation rules can require distinct plan digests or minimum
separation between credited Runs. The Policy also defines:

- qualification validity and renewal timing;
- the Node, boot, operating system, kubelet, driver, and GPU fingerprint fields
  that invalidate prior evidence;
- whether a non-passing Run is ignored, resets progress, or revokes
  qualification; and
- whether the shared protected qualification label is requested.

The ledger records contributing terminal Runs, accepted plan digests,
qualification phase, expiry, fingerprint, and evidence identity. Expired,
revoked, replaced-identity, or policy-mismatched ledgers do not qualify a Node.
No ledger individually owns the shared gate label.

When one or more active Policies request `ProtectedLabel`, the shared label is
present only when **every** such Policy has a current, qualified, unexpired
ledger for the exact Node UID:

```text
node-restriction.kubernetes.io/project-signal-qualified=true
```

This is an AND rule across all active `ProtectedLabel` Policies. Deleting a
ledger or Policy, changing a Policy, expiring qualification, changing the Node
fingerprint, or receiving revoking evidence causes reconciliation and removal
when the rule is no longer satisfied.

The protected gate is disabled by default. While disabled, the controller
removes the protected label and reports `GateProjected=False` with reason
`NodeRestrictionUnverified`. The Kubernetes API does not expose a trustworthy
way for this controller to discover API-server admission plugins, so deployment
must never infer protection.

Before enabling the gate, an operator must positively verify all of the
following outside Project Signal:

1. the API server uses the Node authorizer;
2. the NodeRestriction admission plugin is enabled; and
3. non-node identities with `patch` or `update` access to Nodes are restricted
   to explicitly trusted controllers and operators.

After recording that review, render with the explicit guard:

```bash
make signal-manifests \
  SIGNAL_CONTROLLER_IMAGE=<controller-image>@sha256:<digest> \
  SIGNAL_ADAPTER_IMAGE=<adapter-image>@sha256:<digest> \
  SIGNAL_CERTIFICATION_RUNNER_IMAGE=<runner-image>@sha256:<digest> \
  SIGNAL_QUALIFICATION_GATE_PROTECTED=true \
  SIGNAL_QUALIFICATION_GATE_GUARD=node-authorizer-node-restriction-rbac-verified
```

Rendering fails closed if gate protection is requested without that exact
operator confirmation. The confirmation does not discover or change
API-server configuration; it records that the prerequisite review happened.

One long-running check and many repeated Runs are different qualification
semantics. A Suite can define a single backend with a long continuous deadline.
If that execution is interrupted, a replacement starts a new duration from
zero. Multiple shorter Runs do not prove one uninterrupted duration unless a
Policy explicitly defines a repeated-Run requirement, and even then they remain
separate credited executions rather than a continuous observation.

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

Evidence manifest v2 stores bounded, chunked evidence and includes:

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

Qualification reads terminal Run and evidence references, verifies the
controller, adapter, runner, WorkflowTemplate, plan, Node UID, and evidence
digests, and credits only evidence that satisfies the Policy. Adapter inputs,
mounts, and environment do not change for evidence v2.

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

### Upgrade and migration

Use a guarded two-stage upgrade:

1. Capture the currently installed CRDs, RBAC, WorkflowTemplate, Deployment,
   immutable image references, active Runs, terminal Runs, evidence objects,
   reservations, and Node cordon state as a rollback bundle.
2. Require zero active Runs, Workflows, backend Pods, MPIJobs, and reservations
   before replacing controller execution semantics.
3. Render with qualification protection disabled and verify the computed
   WorkflowTemplate digest is stable across two renders.
4. Apply all five CRDs first, then controller and adapter RBAC, the
   WorkflowTemplate, and finally the controller Deployment.
5. Verify all five CRDs are Established, controller rollout is ready, the
   WorkflowTemplate adapter image matches `--adapter-image`, and the rendered
   WorkflowTemplate digest matches `--workflow-template-digest`.
6. Confirm the default controller argument is
   `--qualification-gate-protected=false`, stale direct
   `signal.unbounded-cloud.io/eligibility=production` labels are removed, and
   protected-label ledgers report `NodeRestrictionUnverified`.
7. Create or migrate Policies only after terminal evidence compatibility is
   understood. Existing Runs remain readable, but only evidence satisfying the
   current Policy and evidence-v2 identity requirements receives qualification
   credit.
8. Enable protected projection only in a separate reviewed rollout after the
   operator guard prerequisites are positively verified.

Do not delete `CertificationPolicy` or `NodeCertification` CRDs while objects
exist. Removing a CRD deletes its API storage and is not a rollback.

### Verification

Verify the deployed provenance and default-off gate:

```bash
kubectl get crd \
  certificationsuites.signal.unbounded-cloud.io \
  certificationprofiles.signal.unbounded-cloud.io \
  certificationruns.signal.unbounded-cloud.io \
  certificationpolicies.signal.unbounded-cloud.io \
  nodecertifications.signal.unbounded-cloud.io

kubectl get deployment -n project-signal project-signal-controller \
  -o jsonpath='{.spec.template.spec.containers[0].args}'

kubectl get workflowtemplate -n project-signal \
  project-signal-participant-adapter -o yaml
```

Independently hash the exact rendered WorkflowTemplate file and compare the
algorithm-qualified digest with the controller argument. Verify controller
RBAC can read all five certification resources and perform the documented
Run, ledger, evidence, and exact-identity Node operations. Verify the adapter
retains Node `get` and Argo `workflowtaskresults` `create` and `patch`.

### Safe rollback

If deployment or a bounded canary fails:

1. stop and clean only attempt-owned workloads;
2. release reservations using exact Run and Node UID ownership checks;
3. render and apply the current controller with
   `SIGNAL_QUALIFICATION_GATE_PROTECTED=false`, then wait until the protected
   qualification label is absent;
4. restore the captured predecessor RBAC, WorkflowTemplate, Deployment, and
   immutable images;
5. retain the new CRDs whenever Policies or ledgers exist; and
6. verify participant cordon state, reservations, active-resource counts,
   terminal Run bytes, and unrelated resources match the rollback snapshot.

Never restore a protected label manually. Let the default-off qualification
reconciler remove it before rolling back controller behavior.

## Non-goals

- Kueue or other queue integration;
- a Campaign CR, recurring campaign orchestration, or automatic retry;
- arbitrary WorkflowTemplate, PodSpec, toleration, capability, or host-path
  pass-through;
- treating one successful Run as production eligibility;
- converting repeated short Runs into an uninterrupted-duration claim;
- general-purpose workload scheduling;
- live Node draining or eviction of workloads that existed before the cordon.
