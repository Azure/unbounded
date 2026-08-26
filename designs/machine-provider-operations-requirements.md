# Unbounded Machine Provider Operations Reference

| Field | Value |
|---|---|
| Status | Draft reference specification |
| Audience | Infrastructure providers and controller implementers integrating machines with Unbounded |
| Scope | Host lifecycle, out-of-band health, diagnostics, serial console, and frontend DPU observability |
| Transport | Transport-neutral; existing REST, gRPC, SDK, Redfish, database, or proprietary interfaces may be used |
| Implementation reference | Upstream `origin/main` at `b4a8a7df` (reviewed 2026-08-06) |

## 1. Purpose

This document describes provider capabilities that enable Unbounded to operate
and diagnose machines without physical access. It is a reference for the
desired Unbounded-facing behavior, not a requirement that a provider implement
a new API or support every operation described here.

An integration may consist of an Unbounded controller, an adapter, existing
provider APIs, and provider-operated telemetry services. The combined
integration is what matters. A controller may translate Unbounded operations to
existing provider RPCs, SDK calls, Redfish actions, HMC commands, or internal
workflows. Method names and JSON messages in this document are illustrative.

A provider may support any subset of capabilities. For example, an integration
that supports only `HostReboot` is useful and valid. Supporting one operation
does not imply support for power control, replacement, diagnostics, console, or
DPU maintenance.

### 1.1 Requirement language

This document uses requirement terms narrowly:

* **MUST** identifies the minimum safety or correctness behavior for the base
  integration, or for a capability that the integration explicitly advertises.
* **SHOULD** identifies preferred behavior that improves the Unbounded customer
  and operator experience but may not be available from an existing provider
  interface.
* **MAY** identifies an optional capability or implementation choice.

Unless a statement explicitly applies to every integration, requirements in a
capability section apply only when that capability is advertised as supported.

## 2. Unbounded lifecycle model

Unbounded distinguishes the **host** from the **node**:

* The host is the physical machine or VM and its host operating system.
* The node is the `systemd-nspawn` environment that runs kubelet, containerd,
  CNI, and customer pods on the host.
* A BMC is a baseboard management controller, commonly exposed through
  Redfish.
* An HMC is a provider hardware management controller or management console
  that may aggregate or supplement BMC state.

The provider-facing host operations currently represented by
`MachineOperation` are:

| Operation | Unbounded intent | Independently optional |
|---|---|---|
| `HostPowerOn` | Start the host and observe it powered on. | Yes |
| `HostPowerOff` | Stop the host and observe it powered off. | Yes |
| `HostReboot` | Restart the host through an out-of-band provider mechanism. | Yes |
| `HostReplace` | Recreate a VM or provision/reimage a physical host with fresh bootstrap configuration. | Yes |

`RepaveNode` is not a provider-facing `MachineOperation`. It replaces the node
root filesystem on an existing running host and is performed by the Unbounded
agent after Kubernetes Node deletion. A provider normally does not implement
it.

For physical machines, `HostReplace` uses capabilities a provider might call
`reimage`, `reinstall`, or `repave`: select a boot source, install an image,
inject bootstrap configuration, boot the host, and report the outcome.

Provider operation success does not mean the Kubernetes Node is `Ready`.
Unbounded verifies cluster enrollment and node health separately.

## 3. Integration and capability discovery

The integration **MUST** have an unambiguous way to determine whether each
operation is supported. This may be a provider capability API, static controller
configuration, or the operations registered with `machineops.NewProvider`. A
native provider capability endpoint is not required.

Unsupported operations **MUST** be reported explicitly to Unbounded rather than
accepted and left pending indefinitely. Capability declarations **MUST** be
truthful for the target machine and credentials.

A useful capability description includes:

| Capability | Example details |
|---|---|
| Host operation | Supported modes, parameters, expected duration, and postcondition. |
| Long-running task | Polling or watch mechanism, status retention, and provider task ID. |
| Cancellation | Supported operations and stages, point of no return, and residual effects. |
| Health and telemetry | Sources, freshness, expected latency, and retention. |
| Serial console | Historical, live read-only, or interactive access and retention. |
| DPU | Observability and maintenance capabilities supported independently. |

Example for a reboot-only integration:

```json
{
  "operations": {
    "HostReboot": {
      "supported": true,
      "implementation": "provider-managed-reset",
      "longRunning": true,
      "cancellation": false
    },
    "HostPowerOn": {"supported": false},
    "HostPowerOff": {"supported": false},
    "HostReplace": {"supported": false}
  },
  "healthTelemetry": false,
  "serialConsoleHistory": false,
  "frontendDPUObservability": false
}
```

## 4. Kubernetes user experience

Kubernetes custom resources are the user interface for machine operations.
Users create a `MachineOperation`; a controller resolves the target `Machine`,
selects an integration, invokes its registered provider callbacks, and writes
status back to the `MachineOperation`.

The current external provider controller supports one target through
`spec.machineRef`. It does not process `spec.machineSelector`; selector-based
host operations are handled only by other executors such as metalman where
documented.

### 4.1 Example `kubectl` flow

The following example uses workload identity. The current external provider
controller requires exactly one `MachineOperationCredential` matching the
Machine's site and provider for every registered operation.

The plugin does not currently create `MachineOperationCredential` resources or
external-provider Machines, so create these integration prerequisites with
Kubernetes YAML:

```bash
kubectl apply -f - <<'EOF'
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineOperationCredential
metadata:
  name: remote-example-cloud
spec:
  siteName: remote
  provider: ExampleCloud
  auth:
    mode: WorkloadIdentity
---
apiVersion: unbounded-cloud.io/v1alpha3
kind: Machine
metadata:
  name: worker-01
  labels:
    unbounded-cloud.io/site: remote
spec:
  host:
    external:
      provider: ExampleCloud
      providerID: example:///regions/region-1/instances/worker-01
EOF
```

```text
machineoperationcredential.unbounded-cloud.io/remote-example-cloud created
machine.unbounded-cloud.io/worker-01 created
```

The Machine identifies the provider and provider-owned host that the controller
will operate:

```bash
kubectl get machine worker-01 -o yaml
```

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: Machine
metadata:
  generation: 1
  labels:
    unbounded-cloud.io/site: remote
  name: worker-01
spec:
  host:
    external:
      provider: ExampleCloud
      providerID: example:///regions/region-1/instances/worker-01
```

Create a `HostReboot` request with the plugin's convenience command. The
explicit operation name makes subsequent status commands predictable. The
`MachineOperation` is the object users watch; they do not call the provider API
directly.

```bash
kubectl unbounded machine host-reboot worker-01 \
  --operation-name reboot-worker-01 \
  --ttl 300 \
  --wait=false
```

```text
machineoperations/reboot-worker-01 created
```

The equivalent resource-oriented plugin command is:

```bash
kubectl unbounded machine operation create reboot-worker-01 \
  --kind HostReboot \
  --machine worker-01 \
  --ttl 300
