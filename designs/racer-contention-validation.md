# Metadata contention scaffold validation

## Review boundary

Implemented on `racer-contention-sim`, based on `da02be5c`, in the separate
`.worktrees/racer-contention-sim` worktree. This branch has not been merged into
`racer-v2`. The simulator and its companion tests are compiled only under
`cfg(test)`. Production admission and reservation code is reused without changes.

The implementation, coverage, and interpretation limits are documented in
`cmd/racer-dataplane/README.md`, under "Metadata contention simulation".
In particular, disk staging/pending-write reclamation, production routing,
in-progress partition faults, and RDMA are outside this model. Logical occupancy
and virtual service times are not production hardware measurements.

## Executed checks

Validated revision: `573bed7f` on September 26, 2026.

```sh
cargo fmt --manifest-path cmd/racer-dataplane/Cargo.toml --check
git diff --check
make racer-dataplane-contention RACER_TEST_ARGS=--quiet
```

Result: **37 passed, 0 failed, 0 ignored**, with 644 unrelated tests filtered out.
Compilation used two jobs and tests ran serially. The fail-closed wrapper reported
`memory.max=17179869184` and `memory.swap.max=0`, covering compilation and test
descendants. This is the existing DST wrapper's 16 GiB ceiling, not the test's
observed memory use.

The tests cover generated replay, cold fleet pulls, overlapping ranges,
singleflight, cache isolation and reclamation, slow readers/disks, shared NIC and
origin service, partition recovery, deadlines, queue saturation/FIFO, cancellation,
and completion-held ownership. Small allocation-backed tests compare production
page ownership and cache reclamation with the actual simulator helpers; separate
production Fill and crypto tests cover dirty shedding and completion reap.

Independent review found simulator FIFO, global transport admission, and canceled
waiter registration defects. Separate fix agents committed each fix on its own
branch; those commits were cherry-picked here. Further separate commits strengthened
the source-NIC oracle and connected fidelity tests to actual simulator helpers.
No production defect was established by this work.

## Scale run and process memory

After the build above, the generated test executable was run separately under
the same cgroup wrapper with `/usr/bin/time -v`:

```sh
bash hack/scripts/memory-safe-run.sh -- /usr/bin/time -v \
  bin/racer-cargo/debug/deps/racer_dataplane-9a60c9fce09db663 \
  --exact contention::scenarios::two_thousand_nodes_keep_per_worker_bounds_under_cold_start \
  --test-threads=1 --nocapture
```

The executable suffix is build-specific; use the path printed by Cargo for a
different build. This measurement excludes Cargo compilation.

| Measurement | Observed result |
| --- | ---: |
| Simulated nodes | 2,000 |
| Workers per node | 2 |
| Requests completed / submitted | 4,000 / 4,000 |
| Failed / canceled requests | 0 / 0 |
| Full-page fills | 16,000 |
| Processed events | 86,000 |
| Peak pending events | 12,021 |
| Peak modeled logical byte occupancy | 603,662,352,640 bytes (about 562 GiB) |
| Delivered logical bytes | 268,435,456,000 |
| Optional persistence skips | 2,000 |
| Maximum resident set size | 36,596 KiB (about 35.7 MiB) |
| Process swaps | 0 |
| Scheduling fingerprint | `1715b431878c36d3` |

The scenario asserts per-worker quota bounds, exactly one terminal outcome per
request, exact delivered bytes, and zero remaining charges after final drain
(`cmd/racer-dataplane/src/contention/scenarios.rs`, `assert_drained`,
`assert_success`, and `two_thousand_nodes_keep_per_worker_bounds_under_cold_start`).
The standalone memory run passed and reproduced the full suite's scale report.
