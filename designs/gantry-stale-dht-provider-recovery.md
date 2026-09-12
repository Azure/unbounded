# Gantry Stale DHT Provider Recovery

**Status:** Draft for discussion

## Evidence Scope

Current-behavior statements in this document were checked against this
repository and the exact `go-libp2p-kad-dht v0.42.2` source selected by
`go.mod`. Dependency paths such as `routing.go` and `records/providers_manager.go`
below are relative to that module.

The `1h` validity, `20m` reprovide interval, `10m` maximum reprovide delay,
in-memory sweeping provider, and `5m` hard cold-start deadline are implemented.
Lookup expansion, availability-probe concurrency, explicit chair completion,
and unconditional eventual fallback remain proposed behavior. The timing
values have not been validated by cluster measurements.

## Implementation Status

Implemented in the current change:

- Section 1: configured provider validity and in-memory sweeping reprovide.
- Section 6: one hard cold-start deadline that bypasses further rediscovery and
   evaluates the existing direct-origin fallback gate immediately.

Sections 2-5 and 7 remain design proposals and are not implemented by this
change.

## Summary

Gantry uses libp2p Kademlia provider records as hints that a peer may hold an
OCI digest. A provider record does not prove that the peer is alive, still has
the digest, or belongs to the current node generation. Provider lookup also
does not return records in freshness order.

This becomes a liveness problem during node replacement. If every node that
advertised a digest is replaced inside the provider-record validity window,
the DHT can continue returning only departed provider identities even after
new chairs have pulled and advertised the same digest.

This design proposes two complementary changes:

1. Bound the stale-record window with a shorter provider validity and a
   correspondingly shorter reprovide interval.
2. Base cold-start and origin-fallback decisions on a usable provider or
   authoritative chair state, never on a merely nonempty DHT result.

The shorter validity limits how long stale records can interfere. It is not
the correctness mechanism. Correctness comes from allowing the state machine
to reach controlled origin fallback when no chair is pulling and no returned
provider can serve the digest.

## Current Behavior

### Provider lookup is bounded and unordered

Gantry calls the synchronous `FindProviders` API
(`internal/gantry/discovery/discovery.go:459`). In
`go-libp2p-kad-dht v0.42.2`, that API passes the DHT bucket size to
`FindProvidersAsync` (`routing.go:492-500`). The default bucket size is 20
(`amino/defaults.go:23-27`), so the first 20 unique providers collected satisfy
the lookup. Provider sets are shuffled before the count is applied, but they
are not ordered by publication time (`routing.go:536-626`,
`records/providers_manager.go:317-354`).

The provider response identifies peer IDs, addresses, and a connection hint.
It does not expose a provider timestamp, sequence, or signed expiry that Gantry
can use to prefer a new record (`pb/dht.proto:30-38`). Records from distinct old
and new provider identities coexist for the same digest because the storage
key includes both the digest and provider peer ID
(`records/providers_manager.go:277-284`).

Gantry shuffles the returned candidates again before fetching, which
distributes attempts within that result set but cannot introduce provider 21
or later (`internal/gantry/mirror/mirror.go:2382`).

### Provider records outlive departed nodes

Each DHT peer stores a provider record with its local receipt time. The
dependency default is 48 hours (`amino/defaults.go:40-43`), but Gantry now
overrides provider validity to one hour through
`dht.ProviderManagerOpts(records.ProvideValidity(...))`
(`internal/gantry/discovery/discovery.go:312-322`).

The dependency's sweeping provider is separate from `dht.New`. Gantry
constructs it explicitly with a 20-minute interval and 10-minute maximum delay
(`internal/gantry/discovery/discovery.go:383-405`). A successful synchronous
`Provide` also registers the digest with the sweeper, and `Withdraw` removes it
from the reprovide schedule (`internal/gantry/discovery/discovery.go:497-525`).
The sweeper uses in-memory key and schedule state; startup inventory reconcile
rebuilds that state after a process restart.