```

Use either command, not both. The convenience command additionally sets the
Machine as the operation's owner.

Immediately after creation, `status.phase` may still be empty. The controller
treats an empty phase as pending. If the operation is waiting behind an older
host operation, it explicitly records `Pending`.

```bash
kubectl get mop reboot-worker-01
```

```text
NAME               MACHINE     OPERATION    PHASE    AGE
reboot-worker-01   worker-01   HostReboot            1s
```

Watch the operation as the controller invokes the provider. Short-lived states
may be missed between watch updates.

```bash
kubectl get mop reboot-worker-01 --watch
```

```text
NAME               MACHINE     OPERATION    PHASE        AGE
reboot-worker-01   worker-01   HostReboot   InProgress   2s
reboot-worker-01   worker-01   HostReboot   Complete     45s
```

For a long-running provider registration, the `InProgress` YAML includes the
target input snapshot and durable provider task handle returned by `Begin`. The
snapshot is immutable, but it does not contain `host.external.providerID`; the
controller reads that field from the current Machine during each reconcile.

```bash
kubectl get mop reboot-worker-01 -o yaml
```

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineOperation
metadata:
  generation: 1
  name: reboot-worker-01
spec:
  machineRef: worker-01
  operationKind: HostReboot
  ttlSecondsAfterFinished: 300
status:
  phase: InProgress
  message: provider reboot task is running
  startedAt: "2026-08-06T14:00:03Z"
  targets:
  - machineRef: worker-01
    phase: InProgress
    stage: WaitingProviderOperation
    message: provider reboot task is running
    startedAt: "2026-08-06T14:00:03Z"
    observedGeneration: 1
    input: {}
    attempts: 1
    lastAttemptAt: "2026-08-06T14:00:03Z"
    providerOperation:
      provider: ExampleCloud
      operationID: provider-task-301
      resumeToken: opaque-non-secret-token
    conditions:
    - type: ProviderOperationStalled
      status: "False"
      reason: WithinExpectedDuration
      message: provider operation is within its expected duration
      observedGeneration: 1
      lastTransitionTime: "2026-08-06T14:00:03Z"
  conditions:
  - type: Completed
    status: "False"
    reason: InProgress
    message: executing HostReboot via ExampleCloud
    observedGeneration: 1
    lastTransitionTime: "2026-08-06T14:00:03Z"
```

The controller polls `provider-task-301`. In another terminal, use the plugin
to wait for the operation to become terminal:

```bash
kubectl unbounded machine operation wait reboot-worker-01 --timeout 5m
```

The wait command streams operation progress and exits nonzero if the operation
fails. After it reports success, inspect the terminal resource:

```bash
kubectl get mop reboot-worker-01 -o yaml
```

Example plugin output, with terminal styling omitted, is:

```text
  --> Target worker-01: InProgress/WaitingProviderOperation - provider reboot task is running
  --> Operation HostReboot: reboot-worker-01 in progress...
  --> Target worker-01: Complete/WaitingProviderOperation - HostReboot completed via ExampleCloud
  --> Operation HostReboot: reboot-worker-01 completed

  ready
```

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineOperation
metadata:
  generation: 1
  name: reboot-worker-01
spec:
  machineRef: worker-01
  operationKind: HostReboot
  ttlSecondsAfterFinished: 300
status:
  phase: Complete
  message: HostReboot completed via ExampleCloud
  startedAt: "2026-08-06T14:00:03Z"
  completedAt: "2026-08-06T14:00:45Z"
  observedMachineGeneration: 1
  targets:
  - machineRef: worker-01
    phase: Complete
    stage: WaitingProviderOperation
    message: HostReboot completed via ExampleCloud
    startedAt: "2026-08-06T14:00:03Z"
    completedAt: "2026-08-06T14:00:45Z"
    observedGeneration: 1
    input: {}
    attempts: 1
    lastAttemptAt: "2026-08-06T14:00:03Z"
    providerOperation:
      provider: ExampleCloud
      operationID: provider-task-301
      resumeToken: opaque-non-secret-token
    conditions:
    - type: ProviderOperationStalled
      status: "False"
      reason: WithinExpectedDuration
      message: provider operation is within its expected duration
      observedGeneration: 1
      lastTransitionTime: "2026-08-06T14:00:03Z"
  conditions:
  - type: Completed
    status: "True"
    reason: Succeeded
    message: HostReboot completed via ExampleCloud
    observedGeneration: 1
    lastTransitionTime: "2026-08-06T14:00:45Z"
