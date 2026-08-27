# Gantry Lease Rendezvous and DHT Puller Selection

**Status:** Experimental implementation - requires fault and scale validation
**Date:** 2026-08-25
**Scope:** Membership-free bootstrap and puller selection for Gantry clusters up to 100,000 nodes

---

## 1. Summary

This document defines the preferred direction for removing Gantry's Kubernetes
Pod membership informer and its remaining Node API dependency:

1. Fixed `coordination.k8s.io/v1` Lease slots provide bounded first contact.
2. libp2p peer IDs become Gantry's native node identities.
3. `FindProviders` remains the warm-content discovery path.
4. `GetClosestPeers` replaces HRW for cold-puller selection.
5. Gantry maintains no complete Pod or Node membership view.

The proposal uses the Kubernetes API only to answer one question for a joining
agent:

> What are a few recently active Gantry peers that I can contact?

After the first connection, libp2p Kademlia discovers additional peers and
handles steady-state routing. Lease holders are ordinary Gantry agents. There
is no dedicated bootstrap deployment, permanent leader, tracker, or Gantry
control-plane service.

This direction deliberately gives up HRW's stronger agreement properties. A
cold miss may perform two DHT traversals, and requesters with different DHT
views may select different origin pullers. Occasional duplicate origin pulls
during churn, degraded routing, or partitions are accepted; their observed
rate still requires validation.

The repository now implements this direction as the only first-contact path.
The Pod and Node informers, HRW selection, Pod self-announcement, and their
RBAC have been removed outright rather than retained behind a mode switch: the
membership path is what fails to scale, so keeping it selectable would preserve
the cost it was removed to avoid. Discovery uses native libp2p peer IDs,
fixed-name Lease GET/update operations, a persisted bounded peer cache,
`FindProviders` for warm content, and `GetClosestPeers` for cold-puller
selection.

Upgrading an existing cluster is a direct cutover rather than a staged
migration. Agents running the previous release and agents running this one do
not share a discovery mechanism, so during the rollout an agent may find no
peer for a given layer. That agent falls back to pulling from the origin
registry, which is the same behavior Gantry already produces whenever peer
selection is exhausted. Accepting a bounded increase in origin pull rate for
the duration of the rollout is deliberate: it removes the need for a
compatibility mode whose only purpose was to keep the Pod informer alive.

The implementation remains experimental. The rendered values (`S=64`, `K=8`,
`C=4`, `M=8`, 90-second leases, and 30-second renewal) are validation defaults,
not measured production recommendations. Membership-backed coordination
authorization has been removed; the libp2p identity handshake is the only peer
identity check until private-network PSK distribution and rotation are
designed. Zone-scoped selection is not supported. The fault
and scale measurements in section 16 are still required before qualification.

The current implementation uses the existing randomized parallel bootstrap
cascade, capped at 32 unique peer dials per pass. Clustered readiness requires
a non-empty routing table and an immediate successful DHT
`Provide(self)->FindProviders(self)` self-test. A successful persisted-cache
connection skips Lease discovery, but the agent still performs one bounded
claim pass so ordinary agents continue rotating through the fixed slots. A
transport/API failure retries that pass with capped jittered backoff; an
occupied pass is complete and stops, so this does not create steady-state
claim traffic.

`gantry.io/bootstrap-sample` is parsed and bounded when present, but holders do
not publish it yet. The sample source, bias controls, and refresh cadence remain
open decisions 4 and 5; the required primary holder address is always
published. This omission does not change the fixed-slot first-contact path.

## 2. Motivation

The former membership path gave every Gantry agent a view of every Gantry
Pod. At cluster size `N`, an initial list delivers `N` Pod records to each of
`N` agents. The aggregate object delivery and local processing are therefore:

```text
N agents * N Pod records = O(N^2)
```

At 100,000 agents, the arithmetic upper bound is 10,000,000,000 delivered Pod
records during a simultaneous cold start. This is derived arithmetic, not a
measured API-server load result.

A DHT removes the need for complete local membership after an agent has joined,
but it cannot solve first contact. A new agent with an empty routing table has
no remote peer to query. The system therefore needs a bounded source of initial
peer IDs and addresses.

