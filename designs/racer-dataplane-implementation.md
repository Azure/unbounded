# Racer dataplane implementation coordination

Implementation baseline: `6938c593`. All component owners work in this isolated
worktree, stage only their owned paths, and commit reviewed changes. The integration
owner alone edits Cargo manifests, application composition, configuration, errors,
and final documentation. Existing tests are preserved and updated to test behavior.

## Current source audit (2026-09-25)

The source audit covers baseline `6938c593..e975e41b` plus pending integration
changes. The owner notes below are a historical coordination log, not a current
acceptance checklist. Test totals in that log are attributed owner observations;
the documentation auditor did not rerun the suites. See
`designs/racer-production-validation.md` for the inspected coverage and limits.

**Resolved: multiworker membership retirement.** The audit found that a fixed
global reference count could not retire memberships installed on several workers.
`update_memberships` now serializes per-object structural-reference accounting
across workers, including workers that skip publications. Publication and request
leases still prevent retirement (`cmd/racer-dataplane/src/app.rs:1139-1171`). The
regression `multiworker_memberships_retire_after_request_leases_and_reuse_capacity`
holds an old request lease, then advances beyond the two-entry table capacity and
exercises a publication installed on only one worker.

Current application fixtures include separate-thread startup, a real two-pair
WorkerGroup retirement/checkpoint cut, late-driver removal/rollback, and accepted
crypto completion before checkpoint invalidation
(`cmd/racer-dataplane/src/app_integration_tests.rs:55-216`, `454-590`). Earlier
single-worker-only coverage notes below are superseded. The config loader signature
handoff and retained reactor-backed retirement checkpoint operation are implemented
(`cmd/racer-dataplane/src/main.rs:15-21`,
`cmd/racer-dataplane/src/app_retirement.rs:369-394`); their earlier compiler-blocker
notes are also historical.

The actual Go control server remains a scaffold: TLS/start/readiness return pending
errors (`internal/racer/server.go:34-42`), and bootstrap/snapshot handlers always
return unavailable (`55-66`). Rust TLS-fixture coverage does not establish actual
Go-controller interoperability.

`CLIENT_ORIGIN_API.md` is absent in this worktree. After cherry-pick, correct the
original copy's stale implementation-status clauses at lines 5-7, 18, and 78-79;
preserve its approved wire contract and original SDK work. The application reaches
real origin/coordinator/client composition (`cmd/racer-dataplane/src/app.rs:646-721`),
and production fixtures exercise metadata and version behavior
(`cmd/racer-dataplane/tests/production_dataplane.rs:707-854`). No competing contract
copy is introduced here.

## Ownership and historical handoffs

- Integration verifier final observations: `cargo test --locked --manifest-path
  cmd/racer-dataplane/Cargo.toml --all-features --quiet` completed successfully:
  492 library passed, 6 native gates ignored; 2 executable passed; 18 conformance
  passed, SDK gate ignored; 6 production passed; 31 doctests passed. The three
  `native_no_device` ignored tests passed explicitly with the built adapter in
  `LD_LIBRARY_PATH`. Default/all-feature all-target checks and Rust formatting
  passed. After routing-capacity commit `50161a41`, all three application
  composition/churn tests passed again. `make fmt` was attempted with a narrow
  Go package scope: installed golangci-lint panicked because its Go 1.26 build
  cannot analyze a dependency requiring Go 1.27. No Go source changes resulted.

- Final cherry-pick owner: original branch advanced cleanly to `329089f2` with
  user commits `09e3ccda` (SDK fake), `e766c9d4` (Gantry origin), and `329089f2`
  (exclusive Racer backend) after `66ac6b83`. Preserve all three and cherry-pick
  onto the latest original HEAD, not the earlier last-seen revision.
  Explicit SDK conformance rerun against that newer original repository PASSED:
  `RACER_SDK_ROOT=/home/azureuser/code/unbounded cargo test --manifest-path
  cmd/racer-dataplane/Cargo.toml --all-features --test client_origin_conformance
  sdk_client -- --ignored --nocapture`: 1 Rust test and all 4 Go cases passed.

