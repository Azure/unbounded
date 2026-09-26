# Peer response staging under ciphertext pressure

Base: `2b1de5f74d72ef30c8419885f81e0c300d0dc8ef`. Worktree and branch:
`tmp/racer-fix-request-stalls`, `racer-fix-request-stalls`. Date: 2026-09-26.
One additional read-only code investigator was launched for this task.

## Confirmed mechanism and fix

At the base, `PeerServer::serve_connection` sends the signed success head, then
allocates and copies a second full-page ciphertext buffer for HTTP transmission
(`cmd/racer-dataplane/src/peer/server.rs:271-284` at the base). A relay has already
received and admitted that page. If the remaining quota cannot hold another copy,
`WireBuffer::for_cache` returns `Overloaded` immediately
(`cmd/racer-dataplane/src/peer/transfer.rs:27-38` at the base), closing the socket
after advertising a body it never sends. This mechanism predates `2b1`; that commit
removed a separate receive/decode double charge. It is not evidence that `2b1`
introduced a new regression.

The server now moves the existing page into `SendPage` and sends from its immutable
allocation (`cmd/racer-dataplane/src/peer/server.rs:271-290`). `SendPage` rejects
mutable access (`cmd/racer-dataplane/src/peer/transfer.rs:18-30`). The existing HTTP
partial-send loop retains the owner (`cmd/racer-dataplane/src/http/io.rs:475-498`);
the reactor submits sends using read-only bytes and retains the owner in completion
state (`cmd/racer-dataplane/src/runtime/reactor.rs:505-534`). No quota, deadline,
retry, wire format, or persistent-state change is needed.

## Read-only AKS evidence

All kubectl calls used explicit context `joolshev-scale-test` and request timeout
20s. Kubectl subprocesses were bounded by 30s, remote tracing by 24s with a 22s
alarm, and bpftrace by 14s with automatic detach. No AKS configuration, pod, image,
or durable state was changed. Captures retain selected protocol fields, not page
payloads or credentials. Temporary uprobes observed the running binary only.

### Startup symptoms and persistent request failures

Initial observation: dataplane 1419/1500 ready, Gantry 1448/1500 ready. Sample
`racer-dataplane-whvb7` had two restarts; its previous process logged
`2026-09-26T06:21:19.909777006Z racer-dataplane: DeadlineExceeded`, with exit code 1
after a 30s lifetime. `racer-dataplane-69pwt` had one analogous 30s exit;
`racer-dataplane-nfq55` had none. These logs do not identify the internal startup
operation that expired. The original post-rollout 0.5ms/10s manifest failures are
not proven to be the transmit-staging bug.

The next three-node probe had stable UIDs/restarts and successful manifests, but
zero complete images: 11 successful layers, eight premature EOFs, five HTTP 503s.
Evidence in the deployment worktree:
`tmp/racer-2b1-request-stalls-sample.json` and `.md`.

After all three DaemonSets reached 1500/1500 ready, another three-node probe still
had zero complete images: nine successful layers, six premature EOFs, nine HTTP
503s. Each sampled loadgen added 11 errors and zero full successes, with stable
process counters and pod restarts. Evidence:
`tmp/racer-2b1-request-stalls-ready-sample.json` and `.md` in the deployment worktree.
The final Prometheus snapshot reports 67 cumulative full successes on 65 pods and
3,595,497 errors, with 1500/1500 scrape coverage. These cumulative counters are not
a controlled before/after causal comparison. Readiness is not image integrity.

### Exact failed layer path

An explicit GET through `gantry-226zq` returned HTTP 200 and Content-Length
59,757,056, then EOF at 16,777,216 bytes in 0.722s. Layer:
`sha256:2115d07a4bfcddb9d1c4322c22fda03d1f24032560ae938b8c8d224eef408c1d`.
The captured page-1 request `W/vvkdi3YrTuyfdcrG7hvw==` tried:

1. `10.242.125.72`, returning a signed downstream overload from
   `80d587ad-66b1-45a4-b82d-74e2333a33a6`.
2. `10.242.226.72`, returning unavailable.
3. `10.241.35.81`, with zero delegated attempts remaining.

Evidence: this worktree's `tmp/unbounded-net-node-2wqt8-wire.json:1-295`.
Pod mapping in `tmp/map.json` identifies the downstream relay as
`racer-dataplane-w27wp`, `10.241.155.73`, on
`aks-ddv5-17198779-vmss0000d5`; sampled peers all use the `2b1` image.

