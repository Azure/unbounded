# Project Signal example

Project Signal is an example Kubernetes controller for approved Node
certification workflows. Manual requests are operator-directed; recurring
admission requires explicitly opted-in, continuously idle Nodes. It separates
admission policy from execution:

- Project Signal selects and reserves Nodes, snapshots test inputs, creates
  runs, records typed results, and projects a compact summary onto each Node.
- Argo Workflows executes each attempt's task graph, deadlines, task logs, and
  owned backend resources. Project Signal bounds retries and waits for
  cancellation cleanup before releasing Nodes.
- An execution backend performs coordinated work. The included example uses
  Kubeflow MPI Operator `MPIJob`.

This directory is an educational example, not a production hardware
qualification suite. The included script verifies only an expected GPU product
and count. Replace it with an organization-reviewed diagnostic before using the
controller to make workload placement decisions.

## Architecture

```text
approved Node or recurring policy
              |
              v
      CertificationProfile
              |
              v
 reserve Nodes and copy the selected ConfigMap key
              |
              v
 immutable per-run ConfigMap + SHA-256 digest
              |
              v
 CertificationRun -> Argo Workflow -> node task -> MPIJob
              |
              v
 typed execution/verdict/reason -> Node projection -> cleanup
```

The mutable source ConfigMap provides a fast review and hotfix surface.
Admission copies the selected key to a run-owned immutable ConfigMap and pins
its digest in the `CertificationRun`. A source update affects only later runs.
Controller code never contains certification script bodies.

## Prerequisites

- Kubernetes with the `Node` and `Pod` APIs.
- Argo Workflows `argoproj.io/v1alpha1` Workflow API and a controller that
  watches the Project Signal namespace.
- Kubeflow MPI Operator `kubeflow.org/v2beta1` MPIJob API for the included
  WorkflowTemplate.
- A controller image built from `cmd/project-signal-controller`.
- A reviewed runner image containing Bash, `nvidia-smi`, jq, Python 3, Open MPI,
  and the MPI Operator's SSH requirements.
  `images/project-signal-runner/Containerfile` provides a digest-pinned CUDA
  baseline. Publish your runner by immutable digest and validate it against
  the target cluster's driver and security policy before use.
- Exact, verified Node selector values and an explicitly approved Node list.

Recurring admission also requires readable Unbounded `Machine` and
`MachineOperation` APIs. Unavailable workload or maintenance state blocks
automatic admission.

Project Signal does not install Argo Workflows or MPI Operator. Install and
scope those dependencies according to the target cluster's security model.
The example API and controller currently reject participant counts greater than
one because the included WorkflowTemplate has single-Node placement semantics.
The inline MPIJob uses one GPU worker and runs `/bin/true` through Open MPI to
verify SSH/bootstrap compatibility; it is not a collective or performance test.
Workers copy the operator-projected authorized keys into a private `0600` file
and generate host keys at startup. Launchers use a pod-local known-hosts file
with `StrictHostKeyChecking=accept-new`, not disabled host-key verification.

## Render the example

The defaults use the reserved `example.invalid` registry and cannot pull an
image. Supply reviewed images explicitly:

```bash
make signal-manifests \
  SIGNAL_CONTROLLER_IMAGE=<controller-image@sha256:digest> \
  SIGNAL_RUNNER_IMAGE=<runner-image@sha256:digest>
```

Generated output is written to `deploy/signal/rendered/` and is intentionally
gitignored. Override `SIGNAL_NAMESPACE` to change the controller and runner
namespace together. `make project-signal-controller-build` builds the controller
without lint or tests; `make project-signal-controller` includes the repository's
test gate.

Before applying anything, edit or overlay the example profile values:

- `REPLACE_WITH_VERIFIED_INSTANCE_TYPE`
- `REPLACE_WITH_VERIFIED_GPU_PRODUCT_LABEL`
- `REPLACE_WITH_VERIFIED_NVIDIA_SMI_PRODUCT`
- `REPLACE_WITH_VERIFIED_GPU_COUNT`
- `REPLACE_WITH_APPROVED_NODE`

Recurring admission is disabled and its expiry is in the past. Enabling it
without setting a future expiry fails closed.

Validate the rendered files with the tools available for the target cluster:

```bash
kubectl apply --dry-run=server -f deploy/signal/rendered/crd/
kubectl apply --dry-run=server -f deploy/signal/rendered/
```

Then apply the APIs and resources:

```bash
kubectl apply -f deploy/signal/rendered/crd/
kubectl apply -f deploy/signal/rendered/
```

## Manual request

The Node must match the selected profile and have sufficient allocatable
resources. Request a run by applying the stable trigger label:

```bash
kubectl label node <approved-node> \
  signal.unbounded-cloud.io/certification-request=gpu-smoke-example
```

The controller removes the request during terminal cleanup and uses
`signal.unbounded-cloud.io/reserved-by` while the run is active.

## Recurring admission

`spec.recurring` is an explicit opt-in policy. When enabled, it requires:

- one or more exact `nodeNames`;
- continuously idle Nodes for at least 15 minutes;
- a positive evidence freshness interval;
- a future absolute expiry;
- bounded profile and failure-domain concurrency;
- failure backoff and a consecutive-failure circuit breaker;
- optional mutually exclusive profiles.

A Node is busy when it has a nonterminal Pod not controlled by a DaemonSet, an
active reservation, or an active Unbounded `MachineOperation`. The controller
persists idle and run history in profile status so restarts do not erase the
policy state.

## Typed results

Workflow infrastructure health and hardware results are separate:

```text
executionState=Completed, verdict=Pass|Fail, reason=<diagnostic reason>
executionState=Error,     verdict=Inconclusive, reason=<workflow reason>
```

A diagnostic script writes `/results/verdict` and `/results/reason`, then exits
successfully so a hardware `Fail` remains a typed result rather than an
orchestration failure.

Cluster-local evidence is represented by the `CertificationRun` status,
Workflow and MPIJob status, Events, pod logs, termination messages, and a
SHA-256 digest of the controller's terminal evidence document. The example does
not configure an external evidence service or cloud identity.
Interrupted runs also retain a cluster-local evidence document containing the
authoritative Workflow status and task nodes captured before cancellation or
deletion. The run records the evidence URI and digest in `status.evidence` and
its interruption timestamp in `status.finishedAt`. A Workflow read or evidence-store
error blocks cleanup, preserving the immutable input snapshot and Node
reservations until evidence retention and execution cleanup can succeed.

## Node metadata contract

Controller-owned, selector-safe labels:

| Key | Values |
| --- | --- |
| `signal.unbounded-cloud.io/state` | `pending`, `testing`, `passed`, `failed`, `inconclusive` |
| `signal.unbounded-cloud.io/eligibility` | `blocked`, `production` |
| `signal.unbounded-cloud.io/profile` | bounded profile identifier |

Controller-owned annotations contain high-cardinality data:

- full profile name and generation;
- projection purpose;
- last run and last successful run;
- terminal timestamps;
- script and evidence digests;
- next due time;
- recurring reason and suspension state;
- active reservation identity.

Pending, testing, failed, and inconclusive states are fail-closed with
`eligibility=blocked`. A successful run projects `passed/production`. Terminal
cleanup removes the request and reservation while retaining the latest useful
summary. Cancellation uses foreground Workflow deletion and does not release
the reservation or immutable script snapshot while execution can still survive.
Admission rollback likewise retains both resources while a Run survives or is
waiting for finalizers. Older runs cannot overwrite a newer terminal projection.
If evidence is written before a failed status checkpoint, reconciliation
validates and reuses the immutable artifact's Run spec, Workflow identity,
digest, and original completion time. It does not overwrite evidence with
later Workflow progress. Conflicting evidence blocks cleanup explicitly.

`spec.purpose` controls projection priority. Full certification takes precedence
over targeted verification, quick certification, preflight, and diagnostics.
An omitted purpose preserves the legacy full-certification priority. The
educational profile explicitly uses `QuickCertification` so an identity check
cannot replace an existing full-certification summary.

`spec.stagePlan` describes the aggregate Workflow's stages. Admission validates
stage IDs and display names and snapshots the plan into the Run and its evidence;
the WorkflowTemplate, not this metadata, defines execution. The included profile
declares only the GPU-identity and inline MPI-bootstrap stages it actually runs.
Per-test dispatch and per-stage result enforcement are not provided by this
aggregate runtime.

## Monitoring

```bash
kubectl -n project-signal get \
  certificationprofiles,certificationruns,workflows,mpijobs,pods

kubectl get nodes -l signal.unbounded-cloud.io/state \
  -L signal.unbounded-cloud.io/state \
  -L signal.unbounded-cloud.io/eligibility \
  -L signal.unbounded-cloud.io/profile

kubectl -n project-signal logs deployment/project-signal-controller
kubectl -n project-signal get events --sort-by=.lastTimestamp
```

Inspect one run and its workflow:

```bash
kubectl -n project-signal get certificationrun <run-name> -o yaml
kubectl -n project-signal get workflow <workflow-name> -o yaml
```

Completed Workflows have a six-hour TTL. Workflow task pods have a two-hour
delayed garbage-collection policy. The example MPIJob requests a one-hour TTL.
Export any required logs before those windows close.

## Cancellation and cleanup

Cancel an active run:

```bash
kubectl -n project-signal patch certificationrun <run-name> \
  --type=merge -p '{"spec":{"cancel":true}}'
```

Disable recurring admission before removing the controller:

```bash
kubectl patch certificationprofile <profile-name> \
  --type=merge -p '{"spec":{"recurring":{"enabled":false}}}'
```

Keep the controller running until all runs have finished finalization and their
reservations have been released, then remove the example:

```bash
kubectl delete -f deploy/signal/rendered/
kubectl delete -f deploy/signal/rendered/crd/
```

Argo Workflows and MPI Operator may be shared cluster services. Do not remove
their CRDs or controllers as part of Project Signal cleanup.

## GB200 sample

`samples/gb200-single-node-smoke.yaml` is disabled and fail-closed. It uses
explicit `REPLACE_WITH_VERIFIED_*` values, the generic example ConfigMap script,
and an invalid past expiry. Do not apply or enable it until selectors, runner
image, product string, GPU count, Node approval, and expiry are verified on the
target cluster.

The sample is one Node, one GPU, and one MPI worker. It is not GB200 NVL72 or
rack-scale certification. Rack-scale support requires a topology-aware,
multi-participant backend, complete participant reservation, gang scheduling,
rank mapping, fabric and collective tests, and per-participant evidence.

## Non-goals

The repository example does not provide:

- production burn-in scripts or proprietary thresholds;
- destructive stress, reboot, cordon, drain, or remediation;
- multi-hour qualification parity;
- rack-scale or NVL72 certification;
- an external evidence store;
- a scheduler integration that consumes the eligibility labels;
- public ingress or long-term history;
- automatic installation of Argo Workflows or MPI Operator.
