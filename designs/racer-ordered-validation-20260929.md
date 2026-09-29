# Ordered subscription validation, 2026-09-29

## Revision and independently executed gate

Reviewed integration HEAD `db6895fd`, including the ordered Rust pipeline,
SDK request-local reuse/read-ahead, live test additions `c8b9a7d7`, fixture fix
`bd91a0ca`, terminal-read cleanup join `288415e1`, and mixed-mode fairness
`db6895fd`. This pass changed documentation only.

From the existing `racer-ordered-integration` worktree:

```sh
timeout --signal=TERM --kill-after=10s 300s \
  env GOTOOLCHAIN=go1.26.6 RACER_SUBSCRIPTION_INTEROP=1 \
  TMPDIR=/home/azureuser/code/unbounded/tmp \
  go test -race ./pkg/racersdk -run '^TestRustSubscriptionInterop$' \
  -count=1 -timeout=5m -v
```

Result: PASS, all 15 Go subtests, no Go race report; package elapsed 91.336s.
The Rust `go_sdk_subscription_server` test also passed and drained normally.
Its reported charged plaintext peak was 50,335,744 bytes against a 67,108,864-byte
limit; final plaintext, flight, and waiter charges were zero. The existing harness
invokes locked Cargo with `subscription-interop` and its own 230-second TERM/kill
timeout, then requests shutdown and reaps the subprocess
(`pkg/racersdk/rust_subscription_interop_test.go:36-75`).

Coverage includes empty/small/multipage ordered reads, 512 MiB + 13-byte Get and
DownloadTo, partial ordered ranges with one/two page credits, fragmented releases,
page/byte-credit socket silence, final Complete while a lease is held, cancellation,
recovered connection admission, and destination failure. Ordered large/partial
reads assert cleaned buffers, credits, and admission
(`pkg/racersdk/rust_subscription_interop_test.go:116-417`,
`pkg/racersdk/ordered_test.go:45-60`).

This is production Rust ClientListeners/read-graph/crypto over a real local UDS
with generated origin and fixture publication. It is not a deployed cluster,
distributed fairness stress test, persistence test, RSS/Go heap high-water
measurement, or Rust race-sanitizer run. No broad unit suite was rerun for this
documentation pass. Inspected pipeline/fairness assertions are cataloged in
[replacement subscriptions](racer-hot-subscriptions.md#evidence-and-validation-scope).

Scoped formatting/lint gate also passed with zero issues and no Go source diff:

```sh
timeout --signal=TERM --kill-after=10s 300s \
  env GOTOOLCHAIN=go1.26.6 TMPDIR=/home/azureuser/code/unbounded/tmp \
  make fmt GO_PACKAGE_PATTERNS=./pkg/racersdk/... GO_PACKAGE_DIRS=./pkg/racersdk
```

## Fixture benchmarks

The integration handoff supplied the following matched-fixture observations;
they were not rerun in this documentation pass:

| SDK Copy fixture comparison | Throughput | Allocated bytes per operation |
| --- | --- | --- |
| Baseline | 2.48-2.58 GB/s | Approximately 1 GiB |
| Integrated reuse/read-ahead | 4.48-4.82 GB/s | Approximately 32 MiB |

These are fixture results, not deployed performance. Allocated bytes per operation
are cumulative allocation, not live heap or RSS. Small per-page bookkeeping still
allocates; the reusable payload buffers are the bounded part.

The old scalar byte-fill generator had address-sensitive/linked-code-layout
overhead that could dominate the transport benchmark even without SDK execution.
It is not a sound basis for attributing throughput changes to the pipeline.
`bd91a0ca` replaces the scalar fill with bulk doubling copies and adds an isolated
generator benchmark (`pkg/racersdk/client_test.go:94-107`,
`pkg/racersdk/benchmark_test.go:249-269`). Comparisons must use the same unbiased
generator on both revisions; do not compare an old scalar-fixture baseline to a
new bulk-copy fixture. The fixture's Go server is not the live Rust graph
(`pkg/racersdk/benchmark_test.go:20-113`).

No blocker was observed for this documentation/live-interoperability gate.
Deployed throughput, multi-node workload behavior, and process-memory claims
still require their own evidence; this run and the handoff benchmarks do not
establish them.
