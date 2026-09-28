# Read implementation handoffs

Resolved composition handoff: Application constructs `HttpIo::for_clients` for
client listeners and responses, separately from page-capped origin/peer framing.
The regression
`responses_stream_more_than_three_pages_only_with_client_sized_http_framing`
and the production/process suites cover ordered multi-page responses. The former
urgent request to split those codecs is historical, not outstanding work.

Security schema handoff: `racer-route-attempts` is required canonical u32 on the
original and each hop. The original ceiling is immutable, every hop is nonincreasing,
and final logical agreement is exact. CopyOnly requires zero; Acquire zero stays
zero. Signed application origin-forbidden/403 and origin-rejected/401 remain
distinct inside outer 200 framing. Native fallback neither mints nor refunds
credits. See `designs/racer-peer-security.md` for the complete reviewed contract.

Read owns this directory and read.rs. Other component owners implement the
runtime, crypto, peer wire, HTTP, origin, model, topology, and application seams.

## Integrated routing and budgets

- Signed initial requests carry `visited=[local]`. `peer::search_budget` removes
  that local sender before topology search; topology and signing intentionally
  consume different representations. Never send an empty signed visited list.
- Client ingress allocates 32 attempts and 96 forwarded links for metadata or
  bootstrap. A normal pinned client range admits each distinct page once with
   eight attempts and sixteen aggregate links in a bounded sliding window. Initial
   metadata and the first slice share the admission deadline. After client success
   headers, newly admitted pages receive fixed child acquisition deadlines;
   already admitted pages and their retries never renew deadlines or credits.
   Client writes use a separate progress-based stall timeout. Successful pages do
   not exhaust a range-wide total. See `CLIENT_ORIGIN_API.md` for wire compatibility.
  Four is the normal per-route ceiling; observed failure permits up to eight from
  that acquisition's allocated allowance. This changes a ceiling, not credits.
- `RouteBudget.remaining_attempts` is signed, decoded, and preserved by forwarding.
  CandidatePolicy removes a remote Acquire allowance from the original budget
  before submission. CopyOnly carries zero acquisition credits. Failed/unknown
  remote completions cannot refund credits. No signed response credit receipt is
  implemented, so unused remote allowance is conservatively spent.
- Destination Coordinator uses the signed attempt allowance and deducts the final
  incoming link exactly once. It never creates a new default allowance.
- Metadata, bootstrap, owned drivers, worker handoffs, retries, and peer fanout
  transfer or partition their acquisition's original budget. No same-page retry
  or remote Acquire receives a fresh allowance. Bootstrap delivers its seeded page
  without reacquisition. Explicit `read_with_budget`/`open_with_budget` callers
  retain an aggregate range ceiling: children receive at most eight attempts and
  sixteen links and return only unused owned credits. Failure mode and the tighter
  deadline survive handoff.
- `PeerResponse::OriginForbidden` is separate from OriginRejected, including signed
  outcome encoding and read mapping. Both fail only the credential supplier;
  Unauthorized remains a terminal peer authentication failure.
- Membership and candidate authority are resolved for each elected caller. Only
  ranked candidates can mint origin authority. Predecessor probes are CopyOnly;
  noncandidates request Acquire through peers. Origin 412 plus an unreachable
  permitted copy remains transient Unavailable, not proof of version absence.
- Page candidate selection accepts a peer copy only after requester-side structural
  and AEAD validation. CorruptRecord or MissingKey from copy validation advances
  the existing ranked loop once per source, retaining predecessor evidence and
  the original attempt/link/deadline allowance. Unauthorized remains terminal.
  After origin 412, every later CopyOnly source is likewise validated before
  acceptance. Unusable copies count as transient evidence, never proof that a
  pinned version is absent. Only validated whole pages are published.
  Disk CopyOnly remains ciphertext-only: the requester decrypts each attempted
  copy once rather than adding a redundant decrypt on the serving peer. Local
  disk acquisition still conditionally invalidates its own bad read token.
- Sequential peer attempts divide remaining time among the remaining candidates,
  including one local-origin share after predecessor probes. Each attempt uses a
  fresh cancellation scope and a deadline capped by the original scope and budget.
  Expiring that share cancels and drains the peer operation before fallback; it
  never cancels the caller or refunds uncertain attempt/link credits. Expired
  original deadlines and caller cancellation remain terminal.

## Completion and memory ownership

