# Hillsboro capacity allocation

## Scope

Hillsboro is the concrete inventory example for the generic Machina
`CapacityClaim` allocation layer. A `CapacityClaim` represents unsatisfied
capacity and binds exactly one existing `Machine`. It does not create hardware,
replace immutable inventory identity, or authorize operations from
`Machine.status.phase`.

The implementation is provider-neutral. Hillsboro-specific values appear only
in example inventory and validation scenarios.

## Architecture

The existing Machina controller manager watches `CapacityClaim`, `Machine`,
`MachineConfiguration`, `MachineConfigurationVersion`, and `Node` resources.
There is no separate allocation binary.

The allocation flow is:

1. Resolve the claim's `MachineConfiguration` version and image.
2. Validate claim policy. `RequireNVIDIA` requires an exact
   `imagePolicy.requiredImage` trust anchor. Dedicated NVIDIA GB200 claims
   additionally require exactly four GPUs.
3. List Machines matching the hardware, site, and domain label selectors.
4. Require authoritative `Available=True`, no reservation, and
   `Quarantined!=True`. A Machine does not need to be launched or
   `AllocationReady` before reservation. `Machine.status.phase` is not
   consulted.
5. Acquire the selected Machine's capacity-domain Lease in
   `unbounded-system`.
6. Re-read the Machine and write one typed reservation with
   `RetryOnConflict`.
7. Pin the requested `MachineConfiguration` and publish the bound Machine name
   and UID on the claim.
8. Invoke the idempotent scale-up lifecycle operation while the bound Machine
   does not satisfy the claim. The production default fails closed until a
   southbound implementation is wired.
9. Derive claim readiness only from typed Node, image, GPU, and fabric
   observations.

Lease names are stable DNS-safe names derived from the domain label. Lease
holders may take an expired Lease, but cannot take an unexpired Lease owned by
another controller instance. A local per-domain gate also prevents concurrent
callbacks in one controller process from treating the shared holder identity
as reentrant. Read-only selection and readiness evaluation may run
concurrently. Reservation, provisioning, duplicate repair, and release
mutations always run while holding the domain Lease.

## API behavior

`CapacityClaim.spec` includes:

- a general Machine label selector;
- independent site and capacity-domain selectors;
- a consumer API identity including UID;
- a release policy;
- a `MachineConfiguration` reference;
- exact image and NVIDIA requirements;
- exact GPU vendor, model, count, and fabric requirements.

`CapacityClaim.status` includes the bound Machine name and UID, observed
generation, authoritative conditions, phase, and release stage. Supported
phases are `Pending`, `Bound`, `Provisioning`, `Ready`, `Releasing`, and
`Failed`.

`Machine.status.reservation` records the claim name, claim UID, domain, and
reservation time. `Machine.status.allocation` records:

- Node name and Ready state;
- current internal IP;
- current worker image;
- GPU vendor and model;
- total and healthy GPU counts;
- GPU fabric health.

The current internal IP is mutable observed state. It is never an inventory
identity or CapacityClaim selector. Inventory identity belongs in immutable
labels or provider-owned spec, such as serial number and MAC address.

## Node observations

The Machine controller refreshes Node name, Ready condition, internal IP, and
the applied agent image before any SSH-specific reconciliation. This also
works for externally managed Machines without SSH configuration. It reads
`nvidia.com/gpu` capacity and allocatable values for total and healthy GPU
counts. GPU vendor and model may fall back to Machine inventory labels. Fabric
health comes only from the current Node observation:

- `unbounded-cloud.io/gpu-vendor`
- `unbounded-cloud.io/gpu-model`
- `unbounded-cloud.io/gpu-fabric-healthy` on the Node

For GB200, allocation readiness requires four total GPUs, four healthy GPUs,
and healthy fabric. A missing fabric observation is not healthy. When the Node
disappears, stale Node and GPU observations are cleared and
`AllocationReady=False`. A Machine can remain in a human-facing Ready phase
while allocation readiness is false.