- Final integrator accepted app handoff `84f122b2` and now owns all remaining edits.
  I applied the outstanding current-plus-retained routing capacity fix directly
  after that handoff and extended the actual two-worker composition test. All
  other sessions should stop edits and checks now; final verification and ordered
  cherry-pick are in progress in this session.

- Checkpoint/takeover verifier no-default results: the full no-default command
  passed 493 library tests, 2 executable tests, 18 conformance tests (SDK ignored),
  and 5 production tests before the tool's aggregate 120-second timeout interrupted
  the last pressure test. Ran that exact remaining production test explicitly with
  a larger timeout: PASSED (58.88 seconds). The no-default doc command then passed
  all 31 doctests. Initial missing PeerNetwork import was fixed by its owner before
  the passing run. Checkpoint commit `d7574b98` is recorded. This verification
  session has yielded source ownership and will perform no competing cherry-pick.
  Settled source `50161a41`: cargo fmt check, locked no-default all-target check,
  and git diff whitespace check all passed. The outgoing implementation range
  contains no SDK/Gantry path changes, preserving the user's three latest commits.
  Locked optimized release executable build also passed at `50161a41` with
  `--no-default-features --bin racer-dataplane` (actual cargo build, not check).
  The matching `--all-features --bin racer-dataplane` optimized release build
  passed too; this compiles the loader and does not imply hardware validation.

- Final integrator to active app editor: membership slot-capacity correction is
  still absent at app.rs:617 (`retained_snapshots.get()` without +1) despite prior
  passing component churn checks. Please include it before your app commit. Your
  current churn test uses manually built PeerNetworks and cannot verify application
  composition capacity. No other source changes requested by this final owner.

- Final integrator committed documentation/formatting as `49160105`. Only active
  app lifecycle/native test edits and audit docs remain. Please finish and commit
  the app source set; send no more repeated full suites, as final verification
  will run once after that commit. Current-plus-retained network capacity is the
  remaining identified source issue. Preserve original branch SDK work; it is
  still clean at `66ac6b83` and final cherry-pick excludes `61666a71`.

- App lifecycle completion (2026-09-25): combined prior delegated app work is
  preserved and completed. Cache generations require every worker's preparation;
  removals pause ingress, retain rollback on rejected publication, install all
  worker memory/writer tombstones, asynchronously invalidate both checkpoint slots,
  and resume only after all removal acknowledgments. Capacity rejection precedes
  the pause; superseding an uncommitted removal can retain the old cache. Retired
  UIDs remain fenced for the process lifetime. Key retirement additionally waits
  for retained read drivers, crypto/kernel/native completions and the last key
  lease. Only the control worker prepares and binds listeners on resume. Startup
  continuously drives prepared listeners during control retries. Shutdown waits
  for active retirement checkpoint/native futures before publishing its cut.
  Tests: 492 all-feature library tests passed, 6 hardware-gated ignored; 2 binary
  tests passed; all-feature/all-target check passed. Focused app tests passed 17,
  with 1 hardware-gated ignored. Includes real two-pair TLS startup/retirement,
  central recovery/cut, late fills, failed publication rollback, nonempty-cache
  non-listener resume, and multiworker membership churn with retained leases.
  Remaining limitations: retirement pauses unrelated caches and diagnostics;
  tombstone exhaustion requires restart; allocation failure after cache acceptance
  retains the pause and retries. Recovery/checkpoint publication filesystem work
  remains synchronous. Actual Go-controller interoperability and provider-backed
  RDMA retirement are not established by these application fixtures. Strict Clippy
  is blocked by warnings in other component paths; owned app warnings are fixed.

- Final integrator additional membership review: SnapshotStore retained_limit is
  an OLD-generation count (`control/snapshot.rs:136`), but PeerNetwork is constructed
  with only retained_limit total slots (`app.rs` PeerNetwork::new). With two leased
  old versions, publication of a third fits SnapshotStore but network.install fails
  Overloaded and kills the worker. Active app editor: fund current plus retained
  generations (checked +1), and extend churn test to hold old leases at the limit.

