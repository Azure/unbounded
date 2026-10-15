# Racer Flight-expiry diagnostics and GPU observation

Experiment date: October 8, 2026. Post-run audits crossed into October 9 UTC.

## Outcome

The bounded expiry diagnostics are deployed. The existing 100 ms allowance, FLIGHTS64, worker plan and resource budgets were unchanged. One C16 diagnostic attempt **aborted on a generic decrypt-failure counter**, with 3,901 successful pulls and three incomplete responses. It did not complete its planned 60-second window.

The new snapshots localize the three client failures more precisely: each FIFO-head admission expiry saw all six Flight entries occupied, and all six sampled entries were acquiring through `DecryptCall`. They do not identify the backend phase inside that call or explain the two counted decrypt failures. No timeout increase, extra threads, C24 or further benchmark was tried.

| Provenance | Value |
|---|---|
| Source worktree commit | `416006b18ac2d2848a20f6a93ae9801bb8862579` |
| Integrated source | `4fd2501310d79693bcaffdab2ad1044b935f448e` |
| Build | `37860244543` |
| Image | `ghcr.io/azure/racer-dataplane@sha256:a8a460418e4e10e2efcedd79f09c4600cf4426ec0b73b9ebf4de6f170417449a` |
| linux/amd64 manifest | `sha256:bc59b92fd18f5072ac5572837cde5c5aec4c9e806970bb4bc0c5b5720ef2953d` |
| Run prefix | `ops-expiry-c16-run-2359` |

## Diagnostic contract and validation

`/debug/flight-expiry` is an independent schema1 ring with 16 records and at most six entry rows per record. The writer reports total/sample/omitted/truncated counts, observation milliseconds, ages and overshoot in microseconds, queue/caller state, table limit, separate Flight/Waiter quota samples and entry lifecycle/driver-call state. Selection is index order, not oldest order. Capture is the poll observing expiry before unwind, not an exact wall-timer interrupt. See `cmd/racer-dataplane/src/telemetry.rs:1047-1095,1211-1264`.

Existing `/debug/gate-events` stays schema2/58 with its separate 64-slot ring (`src/telemetry.rs:1267-1298`). Expiry sequences are independent from gate and terminal sequences. Driver stages describe calls, not backend execution; `CandidateCompositeCall` includes nested validation and hedge/drain work, and completing means a Flight-resource destruction fence rather than backend CQ completion (`src/read/flight.rs:141-165`; expiry header at `src/telemetry.rs:1216`). No object or operation-global identity is exported.

The preserved source checkpoint records focused diagnostics tests and three-crate test-target Clippy passing. The completed read subset had **254 passes and one provider-test failure**. The provider test also failed on untouched base, but isolated assertions differed; a shared root cause was not established. A separate corrupt-copy test was corrected to deterministic simulated time and passed. Do not call the broad suite green. Rust formatting, diff check and the required Go1.26.6 formatter passed before the source commit; this reporting phase ran the Go formatter again without broad tests.

## Fixed setup and exact timeline

Nodes `gpu-07-03` and `gpu-07-13` retained the LG instances, 128 GiB Zipf dataset, 256 MiB objects, exponent 1.2 and seed `gpu-zipf-20261008-128g-v1`. Client verify=false means completed response bytes are **unverified**; it does not disable dataplane CRC or AEAD checks. Those checks remain in `src/security.rs:494-533`.

FLIGHTS64 partitions to six credits per I/O worker, with 64 waiters per flight and 384 Waiter credits per worker. MAX_THREADS16 resolves to 10 I/O and five crypto workers. Plaintext/ciphertext budgets stayed 8 GiB each, registered 2 GiB, dirty 1 GiB and context 256 MiB. DP/LG memory limits remained 64/4 GiB. Self and visible ancestor CPU quotas were `max 100000`; that is not unlimited or isolated physical hardware.

The explicit policy was counted 5% after 32 completions per node, cap500, with hard unknown/deadline/integrity stops. Preflight TTL60, quick gate20, active110, mandatory drain150 and total290-second bounds were retained under external TERM300. No policy was relaxed after the failure.

Authorization/preparation at **2026-10-08T23:56:40Z** was not run start. The run command was checkpointed at 23:58:28Z; first engine output was 23:58:52.485521Z; the C16 control patch succeeded at 23:58:53.909534Z. Exact process-launch time was not separately persisted.

Applied metrics began at 23:58:57.642935Z. The first continuation sample began at 23:59:19.223694Z and was saved at 23:59:32.380733Z. The generic decrypt-failure rule then aborted continuation. **C0/drain was affirmed at 2026-10-08T23:59:38.548292Z**. A later live proof confirmed both LGs applied0/inflight0 at **2026-10-09T00:06:38.931778Z**. An earlier truncated handoff saying “00:59?” was incorrect.

## Accounting: partial interval, not steady-state throughput