Provider records can outlive a departed provider while DHT peers that stored
the records remain alive. Gantry does not configure a DHT datastore, so the
dependency uses an in-memory datastore
(`internal/gantry/discovery/discovery.go:303`,
`internal/config/config.go:100,138-145`). Replacing a DHT process erases the
provider records held only by that process. A complete process replacement
therefore clears old records unless an old provider issued a fresh `Provide`
that wrote its record into replacement DHT peers during the overlap.

libp2p Kademlia has no protocol-level provider withdrawal. Gantry's `Withdraw`
therefore stops future refreshes, while already-published remote records remain
eligible until their storing peer's configured validity expires.

### Failure suppression is requester-local

The mirror records recent failures in process-local maps:

- A peer that returns not found for a digest is suppressed for three minutes.
- An unreachable address is suppressed for 30 seconds.
- A peer that returns invalid content is suppressed for five minutes.

The defaults are in `internal/gantry/mirror/mirror.go:643-645`. Suppression keeps
one requester from immediately retrying a failed candidate. It does not remove
the provider record, affect another requester, or cause a subsequent DHT query
to return different candidates.

### Nonempty DHT results are treated as progress

Two control points currently treat any nonempty provider result as sufficient:

1. Chair cold-start polling returns success on `len(providers) > 0`
   (`internal/gantry/coldstart/chair.go:637`). It does not establish that any
   provider is reachable or that a returned provider is one of the chairs that
   accepted the pull.
2. The final direct-origin-fallback recheck declines fallback on
   `len(providers) > 0` (`cmd/gantry/main.go:521`). It also does not establish
   that any provider is usable.

These checks can convert stale provider records into false recovery signals.

## Failure Scenario

Consider a digest advertised throughout a cluster before a rolling node
replacement:

1. Old nodes advertise the digest.
2. During rolling overlap, at least one old provider issues a fresh `Provide`
   after replacement DHT peers have joined, so those peers store the old
   provider identity.
3. The old provider nodes are replaced and receive new libp2p identities when
   their node-local identity storage is not retained.
4. A replacement node misses the digest locally.
5. `FindProviders` returns 20 old provider identities.
6. The requester fails to reach them and suppresses their addresses locally.
7. Cold-start asks the current ranked chair cohort to pull the digest.
8. One or more chairs finish and advertise under their new identities.
9. Chair polling again receives a nonempty set containing only old providers
   and reports success immediately.
10. The mirror cannot fetch from the returned providers and enters repeated
   discovery.
11. Repeated lookups may continue returning the same 20 old identities because
    there is no exclusion cursor or freshness ordering.

Successful chair pulls do not guarantee that a requester sees the resulting
new provider records. If cold-start is changed to report exhaustion but the
direct-origin-fallback recheck remains unchanged, the same stale records still
decline origin fallback.

A complete node replacement by itself does not guarantee this failure. If
every in-memory DHT store containing an old record is replaced and no old
provider writes into a replacement store during overlap, the old records
disappear with those processes regardless of provider validity. The failure
requires at least one stale record to survive on a current DHT peer. Upgraded
peers serve it for at most one hour after receipt; legacy peers in a mixed
rollout may retain it for the dependency's 48-hour default.

## Required Invariants

The implementation must preserve these invariants:

1. A DHT provider record is a candidate, not proof of availability.
2. A failed candidate must not count as cold-start progress for that request.
3. A chair that reports work still in flight may extend the wait within the
   request deadline.
4. A chair that has completed must be distinguishable from one that is still
   pulling.
5. If no chair is pulling and no candidate can serve the digest, cold-start
   must return `ErrExhausted` so controlled origin fallback can run.
6. Direct-origin-fallback recheck may decline fallback only after finding a
   usable provider, not merely a provider record.
7. Chair completion must not force every requester to fetch from one chair.
   Chairs are bounded initial seeds, not the permanent data plane.
8. Warm discovery must expand past failed provider sets up to a configured
   cluster-size ceiling before declaring a cold miss, subject to one hard warm
   lookup deadline.