`read::drivers` is a bounded worker-local runnable-aware owner. Fill transfers the
leader, retained FlightOperation, charged context, and original budget into an
owned driver. Dropping ingress detaches the waiter and requests cancellation;
accepted work remains owned until completion. Flights::poll_with_context drives
these tasks outside the flight-table borrow, including during drain.
Parked acquisition and directory-command futures are polled only after their
task-specific wake; command cancellation/deadline checks retain bounded turns.

Origin, peer, disk, and crypto methods returning after accepted submission must
fence their actual completions before returning cancellation. The runtime owner
fixed CryptoClient's early cancellation return. The strengthened regression
`canceled_supplier_retains_crypto_fence_before_replacement_origin_work` now passes.
Never substitute buffer retention or a timeout for that completion fence.

CredentialCrypto::open_charged returns a quota-owning context for encrypted
envelopes. Same-owner range pages and acquisition drivers use `local_context`,
which admits independent zeroizing fields before allocation without an AEAD or
mailbox round trip. Driver queue permits
are acquired before retaining a FlightOperation; submission is infallible after
reservation. No credential-bearing context enters a completed flight or cache.

Flight waiters and directory receipts own cancellation subscriptions. Their
task-specific wakers must not accumulate in the worker-lifetime cancellation
scope inherited by peer ingress. Fill's driver-result wait reuses its acquisition
waiter's subscription; detach releases notification capacity but not outstanding
I/O/crypto fences. The assembled peer regression completes 1,100 authenticated
cross-worker dispatches on one worker scope, and the fill regression recovers
after 1,100 failed cohorts without recreating that scope or raising any limit.

Fill::reserve_progress reclaims only the exhausted resource class's deficit,
restricting fair-share reclamation to the requesting cache. Bounded passes stop
after sufficient idle memory or unsubmitted write capacity is released. Busy
reader/ciphertext leases and submitted writes remain pinned. Dirty-only saturation
permits memory-only completion without reclamation. Bootstrap and local disk
staging use the same policy; writer enqueue staging overload is disposable and
never fails plaintext delivery.
Fill-owned writer enqueue also applies this bounded reclamation to the exact
aligned ciphertext staging charge before accepting a dirty copy. The current
fill's buffers, other live readers, and submitted writes stay pinned. An idle
memory cache must not block writeback merely because it consumes the staging
headroom; unreclaimable live-byte pressure still permits memory-only completion.
Fresh origin bootstrap uses Fill::reserve_bootstrap for the same bounded
reclamation before network work, even when no fresh metadata pointer exists.

## Composition interfaces

- WorkerDirectory::new takes an immutable Arc<WorkerMap>, worker IDs, and bounded
  queue capacity. Install each endpoint after local Coordinator assembly and poll
  WorkerEndpoint alongside Flights. Drain accepted commands before changing maps.
- Fill::new installs the shared CredentialCrypto on CandidatePolicy. Metadata uses
  the same policy and budget-aware resolve/bootstrap paths.
- Origin::page_reserved consumes the fill's plaintext reservation, avoiding a
  second full-page reservation. OriginClient implements this production path.
- Origin::bootstrap_reserved likewise consumes the read owner's page-zero
  plaintext reservation. Empty replies and failed operations release that charge.
- Fill::publish_bootstrap_with_context(origin_page, membership, context, scope,
  budget) joins the shared page-zero flight. Only an elected supplier encrypts the
  prefetch; an existing acquisition wins. Empty objects allocate no page.
- Client keeps ReadKind::Head and HeadPinned { etag }; coordinator maps these to
  fresh and pinned metadata respectively. Pinned range 416 uses selected length.

## Verification

All 70 integrated release read tests pass, including real TCP candidate requests through
production Requester, handshake, Forwarding, and PeerServer. They check signed
visited state, inherited attempts, final-link debit, and distinct OriginForbidden.
The crypto-fence and sequential full-page pressure regressions pass. Pressure tests
acquire 8/12 full 16 MiB pages under a two-page plaintext budget with page zero held
by another reader; all accounting is bounded and returns to zero after release.

`cargo check --all-targets --all-features` passes with an unrelated integration-test
dead-field warning. Whole-application production tests currently fail during
fixture setup at tests/production_dataplane.rs:181: installing the decoded static
control/testdata/bundle.json returns InvalidConfiguration before read admission.
Integration owns fixing that fixture and rerunning its three end-to-end tests.

`python3 src/read/check-component.py` remains a focused runner for unchanged
production modules excluding application/telemetry roots. It is not a replacement
for the integrated Cargo tests above.
