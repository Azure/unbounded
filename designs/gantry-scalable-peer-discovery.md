# Scalable Peer Discovery for Gantry

**Status:** Draft for discussion

## Summary

Gantry speeds up container image pulls in Kubernetes. It runs on every node,
keeps image content in containerd, the node's container runtime, and lets other
nodes fetch that content directly. When no node has the requested content yet,
Gantry coordinates which node should fetch it from the origin registry. The
origin is the remote registry where the image was published.

Today, every Gantry agent learns about every other Gantry Pod. That complete
membership view is used both to find initial peers and to choose the node that
fetches new content. This works at smaller scales, but it makes every agent
process cluster-wide Pod and Node state.

This design removes that complete membership view:

- A fixed set of Kubernetes Lease objects gives a new agent its first few peer
  addresses.
- After the first connection, a distributed hash table (DHT) built with the
   libp2p peer-to-peer networking library handles peer and content discovery.
- If content already exists in the cluster, Gantry fetches it from a peer that
  advertises it.
- If the content is new, Gantry asks the DHT for peers closest to the content's
  digest and chooses a puller from that small set.

There is no dedicated bootstrap service, leader, tracker, or special class of
Gantry node. Ordinary agents temporarily publish themselves through Lease
slots, and all agents participate equally in the DHT.

This is proposed behavior, not the current implementation. Details such as the
Lease schema, retry loops, metric names, configuration fields, and code changes
are intentionally left for an implementation design.

## Background

### What Gantry does

A container image is made of immutable objects: a manifest, a configuration,
and one or more layers. Each object is identified by a cryptographic digest.
Gantry handles each digest independently.

When containerd asks Gantry for a digest, one of three things happens:

1. **Local hit:** the object is already on this node, so Gantry serves it
   immediately.
2. **Peer hit:** another node has advertised the object, so Gantry fetches it
   from that peer.
3. **Cold miss:** no node has advertised the object, so one node must fetch it
   from the origin registry.

The first two cases are content discovery. The third also requires
coordination: thousands of nodes may request the same new layer at once, but
they should not all fetch it from the origin.

### Why membership is the scaling problem

The current design gives each agent a complete view of Gantry Pods and their
Kubernetes Nodes. At a cluster size of `N`, a simultaneous startup can deliver
approximately:

```text
N agents * N Pod records = N^2 Pod-record deliveries
```

For 100,000 agents, that arithmetic is 10,000,000,000 Pod-record deliveries.
This is a derived upper bound, not a measured API-server result. It illustrates
why complete membership is the wrong dependency at the target scale.

Gantry does not otherwise need to know every node. It needs answers to two
smaller questions:

1. When I start, what is one live peer I can contact?
2. For this digest, which peers either have the content or can fetch it?

The proposed design answers those questions directly.

## Goals

- Remove Gantry's Pod and Node list/watch dependencies.
- Keep per-agent Kubernetes discovery work bounded as the cluster grows.
- Preserve peer-to-peer delivery for content already present in the cluster.
- Coordinate cold origin pulls without a complete membership list.
- Avoid dedicated bootstrap nodes and centralized Gantry services.
- Allow established peers to keep operating during Kubernetes API outages.
- Support a cold cluster in which no Gantry agent has joined the DHT yet.

## Non-goals

- Eliminating Kubernetes from initial peer discovery.
- Guaranteeing exactly one origin pull during network partitions or while DHT
  views disagree.
- Choosing production Lease counts, timeouts, or rate limits without scale
  measurements.
- Designing zone-local peer selection in the first version.
- Replacing the origin registry or changing Open Container Initiative (OCI)
   digest verification.

## Current and Proposed Lookup

A DHT is a peer-to-peer lookup system. Instead of keeping a central directory
or giving every agent a complete member list, each agent keeps a smaller
routing view. A lookup is forwarded through that view toward peers whose
identifiers are progressively closer to the lookup key.

Current Gantry uses two different mechanisms:

- **Warm content:** it uses the DHT to find peers that have advertised a
  particular digest.
- **Cold content:** if nobody has advertised the digest, it uses HRW hashing
  over the complete Kubernetes membership snapshot to order candidate pullers.

This proposal keeps DHT provider lookup for warm content. It replaces the HRW
cold path with a DHT closest-peer lookup, which finds a small, digest-specific
candidate set without complete Kubernetes membership.

