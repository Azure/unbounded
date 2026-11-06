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

- `CertificationTest` is the cluster-scoped reusable test catalog entry. It
  declares typed parameters and metrics, participant topology, security
  requirements, and one immutable Script, Container, or WorkflowTemplate
  driver. Trust is an explicit status decision: `Approved`, `Diagnostic`,
  `Rejected`, or `Revoked`.
- `CertificationSuite` is the cluster-scoped executable policy. New Suites
  define ordered Startup, Validation, and Terminal phases containing named
  stages. Stages within one phase execute concurrently.
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
- `project-signal-participant-adapter` executes resolved Script, Container,
  and WorkflowTemplate stages and captures typed invocation evidence. Script
  and Container stages use participant-pinned Pods. WorkflowTemplate stages
  verify the approved template name, live resource version, and semantic
  digest before creating the bounded child Workflow. Child Workflows must use
  the dedicated `project-signal-vllm-workload` ServiceAccount; the adapter
  rejects another ServiceAccount, external template references, `podSpecPatch`,
  or a template-level override of the controller-owned Run UID label.
- `project-signal-participant-adapter` is the fixed controller-to-adapter
  WorkflowTemplate used by every legacy check and resolved stage. Profiles and
  Suites cannot replace that control-plane template.

Argo Workflows is required. The Kubeflow MPI Operator is required only for
legacy Suites that still contain an `MPIJob` check.

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
Set `SIGNAL_RESOLVER_IMAGES` to a comma-separated list of digest-pinned
resolver images when external input resolution is part of the accepted plan.
Those images are recorded in immutable plan and evidence provenance.

Rendering also installs an immutable `project-signal-gpu-inventory-v1` script
ConfigMap and two catalog entries:

- `project-signal-gpu-inventory-v1` is the single canonical qualification
  test after an operator records an `Approved` trust decision for its exact
  generation and content digest.
- `project-signal-gpu-inventory-compat-v1` is compatibility-only and must
  receive `Diagnostic` trust. R01 through R15 may reference it to preserve
  legacy names and order, but those stages receive no qualification credit.

The renderer independently hashes the exact ConfigMap script bytes and fails
when they do not match `SIGNAL_GPU_INVENTORY_SCRIPT_DIGEST`.

Install the rendered CRDs and namespaced resources:

```bash
kubectl apply -f deploy/signal/rendered/crd/
kubectl apply -f deploy/signal/rendered/
```

The install creates:

- the `CertificationTest`, `CertificationSuite`, `CertificationProfile`,
  `CertificationRun`, `CertificationPolicy`, and `NodeCertification` CRDs;
- the `project-signal` namespace;
- controller, adapter, and minimal child-workload service accounts and RBAC;
- the fixed participant adapter WorkflowTemplate;
- the immutable GPU inventory source and untrusted catalog specifications;
- the controller Deployment.

The controller requires cluster permission to get, list, watch, update, and
patch Nodes because it owns participant reservation markers, cordons, and
qualification-label reconciliation. Exact Node UID checks in the controller
prevent a replacement Node with the same name from inheriting reservation or
qualification state. The controller watches Profiles, Suites, and Tests for
admission changes, and Policies, NodeCertifications, Runs, Profiles, Suites,
and Nodes for qualification changes. Legacy checks use the deployment-owned
runner image. Generic Script and Container drivers must carry their own exact
digest-pinned runtime identity in the approved Test.

The canonical GPU inventory Test queries actual GPU indices and UUIDs but
never emits UUIDs into durable evidence. It requires unique canonical indices
forming exactly `0..N-1`, unique canonical GPU UUIDs, and one nonempty driver
version shared by every observed GPU. The inventory digest is SHA-256 over the
actual UUID bytes in numeric index order, separated by one NUL byte with no
trailing NUL, and is recorded with the `sha256:` prefix. Participant
start/end driver versions and inventory digests must match. Visible and
covered device labels remain redacted as `gpu-index-N`; missing, mixed,
malformed, duplicate, gapped, partial, or changed identity evidence fails
closed.

## CertificationSuite contract

A phase-based Suite is an ordered execution plan:

| Field | Meaning |
|---|---|
| `spec.phases` | Ordered phases; Startup must be first and Terminal must be last |
| `phase.role` | `Startup`, `Validation`, or `Terminal` |
| `phase.stages` | Named stages that execute concurrently within the phase |
| `stage.test` | Exact Test name, UID, generation, and accepted content digest |
| `stage.schedule` | `RunOnce`, `Continuous`, or `Periodic` execution contract |
| `stage.assertions` | Typed assertions over metrics declared by the referenced Test |
| `stage.retry` | Optional retry of infrastructure failures only |

