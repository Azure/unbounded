# Racer CPU profiling and GPU results

Experiment date: October 9, 2026, UTC.

## Outcome and qualification

Native CPU profiling is implemented and deployed. A corrected C8 run produced two usable 20-second profiles. **The profile data is valid for offline analysis; the whole measurement remains unqualified.** The original failed postchecks were not rewritten.

The largest identified userspace CPU cost is **AVX2 XChaCha20-Poly1305 decryption**, not AES/GCM. Five crypto workers account for about 75.5% of sampled user CPU on each node. This does not prove sustained worker saturation, explain the earlier C16 decrypt failures, or measure off-CPU queue time.

The profile-only qualification receipt is `ops-profile-qualification-0318-receipt.json`, canonical SHA256 `2549f00ef8e942b075e2966d3ff6028f8ed3d2eda832534534f52271b5748f07`. It records `profile_data_valid=true`, `whole_measurement_qualified=false`, and `historical_unqualified_valid=false`. Final C0 was confirmed at **03:19:49.550603Z**, with both load generators applied0/inflight0 and no profiler helpers.

## Feature and release

| Item | Value |
|---|---|
| Initial integrated source | `afdab78ca9cb5d85710a451568f28b8bc7e7710c` |
| Corrected integrated source | `26cc46c387107907ee429cfffa1e82886f74872a` |
| Corrected build | [Actions37875814170](https://github.com/Azure/unbounded/actions/runs/37875814170) |
| Corrected image | `ghcr.io/azure/racer-dataplane@sha256:eca33ff671fd2def5556ee4ff58b1e3e5d173638ef55f9d3c639748b3f8c4fa7` |
| linux/amd64 manifest | `sha256:b17efbeb6a29b5610b3032f76cb86fca57c551fe12a4104acef10b4629b0a96d` |
| Matching binary SHA256 | `ce8a6ffdfda6814b3ba816657a40a0dab600b422907f98e4855b8d0e3463d2bd` |
| Matching ELF build ID | `70aa48eee7ad538350f5dcdfbd15791b24cdad3c` |

Only the original `racer-v2` branch was integrated, pushed and built. Worktree branches were not published. Exact release symbols were copied from never-started containers, which were then removed. Both the original887 and correctedeca symbol sets are preserved.

Profiling is opt-in with `RACER_PPROF_ENABLED=true`; default is false. The diagnostic listener serves `GET /debug/pprof/profile?seconds=N` on port 9090. Duration is an integer from 1 to 60 seconds, default 30 when omitted. The endpoint returns raw pprof protobuf, not JSON. A successful capture can be decoded with `go tool pprof`; use the exact matching binary for local symbolization. This endpoint starts a capture, so an operational request requires separate authorization.

The backend samples discovered threads' userspace CPU at 49 Hz using Linux perf events, without SIGPROF. It caps encoded output at 16 MiB and reserves 192 MiB process-wide for the diagnostic. **That reservation is not an RSS or kernel-memory cap.** One process-wide capture/response owner remains busy until helper and response ownership finish. Release builds force frame pointers; this changes the binary even when profiling is disabled. No host capability, sysctl, seccomp or permission setting was changed for these operations. See `cmd/racer-dataplane/src/profiling.rs`, `src/profiling/perf.rs`, `src/telemetry.rs:267-332`, and `telemetry/src/server.rs`.

Operational clients must retain external deadlines. `kubectl --request-timeout` can add a `timeout` query parameter that the strict endpoint rejects. The reviewed capture transport instead used identity-checked `kubectl exec`, a bounded bash TCP GET to the podIP:9090 listener, and private HTTP status/body capture. It used no verbose auth/header logging, kubeconfig copying, or TTY. HTTP errors are not profiles and are never retried automatically.

## Validation limits

Preserved source checkpoints record focused backend, HTTP, configuration, application telemetry and lifecycle tests, including the closed-ingress shutdown fix. A native two-busy-worker fixture was decoded with Go pprof and retained both workers. Focused strict Clippy checks passed. The mapping0 correction added tests for unknown leaves/callers, recursion, weights, graph counts, location limits, and real executable-map mismatch rejection.

linux/amd64 was built and tested. **ARM64 remains locally unvalidated** because the cross C compiler was unavailable. Required `make fmt` with Go 1.26.9 ran during this reporting phase and failed on the known 11 unrelated Gantry HTTP/2 deprecation findings. Those findings were not fixed here. Do not call repository-wide lint or all platforms green.

## Unchanged experiment setup

Nodes were `gpu-07-03` and `gpu-07-13`. The load generators used UDS and a 128 GiB Zipf catalog: 512 objects of 256 MiB, exponent 1.2, seed `gpu-zipf-20261008-128g-v1`. Client `verify=false` means delivery bytes are **unverified successful pulls times 256 MiB**, not verified content. Dataplane checksum/authentication checks remained enabled.

FLIGHTS64, the 100 ms allowance, MAX_THREADS16 and the actual 10 I/O + 5 crypto worker plan were unchanged. Plaintext/ciphertext budgets stayed 8 GiB each, registered 2 GiB, dirty 1 GiB and context 256 MiB. DP/LG memory limits stayed 64/4 GiB. Self and visible ancestor CPU quotas were `max 100000`; this is not proof of isolated or unlimited hardware capacity.

Approved NVMe layout stayed 7+8 distinct device identities, with all 16 devices inventoried and 30 approved prefix/suffix guards. Node03's protected NVMe/PV was not opened for payload reads or writes. Original write/discard baselines and frozen old checkpoint hashes remained in force. No wipe, repair, restore, extra workers or timeout tuning was performed.

## Capture sequence

1. The initial887 image produced a valid3-second idle C0 profile:3,955bytes,16 weighted samples,326.53ms sampled user CPU across13 TIDs. This was a feature smoke, not bottleneck evidence.
2. Two later C8 attempts on887 ended on profile503 responses and returned to C0. The second used direct HTTP and preserved the complete `mapping_changed\n` body from both nodes. The old backend could emit that code for an unmapped user PC as well as an executable-map snapshot change; these bodies alone do not identify which branch fired.
3. The correction retains unmapped user PCs as legal mapping0 locations, with raw frames and weights unchanged and explicit unknown-frame counts. Actual executable-map changes still fail. See `src/profiling/profile.rs:38-44,255-283,316-327` and tests403-463.
4. Corrected source26cc/imageeca was rolled out at C0, changing only the two images and retaining the already-true profiling flags. Full rollout guards passed; no profile was requested during rollout.
5. One corrected C8 run, `ops-pprof-corrected-c8-0302-run`, completed its measured continuation and produced both profiles. It then failed exact post-run index equality; a later mandatory postverify also rejected a monitoring restart. No higher concurrency or repeat capture followed.

The C8 patch was acknowledged at03:02:37.880Z, both-applied evidence was saved at03:02:54.750Z, C0 was acknowledged at03:03:58.314Z, and both load generators drained at03:04:02.503Z. The continuation lasted61.349497seconds because collection and diagnostic work crossed the nominal60-second boundary.

| Capture | Node03 | Node13 |
|---|---:|---:|
| HTTP status | 200 | 200 |
| Profile bytes | 73,836 | 74,108 |
| Aggregated sample rows | 1,283 | 1,272 |
| Weighted sample count | 4,590 | 4,608 |
| Weighted user CPU, seconds | 93.673468170 | 94.040815104 |
| Encoded duration, seconds | 20.000138788 | 20.000080250 |
| Mean sampled user cores | 4.683641 | 4.702022 |

Profiles cover the early20seconds after applied C8, not the entire61-second run. Embedded profile starts are03:02:59.885102769 and03:03:01.502901257, ending03:03:19.885241557 and03:03:21.502981507. Local download receipts completed earlier, at03:03:16.368 and03:03:16.170. **Remote and local clock domains differ; exact cross-host event alignment is not established.** Use duration and sample weights, not a silently aligned wall-clock timeline.

The completed window reported about10.612/9.955GiB/s of unverified delivery, with zero counted failures. Whole-attempt baseline-to-drain totals were3,431/3,240 successful pulls and zero failures. These observations do not override the failed whole-measurement postchecks.

## Where user CPU went

Independent Go raw decoding preserved every original sample count and CPU weight. Exact ELF symbol bounds supplied qualified names; shortened Go names were not treated as unique functions. The profiles contain no mapping build IDs, so the association comes from immutable image/source/binary provenance.

| Named flat cost | Node03 | Node13 |
|---|---:|---:|
| `chacha20::backends::avx2::inner` | 46.82% | 47.44% |
| `poly1305::backend::avx2::State::compute_block` | 18.06% | 18.25% |
| Poly1305 `proc_par_blocks` | 1.61% | 1.43% |
| `crc64fast::pclmulqdq::update_simd` | 5.14% | 4.99% |

These are disjoint flat costs. Cumulative costs overlap and must not be added: `PageCryptoEngine::process` covered75.14/75.24%, `racer_crypto::open/open_page`66.49/67.21%, and `decrypt_inout_detached`65.99/66.62%. Large runtime wrappers in cumulative stacks are not evidence that their own instructions dominate.

All 15 sampled TIDs per node matched saved task start ticks and identities. Five crypto workers accounted for 75.512/75.521% of user CPU, or 3.537/3.551 average user cores. The nine named I/O threads plus caller TID1, which also runs I/O lane0, accounted for the remainder. Individual crypto workers averaged approximately 0.645 to 0.818 user cores. **That is not sustained 100% utilization.** Total sampled user cores exclude kernel CPU and are not interchangeable with process CPU totals.

Unknown mapping0 locations were1 per node, with42/38 unweighted frame references and42/38 aggregated rows. No weighted leaf CPU was mapping0; unknown callers affected1.0675/1.0200% of sampled user CPU. Counts describe the exported graph, not sample loss or weighted CPU. Unknown PCs were neither dropped nor moved to a nearby mapping.

### Runtime memory work

The initial unresolved libc flat bucket was21.68/20.75% of total user CPU. Exact runtime libc and loader DSOs were extracted from the sameeca image, not taken from the host. They did not provide function names for most of that bucket.

Instruction-level inspection found a hot REP STOS byte-fill PC at11.9826/11.0026% of total user CPU, and two PCs in a vector load/store copy loop at3.3115/3.2986%. **These are subsets of the libc bucket, not additional or additive costs.** Sampling skid and missing register values limit interpretation. The fill byte was not recorded.

Objdump's nearby `__nss_database_lookup` label was outside the sampled PCs' actual symbol extent. It is not valid NSS/DNS attribution. No hidden memset/memcpy/IFUNC function name was invented. Caller stacks linked much of the fill work to charged-buffer destruction/reclamation and crypto processing, and the copy PCs largely to disk-read staging. Source supports those paths but does not prove the exact inlined call for every sample: `flow/src/lib.rs:407-434,854-857,1155-1162`, `alloc/src/lib.rs:514-550`, and `src/store.rs:254-258`.

## Safety and why the whole result stays unqualified

Node13 indexed payload grew from 137170518016 to 137438953472 bytes: **+256 MiB, from 127.75 GiB to 128 GiB**. Node03 stayed at 128 GiB. Growth is consistent with catalog warming, not observed cache loss, but the existing exact-index postcheck rejected it. That failed receipt remains unchanged. The later independent audit found both indexes stable since 03:05 and reported runtime growth separately; it did not change restart-preservation policy.

Node13 monitoring `metrics-collector` restarted182->183 with OOMKilled/exit137 at03:04:51Z, after the recorded C0/drain transition. It changed the original shared scope. No causal attribution to Racer is established, and cross-host clock caveats still apply.

The separate03:19 audit passed original raw inventory/FD/edge/PV/write-discard/frozen-checkpoint/resource/node/Racer-identity guards, found helpers absent and C0, and observed zero increments in all eight checked integrity/corruption counters on both nodes. It retained **SharedHealth INCIDENT** rather than manufacturing a matching baseline. Earlier unknown protected reads, the earlier index-recovery incident, and the old two generic decrypt failures remain unresolved. See [expiry results](racer-gpu-flight-expiry-results-20261008.md#shared-incidents-storage-and-final-qualification) and [flight-wait results](racer-gpu-flight-wait-results-20261008.md).

## Next work, not an approved change

First investigate verified-plaintext reuse and retention to avoid repeated full-page decrypt/read cycles where per-page evidence supports it. The existing fast path and local-copy publication make this a plausible target (`src/read/fill.rs:720-734,1232-1267`; `src/memory.rs:541-557,633-649,681-699`). Current aggregate hit/miss counts do not prove redundant decrypts of a particular page or a cache bug.

Second inspect disk staging copies and buffer lifecycle costs. Preserve authentication, checksums and secure wiping. Do not weaken cryptography, remove clearing, add workers, or claim a speedup from these profiles alone. CPU profiling does not measure scheduler waits, off-CPU queue delay, NUMA/memory-bandwidth limits or the cause of prior C16 crypto errors.

## Private evidence and preservation

Evidence is preserved outside the worktree at `tmp/racer-gpu-pprof-20261009-artifacts` in the original repository. Directories are0700 and files0600. `MANIFEST.json` and `SHA256SUMS` identify copied files; `EXTERNAL_REFERENCES.json` records predecessor archive paths and seals without recursively copying their imports again.

The archive includes valid and failed raw profiles, private HTTP error receipts, consumed run receipts, all operational checkpoints/scripts/tests, both887 andeca binaries/provenance, exact runtime DSOs, independent raw/top/cumulative/tags/list reports, and the profile-only qualification. Build targets and compiler caches are excluded. Synthetic artifacts are labeled TEST. Profile addresses and paths are sensitive; operational JSON, binaries, credentials and profile data are not committed with this report.

**DO NOT REPLAY archived scripts.** Rollouts and load attempts are already applied or consumed; failed/canceled attempts are retained as evidence, not retry instructions. New operations require fresh review and authorization. The final report does not authorize load resumption. Both symbol sets and evidence must remain preserved before eventual worktree removal.

Key private references are `pprof-independent-report.txt`, `pprof-independent-final-audit.json`, `pprof-independent-runtime-report.txt`, `pprof-independent-runtime-final-audit.json`, and `ops-profile-qualification-0318-receipt.json`. Old raw receipts were hashed before and after qualification and left unchanged.
