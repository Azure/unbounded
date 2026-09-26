# Remaining transfer failures: transient relay admission

Base: `e0f754d3f1c7de2c2b702f21a694c15d6ac9b68d`.
Branch/worktree: `racer-fix-remaining-transfer`, `tmp/racer-fix-remaining-transfer`.
Affected artifact: **racer-dataplane only**.

## Live evidence

Read-only observations on September 26, 2026 used explicit context
`joolshev-scale-test`, namespace `unbounded-system`, kubectl request timeout 20s
and process timeout 30s. Remote bpftrace had an eight-second exit probe and
`timeout -k 2s 14s`; remote Python had a 26-second alarm and 28-second wrapper.
No remote files were written. Selected signed headers were decoded in memory;
only correlation fields and SHA-256 bindings were retained, not credentials,
signatures, or page payloads. Cleanup independently checked both traced hosts:
`pgrep -a -x bpftrace` returned 1 with no matches after each trace.

Fresh pod mapping confirmed Gantry `gantry-226zq` at `10.244.2.60`, dataplane
`racer-dataplane-pkmtt` at `10.244.2.79`, and relay `racer-dataplane-bn697` at
`10.242.125.78`. Both dataplanes ran the exact base image. Gantry ran
`4b898044c7046753ec3b51de64124236e461ab43`.

At Unix 1790440927.078225, a GET through Gantry for layer 2, digest
`a266e641d21b5be9914a8bfaad12b2b6331aec5c558f731e4efbd548b9ee0c24`, failed
HTTP 503 in 0.028796s. Its page-zero request
`AbXLvepG0g63WNZknwr/Fg==` received three binding-matched overloaded responses
from next hops `10.244.252.88`, `10.242.125.78`, and `10.242.226.78`.
No ingress admission failure occurred in this request window. The trace did
observe a later ciphertext reservation failure, which is not this request's cause.

The follow-up probe at **16:43:01.714801 UTC** again failed HTTP 503, in 0.052599s.
On the implicated relay, the selected request was:

- Flow: `10.244.2.79:54824 -> 10.242.125.78:8082`.
- Layer 2, page 0, Acquire; request `bkxOMQCHRDQQZqQz/R+kRg==`.
- Attempt `utrlGeY+Z1ScB90QOw4CWA==`.
- Binding `enG3J6VcNoHsCugOLkKgbbM34dpMNU89T5dDuLJZGRg=`.
- Request time 1790440981.7634575; matching overloaded response
  1790440981.7649994.
- Relay admission failures at 1790440981.7636375 and 1790440981.7645310,
  TID 1295508, class 10 (Relay), **used 8 / limit 8**, caller virtual address
  `0x2a0458`, symbolized to `Relay::forward`.

The trace recorded 132 relay-limit rejections and six ciphertext-limit rejections.
The two events inside the selected request window are relay-limit rejections.
Binding equality matches the peer request and response exactly; attribution of
one uprobe event to that request is temporal, because the uprobe lacks a request
ID. Association with the foreground GET uses layer, source, and time, not an
end-to-end SDK correlation ID. These are explicit limits on the evidence.

Fresh ELF disassembly verified `Admission::reserve_inner` at `0x31e350`, overload
branch `0x31e876`, executable virtual/file offset difference `0x1000`, class in
r12, limit in r13, and aggregate usage through Admission+160. Old addresses from
previous images were not reused. The first trace failed attachment because of
newline escaping; the HTTP gate prevented a probe, and cleanup found no tracer.

Raw evidence and bounded scripts are gitignored under this worktree's `tmp/`:
`unbounded-net-node-2wqt8-{inspect.log,trace-1.json,trace-2.json}`,
`unbounded-net-node-h5jf5-{inspect.log,trace-3.json}`, `relay-correlation.json`,
`inspect.py`, `trace.py`, and `summarize.py`.

## Implementation and regression

At the base, `cmd/racer-dataplane/src/peer/relay.rs:72` immediately propagates
relay permit exhaustion. `src/peer/server.rs:330-334` signs overload, and
`src/client/response.rs:109-122` permits late page failures to truncate an already
started response. All abbreviated source paths here are under `cmd/racer-dataplane`.

The fix adds a bounded, charged FIFO wait before forwarding. It preserves the
same request, signed deadline, and active-transfer cap. Permit release wakes
waiters; cancellation subscriptions and the existing worker admission-deadline
hook handle cancellation, expiry, and shutdown. The hook is reachable through
`src/app.rs:1021`. Queue entries are limited by existing `queue_entries`; every
waiter reserves Waiter and RequestContext capacity. No byte/connection/relay quota
was increased. Queued work owns no outgoing connection or ciphertext allocation.