```

If provider polling returns `Failed` or `Canceled`, the same flow ends with
`status.phase: Failed`. The `Completed` condition and target message contain the
stable provider reason and diagnostic text. The original operation and provider
task handle remain visible until `ttlSecondsAfterFinished` expires.

For example, a provider timeout may appear as:

```yaml
status:
  phase: Failed
  message: reboot task exceeded the provider deadline
  startedAt: "2026-08-06T14:00:03Z"
  completedAt: "2026-08-06T14:05:03Z"
  targets:
  - machineRef: worker-01
    phase: Failed
    stage: WaitingProviderOperation
    message: "ProviderDeadlineExceeded: reboot task exceeded the provider deadline"
    startedAt: "2026-08-06T14:00:03Z"
    completedAt: "2026-08-06T14:05:03Z"
    observedGeneration: 1
    providerOperation:
      provider: ExampleCloud
      operationID: provider-task-301
  conditions:
  - type: Completed
    status: "False"
    reason: ProviderDeadlineExceeded
    message: reboot task exceeded the provider deadline
    observedGeneration: 1
    lastTransitionTime: "2026-08-06T14:05:03Z"
```

### 4.2 Controller mediation

The current Go extension point is an immutable provider registration, not an
interface that provider implementations must satisfy. A provider registers only
the operations it supports. Each operation is either immediate or long-running:

```go
provider, err := machineops.NewProvider(
    "ExampleCloud",
    machineops.WithProviderMachineKind(schema.GroupKind{
        Group: "infrastructure.example.com",
        Kind:  "ProviderMachine",
    }),
    machineops.WithLongRunningOperation(
        v1alpha3.OperationHostReboot,
        beginReboot,
        pollReboot,
    ),
    machineops.WithLongRunningOperation(
        v1alpha3.OperationHostReplace,
        beginReplace,
        pollReplace,
        machineops.RequiresReplaceUserData(),
        machineops.WithCleanup(cleanupReplacement),
    ),
)
```

`WithImmediateOperation` receives one `Execute` callback.
`WithLongRunningOperation` receives idempotent `Begin` and resumable `Poll`
callbacks. Cleanup and replacement bootstrap data are optional operation
settings. `NewProvider` requires at least one registered operation, so a
provider that registers only `HostReboot` is valid.

`WithProviderMachineKind` is needed only when the provider accepts a
provider-owned resource through `Machine.spec.host.external.machineRef`. A
provider using only `host.external.providerID` does not need it.

The controller flow is:

```text
MachineOperation
  -> load Machine
  -> resolve host owner from Machine.spec.host
  -> select controller by Machine site and provider
  -> look up the registered operation strategy
  -> wait for older host operations on the same Machine
  -> resolve the matching MachineOperationCredential
  -> snapshot Machine generation and provider-specific target input
  -> mark MachineOperation InProgress
  -> immediate: Execute existing provider API
  -> long-running: Begin, persist provider operation ID, then Poll
  -> apply replacement provider identity when returned
  -> run provider cleanup
  -> mark MachineOperation Complete or Failed