`RunOnce` always has one positive, whole-second `invocationTimeout` and cannot
declare repeated-execution fields. `Continuous` and `Periodic` additionally
require a positive coverage window, minimum execution count, and maximum gap.
`Periodic` also requires an interval. Startup and Terminal stages must use
`RunOnce`.

The adapter owns repeated invocation scheduling. An interruption never
silently resumes or aggregates prior activity into a new qualification window.
Persisted invocation evidence may be used only to resume the same immutable
execution identity. A replacement Run starts coverage from zero.

The controller resolves every Test and fails admission when:

- the Test UID, generation, or accepted content digest differs from the Suite;
- trust status is missing, stale, rejected, or revoked;
- required parameters, participant roles, or metric assertions are invalid;
- a mutable environment input remains unresolved for an otherwise approved
  stage;
- a WorkflowTemplate's live semantic digest differs from its approved driver
  digest; or
- phase deadlines cannot be represented safely.

An approved Test with complete immutable inputs may be qualification eligible.
A Diagnostic Test or a stage with incomplete inputs is always diagnostic.
The adapter detects repeated qualification stages with the same Test identity
and driver identity and forces the duplicate execution inconclusive. Renaming
the same source cannot create independent qualification evidence.

### CertificationTest drivers

- **Script:** uses an immutable ConfigMap, OCI artifact, or Git commit plus an
  exact source digest, digest-pinned runner image, and explicit resource
  requirements. The current Kubernetes executor supports immutable ConfigMap
  script sources.
- **Container:** uses a digest-pinned image with bounded command, arguments,
  resources, and reviewed security requirements.
- **WorkflowTemplate:** uses an approved name and semantic SHA-256 digest. At
  admission, the controller reads the live template, recomputes the digest of
  its `spec`, and pins `name@resourceVersion`. At execution, the adapter
  verifies all three values before creating the child Workflow. The approved
  spec and every nested template must select only
  `project-signal-vllm-workload`; external template references,
  `podSpecPatch`, and template-level Run UID label overrides are rejected.
  This prevents generic child code from selecting the adapter or controller
  identity or escaping the approved semantic digest.

Test trust is a separate operator or policy decision through the
`CertificationTest/status` subresource. Applying the catalog specification
does not approve it.

After applying a Test specification, read its live generation, independently
compute the canonical specification digest, and record the trust decision
through the status subresource. For example:

```bash
test_name=project-signal-gpu-inventory-v1
generation="$(kubectl get certificationtest "$test_name" -o jsonpath='{.metadata.generation}')"
content_digest="$(go run ./hack/cmd/manifest-digest \
  --file deploy/signal/rendered/05-gpu-inventory-tests.yaml \
  --kind CertificationTest --name "$test_name" --field spec)"
kubectl patch certificationtest "$test_name" --subresource=status --type=merge \
  -p "{\"status\":{\"observedGeneration\":${generation},\"trust\":{\"state\":\"Approved\",\"contentDigest\":\"${content_digest}\",\"reason\":\"reviewed immutable qualification test\"}}}"
```

Use `Diagnostic`, not `Approved`, for
`project-signal-gpu-inventory-compat-v1`. A new Test generation invalidates
the prior trust decision until it is reviewed again.

### Legacy compatibility

Legacy `spec.scriptBundle` plus `spec.checks` remains readable and executable
for migration. A Suite must use exactly one model: phases or legacy checks.
Do not create new qualification definitions with the legacy model.

The prior R01 through R15 baseline used 15 byte-identical copies of one GPU
inventory script. Migration preserves those names only as ordered diagnostic
stages referencing `project-signal-gpu-inventory-compat-v1`. The qualification
Suite contains one separate stage referencing the canonical
`project-signal-gpu-inventory-v1` Test. The compatibility stages must not be
represented as 15 independent tests or credited 15 times.

Suite edits affect only future Runs.

The acceptance Suite catalog is disabled until the applied Test objects have
live UIDs, generations, reviewed content digests, and trust decisions. Render
the catalog only after recording those exact identities:

