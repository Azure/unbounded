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

This directory includes an educational one-GPU identity example and a bounded,
non-destructive full-node GPU workflow. The full-node workflow snapshots Node
GPU capacity and allocatable inventory at admission, fails before requesting
the full GPU set when inventory is incomplete, then validates every allocated
GPU and MPI rank-to-GPU access. It is not a substitute for an
organization-reviewed burn-in or performance qualification suite.

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
 CertificationRun -> Argo Workflow -> node group -> individual tests / MPIJob
              |
              v
 typed execution/verdict/reason -> Node projection -> cleanup
```

The mutable source ConfigMap provides a fast review and hotfix surface.
Admission copies the selected key to a run-owned immutable ConfigMap and pins
its digest in the `CertificationRun`. A source update affects only later runs.
Controller code never contains certification script bodies.

## External payload inputs

Pipeline packaging can supply a release-named source ConfigMap and reviewed
runner image through the existing `CertificationProfile` fields:
`spec.script.name`, `spec.script.key`, `spec.runnerImage`, `spec.workflow.name`,
and `spec.parameters`. The source ConfigMap must be readable in the controller's
`--script-namespace`; the Run snapshot lives in the Run namespace. Private
checkout and authentication belong in the pipeline, never in runtime jobs.

Admission copies only the selected decoded text `data[key]`, including its
newline bytes, plus its computed SHA-256. Other keys, `binaryData`, source labels,
annotations, and SCM metadata are not copied. Keep the selected script
self-contained or use reviewed dependencies already installed in the image.
An image reference string is frozen but tags are not resolved; use digest-pinned
images when packaging releases. WorkflowTemplate names are referenced, not
content-snapshotted or hashed. Strict test IDs remain coupled to the recognized
built-in workflow names.

For new identity/full-node payload releases, set
`spec.parameters.mpi-payload-mode: script`. This existing parameters-map hook
moves the executable MPI launcher/worker boundary into the selected script:

| Caller | Selected-key invocation | Result contract |
| --- | --- | --- |
| Identity or full-node stage | `/bin/bash /scripts/<key>` with the existing stage environment | Existing `/results/verdict`, `/results/reason`, `/results/measurements.txt`. |
| MPIJob Launcher | `/bin/bash /scripts/<key> mpi-launcher <gpu-count>` | Preserve the existing MPI command and propagate its exit status. |
| MPIJob Worker | `/bin/bash /scripts/<key> mpi-worker` | Perform the existing SSH initialization and exec the worker service. |
| Full-node MPI rank, invoked by the payload's launcher | `/bin/bash /scripts/<key> mpi-rank` | Preserve the existing local-rank GPU query and propagate its exit status. |

Both MPI pods mount the same immutable Run-owned ConfigMap read-only at
`/scripts`. MPI-role dispatch must precede baseline script initialization and
result-file writes. To retain exact baseline keys, packaging may add separate
self-contained `gpu-identity-mpi.sh` and `gpu-full-node-mpi.sh` keys with a
dispatcher followed by the unchanged baseline body; selecting a helper key does
not implicitly copy the baseline. The identity launcher still runs the existing
one-rank `/bin/true` check. MPI resource-status measurements, device inventory,
node/attempt/script provenance, resource allocation, and foreground cleanup are
unchanged; MPI roles do not introduce new result files or test IDs.

Absent or empty mode retains `legacy-inline`; unsupported values fail closed.
Already-admitted empty-test-plan Runs always retain inline MPI compatibility,
even if an opaque legacy parameter happens to use the new name. Their selected
script still receives no new arguments. The mixed-burn component interface is
unchanged and does not support `mpi-payload-mode: script`.

## Per-test execution

For newly admitted built-in runs, the included templates expose real test
invocations rather than treating a successful suite container as a certification
pass:

| Node workflow | Individually tracked tests |
| --- | --- |
| `project-signal-gpu-certification` | GPU identity and MPI backend readiness. |
| `project-signal-gpu-full-node-certification` | Inventory preflight, allocated GPU health, MPI rank-to-GPU access, and final health. |
| `project-signal-gpu-burn-canary` | GPU Burn with continuous safety observation, CPU stress, memory integrity, and ephemeral-storage integrity. |

Each invocation carries the target Node name and UID, test identity, effective
parameters, script digest, attempt, execution state, typed verdict and reason,
and retained measurements. GPU inventory evidence includes UUIDs and PCI
addresses. MPI evidence retains the observed target-device inventory and
backend completion status. These are the configured workloads in this
repository; no additional precision variants or performance floors are implied.

Full-node GPU-heavy tasks remain ordered. A failed preflight skips full-node
allocation, and final health still runs after workload failure. Each MPI task
has a foreground cleanup hook so its workers release their GPU allocation
before the node group completes. Workflow cancellation retains the existing
foreground deletion barrier before reservations are released.

Mixed burn uses an Argo `containerSet`: `main` owns the GPU allocation and runs
GPU Burn plus the shared safety observer; `cpu-stress`, `memory-integrity`, and
`storage-integrity` are distinct concurrent Argo container nodes. They share a
preflight/ready/stop protocol and the original configured burn duration. Each
test writes its own verdict, reason, and measurements, including completed
host work and per-device GPU computation coverage. A zero exit without the
required measurements is not a pass. A non-main failure fails the group even
if GPU Burn succeeds. Safety failures or cancellation stop the whole coupled
workload; interrupted tests remain inconclusive rather than reporting a pass.
The four workload containers retain one GPU allocation and the existing total
request/limit budget of eight CPUs, 16 GiB of memory, and 8 GiB of ephemeral
storage. ContainerSet requires per-container budgets: GPU/safety receives two
CPUs and 8 GiB, CPU stress receives four CPUs and 1 GiB, memory integrity
receives one CPU and 4 GiB, and storage integrity receives one CPU and 3 GiB.
The configured worker counts and data sizes are unchanged; the new per-container
quota distribution still requires validation on the target hardware.

Task execution status and diagnostic verdict are deliberately separate. A task
may execute successfully and report hardware `Fail`. Missing required tests,
missing measurements, malformed provenance, skipped coverage, and execution
errors must not produce a certification `Pass`. Per-test outcomes are retained
with the run's immutable evidence instead of depending on pod-log retention.

## Prerequisites

- Kubernetes with the `Node` and `Pod` APIs.
- Argo Workflows `argoproj.io/v1alpha1` Workflow API and a controller that
  watches the Project Signal namespace.
- Kubeflow MPI Operator `kubeflow.org/v2beta1` MPIJob API for the included
  WorkflowTemplate.
- Readable Unbounded `Machine` and `MachineOperation` APIs for grouped admission
  safety checks, in addition to Pod and Node reads. Unavailable safety state
  does not permit a group to run.
- Controller and dashboard images built from `cmd/project-signal-controller`
  and `cmd/project-signal-dashboard`.
- A reviewed runner image containing Bash, `nvidia-smi`, Open MPI, and the
  MPI Operator's SSH requirements. `images/project-signal-runner/Containerfile`
  provides a baseline CUDA runner; publish it by immutable digest and validate
  it against the target cluster's driver and security policy before use.
- Exact, verified Node selector values and an explicitly approved Node list.

Project Signal does not install Argo Workflows or MPI Operator. Install and
scope those dependencies according to the target cluster's security model.
The portable node templates retain single-Node placement. The
`project-signal-rack-certification` composition template passes each
participant's snapshotted identity and inventory to those same node templates;
a comma-separated participant list is never used as a pod hostname.
Grouped profiles declare exact `participants.nodeNames` and a `topologyKey`;
the controller validates the same nonempty label value on every declared
member. The label validates membership, but never expands the approved set.
Existing single-Node profiles with `participants.count: 1` do not need either
new field. Do not bypass controller-managed admission or reservations by
submitting participant lists directly to Argo.

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
New built-in runs also snapshot `spec.participantPlan` and `spec.testPlan`.
Their execution plan shows each Node/test/attempt and its typed verdict,
parameters, and per-attempt evidence reference, using the same result observer
as the controller. Retained results remain visible after the Workflow and
task pods expire. Legacy runs without a test plan retain the stage-log view.
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

## Explicit rack groups

`samples/rack-explicit-nodes.yaml` is a disabled, placeholder-only example. It
uses the existing full-node tests on two explicitly approved Nodes:

```yaml
participants:
  count: 2
  nodeNames:
    - REPLACE_WITH_APPROVED_RACK_NODE_A
    - REPLACE_WITH_APPROVED_RACK_NODE_B
  topologyKey: REPLACE_WITH_VERIFIED_RACK_LABEL