```

For example, an adapter might map `HostReboot` to an existing cloud SDK restart
call, a Redfish reset action, an HMC RPC, or a provider's internal orchestration
job. The provider registration does not require that the native API use
Unbounded names or request schemas.

The current `MachineOperation` phases are `Pending`, `InProgress`, `Complete`,
and `Failed`. Immediate callbacks complete within one `Execute` invocation.
Long-running callbacks return a provider operation ID and optional non-secret
resume token, which the controller persists in
`status.targets[].providerOperation`. The controller then polls that handle
across reconciles and controller restarts.

Before invoking a provider, the controller also freezes target input in
`status.targets[]`: the observed Machine generation, resolved host image, and,
when used, the provider-owned Machine resource identity, UID, and generation.
The external `providerID` is not part of this snapshot; each reconcile uses its
current value from the Machine.

Current host identity forms are `spec.host.azure.resourceID` for the built-in
Azure provider and `spec.host.external`, which accepts either `providerID` or a
provider-owned `machineRef`. Deprecated top-level `spec.provider` and
`spec.providerID` remain readable for migration. New `spec.host` ownership
cannot be combined with those legacy ownership fields.

The controller currently recognizes provider poll states `InProgress`,
`Succeeded`, `Failed`, and `Canceled`. It does not currently expose a
cancellation request in `MachineOperation` or a provider `Cancel` callback.

## 5. Reference operation behavior

This section describes preferred inputs, outputs, and semantics. Only an
operation advertised as supported is subject to its conditional requirements.
Existing provider parameters and outputs may be mapped by the controller.

### 5.1 Common request context

An integration SHOULD preserve equivalent correlation and safety context:

```json
{
  "machineId": "machine-042",
  "requestId": "req-8f831",
  "idempotencyKey": "7d921e3c-b3ee-47c9-8207-f72f3d48b816",
  "expectedResourceVersion": "184467",
  "deadline": "2026-08-06T15:00:00Z",
  "reason": "Recover host after failed boot"
}
```

| Field | Description |
|---|---|
| `machineId` | Stable provider identifier for the target. |
| `requestId` | End-to-end correlation value for logs and audit records. |
| `idempotencyKey` | Mutation key used to avoid repeating an effect after a lost response. The controller supplies the `MachineOperation` UID to provider callbacks. |
| `expectedResourceVersion` | Optional optimistic concurrency guard. |
| `deadline` | Desired terminal deadline; it does not prove physical work stops then. |
| `reason` | Human-readable audit context. |

If an operation can create duplicate destructive effects, the integration
**MUST** make retries safe. Native provider idempotency is preferred, but the
controller may provide deduplication or provider-side operation discovery.

### 5.2 HostPowerOn

Illustrative call:

```text
PowerOnMachine(machineId, commonContext) -> OperationResult
```

If `HostPowerOn` is supported, successful completion **MUST** mean that the
integration observed the provider's powered-on state. Guest and Kubernetes
readiness are not implied. Calling the operation when already on should succeed
without repeating an effect.

```json
{
  "powerState": "ON",
  "observedAt": "2026-08-06T14:01:22Z",
  "providerTaskId": "task-101"
}
```

### 5.3 HostPowerOff

Illustrative call:

```text
PowerOffMachine(machineId, mode, gracePeriod, allowEscalation, commonContext)
  -> OperationResult
```

| Parameter | Description |
|---|---|
| `mode` | `GRACEFUL` or `FORCE`, when the provider distinguishes them. |
| `gracePeriod` | Maximum wait for graceful shutdown. |
| `allowEscalation` | Allows force-off after graceful timeout. |

If `HostPowerOff` is supported, successful completion **MUST** mean that the
integration observed the provider's powered-off state. An integration that
offers graceful mode **MUST NOT** silently escalate to force-off unless allowed.

```json
{
  "powerState": "OFF",
  "requestedMode": "GRACEFUL",
  "effectiveMode": "FORCE",
  "escalatedAfter": "PT2M",
  "observedAt": "2026-08-06T14:05:20Z"
}
```

### 5.4 HostReboot

Illustrative call:

```text
RebootMachine(machineId, mode, timeout, commonContext) -> OperationResult
```

| Parameter | Description |
|---|---|
| `mode` | Provider-supported restart mode, such as reset or power cycle. |
| `timeout` | Maximum time to wait for the provider's completion signal. |

If `HostReboot` is supported, success **MUST** mean that the provider restart
action completed according to the provider's documented postcondition. The
integration SHOULD observe a new boot attempt or an `ON` to `OFF` to `ON` power
cycle when the underlying interface exposes that evidence. Existing reset APIs
that do not expose an off state are acceptable, but the capability description
should state the weaker verification level.

```json
{
  "restartVerification": "PROVIDER_TASK_COMPLETE",
  "powerState": "ON",
  "bootAttemptId": null,
  "providerTaskId": "task-301",
  "observedAt": "2026-08-06T14:12:41Z"
}
```

### 5.5 HostReplace

`HostReplace` is optional and is expected only when VM replacement or
bare-metal provisioning is in scope.

Illustrative call:

```text
ReplaceMachine(machineId, image, bootstrap, dataDisposition, bootMethod,
               preserve, allowDisruption, commonContext) -> OperationResult