A simultaneous packet/uprobe capture on that relay observed five ciphertext
overloads from server transmit staging. `addr2line` identifies the reserve caller
as `WireBuffer::for_cache` at ELF return address `0x1433e7`, and its caller as
`PeerServer::serve_connection` at `0x2c70a7`. Receive staging and reserve-fill
overloads were also observed and are not counted as transmit failures.
Evidence: `tmp/unbounded-net-node-82j5x-wire.json:14753-14764`.
The reserve entry/overload uprobe file offsets are `0x19b730`/`0x19bef4`, verified
against the running binary's mappings and disassembly in
`tmp/unbounded-net-node-82j5x-disasm.log`.

One background request for this same layer's page 1,
`YIsRNJtS/ZqjCKOULEeqgQ==`, arrived via `10.240.150.72:44670`, traversed the relay
to candidate `4d68f064-4d3e-4a5a-92f5-9b221a52458c`, and received a signed page
response advertising 16,777,232 ciphertext bytes at Unix time
1790404064.0245419. A server staging overload occurred at approximately
1790404064.032699, followed by FIN on that response socket at
1790404064.0327575. Evidence:
`tmp/unbounded-net-node-82j5x-wire.json:11015-11051,11210-11256,11453-11515`.
The explicit GET in this window also truncated at 16 MiB, but is a different
request from this background correlation.

The uprobe records callers and timestamps, not request IDs. Temporal/socket
correlation supports this attribution, not exclusive causality for every error.
Host packet capture can duplicate/reorder/drop packets; its reconstructed sequence
offsets are not used to prove exact body counts. Selected signed fields were decoded,
not independently signature-verified by the capture script.

## Old-fail/new-pass verification

`full_image_through_relay_sends_admitted_ciphertext`
(`cmd/racer-dataplane/src/peer/fleet_layer_tests.rs:161`) uses independent requester,
relay, and responder admission graphs with real TCP/io_uring, authenticated page
envelopes, and signed reverse forwarding. It transfers manifest/config fixture
bytes, then eight layers of four 16 MiB pages plus 17 bytes, with layer concurrency
1 and 4. One other live page per reader is retained at the relay after page zero;
the quota admits receive but not an additional full-page transmit copy.
Assertions verify page identity, AEAD, full lengths, SHA-256 and fixture manifest
descriptors, then zero connection/ciphertext/relay admission after drain.

With the final regression and only the server change reverted to base:
`peer server stopped: Err(Overloaded)`. With the fix restored: all eight layers,
536,871,048 bytes, pass at both concurrency settings. Both earlier full-layer
regressions also pass.

`send_page_is_immutable_and_completion_owned`
(`cmd/racer-dataplane/src/peer/transfer.rs:406`) checks unchanged allocation address,
rejected mutable access, a real partial socket send, cancellation, deadline, and
abandonment; charges remain owned until completion/drain and return to zero.

Commands used explicit outer budgets: up to 300s for initial compilation, 240s for
the full release suite, 120s for the opt-in layer regression, 180s for Clippy and
each scoped Go check. Final checks:

- Release all-feature Cargo suite: 557 library, two binary, 18 conformance, nine
  production-graph, and 31 doc tests passed. Eleven library opt-ins ignored by the
  normal suite; the three full-layer tests were run separately and passed.
- Cargo fmt/check and all-feature/all-target Clippy passed. Existing Clippy
  warnings are outside changed code.
- Scoped `make fmt` and `make lint`, `GO_PACKAGE_PATTERNS=./cmd/racer-loadgen`
  (`GO_PACKAGE_DIRS` also scoped for fmt), passed using repository-local
  golangci-lint 2.13.1; actionlint passed. No Go changes resulted.
- An earlier overbroad `--include-ignored peer::` run invoked a real-RDMA test
  without a device and exposed a new fixture accept-count deadlock. The fixture
  was corrected; the final bounded runs above passed. Hardware RDMA is unverified.

The new regression uses a synthetic encrypted page service and fixture image
metadata, not Gantry, containerd extraction, placement/election, or production
Fill. The one-dataplane kind environment was not used as evidence of multi-peer
recovery. This is a confirmed multi-hop integrity fix, not a successful 1500-node
benchmark or a proof that all remaining admission failures are resolved.

Only the **racer-dataplane** production image is affected. The parent must rebuild
from this commit and handle rollout and full-image fleet verification. No image
was published or deployed by this task.
