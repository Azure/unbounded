# Read implementation handoffs

Read owner coordinates only this directory. Integration owns application and errors.

## Blocking signed budget handoff (peer/security/topology owners)

Add `remaining_attempts: u32` to topology::paths::RouteBudget, include it in signed
canonical peer request encoding/decoding, preserve/decrease across forwarding.
Read candidate policy reserves a child acquisition attempt allowance before a
remote Acquire; destination Coordinator must consume that allowance rather than
create 32 fresh attempts. CopyOnly gets zero acquisition attempts. Conservative
charging may discard unused allowance when the response lacks a signed credit
receipt; it must never mint fresh credits. This field is needed for the requested
full original budgets across nodes. Add PeerResponse::OriginForbidden and sign its
distinct 403 outcome rather than collapsing it into OriginRejected/401.

Read delegates are separate CLI processes. Do not use concurrent opencode run
--session to send steering to a running delegate: that starts another writer for
the same files. Add coordination notes here; the read parent consolidates delegates.

Latest shared check: owned read modules compile; integration app.rs:92 must replace
Arc::new(WorkerDirectory) with WorkerDirectory::new(Arc<WorkerMap>, Vec<WorkerId>,
queue_capacity). Install each directory endpoint after Coordinator assembly and
poll WorkerEndpoint along with flights; coordinate final delegate API below.
Security currently needs memory PlaintextBuffer::into_parts (external owner).

Update: cargo check --lib --all-features passes. Test compilation currently blocked
by app composition test Result unwraps (app.rs:865-904). Memory test visibility is
resolved. Read tests now include real crypto-engine driven origin fill/coalescing,
pending ciphertext reuse, independent credential rejection retry, and cancellation
generation fencing in fill_tests.rs, but cannot execute until app test compile fixes.
Candidate route visited starts empty: topology validates prior senders excluding
the current sender and forwarding adds it. Starting with self would reject every
outbound request as a loop.

- CandidatePolicy keeps its existing constructor. Fill::new installs its shared
  CredentialCrypto through CandidatePolicy::set_credentials. Metadata is composed
  after Fill and uses the same policy. resolve_with_budget takes candidates,
  context, operation, scope, and mutable AcquisitionBudget. It returns either a
  verified copy/acquisition response or scoped origin authority. Noncandidates
  request Acquire from ranked candidates; candidates probe predecessors CopyOnly.
- Flight budget needs remaining_attempts(), remaining_links(), deadline(), and
  refund_links(u8) for reserving a route before transport and reconciling a verified
  completed route. Failed exchanges conservatively retain the full route debit.
- Metadata exposes resolve_with_budget/bootstrap_with_budget. Existing convenience
  entry points create one initial budget, never reset it inside retry loops.
- Integration supplies OriginRejected (401) and OriginForbidden (403), each a
  caller-only origin failure. Peer authentication stays Unauthorized and terminal.
- Integration must install/poll bounded WorkerDirectory endpoints on each worker
  and drive shared Flights completions even when requesting futures disappear.
- Client owner retains ReadKind::Head and supplies HeadPinned { etag: StrongEtag }
  for pinned SDK HEAD. Coordinator must handle these distinct variants.
- Error::OriginForbidden joins OriginRejected as a caller-only origin failure;
  Unauthorized remains a terminal peer authentication failure.

These are interface requirements, not assertions that external owners already
implemented them. Tests and final integration reports identify unresolved edges.

## Flight delegate integration request (updated)

Fill now uses read::drivers, a bounded worker-local round-robin future owner. Flight
delegate: call `super::drivers::poll(cx, work_budget)` from worker poll/drain paths
outside any RefCell table borrow; use noop_waker_ref for poll_budgeted if needed.
Fill moves a leader, retained operation
token, an owned sealed/opened origin context, a transferred original budget, and
scope into this future. Its oneshot reply returns remaining budget to a live caller;
if the caller disappears its credits disappear too. The driver completes its
operation token before publishing/failing. Never drop accepted drivers on shutdown.
There is no need to implement a second spawn_driver queue in Flights.
Security implements open_charged -> ChargedOriginContext (Deref OriginContext).
Use this owner for all retained driver contexts. drivers::reserve() yields a Permit BEFORE retaining any
FlightOperation; Permit::submit cannot fail, avoiding a leaked completion token on
queue overload. drivers::spawn remains convenience for unsubmitted operations.

## Origin and peer resource handoffs

Origin owner: Fill reserves full progress before acquisition. Add
`Origin::page_reserved(authority, context, page, Reservation, scope)` so OriginClient
uses the supplied plaintext reservation rather than reserving a second full page.
Keep page() as the convenience wrapper for independent callers. Fill will use the
reserved operation. Default implementations for test doubles may release the
reservation before calling page(), but production must consume it into the buffer.

Peer owner: preserve OriginForbidden separately from OriginRejected on signed
responses. Full request accounting needs request_with_budget or a verified route
consumption receipt: attempts consumed at candidates must not reset at destination,
and completed forwarding link consumption must reconcile with the parent budget.
Current candidate code conservatively charges the permitted route before sending,
so failed responses cannot refund unknown link consumption.

Metadata owner: Fill now implements publish_bootstrap_with_context(origin_page,
membership, context, scope, budget). It joins the same page flight, supplies the
prefetch only if elected, encrypts once, validates full identity/length, and persists
only on candidates. Do not call the old proposed publish_bootstrap without context.

## Required completion fence contract

Retained read drivers mark their FlightOperation complete only when awaited origin,
peer, disk and crypto operations return. Those operations MUST NOT return a
cancellation/deadline error after submission until their accepted I/O/crypto work
is actually reaped. Runtime CryptoClient::execute currently checks scope before
consuming an accepted completion (runtime/crypto.rs around 400); change accepted
waiters to keep polling until completion, then return the cancellation error.
The same requirement applies to submitted reactor operations. Dropping an ingress
future never drops the retained read driver; safe late returns make generation
drain exact without global worker fences. Retaining buffers alone is insufficient
if an early returned error lets read elect a new origin acquisition.