`Available` is inventory eligibility, not launched readiness. Node observation
does not automatically set it true. Release sets it true only after all
barriers complete and only when the Machine is not quarantined.

## Release barriers

Deletion of a bound claim advances one durable stage at a time:

1. `CordonDrain`
2. `RevokeCredentials`
3. `ResetWorker`
4. `DeleteNode`, unless `releasePolicy` is `RetainNode`
5. `ReleaseReservation`

The production lifecycle implementation currently fails closed for scale-up
and the first four southbound release stages. Reset and Node deletion clear
stale allocation observations and force `AllocationReady=False`. The
controller never clears the reservation or sets `Available=True` until every
required barrier succeeds. Unit tests use a fake lifecycle to verify stage
ordering and barrier behavior.

## Hillsboro inventory example

`deploy/machina/examples/hillsboro-capacity.yaml` contains:

- four existing GB200 Machine inventory objects;
- a MachineConfiguration using
  `ghcr.io/azure/agent-ubuntu2404-nvidia:v20260619`;
- a CapacityClaim requiring one dedicated four-GPU GB200 system.

The example uses serial and MAC labels for immutable inventory identity. It
does not include credentials, current DHCP addresses, or Rack-SCM identifiers.
The CapacityClaim selector uses hardware class, GPU type, site, and allocation
domain. It deliberately does not select a serial number or MAC address.

The operational examples represented by tests are:

| Machine | Allocation state |
| --- | --- |
| gb200-1 | Eligible inventory may be reserved before worker bootstrap |
| gb200-2 | DHCP changes affect observations, not inventory identity |
| gb200-3 | Rejected while `Quarantined=True` |
| gb200-4 | Rejected while inventory `Available=False` |

The tracked example manifests do not set status. Inventory and observation
controllers own status in a live deployment.

## Restart and duplicate recovery

On restart, a claim without bound status searches typed Machine reservations
for its name and UID. If more than one Machine carries that reservation, every
affected Machine is quarantined and retains its reservation for explicit
cleanup. The controller does not bypass release barriers to repair an
ambiguous reservation.

If multiple claims report the same bound Machine and the Machine has no
reservation, claim name and UID ordering selects one deterministic winner.
Losers become `Failed` with an actionable duplicate-binding reason. If a
Machine already has a reservation, that typed reservation is authoritative.

## Validation

Focused tests cover:

- eligible allocation and configuration pinning;
- restart recovery;
- fail-closed quarantine for duplicate restart reservations;
- deterministic duplicate rejection;
- release stage ordering and barriers;
- lock contention and Lease expiration;
- DHCP address changes remaining status-only;
- unhealthy GPU fail-closed behavior;
- quarantined and blocked gb200-3/4 scenarios;
- GB200 policy serialization and API generation.

Generated deepcopy and CRD artifacts are produced with the repository's
`go generate` workflow. Machina RBAC and embedded rendered manifests are
produced with `make machina-manifests`.

## Remaining southbound seams

- Implement cordon and eviction with explicit disruption policy.
- Implement credential revocation for the selected bootstrap mechanism.
- Implement worker reset behind an audited provider-neutral interface.
- Implement Node deletion after reset and credential barriers.
- Add an authenticated hardware observer for GPU model and fabric labels.
- Add provider adapters that create CapacityClaims from external schedulers.

Until those seams are implemented and wired, release stops at the first
destructive stage and preserves the reservation.

## Non-goals

- CapacityClaim does not create Machines or cloud instances.
- CapacityClaim does not use Machine phase as an allocation signal.
- CapacityClaim does not use IP address or Rack-SCM ID as host identity.
- CapacityClaim does not terminate sessions, reboot hosts, invoke BMC, change
  PXE state, alter switches, manage credentials, or mutate disks.
- CapacityClaim does not provide a CPU fallback for dedicated GPU claims.
- CapacityClaim does not let future Karpenter NodeClaims directly own
  Machines.