The DHT can discover more peers after an agent has joined, but it cannot find
the first peer from an empty routing view. The Lease mechanism exists only to
bridge that first-contact gap.

## Proposed Design Overview

The design separates first contact from normal operation:

```text
                            Kubernetes API
                                  |
                         fixed Lease slots
                                  |
                                  v
                    +--------------------------+
                    | New Gantry agent         |
                    | learns one or more peers |
                    +------------+-------------+
                                 |
                                 v
                    +--------------------------+
                    | libp2p DHT               |
                    | peer and content lookup  |
                    +------+-------------------+
                           |
             +-------------+-------------+
             |                           |
             v                           v
    peer already has content    no peer has content
             |                           |
             v                           v
      fetch from peer          choose a nearby puller
                                         |
                                         v
                                chosen peer fetches from
                                origin, then serves peers
```

Kubernetes is used only to enter the peer network. It is not the membership
database for the peer network and is not involved in content lookup.

## Joining the Peer Network

### Lease slots as a rendezvous point

The proposed deployment precreates a fixed number of predictably named Lease
objects. Their names are part of the deployment configuration, so every agent
can read selected slots directly without first listing them. Think of the
slots as a small bulletin board: each occupied slot says, "this Gantry agent
was recently alive, and here is how to contact it."

Any ordinary Gantry agent may hold a slot. A holder refreshes its slot while it
is healthy. If the holder disappears and the slot expires, another agent may
take its place. The role is temporary and carries no authority over other
agents. Joining the DHT does not cause a holder to release its slot; healthy
holders continue refreshing slots so later agents have bootstrap contacts.

A joining agent:

1. Tries peers remembered from an earlier run, if any.
2. Reads a small, fixed sample of the configured Lease names.
3. Discards expired or malformed entries.
4. Dials one or more advertised peers.
5. Joins the DHT and learns additional peers through normal DHT traffic.

The agent uses exact reads of known object names. It does not list or watch
Leases, Pods, or Nodes.

### Forming a completely cold cluster

The first agent finds no live contact, claims an available Lease slot, and
starts a one-node DHT. Later agents discover it through the slot and join.
As more agents arrive, some of them claim other slots, spreading future join
traffic across multiple ordinary peers.

The number of slots is fixed by deployment configuration. It does not grow
with the number of nodes.

## Finding Content

Once an agent has joined the DHT, Lease slots are no longer part of the image
pull path.

### Content already exists: find a provider

An agent first asks the DHT for providers of the requested digest. A provider
is a peer that has explicitly advertised that it holds the content.

The DHT operation commonly called `FindProviders` may query remote peers; it is
not limited to the local routing table. If it returns usable providers, Gantry
fetches from one of them. This remains the normal warm-content path.

### Content is new: choose a cold puller

In current Gantry, a provider miss enters the HRW path described above. In this
proposal, a provider miss instead asks the DHT for peers whose identifiers are
closest to the digest. This is the `GetClosestPeers` operation.

Those peers do not necessarily have the content. Their purpose is to provide a
small, digest-specific candidate set without consulting complete cluster
membership. Under this proposal, Gantry includes itself in the same distance
ordering, checks which candidates are reachable and available, and asks the
closest usable candidate to fetch the digest from the origin.

Requesters with the same DHT view get the same distance order, which gives them
a common first choice without complete membership. Different digests map to
different parts of the peer-ID space, so origin work is spread among peers
rather than assigned to one permanent node. Requesters with different DHT
views may still choose different pullers.

Provider lookup does not return the routing peers it encountered along the
way. Under this proposal, a cold miss therefore performs a second DHT lookup to
find closest peers. That extra traversal is accepted initially and must be
measured.

When that pull completes, the puller advertises itself as a provider. Waiting
requesters then discover the provider and fetch the content peer to peer.

```text
request digest
      |
      v
local content? ---- yes ---> serve locally
      |
      no
      v
find providers ---- found --> fetch from provider
      |
      none
      v
find closest peers
      |
      v
choose reachable puller
      |
      v
puller fetches origin and advertises
      |
      v
requesters fetch from puller
```

### Why HRW is removed

The current cold-puller algorithm uses highest-random-weight (HRW) hashing.
HRW gives strong agreement only when requesters use the same complete candidate
set. Maintaining that shared set is the reason Gantry watches every Pod and
Node.

