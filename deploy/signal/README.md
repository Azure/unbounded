# Project Signal controller

Project Signal is a Kubernetes controller for admitting approved idle Nodes to
certification workflows. The upstream integration provides the API, controller,
CRDs, controller Deployment, and an optional generic participant adapter for
ordinary Pods and Kubeflow MPIJobs. It intentionally does not publish
certification scripts, runner images, CertificationProfiles, hardware policy,
or environment-specific queue configuration.

## Architecture

```text
approved Node request
              |
              v
      CertificationProfile
              |
              v
 reserve Nodes and snapshot the ordered 15-check script plan
              |
              v
 immutable CertificationRun input + SHA-256 digest
              |
              v
 one Argo Workflow with a serial, visible 15-task DAG
              |
              v
 per-check typed results -> aggregate evidence -> Node projection -> cleanup
```

The controller snapshots all 15 selected script keys into one run-owned
immutable ConfigMap. Updating a source affects only later runs. The controller
does not contain or publish certification script bodies.

## Prerequisites

- Kubernetes with the `Node`, `Pod`, and `ConfigMap` APIs.
- Argo Workflows `argoproj.io/v1alpha1` Workflow API.
- A controller image built from `cmd/project-signal-controller`.
- An operator-owned script ConfigMap and execution image.
- An operator-owned WorkflowTemplate implementing the interface below, or the
  included `project-signal-participant-adapter` WorkflowTemplate.
- Explicitly reviewed Node selectors, resource requests, tolerations, policy
  values, and cleanup behavior.

The execution image used with the included adapter must contain Bash and the
standard `mkdir`, `chmod`, `base64`, and `tr` utilities, plus every tool required
by the selected script. The adapter image and execution image are separate: the
upstream adapter creates and observes workloads, while the operator-owned
execution image runs the script.

The included adapter supports `v1` Pods and `kubeflow.org/v2beta1` MPIJobs.
Project Signal does not install MPI Operator or Kueue. Its adapter RBAC is
namespace-scoped and intentionally grants only the Pod, Pod log, MPIJob,
Workflow, and Argo task-result access needed by the included template.

## WorkflowTemplate interface

The referenced WorkflowTemplate must accept these string parameters:

| Parameter | Meaning |
| --- | --- |
| `run-name` | CertificationRun name |
| `run-uid` | CertificationRun UID |
| `participants-json` | Ordered JSON array of participant indexes, Node names, immutable UIDs, capacity, and allocatable resources |
| `participant-count` | Number of atomically admitted participants |
| `script-configmap` | Immutable run-owned ConfigMap |
| `script-key` | Selected ConfigMap key |
| `script-digest` | Algorithm-qualified SHA-256 of the selected script bytes (`sha256:<64 lowercase hex>`) |
| `script-requires-runtime-stage` | Whether the selected script requires a runtime stage |
| `runtime-stage-name` | Optional validated runtime stage name |
| `backend-api-version` | Backend API version from the profile |
| `backend-kind` | Backend kind from the profile |
| `backend-active-deadline-seconds` | Participant backend deadline, shorter than the outer Workflow deadline |
| `queue` | Optional backend queue |
| `runner-image` | Digest-pinned execution image |
| `tolerations` | JSON-encoded toleration list |
| `script-environment` | JSON-encoded, validated environment map |
| `workload-policy` | JSON-encoded immutable participant-container resources, security context, and bounded volume policy |
| `backend-evidence-bytes` | Per-check evidence output bound, at most 64 KiB |
| `mpi-payload-mode` | Compatibility input for operator-defined templates |
| `expected-gpu-product-regex` | Compatibility input for operator-defined templates |
| `expected-gpu-count` | Compatibility input for operator-defined templates |
| `observed-gpu-capacity` | Compatibility input copied from admitted Node inventory |
| `observed-gpu-allocatable` | Compatibility input copied from admitted Node inventory |
| `gpu-resource-count` | GPU count, defaults to `0` for Pod diagnostics; must be positive for MPIJobs |
| `privileged` | Explicit boolean, defaults to `false` |
| `attempt` | One-based attempt number |

The Workflow must publish only these output parameters:

| Output | Limit | Meaning |
| --- | ---: | --- |
| `verdict` | 32 bytes | `Pass`, `Fail`, or `Inconclusive` |
| `reason` | 4 KiB | Bounded diagnostic reason |
| `measurements` | 16 KiB | Bounded measurement summary |
| `backend-resources` | 16 KiB | Optional JSON array of backend workload identities and covered participant indexes; required for a multi-Node `Pass` |
| `backend-evidence` | 64 KiB per check | Required redacted pre-cleanup backend object, container state, and bounded current/previous log projection |

Unexpected, duplicate, malformed, or oversized outputs are rejected and the
run remains fail-closed. Each `backend-resources` entry must contain
`apiVersion`, `kind`, `name`, `uid`, and the zero-based `participantIndexes`
that the workload executed. `namespace` is optional and defaults to the
CertificationRun namespace; an explicitly different namespace is rejected.
A multi-Node successful result must cover every admitted participant.
The controller reads outputs only
from each check's unique deterministic DAG task node. Missing, malformed,
duplicate, or ambiguous task identity and outputs fail closed. Operator script
parameters, including GPU expectations and observed inventory, are carried in
`script-environment`. Their existing individual compatibility inputs remain
available to external templates; the supplied adapter uses the environment.

Before foreground deletion, the adapter captures and writes a typed v1
projection for every participant backend to the Argo output path. The
projection includes Pod or MPIJob state and
conditions, launcher Job conditions, participant container exit state and
restart count, and bounded current and previous container log tails. Generated
participant commands emit lifecycle markers for result-directory preparation
and final verdict, reason, and measurements file presence. The projection
records those markers without retaining the result payload itself.

Each retained log tail is limited to 512 bytes and redacts bearer credentials,
secret-like assignments, URL user information, private-key blocks, and complete
result-marker payloads. Object reason and message fields are also bounded. If a
required object or current log cannot be captured, a restarted container's
previous log is unavailable, or the projection must omit content to fit the
configured per-check output limit (at most 64 KiB), `complete` is false and the run cannot pass. Capture and
cleanup failures remain explicit, while an earlier backend execution failure
continues to be the causal result reason.

The adapter cannot observe or serialize its own state if the adapter container
or Node terminates before this pre-cleanup write. In that boundary the
`backend-evidence` output is missing and the controller rejects the Workflow
outputs as Inconclusive; the backend cause remains unknown rather than being
inferred. A termination after the write may leave the evidence file available
to Argo, but collection still depends on the Workflow executor surviving long
enough to publish output parameters.

The controller cross-checks typed backend evidence against the declared backend,
namespace, exact workload UID, participant coverage, and required Pod/container
and current/previous log records. A template's `complete: true` alone is not
sufficient. Log collection requests the latest 64 lines, retains at most 64 KiB
for result parsing, and projects redacted tails of at most 512 bytes into evidence.
Earlier log content is intentionally outside this bounded result contract.

After durable terminal evidence is recorded, the controller foreground-deletes
the Workflow and waits for owned backend descendants to be removed before
releasing Node reservations. This also covers adapter crashes, cancellation,
and a controller crash between Workflow creation and status persistence.
The six-hour Workflow TTL is a fallback, not the normal evidence-retention
mechanism; immutable terminal evidence remains available after Workflow cleanup.
The controller's namespace-scoped backend patch/delete and owner-finalizer
permissions allow Kubernetes' owner-reference admission checks to authorize
blocking deletion fences. It does not receive backend workload creation rights.

## Multi-Node execution

Participant admission is deterministic and atomic. The requesting Node is
participant zero, additional eligible Nodes are selected by name, and a failed
reservation rolls back every reservation already acquired. Each participant is
bound by Node UID and an immutable capacity and allocatable snapshot in the
CertificationRun.

The participant group is a certification boundary, not one distributed process
group. The included adapter creates one independently pinned backend workload
per participant. Ordinary diagnostics use one Pod per participant. MPI
diagnostics use one MPIJob per participant with one launcher and one GPU worker;
ordinary Pods and MPI workers use required Node affinity on the immutable
`metadata.name` field to bind to the exact admitted Node without bypassing
scheduler admission. Only the worker receives the configured GPU limit. The
adapter propagates the Workflow active deadline, applies the profile's optional
queue, records API-assigned workload UIDs, reads the final typed result from
bounded workload logs, aggregates diagnostic state separately from backend
execution state, and foreground-deletes every created workload.

