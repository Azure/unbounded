# Machine Provider Maintenance and Repair Reference

Optional extensions to the [Unbounded API design guide](golden-spec.md).
This is proposed design guidance, not a versioned contract. Implement only the
capabilities an integration needs; when implementing one, preserve the
guarantees below. Routes and field names are reference choices.

## Capabilities and routes

All event actions require machine Get/List maintenance visibility and mutation
deduplication as described in the main guide. Events are read through their
machine, not through a separate event endpoint.

The main guide's per-machine serialization applies to lifecycle operations.
Event reporting and handoff actions use the rules below; accepting a report or
recording readiness does not authorize work to overlap a conflicting lifecycle
operation.

| Capability | Reference request | Success and dependencies |
|---|---|---|
| Readiness | `POST /v1/machines/{machineId}/maintenance/{eventId}/ready` | `200 OK`, event with readiness accepted; requires a `ready` event and current `handoffVersion`. |
| Reschedule | `POST /v1/machines/{machineId}/maintenance/{eventId}/reschedule` | `200 OK`, updated event; requires a reschedulable event and current `handoffVersion`. |
| Repair | `POST /v1/machines/{machineId}/repair` | `201 Created`, repair event; requires report cancellation and a supported handoff for deferred disruption. |
| Cancel repair report | `POST /v1/machines/{machineId}/maintenance/{eventId}/cancel` | `200 OK`, canceled event; required when repair reporting is supported. |

Advertise readiness, rescheduling, and repair capabilities per machine.
`proceedSignal: "ready"` requires readiness support; `release` requires
delete/release support. Do not emit a handoff the integration cannot perform.
Repair requests using `after_handoff` require at least one of these handoffs;
reject unsupported combinations before accepting work.

## Event representation

Include current and retained ended events in each machine's `maintenance`
array. `[]` means no events; `null` means information is unavailable. Events
must be externally readable, even if guest connectivity is lost. Optional
push notifications supplement authoritative polling and carry the same IDs.

| Field | Meaning or reference values |
|---|---|
| `id`, `machineId` | Stable event and allocation identities; never reused for another event or allocation. |
| `type`, `origin` | `maintenance` or `repair`; `provider` or `customer`. |
| `status` | `scheduled`, `in_progress`, then `completed`, `failed`, `canceled`, or `superseded`; states may be skipped but never move backward. |
| `reasonCode`, `reason` | Stable machine-readable code and human explanation. |
| `urgency`, `scope` | `planned` or `emergency`; `machine` or `group`. |
| `impacts`, `expectedImpact` | Machine-readable impacts and explanation, such as `pause`, `reboot`, `network_interruption`, `unavailable`, or `retirement`. |
| `requestedPreparation` | `none` or `drain`; Unbounded performs any Kubernetes drain. |
| `proceedSignal` | `release`, `ready`, or `none`, as defined in the main guide. |
| `readiness` | `pending` or `accepted`; `null` unless the handoff is `ready`. |
| `handoffVersion` | Opaque version identifying the current schedule and preparation agreement; used to reject stale actions. |
| `updatedAt` | Timestamp changed on every event update. |

Document stable reason codes such as `planned_update`, `gpu_failure`,
`memory_failure`, `network_failure`, and `other`. Providers may add codes
without changing published meanings. Status must not be encoded in text.

Keep estimates separate from authorization:

| Field | Meaning |
|---|---|
| `windowStartTime`, `windowEndTime` | Expected maintenance period, not a guarantee of start time. |
| `preparationDeadline` | When preparation and handoff are due. |
| `latestStartTime` | Latest reschedulable start; omitted or `null` means rescheduling is unavailable. |
| `estimatedDisruptionSeconds` | Expected disruption duration; `0` means none expected. |
| `mayProceedAfterDeadline` | Explicit boolean: whether the provider may proceed after the deadline without handoff. |

Provider-originated events awaiting handoff must specify a deadline and a
boolean proceed policy. `false` means wait for handoff even after the deadline;
`true` permits proceeding at or after it. Customer-reported repairs awaiting
handoff use `false` and may omit the deadline. Missing estimates mean unknown,
not permission to disrupt.

Emergency events may first appear in progress, with no opportunity to prepare.
Incomplete observations from an existing API must remain unknown, not be
interpreted as permission or a promise to defer. Such limitations belong in
the integration's capability documentation.

Example scheduled event:

```json
{
  "id": "maintenance-123",
  "machineId": "machine-123",
  "type": "repair",
  "origin": "provider",
  "reasonCode": "memory_failure",
  "reason": "Host requires hardware repair.",
  "scope": "machine",
  "urgency": "planned",
  "status": "scheduled",
  "impacts": ["unavailable"],
  "expectedImpact": "Host will be unavailable during repair.",
  "requestedPreparation": "drain",
  "updatedAt": "2026-10-16T12:00:00Z",
  "windowStartTime": "2026-10-17T12:00:00Z",
  "windowEndTime": "2026-10-17T14:00:00Z",
  "preparationDeadline": "2026-10-17T12:00:00Z",
  "latestStartTime": "2026-10-19T12:00:00Z",
  "estimatedDisruptionSeconds": 1800,
  "mayProceedAfterDeadline": true,
  "proceedSignal": "ready",
  "readiness": "pending",
  "handoffVersion": "v1"
}
```

## Readiness and concurrent changes

Readiness grants permission for a specific event, allocation, and
`handoffVersion`. It does not release the allocation, drain Kubernetes, or
establish maintenance completion. Acceptance permits an immediate start, even
before the estimated window, but does not guarantee one.

