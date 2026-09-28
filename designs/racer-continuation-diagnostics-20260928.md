# Remaining Racer layer truncation: instrumentation handoff

## Status

This is an instrumentation change, not a runtime truncation fix. The live root
cause after `20c043848eeaad4975787cbddda792d3b5efa40e` remains unproven. Restoring
complete verified pulls is still required before the NIC concurrency sweep.

The supplied operational evidence is in the parent workspace's ignored
`tmp/racer-operational-evaluation.md:63-80`: a direct UDS continuation advertised
52,157,952 bytes and closed after 16,777,216; a Gantry layer advertised 77,553,152
and closed after 67,108,864. Subsequent full/final-page reads verified. The final
five-minute window still reported 80.80% failed image attempts despite 1 GiB each
plaintext/ciphertext and larger socket budgets. This task did not redeploy or
change the shared cluster configuration.

## Source evidence

- `cmd/racer-dataplane/src/client/response.rs:154-227` prepares the first slice
  before success headers, then propagates subsequent acquisition/write errors.
  Once headers have escaped the only valid failure is an incomplete closed body.
- `cmd/racer-dataplane/src/read/candidates.rs:242-300` treats remote overload and
  unreachable candidates as transient evidence, ultimately returning Unavailable.
- `cmd/racer-dataplane/src/read/serve.rs:472-479` now records the typed local peer
  error before `peer_error` maps it to a wire response. Peer dispatch also records
  relay/local failures before its mappings in `src/peer/server.rs:386-442`.
- `cmd/racer-dataplane/src/peer/transfer.rs:253-343` records checkout, handshake,
  head, receive-admission/body, and decode failures. `src/runtime/admission.rs`
  records quota failures at their actual source, including cache fair-share and
  ingress socket quotas.

These paths explain loss of failure specificity. They do not establish which
internal error caused the live incident. No quota or retry increase is proposed.

## Reproduction and validation

`cmd/racer-dataplane/src/app_dst_tests.rs:2485-2582` adds a deterministic
production-composition test with 40 nodes, multiple workers, routing/relays,
four distinct concurrent five-page layers, 256 MiB payload budgets per worker,
512 connections per worker, and 16 connections per neighbor. It verifies four
complete reads, injects admitted ciphertext pressure after success headers,
asserts actual continuation and internal admission/peer diagnostic records, then
verifies four complete reads after releasing pressure on the same state. It
asserts both secondary-worker work and relay activity. Byte checking uses the
existing independent content oracle, not just HTTP status or advertised length.

This controlled pressure validates the diagnostic path and recovery. It is not a
reproduction of the unknown spontaneous failure on the 1,500-node warmed fleet.
The initial 12-node fixture failed its relay-activity assertion; 40 nodes exercise
that path and pass. No healthy failure assertion was relaxed.

Checks used external TERM/kill-after-10-second bounds of at most 300 seconds;
Go tests also used `-timeout=5m`:

- New multiworker/routed production-composition regression: passed.
- Rust library excluding DST/contention: 745 passed, 7 explicit ignores.
- Final changed telemetry group: 14 passed; actual-UDS group: 11 passed.
- Production dataplane integration: 15 passed, 2 explicit ignores.
- Client/origin conformance: 19 passed, 1 opt-in SDK ignore.
- All-target/all-feature `cargo check`; 32 doctests: passed.
- Go SDK, Gantry mirror, Gantry Racer adapter: passed. The SDK initially failed
  because this new worktree lacked its required `tmp/` directory; creating that
  ignored fixture root resolved the setup failure.
- Cargo formatting and whitespace checks: passed.
- Clippy completed with 96 library / 122 library-test warnings in existing
  code. The two new `manual_inspect` findings were corrected. Strict warning-free
  Clippy is not claimed.
- Scoped `make fmt` / `make lint` on the SDK and actionlint passed using the
  repository tools and Go 1.26.6. Formatter-only changes to existing Go files
  were removed from this Rust-only item.

## Deployment and collection required from parent

Build and deploy a dataplane image containing this commit to data-serving nodes,
including candidates and relays. Existing controller, Gantry, and loadgen images
can remain. Preserve node identities, cache keys, slabs, and the current workload
and runtime settings so the observation remains comparable. No migration is
required. Diagnostics now reserve 340 KiB rather than 40 KiB of RequestContext
memory to hold bounded 64 KiB responses.

GET `/debug/failures` on the existing pod diagnostic port. For example, from an
authorized diagnostic pod with network access:

```sh
curl --fail --max-time 2 http://DATAPLANE_POD_IP:9090/debug/failures
```

Capture snapshots promptly during ordinary four-layer verified probes, across
requesting nodes, candidates, and relays. The node-wide ring retains 128 records;
collect repeatedly and deduplicate by pod/process plus sequence. Correlate
request/attempt IDs across nodes and worker/time/sequence for low-level quota
records. `total` is internal diagnostic events, not failed image requests.
No application object keys, ETags, headers, credentials, or payloads are exported.
See `cmd/racer-dataplane/src/telemetry/INTEGRATION.md` for field semantics.

The next runtime fix must follow the captured typed error/resource and reproduce
that failure before changing acquisition behavior. Full verified fleet pulls and
a healthy observation window remain the gate for the NIC sweep.
