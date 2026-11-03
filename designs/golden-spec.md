# Unbounded: Machine Provider API Design Guide

**Status: Proposed design guidance, not a versioned integration contract.**

This guide describes the ideal infrastructure API for third-party clouds
integrating with Unbounded. Build toward a simple flow: allocate a machine,
install the requested OS and startup payload, observe completion, and manage
the machine safely through upgrades, failures, and maintenance.

The behavior matters more than the spelling. REST routes and JSON below are
reference choices; equivalent native APIs can be mapped by a provider-specific
adapter. Following this guide does not by itself establish a supported
integration. Supported capabilities and integration ownership must be agreed
separately.

## Responsibilities

A **machine** is a VM or physical host; an **allocation** assigns it to a
customer. The provider API manages that infrastructure. Unbounded manages
the Kubernetes worker environment and coordinates draining and Node readiness.
The cluster operator remains responsible for host OS policy, identity, network
connectivity, and workload configuration.

An adapter translates lifecycle requests and results between Unbounded and
the provider API. This guide does not assign responsibility for developing or
supporting that adapter.

The provider owns host selection, placement, and enforcement of the customer's
quota. Unbounded requests machines through Create; it does not select physical
hosts from the provider's inventory or manage their placement.

## Ideal API surface

New APIs should target the complete core surface below, with durable operation
tracking and mutation deduplication. Maintenance visibility and host health
complete the recommended operational picture.

| Capability | Reference request | Inputs and observable result |
|---|---|---|
| Inventory | `GET /v1/machines`, `GET /v1/machines/{machineId}` | List accessible machines or describe one, including state and capabilities. |
| Create | `POST /v1/machines` | Resource selection, image, startup payload, and provider options; returns an operation that identifies the allocation. |
| Delete / release | `DELETE /v1/machines/{machineId}` | Deletes a VM or releases a physical-host allocation; returns an operation. |
| Reimage | `POST /v1/machines/{machineId}/reimage` | Target image and fresh startup payload; returns an operation. |
| Power control | `POST /v1/machines/{machineId}/power-on`, `/power-off` | Changes power state without releasing the allocation; returns an operation. |
| Operation tracking | `GET /v1/operations/{operationId}` | Returns progress, terminal outcome, resulting machine identity, and failure details. |
| Image discovery | `GET /v1/images` or equivalent discovery mechanism | For provider image IDs, exposes available images, versions, architectures, and startup compatibility. |
| Maintenance and health | Fields on machine Get/List | Reports disruptions, required preparation, outcomes, and provider-observed health. |

Existing-host integrations can support a subset without create/delete. The
minimum useful subset is inventory, operation tracking, and at least one
lifecycle action; it is not the ideal target. Advertise supported actions per
machine and provider-wide capabilities separately. Distinguish unsupported
actions from temporarily unavailable ones.

Optional extensions include native reboot, maintenance readiness, rescheduling,
repair requests, and operation cancellation. The
[maintenance reference](machine-provider-maintenance.md) defines dependencies
and detailed event behavior. Resize, network provisioning, data disks,
diagnostics, and console access are outside this guide.

## Essential guarantees

Here, **must** identifies an essential guarantee of the proposed target;
**should** identifies a recommendation. These words do not claim that existing
integrations already meet the target.

### Identity and inventory

The machine ID must identify an allocation, not a reusable physical asset.
Do not reuse it for a later customer's allocation. Get and List should expose
the same machine representation: allocation state, architecture, CPU, memory,
accelerators, location, network attachments and addresses, observed power,
provider-recorded image, and capabilities.

Unknown observations must remain unknown. A recorded image is not proof of the
running OS version. Use `maintenance: []` for no current or retained events,
and `null` when maintenance information is unavailable. Provider-observed
health is separate: `healthy`, `degraded`, `unhealthy`, or `unknown`, with a
timestamp and reason codes.

List should support pagination. The reference response is
`{"items": [], "nextPageToken": "..."}`; the caller supplies an opaque
`pageToken` on the next request. Document filters and consistency behavior.

### Certification before customer use

The provider is responsible for supplying hosts certified under the
integration's agreed qualification process before customer use. It can allocate
pre-certified hosts or run an agreed certification suite before releasing hosts
for customer use. Provider-owned placement includes selecting eligible hosts;
Unbounded does not take over physical-host placement to perform certification.

Agree who supplies and executes the tooling, the required evidence and suite
version, evidence validity, and when recertification is needed. This guide does
not prescribe a certification API or suite. Certification is separate from
infrastructure completion, startup execution, and Kubernetes Node readiness.

### Image discovery

Integrations accepting provider image IDs must document how callers discover
valid IDs. The reference `GET /v1/images` returns `items` containing image IDs,
immutable versions, OS versions, architectures, and compatible startup formats.
An equivalent native discovery mechanism is acceptable. Document catalog scope
and pagination where applicable.