Goodput is successful pulls times 256 MiB divided by each pod's actual monotonic interval. Partial received bytes are excluded; verified baseline/current/delta stayed zero. LG midpoints in `before.json` were refreshed by the quick gate while inherited UTC fields remained from preflight, so those old UTC fields are not denominators.

| Phase | Node03 success / failure | Node13 success / failure | Actual intervals03 /13, seconds | Goodput03 /13, GiB/s |
|---|---:|---:|---:|---:|
| Before to applied | 84 /0 | 32 /0 | 7.266197 /7.212475 | 2.890095 /1.109189 |
| Applied to first tick, partial | 1023 /0 | 1037 /2 | 21.537345 /21.581898 | 11.874723 /12.012382 |
| First tick through drain | 896 /0 | 829 /1 | 18.451099 /18.426421 | 12.140198 /11.247436 |
| Whole attempt through drain | 2003 /0 | 1898 /3 | 47.254642 /47.220794 | 10.596843 /10.048539 |

Total: **3,901 successes, three incomplete failures, 975.25 GiB delivered unverified**. The error fraction is 3/3904=0.0768% overall and 3/1901=0.1578% on node13. Node13 received 368 MiB beyond successful-completion bytes: 160 MiB before the first tick and 208 MiB afterward. The partial observation's summed rate was 23.887105 GiB/s, not a completed 60-second throughput point.

## New finding: occupied decrypt-call entries at expiry

All three expiry records are on node13. The gate and expiry records match by request, request-relative page and owner worker, with equal recorded millisecond timestamps. Terminal page is absent, but the primary stream boundary's sent bytes divided exactly by 16 MiB equals the recorded page. Delivery workers can differ from owner workers.

| Request | Owner | Page | Terminal sent bytes | Caller/head age, us | Overshoot, us | Queue / limit | Waiter used / limit |
|---|---:|---:|---:|---:|---:|---:|---:|
| `b3bdd09600b21d6a495c4cafee806ab9` | 4 | 5 | 83886080 | 100064 | 66 | 6/25 | 12/384 |
| `16d60518ca1350aff3e7f4a1caa81d23` | 4 | 5 | 83886080 | 100065 | 67 | 3/25 | 9/384 |
| `491f45983ef909457351e11b2df15969` | 2 | 13 | 218103808 | 100090 | 92 | 5/25 | 11/384 |

Every caller was the FIFO head. Each snapshot reported **entries6/limit6 and Flight used6/limit6**, with all six rows sampled, omitted0 and truncated=false. Tail ages were 12491, 13216 and 6209 us. These are 18 entry **observations**, not 18 proven distinct entries: entry identities are not exported and snapshots may overlap.

All observed entries were **Acquiring / DecryptCall**, each with one waiter, one operation and one retained operation. Completing was zero; no cancellation request had been observed and no driver-abandoned flag was set. Two entries per snapshot had call ages above100 ms; the maximum was130.784 ms. There is no evidence here of dead Flights, completed entries kept by attached waiters, or a destruction-fence backlog.

The serialized local-copy path has returned from the local/disk lookup and completed plaintext reservation before entering the wrapped decrypt call (`src/read/fill.rs:1232-1249`). The stage covers the driver's await, including crypto admission, queueing, execution and reap/delivery; it is **not hardware execution time**. These snapshots establish occupied call-level state at expiry, not the backend cause of its duration. The old gate's `prior EntryCap` field remains historical; the new snapshot is the separate current observation, with quota reads still non-atomic.

All58 gate counters and all retained records were parsed for five scheduled six-endpoint captures, plus a later saved HTTP reread. Node13 had gate sequences1-3, expiry1-3 and terminal1-6, with zero overwrites or missing sequences; node03 totals stayed zero. Terminal6 is three primary NextSlice records plus three ClientWrite duplicates. No sequence-number equality was used as a join key. There is no LG trace-ID join or object identity, so two requests at relative page5 do not establish the same object page.

## Two unexplained accepted decrypt failures

Node13 `racer_crypto_decrypt_failure_total` was baseline0, applied0, first tick1, postdrain2 and latest saved2. Node03 remained0. These two errors remain unexplained and were not dismissed as cancellation.

The counter aggregates once on the owning I/O shard when an executed crypto completion is reaped; any outcome other than Completed increments failure (`src/security.rs:1071-1117,1361-1383`). Rejected pre-accept queue submissions do not produce this counted completion (`:1239-1254,1333-1357`). An **accepted** decrypt may fail for cancellation or deadline checks, missing/mismatched key, malformed `CorruptRecord`, invalid configuration, allocation `Overloaded`, CRC rejection or AEAD rejection: see `:400-420,428-538`, `src/runtime.rs:192` and `flow/src/lib.rs:407-428`. These are possible paths, not an assignment of either observed error to a cause.

CRC-rejected, AEAD-rejected, corrupt-miss and corrupt-decrypt-class counters stayed zero; both AEAD rings were empty. That does not exclude every malformed-record path: envelope/length validation can return CorruptRecord before AEAD rejection is marked (`src/security.rs:506-532`). Client verify=false does not change those checks.

