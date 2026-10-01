# Reviewed page-window 1 to 2 experiment

## Implementation update: HTTP streaming

The memory calculations and copy-path observations below describe the buffered
SDK `Get` path used by the original experiment. Gantry now selects
`Client.GetStreaming` and consumes it through `Value.WriteToHTTP`. This path
does not allocate whole-page receive buffers: it validates ordered frame headers,
drains buffered read-ahead, and forwards exact payload batches of at most 256KiB.
On eligible Linux plaintext HTTP/1 connections it preserves the raw Unix socket
under one `io.LimitedReader`, allowing the Go HTTP/TCP implementation to splice
through a kernel pipe. TLS, HTTP/2, and unsupported writers/platforms copy instead.
No HTTP connection is hijacked, and range responses and keepalive remain supported.

Streaming can expose an incomplete page prefix on failure. It withholds the final
selected byte until the subscription's `Complete` frame validates, then Gantry
aborts the response on any error or short delivery. Empty responses defer flushing
until validation because there is no final byte to hold. Existing `Get`, `Read`,
and `OpenPages` keep their buffered validation behavior. Copy scratch admission
remains bounded even when an arbitrary writer cannot be interrupted; streaming
still consumes connection admission and Racer's negotiated page/byte credits.

This changes the SDK memory model, not the server acquisition window, configured
admission limits, or rollout approval. Do not apply the historical per-stream page
buffer estimates to streaming clients, or interpret reduced SDK allocations as a
fleet memory safety proof. Kernel socket/pipe storage and Racer buffers still count.
Keep all measurement and recovery gates below. A local protocol-fixture benchmark
is available as `BenchmarkHTTPStream`; it is not a production goodput result.

The older copy-versus-splice observation below is retained as experiment history,
not a description of Gantry's current delivery path.

`page_window.py` changes only the existing Gantry `config.yaml` scalar and,
in a separately reviewed stage, the two Racer DaemonSet environment overrides
in `unbounded-component-overrides/data/racer-v2.yaml`. It does not edit the
inherited Racer ConfigMap, images, resources, routing, integrity, load or other
configuration. Dependencies are the existing Python 3/PyYAML/kubectl tools.

## Why both settings, and why memory is a gate

Gantry passes `RacerPageWindow` into the SDK (`cmd/gantry/agent_racer.go:243-253`),
which uses it for page and byte credits (`pkg/racersdk/subscription.go:92-104`).
Ordered Racer acquisition uses **min(configured range window, client credits)**
(`cmd/racer-dataplane/src/read/range_stream.rs:259-266`). Raising only one from
1 to 2 cannot increase that effective acquisition window. This is a staged
two-setting experiment, not a single-variable claim. Gantry-only is a control
stage, but its SDK buffer allowance still changes.

SDK ordered buffering is capped at two pages (`pkg/racersdk/ordered.go:27-30`);
the single-credit test checks resident storage and reuse
(`pkg/racersdk/ordered_test.go:159-204`). At 16MiB/page, window 2 permits 32MiB
per active bulk stream, versus 16MiB at window 1. At four simultaneous layers
per image this is 768MiB at c6, 1GiB at c8, or 1.25GiB at c10, before runtime,
GC garbage, small requests and other allocations. The 64 bulk admission slots
could consume the entire 2Gi limit in page buffers alone. **2Gi is not a proof
of safety at window 2**, and the tool does not certify load concurrency.

Read-only inspection on 2026-09-30 at 22:50-22:51 UTC confirmed both configured
windows were 1, Gantry's live limit was 2Gi, and no Gantry env/flag shadowed its
mounted config. On node `aks-ddsv6-84072342-vmss000000`, the Gantry container
sample at 22:51:20Z had workingSetBytes=705196032, rssBytes=702054400 and zero
memory PSI avg10. This is one transient sample, not a fleet bound or headroom
approval. Racer had no container memory limit in the inspected DS, and the
same node's Racer working set was 9588101120 at 22:51:24Z. Window acquisition
floors and per-worker partitioning still apply (`src/config.rs:382-407` and
`src/app.rs:236-306`, relative to `cmd/racer-dataplane`). No budget is raised.

This follows the bounded concurrency/integrity intent in `~/design.md:23-39,53`.
The existing implementation differs from that design's client splice example
(`~/design.md:48-53`): the subscription path copies validated buffers
(`pkg/racersdk/http_transfer.go:8-14`, `ordered.go:121-125`). This tuning neither
fixes that known mismatch nor claims zero-copy behavior. No throughput gain
is established until fully verified completions improve at fixed load.

## Parent gates and exact staged commands

Do not plan while the relay rollback or another tuning operation is in progress.
The tool rejects relay=true in overrides or live Racer templates. Parent must
also attest all running pods have converged; a template GET alone cannot do that.
Establish a fixed-concurrency baseline with fleet memory/restarts/queue/error
coverage, a memory abort threshold below 2Gi, and sufficient GC headroom. Keep
the workload, catalog, verification and image pins fixed across comparisons.
Parent owns pause/drain, canary, readiness, warming, measurements and recovery.