`sdk_full_image_waits_for_transient_relay_admission` extends the actual Go SDK
fixture and production Coordinator/Fill, crypto, signed TCP transport, and relay
graph. A test-only peer-client adapter selects a first hop distinct from the
destination, forcing relay execution in the four-member graph. All subsequent
forwarding uses production Requester and PeerServer. The fixture keeps the exact
eight fleet layer sizes, four concurrent layers, two neighbor slots, and seven
full-page ciphertext charges on each node. Manifest/config are fixture objects;
layer bytes total **542,950,400**, verified by length and SHA-256.

After four SDK bootstraps, finite competing reservations occupy the eight relay
permits for 150ms. This controlled pressure reproduces the observed 8/8 admission
boundary; it does not recreate or explain the lifetime of all eight live owners.
The regression requires a nonzero actual relay-wait count and zero remaining
page, connection, and relay charges after drain.

**Old-fail/new-pass comparison:** temporarily returning Overloaded at the gate's
initial exhausted reserve restores the base fail-fast decision. The identical
SDK test fails: all first four layers stop at 16,777,216 bytes; pinned sends return
Unavailable and the Go SDK reports I/O. With FIFO waiting restored, both warm and
cold full images pass. No baseline switch remains in production code.

This is a local production-graph regression, not a kind or Gantry test and not
fleet recovery. The existing one-dataplane kind cluster was not changed; no new
multi-dataplane cluster or host inotify/sysctl change was attempted. Sustained
relay saturation, dependency cycles, ciphertext pressure, and other fleet EOF
causes remain outside the demonstrated transient-admission result.

## Checks

All commands had explicit timeouts of at most 300s, with results inspected before
continuing. Results:

- New relay admission tests: **3 passed**, covering FIFO, release wake, queue
  capacity, abandonment, charged lifetime, cancellation wake, deadline wake,
  stop, and context-exhaustion rollback. Waiting is asserted not to self-wake.
- Release library suite, serial: **569 passed, 13 ignored**.
- All six SDK variants, serial: **6 passed**, each warm and cold.
- Production dataplane integration suite, serial: **10 passed**.
- Scoped `make fmt` with `GOTOOLCHAIN=go1.26.6`: **passed, 0 issues**.
  Initial Go 1.26.0 attempt failed because go.mod requires 1.26.6.
- Clippy all-target release warning mode completed. It found existing warnings
  and one new test-module ordering warning, subsequently corrected. Final JSON
  diagnostics were checked against added diff lines and the entire new module:
  **zero diagnostics on added lines**, exit 0. Strict warning-free repository
  lint is not claimed. Logs: `tmp/final-clippy.{jsonl,log}`.
- Release binary build, crate rustfmt check, and `git diff --check`: **passed**.

The serial library run preceded the additional context-exhaustion test and
test-module relocation. The final focused run included all three new tests and
passed; the broader suite was not redundantly rerun for those test-only changes.

Reproduction and final check commands (run from this worktree):

```sh
timeout 100s cargo test --manifest-path cmd/racer-dataplane/Cargo.toml --release --lib sdk_full_image_waits_for_transient_relay_admission -- --ignored --nocapture
timeout 180s cargo test --manifest-path cmd/racer-dataplane/Cargo.toml --release --lib -- --test-threads=1
timeout 100s cargo test --manifest-path cmd/racer-dataplane/Cargo.toml --release --lib sdk_ -- --ignored --test-threads=1
timeout 180s cargo test --manifest-path cmd/racer-dataplane/Cargo.toml --release --test production_dataplane -- --test-threads=1
timeout 180s env GOTOOLCHAIN=go1.26.6 make fmt GO_PACKAGE_DIRS=cmd/racer-dataplane/tests/conformance/production_stream_fixture_test.go.txt GO_PACKAGE_PATTERNS=./pkg/racersdk
timeout 100s cargo test --manifest-path cmd/racer-dataplane/Cargo.toml --release --lib relay:: -- --nocapture
timeout 100s cargo build --manifest-path cmd/racer-dataplane/Cargo.toml --release
timeout 10s cargo fmt --manifest-path cmd/racer-dataplane/Cargo.toml -- --check
```

No push, deployment, AKS configuration mutation, durable state deletion, or
main-worktree edit was performed. Parent rollout must validate fleet behavior
using full-image success and strict byte/digest checks; these local results do
not establish fleet recovery.
