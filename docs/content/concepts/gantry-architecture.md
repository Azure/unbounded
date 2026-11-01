---
title: "Gantry Architecture"
weight: 3
description: "How Gantry discovers, coordinates, transfers, and stores OCI content across Kubernetes nodes."
---

Gantry is a node-local OCI registry mirror and peer-to-peer distribution
system. It reduces duplicate origin traffic without introducing a separate
cluster cache or centralized content index.

Every Gantry agent uses the local containerd content store as its storage
backend. Agents advertise available digests through a libp2p Kademlia DHT,
transfer content directly between nodes, and use Kubernetes Lease-backed
chairs to coordinate the first origin pulls for content that no peer has
advertised.

## System Context

![Gantry architecture: a requester containerd uses its node-local Gantry mirror; Gantry queries provider records, coordinates a cold pull when necessary, transfers content from a selected peer, and stores verified content in containerd](../../img/gantry-architecture.svg)

Gantry is transparent to workload specifications:

1. Kubelet asks containerd to pull a normal OCI image reference.
2. Containerd resolves tags and requests digest-addressed content through its
   configured Gantry mirror.
3. Gantry serves local content, fetches from a peer, or coordinates an origin
   pull.
4. Containerd commits and verifies the returned content in its own content
   store.

Gantry does not store image bytes in the DHT. Provider records map a digest to
peers that have advertised that digest.

## Components

Each agent wires these subsystems into one process:

| Component | Responsibility |
| --- | --- |
| Registry mirror | Implements the containerd-facing OCI distribution endpoints |
| Containerd store | Reads, writes, and inventories content in the local containerd namespace |
| Discovery host | Runs libp2p and the Kademlia DHT |
| Advertiser | Reconciles local content with DHT provider records |
| Transfer server and client | Streams digest-addressed content between peers |
| Chair manager | Claims, renews, rotates, and observes Kubernetes Lease-backed chairs |
| Cold-start resolver | Selects chair-backed origin pullers after peer lookup misses |
| Coordination services | Carry bounded `please_pull` requests and outcomes |
| Origin clients | Fetch from configured registries with delegated or shared authentication |
| Negative cache | Applies local cooldowns after terminal origin failures |
| Operations endpoint | Exposes metrics, readiness, and liveness |

The command composition lives in `cmd/gantry`. Reusable subsystem
implementations live under `internal/gantry`.

## Listeners and Protocols

Default chart ports are:

| Port | Protocol | Scope | Purpose |
| --- | --- | --- | --- |
| `5000` | HTTP | Node-local mirror route | Containerd-facing OCI registry mirror |
| `5001` | Cleartext HTTP/2 | Peer network | Digest transfer between agents |
| `5002` | HTTP coordination | Peer network | Chair cold-start requests |
| `4001` | libp2p | Peer network | DHT and peer connectivity |
| `9095` | HTTP | Operations network | `/metrics`, `/readyz`, and `/livez` |

The transfer endpoint requires the internal `Gantry-Mirrored: 1` request
header and serves only content already present in the local store. It is not a
general registry proxy. NetworkPolicy or another cluster-network control must
provide network isolation.

Digest coordination also has a libp2p stream protocol identified by
`/gantry/coord/1.1.0`, while chair calls use the dedicated HTTP listener.
Stream requests and responses are length-delimited protobuf messages with
deadlines, concurrency limits, and digest-count limits.

## Startup and Readiness

Startup follows a dependency order:

1. Load defaults, optional YAML, environment variables, and command-line
   flags.
2. Validate the complete configuration.
3. Start the libp2p discovery host and DHT.
4. Connect to the local containerd content store.
5. Start peer transfer and chair coordination endpoints.
6. Start chair management and cold-start coordination.
7. Construct origin clients, negative caching, and fallback controls.
8. Start the containerd-facing mirror.
9. Start containerd content subscription and provider reconciliation.
10. Start metrics, readiness, and liveness.

The mirror has a startup gate. Until Gantry has satisfied readiness
prerequisites, its OCI base endpoint returns `503 Service Unavailable`, which
allows containerd to continue through its configured host chain. The mirror is
marked ready only after its dependencies are available. Draining overrides
readiness during shutdown.

`/livez` answers whether the process is running. `/readyz` is stricter and may
return 503 when the local containerd store, chair set, advertised addresses, or
peer routing state cannot support normal service.

