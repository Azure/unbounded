# Persistent peer-local overload: cancellation registration leak

## Proven mechanism

Peer ingress in `WorkerApplication::poll_services` clones `task_scope` for each
accepted connection, and `PeerServer::serve_connection` narrows that clone for
each signed request. Cancellation is shared, not recreated. This is also true
on live revision `6c7fd891` (`cmd/racer-dataplane/src/app.rs:1337-1343` and
`cmd/racer-dataplane/src/peer/server.rs:277-285` on parent `3f2515b4`).

Before this fix, directory `Receipt::poll`, acquisition/copy waiter waits, and
Fill's driver-result wait called `Cancellation::register` with task-specific
wakers. Unlike `subscribe`, `register` retains entries until explicit unregister
or cancellation. These callers did neither on completion/drop. Different
`FuturesUnordered` ingress tasks therefore consumed the same worker-lifetime
1,024-entry notification budget. Once exhausted, otherwise idle requests returned
`Overloaded`. Releasing byte pressure or reducing concurrency could not remove
those stale registrations. This is a notification-capacity leak, not evidence
that page admission limits are too small or singleflight caches terminal errors.
The cap and retention behavior are in
`cmd/racer-dataplane/src/runtime/deadline.rs:78-102`; owned subscription cleanup
is at lines 39-46 of that file. The unfixed registrations are at
`read/dispatch.rs:143`, `read/flight.rs:437,535`, and `read/fill.rs:512` under
`cmd/racer-dataplane/src/` on parent `3f2515b4`.

The fix uses the existing owned `CancellationRegistration` for flight waiters and
directory receipts. Fill borrows the acquisition waiter's same registration while
waiting for its driver, avoiding a second slot and preserving one current wake
target. Subscription admission occurs before installing table/mailbox work.
Dropping a notification registration does not complete I/O, free a retained
operation, refund credits, or revoke another caller's subscription.

## Regression evidence

- `completed_peer_dispatches_do_not_exhaust_worker_cancellation`: assembled
  `WorkerApplication`, real forwarding authentication, `PeerServer::dispatch`,
  cross-worker directory, Coordinator, and signed responses. Each request has a
  new ingress task waker and the actual worker task scope; no synthetic wakers
  are inserted into cancellation state. Pre-fix request index 1023 returns signed
  `Overloaded` instead of `Miss`; fixed code completes all 1,100 requests with
  zero live waiter/flight charges and drivers between requests. The test starts
  after transport decoding, so it does not claim socket/outage reproduction.
- `completed_fill_waits_release_shared_cancellation_capacity`: real Fill,
  Flights, owned driver queue, Admission, memory/store dependencies, and crypto.
  After 1,100 cohorts under deliberately held plaintext admission, all flights,
  waiters, and drivers have drained. Releasing the charge still returns
  `Overloaded` before the fix. Fixed code reacquires and verifies `abc` on the
  same scope, without a restart or retry-limit change.
- `detached_copy_waiters_release_notifications_without_losing_live_wake`:
  1,100 abandoned copy waits, followed by two live subscribers sharing an executor
  waker. Dropping one does not remove the other's cancellation notification.
  Outstanding completion resources remain retained until explicit completion.

## Live evidence and limits of attribution

Source artifacts are the full saved stage-10 JSON rings under project `tmp/`, not
only the summary Markdown. The clean concurrency-8 artifact contains 7,500
snapshots, with 960,000 unique retained records under pod UID/worker/sequence
deduplication. Its 188,025 `PeerLocal/Overloaded` records split into **178,891 with
no attempt ID** and **9,134 with an attempt ID**; only one has an adjacent
same-worker/same-millisecond Admission record. The server's outer local-dispatch
observer has no attempt ID, whereas Coordinator's Fill observer attaches one.
See `cmd/racer-dataplane/src/peer/server.rs:521-526` and
`cmd/racer-dataplane/src/read/fill.rs:70-81`.
This is consistent with directory receipt failures before Coordinator handling;
it is not proof that every such record is this bug. Other early dispatch failures
can produce the same ring signature, and rings do not expose cancellation counts.

For comparison, the baseline full ring file has 2,563 no-attempt and 509
attempt-bearing local overload records; retained cipher6 has 58 and 40. These are
bounded retained samples, not fleet event rates or failed-pull causal counts.

The exact handoff request `49505dbdbe8c1cfc72197c829b6c7c15` appears as Relay
admission rejection on `racer-dataplane-7vn8r`, candidate overload responses on
`racer-dataplane-hcf5d`, and canceled first-slice/peer work. Request
`bd61b2028f41a84a0374560f56618fe0` includes both a no-attempt worker-0 local
overload and a worker-3 ciphertext-correlated local overload on
`racer-dataplane-j2znf`. Request identity alone does not identify one failed
attempt or resource cause. Node outage and conntrack/DNS attribution remain a
separate infrastructure investigation; this patch makes no cluster changes.

## Deployment boundary

The changed read-path implementation is identical between live `6c7fd891` and
parent `3f2515b4` before this patch. The latter includes `bc412276` opaque relay,
which was not live during stage 10. `PeerServer::opaque_relay` on that parent
returns true in production; its materialized-path switch is test-only. There is
no production opt-out in that revision.
See `cmd/racer-dataplane/src/peer/server.rs:298-303,543-548`.

Do not build the next live image from this parent and implicitly enable opaque
relay. Apply this isolated fix onto `6c7fd891` for a materialized-path image, or
first land a separate, default-off production feature flag for opaque relay.
No protocol, wire schema, dependency, controller, cluster configuration, cache
flush, or admission/retry increase is needed for the cancellation fix itself.
No claim of restored live throughput is made without a separately authorized
deployment and accepted measurement window.

### Integrated default-off relay gate

The separate follow-up adds `RACER_OPAQUE_RELAY`, default `false`, to
`cmd/racer-dataplane/src/config.rs` and passes it through worker assembly in
`cmd/racer-dataplane/src/app.rs`. Absent or explicit `false` selects the previous
materialized path; exact `true` retains the experimental opaque path. Invalid
values fail startup. Both paths retain authentication and bounded admission.
The assembled relayed-page and pressure/recovery regressions exercise both modes.

A future image from the integrated branch includes other concurrent improvements,
including worker metric sharding (`29a08798`), not just cancellation cleanup.
Keeping opaque relay off removes that experiment from the comparison but does not
make a fleet comparison a cancellation-only attribution. No image build or
deployment is part of this integration.
