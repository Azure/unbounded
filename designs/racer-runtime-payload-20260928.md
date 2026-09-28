# Racer layer receive admission failure, September 28, 2026

## Runtime observations

Context: `joolshev-scale-test`, namespace `unbounded-system`, dataplane
`ebd581399110d22ea04ce3c1de2c0148f2e69392`. At investigation time all three
Racer/Gantry/loadgen DaemonSets had 1,500 Ready pods.

At approximately 05:42 UTC, five-minute Prometheus request rates were:

| Loadgen request outcome | Requests/second |
| --- | ---: |
| manifest success | 1,335.5 |
| config success | 1,334.8 |
| layer error | 1,367.4 |
| layer canceled | 3,963.2 |
| layer success | 70.0 |

Loadgen logs sampled from the DaemonSet contained startup messages only. Its worker
loop records metrics but does not log individual pull errors (`cmd/racer-loadgen/pull.go`).

Direct requests through `gantry-8xmq2` at 05:42-05:43 UTC reproduced:

- Layer `sha256:2115d07a4bfcddb9d1c4322c22fda03d1f24032560ae938b8c8d224eef408c1d`
  returned all 59,757,056 bytes with matching SHA-256.
- Layer `sha256:515e6f5a38f01eda728d0b6497ff117551ea816b14b684ffae35d2134d710c9e`
  advertised 68,935,168 bytes but delivered only 16,777,216 bytes.
- Several other layers likewise stopped at 16 MiB or returned HTTP 503.
- Gantry counters included 497 aborted HTTP 200 responses and 3,184 completed
  HTTP 503 responses. SDK queue rejection and timeout counters were zero.

The same layer through `gantry-zb2hm` at 05:44 UTC returned one page when resumed
at offset 16,777,216, returned 503 at offset 33,554,432, and completed its final
18,603,520 bytes when resumed at offset 50,331,648. This localizes failure to
individual page acquisition rather than image generation or one global size cap.

Placement computed from Node last-admitted-member annotations and the production
hash algorithm put page 2 on these candidates, in order:

1. `aks-ddsv6-84072342-vmss00007w`, `gantry-2pdlr`, `10.243.213.99:8082`.
2. `aks-adsv5-13731677-vmss00001c`, `gantry-cgl9c`, `10.244.127.109:8082`.
3. `aks-ddv5-17198779-vmss00009i`, `gantry-xcnkk`, `10.241.55.108:8082`.

At 05:46 UTC a resumed request directly through the first candidate delivered
page 2, then truncated on the next page. Its Gantry origin counter showed one
successful GET/206. The third candidate behaved similarly; the second returned
503. A temporary client-socket diagnostic on the source node subsequently read
page 2 successfully while other pages returned 503. The diagnostic mounted only
the existing client directory read-only, used no service-account token, and was
deleted and confirmed absent.

## Reproduced cause and fix

The deployed 256 MiB node ciphertext budget partitions into 64 MiB per worker
on a four-pair node. A deterministic 40-node production-stack simulation with
that worker budget succeeds for a two-page object but truncates a five-page
object on the original implementation.

Temporary local stack instrumentation showed receive admission failing with
50,331,696 bytes already charged and a 16,777,232-byte request against a
67,108,864-byte limit. Failures occurred in both `WireBuffer::new` and
`SecurityCodec::response`. The candidate loop exhausted alternatives with
`[Overloaded, Overloaded, Unreachable]`, returning `Unavailable` to the client.

Three parts of the same ownership defect contribute:

- Fill reserves encryption output even when a peer supplies existing ciphertext.
- Peer receive admission cannot reclaim otherwise idle cached ciphertext.
- Decoding charges the already allocated receive buffer a second time.

The fix reserves encryption output only for origin, connects peer reception to
bounded idle reclamation, and transfers the cache-scoped receive reservation into
the decoded page. Admission ownership, cache identity, resource class, capacity,
and page descriptors remain checked. Signature, MAC, AEAD, origin authority,
retry, and deadline enforcement are unchanged.

`healthy_relayed_page_reads` now verifies 40 full five-page reads, totaling
2,684,364,840 bytes, against independent expected bytes. The reservation test
covers exact-capacity success and foreign-admission, wrong-cache, wrong-class,
and insufficient-capacity rejection with charge cleanup.

The runtime-to-local root-cause attribution is an inference supported by matching
page-boundary failures and the deterministic production-stack reproduction.
The deployed binary exposes only aggregate request errors, so no live internal
error stack was available. Cluster-wide resolution still requires deploying the
fixed dataplane and observing successful full image pulls over a complete
five-minute rate window; shared workloads were not rolled out during this task.

## Verification

Every test command used external TERM/kill-after bounds of at most 300 seconds.

- All-feature Rust library group excluding DST/contention: 743 passed, 7 ignored.
- New all-feature full-layer multi-node regression: passed.
- Production dataplane integration: 15 passed, 2 ignored.
- Client/origin wire conformance: 19 passed, 1 opt-in SDK test ignored.
- Rust doctests: 32 passed; all-target/all-feature `cargo check` passed.
- Go SDK, Gantry mirror, and Gantry Racer adapter tests passed with `-timeout=5m`.
- `cargo fmt --check` and `git diff --check` passed.
- Clippy completed with existing warnings. Strict `-D warnings` is not clean on
  the base code; new patch-specific warnings were corrected.
- Scoped `make fmt` and golangci-lint passed under `GOTOOLCHAIN=go1.26.6`.
  The default Go 1.27/linter combination is incompatible. `make lint` reached
  zero Go issues but its workflow-lint step was blocked by missing `actionlint`.

Deployment requires a new Racer dataplane image on all data-serving nodes,
including relays. No controller, Gantry, loadgen, schema, quota, identity, or
stored-data migration is required.
