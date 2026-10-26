# SiteNodeSlice Reconciliation

## Purpose

The site controller publishes non-gateway node connectivity data in
`SiteNodeSlice` resources. Kubernetes remains the authoritative persisted
state. Informer caches are used to avoid API reads when cached desired and
observed state already match.

## Reconciliation Scope

Node and `SiteNodeSlice` events dirty only the affected Sites when their Site
membership is known. A reconciliation pass still evaluates Site ownership
against the complete cached Site set, but only rebuilds slices, status, and
protection finalizers for dirty Sites.

Changes with broad matching effects use a global fallback:

- Site additions, updates, and deletions
- GatewayPool selector changes
- Initial controller startup
- Events whose resource type cannot be decoded safely

This preserves correctness when a change can move nodes between Sites or
change which nodes are excluded as gateways.

## Cache and API Authority

An unchanged cache-synchronized pass performs no live Site or SiteNodeSlice
reads. Cached objects are comparison inputs only. Before a create, update, or
delete, the controller reads the live object and applies these fences:

- Site UID must still match, preventing writes for a deleted and recreated Site.
- Slice updates start from the live object, preserving metadata, finalizers,
  and fields owned by other actors.
- Conflict, AlreadyExists, and NotFound responses cause a fresh live read.
- Slice deletes use live UID and resourceVersion preconditions.
- Site status and protection finalizers use live state whenever a cached
  difference requires mutation.

Informer events provide eventual repair when another actor changes managed
slice fields or Site metadata.

## Orphan Repair

Orphan detection requires a live cluster-wide Site LIST and live slice reads,
so it does not run for every topology event. The controller runs it at startup
and periodically afterward.

Repair remains conservative:

- A failed Site LIST does not authorize any deletion.
- An empty Site LIST does not mean every slice is orphaned.
- Legacy net-group Sites are checked during migration.
- Orphan deletion uses live UID and resourceVersion preconditions.

Owner-reference garbage collection remains the normal path for deleted Sites.