Run from the worktree/repository root. All state directories must be fresh and
inside that root; keep private artifacts ignored and out of commits. Planning
only GETs. Inspect the full `plan.json`, `patch.json`, and byte-preserving diff.

```sh
timeout --signal=TERM --kill-after=10s 300s python3 -B hack/scripts/racer-rollout/page_window.py plan --stage gantry --context joolshev-scale-test --state tmp/window-gantry
timeout --signal=TERM --kill-after=10s 300s python3 -B hack/scripts/racer-rollout/page_window.py verify --context joolshev-scale-test --state tmp/window-gantry --approved-plan GANTRY_HASH
timeout --signal=TERM --kill-after=10s 300s python3 -B hack/scripts/racer-rollout/page_window.py apply --context joolshev-scale-test --state tmp/window-gantry --approved-plan GANTRY_HASH
```

Only the parent executes `apply` after its gates. Verify is server dry-run only.
The Gantry operator hashes the config into the DS template, triggering rollout
(`internal/operator/components/gantry/gantry.go:332-355`). Wait for that rollout,
attest credits=2, check memory, and measure the control stage before proceeding.
If unacceptable, roll back Gantry without ever applying the Racer stage.

The existing config payload is preserved by the operator, not replaced with
embedded defaults (`gantry.go:389-395`, `gantry_test.go:152-175` in that package).

```sh
timeout --signal=TERM --kill-after=10s 300s python3 -B hack/scripts/racer-rollout/page_window.py plan --stage racer --context joolshev-scale-test --state tmp/window-racer
timeout --signal=TERM --kill-after=10s 300s python3 -B hack/scripts/racer-rollout/page_window.py verify --context joolshev-scale-test --state tmp/window-racer --approved-plan RACER_HASH
timeout --signal=TERM --kill-after=10s 300s python3 -B hack/scripts/racer-rollout/page_window.py apply --context joolshev-scale-test --state tmp/window-racer --approved-plan RACER_HASH
```

After Racer rollout, attest effective window 2 in both settings, unchanged image
pins and full fleet coverage. Exclude mixed rollout/warm-up intervals. Compare
fully verified image goodput, errors, zero-goodput nodes, memory/restarts and
admission pressure, not merely VF TX. A rise in TX alone is not success.

## Reverse-order rollback

Each rollback plan uses the original forward plan and fresh live resourceVersions.
It restores the original YAML bytes, including removal of inserted Racer env.
Roll back Racer first, gate convergence, then Gantry. Each requires its own review:

```sh
timeout --signal=TERM --kill-after=10s 300s python3 -B hack/scripts/racer-rollout/page_window.py rollback-plan --context joolshev-scale-test --state tmp/window-racer-back --source-plan tmp/window-racer/plan.json
timeout --signal=TERM --kill-after=10s 300s python3 -B hack/scripts/racer-rollout/page_window.py verify --context joolshev-scale-test --state tmp/window-racer-back --approved-plan RACER_BACK_HASH
timeout --signal=TERM --kill-after=10s 300s python3 -B hack/scripts/racer-rollout/page_window.py apply --context joolshev-scale-test --state tmp/window-racer-back --approved-plan RACER_BACK_HASH
timeout --signal=TERM --kill-after=10s 300s python3 -B hack/scripts/racer-rollout/page_window.py rollback-plan --context joolshev-scale-test --state tmp/window-gantry-back --source-plan tmp/window-gantry/plan.json
timeout --signal=TERM --kill-after=10s 300s python3 -B hack/scripts/racer-rollout/page_window.py verify --context joolshev-scale-test --state tmp/window-gantry-back --approved-plan GANTRY_BACK_HASH
timeout --signal=TERM --kill-after=10s 300s python3 -B hack/scripts/racer-rollout/page_window.py apply --context joolshev-scale-test --state tmp/window-gantry-back --approved-plan GANTRY_BACK_HASH
```

Changes outside the reviewed configuration fail closed, including parent relay
rollback changing the overrides resourceVersion. Replan and review, never adjust
a saved patch's resourceVersion. Each write atomically tests UID, resourceVersion
and full data; non-target ConfigMaps and workload configuration are separately
checked. There is **no cross-resource transaction**: parent must serialize all
tuning and attest effective configuration afterward. Conflicts are not retried;
ambiguous/interrupted apply requires inspection, not blind replay. Exact already
applied config skips mutation but is not a rollout/readiness attestation.

Phases reuse the existing bounded runner: internal 285s deadline, child cleanup,
timestamped before/after-command checkpoints, command bounds <=40s. No Go files
change; do not rerun the known incompatible Go linter just for these files.

```sh
timeout --signal=TERM --kill-after=10s 300s python3 -B -m unittest discover -s hack/scripts/racer-rollout -p 'test_*.py'
```
