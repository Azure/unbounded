# Project Signal controller

Project Signal is a Kubernetes controller for admitting approved idle Nodes to
certification workflows. The upstream integration provides the API, controller,
CRDs, controller Deployment, and an optional generic participant adapter for
ordinary Pods and Kubeflow MPIJobs. It intentionally does not publish
certification scripts, runner images, CertificationProfiles, hardware policy,
or environment-specific queue configuration.

## Architecture

```text
approved Node or recurring policy
              |
              v
      CertificationProfile
              |
              v
 reserve Nodes and snapshot the selected ConfigMap key
              |
              v
 immutable CertificationRun input + SHA-256 digest
              |
              v
 operator-supplied or generic adapter WorkflowTemplate
              |
              v
 typed result -> retained evidence -> Node projection -> cleanup
```

The controller snapshots one selected script key from a source ConfigMap into
a run-owned immutable ConfigMap. Updating the source affects only later runs.
The controller does not contain or publish certification script bodies.

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
| `participants` | Comma-separated admitted Node names retained for compatibility |
| `participants-json` | Ordered JSON array of participant indexes, Node names, immutable UIDs, capacity, and allocatable resources |
| `participant-count` | Number of atomically admitted participants |
| `request-node-name` | Node that triggered the grouped run |
| `script-configmap` | Immutable run-owned ConfigMap |
| `script-key` | Selected ConfigMap key |
| `script-digest` | SHA-256 of the selected script bytes |
| `backend-api-version` | Backend API version from the profile |
| `backend-kind` | Backend kind from the profile |
| `queue` | Optional backend queue |
| `runner-image` | Digest-pinned execution image |
| `tolerations` | JSON-encoded toleration list |
| `script-environment` | JSON-encoded, validated environment map |
| `workload-policy` | JSON-encoded immutable participant-container resources, security context, and bounded volume policy |
| `mpi-payload-mode` | Optional operator-defined execution mode |
| `expected-gpu-product-regex` | Optional operator policy |
| `expected-gpu-count` | Optional operator policy |
| `observed-gpu-capacity` | Capacity copied from the admitted Node |
| `observed-gpu-allocatable` | Allocatable count copied from the admitted Node |
| `gpu-resource-count` | Optional operator policy |
| `privileged` | Optional operator policy |
| `attempt` | One-based attempt number |

The Workflow must publish only these output parameters:

| Output | Limit | Meaning |
| --- | ---: | --- |
| `verdict` | 32 bytes | `Pass`, `Fail`, or `Inconclusive` |
| `reason` | 4 KiB | Bounded diagnostic reason |
| `measurements` | 16 KiB | Bounded measurement summary |
| `backend-resources` | 16 KiB | Optional JSON array of backend workload identities and covered participant indexes; required for a multi-Node `Pass` |

Unexpected, duplicate, malformed, or oversized outputs are rejected and the
run remains fail-closed. Each `backend-resources` entry must contain
`apiVersion`, `kind`, `name`, `uid`, and the zero-based `participantIndexes`
that the workload executed. `namespace` is optional. A multi-Node successful
result must cover every admitted participant.

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
only the worker is pinned to the participant Node and receives the configured
GPU limit. The adapter propagates the Workflow active deadline, uses the
profile's queue, records API-assigned workload UIDs, reads the final typed result
from bounded workload logs, aggregates diagnostic state separately from backend
execution state, and foreground-deletes every created workload.

The selected script is invoked with no arguments for a Pod backend,
`mpi-launcher <gpu-count>` in an MPIJob launcher, and `mpi-worker` in its worker.
Scripts write `verdict`, `reason`, and optional `measurements.txt` under
`/results`. A nonzero process exit remains an execution error and cannot be
converted into a diagnostic `Fail`. Missing results, partial backend execution,
identity gaps, malformed output, deadline interruption, and cleanup failure are
all `Inconclusive`.

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
CertificationProfile that references their own script ConfigMap and
digest-pinned runner image. To use the included adapter, set
`spec.workflow.name: project-signal-participant-adapter` and select either:

```yaml
backend:
  apiVersion: v1
  kind: Pod
  queue: <optional-local-queue>
```

or an explicit Kueue queue:

```yaml
backend:
  apiVersion: kubeflow.org/v2beta1
  kind: MPIJob
  queue: <local-queue>
```

When a Pod backend supplies a queue, the adapter writes the standard
`kueue.x-k8s.io/queue-name` label. MPIJob backends require a queue; Pod backends
may omit it only when the operator intentionally runs them outside Kueue.

Profiles may set `spec.workloadPolicy` when a diagnostic needs explicit
participant-container policy. The controller validates and snapshots this
policy into the immutable CertificationRun before reserving Nodes. The included
adapter applies it to the ordinary Pod container or the MPIJob worker only; the
unpinned MPI launcher does not receive participant resources, host devices, or
extra mounts.

```yaml
workloadPolicy:
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

A `CertificationProfile` and `CertificationRun` each select one immutable
script key and one backend. `purpose: FullCertification` and `stagePlan`
describe evidence; they do not execute a multi-key suite. Operators that define
a full suite must create one run per script and admit the next run only after
the previous run is terminal and its backend cleanup has completed. The
upstream controller does not synthesize a cross-run verdict.

## Results and evidence

Workflow infrastructure state and certification verdicts are separate:

```text
executionState=Completed, verdict=Pass|Fail, reason=<diagnostic reason>
executionState=Error,     verdict=Inconclusive, reason=<workflow reason>
```

Ambiguous or incomplete workflow execution remains `Inconclusive`. The
controller does not infer a successful result from partial evidence.

Cluster-local evidence consists of CertificationRun status, the referenced
Workflow and backend status, Events, pod logs, termination messages, and the
SHA-256 digest of the controller's terminal evidence document.

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

Before removing the controller, disable recurring policies and wait for active
runs to become terminal. Argo Workflows and any workload backend may be shared
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