```

| Parameter | Description |
|---|---|
| `image` | Provider image or artifact identifier; an immutable digest is preferred. |
| `bootstrap` | Short-lived cloud-init, ignition, agent configuration, or equivalent first-boot data. |
| `dataDisposition` | Data classes to erase, preserve, or detach. |
| `bootMethod` | PXE, virtual media, disk image, or an existing provider mechanism. |
| `preserve` | NICs, addresses, data disks, identity, or other resources to retain. |
| `allowDisruption` | Explicit confirmation of destructive work. |

If `HostReplace` is supported, the integration **MUST** validate required inputs
before initiating destructive work and **MUST** report whether the provider's
replacement postcondition completed. The integration SHOULD:

* verify an immutable image digest when supported;
* preserve operation and provider task identity across retries;
* report stages such as validation, imaging, booting, verification, and cleanup;
* restore temporary boot policy, media, and credentials;
* expose failed provisioning and boot evidence; and
* return a replacement provider ID before deleting the old resource when the
  provider creates a new VM identity.

```json
{
  "machineId": "machine-042",
  "replacementMachineId": null,
  "installedImageDigest": "sha256:9f7b...",
  "imageVerified": true,
  "powerState": "ON",
  "bootState": "OS_RUNNING",
  "bootAttemptId": "boot-6102",
  "bootPolicyRestored": true,
  "providerTaskId": "task-801"
}
```

## 6. Long-running tasks and cancellation

Provider actions may be immediate or long-running. The upstream controller
implements both modes:

* An immediate operation runs one callback. `ReplaySafe` may be declared when
  repeating that callback after an interrupted reconcile is safe.
* A long-running operation uses `Begin` and `Poll`. `Begin` must be idempotent
  for a stable `OperationRequest.OperationUID` until its handle is persisted.
* The controller stores the provider name, operation ID, and optional non-secret
  resume token in `status.targets[].providerOperation`.
* The controller polls persisted handles after a restart. Ordinary `Begin` and
  `Poll` errors are retried; `PermanentError` terminates the operation.
* A provider operation that exceeds the expected duration remains in progress
  and receives `ProviderOperationStalled=True` in
  `status.targets[].conditions` rather than being assumed failed.

An immediate `Execute` error fails the operation. If the controller records
`InProgress` but loses the callback outcome, it invokes the callback again only
when `ReplaySafe` was registered. A non-replay-safe immediate operation can
remain `InProgress` because the controller cannot safely infer or repeat its
effect. Providers should prefer long-running registration when the native API
returns a durable task handle.

An existing provider task API can map naturally to `Begin` and `Poll` without
implementing the illustrative service methods below. Provider-native list or
watch methods remain optional:

```text
GetOperation(operationId) -> Operation
ListOperations(machineId, kind, state, pageToken) -> OperationPage
WatchOperations(machineId, afterCursor) -> stream OperationUpdate
```

These methods need not exist in the native provider API. Equivalent behavior
is implemented by the controller's persisted target status and provider
callbacks.

Recommended provider task states are:

| Provider state | Current `MachineOperation` phase |
|---|---|
| Provider task not started | `Pending` |
| `InProgress` | `InProgress` with stage `WaitingProviderOperation` |
| `Succeeded` | `Complete` |
| `Failed` | `Failed` |
| `Canceled` | `Failed`; the condition reason is the provider reason or defaults to `Canceled` |

Recommended status fields include operation ID, provider task ID, stage,
message, timestamps, attempt number, deadline, progress, result or structured
error, and correlation to boot or provisioning attempts.

### 6.1 Cancellation

Cancellation is optional and independent for every operation. An integration
may advertise `HostReboot` while declaring cancellation unsupported.

Upstream `b4a8a7df` can observe that an existing provider task reached
`Canceled`, but `MachineOperation` has no cancellation request field and the Go
registration has no `Cancel` callback. Therefore controller-initiated
cancellation described below is proposed reference behavior, not current API.

An extended integration that supports cancellation may expose an equivalent of:

```text
CancelOperation(operationId, reason) -> Operation
```

If cancellation is advertised, the integration **MUST**:

* make repeated cancellation requests safe;
* report whether cancellation was accepted or is no longer possible;
* avoid claiming rollback merely because cancellation was requested; and
* report known residual effects when cancellation completes.

The integration SHOULD declare cancellable stages and the point of no return.
Canceling a client request or watch should not implicitly cancel provider work.

```json
{
  "state": "Canceled",
  "residualEffects": {
    "powerState": "OFF",
    "imageComplete": false,
    "bootOverrideRestored": true
  },
  "recommendedAction": "Start a new reimage before booting the host"
}
```

### 6.2 Deadlines and errors

If deadlines are supported, a deadline failure SHOULD include the last stage,
last machine state, provider task ID, whether work may continue, and safe retry
guidance. A timeout is not proof that a BMC, HMC, hypervisor, or imaging task
stopped.

Errors SHOULD use stable categories such as invalid input, authentication,
authorization, unsupported operation, conflict, failed precondition,
cancellation rejected, rate limited, provider unavailable, deadline exceeded,
and internal failure. Provisioning integrations should distinguish image-write,
network-boot, hardware, and OS-boot failures where possible.

## 7. Health, BMC, and HMC observability

Health and diagnostics are strongly recommended because an Unbounded operator
may have no direct hardware access. They are not prerequisites for supporting an
otherwise useful lifecycle operation unless agreed as part of that integration.

### 7.1 Recommended minimum health service

The provider SHOULD expose a log and metric service that mirrors relevant BMC
and HMC instrumentation with documented latency. This may be a database,
shallow-buffer proxy, direct API, or combination.

Recommended state and metric classes include:

* current power, boot, aggregate health, and component health;
* BMC and HMC events, lifecycle logs, and hardware alarms;
* temperature, fans, power supplies, and power draw;
* memory ECC and CPU machine-check events;
* storage SMART, media wear, and controller status;
* NIC errors, drops, and firmware health; and
* accelerator ECC, reset, thermal, power, and fabric state.

When telemetry is exposed, the integration **MUST NOT** represent unavailable
or stale data as healthy or zero. Responses SHOULD include observation time,
source, component, unit, quality, expected freshness, and collection errors.

Illustrative methods:

```text
GetMachineHealth(machineId, componentTypes) -> MachineHealth
QueryMachineMetrics(machineId, names, startTime, endTime) -> MetricPage
QueryMachineLogs(machineId, sources, startTime, endTime,
                 operationId, bootAttemptId, cursor) -> LogPage