```bash
make signal-manifests \
  SIGNAL_ACCEPTANCE_SUITES_ENABLED=true \
  SIGNAL_GPU_INVENTORY_TEST_UID=<live-uid> \
  SIGNAL_GPU_INVENTORY_TEST_GENERATION=<live-generation> \
  SIGNAL_GPU_INVENTORY_TEST_CONTENT_DIGEST=sha256:<reviewed-spec-digest> \
  SIGNAL_GPU_COMPAT_TEST_UID=<live-uid> \
  SIGNAL_GPU_COMPAT_TEST_GENERATION=<live-generation> \
  SIGNAL_GPU_COMPAT_TEST_CONTENT_DIGEST=sha256:<reviewed-spec-digest>
```

This renders separate `project-signal-gpu-baseline-v1` and
`project-signal-gpu-compat-v1` Suites. The baseline has one qualification
stage. The compatibility Suite has 15 ordered diagnostic phases so R01 through
R15 remain sequential without creating duplicate qualification credit.
Environment-specific Profiles are not rendered by the static package because
their participant selectors, namespaces, storage, network, and resolver inputs
must be reviewed at cutover. The separate execution-authority overlay renders
only the reviewed baseline, compatibility-diagnostic, and vLLM Profiles.

## CertificationProfile contract

A Profile contains the participant policy and references one Suite:

| Field | Meaning |
|---|---|
| `spec.participants.count` | Total Nodes selected for a Run |
| `spec.participants.selector` | Labels every selected Node must match |
| `spec.suite.name` | Cluster-scoped Suite resolved and pinned at admission |
| `spec.environment` | Approved namespace, storage, network, secret names, resolvers, model caches, object stores, and resource bounds |

The requesting Node is participant zero. The remaining participants are
selected deterministically from currently ready, schedulable, unreserved
Nodes matching the participant selector. Named participant-role bindings must
satisfy the Test topology. Environment references become qualification inputs
only when they resolve to immutable digest identities. Secret values never
enter Profile, Run, plan digest, or evidence objects. Profile edits affect only
future Runs.

For example, define the executable Suite separately from the participant
Profile:

```yaml
apiVersion: signal.unbounded-cloud.io/v1alpha1
kind: CertificationSuite
metadata:
  name: standard
spec:
  phases:
    - name: validation
      role: Validation
      stages:
        - name: gpu-inventory
          test:
            name: project-signal-gpu-inventory-v1
            uid: <test-uid>
            generation: 1
            contentDigest: sha256:<approved-test-content-digest>
          schedule:
            mode: RunOnce
            invocationTimeout: 2m
          assertions:
            - name: nvidia-smi-available
              metric: nvidiaSmiAvailable
              operator: BooleanEquals
              expected:
                type: Boolean
                booleanValue: true
            - name: eight-gpus-visible
              metric: observedGpuCount
              operator: Range
              minimum:
                type: Integer
                integerValue: 8
              maximum:
                type: Integer
                integerValue: 8
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

1. resolves the Profile's Suite, every Test identity and trust decision, all
   typed bindings and assertions, immutable environment inputs, and driver
   provenance;
2. selects the requesting Node plus any additional participants;
3. atomically records the Run reservation name, participant Node UID, and
   `spec.unschedulable=true` on each participant;
4. resolves phase and stage schedules into one immutable plan and derives its
   exact deadline and digest;
5. creates one immutable `CertificationRun`;
6. projects the pending certification state and marks admission ready.

Ordinary workloads cannot schedule onto cordoned participants. Signal backend
Pods receive only the fixed unschedulable toleration needed to execute on
participants reserved by this Run. Script and Container Pods are pinned to the
admitted Node. A WorkflowTemplate invocation receives the complete participant
and role bindings plus the primary participant name; the approved template
remains responsible for its bounded workload shape. The adapter has read-only
`get` access to Nodes solely to record and verify scheduled Node UIDs; it
cannot mutate Nodes.

## CertificationRun contract

The Run spec snapshots:

- Profile name, UID, and generation;
- Suite name, UID, and generation;
- ordered participants with Node name and Node UID;
- the resolved phases and stages, Test identities, typed schemas, drivers,
  parameters, participant roles, schedules, retries, assertions, and immutable
  input identities;
- controller, adapter, WorkflowTemplate, runner, and resolver provenance;
- for legacy Runs only, the immutable script bundle and check snapshots;
- monotonic `cancel` and `release` actions.

The controller derives the Workflow deadline from ordered phase deadlines.
Concurrent stages contribute the maximum stage deadline for their phase.
Repeated schedules include the coverage window, invocation timeout, approved
infrastructure retry tail, and bounded evidence reserve.

Participant and stage positions remain immutable within the Run. Participant
zero is the requesting Node. Legacy check positions still derive stable IDs
starting at R01.

Run status contains:

- `observedGeneration`;
- phase and bounded reason;
- Workflow name, UID, and observed phase;
- evidence URI and SHA-256 digest;
- phase and stage summaries with typed assertion and continuity results;
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
For Reset and Revoke semantics, it also records a monotonic watermark for the
latest terminal non-passing Run. That watermark survives Run and evidence
deletion so older passing Runs cannot silently regain credit. No ledger
individually owns the shared gate label.

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

## Bounded vLLM acceptance canary

The vLLM canary is a separate WorkflowTemplate Test and must run separately
from the GPU inventory baseline. It is disabled by default. Rendering it
requires explicit immutable and bounded inputs:

```bash
make signal-manifests \
  SIGNAL_VLLM_CANARY_ENABLED=true \
  SIGNAL_VLLM_CANARY_IMAGE=<approved-harness>@sha256:<digest> \
  SIGNAL_VLLM_CANARY_CPU=<approved-cpu-quantity> \
  SIGNAL_VLLM_CANARY_MEMORY=<approved-memory-quantity> \
  SIGNAL_VLLM_MODEL_CACHE_PVC=<approved-read-only-cache-pvc>