A readiness request carries `{"handoffVersion": "v1"}` and an
`Idempotency-Key`. The provider atomically checks the version and records
readiness. A stale version returns `409` without granting permission; unknown
events return `404`. Events no longer accepting new readiness return `409`.
A readiness call cannot substitute for release.

Before maintenance starts, any schedule or deadline change must issue a new
`handoffVersion` and reset readiness for all affected machines. Material
changes to preparation, impacts, or handoff policy instead supersede the event:
retain the old event as
`superseded` with `supersededBy`, create a new event ID, and require fresh
readiness where applicable. Old permission must never authorize changed work.

Deduplicate accepted actions **before** reevaluating version/state preconditions.
A retry with the same key and identical inputs returns the recorded event
snapshot with `200 OK`, including its original `handoffVersion`, without
changing current state. A changed request with the same key returns `409`.
Read the machine again for current state; a replayed response is not proof
that permission remains current.

For example, if rescheduling changes `v1` to `v2`, a delayed new readiness
request for `v1` fails. Replaying an acknowledgment already accepted for `v1`
only recovers its old result; it does not grant readiness for `v2`.

## Rescheduling

The request includes `handoffVersion` and an optional `startTime`. Omitting
`startTime` requests `latestStartTime`. A scheduled, reschedulable event must
have known window bounds and a preparation deadline.

Atomically check the version and scheduled state, then move `windowStartTime`
no earlier than its current value and no later than `latestStartTime`. Shift
`windowEndTime` and `preparationDeadline` by the same amount; issue a new
`handoffVersion` and reset readiness. A request for the unchanged start time
with the current version is a no-op: it does not invalidate readiness.

Unsupported rescheduling or invalid times return `400`; stale versions or
events no longer scheduled return `409`. Idempotent replays return the
recorded result without rescheduling again. Further deferrals use the current
version and a new key.

The latest start bounds deferral, not authorization. If work outlasts that
bound, Unbounded must decide how to prepare and hand off; the provider
still honors the event's explicit proceed policy.

## Group maintenance

Concurrent planned and emergency event records are allowed; their coexistence
does not authorize conflicting work. A group event appears on each affected
machine with the same event ID and `handoffVersion`;
`affectedMachineIds` lists only machines visible to the caller. Readiness is
recorded per allocation. The provider coordinates all affected allocations
before proceeding, except where the event's deadline policy permits otherwise.

Rescheduling changes the entire group event and resets all affected readiness.
Authorize group-wide changes explicitly; visibility of one machine alone does
not grant authority over other customers' allocations. If a native API uses a
group acknowledgment, the adapter must coordinate all necessary handoffs
before submitting it, or declare that mapping unsupported.

## Repair requests and cancellation

Repair reporting requests action, not feedback only. The body carries a
`reasonCode`, description, optional evidence, and disruption choice:

- `immediate`: no drain is needed. Use `proceedSignal: "none"`, or `release`
  if repair needs the allocation returned first.
- `after_handoff`: wait for `ready` or completed `release`; do not start
  disruption merely because the report was accepted.

```json
{
  "reasonCode": "gpu_failure",
  "description": "GPU 3 reports uncorrectable ECC errors.",
  "evidence": {"source": "dcgm", "detail": "XID 48"},
  "disruption": "after_handoff"
}
```

Return a new event with `type: "repair"` and `origin: "customer"`, setting
preparation and a supported handoff. Providers must accept `other` and may map
unrecognized reason codes to it. Invalid reports return `400`, an existing
active repair returns `409` with its event ID, and throttling returns `429`.
A report during an active lifecycle operation is accepted and waits for that
operation before repair begins.

The reporting customer may cancel its own report while scheduled. Reject
provider-originated or in-progress events with `409`; apply authorization
boundaries between callers. Accepted cancellation ends the event as
`canceled`; retries recover the same result. If provider triage finds no
fault, it also ends the event as `canceled` with an outcome message.

## Outcomes and retention

While maintenance is in progress, conflicting lifecycle requests return `409`
with `eventId`. Completed or failed events include an `outcome`:

```json
{
  "identity": "same",
  "replacementMachineId": null,
  "healthVerified": true,
  "endedAt": "2026-10-17T13:30:00Z",
  "message": "Memory replaced and host validated."
}
```

Identity outcomes are `same`, `replaced` (with `replacementMachineId`),
`released`, or `unknown`. Health verification means provider host checks, not
Kubernetes readiness; use `null` if verification is unknown. Other terminal
states may also include an outcome.

Keep ended events readable for at least seven days after they end. If a
machine is released or replaced, retain its Get record with `state: "released"`
through active associated events and at least seven days after both release
and the last associated event's termination. This preserves outcomes even
when repair outlasts release. Do not carry events to another allocation of
the same physical host. Extend retention as needed to honor advertised
retry-result windows.

Disappearance alone never establishes success. After failure, Unbounded
may request repair again or release the host. It verifies host/agent health
and Kubernetes readiness before restoring workloads.

## Other optional helpers

Operation cancellation is independent of maintenance support; advertise it per
operation kind. `POST /v1/operations/{operationId}/cancel` returns `202 Accepted`
with the original operation when cancellation is requested. It is best effort
and may be rejected after irreversible work. Acceptance is not completion or
rollback; keep observing the original operation.

Image discovery is part of the main guide's
[provisioning guidance](golden-spec.md#image-discovery), not an optional
maintenance capability.
