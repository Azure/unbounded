# Hillsboro Karpenter capacity provider

## Scope and decision

Hillsboro uses the standard Karpenter ownership chain:

`NodePool -> NodeClaim -> UnboundedNodeClass -> CloudProvider -> Machine`

`UnboundedNodeClass` is cluster scoped in
`karpenter.unbounded-cloud.io/v1alpha1`. A distinct API group keeps
provider-specific configuration separate from the generic Machina inventory
API and matches Karpenter's provider NodeClass convention.

Karpenter owns demand, scheduling, disruption, drain, PDB handling, and
NodeClaim lifecycle. The Unbounded provider owns selection and reservation of
one existing `Machine`, worker provisioning handoff, stable provider identity,
and release barriers. Machina continues to own generic Machine observation and
configuration. MetalMan and Rack-SCM are outside this provider boundary.

## Provider architecture

The dedicated `unbounded-karpenter-provider` binary uses Karpenter v1.15.0's
real `CloudProvider` interface and standard controller suite. It registers
Machine and UnboundedNodeClass APIs in the global client-go scheme before
calling `operator.NewOperator`.

The provider exposes every instance capacity declared by an
`UnboundedNodeClass`, including types with no free offering. Availability and
reserved-capacity counts reflect unreserved matching Machine inventory. The
Hillsboro profile uses Karpenter's standard `reserved` capacity type because
the provider allocates pre-existing physical hosts rather than buying new
on-demand capacity.

Create performs these steps:

1. Resolve the NodeClass and require its standard `Ready=True` condition.
2. Resolve the referenced `MachineConfiguration` version and exact worker
   image.
3. Select a compatible instance type and list matching Machine inventory.
4. Require `Available=True`, no reservation, and `Quarantined!=True`.
   `Available` is derived from immutable identity, a capacity domain, and
   reservation/quarantine state. Machine phase is never allocation
   authorization.
5. Derive `unbounded://<sha256>` from immutable host ownership fields.
6. Acquire the per-domain Kubernetes Lease and reserve exactly one Machine
   with a conflict-safe status update keyed by NodeClaim UID.
7. Pin the Machine configuration, provider ID, and resolved Node labels.
8. Invoke the narrow southbound provisioning interface if the reserved worker
   is not already healthy. The production default fails closed.
9. Hydrate the NodeClaim only after the Ready Node reports the exact provider
   ID, required image, and required hardware health.

Reservations are durable and contain NodeClaim name and UID, provider ID,
domain, NodeClass generation, selected instance type, configuration version,
image ID, resolved labels and taints, capacity, GPU policy, reservation time,
and release stage. Get and List reconstruct NodeClaims from this immutable
launch snapshot after process restarts or NodeClass changes.

## Identity

Provider identity may derive from:

- typed immutable physical serial and MAC identity;
- Azure resource ID;
- an immutable external provider Machine reference.

DHCP addresses, SSH endpoints, Node InternalIP, and mutable Rack-SCM server IDs
are observations or routes, never identity. Mutable external provider IDs and
netboot lease data are also excluded from provider identity. Create fails
closed if no immutable host identity is available. Kubelet receives the
reservation provider ID through `--provider-id`, and the provider requires the
resulting `Node.spec.providerID` to match exactly.

## GB200 policy

The Hillsboro profile is `hillsboro-gb200-4g`. Its NodeClass declares CPU,
memory, pod, and exactly four `nvidia.com/gpu` capacity. Successful hydration
requires:

- the exact approved NVIDIA-enabled image;
- a Ready Node;
- exactly four total GPUs;
- exactly four allocatable healthy GPUs;
- current healthy fabric observation;
- an exact provider ID match.

There is no `DisableNVIDIA` path and no CPU fallback.

## Release

Karpenter drains the Node and enforces PDB and disruption policy before
calling provider Delete. Delete acquires the same domain Lease and advances one
durable Machine reservation stage per call:

1. `RevokeCredentials`
2. `ResetWorker`
3. `DeleteNode`
4. `ReleaseReservation`

The Machine does not return to `Available=True` until every barrier succeeds.
The first call that clears the reservation returns nil. A later call returns
Karpenter's `NodeClaimNotFoundError`, which completes provider deletion.
Unavailable southbound handlers fail closed and preserve the reservation and
current stage.

## Deployment prerequisites

Install upstream Karpenter v1.15.0 CRDs before the provider. This repository
does not copy the upstream NodePool or NodeClaim CRDs. Then install:

- Machina CRDs and controller;
- `deploy/karpenter-provider/crd/karpenter.unbounded-cloud.io_unboundednodeclasses.yaml`;
- the provider RBAC, Service, and Deployment;
- a valid MachineConfiguration;
- an UnboundedNodeClass and NodePool, such as
  `deploy/karpenter-provider/examples/hillsboro-gb200.yaml`.

The example contains no credentials, DHCP identity, or Rack-SCM identity. It
uses the approved Hillsboro NVIDIA worker image and marks gb200-3 and gb200-4
allocation-disabled until their operator-session gates are cleared.

## Upgrade and rollback

Upgrade Karpenter and the provider together only after interface and CRD
compatibility validation. Preserve Machine reservations during rollout so Get,
List, and Delete recover after restart. Rollback must use a provider build that
understands every persisted reservation field and release stage. Do not remove
the NodeClass CRD or Karpenter CRDs while reservations or NodeClaims exist.

## Current operational history

The existing Hillsboro live-cluster work remains truthful: GB200 hosts were
audited, NVIDIA bootstrap was hardened, and some hosts were deliberately
quarantined or blocked when health and operator-session gates failed. The
Karpenter provider in this design has not been deployed to that cluster. No
live capacity action, host reset, or Rack-SCM mutation was performed as part of
this corrective pivot.

## Remaining fail-closed seams

- Wire a production provisioning handler to the existing Machine lifecycle.
- Wire credential revocation, worker reset, and Node cleanup handlers.
- Add an authenticated source for GPU model and fabric health.
- Validate the provider and upstream Karpenter controllers in an isolated
  integration cluster before any Hillsboro deployment.

## Non-goals

- Creating a replacement custom demand or claim CRD.
- Creating physical Machines or cloud instances from this inventory provider.
- Treating Machine phase, DHCP address, or Rack-SCM ID as allocation identity.
- Bypassing Karpenter drain and PDB semantics.
- Returning fake launch or release success when southbound handlers are absent.
- Providing CPU fallback for dedicated GB200 capacity.
