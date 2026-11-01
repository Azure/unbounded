# Gantry Design

**Status:** Current implementation

This document records the design decisions and invariants implemented by
Gantry. The public architecture document describes how the system works; this
document explains why the system has these boundaries and what tradeoffs they
create.

Historical Gantry documents remain in this repository during the documentation
transition. Some describe proposals rather than shipped behavior. When they
disagree with this document, implementation and tests remain authoritative.

## Problem

Large Kubernetes rollouts can make hundreds or thousands of nodes request the
same image content at nearly the same time. Pulling every copy from the origin
registry creates four related problems:

- Origin bandwidth scales with the number of nodes instead of the unique
  content.
- Registry rate limits and authentication services become rollout
  bottlenecks.
- Cross-region or metered origin traffic becomes unnecessarily expensive.
- A cold rollout can be limited by origin capacity even when nodes have ample
  east-west bandwidth.

Containerd already stores content by digest on every node. Gantry turns those
existing stores into a distributed source of OCI content while keeping
containerd responsible for image assembly and final digest verification.

## Goals

1. Reuse digest-addressed OCI content already present on cluster nodes.
2. Keep workload image references and Kubernetes credential mechanisms
   unchanged.
3. Bound normal cold-start origin work to a small seed cohort per digest.
4. Avoid a central content index, central scheduler, or new persistent cache.
5. Preserve pull liveness when discovery, peers, chairs, or the network are
   degraded.
6. Verify all peer-supplied content against its requested digest.
7. Scale control-plane and peer state independently of the total content size.
8. Expose enough health and metrics to distinguish local, peer, origin, and
   fallback paths.

## Non-goals

- Replacing the origin registry as the image source of truth.
- Publishing or mutating images.
- Providing image signing, provenance, or admission policy.
- Coordinating mutable tag freshness inside Gantry.
- Guaranteeing exactly one origin pull during partitions or failures.
- Providing cross-cluster discovery.
- Encrypting the peer data plane without help from the cluster network.
- Maintaining a separate Gantry-owned content cache.

## Core Invariants

### Content identity is a digest

Peer discovery, transfer, cold-start ranking, negative caching, and local
storage all operate on OCI digests. Tags are mutable names and are not suitable
as distributed cache keys.

The mirror therefore returns 503 for tag-manifest requests so containerd can
continue to the origin and resolve the tag. Digest-addressed requests can then
use Gantry.

Implementation:

- [`internal/gantry/mirror/mirror.go`](../internal/gantry/mirror/mirror.go#L671-L839)
- [`internal/gantry/digest/digest.go`](../internal/gantry/digest/digest.go)

### Content is served only when it is openable

Containerd metadata alone does not prove that an ingest completed. Gantry
advertises and serves content only when the local store can open it.

Implementation:

- [`internal/gantry/containerdstore/store.go`](../internal/gantry/containerdstore/store.go#L111-L190)
- [`internal/gantry/advertise/advertise.go`](../internal/gantry/advertise/advertise.go#L180-L245)

### Peer bytes are verified

The requested digest remains authoritative regardless of the peer that returns
the stream. Gantry verifies the incoming stream before committing it, and
containerd verifies content again before using it.

Implementation:

- [`internal/gantry/digestpipe/digestpipe.go`](../internal/gantry/digestpipe/digestpipe.go)
- [`internal/gantry/containerdstore/store.go`](../internal/gantry/containerdstore/store.go)

### Origin fallback is a liveness path, not the common path

Direct-origin fallback is considered only after local, peer, and cold-start
paths have failed. It is observable and intentionally allowed because a strict
pull-once rule would turn discovery or coordination failures into workload
outages.

Implementation:

- [`internal/gantry/mirror/mirror.go`](../internal/gantry/mirror/mirror.go#L892-L931)
- [`internal/gantry/mirror/nf5.go`](../internal/gantry/mirror/nf5.go)

### Unavailable dependencies fail explicitly

An unavailable containerd store is not reported as a cache miss. Transfer
returns 503, readiness fails, and provider reconciliation pauses. Missing
credential files, invalid configuration, and unsupported authorization also
surface explicit errors.

Implementation:

- [`internal/gantry/containerdstore/store.go`](../internal/gantry/containerdstore/store.go)
- [`internal/gantry/transfer/transfer.go`](../internal/gantry/transfer/transfer.go#L379-L405)
- [`internal/gantry/config/config.go`](../internal/gantry/config/config.go#L907-L1110)

## Decision: Use Containerd as the Only Content Store

Gantry previously considered a separate host-path cache. The current design
uses the node's containerd content store exclusively.

### Rationale

- Image content is already stored there.
- A second cache would duplicate disk use and require separate eviction.
- Containerd supplies a content-addressed writer and reader contract.
- Kubelet and containerd keep their normal ownership of image lifecycle.
- Peer content becomes immediately reusable by normal image pulls.

### Consequences

- Gantry requires access to the containerd socket.
- Containerd namespace and socket layout are deployment configuration.
- Background ingests need leases so garbage collection cannot remove
  incomplete work.
- Readiness depends on opening the local content store.
- Containerd restart and socket replacement must be handled without silently
  serving stale success.

Implementation:

- [`cmd/gantry/agent_storage.go`](../cmd/gantry/agent_storage.go)
- [`internal/gantry/containerdstore/lease.go`](../internal/gantry/containerdstore/lease.go)

## Decision: Keep Tag Resolution at the Origin

Gantry deliberately avoids a distributed tag cache. Tag freshness requires
registry-specific invalidation, consistency, and authorization rules that are
separate from content distribution.

Returning 503 for a tag request lets containerd use the next configured host.
Once containerd has a manifest digest, immutable content can use the peer
system.

### Tradeoff

Every pull may still contact the origin for tag resolution. Gantry primarily
reduces manifest, configuration, and layer bytes, especially for large layers
shared by many nodes.

## Decision: Use a DHT for Warm Content Discovery

Agents publish digest provider records to a libp2p Kademlia DHT. The DHT stores
provider metadata, not image bytes.

### Rationale

- Provider state is distributed rather than held by one service.
- Lookup keys naturally match immutable content digests.
- Agents can join and leave without updating one complete cluster membership
  object.
- Discovery and data transfer remain separate concerns.

### Consequences

- Provider records can be stale.
- Agents need persisted libp2p identities and usable advertised addresses.
- Initial bootstrap and routing-table convergence affect readiness.
- Provider reconciliation must tolerate containerd unavailability.
- DHT degradation requires a separate cold-start and fallback path.

Implementation:

- [`internal/gantry/discovery/discovery.go`](../internal/gantry/discovery/discovery.go)
- [`internal/gantry/advertise/advertise.go`](../internal/gantry/advertise/advertise.go)

## Decision: Transfer Content Outside the DHT

Peers transfer content directly over cleartext HTTP/2. DHT connections are for
discovery and coordination, not bulk OCI payloads.

### Rationale

- HTTP supports streaming, `HEAD`, range responses, and standard cancellation.
- The transfer server can read directly from containerd.
- Bulk transfer does not consume DHT stream and connection budgets.
- Source-specific metrics can measure peer traffic.

### Security tradeoff

The endpoint uses h2c and assumes a trusted or protected cluster network. It
requires an internal request header and serves only local digest content, but
those controls do not provide confidentiality or strong peer identity.
Deployments requiring transport security must add NetworkPolicy, network
encryption, or a service mesh.

Implementation:

- [`internal/gantry/transfer/transfer.go`](../internal/gantry/transfer/transfer.go)
- [`internal/gantry/transfer/client.go`](../internal/gantry/transfer/client.go)

## Decision: Coordinate Cold Pulls with Lease-Backed Chairs

A DHT lookup cannot return a provider for content that does not exist in the
cluster. Gantry uses a bounded set of stable chair names represented by
Kubernetes Leases.

Agents claim and renew chairs. For each digest, requesters deterministically
rank the chair set and contact a bounded seed cohort. Selected holders
deduplicate local pull work, fetch from the origin, and advertise the completed
content.

### Why stable chair names

Ranking a stable set produces the same cohort on independent requesters without
a central scheduler. Different digests produce different rankings, spreading
origin work across holders.

### Why multiple seeds

One seed minimizes origin traffic but makes cluster progress depend on one
node's performance and availability. A bounded cohort accepts a small amount
of redundant origin traffic to improve convergence and tolerate slow or failed
holders.

### Why not recruit replacements immediately

A chair that has not completed may already be pulling. Immediate replacement
adds origin work rather than moving it. The cascade therefore distinguishes a
busy cohort from a completely unreachable cohort and keeps direct fallback as
the final liveness option.

### Control-plane tradeoff

Chair Leases create a bounded coordination set instead of a Lease or watch per
digest. The configured chair count and active DaemonSet capacity bound the
holder pool. Rotation redistributes the extra origin duty.

Implementation:

- [`internal/gantry/chairs/`](../internal/gantry/chairs/)
- [`internal/gantry/coldstart/`](../internal/gantry/coldstart/)
- [`internal/gantry/hrw/`](../internal/gantry/hrw/)
- [`internal/gantry/coord/`](../internal/gantry/coord/)

## Decision: Prefer Requester-Delegated Authentication

The requester already has the authorization context chosen by kubelet and
containerd. Gantry relays origin challenges and carries request-scoped
authorization to the selected puller.

### Rationale

- Selected chairs do not need every workload's Secret.
- Existing `imagePullSecrets` and kubelet credential providers remain the
  policy authority.
- Credentials need not be persisted in Gantry.
- An origin rejection does not silently switch to a more privileged node
  identity.

### Shared identity

A configured credential file remains available for environments that cannot
delegate credentials. It is a compatibility mode with a larger credential
exposure surface.

Implementation:

- [`internal/gantry/origin/origin.go`](../internal/gantry/origin/origin.go)
- [`internal/gantry/registryauth/registryauth.go`](../internal/gantry/registryauth/registryauth.go)

## Decision: Keep Origin Failure Cooldowns Local

Terminal origin failures enter a local exponential negative cache. Success
clears the entry.

A cluster-wide failure cache could reduce duplicate failures further, but it
would also distribute authorization-specific or transient failures and create
another consistency contract. Local cooldowns provide backoff without making
one node's result authoritative for every requester.

Implementation:

- [`internal/gantry/negcache/negcache.go`](../internal/gantry/negcache/negcache.go)

## Failure Model

Gantry favors workload liveness over a strict global pull-once property.

| Condition | Design response | Accepted cost |
| --- | --- | --- |
| Stale provider | Filter or rediscover and try another source | Additional lookup latency |
| Peer disconnect | Retry resolution or cold-start path | Duplicate peer work |
| Chair unavailable | Continue bounded cascade | Possible extra seed |
| DHT partition | Use reachable chairs and gated fallback | Redundant origin pulls across partitions |
| Origin terminal failure | Enter local cooldown | Node-local retry state may differ |
| Containerd unavailable | Fail readiness and transfer explicitly | Node temporarily stops serving |
| Agent shutdown | Drain mirror and stop dependencies in order | New requests fall through |
| Corrupt peer stream | Reject on digest mismatch | Retry from another source |

The system does not promise exactly one origin pull. Under healthy conditions,
origin work should approach the configured seed cohort per digest. Failures,
partitions, retries, and stale discovery can increase that count.

## Scalability Model

The main scaling boundaries are:

- **Content size:** bytes move directly between peers rather than through the
  Kubernetes API or DHT.
- **Content count:** provider records and local inventory grow with digests.
- **Node count:** DHT routing and connection-manager limits grow with the
  routing working set, not a deliberate full mesh.
- **Cold starts:** chair count bounds stable origin-puller identities; seed
  count bounds normal origin copies per digest.
- **Control plane:** Lease activity depends on the configured chair pool, not
  on every digest request.
- **Local concurrency:** transfer, coordination, inflight, and puller limits
  bound work accepted by one agent.

The code includes explicit connection-manager, coordination-stream,
max-digest, serve-concurrency, and timeout controls. These are safety bounds,
not proof of behavior at every cluster size.

## Performance Evidence

The benchmark harness compares direct origin pulls with Gantry pulls using
fresh image content and measures registry bytes, successful origin requests,
pod readiness, and peer activity.

Recorded campaigns include:

- A 1,000-node AKS cluster pulling a 40 GiB image with 40 layers.
- A 2,000-node campaign using the same general benchmark shape.
- Smaller direct-mode campaigns used for operational validation.

Across the recorded 1,000-node and 2,000-node result rows, Gantry phases
reported more than 99.4 percent reduction in source ACR bytes and 2 to 6
successful ACR pulls in the measured phases.

These results are observations, not protocol guarantees:

- The runs used specific AKS, ACR, network, image, and chart configurations.
- Some latency rows use retained pod timestamps when audit telemetry was
  unavailable.
- Direct-mode baselines can encounter ACR throttling and 429 responses.
- Monitoring queries and Prometheus memory became constraints in larger runs.
- A benchmark proves only the code, scale, and failure conditions exercised by
  that campaign.

Evidence:

- [`hack/gantry-benchmark/RESULTS.md`](../hack/gantry-benchmark/RESULTS.md)
- [`hack/gantry-benchmark/README.md`](../hack/gantry-benchmark/README.md)
- [`hack/cmd/gantry-benchmark/`](../hack/cmd/gantry-benchmark/)

## Large-Layer Behavior

Continuous streaming avoids buffering an entire layer in Gantry memory, but
very large single-layer pulls remain sensitive to timeouts, cancellation, and
interruption.

The existing 128 GiB analysis identifies two distinct concerns:

1. A continuously progressing transfer must not fail because of an unrelated
   short absolute timeout.
2. An interrupted transfer does not yet have an efficient general ranged
   resume contract across every origin, peer, and containerd ingest path.

The first concern has driven no-progress timeout work. The second remains a
separate efficiency limitation. The repository does not yet contain production
measurements proving 128 GiB single-layer behavior across all paths.

Evidence:

- [`designs/gantry-128gib-single-layer-pulls.md`](gantry-128gib-single-layer-pulls.md)

## Compatibility and Upgrade Boundaries

### Configuration

Configuration precedence is defaults, YAML, scalar environment variables, then
flags. YAML rejects unknown fields. The legacy `chair_holder_count` name is
accepted only as a compatibility alias and conflicts are rejected.

### Coordination protocol

The libp2p coordination protocol ID is `/gantry/coord/1.1.0`. Changes to
protobuf meaning, request limits, authentication propagation, or chair
semantics require mixed-version analysis.

### Chair count

Releases that only understand the original 64 chair names cannot safely run
while newer agents create higher-numbered Leases. Increase chair count only
after every agent is upgraded to a count-aware release.

### Installation ownership

Standalone Helm and operator-managed installs are mutually exclusive.
`PriorityClass/gantry-low` records the active manager. Operator mode excludes
the standalone node configurator because `unbounded-agent` owns the containerd
route.

## Alternatives Considered

### Every requester pulls from the origin

This preserves simplicity but causes origin traffic to scale with node count
and recreates the pull storm Gantry is intended to prevent.

### One permanent origin puller

One puller minimizes origin copies but becomes a throughput bottleneck and
single failure domain. Per-digest ranking across a rotating chair pool spreads
load and permits a bounded seed cohort.

### Central content index or scheduler

A central service simplifies lookup and assignment but introduces a scaling,
availability, and operational dependency. Gantry instead distributes warm
discovery and computes cold-start ranking independently.

### Complete Kubernetes membership view on every agent

Watching every Gantry Pod or Node makes selection straightforward, but sends
fleet-size membership state and churn to every agent. The bounded chair set
limits Kubernetes coordination state.

### Separate Gantry cache

A separate cache allows independent eviction but duplicates containerd content
and lifecycle machinery. The current implementation accepts tighter
containerd coupling to avoid duplicate storage.

### Cluster-wide negative cache

Sharing origin failures could reduce retries but risks spreading
request-specific authentication failures and adds consistency state. Gantry
keeps cooldowns local.

### Immediate replacement of slow seeds

Replacing a slow seed can add another origin pull while the original continues.
The bounded cascade avoids treating slowness as proof that no pull is active.

## Open Questions and Known Limits

1. Validate chair and DHT behavior at the 100,000-node design target.
2. Measure duplicate `please_pull` traffic and dissemination pressure at
   scales beyond current benchmark campaigns.
3. Define an efficient resume contract for interrupted very large layer
   transfers.
4. Establish production evidence for 128 GiB single-layer pulls.
5. Determine whether the plaintext peer data plane needs first-class mutual
   authentication or encryption.
6. Quantify convergence and origin amplification during prolonged network
   partitions.
7. Expand e2e coverage for origin-failure circuits, puller termination during
   cold start, and DHT-empty startup fallback.
8. Continue testing mixed-version boundaries before changing coordination,
   advertisement, or chair semantics.

## Verification Strategy

Changes to Gantry should preserve the design at several levels:

| Level | What it verifies |
| --- | --- |
| Unit tests | Parsing, ranking, cache behavior, protocol limits, range handling, and errors |
| Package integration tests | Containerd ranges, digest mismatch, stale providers, cold-start, and authentication |
| Kind e2e | Deployment, readiness, peer reuse, cold starts, leases, hardening, and containerd restart |
| Live-cluster install validation | Node configuration, real credential providers, registry access, and uninstall |
| Scale benchmark | Origin bytes, pull counts, latency, peer activity, and operational ceilings |

Documentation claims should be labeled according to the strongest available
evidence: implemented, test-covered, benchmark-observed, or proposed.

## Historical Documents

The following documents remain available during the documentation transition:

- [`gantry-detailed-design.md`](gantry-detailed-design.md)
- [`gantry-cold-pull-explained.md`](gantry-cold-pull-explained.md)
- [`gantry-cold-pull-rotating-chair.md`](gantry-cold-pull-rotating-chair.md)
- [`gantry-scalable-peer-discovery.md`](gantry-scalable-peer-discovery.md)
- [`gantry-128gib-single-layer-pulls.md`](gantry-128gib-single-layer-pulls.md)
- [`gantry-unbounded-integration.md`](gantry-unbounded-integration.md)

These files are historical evidence, proposals, or focused analyses. Their
continued presence does not make unimplemented proposals part of the current
design.
