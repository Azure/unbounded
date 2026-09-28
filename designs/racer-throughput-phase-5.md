# Racer throughput architecture: Phase 5

## Authority and execution

Implement the complete user-approved Phase 5 on `racer-throughput-architecture`,
as the sole writer, without agents. The approved scope authorizes cross-package
changes and resolution of the prose conflicts identified below. The canonical
`/home/azureuser/design.md`, worktree AGENTS.md, Phase 1-4 plans, and executable
process README were read before implementation. This plan is committed first.

Every test, benchmark, and test-running script uses the external prefix
`timeout --signal=TERM --kill-after=10s 300s`; shorter limits are allowed. Tool
timeouts are at most 320000 ms. Go tests also use `-timeout=5m`. Split groups,
investigate timeouts, and stop/reap child processes. Scratch stays in this
worktree. Use apply_patch for edits, cargo fmt for Rust and make fmt for Go.
Before every incremental commit inspect status, diff and the last ten commits.
Preserve tests, generated-file workflows, completion fences and existing caches.
Integrate with the parent only after checking its branch and working-tree state.

## Inspected starting point and approved corrections

Rust paths below are relative to `cmd/racer-dataplane`.

- `src/topology/placement.rs:139-167` keys a bounded entry-count cache by the
  Membership allocation, losing completed work on endpoint-only replacement.
  `:185-205` cooperatively hashes 256 members per poll but has no request scope.
  Exact cold ranking remains O(N); the rendezvous arithmetic and vectors stay
  unchanged. Separate local placement identity from authoritative routing epochs.
- `src/read/fill.rs:626-632` ranks before checking queued/disk copies at
  `:640-661`. Move authority-independent local reads ahead of remote ranking,
  preserving Phase 4 ciphertext flights and lazy authenticated promotion.
- `src/control/snapshot.rs:139-169` prepares immutable state outside the lock,
  while `:239-247` can still clone cache definitions under publication lock.
  `:219-236` retains only weak old versions and counts externally pinned ones:
  there is no cross-node time grace when a receiving node has no local lease.
- `internal/racer/publications.go:96-140` prepares shared immutable full encodings;
  `:216-305` authenticates and coalesces polls but serves only full snapshots.
  Add versioned deltas with a bounded full fallback and identical canonical hashes.
- `internal/racer/membership.go:139-145,194-200` explicitly loses accepted member
  identity on controller restart during endpoint gaps. This contradicts canonical
  stable placement (`/home/azureuser/design.md:20-22`). Persist last-admitted
  identity tied to Node UID; deletion/exclusion must still remove placement.
  Shares currently come from Node annotations/defaults (`:43-52`); support the
  canonical authenticated dataplane env proposal with explicit annotation priority.
- `src/store/writer.rs:315-379,387-394` serializes persistence and assumes one free
  index slot cannot be consumed across await. `src/app.rs:1154` drives one write.
  Parallel submission requires pre-SQE index tickets and segment reservations.
- `src/app.rs:559,594` uses metadata limits for disk indexing as well as catalog
  state. Separate these budgets and checkpoint memory from metadata catalog size.
- `src/store/checkpoint.rs:55-77,143-159` snapshots synchronously and serializes
  full images; the application calls checkpoint during shutdown. The process
  README `:74-88` expects total safe refetch after uncheckpointed SIGKILL. Extend
  this to bounded recent-checkpoint rediscovery with actual SIGKILL evidence.

Compatibility: retain Phase 4 peer v4 coordinated upgrade and record v2 writes /
v1-v2 reads; CRC and AEAD checks remain mandatory. Phase 2's 24-hour key cadence
stays. The approved local certificate storage recommendation remains an explicit
exception to canonical volume-mounted client certificates; do not add a second
Secret workflow. Placement hashes remain dataplane-computed, never locally
overridden or made authoritative by the controller.

## Implementation sequence and acceptance

### A. Exact placement with bounded working sets

1. Give validated memberships a placement identity covering sorted Node IDs and
   shares only. Preserve independent routing membership versions and return the
   caller's current routing lease even when completed rankings are reused.
2. Retain compact top candidates within an explicit memory budget, including
   descriptor/index overhead. Coalesce active cold work; no million-slot tables.
   Maintain demand-observed ranks across membership changes in bounded turns.
   Incrementally update when enough retained information proves exactness;
   invalidate/recompute cooperatively when removal loses required candidates.
3. Add scope-aware cooperative ranking, cancellation checks at every quantum,
   bounded demand warming and maintenance. No global-lock hashing or cache sweep.
4. Try authority-independent memory/queued/disk data before remote ranking.
   Origin ingestion and authoritative replies still prove candidate ownership.