The selected script is invoked with no arguments for a Pod backend,
`mpi-launcher <gpu-count>` in an MPIJob launcher, and `mpi-worker` in its worker.
Scripts write `verdict`, `reason`, and optional `measurements.txt` under
`/results`. The script exit code must match the result verdict: `Pass` exits 0,
`Fail` exits 1, and `Inconclusive` exits 2. A valid bounded result remains
authoritative when its verdict-associated exit code makes the backend Pod or
MPIJob terminally unsuccessful. The adapter removes reserved result files
before each invocation and removes trailing newline bytes from repository-owned
result files before encoding them. It rejects missing files, malformed values,
stale or contradictory result/exit combinations, and any other exit code.
Partial backend execution, identity gaps, deadline interruption, and cleanup
failure also remain `Inconclusive`.

The MPI launcher also applies a compatibility fence for
[`kubeflow/mpi-operator#615`](https://github.com/kubeflow/mpi-operator/issues/615),
whose suspend/resume race is fixed upstream by
[`kubeflow/mpi-operator#868`](https://github.com/kubeflow/mpi-operator/pull/868)
at commit `3a1e56d8accd0d924703d79af3149e3fa8ba1b01`. Before invoking the diagnostic
launcher command, the adapter writes `/tmp/project-signal-hostfile` from the
operator's deterministic worker DNS contract
`<mpijob>-worker-<index>.<mpijob>.<namespace>.svc` and the validated
`slotsPerWorker`. It exports that file through
`OMPI_MCA_orte_default_hostfile`, so an empty or stale operator-injected
`/etc/mpi/hostfile` cannot control launcher behavior after resume.

The launcher waits for every generated host to complete SSH `/bin/true` with
`BatchMode=yes`, `ConnectTimeout=2`, `ConnectionAttempts=1`,
`StrictHostKeyChecking=accept-new`, and a launcher-local
`UserKnownHostsFile=/tmp/project-signal-known-hosts`. SSH stderr remains visible
for diagnosis. The wait is capped at 30 seconds total and is always at least
one second shorter than `backendActiveDeadline`; timeout exits 124 and names
the last unready host. Launcher and worker replica restart policies are both
`Never`, preserving the result prelude and marker.

The adapter keeps the current launcher log primary and reads the previous
container log defensively. A single valid previous result can recover a
restarted launcher when the current log has no result, identical current and
previous results select the current log, and conflicting results or a malformed
current marker fail closed. Bounded current/previous availability, byte counts,
parse status, and selected-source metadata are included in aggregate
measurements before foreground backend cleanup. This preserves the first
execution failure while keeping any cleanup failure separately visible.

Other backend kinds remain operator-supplied and are not installed or granted
permissions by the included adapter.

## Render and install

Render the controller manifests:

```bash
make signal-manifests \
  SIGNAL_CONTROLLER_IMAGE=<controller-image@sha256:digest> \
  SIGNAL_ADAPTER_IMAGE=<adapter-image@sha256:digest>
```

Local adapter builds use the separate mutable
`SIGNAL_ADAPTER_BUILD_IMAGE=<registry/repository:tag>` staging reference because
container engines cannot tag a local build with an `image@sha256:` reference.

Generated output is written to `deploy/signal/rendered/`. Validate it with the
target cluster before applying:

```bash
kubectl apply --dry-run=server -f deploy/signal/rendered/crd/
kubectl apply --dry-run=server -f deploy/signal/rendered/
kubectl apply -f deploy/signal/rendered/crd/
kubectl apply -f deploy/signal/rendered/
```

The upstream manifests install no profiles. Operators must create a
CertificationProfile with exactly 15 checks that reference operator-owned
script ConfigMaps and digest-pinned runner images. To use the included adapter,
set `spec.workflow.name: project-signal-participant-adapter`. Each
`spec.checks[]` entry selects either:

```yaml
checks:
  - id: <stable-check-id>
    order: 0
    script:
      name: <script-configmap>
      key: <script-key>
      digest: sha256:<approved-script-digest>
    runnerImage: <runner-image>@sha256:<digest>
    backend:
      apiVersion: v1
      kind: Pod
      queue: <optional-local-queue>
    backendActiveDeadline: 19m
    resultBounds:
      reasonBytes: 4096
      measurementsBytes: 16384
      backendResourcesBytes: 16384
      backendEvidenceBytes: 65536
    maxAttempts: 1
```

or an MPIJob backend, optionally submitted through a Kueue queue:

```yaml
checks:
  - id: <stable-check-id>
    order: <0-14>
    backend:
      apiVersion: kubeflow.org/v2beta1
      kind: MPIJob
      queue: <local-queue>
```

When either backend supplies a queue, the adapter writes the standard
`kueue.x-k8s.io/queue-name` label. Pod and MPIJob backends may both omit it for
standalone execution without Kueue. A `kueueTopology` policy still requires an
explicit queue.

Checks may set `spec.checks[].workloadPolicy` when a diagnostic needs explicit
participant-container policy. The controller validates and snapshots this
policy into the immutable CertificationRun before reserving Nodes. The included
adapter applies it to the ordinary Pod container or the MPIJob worker only; the
unpinned MPI launcher does not receive participant resources, host devices, or
extra mounts.

```yaml
workloadPolicy:
  kueueTopology:
    requiredLevel: kubernetes.io/hostname
  resources:
    requests:
      cpu: "8"
      memory: 32Gi
      ephemeral-storage: 20Gi
    limits:
      cpu: "8"
      memory: 32Gi
      ephemeral-storage: 40Gi
  securityContext:
    privileged: false
    capabilities:
      add: [IPC_LOCK]
  hostNetwork: true
  workingDir: /opt/diagnostic
  results:
    sizeLimit: 2Gi
  volumes:
    - name: work
      mountPath: /work
      emptyDir:
        sizeLimit: 16Gi
    - name: host-proc
      mountPath: /host/proc
      readOnly: true
      hostPath:
        path: /proc
        type: Directory
```

Each extra volume must select exactly one `hostPath`, `emptyDir`, or
`persistentVolumeClaim` source. Names and mount paths must be unique;
`scripts`, `results`, `/scripts`, and `/results` are reserved. Paths must be
clean and absolute, resource quantities must be nonnegative, and limits cannot
be below requests. The policy is bounded to 16 volumes and 16 KiB of serialized
input. GPU requests and limits remain controlled by `gpu-resource-count`, which
sets equal requests and limits on the participant container. Kubernetes
admission and operator policy still decide whether a capability, host path,
device, PVC, or privileged context is allowed; technical rendering is not
authorization.

Stage-aware scripts declare
`spec.checks[].script.requiresRuntimeStage: true` and require an explicit
`spec.checks[].runtimeStage.name`. The name is a lowercase DNS label of at most
63 characters and is snapshotted into the immutable CertificationRun and
evidence. The adapter exports it as `PROJECT_SIGNAL_STAGE_NAME` only to ordinary
participant Pods and MPIJob workers. MPI launchers never receive it. Scripts
that are not stage-aware leave both fields unset.

Every profile sets two distinct deadlines:

```yaml
deadline: 20m
checks:
  - backendActiveDeadline: 19m
```

`deadline` is the outer Workflow deadline for backend observation, result and
evidence projection, and cleanup. Each check's `backendActiveDeadline` is
rendered to its participant Pod or MPIJob `activeDeadlineSeconds`. It must be
positive and leave at least 60 seconds before the outer deadline; the
controller rejects an invalid ordering before reserving Nodes.

`kueueTopology.requiredLevel` is an explicit Kueue Topology Aware Scheduling
request. It must be a valid Kubernetes label key and requires a nonempty backend
queue. The adapter writes
`kueue.x-k8s.io/podset-required-topology: <requiredLevel>` on ordinary
participant Pods and MPIJob worker Pod templates only. It never annotates the
unpinned MPI launcher, does not accept arbitrary annotations, and does not
choose a topology level when the field is omitted.

`hostNetwork: true` is an explicit participant policy for diagnostics that must
observe the selected Node's network namespace, such as physical NIC inventory.
The adapter sets `dnsPolicy: ClusterFirstWithHostNet` with it. For MPIJobs this
applies only to the Node-pinned worker; the unpinned launcher remains outside
the host network namespace. Omitting the field keeps Kubernetes' default Pod
networking.

`gpu-resource-count` remains an operator-supplied profile parameter. It may be
zero for Pod diagnostics and must be positive for MPIJobs. The MPIJob policy is
fail-closed: `backoffLimit: 0`, `cleanPodPolicy: All`, a one-hour post-finish
TTL, replica-level `restartPolicy: Never`, `WaitForWorkersReady`, and
`runLauncherAsWorker: false`.

Operators can override the nonnegative MPI retry and TTL values through profile
parameters `backend-backoff-limit` and
`backend-ttl-seconds-after-finished`. These become
`BACKEND_BACKOFF_LIMIT` and `BACKEND_TTL_SECONDS_AFTER_FINISHED` in the validated
script environment and remain explicit per-profile policy rather than cluster
defaults.

A `CertificationProfile` defines exactly 15 ordered checks. Admission snapshots
the complete plan and selected participants once into one immutable
`CertificationRun`. Each check binds its script identity and digest, runner,
backend, workload and topology policy, runtime inputs, deadline, result bounds,
participant-role inclusion or exclusion, and `maxAttempts: 1`.

The controller creates one Argo Workflow with an inline DAG entrypoint. Every
task invokes the configured adapter `certification` template through
`templateRef`; dependencies make execution serial and visible in
`status.nodes`. `executionPolicy.onFailure` is immutable and is either `Stop`
or `Continue`. A stopped task is recorded as skipped/not-run, never inferred as
a diagnostic result.

Before creating the Workflow, the controller durably records its deterministic
name and attempt in CertificationRun status. Once that creation intent exists,
a missing Workflow terminates the Run as Inconclusive and is never recreated.
This fail-closed fence prevents a controller outage plus Workflow TTL cleanup
from executing the one-time certification suite twice.

Participant roles and topology constraints are generic typed policy. Profiles
can, for example, bind one role to one participant, another role to three
participants, require the latter to span at least two values of an operator
chosen topology label, and exclude a role from a specific check without
publishing environment-specific names or label values upstream.

## Results and evidence

Workflow infrastructure state and certification verdicts are separate:

```text
executionState=Completed, verdict=Pass|Fail, reason=<diagnostic reason>
executionState=Error,     verdict=Inconclusive, reason=<workflow reason>
```

Ambiguous or incomplete workflow execution remains `Inconclusive`. The
controller identifies each check only by its deterministic DAG task-node name
and rejects missing, duplicate, malformed, partial, or unexpected outputs. An
aggregate `Pass` requires all 15 checks, exact participant and backend
coverage, complete v1 backend evidence, and successful backend cleanup.
If backend execution and cleanup both fail, `reason` retains the first causal
backend failure and the bounded `cleanup-failure` Workflow output records
`BackendCleanupFailed` separately. Cleanup failure always forces
`executionState=Error` and `verdict=Inconclusive`, including after an otherwise
successful diagnostic.

CertificationRun status retains bounded measurements, backend identities, and
the SHA-256 digest and completeness of each check's backend-evidence v1
document. Full backend-evidence content and script snapshots are retained only
in the immutable terminal evidence artifact. Small artifacts use one
ConfigMap; larger artifacts use an immutable manifest plus bounded immutable
chunks so valid grouped evidence cannot exceed one Kubernetes object limit.

## Node metadata

Controller-owned labels:

| Key | Values |
| --- | --- |
| `signal.unbounded-cloud.io/state` | `pending`, `testing`, `passed`, `failed`, `inconclusive` |
| `signal.unbounded-cloud.io/eligibility` | `blocked`, `production` |
| `signal.unbounded-cloud.io/profile` | bounded profile identifier |

Pending, testing, failed, and inconclusive states use
`eligibility=blocked`. A successful run projects `passed/production`. Older
runs cannot overwrite a newer terminal projection.
Projection and request clearing also require the current reservation to name
the same Run, so historical Runs cannot overwrite a newer pending/testing run
or consume its request. Completed finalizer cleanup is not replayed.

## Operations

Request a run by applying the stable trigger label:

```bash
kubectl label node <approved-node> \
  signal.unbounded-cloud.io/certification-request=<profile-name>
```

Cancel an active run:

```bash
kubectl -n project-signal patch certificationrun <run-name> \
  --type=merge -p '{"spec":{"cancel":true}}'
```

Before removing the controller, wait for active grouped runs to become
terminal. Argo Workflows and any workload backend may be shared
cluster services and must not be removed as part of Project Signal cleanup.

## Non-goals

This upstream integration does not provide:

- certification scripts or proprietary thresholds;
- a runner image;
- bundled CertificationProfiles or WorkflowTemplates;
- workload-backend installation or runner RBAC;
- destructive stress, reboot, cordon, drain, or remediation;
- an external evidence service;
- authoritative recovery of ambiguous external mutations.