The general failure rings later showed node13 total77219/retained128 and node03 total75410/retained128, all retained rows Admission/Overloaded. Baseline general-ring totals were not captured, so they are unknown, not zero. **Do not assume the exact ordinary decrypt error was once present and overwritten.** `CryptoClient::observe` records aggregate metrics and optional AEAD evidence, but does not record every ordinary failed completion's error in that ring (`src/security.rs:1368-1383`). The retained-ring loss and missing error instrumentation are distinct limitations. Pipe/Ciphertext pressure rows are not a causal explanation for these failures.

## Next diagnostic target, not tuning

The useful next split is **DecryptCall admission, queue wait, execution and reap/delivery**, with a small bounded protected record for each failed completion: actual error, CryptoID, owner worker, reap time and relevant scope condition, without payload/key material. This would distinguish an accepted job's failure from a rejected queue submission and separate where Flight credit remains held.

Do not raise100 ms to250 ms or add threads blindly. Earlier C16 runs had noisy queue means around25 ms versus execution around13 ms and crypto duties roughly73-86%, not sustained100%. Those runs had different RAM/cache state, random Zipf selections and intervals; they are not controlled same-work comparisons. The current snapshots narrow the occupied call boundary but do not prove backend saturation, scheduler starvation or a specific queue bug.

## Shared incidents, storage and final qualification

Shared monitoring OOM history is preserved. Node03 advanced203->205 before this run; the then-current lastState identified OOM at23:38:11Z, not the earlier increment's cause. Node13 advanced176->177 at23:47:59Z and177->178 at23:48:45Z during rollout. After the original run had drained, node13 advanced178->179 at2026-10-09T00:03:04Z and node03 advanced205->206 at00:04:14Z. There was no detected shared-scope change during sampled active checks; that is not continuous proof and no Racer causal claim is made.

The first post-audit passed at00:01:12Z. A later strict shared-scope check rejected the new OOM changes after preserving all requested raw endpoints and logs. Independent Racer raw/resource/PV/frozen-checkpoint checks then passed at00:06:38.931778Z, with **SharedHealth INCIDENT and unqualified validity false**. Both LGs remained Ready, applied0/inflight0. No health incident was erased to authorize another operation.

Original raw inventory16, distinct approved FD identities7+8,30 zero edge hashes, protected PV, old checkpoint hashes and resource guards passed. Protected writes/discards remained unchanged. Earlier repeated8-read/18432-byte anomalies still have unknown source; no further protected-counter delta was observed in the00:06 audit. No protected payload read, disk wipe, repair or restore was performed.

The prior checkpoint-recovery incident (an approximately8 GiB indexed cache falling to zero) remains unresolved. Latest image/config restarts preserved the full indexed payload; that does not explain the earlier incident. Final indices were exactly137438953472 bytes on node03 and137170518016 on node13. Old private backups remain untouched and are not evidence of a current recovery requirement.

## Private evidence and reproduction

Evidence is preserved outside the worktree at `tmp/racer-gpu-flight-expiry-20261008-artifacts` in the original workspace. Directories are0700 and files0600. Build targets, including `expiry-target` and `expiry-base-target`, are excluded. No raw checkpoint payload, credential or operational JSON is committed with this report.

`PRESERVATION.json` lists every copied file and hash plus original-to-preserved paths. Existing sealed archives are verified and referenced, not recursively copied. The predecessor archive `tmp/racer-gpu-flight-wait-20261008-artifacts` has SHA256SUMS seal `c9f48682e53d319cbd46150c013fdecc5ecd32b31b46d5d95bbf87fe082a9d61` and preservation-manifest hash `0380a24e2639531a6f779158e07c463136a3876568323d75ac2f1352e83fa32a`; its12 external binary references remain hash-verified in place. `SHA256SUMS` and `SEAL.json` seal this new archive. Original archives are unchanged.

Key files under `worktree-tmp/`:

- `expiry-source-checkpoint.md`, `expiry-build-checkpoint.md` and source-validation logs.
- `ops-expiry-accounting-0008-report.md`, `ops-expiry-accounting-0008-analysis.json` (SHA256 `4a058329f41088a736586d6552de119098d52b641795d8433442fd8b2d877a47`), and its verification/seal.
- `ops-expiry-c16-preflight-2358-*`, `ops-expiry-c16-run-2359-*`: source/scope receipts, once-consumed marker, all phase samples and six-endpoint captures.
- `ops-expiry-failure-audit-0005-raw-endpoints.json`, scoped logs, and `ops-expiry-failure-independent-0006-*`: failure evidence, independent safety, incident and exact C0 proof.
- Parser/fixture/capture tests, image/config/conditional receipt bindings, bounded CLIs and operational checkpoints.

Absolute paths in preserved scripts describe historical execution, not permission to replay an apply or consumed run. Reproduction starts offline with hash verification. Any further live work needs fresh authorization and unchanged safety controls. This report commits results only; parent review/integration remains separate.
