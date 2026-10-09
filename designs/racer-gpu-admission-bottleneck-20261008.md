# Racer GPU admission bottleneck: flight headroom

## Finding and disposition

The strongest actionable constraint observed in this round was **worker-local
flight headroom**, not a CPU quota or reactor-submission rejection. With the
default node flight budget 64 divided over 10 I/O workers, each worker had 6 flight
entries/credits. The diagnostic C8 run produced 43 client failures: 41 matched
EntryCap/Join events and 2 matched FlightQuota/Join events, with same-call used 6,
limit 6, requested 1 facts. All 24 reactor-rejection counters stayed zero.

Adding only `RACER_FLIGHTS=128` raised the derived per-worker flight limit 6->12
and per-worker waiter budget 384->768; the per-flight waiter limit 64 stayed fixed.
The next completed C8 had **zero errors in all 7,783 pulls**, zero gate events,
and zero terminal records. The setting is left applied as the observed mitigation.
No C16 was tested with 128 and no further tuning or load is authorized by this report.

Successful unverified delivery increased from **19.997307 to 20.973133 GiB/s**
aggregate, about 4.88%. This is **not a controlled causal throughput comparison**:
both tests followed DP restarts, RAM warmed during each run, durable indexed bytes
differed, and the RNG state was not fixed across runs. The error/gate change supports
the headroom diagnosis; it does not establish maximum throughput or eliminate all
other limits. This continues the [Zipf report](racer-gpu-zipf-results-20261008.md)
and [guarded NVMe report](racer-gpu-nvme-results-20261008.md).

## Workload and single-setting change

| Item | FLIGHTS64 baseline | FLIGHTS128 comparison |
|---|---|---|
| Workload | C8;512 x256MiB =128GiB; Zipf1.2 | Same |
| Seed | `gpu-zipf-20261008-128g-v1` | Same |
| Client validation | `verify=false`, `diagnose-integrity=false` | Same |
| DP image | e740 diagnostic digest below | Same |
| Node flights | Absent env key, runtime default64 | Explicit128 |
| Derived flights per I/O worker | floor(64/10)=6 | floor(128/10)=12 |
| Derived waiter budget per worker | 6 x64 =384 | 12 x64 =768 |
| Waiters per flight | Default64 | Same |
| CPU limits | Absent, self and visible ancestors `max 100000` | Same |
| Memory limits / requests | DP64Gi,4CPU/8Gi; LG4Gi,1CPU/512Mi | Same |
| Threads | MAX_THREADS16; final10I/O+5crypto | Same |
| Cache budgets | Plaintext8GiB/ciphertext8GiB/registered2GiB | Same |
| Queue/pipes/client connections |256/16/128 node defaults | Same |
| Indexed bytes before restart/run |122.1875/123.1875GiB |125.140625/125.25GiB |

Sources: `cmd/racer-dataplane/src/config.rs:224-229` defines defaults;
`src/app.rs:2331-2357` divides node budgets; `src/read/flight.rs:598-607` uses the
partitioned flight limit for entry capacity; `src/admission.rs:128` uses it for
Flight credit. Actual envFrom was the dataplane ConfigMap, without an explicit
pod env mask. The ConfigMap CAS tested UID/RV/full old data and added only FLIGHTS128.
An operator config-hash change caused the restart. No LG, image, CPU, thread,
queue, pipe, client-connection, or cache-byte setting changed in that experiment.

Control slots still impose the worker-count floor: each worker needs12 client
connections to fund3 control slots, so128 node client connections permits10 I/O
workers (`src/admission.rs:134-137`, `src/app.rs:2406-2409`). Raising MAX_THREADS
alone would not remove that floor. This round does not show that higher connection
or CPU limits are needed.

## Exact diagnostic evidence

The new `/debug/gate-events` endpoint exposes42 fixed named counters and a protected
64-row ring. It explicitly states `terminal_cause=false`, no operation identity,
request-relative page identity, and non-atomic counter/ring snapshots. The runner
captured baseline, applied, four existing ticks/end, and a separate post-drain
snapshot, using bounded concurrent read-only queries. Canonical JSON string keys
preserved receipt hashes and sequence deduplication across processes.

| FLIGHTS64 C8 evidence | node03 | node13 | Total |
|---|---:|---:|---:|
| LG failures, activation through drain |28|15|43|
| Distinct primary terminal requests |28|15|43|
| EntryCap/Join counter delta |26|15|41|
| FlightQuota/Join counter delta |2|0|2|
| Other40 counter deltas |0|0|0|
| Gate rows captured / missed |28/0|15/0|43/0|
| Terminal rows captured / missed |53/0|30/0|83/0|
| FirstSlice/Stream records |3|0|3|
| NextSlice/Stream records |25|15|40|
| ClientWrite duplicates |25|15|40|

