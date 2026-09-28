# Diagnostic submission isolation

## Evidence and scope

Investigation resumed from `ff7fd17b` in the existing readiness worktree. It
already contained a reactor-capacity draft, a sustained-pressure diagnostic
test, two reservation ownership tests, and updates to listener regressions and
telemetry integration notes. Those changes were retained and completed. No
saved test-result log was found, so no prior passing result is assumed.

Saved stage14 events record readiness TCP resets for `racer-dataplane-xdws9` on
`aks-ddv5-17198779-vmss00000h` at 19:46:02, 19:50:17 and 19:50:32 UTC on
2026-09-28 (`tmp/racer-stage14-events-1790625077.json:569866-569996`). The later
snapshot records restartCount 0 and a process start of 19:02:41
(`tmp/racer-stage14-rings-1790625116.json:362-385`). Diagnostic collection failed
in that snapshot (lines 492-494); these failures are not zero diagnostic error
counters or a measurement of the reactor queue.

This proves interrupted readiness with a surviving process, not its exact
production cause. Fleet CPU profiles were on another node, and minute scrapes
cannot exclude short stalls (`tmp/racer-stage14-results.md:76-83`). Exit139 and
the later host reboots are separate incidents. No fleet load, image, quota,
identity, or storage change is part of this fix.

## Proven mechanism

At `ff7fd17b`, `runtime/reactor.rs:471-483` caps the common entry table and
charges every submission to shared RequestContext memory. Diagnostic startup
reserves control slots and memory, but neither reservation is used by those
submissions (`telemetry/server.rs:47-56`). Accept retries overload; receive and
send propagate it (`telemetry/server.rs:198,226`). The connection is dropped and
DiagnosticIoError increments while the listener keeps running (lines 131-143).

Thus ordinary I/O can deny diagnostics progress even when the worker is being
polled and health is fresh. This is resource admission starvation, not proof of
CPU scheduler starvation or control-channel renewal failure. Merely moving
diagnostic polling earlier cannot guarantee a slot while existing I/O fills the
table. Increasing quotas only moves the failure threshold.

The real io_uring regression uses eight total entries, an accepted probe, three
pending ordinary readiness operations, and all remaining RequestContext bytes.
Four clients request readyz, metrics, debug/failures and healthz while pressure
remains held. A negative control temporarily routes diagnostic receive through
the ordinary API, as before the fix. It fails with `Connection reset by peer
(os error 104)`. Restoring reserved receive makes all four requests finish with
HTTP 200, without DiagnosticIoError or DiagnosticTimeout. Stale observations and
stopped admission still produce HTTP 503. See
`telemetry/server/tests.rs::diagnostic_probe_and_monitors_progress_under_sustained_queue_pressure`.
This isolates the code defect; it does not retroactively establish which quota
or other cause produced the saved fleet resets.

## Fix and bounds

Attachment partitions five entries from the existing queue ceiling and 20 KiB
from the existing 340 KiB diagnostic memory charge. Only reactor-bound diagnostic
accept/receive/send can use this capacity. Provenance, duplicate attachment,
late overcommit and per-operation bookkeeping size are checked. No extra reactor,
thread, dependency, configuration limit or externally exposed capability is added.

Reserved slots include completed-but-unconsumed results because bookkeeping is
prepaid. Kernel original/cancel fences retain abandoned submissions and their
partition. Ordinary slots retain their previous behavior: release at the kernel
fence, with unconsumed reply memory still dynamically charged. The interrupted
draft also held ordinary slots until result consumption; that unnecessary
behavior change was narrowed and covered by a regression.

Four slow diagnostic connections can still occupy the diagnostic service until
their two-second deadline. An unpolled worker cannot make progress. Readiness
still requires fresh observations from all workers (`app_health.rs:13-46,97-112`);
this change does not force unhealthy nodes ready or reserve controller traffic.

## Validation and next evidence

One negative-control test run reproduced the reset. One focused run passed all
14 diagnostic, reserved-submission, and listener-pressure tests, including
shutdown, abandonment, provenance, ordinary completion semantics and stale health.
No passing matrix was repeated. Final reactor integration ran the remaining 23
reactor tests once, explicitly skipping the three already-passing reservation
tests; all passed. Formatting used cargo fmt for the Rust changes. Required
`make fmt` failed because the installed golangci-lint was built with Go 1.26 but
the repository requires Go 1.27; it left no Go changes. Strict cargo clippy
reported 97 lint errors across existing code (including the unchanged
let-and-return in the reactor finish closure). No unrelated lint cleanup was
included. No command timed out.

To attribute a recurrence, correlate incident-time diagnostic I/O-error versus
timeout counters, queue/RequestContext admission evidence, and worker observation
freshness on the affected node, using an independent observation path if HTTP
diagnostics are inaccessible. Do not infer success from a missing scrape. This
source fix is not deployed or evidence that load6 is now safe; retain 4x4.
