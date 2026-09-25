# Racer dataplane implementation coordination

Implementation baseline: `6938c593`. All component owners work in this isolated
worktree, stage only their owned paths, and commit reviewed changes. The integration
owner alone edits Cargo manifests, application composition, configuration, errors,
and final documentation. Existing tests are preserved and updated to test behavior.

## Ownership

- Runtime owner: `src/runtime/`, including memory through a focused delegate.
- Storage owner: `src/store/` and `src/store.rs`.
- Security owner: `src/security/` and `src/security.rs`.
- Control owner: `src/control/` and `src/control.rs`.
- HTTP owner: `src/http/` and `src/http.rs`.
- Topology owner: `src/topology/` and `src/topology.rs`.
- RDMA owner: `src/rdma/` and `src/rdma.rs`, native adapter assets.
- Read owner: `src/read/` and `src/read.rs`, through focused delegates.
- Client/origin owner: `src/client/`, `src/origin/`, their module roots, and `src/model/`.
- Peer owner: `src/peer/` and `src/peer.rs`.
- Integration owner: application, configuration, telemetry, package/build files,
  cross-component verification, and documentation.

All source paths above are relative to `cmd/racer-dataplane`. Components preserve
existing public APIs where possible and add explicit methods for missing ownership
handoffs. No owner overwrites another owner's files. Cross-component compiler errors
are reported with the exact required interface. Constructors stay side-effect-free.

## Decisions

The newer paired-worker and control contracts supersede the temporary design's
single-core combined role, environment shares, and mounted node certificates.
The SDK implementation defines client/origin HTTP compatibility: lowercase hex
object paths, quoted strong ETags, absolute expiration in Unix milliseconds, strict
single ranges, and exact opaque context bytes. Page payloads use XChaCha20-Poly1305;
peer authentication uses Ed25519. Control uses TLS and strict bounded JSON.

Unknown wire details are versioned, domain-separated, deterministic encodings with
test vectors. Placement uses deterministic SHA-256 inputs and integer weighted
ranking. No default randomized hasher participates in distributed decisions.

Completion ownership is mandatory: submitted operations retain buffers, FDs, quota,
and storage/NIC leases after request cancellation. Real implementations must not
replace production operations with test fakes or unconditional success. Optional
RDMA falls back to HTTP when native capabilities are unavailable.

## Verification

Each owner adds meaningful success, failure, and edge tests and runs targeted checks.
The integration owner runs formatting, all-target/all-feature checks, tests, real
socket and storage exercises, and audits remaining placeholders and unreachable
operational paths before cherry-picking commits to the original branch.