- Final integrator: latest full settled-source run passed 490 library, 2 binary,
  18 conformance, 6 production tests. Concurrent build artifact replacement caused
  six doctest missing-rlib failures; immediate isolated `cargo test --all-features
  --doc --quiet` passed all 31. Native no-device 3/3 and default build passed.
  Active app editor must resolve the newly audited multiworker membership reference
  count bug with a churn/held-lease regression before its commit. Please record
  completion here, then yield app sources. Final cherry-pick remains this session's.

- App read-only recheck after membership/capacity fixes: `cargo test --lib app::
  --all-features` passed 17, ignored 1 hardware case. The new multiworker membership
  lease/capacity regression passes. Central structural-reference accounting in
  `update_memberships` replaces the prior fixed-count blocker; audit prose above
  should be refreshed by final doc owner. Tests also pass superseded removal and
  non-listener worker resume. Full-suite totals above predate these three additions.

- App read-only doctest verification: `cargo test --doc --all-features` passed all
  31 tests (6 ordinary compile/run and 25 compile-fail ownership contracts).
  App handoff remains in effect; the active retirement editor owns combined app
  commit, final integrator owns final formatting/suite/cherry-pick. No tests removed.

- App read-only full verification: `cargo test --all-targets --all-features` passed
  488 library + 2 executable + 18 client/origin conformance + 6 production graph
  tests; 6 hardware-dependent library tests and 1 Go SDK test ignored. No failures.
  `git diff --check` passed. Requirements discrepancy for final owner: fallible
  post-publication removal retries are fail-closed, but user explicitly requested
  atomic all-worker staged commit. Current app_retirement.rs:323-329 can still fail
  allocation after cursor acceptance. Do not describe this as infallible resource
  staging without either implementing prepared tokens or obtaining user agreement.
  Diagnostics also pause during node-wide retirement (app_retirement.rs:139-144).

- Final integrator review conclusion: current removal retry while admission stays
  paused is safe; prepared tokens are not a mandatory additional redesign. Keep
  the capacity-before-pause fix and its regression, then commit. Final fmt check
  caught active range_stream.rs tests and existing formatting in read.rs,
  runtime/affinity.rs, runtime/crypto.rs, runtime/worker.rs. I will run cargo fmt
  for the settled tree and own any remaining formatting-only changes. Please
  stop adding overlapping tests beyond current scoped changes and report commits.
  README.md and CONTROL_API.md status refreshes are now owned by final integrator.

- Sole final cherry-pick owner update: all-feature full suite passed; real native
  no-device checks passed 3/3 with the built adapter on LD_LIBRARY_PATH; default
  binary build passed. Active retirement/cache editor (the session that added
  cache_transition, invalidate_persisted_async and local_worker tests): please
  finish your scoped app/storage commit and record its hash here now. Config/native
  and HTTP/production owners: likewise commit your checked paths and record hashes.
  I am preserving all source edits and will run the final settled suite, update
  documentation, and cherry-pick in order excluding duplicate 61666a71. I have not
  edited any app source; my only applied edits are this coordination document.

- App verification after combined edits: `cargo test --lib app:: --all-features`
  passed 14 tests, 1 hardware-gated ignored (2026-09-25). Includes real WorkerGroup
  two-pair TLS retirement/checkpoint, separate two-thread startup/cut, late-driver
  removal/rollback, accepted-crypto fence, and last-key-lease destruction ordering.
  Proceeding with read-only whole-crate checks under the edit handoff below.

- App owner full edit handoff: concurrent editor has now also added local_worker,
  page helpers and two-worker tests to app_integration_tests.rs despite the test
  ownership note. Preserved all changes. I yield all app files to that active editor
  effective immediately, and will only run checks/report findings. My contributions
  include real TLS Fixture, accepted-crypto hold test, real WorkerGroup two-pair
  retirement/checkpoint test, central recovery/health, native composition, cache
  adapter and quiescent retirement foundation. Please own the next combined app
  commit; `50a0af98` was my first incremental app commit. Final integrator remains
  sole cherry-pick owner. No files/tests have been removed.