9. Cold-start has one hard end-to-end deadline. DHT results, chair retries,
   backup cohorts, and fallback gates must not extend it indefinitely.
10. When that deadline expires without a usable provider, the request must
    enter direct origin fallback regardless of the discovery or coordination
    failure that prevented peer recovery.
11. Periodic reprovide must use the dependency's sweeping provider and stagger
   work both across the interval and across provider peer identities. Gantry
   must not reprovide every digest on every node in one periodic burst.

## Proposed Design

### 1. Shorten provider validity and reprovide interval

Add explicit Gantry configuration for both values:

```yaml
dht_provider_validity: 1h
dht_reprovide_interval: 20m
```

These are proposed initial defaults, subject to scale testing. Pass provider
validity to `dht.New` through `dht.ProviderManagerOpts` and
`records.ProvideValidity`. Integrate the dependency's `provider.SweepingProvider`
as the required periodic reprovide scheduler. The interval is not a `dht.New`
option. Gantry's advertiser must register present digests with
`StartProviding` and remove absent digests with `StopProviding`; event-driven
publication remains the immediate path for newly available content.

The sweeping provider divides the DHT keyspace into regions and schedules those
regions across the complete reprovide interval. Its region schedule is
permuted using the provider peer ID (`provider/provider.go:630-689`), so nodes
with different identities do not all intentionally walk regions in one common
order. This is a distribution mechanism, not a guarantee that every pair of
peer IDs receives a different offset for a given region. Gantry must preserve
the per-peer ordering when wiring the provider.

Gantry uses the sweeping provider's in-memory keystore and datastore. A restart
rebuilds the key set from containerd inventory and starts a fresh schedule.
Persisting provider scheduler state is not required.

Cross-node staggering is a requirement, not an assumption derived only from
the library API. Scale validation must measure publication rate by node and
keyspace region. If synchronized node startup or scheduler recovery still
produces aligned bursts, Gantry may select a fresh random startup phase on each
restart before beginning periodic reprovide. Restart continuity is not a
requirement; the maximum resulting refresh gap must remain below provider
validity.

Validation must require both values to be positive and the reprovide interval
to be shorter than the provider validity.

With these values:

- A live node refreshes its provider records every 20 minutes.
- A departed node cannot refresh.
- A DHT peer configured with the one-hour validity stops serving a provider
   record one hour after that peer last accepted it. Reads enforce expiration;
   the cleanup cadence only controls physical datastore reclamation.

This change increases DHT publication traffic. A naive per-digest scheduler on
a node advertising `D` digests would schedule approximately `3D` refreshes per
node per hour, which is why this design requires the sweeping provider. The
sweeper batches work by DHT keyspace region and spreads it across the interval,
so `3D` is not an estimate of network lookups or RPCs. Cluster-scale
measurements must determine whether these defaults are sustainable.

A one-hour validity does not make stale results impossible. A replacement can
happen inside an hour, and old providers can write records into replacement
DHT peers during rollout. Mixed-version peers can also retain the old validity.
The recovery state machine must remain correct when every returned candidate
is stale.

### 2. Expand warm discovery until usable or bounded exhaustion

Replace the synchronous 20-provider lookup with `FindProvidersAsync` and an
explicit target count. The first round requests 20 candidates. If every new
candidate fails, double the target and query again:

```text
20, 40, 80, 160, ... min(next target, cluster-size ceiling)
```

Each target is a cumulative result count for that lookup because libp2p
provider lookup has no exclusion list or pagination cursor. A request-scoped
set removes providers returned and tried in earlier rounds. Therefore a
40-result round may contain the same failed 20, but only its previously unseen
candidates are eligible for probing.

```text
target = 20
attempted = {}

while warm lookup deadline has not expired:
   candidates = FindProvidersAsync(digest, target)
   candidates = candidates - self - suppressed - attempted
   probe candidates with bounded concurrency

   if one candidate can serve the digest:
      fetch from that provider
      stop

   attempted += candidates

   if target == cluster-size ceiling:
      break

   target = min(target * 2, cluster-size ceiling)

enter cold-start
```

