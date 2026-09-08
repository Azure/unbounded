**Gantry Deterministic Cold Seeding with Rotating Lease Chairs**

New Lease based cold start design with deterministic puller selection and doesnt need pod or node informers or watches
**Scope**

- Target: `100,000` Gantry nodes.
- `64` fixed Kubernetes Lease names are stable chairs.
- Each chair has one current Gantry holder.
- A holder is a final origin seed puller, not a coordinator that performs another DHT selection.
- Each node may hold at most one chair.
- Holder identity is its persistent libp2p peer ID.

**Per-Digest Selection**

```text
ranking = HRW(blob digest, 64 stable chair IDs)
top 8   = primary seed chairs
rest    = ordered backup chairs
```

- Every requester using the same snapshot gets the same ordering.
- Different digests normally select different primary sets.
- DHT is not involved in cold-puller selection.
- DHT remains responsible for discovering completed content.
- Backups replace the whole cohort, never an individual chair. A chair that
    fails to answer is not substituted, because it has usually started the pull
    anyway and a substitute would be an extra origin fetch rather than a
    replacement one.

**Lease State**

Each chair records:

```text
chair ID
holder peer ID
holder network address
holder generation
assignment epoch
renewTime
leaseDuration
next holder for planned rotation
```

The heartbeat renewal period and chair rotation period are separate.

**Snapshot Cache**

Every image pull performs a local check:

```text
if cachedSnapshot.epoch != currentAssignmentEpoch:
    refresh complete 64-chair snapshot through singleflight

use cachedSnapshot
```

- Every pull checks the epoch.
- Only the first stale pull performs the refresh.
- Concurrent pulls share one refresh.
- Gantry agents do not watch Leases. The operator watches chair deletions only
    so the fixed objects are recreated; heartbeat updates do not reconcile.
- No per-digest Lease reads.
- Nodes without pulls perform no snapshot refresh.
- A dial failure triggers an immediate targeted chair refresh, even if the global epoch is unchanged.
- If Kubernetes is unavailable, the old snapshot remains usable as dial hints, but no chair may be claimed.

**Initial Startup**

When all 64 Leases are empty:

1. Empty chairs are immediately claimable.
2. Nodes enter deterministic, jittered claim rounds.
3. A hash lottery limits how many of the 100,000 nodes participate per round.
4. Each eligible node chooses one chair and may claim only that chair.
5. Claim uses a Lease `resourceVersion` update.
6. One node wins each chair.
7. Eligibility widens in later rounds until enough chairs are occupied.
8. Cold selection becomes available once at least eight chairs exist.

**Cold Pull Path**

1. Check local content.
2. Query DHT for existing providers.
3. If a provider exists, peer-fetch from it.
4. If genuinely cold, rank the 64 chairs for that digest.
5. Contact the holders of the top eight chairs.
6. Group multiple image children assigned to the same holder into one `please_pull` RPC.
7. Each holder validates the chair epoch and generation.
8. Each holder checks local content and its local in-flight map.
9. At most one origin pull starts per digest on each holder.
10. Duplicate requests return `ALREADY_PULLING`.
11. Completed content is committed and advertised through DHT.
12. Other nodes discover providers and peer-fetch normally.

Nominal cold-origin seeding is eight copies per digest.

The cohort is contacted in one pass and partial acceptance is sufficient. The
requester moves to the next eight chairs only when the entire cohort accepts
nothing; any acceptance means the pull is under way and the requester waits on
DHT for it. Recruiting until eight acceptances are collected would count a
silent chair as absent when it is in fact already fetching, so each timeout
would add a seed instead of moving one. Measured on 1,000 nodes, that
distinction is the difference between 13.4 and 8.0 origin copies per layer.

**Please-Pull Transport**

`please_pull` is served over HTTPS on a dedicated listener, not over a libp2p
stream.

- The chair listens on `chair_listen`, default `0.0.0.0:5002`, and agents share
    the port by convention as they do the transfer port.
- The request carries the requester's delegated registry Authorization header,
    so the transport must be encrypted.
- The chair's TLS certificate is self-signed with its libp2p identity key. The
    requester pins it to the peer ID already published in the chair Lease, so no
    certificate authority, issuance or rotation is involved.
- The pin and the dial address come from the same Lease record and therefore
    rotate together.
- The peer ID is encoded in the request URL host so the HTTP connection pool
    keys on identity. Pooling by address alone would let a connection verified
    against one holder be reused after a rotation.

A libp2p stream shares the host's connection budget with DHT traffic. DHT
lookups open a connection per peer walked and nothing reclaims them, so the
swarm tends toward a full mesh and the connection manager trims whatever looks
idle, including chair connections about to be reused. An evicted chair
connection re-dials into libp2p's per-peer backoff and the requester records the
chair as failed. A separate listener removes chair calls from that budget, which
is what allows the libp2p watermarks to be sized for the DHT working set rather
than for the cluster.

