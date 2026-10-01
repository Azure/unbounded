# Bounded shares application

Parent execution only; the implementation agent does not run this helper live.
The helper never changes load, caps, workloads, enrollment or key rotation.
It applies only the reviewed shares key. All clients and sparse V5 remain intact.

For each UID-sorted slice (start 0, 500, 1000; count 500), use a new state path:

```
timeout --signal=TERM --kill-after=10s 280s python3 -B hack/scripts/racer-shares-apply.py --plan tmp/racer-weight-plan-artifacts/plan.json --context joolshev-scale-test --direction apply --start 0 --count 500 --state tmp/NEW-PHASE-STATE --authorize 65ac1f3576631495585aebb42374fef3bedeea6ba82179f31ed0b57b9dc9ba47
```

Rollback uses the same plan and slice with `--direction rollback`, also a fresh
state path. Resume rereads live values, not a completion ledger. Candidate state
is skipped on apply; exact absent/1 is skipped on rollback. Any third state,
UID drift, exclusion, deletion, or enrolled shares other than 4 stops the phase.

Before writing, the helper requires global ConfigMap concurrency 0, the exact
eleven C1 caps, eight zero max-over-3m gauges, healthy loadgen/dataplane scrapes,
all 1500 node labels, at least six samples per series, early-window samples and
fresh current samples. This is sampled telemetry evidence, not a continuous
proof between scrapes. Missing or warning-bearing queries fail closed. Parent
must finish the existing drain process first; this helper does not wait for it.
All phase nodes are preflighted before any write; each write uses a fresh GET.

The drain gauges are applied concurrency, loadgen in-flight, active requests,
active fills, peer exchanges, pending disk writes, active deliveries and worker
relay use. Control data/UID is bracketed around the gate and checked again after
preflight and after writes. This is not a lease preventing a concurrent load
writer: parent must remain the sole load owner and keep C0 until final attestation.

JSON patches test UID and fresh resourceVersion; existing shares additionally
get a value test. For absent shares, absence is inspected in the GET and protected
by resourceVersion, never tested as JSON null. Only the shares child is changed,
not the annotation map. Rollback removes originally absent shares or restores 1.
Controller last-admitted may legitimately lag while changing membership.

An ambiguous command result is reconciled by another GET: desired means observed
success, permitted source with changed RV permits a rebuilt retry, unchanged RV
or third state stops. At most three writes per node. Generic command failures
are not labeled HTTP conflicts; private output/stderr is never logged.

The dynamic queue has at most 16 futures, no 500-item prequeue. Any failure stops
dispatch and signals active process groups. SIGTERM/SIGINT and the internal
220-second alarm do the same. Every command has external TERM/kill-after10 bounds,
its own group, bounded communicate and group cleanup/reap. Partial writes may
have committed even after cancellation: inspect live state on the next fresh
phase. The 280-second external bound reserves cleanup headroom. Do not retry
unbounded or weaken guards to finish 500 nodes in one phase.

`events.jsonl` contains only node identities, share values, versions, phase/map
identity and sanitized outcomes. No full Node objects, credentials or payloads.
Keep these parent-owned progress records alongside the reviewed plan/rollback.

Shares updates can coalesce into publications. Neither 500 successful patches
nor a Ready count proves publication application. After all phases, parent runs
`racer-membership-attest.py` with the exact image and `--candidate-plan`, requiring
the full worker-applied hash/version sweep before deciding to resume. Rollback
also stays at C0 and requires baseline membership verification. No controller
pause, key rotation change, separate 400/100 rollout, or automatic load resume.

Validation: `timeout --signal=TERM --kill-after=10s 30s python3 -B hack/scripts/racer-shares-apply_test.py`.