The rendezvous mechanism must satisfy these properties:

1. Per-agent discovery work is independent of total cluster size.
2. Stored rendezvous state is independent of total cluster size.
3. There is no special Gantry node type or permanent leader.
4. Existing DHT participants continue operating during Kubernetes API outages.
5. A cold cluster can form a DHT without waiting for Gantry Pods to become
   Ready.
6. Joining agents learn both a libp2p peer ID and a dialable address.

## 3. Non-goals

- Replacing the Kubernetes API or accessing etcd directly.
- Storing complete Gantry membership in Lease objects.
- Using Leases for layer placement, content-provider records, origin-pull
  coordination, or peer transfer.
- Selecting concrete slot counts, lease durations, or API rate limits without
  measurements.
- Preserving zone-scoped selection in the first version. Zone behavior remains
  an open design item.
- Preserving HRW's complete-membership calculation or its stronger guarantee
  that requesters with identical membership select one puller.
- Guaranteeing exactly one origin pull while DHT views disagree or the network
  is partitioned.
- Making the DHT available without any first-contact mechanism. That is not
  possible when a peer starts with an empty routing table.

## 4. Current implementation anchors

The implementation is owned by these paths:

| Concern | Current implementation |
| --- | --- |
| Fixed-slot read, claim, and renewal | `internal/gantry/rendezvous/manager.go` |
| Cache and Lease bootstrap loop | `internal/gantry/rendezvous/bootstrap.go` |
| Advertised address rewriting | `internal/gantry/address/factory.go` |
| DHT host and closest-peer lookup | `internal/gantry/discovery/discovery.go` |
| Cold-start candidate selection | `internal/gantry/coldstart/coldstart.go` |
| Closest-peer prefetch grouping | `internal/gantry/coldstart/prefetch.go` |
| Agent wiring and readiness | `cmd/gantry/main.go` |
| Fixed Lease manifests and RBAC | `deploy/gantry/rendezvous-leases.yaml.tmpl`, `deploy/gantry/serviceaccount.yaml.tmpl` |
| Create-if-absent and stale-slot cleanup | `internal/operator/components/gantry/gantry.go` |

## 5. Design overview

The selected architecture assigns one purpose to each mechanism:

| Mechanism | Purpose |
| --- | --- |
| Local content store | Serve content already present on this node |
| Kubernetes Lease slots | Supply a bounded set of initial peer IDs and addresses |
| Kademlia routing table | Route queries without complete membership |
| `FindProviders` | Return peers that advertised possession of a digest |
| `GetClosestPeers` | Return candidate cold pullers when no provider exists |
| Pull-intent and please-pull RPCs | Observe candidate state and request an origin pull |

Lease slots are not used for layer placement. `GetClosestPeers` does not replace
`FindProviders`: closest DHT peers are routing peers and do not necessarily hold
the requested content.

### 5.1 Fixed rendezvous slots

Deployment creates a fixed number `S` of predictably named Lease objects in the
Gantry namespace:

```text
gantry-rendezvous-0000
gantry-rendezvous-0001
...
gantry-rendezvous-(S-1)
```

`S` is a deployment parameter. It does not grow with the number of Gantry
agents. No value for `S` is proposed here.

Each slot may be held by one ordinary Gantry agent. A holder periodically
renews the Lease and publishes a bounded contact bundle containing its own
libp2p address and, optionally, a small sample of other peers from its DHT
routing view.

Precreating the Lease objects allows Gantry RBAC to omit `create`. It also makes
the complete key space known without a list operation.

### 5.2 Lease representation

The standard Lease fields carry ownership and freshness:

```yaml
apiVersion: coordination.k8s.io/v1
kind: Lease
metadata:
  name: gantry-rendezvous-0007
  namespace: unbounded-system
  annotations:
    gantry.io/p2p-addrs: >-
      /ip4/10.42.1.7/tcp/4001/p2p/12D3Koo...
    gantry.io/bootstrap-sample: "[...]"
spec:
  holderIdentity: "12D3Koo..."
  leaseDurationSeconds: 90
  renewTime: "2026-08-25T12:00:00Z"
```