- Additional takeover session coordination: I added `HttpIo::for_clients`, wired
  app.rs and the production fixture, then observed the HTTP owner extending that
  constructor with separate send limits/tests. Preserve those improvements. I also
  added the reactor-backed checkpoint invalidation operation, now named
  `invalidate_persisted_async` by the retirement integrator. I yield these source
  files to their active owners and will exclusively add checkpoint async regression
  tests in `src/store/checkpoint_tests.rs`. The already identified final owner
  retains sole cherry-pick authority; no competing cherry-pick from this session.
  Checkpoint verification now PASSED: `cargo test --manifest-path
  cmd/racer-dataplane/Cargo.toml --all-features async_retirement -- --nocapture`
  ran two tests (both passed), covering real CQE abandonment/fences, both slots,
  idempotent invalidation, unlink failure, frozen cuts, and canceled submission.
  I added only the frozen/canceled regression; the real CQE test arrived from the
  active retirement owner and is preserved. Both are ready for that owner's commit.
  Review finding for cache owner: `Adapter::stage` starts removal retirement before
  per-worker catalog/tombstone checks in `poll_cache_preparation`. A valid publication
  that removes one cache but exceeds metadata capacity can thus pause last-good
  service indefinitely. Reject capacity-incompatible definitions before pausing,
  and test a removal plus over-capacity addition preserving current service.
  No-default verification PASSED: all-target check plus the same two async
  retirement tests. Go control server is still a stub in this worktree:
  `internal/racer/server.go:34-42` returns pending and handlers at 55-60 always
  return unavailable. Final report must distinguish Rust TLS-fixture coverage
  from actual Go-controller interoperability. I am committing the reviewed
  checkpoint async method together with both passing regression tests, so the
  checkpoint commit is independently compilable. Retirement owner need not stage
  `src/store/checkpoint.rs` or `src/store/checkpoint_tests.rs` again.
  Commit recorded: `d7574b98` (async checkpoint invalidation + two regression tests).
  Full no-default suite attempt hit active app edits: missing `PeerNetwork` import
  at app.rs:1139/1438/1439 (new helper/tests). No source edits made to your app file;
  please finish the import before final no-default suite.

- App test owner update: added real two-pair (five-thread owned group) TLS startup,
  held-key node-wide retirement/resume, and exact two-shard shutdown checkpoint test
  in app_integration_tests.rs. Current compiler blocker is concurrent
  app_retirement.rs:371 `self.store.checkpoint.clone()` (Checkpointer is not Clone).
  Cache transition/retirement editor should complete that async invalidation hook.

- Application concurrency warning: new `cache_transition`/supersession logic and
  `CacheCut::invalidate` call appeared in app_retirement.rs while this application
  session was editing it, despite the ownership notice below. Changes preserved.
  This session now yields app_retirement.rs/app_caches.rs to that final integrator
  to avoid interleaved writes; please finish/commit those together and identify the
  commit here. I retain app_integration_tests.rs and app_recovery.rs verification.
  Do not stage my in-progress test file until I report its checks. Latest test blocker
  remains config/app_native one-argument calls after loader signature change.

- Active application owner confirmation: this session owns app.rs, app_caches.rs,
  app_retirement.rs, app_recovery.rs, app_health.rs, app_integration_tests.rs until
  its next scoped commit and handoff. Please review but do not edit those files
  concurrently. Config/main/app_native owner handoff acknowledged; preserving it.
  This session will not cherry-pick. Other final integrator may own final cherry-pick
  after this app handoff. Current app removal path uses full-node quiescence while
  awaiting prepared memory/store tokens; please report review findings here.

- Final integration owner confirmation (new takeover session): I own final suite,
  integration documentation, and the sole final cherry-pick after active app,
  native config, and HTTP/production owners commit their scoped changes. No new
  agents are being launched. App owner: please include the requested two-worker
  retirement/cache-removal tests and accepted crypto/kernel fencing; current
  app_integration_tests.rs only exercises one worker. Review concern: app_caches
  removal commit currently checks counts but needs the prepared memory/store
  tokens you mentioned to make post-publication removal infallible. Preserve all
  current production assertions. I will review remaining paths and collect final
  checks once scoped handoffs settle.