5. Test unchanged golden ranks, shares/ties, endpoint-only cache reuse/current
   leases, add/remove/share churn, cache pressure and cancellation. Exercise a
   100k-member cold working set with measured poll/traffic responsiveness.

### B. Authenticated scalable control and stable membership

1. Prepare immutable validation, canonical hashes and shared cache state once,
   outside publication locks. Recheck cursor/base/limits on commit only. Preserve
   independent pending-install, key reload and identity-renewal duties.
2. Introduce authenticated versioned deltas carrying base and target sequence,
   membership versions, content hashes, member add/remove/update and cache changes.
   Produce bounded shared/coalesced encodings in Go, apply/validate in Rust using
   the same canonical contract, and fall back to a full snapshot for missing base,
   replay/conflict, bad hash, excessive delta size or disconnected consumers.
   Never use controller-computed placement or an alternate canonical authority.
3. Bound wire plus decoded/prepared state and retained delta history by bytes and
   counts. Reuse TLS connections with bounded backoff/jitter. Trust or identity
   replacement invalidates pooled authenticated connections and renews correctly.
4. Keep bounded cross-node old-membership grace by time/bytes/count, with default
   two old versions. If a version falls outside grace, use explicit bounded stale
   retry preserving original object pins, deadline, attempt/hop/link credits.
   Exercise actual staggered nodes at default limits, not only DST's larger cap.
5. Support RACER_SHARES (default four) through authenticated enrollment; explicit
   Node annotation wins. Persist last-admitted Node-UID identity for restart gaps.
   Test missing endpoints, exclusion/deletion, UID replacement, annotation changes,
   malformed proposals, restart and authenticated publication on all dataplanes.
6. Test cross-language add/remove/update, replay, missing base, bad hash/full
   fallback, key progress during pending publication, renewal and retained churn.

### C. Concurrent storage and crash-bounded cache hints

1. Reserve index capacity tickets, append extents and generation leases before
   SQE submission. Drive a bounded set of concurrent writes in the real application
   (depth greater than one), publishing independently after full successful CQEs.
   Out-of-order completion, short writes, failure, lifetime cancellation, key/cache
   removal and segment reincarnation must preserve charge and generation fences.
2. Separate disk page-index capacity from metadata catalog config/deployment and
   checkpoint budgets. Account reserved tickets in capacity. Page-index eviction
   removes individual mappings without requiring whole-segment reclamation.
   Maintain open/free counts and bounded eviction cursors incrementally.
3. Add asynchronous periodic/incremental checkpoints or an equivalent bounded
   journal of disposable hints. No fsync. Bound wire and decoded image memory,
   avoid long reactor freezes, retain compatible safe recovery and corruption
   misses. Bound ordinary-process-crash rediscovery loss by documented checkpoint
   cadence/backlog; never promise power-loss durability.
4. Expose effective payload capacity and tail waste under actual geometry. A
   64 MiB segment holds only three full encrypted pages; reserve segments further
   reduce useful capacity (roughly 672 MiB for the current 1 GiB configuration).
5. Test multiple outstanding disk SQEs, out-of-order CQEs, reservation races,
   pressure eviction, full-page bytes, both cancellation fence orders and charge
   release. Actual SIGKILL after a recent checkpoint must recover old pages with
   origin disabled and bound loss of later pages; graceful-only tests do not count.

### D. Exit verification and integration

Run focused groups for each increment, then complete Rust library (including DST),
production and client/origin conformance, doctests, strict real-process groups,
affected Go controller/wire/operator/SDK/Gantry groups with race checks where
appropriate. Use the Phase 4 baseline of 754 library passes/seven ignores,
14 strict process passes, production 13/two, conformance 19/one, 32 doctests as
regression context, not as proof of new behavior. Run actual traffic through
100k-member updates/cold churn and staggered control versions. Record exact
commands, counts, timeout investigations and child cleanup below.

Final delivery must map every obligation to implementation and executable
evidence, list Phase 6 APIs and commits, report hardware limitations accurately,
and leave a clean worktree. RDMA/NIC/NUMA or isolated-host latency results are
unverified unless measured explicitly. Do not report scaffolding as completion.

## Execution results

Plan committed before source changes as `81396ff3`.

### Placement increment

Placement identity now excludes routing fields, retains compact numeric rankings
without snapshot ownership, and returns current routing leases on reuse. A 16 MiB
aggregate memory-derived default replaces 128 tiny entries. Eviction examines at
most 64 entries. Prepared memberships retain at most 64 edits for demand-driven
exact maintenance: additions/nonwinner removals/improved winners update retained
ranks; missing/worsened winners and larger deltas take cooperative cold scans.
Local queued/disk reads precede remote ranking; all production CandidatePolicy
callers pass scope into ranking. Algorithm arithmetic/vectors are unchanged.