The example duration is illustrative only.

`holderIdentity` is the holder's libp2p peer ID. `gantry.io/p2p-addrs`
contains one or more complete multiaddrs ending in `/p2p/<peer-id>`.
`gantry.io/bootstrap-sample`, if used, is a versioned, bounded JSON payload of
additional peer IDs and multiaddrs.

Lease freshness proves only that the holder recently reached the Kubernetes
API. It does not prove that the holder's libp2p listener is reachable. Joiners
must try multiple contacts and treat dial failure as normal.

### 5.3 Native peer identity

The membership-free path uses the libp2p peer ID as Gantry's remote node
identity. It does not translate a Kubernetes node name into a peer ID.

The local peer ID continues to come from Gantry's persisted Ed25519 identity.
The existing host path preserves that identity across Pod replacement on the
same Kubernetes node.

Remote transfer addresses are derived from peerstore multiaddrs plus the
cluster-wide transfer port, matching the existing DHT provider conversion.

### 5.4 Address advertisement

The current implementation rewrites wildcard listeners to the Downward API
Pod IP and publishes the result in Pod annotations. Without Pod annotations,
the libp2p host must advertise the rewritten address through its Identify and
DHT address path.

The proposed implementation adds an address factory that:

1. Reads the Pod IP from the existing Downward API environment variable.
2. Rewrites `/ip4/0.0.0.0/...` or `/ip6/::/...` to the same-family Pod IP.
3. Rejects loopback, link-local, malformed, and cross-family addresses.
4. Publishes the resulting address through libp2p.

This uses Pod metadata injected by kubelet. It does not call the Kubernetes
API and does not require an informer.

## 6. Agent protocol

### 6.1 Startup order

A Gantry agent performs these steps:

1. Load or create its persistent libp2p identity.
2. Start the libp2p listener and Kademlia DHT server.
3. Advertise dialable Pod-IP multiaddrs through libp2p.
4. Try a bounded peer cache persisted on the host, if present.
5. If no cached peer connects, read a bounded selection of rendezvous slots by
   exact object name.
6. Validate Lease freshness and parse the contact bundles.
7. Randomize the fresh contacts and dial a bounded number in parallel.
8. Attempt to claim one empty or expired slot if this agent does not already
   hold one.
9. Retry discovery and claim operations with jittered backoff until at least
   one peer connects, or accept an empty routing table in explicit single-node
   mode.
10. Run normal DHT bootstrap and routing-table refresh.

After the routing table gains a peer, the agent performs an immediate DHT
self-test before reporting clustered readiness. Normal discovery and claim
traffic then stops; only agents that hold a slot continue bounded renewal
writes.

The agent does not issue a Pod `List`, Pod `Watch`, Lease `List`, or Lease
`Watch` in the normal path.

### 6.2 Reading slots

Let `K` be the maximum number of slot GETs in one discovery round. The agent
derives a pseudorandom slot permutation from its peer ID and a retry-round
nonce, then directly GETs the first `K` names.

Changing the retry-round nonce prevents a peer from repeatedly sampling the
same unavailable holders. Startup jitter prevents all joining agents from
issuing the same request pattern at once.

If repeated bounded samples find no contact, the implementation may perform a
bounded full scan of all `S` slots before declaring rendezvous unavailable.
Because `S` is fixed independently of cluster size, this fallback remains
`O(1)` with respect to `N`, but its API cost still requires measurement.

### 6.3 Claiming a slot

An agent ranks slot names by a stable hash of `(peerID, slotName)`. It examines
a bounded prefix of that ordering and attempts to claim the first empty or
expired slot.

Claim uses a normal Lease update with the observed `resourceVersion`.
Concurrent claimers therefore cannot both commit the same version. A conflict
causes the loser to select another candidate or back off; it does not retry in
a tight loop.

An agent holds at most one slot. If it reads a fresh slot already containing
its peer ID, it resumes renewal instead of claiming another slot.

### 6.4 Renewing a slot

Only current slot holders renew. A holder updates `renewTime` before
`leaseDurationSeconds` elapses and refreshes its advertised addresses when they
change.