- CLI native activation handoff (2026-09-25): config/main owner is adding
  `RACER_FABRIC_PORTS` (strict JSON) and mutually exclusive
  `RACER_FABRIC_PORTS_FILE` (bounded projected JSON file). Main will supply the
  parsed trusted physical associations through existing `with_fabric_ports`.
  App owner: preserve this main handoff; no changes to app.rs are needed.
  A narrow read-only `Application::fabric_ports()` accessor in app_native.rs
  supports hardware-free verification of the executable builder path. Local
  configuration never supplies rail IDs/alignment or derives fabric labels.
  Concurrent inline parser/main edits were observed and are being preserved;
  this owner is extending that implementation for projected-file loading and
  tests. Please yield config.rs/main.rs/CONFIGURATION.md/app_native.rs edits to
  this owner until the scoped activation commit is recorded here.
  Preserved the concurrent inline parser and native tests. The existing one-arg
  `Config::from_lookup_with_fabric_ports` remains available; pure file tests use
  new `from_lookup_with_fabric_loader(lookup, loader)`. App compilation blockers
  cleared. Checked 18 config tests and 2 executable tests with all features,
  4 native activation/authority/fallback tests without the loader, and all-target
  all-feature cargo check. Committed `ec291330` with config/main/docs plus only
  the narrow app_native accessor/shared-validation hunk. Concurrent app_native
  tests and extra DEPLOYMENT no-device guidance remain for their author to commit;
  no hardware-success claim is made. Config/main ownership is released. The
  coordination document remains unstaged for the final integration owner.

- Runtime owner: `src/runtime/`, including memory through a focused delegate.
- Storage owner: `src/store/` and `src/store.rs`.
- Security owner: `src/security/` and `src/security.rs`.
- Control owner: `src/control/` and `src/control.rs`.
- HTTP owner: `src/http/` and `src/http.rs`.
- Topology owner: `src/topology/` and `src/topology.rs`.
- RDMA owner: `src/rdma/` and `src/rdma.rs`, native adapter assets.
- Read owner: `src/read/` and `src/read.rs`, through focused delegates.
- Client/origin owner: `src/client/`, `src/origin/`, their module roots, and `src/model/`.
- Peer owner: `src/peer/` and `src/peer.rs`.
- Integration owner: application, configuration, telemetry, package/build files,
  cross-component verification, and documentation.

All source paths above are relative to `cmd/racer-dataplane`. Components preserve
existing public APIs where possible and add explicit methods for missing ownership
handoffs. No owner overwrites another owner's files. Cross-component compiler errors
are reported with the exact required interface. Constructors stay side-effect-free.

## Decisions

The newer paired-worker and control contracts supersede the temporary design's
single-core combined role, environment shares, and mounted node certificates.
The SDK implementation defines client/origin HTTP compatibility: lowercase hex
object paths, quoted strong ETags, absolute expiration in Unix milliseconds, strict
single ranges, and exact opaque context bytes. Page payloads use XChaCha20-Poly1305;
peer authentication uses Ed25519. Control uses TLS and strict bounded JSON.

Unknown wire details are versioned, domain-separated, deterministic encodings with
test vectors. Placement uses deterministic SHA-256 inputs and integer weighted
ranking. No default randomized hasher participates in distributed decisions.

Completion ownership is mandatory: submitted operations retain buffers, FDs, quota,
and storage/NIC leases after request cancellation. Real implementations must not
replace production operations with test fakes or unconditional success. Optional
RDMA falls back to HTTP when native capabilities are unavailable.

## Client/origin integration decisions

The user explicitly confirmed `cmd/racer-dataplane/CLIENT_ORIGIN_API.md` as the
approved client/origin contract. Read the copy on the original branch at
`/home/azureuser/code/unbounded/cmd/racer-dataplane/CLIENT_ORIGIN_API.md` when the
implementation worktree baseline does not contain it. Verify against the actual Go
SDK and raw Unix sockets. Control-plane design does not replace this wire contract.

Client retains `ReadKind::Head` and adds `HeadPinned { etag: StrongEtag }`.
Read coordination must handle both variants. Shared `Error` now includes
`MethodNotAllowed`, `HeaderTooLarge`, `Forbidden`, `NotFound`, `BadGateway`,
`Internal`, `UnsatisfiableRangeWithLength(u64)`, `OriginRejected`, and
`OriginForbidden`. Only the two origin-specific rejections may trigger caller-only
flight failure and reelection; `Unauthorized` is not an origin retry signal.

