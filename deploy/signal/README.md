# Project Signal controller

Project Signal is a Kubernetes controller for admitting approved idle Nodes to
operator-supplied certification workflows. The upstream integration provides
the API, controller, CRDs, RBAC, and controller Deployment. It intentionally
does not publish certification scripts, runner images, CertificationProfiles,
or WorkflowTemplates.

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
 operator-supplied Argo WorkflowTemplate
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
- An operator-owned WorkflowTemplate implementing the interface below.
- Explicitly reviewed Node selectors, resource requests, tolerations, policy
  values, and cleanup behavior.

Backends such as Jobs, MPIJobs, or other workload APIs are operator choices.
Install their controllers and grant their runner permissions separately.
Project Signal does not install them or grant generic workload-runner RBAC.

## WorkflowTemplate interface

The referenced WorkflowTemplate must accept these string parameters:

| Parameter | Meaning |
| --- | --- |
| `run-name` | CertificationRun name |
| `run-uid` | CertificationRun UID |
| `participants` | Comma-separated admitted Node names |
| `script-configmap` | Immutable run-owned ConfigMap |
| `script-key` | Selected ConfigMap key |
| `script-digest` | SHA-256 of the selected script bytes |
| `backend-api-version` | Backend API version from the profile |
| `backend-kind` | Backend kind from the profile |
| `queue` | Optional backend queue |
| `runner-image` | Digest-pinned execution image |
| `tolerations` | JSON-encoded toleration list |
| `script-environment` | JSON-encoded, validated environment map |
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

Unexpected, duplicate, malformed, or oversized outputs are rejected and the
run remains fail-closed.

## Render and install

Render the controller manifests:

```bash
make signal-manifests \
  SIGNAL_CONTROLLER_IMAGE=<controller-image@sha256:digest>
```

Generated output is written to `deploy/signal/rendered/`. Validate it with the
target cluster before applying:

```bash
kubectl apply --dry-run=server -f deploy/signal/rendered/crd/
kubectl apply --dry-run=server -f deploy/signal/rendered/
kubectl apply -f deploy/signal/rendered/crd/
kubectl apply -f deploy/signal/rendered/
```

The upstream manifests install no profiles. Operators must create a
CertificationProfile that references their own script ConfigMap,
WorkflowTemplate, backend, and digest-pinned runner image.

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