The optional bootstrap sample is refreshed less frequently than `renewTime`.
It contains a bounded set of peers selected from the holder's current DHT or
libp2p connection view. This gives a joiner alternatives when the Lease holder
is API-live but not network-reachable and preserves useful contacts briefly
after a holder fails.

The sample does not assert membership or health. Every address is an
untrusted dial hint until the libp2p identity handshake succeeds.

### 6.5 Releasing and replacing slots

On graceful shutdown, a holder may clear its Lease, but correctness cannot
depend on graceful release. After abrupt failure, the slot becomes claimable
when its duration expires.

A joining agent that observes an expired slot may claim it after establishing
its listener. No controller assigns replacements. If no agents are joining,
temporary slot loss does not interrupt an already-connected DHT.

Stale contact bundles may be tried for a bounded grace period because a Lease
holder can lose API access while its libp2p listener remains healthy. The
freshness policy and stale grace require explicit configuration and tests.

## 7. Cold-cluster formation

On a first deployment or complete host replacement, all slots begin empty or
expired:

1. Agents start listeners before touching Lease state.
2. Agents jitter their first API operation.
3. Concurrent agents sample and conditionally claim different slots.
4. The first successful holder has no remote peer yet, which is valid.
5. Later agents read occupied slots and dial those holders.
6. Agents that claimed different slots resample after backoff and connect the
   initially separate components.
7. Kademlia queries propagate additional peer IDs and addresses.

There is no readiness dependency on another Gantry Pod becoming Ready. Lease
advertisement begins when the local libp2p listener has a dialable address.

Cold-cluster convergence is probabilistic while agents use sampled slot reads.
The bounded full-scan fallback provides a deterministic way to find every
published slot, subject to API availability and network reachability.

## 8. Scale model

Define:

- `N`: Gantry agent count.
- `S`: fixed Lease slot count.
- `K`: slot GETs per discovery round.
- `C`: maximum conditional claim attempts per joining agent.
- `D`: peer dials attempted by each joining agent.
- `T`: Lease renewal period in seconds.
- `M`: contacts advertised per occupied slot, including the holder.

### 8.1 Stored control-plane state

```text
Lease objects = S = O(1) with respect to N
```

### 8.2 Normal join work

```text
API reads per discovery round <= K
claim updates per joining agent <= C
peer dials per joining agent <= D
```

These are all bounded independently of `N`.

### 8.3 Simultaneous cold start

Ignoring retries, the aggregate operation bounds are:

```text
GET operations <= N * K
claim update attempts <= N * C
peer dial attempts <= N * D
```

This is `O(N)` for fixed `K`, `C`, and `D`. It does not establish that a given
Kubernetes API server can absorb the resulting burst. QPS, response latency,
conflict rate, and recovery time must be measured.

### 8.4 Steady renewal traffic

With all slots occupied:

```text
renewal writes per second approximately S / T
```

This is derived arithmetic. Actual writes include retries, address changes,
and contact-sample refreshes.

### 8.5 Bootstrap connection fan-in

If joiners choose uniformly among the advertised contacts, average inbound
dials per advertised contact during a simultaneous join are approximately:

```text
(N * D) / (S * M)
```

This average does not bound the maximum. Hash skew, correlated retries,
unreachable contacts, and connection lifetime can produce larger hot spots.

No concrete values for `S`, `K`, `C`, `D`, `T`, or `M` should be selected from
these formulas alone.

## 9. Warm discovery and cold-puller selection

Lease rendezvous solves first contact only. Per-digest routing uses two distinct
Kademlia operations.

### 9.1 Warm path: `FindProviders`

After a local content-store miss, Gantry calls `FindProviders(digest)`.
`FindProviders` first checks the local DHT provider store. Unless that store
already satisfies the requested provider count, the operation performs an
iterative network lookup. Remote DHT peers return both provider records and
closer routing peers.

The public `FindProviders` result contains only peers that advertised the
digest. The closer peers encountered while routing the query are consumed
internally and are not returned to Gantry.

If providers are returned, Gantry randomizes the usable provider order and
attempts peer transfer. This preserves the existing warm-content behavior.