Create/reimage may also accept supported image URLs outside the catalog, subject
to validation. URL-only integrations do not need a provider image catalog.

### Provisioning and reimage

Create should combine allocation, OS provisioning, and startup configuration
in one operation. Accept an offering or resource requirements, plus applicable
operator-supplied options such as network placement. Return the resources
actually allocated; a universal SKU or network schema is not required.

The allocation flow is:

1. Unbounded submits Create with resource selection, image, and startup payload.
2. The provider returns a durable operation and handles quota checks, eligible
   host selection, allocation, and provisioning.
3. The caller persists the accepted operation URL and polls for completion and
   the resulting machine identity.
4. Unbounded verifies agent registration and Kubernetes Node readiness before
   admitting workloads.

Create and reimage must accept a supported image reference and startup payload
as a compatible pair. Image references can be provider IDs or supported URLs;
downloaded images should be verified against a supplied checksum. Startup can
use cloud-init or an equivalent mechanism.

Model the reference `startup` object as a `oneOf`: exactly one supported variant,
such as `cloudInit` with its `content`. Reject missing, unsupported, or multiple
variants with `400` before accepting work. Equivalent native startup schemas may
be mapped by the adapter.

Each provisioning cycle must use the payload supplied for that request, not
cached configuration from an earlier cycle. Configure execution after OS
installation; an OS disk swap without fresh startup delivery is insufficient.
Document formats, size limits, execution timing, and whether payloads run again
on ordinary reboot. Do not imply exactly-once script execution.

Reimage should preserve identity and allocation; on bare metal it reinstalls
the existing host. If VM recreation is necessary, report both original and
resulting machine IDs. An adapter may compose create/delete where native
reimage cannot satisfy these guarantees, with explicit identity handoff and
cleanup.

The provider must document each action's completion condition, including
deletion/release and graceful or forced reboot/power-off behavior. Keep three
outcomes distinct:

1. **Infrastructure complete:** provider work finished and startup is configured.
2. **Bootstrap complete:** startup execution succeeded, if separately observable;
   otherwise unknown.
3. **Node ready:** Unbounded verifies agent registration and Kubernetes
   readiness.

A running VM alone does not prove that a requested reboot completed.

### Power control and optional reboot

Native `POST /v1/machines/{machineId}/reboot` is optional and returns an operation
when supported. An adapter may instead compose power-off followed by power-on
when both preserve the allocation and satisfy the integration's documented
reboot semantics.

Persist workflow intent and a distinct retry key for each step before submitting
it. Wait for power-off to succeed, then durably record the transition to power-on
before submitting power-on. Persist each accepted operation URL and recover
through GET; if acceptance is unknown, apply the same-key retry rules below.
Once the workflow advances to power-on, recovery must not repeat power-off.

The adapter must retain per-machine workflow ownership across both steps and
recovery, coordinating other writers so they cannot interleave lifecycle work
between the steps. Report failures and known partial effects, including a machine
left powered off; do not silently restart the sequence.

### Operations and safe retries

Providers must serialize lifecycle operations per machine: at most one may be
`pending` or `running`. Reads remain available. Maintenance event reporting and
handoff actions follow the separate concurrency rules in the
[maintenance reference](machine-provider-maintenance.md); recording an event is
not permission to begin disruptive work.

Every accepted lifecycle mutation must have a durable operation resource.
The reference response is `202 Accepted` with its URL in `Location`.
Operations expose kind, target when assigned, state, and result or error.
States are `pending`, `running`, `succeeded`, `failed`, and `canceled`;
terminal states must not change. Report known partial effects on failure or
cancellation and identify created or replacement machines in results.

Every mutation, including optional maintenance actions, must support
deduplication through a caller-supplied token, shown as `Idempotency-Key`:

- The same key and request recover the same operation or maintenance action
  result, without repeating work, even when concurrent retries arrive.
- Reusing a key with different inputs returns `409 Conflict`.
- A different lifecycle request while another lifecycle operation is active,
  or conflicting with in-progress maintenance, returns `409` and the active
  operation or maintenance event reference; it is not queued.

Deduplicate accepted retries before checking current-state conflicts.
Serialization does not replace deduplication: a lost-response retry can arrive
after the original operation has completed. Keep operation IDs distinct from
machine IDs so later operations do not overwrite earlier outcomes, and Create
can be tracked before a machine ID is assigned.

Document token scope and retention. Keep accepted-operation keys and records
through active work and a documented recovery window after termination;
operation records must remain readable for at least as long as their keys.
Deleting a machine must not shorten that window. Maintenance action results
likewise remain recoverable for their advertised retry window. A common
minimum duration remains an integration decision.