```

### 7.2 Preferred read-only management access

Temporary, machine-scoped, direct read-only Redfish access is preferred for BMC
logs, state, events, inventory, and metrics. Equivalent read-only HMC access is
preferred where the HMC contains additional evidence. This reduces the delay
between discovering a useful signal and adding it to a mirrored telemetry
service.

A proxied Redfish view or normalized provider API is a suitable alternative
when it preserves relevant vendor-specific evidence. Permanent BMC or HMC
administrator credentials should not be exposed.

### 7.3 Bare-metal write access

Direct BMC/HMC write access is not generally required. If `HostReplace` or
bare-metal provisioning is supported, the integration needs direct or mediated
operations for the applicable power, boot-source, virtual-media, imaging, and
cleanup actions. A provider-mediated API is preferred over sharing permanent
management credentials.

## 8. Frontend DPU observability

When a frontend DPU participates in customer traffic, the provider SHOULD expose
enough state to distinguish host, DPU, underlay, and customer-network failures.

Recommended data includes:

* DPU identity, firmware, health, uptime, and reset history;
* physical and logical interface state, speed, errors, drops, and flaps;
* route, neighbor, tunnel, and encapsulation health;
* packet drops by stable reason code;
* tunnel endpoint reachability;
* MTU, queue, buffer, congestion, checksum, and offload failures; and
* logs for configuration changes and forwarding failures.

Data should correlate with machine operations and boot attempts when frontend
networking is needed for provisioning, artifact retrieval, bootstrap, or
Kubernetes enrollment.

DPU maintenance is a separate optional capability. If delegated to Unbounded,
the integration may expose reset, firmware upgrade, or reprovision operations.
Those operations should declare traffic impact, cancellation safety, rollback
behavior, and successful forwarding postconditions. DPU BMC access is not
otherwise expected.

## 9. Serial console

Retained read-only serial or equivalent boot-console output is strongly
recommended because Unbounded may have no other access to a failed machine.

```text
ReadConsoleHistory(machineId, bootAttemptId, startCursor, maxBytes)
  -> ConsoleChunk