workflow:
  name: project-signal-rack-certification
parameters:
  node-workflow-template: project-signal-gpu-full-node-certification
  rack-tests: "[]"
```

Replace every placeholder and verify each member before requesting the group.
`count` must match the number of distinct declared names, up to 128. Every Node
must exist, match the profile's selector and admission guards, and carry the
same nonempty value for `topologyKey`. A request from a Node outside that list
cannot expand or trigger the group. The admitted run freezes member UIDs,
per-member GPU inventory, and topology so a recreated Node or changed rack
label cannot silently stand in for the approved member on a later attempt.
Profiles with `count > 1` must use `project-signal-rack-certification`, not a
native single-Node entrypoint.

Apply the existing manual trigger to any declared member to request the entire
set. Duplicate member requests reuse the active exact-set Run, not one Run per
member. Matching requests are cleared during group cleanup; unrelated requests
and competing reservations are not removed.
Manual groups require current workload and maintenance idle safety, but do
not opt into the separate recurring minimum idle window, expiry, backoff,
or concurrency policy.

Workload admission is all-or-none for the declared set: no Workflow starts
until every member is reserved and revalidated. The individual Kubernetes
writes are not a transaction. A partial acquisition failure rolls back only
reservations owned by that run, never a competing owner's reservation. Cleanup
errors remain explicit and are retried; the controller does not execute a
smaller subset as a fallback.

The durable pending Run and finalizer precede reservation writes.
`spec.admission` freezes the exact participant policy, shared topology value,
trigger, and applicable recurring guards; `spec.participants` pins expected
Node UIDs. `status.admissionReady` records whole-set reservation readiness.
It is historical, not a permanent guarantee that every reservation is still
held, and is never a hardware certification verdict.
Every attempt revalidates the frozen membership rather than trusting a later
profile edit. Waiting for admission consumes the existing Run deadline instead
of resetting the time budget.

The rack Workflow fans out the same node templates with `failFast: false`,
preserving individual results on every member. Its root verdict checks the
expected number of node-group outcomes; missing results are inconclusive and
any typed failure prevents a pass. The controller additionally validates every
required node/test/attempt and its provenance before accepting the group.
Cancellation applies to the whole Workflow, and reservations remain protected
by the existing foreground Workflow/MPI cleanup barriers.

**The declared list defines the tested scope.** A shared label does not prove
that the list contains every physical rack member. Grouping these existing
node tests does not certify inter-node fabric or a multi-node collective.
`rack-tests` has no supported nonempty execution inventory in this baseline;
requesting one fails explicitly instead of running a successful placeholder.

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

For an explicit rack group, `recurring.nodeNames` must equal
`participants.nodeNames`, and every member must satisfy the due, idle, expiry,
backoff, eligibility, and concurrency guards before a single grouped Run is
admitted. Profile concurrency counts Runs; failure-domain usage charges each
member, so the budget must accommodate the entire declared set in each domain.
The disabled rack sample's values are not permission to increase those budgets
or start recurring work without review. Recurring status records Node UIDs;
a recreated Node cannot inherit another UID's idle window or successful result.

## Typed results

Workflow infrastructure health and hardware results are separate:

```text
executionState=Completed, verdict=Pass|Fail, reason=<diagnostic reason>
executionState=Error,     verdict=Inconclusive, reason=<workflow reason>
```

Those execution states describe the run-level outcome. Individual test results
retain their actual Argo task/container phase, such as `Running` or `Succeeded`;
an Argo `Succeeded` phase does not replace the diagnostic verdict.

A diagnostic script writes `/results/verdict` and `/results/reason`, then exits
successfully so a hardware `Fail` remains a typed result rather than an
orchestration failure.

Built-in runs use `spec.testPlan` as the required coverage contract.
`status.testResults` retains distinct `(attempt, nodeUID, id)` results,
including actual Argo task/container identity, effective parameters, script
digest, measurements, and a per-attempt evidence reference.
`status.attempts` retains the aggregate outcome and evidence for every completed
or interrupted attempt; `status.evidence` points to the final summary.
The workflow arguments `participant-plan` and `test-plan` cannot be overridden
by profile parameters. Newly admitted built-in runs always freeze their required
test plan. Existing runs without one keep an empty workflow test plan and use
the compatibility branch, including on retry: an old immutable burn snapshot
executes once, without new component arguments or required output files.
Their original script, image, placement, deadlines, reservations, and cleanup
remain in force; per-test coverage is not retroactively claimed for an opaque
snapshot. Fresh admission uses the new test boundaries.

Both compatibility and planned entrypoints retain root `verdict` and `reason`
outputs. These aggregate required test results rather than forwarding only the
last health check. A passing final health check cannot erase an earlier failure,
and missing required execution cannot become `Pass`. Unknown custom templates
without an immutable test plan retain the legacy root-output contract.

Inline Argo node status and base64/gzip `compressedNodes` are supported, with
decoded node status bounded at 16 MiB. Malformed or oversized status is
inconclusive. Unhydrated `offloadNodeStatusVersion` is explicitly rejected as
`WorkflowNodesOffloaded`, even if partial inline nodes exist. The immutable
evidence preserves the original representation and observation error.

Cluster-local evidence includes the immutable run inputs, individual test
results and workflow-node provenance, Workflow and MPIJob status, Events, pod
logs, termination messages, and a SHA-256 digest of the controller's retained
evidence document. The example does not configure an external evidence service
or cloud identity. Export any required raw logs before the pod retention window
closes; retained test measurements are not a replacement for full raw logs.

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
rack-fabric certification. A grouped workflow composes node tests, not an
unimplemented multi-node collective. Fabric qualification still requires a
supported topology-aware backend, gang scheduling, rank mapping, and reviewed
fabric/collective tests. A requested rack test without an execution adapter is
rejected explicitly rather than represented by a successful placeholder.

## Dapple B200 full-node profile

`deploy/signal-dapple/00-b200-full-node.yaml.tmpl` defines the reviewed Dapple
contract for `Dapple_B200_8x180GB` / `NVIDIA-B200` Nodes. Render it separately
from the portable Signal manifests:

```bash
make signal-dapple-manifests
```

The profile expects eight GPUs but requires only one GPU for admission and
preflight. The controller overwrites the reserved
`observed-gpu-capacity` and `observed-gpu-allocatable` run parameters with the
Node status observed at admission. A seven-GPU Node therefore produces typed
`Fail` / `NodeGPUCapacityMismatch` evidence without attempting an unschedulable
eight-GPU pod. An eight-GPU Node proceeds through the full allocation, parallel
per-device health query, MPI rank-to-GPU check, and final health stage using the
pinned Project Signal runner image.

## Dapple B200 burn canary

`deploy/signal-dapple/01-b200-burn-canary.yaml.tmpl` defines one six-hour
diagnostic canary for the healthy eight-GPU Node and one separate seven-GPU
diagnostic for the degraded Node. Both use the immutable
`project-signal-burn-runner`, which builds GPU Burn from a pinned source commit
for Blackwell compute capability 10.0.

The canary runs GPU Burn at 50 percent device memory alongside bounded CPU,
memory-integrity, and ephemeral-storage integrity load. It samples each GPU
once per minute and fails closed on:

- new kernel Xid messages;
- uncorrectable volatile or aggregate ECC growth;
- exhaustion of the device-reported thermal margin;
- power draw above 105 percent of the device-reported enforced limit for three
  consecutive one-minute samples, rejecting sustained material excess without
  treating normal instantaneous power-cap sampling jitter as hardware failure;
- early exit or failure of any required workload.

The GPU-owning `main` container is privileged to read the host kernel ring
buffer for Xid comparison; the host stressor containers are not privileged.
The group captures preflight, periodic workload measurements, per-test results,
and health-after evidence. GPU Burn must produce computation measurements for
every assigned GPU, and logged computation warnings or dead workers fail the
test even if the executable exits zero. The workflow deadline is six hours and
30 minutes with no retries.

Burn profiles use `purpose: Diagnostic`. Full certification has higher node
projection precedence, so a successful seven-GPU diagnostic cannot replace the
degraded Node's failed/blocked eight-GPU certification.

This is one source-defined canary suite, not the design's complete four-pass
initial qualification campaign. It does not claim rack-scale NCCL, external
RDMA traffic, firmware remediation, reboot, or destructive storage endurance
coverage.

## Non-goals

The repository example does not provide:

- proprietary performance floors or fixed B200 thermal/power thresholds;
- destructive storage endurance, reboot, drain, or remediation;
- the complete four-pass initial burn-in campaign;
- rack-fabric or NVL72 collective certification;
- an external evidence store;
- a scheduler integration that consumes the eligibility labels;
- dashboard mutation controls, public ingress, or long-term history;
- automatic installation of Argo Workflows or MPI Operator.

See `designs/certification-controller.md` for the broader design context.
