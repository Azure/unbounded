# Project Signal example

Project Signal is an example Kubernetes controller for admitting approved,
idle Nodes to a hardware certification workflow. It separates admission policy
from execution:

- Project Signal selects and reserves Nodes, snapshots test inputs, creates
  runs, records typed results, and projects a compact summary onto each Node.
- Argo Workflows executes the task graph, deadlines, cancellation, retries,
  task logs, and cleanup.
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
- Controller and dashboard images built from `cmd/project-signal-controller`
  and `cmd/project-signal-dashboard`.
- A reviewed runner image containing Bash, `nvidia-smi`, Open MPI, and the
  MPI Operator's SSH requirements. `images/project-signal-runner/Containerfile`
  provides a baseline CUDA runner; publish it by immutable digest and validate
  it against the target cluster's driver and security policy before use.
- Exact, verified Node selector values and an explicitly approved Node list.

Recovery-gated Dapple certification additionally requires:

- the Project Signal recovery verifier image built from
  `images/project-signal-recovery/Containerfile`;
- an immutable trusted resolver file containing the canonical authority public
  key, ledger revision, and ledger SHA-256;
- a dedicated durable SQLite volume with WAL, full synchronous writes,
  retention lock, verified backup, and single-writer fencing;
- a separately privileged observer identity that alone may run `observe`;
- a verifier identity that can acquire SQLite write transactions only to fence
  current-latest reads and durably record signed receipt authorizations, but
  cannot invoke the privileged `observe` operation;
- a controller Secret containing only the verifier public key, rendered with
  `SIGNAL_RECOVERY_VERIFIER_SECRET=<secret-name>`;
- exactly one admitted evidence service and three distinct witnesses with an
  exact two-of-three threshold.

Profiles that set `parameters.require-recovery-gate: "true"` cannot project a
`Pass` result unless the Workflow publishes an immutable ConfigMap name in the
`recovery-receipt-configmap` output parameter. The ConfigMap must contain
`receipt.json` and the exact `bundle.json`; the receipt must be short-lived,
bound to the Run UID and Workflow UID, report terminal validated success, hash
the bundle exactly, and verify under the controller's configured verifier key.
Missing, malformed, stale, forged, or unbound evidence becomes
`Inconclusive`, never `Pass`.

The recovery regression gate consists of three separately reviewable layers:

- the immutable 78-test rev24 reference corpus;
- 18 rev25 protocol, durability, receipt, and controller integration tests;
- `coordinator-attacks.json`, which names exactly 10 reconstructed rev357
  coordinator attacks plus the witness-restart and malformed-success attacks
  requested by the upstream reviewer.

The original coordinator artifact was not present in any reachable upstream or
preservation ref. The manifest records that provenance limitation instead of
claiming byte-for-byte identity, and binds every required attack ID to an
executable regression test.

The GB200 sample enables this gate deliberately. Its generic identity script
does not create a receipt, so the sample cannot certify a Node as production
eligible until a reviewed Dapple workflow adds the verifier step and publishes
the immutable receipt ConfigMap.

### Dapple deployment acceptance

The recovery gate is not deployment acceptance. Before any live Dapple read or
mutation, reviewers must separately record all of the following:

1. the exact native runner and recovery-verifier image digests;
2. the immutable trusted authority root, ledger revision, and ledger digest;
3. one service identity and exactly three distinct witness identities and
   public keys, with an exact two-of-three threshold and no key reuse;
4. service and witness endpoints, key-custody evidence, quorum reachability,
   freshness-gossip health, persistence, retention lock, and verified backup;
5. the durable volume identity and single-writer fencing used for ACK
   consumption, witness statements, and current-latest observations;
6. the bounded baseline, assigned Dapple Node UID, runtime policy, and a fresh
   one-use transfer authority.

The observer and verifier must not share operation authority. `observe` is the
only operation allowed to advance current-latest state. Receipt-issuing
`verify` needs transaction-capable access to the same database so it can hold
an immediate transaction while comparing current-latest state, signing the
receipt, and durably recording the authorization. A verifier without receipt
arguments remains read-only. The verifier signing key must be unavailable to
the Workflow and diagnostic runner. The controller receives only its public
key.

### Bounded Dapple validation plan

After deployment acceptance is complete, the final validation phase is:

1. render the manifests with immutable image digests and the verifier public-key
   Secret, then complete server-side dry runs;
2. deploy the controller and recovery verifier without creating a
   `CertificationRun`;
3. confirm durable-state restart behavior, backup restoration, exact witness
   membership, and healthy two-of-three quorum using synthetic evidence only;
4. accept a new bounded baseline and assign exactly one Dapple Node UID;
5. issue a new challenge and one-use transfer authority bound to that Run,
   Node, contract, generation, and current ledger;
6. allow the authority observer to record the signed current-latest checkpoint;
7. execute the guarded flow with at most one near-write GET and one conditional
   full-spec PATCH, with no retry, fallback, extra call, or post-state GET;
8. publish the immutable evidence bundle and signed short-lived recovery
   receipt, then require the controller to verify it before projecting `Pass`;
9. restart the service and each witness and repeat synthetic replay and
   equivocation attacks to prove durable rejection;
10. stop after the single bounded validation and preserve honest `UNKNOWN` if
    the send or response boundary is ambiguous.

Rows 05, 07, and 08 remain evidence-dependent. Row 15 is prohibited and is not
authorized by this workflow or plan.

Project Signal does not install Argo Workflows or MPI Operator. Install and
scope those dependencies according to the target cluster's security model.
The example API and controller currently reject participant counts greater than
one because the included WorkflowTemplate has single-Node placement semantics.

## Render the example

The defaults use the reserved `example.invalid` registry and cannot pull an
image. Supply reviewed images explicitly:

```bash
make signal-manifests \
  SIGNAL_CONTROLLER_IMAGE=<controller-image@sha256:digest> \
  SIGNAL_DASHBOARD_IMAGE=<dashboard-image@sha256:digest> \
  SIGNAL_RUNNER_IMAGE=<runner-image@sha256:digest>
```

Generated output is written to `deploy/signal/rendered/` and is intentionally
gitignored.

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

## Read-only workflow dashboard

`project-signal-dashboard` is a small Go HTTP service with an embedded static
UI. It has no write API, mutation controls, external database, cloud identity,
or kubeconfig. Its ServiceAccount can read:

- CertificationRuns and approved CertificationProfiles;
- Argo Workflows and MPIJobs in the Project Signal namespace;
- pod readiness in that namespace for controller and Argo health, plus bounded
  `pods/log` reads used only to parse exact `PROJECT_SIGNAL_STAGE` records;
- individual Nodes referenced by a profile or run. The dashboard never lists
  the fleet and uses deterministic aliases in participant and Node columns.

The authenticated operator view retains Kubernetes run, Workflow, and MPIJob
object names so an operator can correlate the page with `kubectl`. Treat the
dashboard as cluster operational data; screenshots intended for broader sharing
must replace those object names with sanitized examples.

The Service is `ClusterIP` only. An authenticated Kubernetes operator accesses
it through the API server:

```bash
kubectl -n project-signal port-forward service/project-signal-dashboard 8080:8080
```

Then open `http://127.0.0.1:8080`. The page refreshes every seven seconds and
separates active/current health from retained history and classifies diagnostic
and superseded attempts explicitly. Run details show admission policy, the
immutable declared stage plan, observed script stages, finalization, typed
hardware verdicts, Argo task nodes, MPIJob conditions, cleanup status, and
plain-language policy reasons. Profiles may declare `purpose` and an ordered
`stagePlan`; both are snapshotted onto each admitted run.
Kubernetes reason and message fields are rendered as text, truncated to a safe
bound, and GPU UUIDs are redacted. Arbitrary raw pod logs and script bodies are
never returned by dashboard endpoints.

Stable JSON endpoints:

| Endpoint | Purpose |
| --- | --- |
| `GET /healthz` | Process liveness. |
| `GET /readyz` | Required Signal and Argo API readability. |
| `GET /api/summary` | Run counts, approved-node counts, component health, and reason glossary. |
| `GET /api/runs?phase=&verdict=&profile=&hardware=&view=&offset=&limit=` | Bounded run list. `view` accepts `Current`, `Historical`, `Diagnostics`, or `Superseded`; the maximum page size is 100. |
| `GET /api/runs/{name}` | Correlated admission, immutable execution plan, normalized stage events, Workflow DAG nodes, MPIJob status, typed outputs, finalization, participants, and cleanup. |
| `GET /api/nodes` | Approved-node projection only. |

Responses contain `partial` and `errors` fields when a noncritical Kubernetes
API cannot be read. The dashboard does not replace alerting, long-term history,
authentication, authorization, or raw log tooling. Use Kubernetes RBAC to
control who may port-forward the Service.

## Manual request

The Node must match the selected profile and have sufficient allocatable
resources. Request a run by applying the stable trigger label:

```bash
kubectl label node <approved-node> \
  signal.unbounded-cloud.io/certification-request=gpu-smoke-example
```

The controller removes the request after admission and uses
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

## Node metadata contract

Controller-owned, selector-safe labels:

| Key | Values |
| --- | --- |
| `signal.unbounded-cloud.io/state` | `pending`, `testing`, `passed`, `failed`, `inconclusive` |
| `signal.unbounded-cloud.io/eligibility` | `blocked`, `production` |
| `signal.unbounded-cloud.io/profile` | bounded profile identifier |

Controller-owned annotations contain high-cardinality data:

- full profile name and generation;
- last run and last successful run;
- terminal timestamps;
- script and evidence digests;
- next due time;
- recurring reason and suspension state;
- active reservation identity.

Pending, testing, failed, and inconclusive states are fail-closed with
`eligibility=blocked`. A successful run projects `passed/production`. Terminal
cleanup removes the request and reservation while retaining the latest useful
summary. Older runs cannot overwrite a newer terminal projection.

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
kubectl -n project-signal get deployment,service project-signal-dashboard
```

Inspect one run and its workflow:

```bash
kubectl -n project-signal get certificationrun <run-name> -o yaml
kubectl -n project-signal get workflow <workflow-name> -o yaml
```

Completed Workflows have a six-hour TTL. Workflow task pods have a two-hour
delayed garbage-collection policy. The example MPIJob requests a one-hour TTL.
Export any required logs before those windows close.

Dashboard troubleshooting:

```bash
kubectl -n project-signal logs deployment/project-signal-dashboard
kubectl auth can-i --as=system:serviceaccount:project-signal:project-signal-dashboard \
  list workflows.argoproj.io -n project-signal
kubectl auth can-i --as=system:serviceaccount:project-signal:project-signal-dashboard \
  get pods/log -n project-signal
kubectl auth can-i --as=system:serviceaccount:project-signal:project-signal-dashboard \
  list nodes
```

The log read should print `yes`; it is bounded in bytes, lines, and time and
only normalized stage records are returned. The last command should print `no`;
the dashboard is intentionally limited to individual `get` requests for
approved Nodes.

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

After all runs are terminal:

```bash
kubectl -n project-signal scale deployment/project-signal-dashboard --replicas=0
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
- dashboard mutation controls, public ingress, or long-term history;
- automatic installation of Argo Workflows or MPI Operator.

See `designs/certification-controller.md` for the broader design context.