Commands from `cmd/racer-dataplane`, all tests preceded by the mandatory external
timeout prefix:

- `cargo fmt`: passed.
- `cargo test --locked --all-features --lib topology::placement -j 2 -- --test-threads=2 --quiet`:
  nine passed, including 100k endpoint reuse, cancellation and churn/cold oracle.
- First full library: 756 passed, one failure, seven ignores. Scoped ranking had
  retained a completed operation's cancellation waker, spuriously marking the
  acquisition driver runnable. Changed to an operation-owned subscription that
  unregisters on completion. No test/assertion was removed or weakened.
- `cargo test --locked --all-features --lib blocked_metadata_leader_and_follower -j 2 -- --test-threads=1 --quiet`:
  one passed after the subscription fix.
- `cargo test --locked --all-features --lib -j 2 -- --test-threads=2 --quiet`:
  final 757 passed, zero failed, seven ignores (58.43 s). No timeout fired.

Remaining: actual 100k traffic/update latency gate, bounded warming beyond
on-demand maintenance, full control/storage obligations, exit suites and merge.

### Concurrent storage increment

The application drives batches of up to eight persistence futures concurrently.
Index PageTicket owns capacity before SQE submission; segment append/publication
leases retain generation fencing. Individual oldest mappings are evicted under
page-index pressure. Segment open/free indexes avoid slab-wide append/count scans;
eviction visits at most 64 segments and 256 mappings per turn. A real-reactor test
observes two outstanding writes before either mapping publishes, verifies disjoint
extents and reads both records back. Full out-of-order injection and process-level
pipeline/capacity/checkpoint acceptance remain to be added.

All tests below used the mandatory external timeout prefix, from the crate:

- `cargo test --locked --all-features --lib store:: -j 2 -- --test-threads=2 --quiet`:
  initial 14 failures exposed Rust's disjoint async capture dropping DirtyCleanup
  before submission. Explicitly capturing/dropping the complete guard after await
  restored ownership. Final 57 passed, one ignore after adding concurrency coverage.
- `cargo test --locked --all-features --lib concurrent_writes_reserve -j 2 -- --test-threads=1 --quiet`:
  one passed with two actual pending disk operations.
- `cargo test --locked --all-features --lib -j 2 -- --test-threads=2 --quiet`:
   758 passed, zero failed, seven ignores (59.98 s). No timeout fired.

### Integrated control and periodic recovery increment

Implemented optional authenticated delta v1 with an exact base hash, target hash,
one shared previous-generation delta (4 MiB maximum), and full-image fallback.
TLS connections recycle only after complete framing within the identity/trust
epoch. Default old-membership retention now includes bounded 30-second grace;
an authenticated stale response allows one fresh-epoch retry with original credits.
Enrollment publishes RACER_SHARES proposals, with explicit Node annotations taking
precedence. UID-bound last-admitted annotations preserve endpoint gaps on restart.

Disk page-index and checkpoint budgets are independent. Periodic five-second
checkpoint generations pause new disk batches, drain accepted writes, freeze
segment reuse, incrementally snapshot/encode, then use completion-owned filesystem
operations for write/rename. Payload reads continue. This is disposable recovery
metadata without fsync, not a power-loss durability contract. Geometry metrics
report conservative full-page payload capacity and tail waste.

Direct parent review and verification (all tests externally bounded):

- `cargo check --locked --all-features --all-targets -j 2`: passed.
- `go test -timeout=5m ./internal/racer/...`: both packages passed.
- `cargo test --locked --all-features --lib -j 2 -- --test-threads=2 --quiet`:
  763 passed, seven explicit ignores. The new staggered test first exposed a
  fixture assumption that future memberships could resolve before propagation;
  it now checks old-version grace, then completes propagation before arbitrary
  bidirectional traffic. No success assertion was removed.
- Privileged `cargo test --locked --all-features --test process_restart
  periodic_checkpoint_sigkill -j 2 -- --ignored --test-threads=1 --nocapture`:
  passed, recovering checkpointed bytes with origin disabled after actual SIGKILL.
- Scoped `make fmt`: the installed Go-1.26-built linter panicked on Go 1.27.
  Re-running the same target with the repository-pinned v2.13.1 through `go run`
  passed with zero issues; no global tool configuration was changed.

Remaining integrated review includes control delta interoperability/transport
failure gates, checkpoint CPU/memory bounds at large configured limits, all strict
process tests, and the final documentation/compatibility pass. These are not
claimed as covered by the focused SIGKILL or small simulated-cluster tests.