Running HRW over each agent's partial DHT view would keep the algorithm's name
but lose its agreement property. This design instead uses the DHT's native
closest-peer result and explicitly accepts that different requesters may
occasionally choose different pullers. The result may be extra origin traffic,
but it cannot change the bytes accepted by containerd because every object is
still verified against its digest.

## Failure Behavior

The design favors continued image pulls over strict single-puller agreement.

| Situation | Expected behavior |
| --- | --- |
| Kubernetes API becomes unavailable after an agent joins | Existing peers continue DHT lookup, coordination, and transfer. |
| A new agent has cached peer addresses but the API is unavailable | It tries the cached peers and can join if any are reachable. |
| A brand-new agent has no cached peers and the API is unavailable | It cannot join until the API or another configured contact becomes available. |
| A Lease points to a dead peer | The joiner tries other sampled contacts; the stale slot is replaced after expiry. |
| The selected cold puller fails | The requester tries another candidate or eventually uses controlled origin fallback. |
| DHT routing is degraded | Provider and closest-peer lookups may both fail because they share the same network. |
| DHT views disagree or the network partitions | More than one peer may pull the same digest from origin. Each partition can still make progress. |

If neither DHT lookup produces a usable result, Gantry retains a guarded direct
origin fallback. That fallback must be jittered, rate limited, deduplicated
within the node, and preceded by a final provider check. It protects liveness;
it does not recreate cluster-wide single-puller coordination.

Duplicate origin pulls waste bandwidth but do not weaken content integrity.
OCI content remains identified and verified by digest regardless of which peer
fetched it.

## Kubernetes Dependencies

The target design makes no Gantry API calls to list, watch, or patch Pods and
makes no Node API calls.

| Current need | Replacement |
| --- | --- |
| Discover all Gantry Pods | Discover a bounded set of initial contacts through Lease slots |
| Publish addresses in Pod annotations | Publish contact information in a held Lease slot |
| Use Kubernetes node names as peer identity | Use the persistent libp2p peer identity directly |
| Read Node labels for zone-aware selection | Not supported in the first version |
| Size behavior from exact cluster membership | Use explicit bounded configuration and observed DHT health |

The agent still receives its own Pod IP through the Kubernetes Downward API.
That is local environment injection by kubelet, not a Pod or Node API request
from Gantry.

## Scale Model

Let:

- `S` be the fixed number of Lease slots.
- `K` be the number of slots read during a normal join.
- `T` be the Lease renewal period.

Then:

- Kubernetes stores `S` rendezvous objects, independent of cluster size.
- A normal join performs `K` exact Lease reads and a bounded number of dials.
- Steady-state Lease renewal traffic is approximately `S / T` writes per
  second, plus conflicts and retries.
- A bounded fallback may read all `S` slots, but it still does not read all
  Gantry Pods.

These are derived relationships, not measured costs. The values of `S`, `K`,
and `T` must come from API-server, convergence, and connection-fan-in
measurements. No values are selected in this document.

The DHT gives each agent a routing view rather than a complete membership list.
Its memory, network traffic, lookup latency, and convergence at 100,000 nodes
must be measured before rollout.

## Identity and Trust

Each Gantry agent has a persistent libp2p key. The resulting peer ID is its
network identity and remains stable across Pod restarts even when the Pod IP
changes.

The libp2p secure channel proves that a peer owns the key for its peer ID. It
does not, by itself, prove that the peer belongs to this Gantry cluster. Lease
records are discovery hints, not authorization records.

The implementation therefore needs an explicit cluster-admission mechanism,
such as a private libp2p network key or another managed allowlist. Key
distribution, rotation, and mixed-version rollout remain separate security
decisions.

## Readiness and Operations

In clustered mode, an agent should become ready only after it has a dialable
address and has either joined another DHT peer or completed the defined bounded
bootstrap policy. A deliberate single-node mode may treat an empty routing
table as healthy.

Operators need visibility into:

- Lease reads, claims, renewals, conflicts, and stale entries.
- Time from process start to first DHT peer.
- Routing-table health and peer churn.
- Provider and closest-peer lookup latency and failures.
- Cold-puller selection and failover.
- Direct-origin fallback and duplicate origin pulls.
- Inbound connection concentration on Lease holders.