WatchConsole(machineId, fromCursor) -> stream ConsoleChunk
```

Console history should:

* begin early enough to capture firmware, bootloader, kernel, initramfs, and
  first-boot output where possible;
* survive boot failure and client disconnect;
* identify machine, operation, boot attempt, and capture timestamps;
* support bounded reads and cursor resume; and
* report retention, truncation, gaps, encoding, and collection errors.

Interactive console access is optional. If offered, it **MUST** be separately
authorized, machine-scoped, encrypted, short-lived, revocable, and audited.
Temporary session access is preferred over permanent BMC credentials.

## 10. Failed-boot diagnostics

When the provider performs imaging or boot orchestration, it SHOULD make
relevant failed-boot evidence available without physical access. A boot failure
should correlate evidence through an operation ID, provider task ID, or boot
attempt ID.

Recommended evidence includes:

* operation stages, attempts, and provider task history;
* observed power state, boot state, and effective boot source;
* serial-console output for the failed attempt;
* BMC and HMC events and lifecycle logs;
* BIOS/UEFI POST and boot-device discovery;
* image download, write, verification, and disk-controller logs;
* PXE, DHCP, TFTP, HTTP, virtual-media, or equivalent service logs;
* captured kernel, initramfs, cloud-init, or first-boot logs;
* storage health and relevant hardware alarms;
* frontend DPU/network evidence needed for bootstrap reachability; and
* provider orchestration logs and task identifiers.

An integration that advertises a diagnostic bundle **MUST** identify unavailable
or omitted sources rather than presenting a partial bundle as complete. Bundle
downloads **MUST** be authorized to the customer and machine and must not expose
secrets or other-tenant data.

Illustrative methods:

```text
CollectDiagnostics(machineId, operationId, bootAttemptId, classes,
                   allowDisruption) -> Operation
GetDiagnosticBundle(bundleId) -> DiagnosticBundle
```

Diagnostics should be non-disruptive by default. If a diagnostic can reset
hardware, pause traffic, or change machine state, the integration **MUST**
require explicit disruption approval and report the impact.

## 11. Security minimum

All integrations **MUST**:

* authenticate and authorize access to the correct customer and machine;
* encrypt management traffic in transit;
* avoid exposing permanent BMC, HMC, DPU, or provider administrator
  credentials through ordinary operation responses;
* prevent secrets and other-tenant data from appearing in status, logs,
  console output, and diagnostic artifacts; and
* audit privileged and destructive actions with target, principal, time, and
  result.

Separate permissions for telemetry, console, lifecycle, destructive actions,
and interactive access are recommended.