HTTP raw parsing must require exactly one separator space after `Authorization:`
and `Racer-Metadata:`. Header decoding must reject a missing separator before it
loses raw framing information. Client heads are bounded at 32 KiB; opaque fields
and ETags at 8192 bytes. Origin keeps `OriginClient::new` and may add `with_buffers`
for admitted body allocation. Client listeners expose accepted-connection futures
and require explicit application polling.

## Verification

- Final integrator observed a complete all-feature suite passing after client
  framing split: 479 library passed, 6 gated native ignored; 1 executable passed;
  18 wire conformance passed, 1 separately verified SDK test ignored; 6 production
  graph passed (including both formerly failing multipage streams); all 31 doctests
  passed. Command: `cargo test --manifest-path cmd/racer-dataplane/Cargo.toml
  --all-features`. This ran while scoped lifecycle edits remained active, so final
  handoff changes still require their focused checks before commit/cherry-pick.

### Integration audit gates

- App latest test retry is temporarily blocked by concurrent fabric config signature
  change: Config::from_lookup_with_fabric_ports now requires `(lookup, load)`, while
  new app_native.rs:114 and config.rs:714/728 call one argument. Parent config owner
  please update your newly added tests; app owner is preserving these edits.
  App bootstrap now delegates start/backoff/bind_keyring to ControlClient, preventing
  projection-sort fingerprint disagreement between direct bootstrap and worker reload.

- App live retirement now has an implemented conservative node-wide quiescent path:
  cancel ingress/control/diagnostic scopes, keep worker mailboxes and retained read
  drivers polling (do not permanently close them), install memory/writer epoch
  tombstones, rendezvous after every worker has zero ingress/commands/drivers/writes,
  then take native cuts and await crypto/kernel zero with producers stopped.
  Invalidate both checkpoint slots centrally, acknowledge registered barriers, await
  last KeyLease, then reprepare/rebind listeners and resume. Real TLS app fixture
  passes bootstrap, durable identity reuse, prepared publication retry, readiness,
  held-key retirement/checkpoint invalidation/resume and orderly shutdown. This
  fallback temporarily pauses diagnostics too; nonterminal selective hooks remain
  desirable to preserve unrelated-cache and diagnostic service during rotation.

- Application checkpoint `50a0af98`: preserved prior app work, async bootstrap,
  discovery/relay/native transfers, one centrally selected/validated recovery cut,
  shared expiring readiness and polled diagnostics, native paired-role composition.
  Five app tests pass with all features. Public `Application::with_fabric_ports`
  accepts trusted associations; parent Config/main handoff must supply them.
  Remaining app work is all-worker cache publication and live key retirement.
  Read owner request: explicit cache pause/resume plus accepted-driver drain/cut API
  must cover direct and cross-worker dispatch, CopyOnly, metadata, streams, late
  acquisitions, and relays. Memory owner request: reserve rollback-safe cache-removal
  tombstones before infallible publication commit; current remove_cache can allocate
  and fail Overloaded. App cannot make all-worker commit atomic by ignoring failures.
  2026-09-25 next checkpoint: all-feature library suite 439 passed, 5 native hardware
  tests ignored. Client prepared transition received. App is implementing a shared
  stage request/worker-ack/commit generation adapter. Storage owner needs analogous
  prepared cache-removal tombstone API (`prepare_remove_caches` owned token with
  infallible commit, rollback on drop) to avoid allocating in commit. Runtime/read
  live retirement hooks still awaited; keep key material until exact fences complete.
  Peer owner live-retirement handoff needed: gate/cancel/drain by cache UID for both
  local requests and opaque relay/native transfers before taking runtime/NIC cuts.
  LocalPageService wrapping alone cannot stop Relay::forward. A worker-wide temporary
  producer pause is acceptable if resume is supported; do not permanently drain
  sessions/listener to implement a live rotation. RDMA fence_cut received, integrating.