```

The repository publishes `project-signal-vllm-canary` and
`project-signal-vllm-model-cache` through the existing
`.github/workflows/images.yaml` workflow. The harness is based on vLLM v0.25.1
at
`docker.io/vllm/vllm-openai@sha256:e4f88a835143cd22aee2397a26ec6bb80b3a4a6fe0c882bcbc63822904766089`.
The cache image contains only the approved
`EleutherAI/pythia-70m-deduped` Apache-2.0 snapshot at revision
`e93a9faa9c77e5d09219f6c868bfc7a1bd65593c`. Its canonical sorted TSV
manifest digest is
`sha256:52b9b0fa1a8a67424d2e8f7994675698c60e3eb1a7925091930ecc4958dbcd7e`;
the `model.safetensors` digest is
`sha256:3da388330e4549156d76b58d6d268c63cd005e9336b4f4d2d378421e7b7a33fd`.
The deterministic prompt fixture digest is
`sha256:87c87bfc4863e490bdef827c5aa1c0b48f47311e2fed3fd002c3e05dfad23d99`.

Pre-stage the immutable snapshot before creating a Run. Use published image
digests, an approved `ReadWriteOnce` StorageClass, a unique DNS-compatible
seed identity, and a bounded handoff timeout:

```bash
make signal-manifests \
  SIGNAL_VLLM_CANARY_ENABLED=true \
  SIGNAL_VLLM_CANARY_IMAGE=<approved-harness>@sha256:<digest> \
  SIGNAL_VLLM_CANARY_CPU=<approved-cpu-quantity> \
  SIGNAL_VLLM_CANARY_MEMORY=<approved-memory-quantity> \
  SIGNAL_VLLM_MODEL_CACHE_PVC=<approved-cache-pvc> \
  SIGNAL_VLLM_MODEL_CACHE_SEED_ENABLED=true \
  SIGNAL_VLLM_MODEL_CACHE_IMAGE=<approved-cache-image>@sha256:<digest> \
  SIGNAL_VLLM_MODEL_CACHE_SEED_ID=<unique-seed-identity> \
  SIGNAL_VLLM_MODEL_CACHE_CAPACITY=<approved-capacity> \
  SIGNAL_VLLM_MODEL_CACHE_STORAGE_CLASS=<approved-storage-class>

kubectl apply -f deploy/signal/rendered/07-vllm-model-cache.yaml
make signal-vllm-cache-handoff \
  SIGNAL_VLLM_MODEL_CACHE_PVC=<approved-cache-pvc> \
  SIGNAL_VLLM_MODEL_CACHE_SEED_ID=<unique-seed-identity> \
  SIGNAL_VLLM_MODEL_CACHE_HANDOFF_TIMEOUT=<approved-timeout-seconds>s