Gantry does not maintain exact cluster membership. The ceiling must therefore
come from an explicit configured cluster-size estimate. The current
`chair_cluster_size_estimate` defaults to 100,000
(`internal/gantry/config/config.go:484`), but the implementation should expose
a lookup-specific name rather than silently coupling two policies.

The requested count is an output ceiling, not a request to enumerate every
provider or every cluster node. Each remote `GET_PROVIDERS` response stops when
the next provider would exceed libp2p's message-size limit
(`handlers.go:195-233`), and the protocol provides no pagination cursor.
Kademlia lookup also terminates after its normal closest-peer convergence.
Consequently, even a target equal to the cluster-size estimate cannot prove
that every provider record was examined.

The warm lookup also needs one wall-clock deadline. The cardinality ceiling
alone is not a time bound: probing a large stale provider population could take
longer than the caller can wait. The deadline is authoritative. If it expires
before the target reaches the cluster-size ceiling, the request enters
cold-start rather than continuing an unbounded warm search.

Candidate probes need bounded concurrency so sequential dial time does not
scale linearly with the number of stale records. The concurrency and lookup
deadline must be measured and configurable. The existing expensive peer-body
transfer limit remains separate from the cheaper availability-probe limit.

Expansion improves the chance of discovering replacement providers hidden
behind old records. It does not turn DHT lookup into a freshness guarantee, so
bounded cold-start and origin fallback remain required.

### 3. Carry failed candidates through the request

Maintain a request-scoped set keyed by provider peer ID and address. Add a
candidate after a failed dial, not-found response, invalid response, or other
terminal fetch failure.

Every discovery round in the request must filter this set before deciding
whether it found progress. Process-local TTL caches remain useful across
requests, but request-scoped exclusion prevents a short cache TTL or a long
request from retrying the same failed provider as if it were new evidence. A
round that returns only previously attempted providers is an exhausted round,
not progress, and advances the exponential target.

### 4. Make chair completion explicit

The current chair protocol reports both an already-running pull and an
already-cached digest as `PleasePullAlreadyPulling`
(`cmd/gantry/main.go:1700`). Add an explicit available outcome carrying the
chair holder's existing transfer endpoint, or add a chair status query that
can report these distinct states:

- `pulling`: origin work is still in flight.
- `available`: the chair can open and serve the digest.
- `failed`: the pull finished unsuccessfully.
- `absent`: no pull is active and the digest is not available.

Cold-start may wait while at least one accepted chair reports `pulling`. A DHT
result containing only request-scoped failed candidates must not end that
wait.

When chairs report `available`, their endpoints provide authoritative initial
seeds. They should be merged with usable DHT candidates and distributed across
requesters rather than directing every requester to the first ranked chair.
The existing transfer concurrency limit and `429 Retry-After` behavior remain
the load-shedding boundary while successful requesters become additional DHT
providers.

This design does not solve the separate coordination fan-in where many
requesters contact the same ranked chair cohort. It must not increase that
fan-in, and chair RPC scaling needs separate measurement and design work.

### 5. Define usable-provider checks consistently

Introduce one narrow operation used by cold-start polling and
direct-origin-fallback recheck:

```text
find usable provider(digest, excluded candidates)
    collect bounded DHT candidates
    remove self and suppressed/excluded candidates
    probe candidates within a bounded budget
    return success only after a peer confirms the digest is available
```

A successful peer metadata `HEAD` establishes that the transfer endpoint was
reachable and reported the digest available at probe time without transferring
the layer. It does not guarantee that the subsequent `GET` will succeed. A
failed `GET` adds the candidate to the request-scoped failure set and resolution
continues.

The direct-origin-fallback recheck must use this operation. It must not decline
fallback because an unprobed stale record exists.

### 6. Add a hard cold-start deadline