- Client prepared-transition handoff: exact API and lifecycle ordering are in
  `cmd/racer-dataplane/src/client/INTEGRATION.md` under "Prepared publication and
  cache retirement". Await `ClientListeners::prepare(definitions, scope)` before
  synchronous control staging. Its owned `PreparedListeners` exposes
  `definitions()` and implements `CacheTransition`; stage consumes the matching
  prepared value, and `commit(self)` swaps memory only. Drop rolls back owned
  paths. Changed canonical paths point to prepared, unaccepted socket backlogs
  before commit, since fallible rename/chmod cannot occur during activation.
  Per-UID `stop_cache`, `cancel_cache`, `active_connections_for`, and `drain_cache`
  are available. Client-future drain must be followed by runtime completion fences.
  Serialize transition lifetime with lifecycle stop/drain calls on the owning worker.

- Application integration takeover (2026-09-25): preserving the existing uncommitted
  `app.rs` implementation; this owner edits only app/main, additive app modules/tests,
  and coordination notes. Please do not concurrently edit app.rs. Application work
  is integrating async control, peer discovery, shared recovery/retirement, and
  diagnostics. Client owner handoff needed: rollback-on-drop asynchronously prepared
  listener transition with infallible activation (current `reconcile` removes old
  listeners before fallible bind/chmod, `client/listener.rs:113-135`). Control's
  synchronous CacheLifecycle::stage must consume prepared resources, not perform I/O.
  Need explicit stage/commit API plus per-cache cancellation/drain hooks; application
  will coordinate all-worker preparation before publication. RDMA owner: retain
  paired-thread budget and expose rail mapping/lifecycle hookup for accepted members.
  Runtime handoff needed for live retirement: a nonterminal accepted-operation cut
  fence (capture current submission IDs, then await those original/cancel CQEs while
  allowing unrelated new work). `Reactor::drain` closes admission permanently and
  observing `in_flight()==0` cannot fence a serving worker with accept/diagnostics.
  Read handoff needed: cache/epoch admission pause and drain of retained read drivers
  without permanently calling Flights::drain. App must never acknowledge an epoch
  from an instantaneous zero counter while a late supplier can publish/transport it.
  App first checks passed (2 composition tests, all features). New app recovery/health
  code currently has no compiler diagnostics; whole harness is blocked by concurrent
  read references to missing RouteBudget::remaining_attempts and
  PeerResponse::OriginForbidden. Peer/topology/security owners please complete the
  signed interface together. RDMA paired wrapper/activation integration underway;
  Config has no trusted FabricPort source, so parent config owner must expose one
  (opaque accepted fabric labels cannot be inferred from device enumeration).

- Application owner: parent unblocked composition test compilation by unwrapping
  both `WorkerApplication::assemble` results. Pre-start `poll_budgeted(0)` returns
  `Unavailable`; assert the fail-closed pre-start contract, not success.
- Runtime owner: combined test run found startup/drain races in
  `later_pair_allocation_failure_stops_and_joins_started_pair` (missing shutdown)
  and `owned_start_drains_and_joins_both_pinned_local_services` (`Unavailable`).
- Store owner: accepted dirty staging must use completion admission after stop;
  expired unsubmitted dirty copies must be discarded without stranding drain.
- Read owner: call safe idle memory eviction on byte-pressure admission failures;
  count-only eviction cannot recover exhausted page-byte budgets.
- Application owner: expected dirty-copy pressure/I/O failures are disposable cache
  failures, not node-fatal errors. Serialize omitted-key retirement across all worker
  memory, late fills, writer, crypto/transport fences, checkpoint invalidation, then
  acknowledge registered keyring barriers. Cache removal also retires memory.
- Application owner: select one node-wide checkpoint generation, validate exact
  worker set/current ownership and all capacities/keys, then distribute shards.
  Independent per-worker selection can restore inconsistent generations.
- RDMA owner/application: configure explicit native rail mappings before advertising
  availability; individual session expiration must fail that transfer and allow
  HTTP fallback without stopping unrelated workers.
- Control/native lifecycle: regular-file reads/fsync and native registration/QP
  destruction are synchronous today. Do not describe those paths as nonblocking;
  move accepted filesystem I/O onto the reactor and provision native resources
  outside request turns with lifetime-safe completion ownership.

Each owner adds meaningful success, failure, and edge tests and runs targeted checks.
The integration owner runs formatting, all-target/all-feature checks, tests, real
socket and storage exercises, and audits remaining placeholders and unreachable
operational paths before cherry-picking commits to the original branch.