```

The handoff command prints the seed Job UID and cache PVC UID before waiting so
they can be retained with the verifier log. It fails closed unless the seed Job
completes, its verifier log is readable, the exact Job UID remains unchanged,
a UID-preconditioned foreground deletion removes the Job and its Pods, the
exact PVC UID remains unchanged and bound, and two consecutive observations
find zero Pods referencing the PVC and zero `VolumeAttachment`
objects referencing its bound PV before the bounded timeout. Only after that
command succeeds may operators render and apply the runtime WorkflowTemplate,
Test, Suite, Profile, or create the separate vLLM Run. Re-render with
`SIGNAL_VLLM_MODEL_CACHE_SEED_ENABLED=false` before applying the runtime
package. The adapter acquires a create-only, exact-UID-deleted ConfigMap lock
for every Workflow PVC before checking storage readiness and holds it through
foreground Workflow cleanup. A concurrent Run therefore fails closed before
creating a second cache consumer. The adapter also rejects Workflow startup
while any Pod references the cache PVC or any `VolumeAttachment` references
its bound PV.

The approved storage class must provide `ReadWriteOnce` access. The portable
contract is strictly sequential: one seed Pod writes and verifies the
immutable cache, the seed Job and Pod are removed, the CSI attachment is
observed absent, and only then one vLLM Pod with one container mounts the exact
revision subpath read-only. The Workflow retry limit remains zero. This is not
general `ReadWriteMany`, multi-node shared-cache, or concurrent-canary support.

The seed Job has no API token or RBAC. It verifies the embedded manifest and
every file before copying, writes through a Pod-UID-scoped temporary
directory, verifies the copy, and atomically publishes the final snapshot.
An existing valid target is accepted idempotently; an invalid target, extra
file, symlink, digest mismatch, or size mismatch fails closed. Record the PVC
UID, Job UID, cache image digest, manifest digest, and successful verifier log.
The handoff script performs the required exact-UID re-read and foreground
deletion. The PVC is deliberately retained as a pre-staged input and is
mounted into the runtime Pod through the exact revision subpath as read-only.

Do not delete the PVC as ordinary Run cleanup. Dispose of it only in a
separate reviewed operation after proving that no active Run or Workflow
references its exact UID, recording a final digest verification, and
re-reading the UID immediately before deletion. A canceled or restarted Run
removes only its append-only result ConfigMap and child Workflow resources;
it does not mutate or replace the shared model cache.

The renderer computes the semantic digest of the exact
`project-signal-vllm-canary-v1` WorkflowTemplate `spec`. Supplying
`SIGNAL_VLLM_WORKFLOW_TEMPLATE_DIGEST=sha256:<digest>` makes rendering fail on
a mismatch. The corresponding Test records only that exact template digest.
To render the separate vLLM acceptance Suite, also enable the acceptance Suite
catalog and provide the live vLLM Test UID, generation, reviewed content
digest, exact model identity, and bounded invocation timeout:

```bash
make signal-manifests \
  SIGNAL_VLLM_CANARY_ENABLED=true \
  SIGNAL_ACCEPTANCE_SUITES_ENABLED=true \
  SIGNAL_VLLM_TEST_UID=<live-uid> \
  SIGNAL_VLLM_TEST_GENERATION=<live-generation> \
  SIGNAL_VLLM_TEST_CONTENT_DIGEST=sha256:<reviewed-spec-digest> \
  SIGNAL_VLLM_MODEL_IDENTITY=<approved-model>@sha256:<digest> \
  SIGNAL_VLLM_MODEL_REVISION=<exact-40-to-64-character-hex-revision> \
  SIGNAL_VLLM_MODEL_WEIGHTS_DIGEST=sha256:<weight-set-digest> \
  SIGNAL_VLLM_PROMPT_FIXTURE_DIGEST=sha256:<digest> \
  SIGNAL_VLLM_REQUEST_TIMEOUT=<approved-duration> \
  SIGNAL_VLLM_INVOCATION_TIMEOUT=<approved-duration> \
  <the other required baseline and vLLM render inputs>