## Configuration Sources

Runtime configuration uses this precedence, from lowest to highest:

1. Built-in defaults.
2. Optional YAML supplied by `--config`.
3. Scalar `GANTRY_*` environment variables.
4. Command-line flags.

YAML decoding rejects unknown fields. List-valued settings remain file-based
rather than accepting environment overlays. Validation reports all detected
configuration problems together.

Important validation rules include:

- The mirror binds to loopback unless an explicit deployment opt-in allows a
  non-loopback pod listener.
- The optional pprof listener must bind to loopback.
- Containerd is the only supported storage mode.
- The containerd socket and at least one upstream registry are required.
- Chair counts, seed counts, timeouts, listener addresses, and libp2p
  connection-manager bounds must be internally consistent.

## Containerd Request Routing

### Base Endpoint

`GET /v2/` returns the OCI registry base response after the mirror becomes
ready. During startup or draining, the startup gate may return 503.

### Tag Resolution

Gantry distributes content by digest, not by mutable tag. A manifest request
whose reference is a tag returns 503 so containerd can continue to the origin
host and resolve the tag. This avoids creating a separate tag-freshness and
invalidation system inside Gantry.

After origin resolution produces a digest, containerd requests the manifest,
configuration, and layers through the digest-addressed path.

### Registry Namespace

Containerd supplies the original registry namespace through the `ns` query
parameter. Gantry uses it to choose a configured upstream. When multiple
upstreams exist, an absent or unknown namespace is rejected rather than
guessing an origin.

### Digest GET

For a digest-addressed manifest or blob, Gantry follows this broad order:

1. Open the digest from the local containerd content store.
2. Query the DHT for peers advertising the digest.
3. Fetch from a usable peer and stream the content to local containerd.
4. If no provider succeeds, invoke cold-start chair coordination.
5. Wait for newly available providers and retry the peer path.
6. Use the gated direct-origin fallback only after peer and cold-start paths
   are exhausted.

The exact path is failure-aware. Stale provider records, unavailable stores,
origin cooldowns, range requests, and manifest prefetching can alter individual
steps without changing the source preference.

### HEAD and Range Requests

`HEAD` is metadata-only. It does not initiate cold-start coordination or count
as a completed pull.

Blob requests with an open-ended range use origin range handling when needed.
The peer transfer endpoint implements `HEAD` and RFC 7233 range response
semantics, including `Content-Range` and the transmitted `Content-Length`.

## Local Storage

Gantry has no separate cache directory. The production store is the local
containerd content store in the configured namespace, normally `k8s.io`.

The store treats content as available only when it can open the content, not
merely when metadata exists. This avoids advertising or serving incomplete
containerd ingests.

### Background Ingest Leases

Content pulled in the background is protected by bounded containerd leases.
Gantry-created leases carry labels describing their manager, source,
repository, digest, and creation time. A cleanup loop removes expired
Gantry-managed leases. Environments without a containerd lease manager return
an explicit error rather than silently proceeding without protection.

Containerd remains responsible for its normal content lifecycle and garbage
collection after the Gantry lease expires.

## Discovery and Advertising

Each agent runs a persisted libp2p identity and a Kademlia DHT in server mode.
Configured static bootstrap peers can seed initial connectivity, while chair
holder addresses provide the dynamic production bootstrap set.

To advertise content, Gantry converts its digest to a DHT provider key and
publishes a provider record. Provider lookup returns dialable, IP-based peer
addresses with the configured transfer port.

The advertiser continuously reconciles containerd inventory with provider
announcements. If the local store reports itself unavailable, reconciliation
pauses instead of treating every digest as deleted.

Discovery owns a background bootstrap and self-test loop. Its health monitors
contribute to a geometric-mean DHT health score exposed through metrics.

## Peer Transfer and Integrity

The transfer server reads only from the local content store. It rejects tag
references and returns:

- `404 Not Found` when valid digest content is absent.
- `503 Service Unavailable` when the local store itself is unavailable.
- Standard range and metadata responses for supported requests.

Content received from a peer is committed through a digest-verifying pipeline.
The requested OCI digest remains the content identity regardless of which peer
served the bytes. Containerd independently verifies content before using it.

Peer transfer is cleartext HTTP/2 by design. Deployments that require
on-the-wire confidentiality must add a trusted network, service mesh, or
equivalent transport protection.