Cold-start must receive one absolute deadline when it begins. The deadline
covers the complete operation, including:

- Chair snapshot and refresh calls.
- Initial and backup chair dispatch.
- Waiting while a chair reports active origin work.
- DHT polling for newly available providers.
- Chair status rechecks and patience rounds.

Per-kind stall windows may decide when to recheck or move to a backup cohort,
but each sub-operation must be capped by the remaining cold-start time. No
sub-operation may create a new deadline beyond the original absolute deadline.

Until that deadline, cold-start follows authoritative state:

```text
usable peer found
    -> fetch from peer

chair reports pulling
    -> wait within the bounded chair/request deadline

chair reports available
    -> use the available chair set as initial peer seeds

no chair pulling or available, and no usable DHT provider before deadline
    -> coldstart.ErrExhausted
    -> mirror.ErrColdStartExhausted
   -> direct origin fallback
```

An empty DHT result, DHT error, DHT timeout, stale-only result, chair RPC error,
chair turnover, completed-but-undiscoverable chair, or exhausted backup cohort
must all converge on this same deadline. None may create a terminal state that
permanently bypasses direct origin fallback.

### 7. Make origin fallback eventual

The current direct-origin-fallback controller can decline because of bootstrap,
DHT health, local in-flight work, rate limiting, or its final DHT recheck. That
is useful before the hard cold-start deadline, but it does not satisfy the
liveness requirement after the deadline.

After the hard deadline:

- A stale or unprobed DHT record cannot decline fallback.
- Bootstrap and DHT-health gates cannot permanently veto fallback.
- Rate limiting and jitter may schedule fallback only within a separately
   bounded escape window; they cannot return the request to an indefinite
   peer-discovery loop.
- Per-node per-digest in-flight dedup remains valid while a local origin pull is
   actually running. A waiter must follow that pull to completion or failure
   within the request deadline rather than receiving an unbounded series of
   declines.
- If no usable provider materializes, one direct origin attempt must begin by
   the end of the escape window.

This requirement prioritizes liveness over the strict origin-protection goal.
In the worst case, many nodes can reach the hard deadline together and pull the
same digest from origin. Jitter, chair coordination, usable-provider rechecks,
and local in-flight dedup reduce that amplification, but they cannot both
guarantee a direct origin attempt for every isolated requester and guarantee a
single cluster-wide origin pull during a total coordination failure.

"Regardless of failure scenario" here means failures in DHT discovery, peer
availability, chair state, or chair coordination. It does not mean retrying an
origin operation indefinitely after origin itself returns a terminal response
such as an authorization failure or a confirmed not-found response.

## Why Simpler Changes Are Insufficient

### Shorter validity alone

It bounds stale-record lifetime but permits the same failure until expiry. It
also does not fix false success in chair polling or direct-origin-fallback
recheck.

### Returning more than 20 providers alone

It improves the chance of finding a new provider but cannot guarantee one. A
large replacement can leave more stale providers than any practical fixed
candidate limit. Exponential expansion therefore ends at both a cluster-size
ceiling and a wall-clock deadline, then moves to cold-start.

### Requester-local suppression alone

The DHT can return the same suppressed records repeatedly. Other requesters
must learn the same failures independently.

### Fetching only from chairs

It restores liveness but turns the fixed chair cohort into the data plane. The
normal path must still spread through newly completed requesters and other
providers.

### Treating DHT results as the newest records

The provider protocol does not expose or order records by publication time.
There is no "newest providers" query to request.

## Rollout Considerations

Provider validity is enforced by the DHT peers storing provider records. A
mixed-version rollout can therefore contain peers using both the old and new
validity settings. The implementation must not assume that lowering the local
setting immediately removes all old records.

Whether old and new Gantry versions must coexist through the entire rollout is
an open requirement. This document does not introduce a new DHT protocol
namespace or a compatibility bridge without that requirement being decided.
Regardless of rollout policy, usability-based recovery remains necessary.

