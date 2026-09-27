# Streaming load: bounded incoming keepalive retention

Base: `97d97e168a8e38092f98b61d63567303c4844f48`.
Status: targeted correction locally verified; fleet acceptance belongs to deployment validation.
Source paths below are relative to `cmd/racer-dataplane`.

## Diagnosis

A targeted live layer request on `racer-dataplane-92crc` returned exactly
50,331,648 of 65,509,376 bytes, then EOF in 0.706 seconds. Request
`/ueJUygrY25JRif5kfnRbg==` successfully acquired page 2; page 3 received
Overloaded, Overloaded, Unavailable across the three candidate attempts.
Responses were associated using their canonical signed request binding.
This is candidate exhaustion, not a page-body experiment timeout.
CandidatePolicy advances once through the candidates and returns Unavailable
after exhaustion (`src/read/candidates.rs:209-275`). A separate metadata request
`4VQQi74qCTmDK/iY3vHeCA==` received three Overloaded outcomes for layer 1 page 0.

The selected downstream hotspot `racer-dataplane-cj2ms` repeatedly exhausted
its 64 connection charges. One 12-second sample observed 126 reservation
failures, 88 without an outbound idle socket available, and 21 failures at the
two-active-per-endpoint ceiling. Incoming idle registrations numbered 48-55.
A follow-up inspected the first 20 registrations: 19-20 were live and uncanceled.
Registration alone does not prove an empty socket; the regression below supplies
and checks quiet completed exchanges. These hotspot samples are not a complete
internal trace of the individual ingress request.

The mechanism is asymmetric reclamation. `HttpPool::reserve_connection` reclaims
outbound idle leases, but only waiting `checkout_peer` reclaims an accepted idle
owner (`src/http/pool.rs`, `reserve_connection` and `checkout_inner`). Production
transit deliberately uses nonwaiting `checkout` (`src/peer/stream.rs:216`) to avoid
resource cycles. Completed accepted sockets can therefore dominate the quota
while transit returns Overloaded, spending the acquisition owner's finite attempts.

## Correction

On incoming idle registration, target half the existing connection quota for
retained incoming keepalives, with a minimum of one. Each transit normally needs
both an ingress and an outbound connection; the retention target prevents idle
ingress ownership from consuming the outbound half. It changes retention policy,
not the configured total or active-resource admission.

Excess quiet owners use the existing non-consuming socket peek, exchange-local
cancellation and reactor completion fences. Queued bytes protect a new/partial
head. Active responses never register as idle. Cancellation does not release a
charge before the actual completion. The target may temporarily be exceeded by
ready heads or outstanding cancellation fences; it is not a hard quota partition.

No route budget, deadline, retry count, or per-endpoint limit is increased.
No transit operation waits for another active operation to release capacity.
The change is confined to HTTP pool retention. Only racer-dataplane needs rebuilding.

## Causal regression

`streaming_load_reclaims_incoming_keepalives` extends the production signed
1500-member, six-active-peer intersecting-route fixture. Both relays initially
receive 58 real challenge exchanges whose clients remain connected. During the
run each relay receives another completed-keepalive arrival every 5 ms (200/s).
Four clients each issue 12 successive page requests, with new signatures, over
the two crossing routes. Every response must be a page with exact seeded encrypted
bytes; no origin reacquisition is allowed. Small and 16 MiB pages both run.
All Connection, Relay, Ciphertext, Plaintext, Flight and Waiter charges drain to zero.

With only the new retention hook disabled, the same final fixture fails on Io
under connection starvation. The earlier single-batch diagnostic failed on a
signed non-page outcome. Restoring the hook passes the sustained fixture: 48
page completions per size, including 2029 arrivals per relay during the full-page
run. The fault comparison disables only this correction, not other base behavior.

The pool unit regression also fills a four-connection quota with one queued
partial head and three quiet keepalives. Exactly two quiet waits are reclaimed;
all four charges remain until completion, the partial head completes normally,
and final resource accounting is zero. Existing cancellation/drop/deadline and
arrival-after-reclaim tests remain intact.

## Verification and limitations

- Release library: 576 passed, 22 ignored.
- Intersecting-route suite: 6 passed, including sustained arrivals and prior cycles.
- Actual Fill crossing suite: 2 passed.
- Production dataplane integration: 11 passed.
- Actual Go SDK production graph: 7 variants passed, each warm and cold.
- SDK wire conformance passed with `RACER_SDK_ROOT` set. An initial broad `sdk_`
  filter also selected this opt-in test without its required variable and failed;
  the seven production graph variants had passed in that command.
- Release binary build, crate rustfmt, scoped `make fmt`, and diff whitespace check
  passed. Clippy completed in warning mode with existing repository warnings.

The fixture has six running peers, not 1500 running clients. It demonstrates
recovery from the identified idle-ownership mechanism under continuing arrivals,
not universal availability under active connection or endpoint saturation. Live
samples also observed the two-active-per-endpoint ceiling, which this patch does
not change. The parent must validate all-node c1/layer4 and then stock concurrency
64 before declaring the incident resolved.

Ignored evidence under this worktree's `tmp/`: `old-fail.log`, `final-*.log`,
`sdk.log`, and `trace-unbounded-net-node-{t7ssg,7cplm}-*.json`. Correct live uprobe
file offsets subtract 0x1000 from text virtual addresses, verified from PT_LOAD.
Earlier probes with unadjusted offsets are excluded from admission evidence.
Final cleanup checked both bpftrace processes and tracefs registrations on all
three inspected hosts. One registration left by a failed attachment was explicitly
removed; final checks show no remaining processes or registrations. No deployment,
AKS configuration mutation, image push, or fleet probe was performed here.