All primary terminals were Overloaded at Stream boundaries. Every one of the 43
primary request IDs matched a gate request ID and relative page: NextSlice's sent
bytes divided by 16 MiB, or page 0 for FirstSlice. All gate and terminal sequences
were retained across the sampled union; no ring overwrites or missing rows were
observed. ClientWrite records were duplicates of those same primary requests,
not another 40 failed pulls. ReleaseReadiness, ReleaseProcessing and Scope were
not observed as primary boundaries. Private IDs and exact rows remain in evidence,
not in this report.

The rejecting gate was concentrated on node03 worker 7: 17 of 28 failures; workers 6
and 2 each had 5, worker 4 had 1. Gate-worker IDs and client-terminal-worker IDs are
different roles and need not match. Correlation uses request ID and relative page,
not a false same-worker requirement. Node13's15 gate failures were distributed
across7 workers. See `ops-admission-correlation-audit.json`.

This is stronger than the earlier generic Ciphertext/Pipe ring pressure. The
source gives a direct, immediate failure path: for a new page, entry count at its
limit sets EntryCap and returns `Error::Overloaded`; a failed Flight reservation
sets FlightQuota and propagates the error (`src/read/flight.rs:752-766`). The
same-call observer records the gate and page before returning the result
(`:814-827`). Client first/next-slice handling records Stream boundaries
(`src/client.rs:365-371,411-463`). Together with all43 request/page matches, this
makes flight headroom the highest-priority observed constraint. It does not turn
the endpoint's request-only correlation into a general causal tracing guarantee.

At FLIGHTS128, all42 counter deltas, both gate totals, terminal totals, retained
and overwritten counts remained zero throughout. No rejection exposed a measured
used/limit12 tuple;12 is established by configuration and source partitioning, not
invented as an event. Empty valid HTTP200 responses were distinguished from missing
or unsupported endpoints.

## C8 results and all phases

The target window was60s. Collection overhead produced actual runner windows
74.665666s and74.575489s. Client rates use their own metric midpoint intervals,
not nominal60s. Client hashing was off: successful bytes are successful fixed-size
pulls x268435456. RX can include partial failed bytes and is reported separately;
the actual verified-byte delta was0. Success here means size/protocol completion,
not a checked content digest.

| Setting | Node03 MiB/s | Node13 MiB/s | Aggregate GiB/s | Window success/error03;13 | Success p50/p95/p99 ms03 | Same, node13 |
|---|---:|---:|---:|---|---|---|
| FLIGHTS64 |10000.631|10476.612|19.997307|2917/22;3055/11|119.670/466.244/497.051|129.688/465.608/495.467|
| FLIGHTS128 |10750.635|10725.853|20.973133|3138/0;3131/0|111.342/467.259/498.896|132.839/466.912/496.607|

| Phase | FLIGHTS64 node03;node13 success/error | FLIGHTS128 node03;node13 success/error |
|---|---|---|
| Activation |92/1;30/1|110/0;37/0|
| Window |2917/22;3055/11|3138/0;3131/0|
| Drain |631/5;708/3|624/0;743/0|
| Total |3640/28;3793/15|3872/0;3911/0|

The 128 run completed 6,269 successful window pulls and 7,783 total pulls with zero
errors. Baseline total reasons were 25 incomplete + 3 HTTP on node03 and 15 incomplete
on node13. Its total error ratios were 0.7634%/0.3939%, within the authorized 5%
after 32 completions/cap 500 policy. Neither result is a measured capacity ceiling;
no C16 has been tested with 128.

## Resources, tiers and interpretation

| Setting | DP CPU cores03/13 | LG CPU cores03/13 | DP read MiB/s03/13 | Physical read MiB/s03/13 | Selected RDMA TX/RX MiB/s03;13 |
|---|---:|---:|---:|---:|---|
|64|6.643/6.969|1.407/1.515|4201.674/4230.255|4189.511/4251.243|5.902/6.557;6.778/5.903|
|128|6.907/6.975|1.496/1.551|4228.944/4232.777|4235.855/4232.151|2.184/3.494;3.494/2.184|

No CFS throttling was observed. Memory64GiDP/4GiLG limits and requests remained;
sampled usage stayed below75% with max/OOM event counters0. These are sampled,
not continuous, safeguards. The prior thread profile did not show continuous
one-core saturation among usable rows. More CPU or more connections is not the
evidence-backed first intervention here.

Memory, disk and peer hits occurred on both nodes in both runs. For128 the counts
were29652/19746/46 on03 and29615/19763/17 on13. All15 approved NVMe devices had
measured reads/writes. Gate probes/readahead are included in physical counters;
events are not byte fractions. Selected RDMA rates sum only mlx5_00..07. All
other captured ports remain separate, with no alias-unsafe all-port sum. This is
tier participation, not NVMe/RDMA saturation or exclusive traffic attribution.

Both runs followed a DP restart and warmed RAM while running. The first had
122.1875/123.1875GiB durable indexed payload; the128 restart preserved the later
125.140625/125.25GiB values. RNG state was not fixed. Diagnostic overhead/schema
was the same, but these cache/order differences prevent calling the4.88% rate
change a controlled causal effect. Error removal and elimination of the observed
flight gates are the main operational result.

