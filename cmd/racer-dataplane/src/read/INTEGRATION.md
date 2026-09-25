# Read implementation handoffs

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
- Ingress allocates one aggregate allowance: 32 attempts and 96 forwarded links.
  Four is the normal per-route ceiling; observed failure permits up to eight from
  the already allocated allowance. This changes a ceiling, not remaining credits.
- `RouteBudget.remaining_attempts` is signed, decoded, and preserved by forwarding.
  CandidatePolicy removes a remote Acquire allowance from the original budget
  before submission. CopyOnly carries zero acquisition credits. Failed/unknown
  remote completions cannot refund credits. No signed response credit receipt is
  implemented, so unused remote allowance is conservatively spent.
- Destination Coordinator uses the signed attempt allowance and deducts the final
  incoming link exactly once. It never creates a new default allowance.
- Metadata, bootstrap, owned drivers, worker handoffs, and range fanout transfer or
  partition the original budget. Sliding-window children receive at most eight
  attempts and sixteen aggregate links; completed local children return only their
  remaining owned credits. Failure mode and the tighter deadline survive handoff.
- `PeerResponse::OriginForbidden` is separate from OriginRejected, including signed
  outcome encoding and read mapping. Both fail only the credential supplier;
  Unauthorized remains a terminal peer authentication failure.
- Membership and candidate authority are resolved for each elected caller. Only
  ranked candidates can mint origin authority. Predecessor probes are CopyOnly;
  noncandidates request Acquire through peers. Origin 412 plus an unreachable
  permitted copy remains transient Unavailable, not proof of version absence.

## Completion and memory ownership

`read::drivers` is a bounded worker-local round-robin owner. Fill transfers the
leader, retained FlightOperation, charged context, and original budget into an
owned driver. Dropping ingress detaches the waiter and requests cancellation;
accepted work remains owned until completion. Flights::poll_with_context drives
these tasks outside the flight-table borrow, including during drain.

Origin, peer, disk, and crypto methods returning after accepted submission must
fence their actual completions before returning cancellation. The runtime owner
fixed CryptoClient's early cancellation return. The strengthened regression
`canceled_supplier_retains_crypto_fence_before_replacement_origin_work` now passes.
Never substitute buffer retention or a timeout for that completion fence.

CredentialCrypto::open_charged returns a quota-owning context. Driver queue permits
are acquired before retaining a FlightOperation; submission is infallible after
reservation. No credential-bearing context enters a completed flight or cache.

Fill::reserve_progress performs one bounded reclamation pass on overload: discard
only unsubmitted disposable writes, evict idle memory, retry admission. Busy
reader/ciphertext leases and submitted writes remain pinned. Dirty-only saturation
permits memory-only completion. Local disk staging gets one reclamation retry;
writer enqueue staging overload is disposable and never fails plaintext delivery.

## Composition interfaces

- WorkerDirectory::new takes an immutable Arc<WorkerMap>, worker IDs, and bounded
  queue capacity. Install each endpoint after local Coordinator assembly and poll
  WorkerEndpoint alongside Flights. Drain accepted commands before changing maps.
- Fill::new installs the shared CredentialCrypto on CandidatePolicy. Metadata uses
  the same policy and budget-aware resolve/bootstrap paths.
- Origin::page_reserved consumes the fill's plaintext reservation, avoiding a
  second full-page reservation. OriginClient implements this production path.
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