## Cold-Start Coordination

A cold start occurs when no usable provider advertises a requested digest.
Gantry avoids letting every requester contact the origin:

1. A configured set of stable chair names is represented by Kubernetes
   Leases.
2. Agents claim and renew empty chairs, with at most one chair held by an
   agent.
3. Every requester ranks the chair set deterministically for the digest.
4. A bounded top-ranked cohort receives `please_pull` requests.
5. Selected holders deduplicate work locally, fetch from the origin, commit
   the content, and advertise it.
6. Requesters return to provider discovery and fetch from an available seed.

The ranking changes across digests, spreading origin work across the active
chair pool without requiring a central scheduler. Chair rotation moves the
extra origin-serving duty over time.

Cold-start coordination favors liveness during failures. A partition or
unreachable cohort may cause an additional origin pull, but it must not make
image pulling depend permanently on consensus or one process.

## Authentication

Authentication is request-scoped by default:

1. Gantry probes an HTTPS origin when it needs an authentication challenge.
2. It relays supported challenges to containerd.
3. Containerd supplies authorization derived from the workload's pull
   credentials or kubelet credential provider.
4. Gantry forwards that authorization through cold-start coordination to the
   selected origin puller.
5. The authorization is discarded after the request.

Malformed or unsupported authorization fails explicitly. Gantry does not
persist delegated credentials and does not fall back to a selected puller's
identity after the origin rejects a request.

An optional shared-identity mode reads a configured credential file for
environments that cannot delegate requester credentials.

## Origin Failures and Fallback

The negative cache is local to each agent. Terminal origin failures enter an
exponential cooldown; successful completion clears the entry. Local caching
prevents one failing origin from being hammered repeatedly without turning the
cache into a cluster-wide correctness dependency.

Direct-origin fallback is a last-resort liveness path. It is gated behind peer
lookup and cold-start exhaustion and is observable through
`p2p_origin_fallback_total`.

## Failure Behavior

| Failure | Expected behavior |
| --- | --- |
| Local containerd unavailable | Readiness fails; transfer returns 503; advertising pauses |
| DHT has no provider | Invoke cold-start chair coordination |
| Provider record is stale | Record peer miss, filter or rediscover, then continue resolution |
| Selected chair is unavailable | Continue the bounded chair cascade, then use gated fallback if necessary |
| Origin returns terminal failure | Enter local negative-cache cooldown |
| Peer returns corrupt content | Digest verification rejects the transfer |
| DHT is degraded | Readiness and health expose the condition; cold-start and fallback preserve liveness |
| Network is partitioned | Partitions may perform redundant origin work rather than block indefinitely |
| Agent is draining | Stop accepting new mirror work and finish bounded shutdown |

## Shutdown

Signal-driven shutdown marks the mirror draining before stopping dependent
background work. Gantry then stops content subscription, advertisement,
coordination, transfer, operations listeners, discovery self-tests, the DHT,
and the libp2p host in dependency-aware order.

The standalone node configurator removes its containerd route during graceful
termination only when the file still matches the chart-owned payload.

## Observability

The operations endpoint exports:

- Process and Go runtime metrics.
- Local cache, peer fetch, origin pull, and fallback outcomes.
- Bytes served by source and content kind.
- DHT health and connection state.
- Chair claims, coordination requests, and outcomes.
- Containerd lease creation and cleanup.
- Provider advertisement reconciliation.
- Storage-mode identity.

Use labels and counters to determine which node handled a specific phase.
Origin, peer, and mirror counters are intentionally node-local, so a single
sampled pod is not a cluster-wide view.

## Implementation Map

| Area | Source |
| --- | --- |
| Runtime composition | `cmd/gantry` |
| Configuration | `internal/gantry/config` |
| Mirror and fallback | `internal/gantry/mirror` |
| Containerd storage | `internal/gantry/containerdstore` |
| Discovery and advertising | `internal/gantry/discovery`, `internal/gantry/advertise` |
| Peer transfer | `internal/gantry/transfer`, `internal/gantry/digestpipe` |
| Chairs and cold start | `internal/gantry/chairs`, `internal/gantry/coldstart` |
| Coordination | `internal/gantry/coord`, `internal/gantry/chaircall` |
| Origin and authentication | `internal/gantry/origin`, `internal/gantry/registryauth` |
| Helm installation | `deploy/gantry/chart` |