### 9.2 Cold path: `GetClosestPeers`

If `FindProviders` returns no usable provider, Gantry selects candidate cold
pullers as follows:

1. Call `GetClosestPeers(ctx, digest.String())`.
2. Treat the returned libp2p peer IDs as candidates ordered by Kademlia XOR
  distance.
3. Compute self's distance to the same target and merge self into the ordered
  result. An empty or one-node DHT does not return self.
4. Slice the result to the configured probe size.
5. Probe pull intent in parallel.
6. Choose the lowest-distance reachable candidate in the requester's result
  for `please_pull`.
7. Poll `FindProviders` until the puller advertises the completed digest or the
  existing stall policy expires.

The distance order is deterministic for a given candidate set, but there is no
claim that all requesters have the same candidate set. Different routing views
may select different pullers and produce duplicate origin pulls. This is an
accepted reduction in coordination strength compared with HRW.

Randomization remains appropriate for transfer attempts among peers already
known to cache the digest. It is not applied to cold-puller ordering because it
would introduce disagreement even when requesters received identical closest-
peer results.

### 9.3 Accepted DHT coupling and duplicate traversal

`FindProviders` and `GetClosestPeers` both use the Kademlia routing network.
`FindProviders` internally encounters closest peers, but its public API does
not expose them. With the current libp2p API, a cold miss therefore starts a
separate `GetClosestPeers` lookup. The second lookup may reuse connections,
peerstore addresses, and routing state learned by the first, but it remains a
separate network operation.

This duplicate traversal is accepted for the initial design. A future combined
provider-or-closest API could remove it without changing the higher-level
selection semantics.

The new cold path is not independent of provider discovery. If DHT routing is
degraded, both `FindProviders` and `GetClosestPeers` may fail or return partial
results. Gantry then reaches its controlled direct-origin fallback without an
HRW probe. The fallback must retain jitter, rate limiting, local in-flight
deduplication, and a final provider recheck, but it no longer offers a strict
cluster-wide origin-pull bound.

## 10. Removing Pod and Node informer dependencies

Removing the informer requires replacements beyond initial bootstrap:

| Current dependency | Proposed replacement |
| --- | --- |
| HRW candidate set from all Pods | DHT `GetClosestPeers` plus self |
| Pod annotation bootstrap addresses | Fixed Lease rendezvous slots |
| Kubernetes node name to peer ID | Native libp2p peer ID identity |
| Transfer address from Pod snapshot | Peerstore IP plus configured transfer port |
| Wildcard address rewrite in Pod annotation | libp2p address factory using Downward API Pod IP |
| Informer synchronization readiness | Listener, rendezvous, routing-table, and DHT self-test readiness |
| Routing-table target from Pod count | Configured minimum or bounded health threshold |
| NF5 jitter from exact Pod count | Fixed capped jitter or separately configured upper bound |
| Zone metadata from Pod annotation and local Node GET | Not supported initially; requires a separate design |
| Coord authorization against Pod membership | Private-network or other explicit trust mechanism |

The exact cluster size becomes unknown by design. Features that currently use
that count must not silently substitute routing-table size as if it were exact
membership.

The target architecture performs no Pod list, Pod watch, Pod patch, Node list,
Node watch, or Node get. The Pod IP still comes from the Downward API, which is
environment injection by kubelet rather than a Gantry Kubernetes API request.
Removing the Pod informer also removes HRW itself rather than attempting to run
HRW over each peer's partial and different DHT routing table.

## 11. Readiness and degraded operation

Readiness should distinguish explicit deployment modes:

- **Single-node mode:** an empty DHT routing table is valid; local intent and
  local pull remain available.
- **Clustered mode:** the local listener must have at least one dialable
  advertised address. Readiness additionally requires either a connected DHT
  peer or a successful bounded bootstrap policy, as defined during
  implementation.

Kubernetes API behavior:

- Existing connected agents continue DHT lookup, coordination, and transfer
  when the API is unavailable.
- A restarting agent first tries its persisted peer cache.
- A brand-new agent with no reachable cached peer cannot join while both the
  rendezvous API and all configured alternative contacts are unavailable.