## Provenance, validation and preservation

| Provenance | Value |
|---|---|
| Diagnostic worktree source commit |`32807ae862194be3d40f2e10517f5085f4cc7b07`|
| Integrated source after parent cherry-pick |`a2b696cee2cca8b3a1c851ecbdba262b2c5c46c9`|
| Successful Actions build |[37830608185](https://github.com/Azure/unbounded/actions/runs/37830608185)|
| Immutable DP index digest |`sha256:e740e9f79626b21e2f987ad6e1d2ce0e6382a4cf372c265f0c52c8f136ca429a`|
| amd64 manifest |`sha256:433d6a125e878f8fc54d1739826729f29b7ba72e64a08352e9ce9c2e709144a4`|

Source checkpoint validation was focused: 18 gate tests, 1 UDS release-processing
test, 55 runtime reactor tests, 14 response tests, 13 flight-lifecycle tests and 3
protected-failure tests passed; production RDMA Clippy, Rust format and required
Go1.26.6 formatting passed. Payload-write and Scope coverage partly used production
helpers, not full end-to-end delivery. No full application-suite green claim is
made. Build identity was verified by successful Actions push logs/metadata, not
a separate registry pull. See `admission-source-checkpoint.md` and
`admission-build-checkpoint.md`; unchanged suites were not rerun for this report.

Before the diagnostic image restart, four separately approved read-only binary
captures saved checkpoint0/1 per node, **51,207,414 bytes total**, with same-open-FD
metadata, exact byte count, header/embedded checksum, transport SHA256, private
files and fsynced manifests. Sequences were2736/2763 on03 and2800/2827 on13. Each
was individually accepted after C0/pod/source/raw gates; they are not an atomic
cross-slot/node cut or authorization to restore. Directories:
`ops-cp-backup-03-slot0-1921`, `ops-cp-backup-03-slot1-next`,
`ops-cp-backup-13-slot0-next`, `ops-cp-backup-13-slot1-next`. No checkpoint identifiers
or payload metadata are published here.

The earlier Zipf restart's index8GiB->0 anomaly remains unresolved. Both subsequent
image and FLIGHTS rollouts preserved their then-current indexed gauges exactly;
that does **not** establish that the old issue is fixed or that every recovered
payload was verified. No erase, repair, restore, format or checkpoint-directory
change occurred. The older four backups were retained, not unnecessarily repeated
before128; they predate the later125GiB cache state.

Protected node03nvme0 showed an intervening **8 reads/18,432 bytes** before this
round's initial read-only capture, of unknown source. Write/discard counters,
identity and protected PV remained unchanged. Do not claim every protected
counter stayed fixed. Raw FDs covered exactly7+8 distinct approved devices;
all30 reserved endpoint regions matched the SHA256 of1MiB zeros at checks.
The old top-level/file-slab checkpoint hashes remained frozen. CPU quotas stayed
absent throughout; no new physical-CPU capacity claim follows.

One initial128 attempt was rejected by the quick gate before receipt consumption
or activation when a shared monitoring container changed. Diagnosis showed its
metrics-collector restart171->172, OOMKilled/137 with512Mi limit, then stable Ready,
healthy nodes and no Racer counter advance. The parent authorized a new fresh
receipt; the expired unconsumed receipt was not reused and no shared-health
exception was added. This was not a replay of a load attempt.

## Recommended next boundary and final state

Keep 128 as the measured operational mitigation. A possible future source change
is bounded, deadline-aware waiting for worker-local flight headroom instead of
immediate fail-fast rejection. It is **not implemented or proven here**. Any design
must preserve ownership and credit retention until native/disk completion, respect
request cancellation/deadlines, avoid holding resources needed to release headroom,
and prove wakeup/fairness and deadlock safety. Do not enlarge unrelated budgets or
claim a wait design is safe based on this benchmark alone.

Final C0 PATCH completed20:17:24.510Z; both LGs applied0/in-flight0 at
**20:17:28.831Z**. Separate full post-audit completed **20:18:22Z**. FLIGHTS128 remains
applied, derived flights12/waiters768, individual waiters64, MAX_THREADS16 and
10I/O5crypto, same8/8/2GiB budgets, no CPU quotas, memory limits/requests unchanged.
No later live call or load is implied by this report-only phase.

Evidence prefixes are `ops-admission-c8-1950` and `ops-flights128-c8-2015`, including
all phase counts, metrics, full42-counter captures, terminal rings, private request
correlations, C0 receipts and separate safety audits. The source, operations,
backup binaries and required import scripts are preserved under original
`tmp/racer-gpu-admission-20261008-artifacts/`, with0700 directories/0600 files,
source-path/hash manifest and SHA256SUMS. Build targets/caches are excluded.
Original archives are immutable; dependency mappings retain the actual scripts
and their relative baseline files without copying unrelated host caches.
Operational tools are **DONE - DO NOT REPLAY**. No secrets or private backup
payloads are committed. Only this report is committed, pending independent audit.