After persisting an operation URL, the caller resumes with **GET, not another
mutation**. If acceptance is unknown after a lost response, it may resubmit the
same request and key only within the guaranteed deduplication window. A
correlation ID is not a retry token. For existing APIs without deduplication,
or after expiry, do not blindly resubmit: the integration must reconcile
acceptance or report an unresolved outcome.

### Maintenance handoff

Maintenance must be visible through the provider API, not only inside the
guest. Polling is authoritative; optional push notifications use the same
event IDs. Events identify the allocation, stable event ID, status, reason,
impact, requested preparation, and update time. Distinguish planned work from
emergencies that may already be in progress.

Unbounded drains when necessary, then follows the event's `proceedSignal`:

- `release`: complete deletion/release of the allocation; acceptance of that
  operation alone is not handoff.
- `ready`: acknowledge the specific event while retaining the allocation.
- `none`: no caller acknowledgment is required.

Never request a handoff unsupported by the integration. Planned provider
events requiring handoff must give a preparation deadline and explicitly state
whether they may proceed after it without acknowledgment. Scheduling estimates
are not permission to disrupt. Changed schedules must invalidate stale
readiness, as detailed in the [reference](machine-provider-maintenance.md).

Retain explicit terminal outcomes, including replacement identity when known.
Disappearance is not proof of success; Unbounded checks health and
readiness before restoring workloads.

### Authentication and errors

Use HTTPS and noninteractive service authentication; prefer short-lived
credentials or workload identity. Native OAuth, signed requests, or mutual
TLS are acceptable. Authorize account, resource, and action consistently
across machines, operations, and maintenance.

Provider credentials are separate from host enrollment credentials. Startup
payloads may contain secrets and must not be echoed in inventory, operation
responses, or request logs.

Return machine-readable error codes and explanatory messages. Reference HTTP
statuses are `400` for invalid/unsupported requests, `401` for invalid
credentials, `403` for insufficient permission, `404` for missing/expired
resources, `409` for conflicts, and `429` for throttling. Document polling
limits and honor `Retry-After` when supplied. Transport errors and `5xx` can
leave mutation acceptance unknown; apply the retry rules above. Reading a
failed operation still returns `200 OK`; its state and error describe the
failed work.

## Example: reimage through completion

This illustrative exchange uses placeholder IDs and payload text. Requests
also carry the provider's authentication. Create accepts the same `image` and
`startup` objects, plus resource selection and provider options.

```http
POST /v1/machines/machine-123/reimage
Content-Type: application/json
Idempotency-Key: reimage-request-456

{
  "image": {"id": "image-123"},
  "startup": {
    "cloudInit": {
      "content": "<fresh Unbounded installation payload>"
    }
  }
}
```

Acceptance identifies the operation, not successful completion:

```http
HTTP/1.1 202 Accepted
Location: /v1/operations/op-123
Content-Type: application/json

{
  "id": "op-123",
  "kind": "reimage",
  "targetMachineId": "machine-123",
  "state": "running"
}
```

Persist that URL and poll `GET /v1/operations/op-123`. The terminal response
below is `200 OK`; a replacement would have a different `result.machineId`
while retaining the original `targetMachineId`.

```json
{
  "id": "op-123",
  "kind": "reimage",
  "targetMachineId": "machine-123",
  "state": "succeeded",
  "result": {"machineId": "machine-123"}
}
```

Unbounded then verifies agent registration and Kubernetes Node readiness;
provider operation success alone proves neither.

## Provider integration checklist

Publish these details with the API:

- Supported lifecycle actions, image/startup combinations, capability discovery,
  image discovery, and any limitations of an existing API relative to the target.
- Authentication setup, permissions, and operator-supplied identity/network inputs.
- Provider-owned placement and quota enforcement; certification responsibilities,
  tooling, evidence requirements, and recertification policy.
- Allocation identity rules, inventory pagination, and per-action completion
  conditions, including replacement and cleanup behavior.
- Lifecycle serialization, retry-token scope, retention, recovery after unknown
  acceptance, polling limits, throttling, and error codes.
- Native reboot support or durable power-off/power-on composition, including
  allocation preservation, workflow ownership, and partial-failure recovery.
- Startup encoding, size limits, execution/re-execution rules, and protection
  of sensitive payloads.
- Maintenance visibility, supported handoffs, deadlines, terminal outcomes,
  and host health reporting.

**Provider-defined choices:** native schemas, authentication mechanisms, SKU
and placement inputs, startup encoding, and pagination/filter syntax need not
be standardized; their behavior must be documented.

**Before a versioned contract:** agree on exact interoperable schemas,
capability encoding, and minimum recovery windows. The examples here are not
an OpenAPI definition or a conformance certification.

For optional event APIs, see the
[maintenance and repair reference](machine-provider-maintenance.md).
[Cloud mappings and research](machine-provider-api-research.md) explain the
design background; they add no requirements.