- Lease renewal failure does not immediately disconnect a holder from libp2p.
  It only makes the published hint stale.

DHT behavior:

- Provider lookup and closest-peer lookup failures are correlated because both
  operations use the same routing substrate.
- A partial lookup may still yield usable providers or cold-puller candidates.
- If neither lookup produces a usable result, Gantry uses controlled direct-
  origin fallback; there is no membership-based HRW recovery path.
- Duplicate origin pulls during routing disagreement are observable behavior,
  not a content-integrity failure, because every completed object is verified
  against its digest.

The current static bootstrap dial is one-shot. An informer-free implementation
must retry peer-cache and Lease-derived contacts with capped, jittered backoff.

Readiness is evaluated as an ordered sequence of named gates, and the order is
part of the contract rather than an implementation detail. Several conditions
are typically unsatisfied at once during a rollout, and the first failing gate
supplies the reported reason, so the order decides which cause an operator is
shown. The peer-announcement gate therefore precedes the DHT-convergence gate:
early in a first rollout both are unsatisfied, and reporting an empty routing
table would misattribute a condition whose actual cause is that no peer has
published its addresses yet. Gate order is expressed as data and asserted by
test, because an incorrect order degrades diagnosis rather than failing
visibly.

## 12. Security model

The libp2p secure-channel handshake proves possession of the private key for
the advertised peer ID. It does not prove that the peer belongs to this Gantry
cluster.

The coordination server no longer has a complete membership view and therefore
does not perform cluster-membership authorization. Replacement options include:

1. A libp2p private network protected by a cluster PSK.
2. A separately managed peer allowlist.
3. NetworkPolicy restricting which workloads can reach Gantry peer ports.

The trust mechanism is unresolved. A private-network PSK was selected as the
working direction during discussion, but Gantry does not currently configure
one. Secret distribution, rotation, rollout compatibility, and compromise
recovery need their own design treatment.

Lease records are discovery hints, not authorization records. An identity
learned from a Lease must prove possession of its advertised libp2p private key,
but that handshake alone does not prove cluster membership.

Proposed namespace-scoped Lease permissions are:

```yaml
- apiGroups: ["coordination.k8s.io"]
  resources: ["leases"]
  resourceNames:
    - gantry-rendezvous-0000
    - gantry-rendezvous-0001
    # ... rendered fixed slot names
  verbs: ["get", "update"]
```

The implementation uses resource-version guarded `Update`. Precreated slots
avoid `create` and `delete`. Kubernetes
RBAC cannot restrict an update to only the caller's current Lease, so every
Gantry service-account token can modify any configured slot. The threat model
must account for a compromised Gantry agent poisoning or churning rendezvous
state.

## 13. Configuration surface

Names are placeholders until implementation review:

```yaml
rendezvous:
  namespace: unbounded-system
  slot_count: <measured value>
  reads_per_round: <measured value>
  claim_attempts_per_round: <measured value>
  contacts_per_slot: <measured value>
  lease_duration: <measured value>
  renew_interval: <measured value>
  stale_contact_grace: <measured value>
  retry_min: <measured value>
  retry_max: <measured value>
  single_node: false
```

The deployment renderer creates exactly `slot_count` Lease manifests and the
matching RBAC `resourceNames` entries.

The compiled-in defaults describe a single-node agent: `single_node` is true and
no rendezvous namespace is required, so a bare run with no ConfigMap and no
environment still starts. A clustered deployment is expressed entirely by the
rendered manifests, which force `single_node` off. That setting is injected as
an environment variable rather than left to the ConfigMap because it grants
readiness before any peer has been dialed, and the operator retains an existing
ConfigMap rather than replacing it.

## 14. Observability

At minimum, the implementation should expose:

- `gantry_rendezvous_slot_get_total{outcome}`
- `gantry_rendezvous_slot_claim_total{outcome}`
- `gantry_rendezvous_slot_renew_total{outcome}`
- `gantry_rendezvous_contacts_total{freshness}`
- `gantry_rendezvous_dial_total{outcome,source}`
- `gantry_rendezvous_bootstrap_duration_seconds`
- `gantry_rendezvous_slot_held`
- `gantry_rendezvous_peer_cache_entries`