```

The approved harness contract receives canonical invocation, Run, plan, stage,
Test, participant, role, parameter, resolved-input, and result ConfigMap
values. It rejects any resolved input that does not contain the exact approved
model manifest digest. It
must create one bounded result ConfigMap containing the canonical invocation
result envelope. The Workflow and its Pods use the dedicated
`project-signal-vllm-workload` ServiceAccount. That identity can create only
the result ConfigMap and create or patch Argo WorkflowTaskResults; it cannot
create Pods, create Workflows, read Nodes, or use the controller or adapter
identity. The template pins eight GPUs to the exact primary participant,
mounts an operator-approved read-only model cache, disables container restart,
bounds temporary storage, and provides a bounded memory-backed `/dev/shm` for
the eight tensor-parallel workers without exposing host IPC. The stage
invocation timeout becomes the child Workflow deadline.

Before creating the vLLM Suite and Profile, resolve and approve:

- the harness image digest;
- model identity, exact source revision, weight-set digest, tokenizer,
  optional remote-code, and cache snapshot identities;
- the exact WorkflowTemplate live resource version and semantic digest;
- CPU, memory, cache capacity, startup timeout, request timeout, and total
  invocation timeout; and
- one deterministic prompt and decoding fixture.

Hard assertions are limited to service readiness, exact model identity,
revision and weight-set digest, request success, positive prompt and
completion tokens, zero named server/request error sources, zero named
harness-container/Workflow-Pod restart sources, tensor parallel size eight,
exactly eight visible and actively used GPUs, and successful cleanup. The
unprivileged harness does not report restart counts. After validating the raw
result, the adapter observes the exact owner-fenced child Workflow and its
Run-UID-fenced Pods, replaces any forged restart metric with named container
restart and additional Workflow-Pod counts, and records the raw result digest
separately before assertions or persistence. The child template does not use
Argo Pod GC or TTL cleanup, so Pods remain available for this observation and
are removed by the adapter's UID-fenced foreground Workflow deletion. Missing
or ambiguous Pod ownership fails closed. All Test assertions are explicit Suite assertions.
Cleanup is also part of the canonical result envelope, and the adapter prevents
a passed invocation from reporting failed Kubernetes cleanup.
Latency, time to first token, output throughput, and GPU memory are recorded
as measurements only until separate thresholds are approved. Do not infer
MPI, burn, collective, CUDA correctness, or general serving performance from
this canary. Rendering and offline mocks prove the contract only; GPU runtime
success is unproven until an authorized executor runs the exact published
digests on an eight-device participant.

## Terminal behavior

The controller never releases a participant before evidence is durable and
all Workflow and backend resources are gone.

- `Succeeded`: cleanup completes and participants are released automatically.
- `Failed`, `Inconclusive`, or `Canceled`: cleanup completes, but participants
  remain reserved and cordoned.
- Set `spec.release: true` to release a non-successful Run after inspection.
- Deleting a Run requests teardown. Its finalizer stops and removes owned
  workloads, verifies participant identity and reservation ownership, removes
    owned cordons and reservations, deletes legacy snapshots and generic stage
    resources, and then permits deletion.

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

- Run, Profile, Suite, Test, participant, Workflow, and resolved-plan
  identities;
- immutable parameters, driver provenance, resolved inputs, and typed metric
  schemas;
- aggregate execution state, verdict, reason, phase, and timestamps;
- each phase and stage verdict, execution mode, invocation sequence, assertion
  result, observed coverage, maximum gap, per-device interval coverage, and
  cleanup outcome;
- redacted participant fingerprints and device aliases; and
- controller, adapter, Script or Container runner, resolver, and
  WorkflowTemplate provenance.

The adapter persists one immutable stage result ConfigMap owned by the Run
Workflow. Script and Container invocation results are read from canonical
bounded logs. WorkflowTemplate drivers create one bounded result ConfigMap,
which the adapter verifies and deletes with the child Workflow after capture.
Result names derive from the immutable execution and invocation identity, so a
restart never overwrites a prior invocation result. Cleanup uses exact UID
preconditions and retains durable evidence before removing the append-only
result and child resources.
The controller verifies ownership, Run and plan identity annotations, content
digest, runner provenance, and bounded payloads before including the stage in
durable evidence.

Raw Node names, GPU UUIDs, registry identities, and mutable model references
must not enter typed measurements or durable artifacts. Participant identity
projection uses the configured keyed HMAC contract when available; otherwise
the external projection leaves the participant hash null.

If any backend cannot be quiesced, the adapter reports failed cleanup and the
stage cannot pass. The controller deletes the Run Workflow, owned stage result
ConfigMaps, and remaining owned backends only after final evidence is durable.
`WorkloadCleanupComplete=True` is the authoritative record that this
controller-owned cleanup finished.

The cutover inventory must also prove zero attempt-owned Workflows,
`WorkflowTaskResult` objects, Pods, Services, PVCs or model-cache claims,
ConfigMaps, and Secrets. Shared pre-existing caches are preserved only when
their exact UID and non-ownership by the Run are recorded before execution.
Do not treat deletion of the parent Workflow as proof that all children are
gone.

Qualification reads terminal Run and evidence references, verifies the
controller, adapter, runner, resolver, WorkflowTemplate, Test, plan, Node UID,
and evidence digests, and credits only evidence that satisfies the Policy.

## Operations

Inspect Runs and lifecycle conditions:

```bash
kubectl get certificationruns -n project-signal
kubectl describe certificationrun -n project-signal <run-name>
```

Inspect the immutable canonical Script source and catalog:

```bash
kubectl get configmap -n project-signal project-signal-gpu-inventory-v2 -o yaml
kubectl get certificationtests.signal.unbounded-cloud.io
```

Controller restarts are safe:

- Workflow creation intent is recorded before creation;
- a missing Workflow after durable intent becomes Inconclusive and is not
  recreated;
- positional identities are re-derived after every Run read;
- persisted terminal evidence can reconstruct compact Run status;
- owned premature uncordons are repaired while a Run is active or retained.

## Execution-authority overlay

The static package remains inert. A separate authority render is required to
record `acceptanceSuitesEnabled=true`, `cutoverApproved=true`, and the exact
source commit and static package digest. The immutable authority ConfigMap is
versioned by the first 12 hexadecimal characters of both identities. Its
namespace must be distinct from the workload and Script namespaces, and the
controller receives only `get` access to the exact authority and state
ConfigMaps.

The rendered state ConfigMap always bootstraps with `hardAbort: "true"`.
`12-execution-authority-transition.yaml` is a one-time JSON Patch that first
tests for `"true"` and then replaces it with `"false"`. Apply the ordinary
authority objects first, verify zero active Runs and attempt-owned resources,
then apply the transition deliberately:

```bash
kubectl apply --server-side --dry-run=server \
  -f deploy/signal/rendered/10-acceptance-suites.yaml \
  -f deploy/signal/rendered/11-execution-authority.yaml
