# Peer receive allocation double charge

Base: `5fff53da9b6e7cfb7160d811d4a317e830db0f28`. Branch and worktree:
`racer-fix-peer-stream`, `tmp/racer-fix-peer-stream`. Investigation: 2026-09-26.

## Cause and change

The HTTP receive path moves a `WireBuffer` allocation into a `Vec`, retaining its
reservation, then decodes the page. The old decoder reserved the same bytes again
before moving that same `Vec` into `CiphertextPage`. No second allocation justified
that overlapping charge. An otherwise fully received page could therefore fail
with `Overloaded` while its real allocation fit the quota.

The receive buffer now reserves against the requested cache and transfers that
reservation into `LogicalCodec::response_reserved`
(`cmd/racer-dataplane/src/peer/transfer.rs:307-334`). The decoder uses the supplied
charge (`cmd/racer-dataplane/src/peer/decode.rs:258-269`). Existing BufferPool
validation checks admission identity, cache, class, and allocation capacity
(`cmd/racer-dataplane/src/memory/pool.rs:83-120`). Unreserved callers retain the
existing decoding entry point. Limits, retries, deadlines, and wire encoding are
unchanged. This is one receive-allocation accounting fix.

## Read-only fleet evidence

All Kubernetes commands used explicit context `joolshev-scale-test` and
`--request-timeout=20s`; local subprocesses, remote commands, captures, and uprobes
had explicit deadlines. Existing hostPID net-node pods provided access. No AKS
configuration or durable state was changed; temporary uprobes detached on exit.

The initial explicit layer pull through `gantry-226zq` returned HTTP 200,
Content-Length 59,757,056, and EOF at exactly 16,777,216 bytes in 0.265865 seconds.
Layer digest:
`sha256:2115d07a4bfcddb9d1c4322c22fda03d1f24032560ae938b8c8d224eef408c1d`.
Its page 1 request `E7RM7S+FkHZZLb/KSUG/Yw==` tried:

1. `10.242.125.70` (node `133af1b6-abbf-4236-a032-ce7d288b1a0d`), which returned
   a downstream signed overload from `80d587ad-66b1-45a4-b82d-74e2333a33a6`.
2. `10.242.226.70`, which returned downstream unavailable.
3. `10.241.35.79`, which returned overload with zero delegated attempts left.

Evidence: `tmp/trace-wire.json:1-142`. The subsequent first-hop capture mapped the
downstream node to `10.241.155.71`, pod `racer-dataplane-w94bt`, on
`aks-ddv5-17198779-vmss0000d5`. Sampled dataplanes were confirmed at the base image.

On that downstream relay, a four-second admission probe recorded two failures at
the decoder's ciphertext reservation, one at `WireBuffer::new`, and 779 at
`reserve_fill` (the latter retries while waiting). Evidence:
`tmp/unbounded-net-node-82j5x.log`. Disassembly and the process mapping resolve the
decoder caller to ELF virtual address `0x250a93`; buffer staging is `0x2940f9`.
The actual uprobe file offsets were `0x1937f0` (reserve entry) and `0x193fb4`
(overload branch), 0x1000 below their ELF virtual addresses. Earlier probe attempts
with virtual offsets failed attachment or collected no useful evidence and are
not counted.

A final simultaneous packet/uprobe capture narrowed one decoder failure to the
relay's own background acquisition of this same layer's page 1:

- Request `A4qpxecphYnew0qr3NwI/Q==` received a signed page through `10.241.179.70`
  at Unix time `1790402948.1944835`, from candidate
  `8a14c117-fbd8-49b6-88d4-8da6303f1070`.
- Decoder reservation failed at approximately `1790402948.224160`.
- At `1790402948.2248998`, that request tried its next candidate through
  `10.241.35.79`, with zero delegated attempts remaining.

Evidence: `tmp/unbounded-net-node-82j5x-wire.json:7659-7885`. Clock conversion used
the captured wall/monotonic pair. Temporal correlation and the immediate retry
support the attribution; the probe itself records resource/caller, not request ID.
The explicit pull in this final window completed the layer, so it is not claimed
as a failed full-image probe. Earlier probes reproduced 16 and 48 MiB cuts.

Captures retain selected decoded signed fields, not credentials or page payloads.
They do not independently verify signatures. Host-interface capture can lose or
reorder packets, and admission probes include background traffic. The initial
explicit pull's overloads are not individually proven to be this decoder bug.
Other fleet admission failures remain possible. This fix is a confirmed causal
peer truncation mechanism observed on the live failing path, not proof of complete
fleet recovery. Metadata/election changes were not needed to reproduce it.

## Old-fail/new-pass regression

`full_image_through_relay_transfers_receive_charge` in
`cmd/racer-dataplane/src/peer/fleet_layer_tests.rs` runs three independent admission
graphs with real TCP/io_uring and signed reverse-path forwarding. Eight layers each
contain four 16 MiB pages and a 17-byte tail. The test checks page identity, metadata
length, AEAD authentication, full layer byte counts and SHA-256, then drains and
asserts zero connection/ciphertext/relay charges.

Page zero completes before two other page-sized reservations are retained per
reader. The remaining quota admits the next receive allocation but not a duplicate
charge for it. The four-reader variant synchronizes completed pages to make the
peak deterministic. No production limits are raised.

```sh
timeout 115s cargo test --manifest-path cmd/racer-dataplane/Cargo.toml \
  --locked --release --all-features --lib full_image_through_relay \
  -- --ignored --nocapture
```

- Before the production fix: `layer 0 page 1, received 16777216 bytes: Overloaded`.
- After: eight complete layers / 536,871,048 bytes at one-reader concurrency and
  again at four-reader concurrency. The existing idle-neighbor regression passes.
- The real HTTP fragmentation/reuse/truncation test additionally validates charge
  transfer and rejects wrong owner/cache/class/size and malformed body, with quota
  release assertions.

This is a multi-hop full-layer integrity regression with synthetic encrypted page
service and pre-established authentication challenges. It does not exercise OCI
manifest/config parsing, Gantry, placement/election, or all 1,500 members. No new
kind cluster or AKS candidate rollout was performed. The retained one-dataplane
kind cluster was not used as evidence of peer recovery.

## Checks and affected image

- Release all-feature suite: 556 library tests passed, 10 opt-in tests ignored;
  two binary, 18 conformance, nine production-graph, and 31 doc tests passed.
  The two full-layer opt-in tests were separately run and passed.
- Cargo formatting and all-target/all-feature Clippy; preexisting Clippy warnings
  remain outside the change.
- Scoped `make fmt` and lint use `./cmd/racer-loadgen`. The first fmt attempt used
  incompatible golangci-lint 2.11.4; rerunning with existing repository-local 2.13.1
  passed.

Only the **racer-dataplane** production image is affected. Rebuild it from this
commit for the parent's rollout. No image was published and no rollout performed.