**Planned Rotation**

- Chairs rotate on a configurable assignment epoch, separate from heartbeat expiry.
- Before the next epoch, the current holder selects a reachable non-chair Gantry node.
- The candidate must accept before being recorded as `nextHolder`.
- If no candidate accepts, the existing holder keeps the chair.
- At the epoch boundary, the accepted successor becomes holder and increments the generation.
- The old holder stops accepting new work for that chair.
- Existing origin pulls on the old holder continue to completion.
- The old holder remains an ordinary Gantry provider afterward.

**Pod Restart And Rollout**

- A restart on the same host reloads the same persistent peer ID.
- If the Lease still names that peer ID, the pod resumes renewal without election.
- If its address changed, it updates the Lease.
- While it is restarting, requesters use backup chairs.
- If another node claimed the chair while it was unavailable, the restarted pod remains a non-holder.
- Origin pulls terminated with the process must restart elsewhere.
- DaemonSet rollout uses bounded `maxUnavailable`.

**Dead-Holder Replacement**

Replacement is lazy and demand-driven:

```text
claimable = chair empty OR
            (holder unresponsive AND Lease expired)
```

- Unresponsive but Lease fresh: use a backup; do not claim.
- Responsive but Lease expired: continue using it; do not claim.
- Unresponsive and Lease expired: a non-chair requester may claim it.
- No demand: the dead chair remains untouched.
- Claimers jitter, re-read the Lease, then use a resource-version update.
- One claimant wins and increments the generation.
- Losers refresh and use the winner.
- A requester already holding another chair does not claim it.

**Accepted Limitations**

- Timeout ambiguity can temporarily activate more than eight seeds, though a
    silent chair no longer causes a substitute to be recruited.
- Network partitions can temporarily produce old/new-holder overlap.
- Direct duplicate `please_pull` traffic still requires 100,000-node measurement.
- Snapshot refresh bursts require jitter and API-scale validation.
- Eight-seed dissemination performance at 100,000 nodes is unmeasured.
- Chair selection currently lacks zone-awareness.
- Content integrity remains protected by digest verification.
- The libp2p connection watermark bounds the DHT's connection appetite, which is
    otherwise unbounded because a lookup opens connections that nothing later
    reclaims. The shipped value clears the DHT working set at 100,000 nodes but
    the resulting trim rate at that size is unmeasured.

**Implementation Defaults**

- Assignment epoch: `floor(unix_time / 6h)`.
- During a distributed epoch rollover, a holder and requester accept the
    immediately previous epoch until that chair's Lease renews or rotates.
- Lease duration: `60s`; heartbeat renewal: `20s`.
- Each Kubernetes chair API operation is bounded by `5s`.
- Successor selection starts `5m` before the next epoch.
- Startup snapshot jitter: deterministic over `30s`.
- Empty-chair claim rounds run every `1s` with up to `750ms` deterministic
    per-claim jitter.
- The initial claim lottery admits `1/2048` of peers. The divisor halves each
    stage, using one stable peer/epoch ticket so eligibility only widens until
    every node is eligible if fewer than eight chairs have been occupied.
- Widening occurs in eight-round stages. Nonparticipants refresh their chair
    snapshot on deterministic slots in a cluster-sized observation window,
    targeting roughly 500 follow-up Lease lists per second at 100,000 nodes.
- If duplicate ownership is observed after restart, the node retains the
    lowest chair ID and vacates the others.
- Direct-origin fallback jitter uses a configurable cluster-size estimate;
    the shipped value is `100,000`.
- `please_pull` listener: `chair_listen`, default `0.0.0.0:5002`, TLS 1.3.
- libp2p connection manager: trim above `900` connections back to `600`, with a
    `60s` grace period before a connection becomes a trim candidate. The
    resource manager's connection and descriptor ceilings are sized against the
    same figure. go-libp2p's own defaults, `160/192`, sit below this agent's
    working set. The value tracks the DHT working set, which is the routing
    table plus peers with an in-flight transfer, and so grows logarithmically in
    cluster size rather than with it.

**Rolling Interoperability**

- Chair assignment fields and successor offers are additive messages on the
    existing coordination protocol.
- `please_pull` moved to its own HTTPS listener, so a requester on this build
    cannot reach a chair that predates the listener. The chair port must be
    rolled out before the transport is relied on. The libp2p `please_pull`
    handler still exists and still serves legacy callers.
- A new server accepts legacy `please_pull` requests without chair metadata.
- A new client can call an old server because old protobuf readers ignore the
    additive chair field.
- New pods continue publishing their peer endpoint on their own Pod during the
    transition so old membership-based agents can discover them.
- Legacy Pod/Node read RBAC remains for old pods during this transition release;
    the new binary does not start Pod or Node informers.
- After rollout, set `coord_require_chair_assignment: true` to reject legacy
    `please_pull` requests that do not carry a chair generation.