kubectl patch configmap project-signal-execution-state \
  -n <authority-namespace> --type=json \
  --patch-file deploy/signal/rendered/12-execution-authority-transition.yaml \
  --dry-run=server
```

Remove `--dry-run=server` only after the exact render, live object comparison,
image pullability, and hard-stop inventory are approved. Reapplying
`11-execution-authority.yaml` restores `hardAbort: "true"`; it cannot silently
release an abort. Rendering with execution authority disabled gives the
controller an empty static package binding, so stale authority objects cannot
continue admitting or starting Runs.

The overlay does not create a Run. With `VLLMCanaryEnabled=false`, it requires
no vLLM image, Test identity, Profile UID, cache, WorkflowTemplate, Profile, or
Policy requirement. This baseline-only mode can deploy while vLLM remains
deferred.

When vLLM is explicitly enabled, start the canonical baseline and vLLM canary
separately, only after the prior Run is terminal, cleaned up, and released:

```bash
kubectl label node <node-name> \
  signal.unbounded-cloud.io/certification-request=project-signal-gpu-baseline-8x-v1
# Inspect terminal evidence, cleanup, and release before continuing.
kubectl label node <node-name> \
  signal.unbounded-cloud.io/certification-request=project-signal-vllm-canary-8x-v1
```

The compatibility Profile remains diagnostic-only and receives no Policy
credit. The Policy always requires one current pass from the baseline Profile;
it requires one vLLM pass only when vLLM is explicitly enabled. It uses
`nonPassBehavior: Revoke` and keeps `gateProjection.mode: None`.

### Upgrade and migration

Use a guarded cutover. The old controller must finish old-runtime cancellation
and release before any CRD or controller replacement:

1. Freeze repository pushes and record the exact accepted commit ancestry.
   Capture a final old-runtime snapshot of every Signal CR, installed CRD,
   RBAC object, WorkflowTemplate, Deployment, image reference and live
   container `imageID`, active and terminal Run, evidence reference,
   reservation, Node cordon, and legacy eligibility label. Store the complete
   YAML export as the rollback bundle.
2. If an old-runtime Run is still active, deliberately set `spec.cancel=true`
   while the old controller remains deployed. Classify the result as Canceled
   or another truthful non-pass; cancellation is never a successful
   qualification result. Wait for terminal evidence and
   `WorkloadCleanupComplete=True`, inspect it, then set `spec.release=true`.
3. Require `Released=True`, the exact pre-run cordon/reservation projection,
   and zero attempt-owned Workflows, WorkflowTaskResults, Pods, Services,
   PVCs, ConfigMaps, Secrets, MPIJobs, and reservations. Only then may the CRD
   or controller upgrade begin.
4. Back up all Signal CRs before applying CRDs. Validate every historical
   object against the new served schemas and record which legacy objects are
   readable but intentionally non-qualifying. Preserve CRDs across rollback
   when new stored fields or Policy/ledger objects exist.
5. Render with qualification protection disabled and verify the computed
   WorkflowTemplate digest is stable across two renders.
6. Apply all six CRDs first, then controller and adapter RBAC, catalog sources,
   the adapter WorkflowTemplate, and finally the controller Deployment.
7. Verify all six CRDs are Established, controller rollout is ready, the
   WorkflowTemplate adapter image matches `--adapter-image`, and the rendered
   WorkflowTemplate digest matches `--workflow-template-digest`. Record one
   commit-to-controller/adapter/runner/template digest mapping and verify the
   live Pod `imageID` values match it.
8. Inventory and remove stale direct
   `signal.unbounded-cloud.io/eligibility=production` labels. Confirm
   `--qualification-gate-protected=false`, no protected-label projection, no
   controller mutation of Policy specifications, and
   `NodeRestrictionUnverified` on protected-label ledger projection.
9. Record Test trust only after independently hashing exact Test specs, source
   bytes, images, and WorkflowTemplate semantics. The immutable approval
   record must contain Test name, UID, generation, and reviewed content digest;
   a stale generation must not remain trusted.
10. Create the canonical GPU inventory Suite with one qualification stage.
   Preserve R01-R15 only in a separate compatibility Suite using Diagnostic
   trust. Mechanically verify that no qualification Suite or Policy references
   R01-R15 and that the ledger receives exactly one canonical Test credit.
11. Run the GPU inventory baseline and vLLM acceptance as separate Runs.
   Independently recompute Test, source, image, WorkflowTemplate, plan, and
   evidence digests from the live objects before accepting either result.
12. Create or migrate Policies only after terminal evidence compatibility is
   understood. Existing Runs remain readable, but only evidence satisfying the
   current Policy and evidence-v2 identity requirements receives qualification
   credit. Existing ledgers without a latest non-pass watermark remain
   readable; future Reset or Revoke evaluations persist the monotonic
   watermark.
13. Freeze pushes again and recheck exact-head ancestry, required CI, and
    review immediately before deployment. Abort on any head, base, digest,
    imageID, schema, ownership, cleanup, redaction, or prerequisite mismatch.
14. Enable protected projection only in a separate reviewed rollout after the
   operator guard prerequisites are positively verified.

Do not delete `CertificationPolicy` or `NodeCertification` CRDs while objects
exist. Removing a CRD deletes its API storage and is not a rollback.

The review package used for a deployment decision must be context-free and
redacted. It pins the source commit and every controller, adapter, runner,
resolver, harness, model, weight-set, Script, Test, and WorkflowTemplate
digest; lists cluster prerequisites and registry-reachability checks; includes
deterministic render plus client and server dry-run output; scans artifacts for
raw participant, infrastructure, provider, and registry identities; and states
only the bounded claims proven by the planned baseline and canary.

### Verification

Verify the deployed provenance and default-off gate:

```bash
kubectl get crd \
  certificationtests.signal.unbounded-cloud.io \
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
RBAC can read all six certification resources and perform the documented
Run, ledger, evidence, and exact-identity Node operations. Verify the adapter
retains Node `get`, Pod and ConfigMap lifecycle plus logs, Workflow and
WorkflowTemplate execution access, and Argo `workflowtaskresults` `create` and
`patch`. Verify the child-workload Role contains only ConfigMap `create` and
WorkflowTaskResult `create`/`patch`, has no Pod or Workflow creation, and binds
only `project-signal-vllm-workload`.

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

Abort and begin rollback on a digest or live `imageID` mismatch, schema
rejection, historical-object incompatibility, ownership ambiguity, stalled
cleanup, reservation-release failure, unexpected protected or legacy label,
redaction failure, missing registry artifact, failed hard assertion, or any
resource beyond the reviewed bounded shape. Recompute immutable digests
independently from live objects before deciding that rollback completed.

Never restore a protected label manually. Let the default-off qualification
reconciler remove it before rolling back controller behavior.

## Non-goals

- Kueue or other queue integration;
- a Campaign CR, recurring campaign orchestration, or automatic cross-Run
  retry;
- unapproved WorkflowTemplates, arbitrary PodSpec, toleration, capability, or
  host-path pass-through;
- treating one successful Run as production eligibility;
- converting repeated short Runs into an uninterrupted-duration claim;
- general-purpose workload scheduling;
- live Node draining or eviction of workloads that existed before the cordon.