Required log fields include slot name, operation, conflict versus transport
failure, number of contacts considered, and routing-table size. Peer IDs and
addresses are operational metadata; logs must not include private keys, PSKs,
service-account tokens, or registry credentials.

## 15. Rollout compatibility

Old and new agents are not required to interoperate. The rollout is a direct
cutover:

1. Render and apply the manifests. This precreates the fixed Lease slots,
  grants slot-scoped `get`/`update`, and revokes the Pod and Node grants by
  applying an empty ClusterRole rule set.
2. Roll the DaemonSet.
3. Accept origin fallback for the duration of the roll. An agent that finds no
  peer for a layer pulls from the origin registry; this is the existing
  peer-selection-exhausted path, not a new failure mode.
4. Run fault and scale validation.

During the roll, agents on the previous release discover only each other, and
agents on this release discover only each other. Both populations change
monotonically, so the period of reduced peer hit rate is bounded by the
DaemonSet rollout duration. The observed origin pull rate during a cutover is
one of the measurements section 16 still requires.

The DaemonSet injects slot count, the NF5 jitter cap, and `single_node` through
environment variables so they override an operator-retained create-if-absent
ConfigMap. The operator creates slots only when absent and watches deletion
only, so renewals do not trigger reconciliation and holder state is never
applied over. It deletes labeled rendezvous slots outside the currently
rendered fixed key space, so reducing `slot_count` does not leave obsolete
control-plane state.

The config loader removes an exact allowlist of retired membership/HRW keys and
the retired `rendezvous.mode` key before strict YAML decoding. This lets the
operator-retained ConfigMap from the previous release survive the cutover while
preserving unknown-field errors for every key that is not an explicit migration
exception.

The coordination protocol carries an HRW rank field. With HRW removed the
field and its rank-mismatch telemetry are obsolete: the server stamps it -1
(unknown), and puller selection has always used
the requester's own ordering rather than the responder's report. The field
remains on the wire because removing it requires the protocol's normal
compatibility process.

## 16. Validation requirements

### 16.1 Deterministic and fake-client tests

- Empty-slot cold start with one, two, and many agents.
- Concurrent claim conflict permits exactly one committed holder per slot.
- Existing holder resumes its slot after Pod restart with persistent identity.
- Expired holder replacement.
- Address change is published on renewal.
- Malformed, oversized, stale, and mismatched peer records are rejected.
- Sampled reads eventually use the bounded full-scan fallback.
- Single-node mode remains functional with an empty routing table.
- Kubernetes API loss does not interrupt an established DHT.
- A brand-new peer remains NotReady when it has neither API nor cached peers.
- Readiness gates report the first failing reason in a fixed order; when peers
  are unannounced and the routing table is empty at the same time, the reported
  reason is the unannounced peers.
- A warm local hit performs no DHT operation.
- A remote warm hit uses `FindProviders` and does not run cold-puller selection.
- A provider miss runs `GetClosestPeers` and includes self in distance order.
- Identical closest-peer results produce the same local puller choice.
- Different closest-peer results may produce multiple successful origin pulls
  without serving unverified content.

### 16.2 Network fault tests

- Fresh Lease holder with an unreachable libp2p address.
- Stale holder with a reachable sampled contact.
- Partial DHT partition.
- `FindProviders` and `GetClosestPeers` both fail, exercising direct-origin
  fallback without HRW.
- All rendezvous holders fail while non-holder DHT peers survive.
- Full-cluster restart with persisted identities and changed Pod IPs.
- Full host replacement with no persisted peer cache.

### 16.3 Scale measurements

Measure, rather than infer:

- API GET and update QPS during staged and simultaneous joins.
- API latency and throttling.
- Lease update conflict rate.
- Time until the DHT has one peer and reaches a chosen routing threshold.
- Inbound connection distribution across advertised contacts.
- Slot-holder CPU, memory, file descriptor, and bandwidth use.
- Effect of `S`, `K`, `D`, `T`, and `M` on convergence and load.
- Per-digest RPC count and latency for warm `FindProviders` hits.
- Additional RPC count and latency from `GetClosestPeers` after provider miss.
- Origin-pull duplication when `GetClosestPeers` views disagree.

