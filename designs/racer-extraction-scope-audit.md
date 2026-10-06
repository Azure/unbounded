# Racer extraction reconciliation

Source snapshot: `1acedaf52a51878d19fa951fd775d43154845797`, 2026-10-06.
This supersedes the earlier extraction-only audit at `4c3b3859f`, which did not
include the concurrent controlplane, durable enrollment, secure-zeroing and verbs
work. The earlier source totals and validation counts do not describe this tree.

## Concurrent ownership decisions

- Preserve `controlplane::Published`, feeds and rollout as the publication and
  preparation owners. `src/control/publication.rs:79-93` accepts through Published;
  `:156-164` supplies Racer retention policy. Do not introduce a second runtime
  publication registry or restore the old monolithic control adapter.
- Preserve durable `racer_crypto::enrollment::Enrollment`, its secret-safe JSON
  and private-key owners, Host/Attempts fencing and shared strict identity
  validation. `crypto/src/enrollment.rs:60-168` owns enrollment state and protected
  serialization. The older enrollment-only acceptance profile is superseded, not
  a second supported validator.
- Preserve the latest BundleInstaller replay contract: exact cached delivery
  still revalidates current shared epochs (`crypto/src/lib.rs:373-392` and
  `bundle_tests::cached_delivery_revalidates_shared_keyring_epoch`). Do not restore
  the older exact-replay-after-expiry shortcut.
- Preserve backend-aware secure filesystem helpers and all zeroization changes.
  The added checkpoint module composes with these helpers; ChargedBytes retains
  the existing zeroizing transfers in application crypto. A final tree comparison
  confirms crypto, controlplane, and all application control files are byte-for-byte
  identical to the concurrent base `7a02d6c3b`.
- Preserve the concurrent bounded owned-staging reclamation implementation in
  `src/read/fill.rs`; it supersedes the extraction branch's simpler staging fix.
  The independently relocated regression still verifies live-owner rejection,
  successful idle reclamation, optional rejection and dirty-only rejection.

The remaining extraction mechanisms coexist with these newer owners: flow
FIFO/progress/fanout and charged backing; runtime offload/mailbox/affinity and
checkpoint primitives; allocator indexes/retention; HTTP relay/accept/disconnect
drivers; physical verbs discovery and one-shot fences; object-wire, peer-wire,
env-config, simulated UDS ownership and telemetry sampling/exposition. Generic
recycler and HTTP pool tests moved with their assertions; application policy and
boundary tests remain upstairs. WorkerId ordering required by controlplane is
preserved in the extracted object model. Image and notice inputs include all
three added crates alongside controlplane, with independent discovery tests.

## Exact physical Rust source accounting

Counts are newline bytes in tracked `*.rs` blobs under `cmd/racer-dataplane/`.
They include comments, blank lines and all inline/explicit tests. All measured
blobs end in a newline. This is not executable-only LoC.

| Snapshot | Application src files / lines | Workspace files / lines | Src excluding explicit test paths |
| --- | ---: | ---: | ---: |
| Original `4bf6c6c0719b3496df844192b2ba56582506bbec` | 55 / 107,588 | 119 / 181,715 | 71,090 |
| Earlier integration base `f12d57c396f90195040ba1ac402b600193253f6c` | 55 / 107,838 | 119 / 181,965 | 71,111 |
| Concurrent base `7a02d6c3b6e5a61f3dacec60480fc16dba6fb19e` | 58 / 108,806 | 124 / 191,939 | 71,554 |
| Reconciled `1acedaf52a51878d19fa951fd775d43154845797` | 58 / 104,061 | 147 / 197,628 | 67,045 |

The subset excludes `tests` path components, `tests.rs`, `*_tests.rs` and
`test_support.rs`; it still includes inline tests. The original 71,090 figure was
never the total application source size. Against the concurrent base, application
source decreases by **4,745** lines and workspace source grows by **5,689**.
Against the original source snapshot the deltas are **-3,527 / +15,913**, including
the concurrent work. This is not a net workspace shrinkage claim.

Reproduce without reading history by enumerating `git ls-tree -r --name-only`
for each exact snapshot and summing newline bytes from `git cat-file blob
<snapshot>:<path>` for the selected paths, with external command timeouts.

## Validation and limits

After locked workspace test compilation passed, bounded grouped tests completed:

| Configuration | Passed | Ignored |
| --- | ---: | ---: |
| Default helpers including docs and explicit HTTP test-util pool suite | 925 | 6 |
| Default application including binaries, integration tests and docs | 1,154 | 25 |
| Default grouped total | **2,079** | **31** |
| All-feature helpers including docs | 951 | 11 |
| All-feature application including binaries, integration tests and docs | 1,153 | 28 |
| All-feature grouped total | **2,104** | **39** |

Counts exclude repeated diagnostic runs and child-process inner successes.
The final serving-driver fixture change was checked in both feature modes;
the default full suite was rerun after that change. Default helper grouping
excludes the application, so the feature-gated HTTP pool suite is explicit.

Two test-fixture assumptions were reconciled without weakening assertions:
the disconnect case now resets TCP rather than conflating FIN with full disconnect,
preserving the external half-close contract; the drain fixture observes driver
registration before requesting a concurrent drain. All retained ownership/error
assertions remain in place.

Cargo formatting, locked workspace no-run, strict all-target/all-feature Clippy
for both helpers and application, and the deployment/notice-collector Go test
packages passed. Existing external lint annotations were preserved, not presented
as newly removed debt. No third-party dependency versions changed: four incidental
offline lock-regeneration upgrades were restored to the external base versions.

Full container execution, forced ignored hardware/benchmark/Go-launcher tests,
new performance campaigns and Miri were not run. NOTICE content was not regenerated
in this fresh worktree; collector coverage and image Cargo inputs were tested.
The prior Go formatter analyzer-version mismatch is not claimed repaired.
Cargo's nonfatal cache cleanup permission warning was not repaired on the host.