The incorrect 24-hour/12-hour documentation must be updated in the same
implementation change so operators can reason from configured values rather
than dependency defaults.

## Metrics

At minimum, add or retain measurements for:

- DHT candidates returned, filtered, probed, and usable per lookup.
- Warm lookup target and round count, including whether the target doubled or
   reached the cluster-size ceiling.
- Lookups where every returned provider was request-scoped stale or
  unavailable.
- Unique provider identities observed across rediscovery rounds.
- Chair states observed after dispatch: pulling, available, failed, absent.
- Cold-start elapsed time and terminal reason, including hard-deadline expiry.
- Direct-origin-fallback rechecks separated into usable hit, stale-only, empty,
  and lookup error.
- Origin fallback attempts that became mandatory after the hard deadline.
- Sweeping-provider operations, failures, queue depth, schedule lag, and
   duration by node and keyspace region.
- Per-node and cluster-wide reprovide publication rate, including burst size.
- Time from a chair's successful commit to first successful peer fetch.

## Validation

Unit and integration coverage must include:

1. A warm lookup expands through `20, 40, 80, ...` and never probes the same
   provider twice in one request.
2. A usable provider outside the first 20 is found before cold-start.
3. Reaching the configured cluster-size ceiling without a usable provider
   enters cold-start.
4. Expiring the warm lookup deadline before reaching the cardinality ceiling
   enters cold-start.
5. Twenty stale providers do not make chair polling report success.
6. Twenty stale providers do not make direct-origin-fallback recheck decline.
7. A chair reporting `pulling` extends the wait without starting duplicate
   origin work.
8. A chair reporting `available` becomes a usable seed without requiring its
   provider record to appear in the first DHT result.
9. A chair reporting neither pulling nor available allows
   `ErrColdStartExhausted` and the direct-origin-fallback gates to run.
10. DHT errors, stale-only responses, chair RPC failures, chair turnover, and
    exhausted backup cohorts all reach the same hard cold-start deadline.
11. Once the hard deadline expires, bootstrap, DHT health, stale records, and
    token exhaustion cannot return the request to indefinite discovery.
12. A mandatory origin attempt starts before the bounded escape window ends
    when no usable provider appears.
13. Live providers remain discoverable across multiple one-hour validity
   windows through 20-minute reprovide.
14. Reprovide work is distributed across the interval rather than emitted as a
   full-inventory burst.
15. Across a representative population of persistent peer IDs, reprovide work
   for the same keyspace region is distributed across the cycle and does not
   form one synchronized cluster-wide burst.
16. The maximum measured gap between successful reprovides remains below the
   provider validity after accounting for schedule lag and retry delay.
17. Restarting the provider rebuilds its in-memory key set from containerd
   inventory and starts a fresh schedule without requiring continuity from the
   previous process.
18. A DHT peer stops serving a departed provider after the configured validity,
    independently of when physical cleanup deletes the stored entry.
19. A simulated complete provider-node replacement during a mixed-version
   48-hour validity window, where old providers wrote records into replacement
   DHT stores during overlap, makes progress through current chairs or
   controlled origin fallback even when every initial DHT candidate is stale.
20. Concurrent requesters distribute available chair seeds rather than all
    selecting one transfer endpoint.

## Open Questions

1. What warm lookup deadline and probe concurrency give acceptable lookup cost
   before cold-start?
2. Should the warm lookup ceiling have a dedicated configuration value or reuse
   the chair cluster-size estimate?
3. Can a one-hour validity and 20-minute reprovide interval sustain the
   measured digest inventory and cluster size?
4. What maximum random startup phase prevents synchronized restart bursts while
   keeping the worst-case refresh gap below provider validity?
5. Should chair availability be represented by a new `please_pull` outcome or
   a separate status RPC?
6. Must mixed-version agents share one DHT throughout rollout?
7. What bounded escape window limits synchronized origin load while still
   guaranteeing an origin attempt after cold-start expiry?
8. What bounded policy should distribute requesters among available chair
   seeds without increasing chair coordination fan-in?