The current 5,003-node startup validation observed informer synchronization as
a startup bottleneck, but it does not measure Lease rendezvous. A separate
experiment is required before making claims about improvement.

## 17. Alternatives considered

### Complete Pod list or informer

Provides authoritative Kubernetes membership but has per-agent work
proportional to cluster size and aggregate `O(N^2)` object delivery during a
simultaneous initial list.

### Dedicated bootstrap peers

Operationally simple, but creates a special Gantry availability role. This
proposal instead rotates the role among ordinary agents through fixed slots.

### Kubernetes Service backed by every Gantry Pod

Avoids client-side Pod lists, but moves full membership into EndpointSlices
and cluster dataplane programming. It also does not directly provide the
expected libp2p peer ID for a secure first dial.

### Headless Service DNS

Returns Pod IPs but not the peer IDs expected by libp2p. Publishing peer IDs
would require an additional registry such as DNSAddr records.

### mDNS

Depends on multicast reachability and is not a reliable cross-node mechanism
for general Kubernetes CNI networks.

### DHT without rendezvous

Cannot work from an empty routing table because there is no peer to query.

### Direct etcd access

Rejected. Gantry should use supported Kubernetes APIs and authentication, not
couple itself to etcd topology, credentials, storage layout, or versioning.

## 18. Open decisions

1. What measured API and network limits determine `S`, `K`, `D`, `T`, and `M`?
2. Should normal discovery use exact GETs only, or a label-selected list bounded
   to the fixed `S` objects?
3. Is the bounded full-slot scan acceptable during cold-cluster fallback?
4. How are backup contacts selected and refreshed without biasing all slots
   toward the same DHT peers?
5. What stale-contact grace preserves API-outage tolerance without retaining
   unusable addresses too long?
6. What readiness threshold works without exact cluster size?
7. Resolved: a configured node-count upper bound sizes the jitter distribution,
  and `nf5_jitter_cap` provides a finite maximum delay.
8. Is a private-network PSK the cluster admission mechanism, and how is it
   rotated across 100,000 agents?
9. How is zone-local discovery represented if zone scoping is restored?
10. Resolved: no cross-mode discovery compatibility is required. The cutover
  accepts origin fallback during the roll, and only coordination wire
  compatibility is preserved. The resulting origin pull rate is measured under
  question 11.
11. What measured duplicate-origin rate is acceptable under churn and routing-
  table disagreement?
12. Should Gantry eventually add a combined provider-or-closest lookup to avoid
  the second DHT traversal on a cold miss?

Lease rendezvous plus DHT closest-peer cold selection is the preferred direction
for validation. It is now the explicit experimental Gantry path, not a
production-readiness claim; these questions and the required scale and fault
tests remain unresolved.

## 19. Implementation follow-ups

Section 18 lists questions the design leaves open. The items below were
surfaced while implementing it and concern the implementation rather than the
design. None of them block validation.

1. The operator issues one Lease `List` per reconcile to find slots outside the
  rendered fixed key space. This is deliberate and operator-scoped rather than
  per-agent, but it is a `List` in a design whose agent path avoids them.
2. The coordination protocol still carries the HRW rank field described in
  section 15. It is never stamped and never read for selection; removing it
  needs the protocol's normal compatibility process.

Three earlier follow-ups were closed by removing the membership path outright
rather than retaining it behind a mode switch:

- `ifaces.Members` no longer reaches the agent. Cold-puller selection takes a
  self identity, the coordination server no longer accepts a membership view,
  and the `internal/gantry/members` and `internal/gantry/hrw` packages are
  deleted. No test double is on the production path.
- `nf5_jitter_cap` has a single correct value. The `0` default existed only to
  preserve the uncapped membership-era behavior; with no exact cluster size to
  scale against, a finite cap is now the only meaningful setting.
- The compiled-in and rendered defaults no longer disagree about which
  discovery mechanism is in use, because there is only one.
