# Decrypt failure diagnostics, October 9, 2026

## Outcome

The diagnostic image is deployed on **gpu-07-03**. One separately authorized C16 diagnostic attempt completed without reproducing an executed decrypt failure. Generic decrypt failures, all ten new reason counters, and the new failure journal remained0 through the run and read-only post-audit.

**The historical nine generic decrypt failures remain unresolved.** Restarting the DP reset counters; that is not a fix. The new C16 run had nine incomplete pulls over the whole attempt and nine AllowanceExpired Join events, not nine crypto failures. No new C32 was run or authorized by this result. All load is paused. A future C32 requires a separate decision and authorization.

The restart also recovered fewer indexed bytes than expected. Parent accepted that actual state only for cause diagnosis. The original128GiB recovery comparison remains **FAIL**, even after load grew the cache to123.5GiB. Neither the changed starting state nor the long control-projection tail permits a clean performance comparison.

## Source and deployed image

- Diagnostic source commit: `f72074bda47f08850ab0b635e650b83dc4762da3`, two files: security.rs and telemetry.rs.
- Actual integrated build source: `f9a088a22a29cf8549e1945af833f508a14199d1`.
- Build: [37970191282](https://github.com/Azure/unbounded/actions/runs/37970191282), successful linux/amd64 publication.
- Image: `ghcr.io/azure/racer-dataplane@sha256:a5ee91a9ec1a913cfe1d5e26a7a6235c9eac04969e52747594b66e6b35108a9c`.
- amd64 manifest: `sha256:7732ea35bda3d4c2aae96d1a778cbb553a475ef89ba5072e6a2b1fe4eaab8fa1`.
- Previous image: `352eb2f9b2859ffbf69496b1d27f8e807b70a4dfd34a7c5e9f80a496b722691a`, source `bb196c96753dac84f36ea86abdc11036a35a2af2`.
- New DP: `racer-dataplane-86559`, UID `26a2b1c1-d5f3-4280-b28e-9165b7b83c34`, Ready with0 restarts in saved checks,10 IO/5 crypto workers.

The built image is **not a two-file-only change relative to the previous image**. The direct source comparison records33 changed files under cmd/racer-dataplane, including runtime, storage, topology, verbs, dependencies, tests, and additional disk telemetry. The deploy provenance verified that the decrypt writer/reason code was byte-identical between the approved diagnostic commit and the integrated build source; security.rs also matched. These checks do not remove the33-file comparison confound.

No crypto algorithm, memory allowance,100ms admission allowance, catalog or workload setting was changed by ops. The image-only override CAS and one normal-grace UID-preconditioned old DP03 deletion were separately approved and recorded. No force deletion, node13 manual deletion, host repair, key change, checkpoint reset, or cache warmup was performed to conceal the failed recovery check.

## What the instrumentation measures

Source references below refer to the reviewed diagnostic source in this worktree; the deployment provenance records matching decrypt-specific bytes in f9a088. `cmd/racer-dataplane/src/telemetry.rs:403-434` registers `/debug/decrypt-failures`. `telemetry.rs:1770-1814` writes:

- `schema_version=1`, `coverage=executed_decrypt_failure_at_reap`;
- independent ring capacity64, with total, retained and overwritten counts;
- sequence, worker and CryptoId generation/sequence;
- exact Error and one fixed reason;
- optional request ID, numeric envelope page, engine failure-observation time and reap time;
- existing queue/execution nanoseconds and observed entry/final scope-check failure, or `unknown`;
- `scope_at_reap=unknown`, not a newly sampled scope result.

The ten fixed `/metrics` counters use prefix `racer_crypto_decrypt_failure_` and suffix `_total`: `cancelled`, `deadline_exceeded`, `missing_key`, `invalid_configuration`, `invalid_request`, `overloaded`, `corrupt_crc`, `corrupt_aead`, `corrupt_unclassified`, `other`. The reason mapping is at `telemetry.rs:971-1044`; metric names are at2465-2474. These are metrics, not invented reason rows in the HTTP journal.

`cmd/racer-dataplane/src/security.rs:1091-1156` records measured executed completion outcomes on I/O dequeue. Checksum-only and missing-execution records are excluded. A non-Completed outcome increments the existing generic failure count; classified failed decrypts increment a fixed reason. The ring is published at reap (`security.rs:1429`). Queue time is stopped on dequeue and execution timing includes failed/canceled accepted jobs (`security.rs:1071-1088`). Thus a generic failure is not automatically an AEAD failure or a corrupt page. The new ring cannot retrospectively classify the previous image's nine failures.

The ring has no key, nonce, object/cache ID, ETag or payload bytes. It does not add entry/final scope checks or success-path failure-ring publication. Tests assert bounded independent/nonconsuming behavior, redaction, fixed classification and once-at-reap accounting. Final-check engine injection and actual allocator-error-path coverage remain limitations, not inferred passes.

## Restart recovery and conditional diagnostic state

| Observation | Indexed bytes | GiB |
|---|---:|---:|
| Before image replacement | 137,438,953,472 | 128 |
| New DP repeated C0 samples | 117,406,957,568 | 109.34375 |
| C16 applied sample | 117,574,729,728 | 109.5 |
| C16 end sample | 128,580,583,424 | 119.75 |
| Drained and post-audit | 132,607,115,264 | 123.5 |

The18.65625GiB deficit triggered the strict rollout stop. Independent read-only safety checks passed separately; the expected137,438,953,472 bytes were never overwritten. Parent then accepted117,406,957,568 bytes as the exact diagnostic starting state, equivalent to6998 pages of16MiB. The conditional receipt binds actual DP/LG identities, source/provenance and the saved18:13 evidence; it is not recovery or performance acceptance.

The old image's recorded disk-publication total was exactly6998 pages, and6998 x16MiB equals the new recovered index117,406,957,568 bytes. Across the whole diagnostic C16 attempt, origin fills, disk-index misses and disk publications each increased906;117,406,957,568 +906 x16MiB =132,607,115,264 bytes, exactly the drained/post index. Shutdown checkpoint filtering retains entries only with a current cache and usable page-key lease (`cmd/racer-dataplane/src/app/recovery.rs:179-198`). Startup applies the same availability checks (`app/recovery.rs:563-567`) through `store/checkpoint.rs:305-319` before installation. This supports a **high-confidence key-filter explanation**, not proof that particular stored page IDs or key IDs were removed. No Secret or actual stored key material was read. Growth during the diagnostic run is allowed observation, **not restoration of the failed restart result**.

Current checkpoint slots were version3,10 shards,11,877,335 bytes each, sequences19158/19159 at18:14. Slot metadata, hashes and the public36-byte header were captured; payload/key records were not copied. There is no saved predeployment raw-slot sequence/hash pair, so no historical sequence comparison is claimed. Full available startup logs had15 lines. A512MiB checkpoint working-set cap warning is not proof that an actual cut was skipped or caused the deficit.

## One C16 cause-diagnostic attempt

Workload remained512 x256MiB, Zipf exponent1.2, seed `gpu-zipf-20261008-128g-v1`, UDS, verify=false, flights64, max threads16 and10 IO/5 crypto workers. Newcontrol-only C16 was followed by C0. No profiling session ran; opt-in remained true. No C32, restart, tuning, or repeat followed.

Preflight completed **18:27:43Z** with all generic/reason/integrity counters0, exact conditional index, original local storage/resource guards and singleton proof. Run command began **18:27:49Z** and completed **18:30:53Z** with no primary or secondary error. It targeted60s, but the actual LG applied-to-end interval was **69.106520s** because samples and diagnostics were sequential.

| Accounting interval | Successes | Incomplete errors | Successful unverified GiB |
|---|---:|---:|---:|
| Applied to end | 3,385 | 7 | 846.25 |
| End to drained | 3,883 | 2 | 970.75 |
| Whole attempt | 7,364 | 9 | 1,841 |

These are distinct denominators.96 successes preceded the applied sample. **86.414467s is the end-sample-to-drained-sample interval, not an exact ConfigMap projection latency.** C0 was requested before drain and no pre-drain annotation refresh ran; accepted work continued before the LG observed0. The measured tail also includes command and sampling time. It is retained, not silently trimmed to the target window. The command's bounded drain succeeded. Its exact wall-clock C0 acknowledgement/drain time was not separately recorded; the saved drain receipt precedes run completion at18:30:53Z. C0 was freshly reverified by the post-audit at **18:32:25Z**. Do not fabricate a more precise C0 timestamp from a sample label.

Successful bytes are successful-pull deltas x268,435,456. Verified bytes stayed0. Received-byte counters may include partial failed responses and are excluded from successful-delivery totals. This is a cause-diagnostic run with refill, changed source and starting index, not a causal performance comparison or proof of steady state.

Generic decrypt and all ten reason counters were0 before, applied, ticks10/20/30/40/50, end and drained. Keyring generation remained5. `/debug/decrypt-failures` stayed HTTP200/schema1/total0/retained0/overwritten0. There was no executed decrypt failure to associate with CryptoId, engine-check phase, or reap timing. Historical generic9 remain **UNRESOLVED**.

The other rings were not empty: gate58 schema2 recorded9 AllowanceExpired Join events, terminal recorded18 rows, and expiry16 recorded9 rows; all were retained without overwrite. These availability events are not decrypt failure classifications. Nine whole-attempt LG errors were `incomplete`, with no HTTP-status error increase. The approved5% after32/cap500 availability policy passed; hard crypto checks would have overridden it.

Raw records join all nine gate events to expiry records by request ID and page. Each request also has matching NextSlice and ClientWrite terminal rows; every recorded sent offset equals the expiry page number x16MiB. The nine fresh expiry snapshots each show current entry usage6/6 and flight usage6/6, with all54 sampled entries in DecryptCall. None shows an observed cancellation request or abandoned driver. Thus current saturation facts are present, not merely the gate's prior EntryCap observation. DecryptCall still identifies the driver-call stage, not proven backend execution; no hardware-capacity cause follows from these snapshots. These nine availability failures are distinct from the historical nine generic decrypt outcomes.

## Evidence integrity and runner correction

The previous runner lost the C32 triggering end snapshot because it checked before saving, then a second guard exception in cleanup hid the first cause. The corrected runner now saves each raw DP/LG/journal response immediately, before parsing or follow-up reads. It saves each assembled snapshot before its hard guard. On failure, C0 is requested first, direct known-LG drain runs before diagnostics, and drained/result files persist even when integrity remains failed. Original and cleanup exceptions remain distinct.

Separate full preflight uses a60s receipt, exact source/pod/container binding and new-code hashes. Active commands are clamped to an active bound of at most110s.150s is reserved for best-effort cleanup under the290s absolute phase bound; drain calls use `min(overall-5s, now+120s)` with child cleanup allowance. This is not a guarantee of successful drain; uncertain C0 or failed transport still requires parent fallback. The current attempt did not need fallback. No consumed receipt or uncertain mutation was replayed.

## Safety and retained cluster state

- Original global `racer-bench-control`, UID `dc2df38f-7fc8-4444-b19e-800079832384`, **must stay0 indefinitely** while an old node13 LG may exist.
- Single-node control: `racer-bench-single03-control`, UID `579e8466-6810-4de2-9cb3-43a6bf79f724`,0. LG `racer-loadgen-vkw7l`, UID `0edd2211-3092-4677-bf70-f06493a2e912`, applied0/in-flight0.
- Both DP overrides and LG scheduling remain node03-only. Node13 remains explicitly excluded. Its Lease was last observed frozen14:45:40.428890Z; physical processes/storage remain unverified. Do not auto-unexclude it on return. Separate C0 membership/origin restoration is required.
- Singleton version5/membership4/hash `92e0916d997f032685d1718d002f55927a1ad51be67798601df8d56501ea4900`,10/10 workers applied. Public annotation hash matched stable version metadata; no Secrets were requested.
- Independent local safety passed:8 physical devices,7 approved FD device identities,14 zero endpoint hashes; protected PV UID/spec/Bound/Retain; original protected write/discard and frozen old checkpoint hashes; exact resources/env and visible CPU ancestors. No global16-device coverage or outer-host claim.
- DP64GiB and LG4GiB limits and original budgets stayed fixed. User availability waiver qualifies only known metrics-collector OOM restarts; it does not waive node03 pressure, Racer readiness, source, device or integrity checks. Historical monitor and unknown protected-read incidents remain in their original evidence, not cleared.

## Validation and preservation

The source checkpoint records33 distinct focused Rust tests passing, offline locked typecheck, and Rust formatting. Source-phase Go1.26.9 `make fmt` passed with0 issues. The report phase ran Go1.26.9 `make fmt` once again and also passed with0 issues. These are actual scoped results, not an all-broad-suite claim. Engine-level final-scope injection and allocator-error-path test gaps remain documented. Local fake ops tests exercised parser fidelity, source/UID drift rejection, real filesystem persistence, delayed cleanup, first-error retention and conditional-state binding.

Archive: `tmp/racer-gpu-decrypt-errors-20261009-artifacts`. It contains this report, relevant new tmp records, source/build/test checkpoints, full metric/journal captures, conditional receipt, preflight, consumed run receipt and post-audit. Directories0700, files0600, manifest and SHA256SUMS verified. Build targets are excluded. Owned test binary and prior immutable archives are referenced by hash rather than recursively copied. Foreign files/caches are untouched.

The original archive and raw evidence remain immutable. `final-audit-addendum/` holds the corrected report and derived counter/request-join audit with a separate manifest and seal.

Primary evidence: `ops-decrypt-deploy-1809-*`, `ops-decrypt-index-safety-1813-*`, `ops-decrypt-conditional-state-1818.json`, `ops-decrypt-C16-1826-pre-*`, `ops-decrypt-C16-1826-run-*`, `ops-decrypt-C16-1826-post-*`, `decrypt-source-checkpoint.md`, and `decrypt-build-checkpoint.md`.

No additional live calls were made for this report. Worktree retained for parent integration; report-only commit, no push, cherry-pick or worktree removal. A future C32 is **not already cleared** by this zero-decrypt C16 result.
