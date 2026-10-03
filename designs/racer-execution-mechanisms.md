# Racer execution mechanisms

## Boundary and reuse

The corrected boundary keeps core business logic in `racer-dataplane`, not only
composition glue. This round extends `uring-runtime`; it does not introduce an
execution-policy or common/model crate.

- `runtime/src/drivers.rs` owns explicitly polled local tasks, reserved capacity,
  scoped queue selection, deferred child submission, and wake gating. Another
  event-driven proxy could supply its own capacity, polling budget, task futures,
  result handling, and shutdown fencing without any Racer model or read types.
  Queues, permits and scoped futures remain non-Send. Wakes may cross threads.
- `src/read.rs:5-112` keeps Racer's 1024-task ceiling and error/result adaptation.
  Acquisition, routing, retry and lifecycle decisions remain in main. Client
  listeners and dispatch consume the runtime wake primitive directly.
- `src/runtime/admission.rs:39-89` remains Racer policy, including resource
  classes, store-aware ciphertext floors and up to three reserved control
  connections (`min(client_connections / 4, 3)`).
  Generic accounting/enforcement already belongs to `flow-control`; moving this
  policy would invert the corrected ownership rather than improve reuse.
- `src/runtime/deadline.rs:62-195` retains candidate total/idle/body decisions and
  request semantics. Generic cancellation and I/O scope enforcement already live
  in `runtime/src/deadline.rs` and `runtime/src/reactor.rs`. No application model
  types or error taxonomy are moved downstairs merely to satisfy imports.
- `src/runtime/ingress.rs:90-112` chooses admitted destinations. Its queue bound
  follows target reservations (`:159-165`). This is application ingress policy, not a reason to invent a
  generic routing callback. The SPSC channel is not interchangeable with this
  shared, target-selected handoff. The implementation remains upstairs.
- `src/runtime/reactor.rs` retains validated Racer resource-class/provenance
  adapters around the existing generic reactor. Worker/page crypto completion
  ownership stays in main; no wholesale runtime directory move occurs.
- `src/security/aead.rs::capture_aead_failure` captures page fingerprints using
  the existing cached checksum; `src/telemetry/failures.rs` owns records/rings
  without importing a page buffer. Both remain upstairs: diagnostic semantics
  and the crypto-operation identity are Racer-specific. No raw-key APIs change.

## Acceptance map

| Contract | Owner and coverage |
| --- | --- |
| Bounded permits, nested owners, wake gating, detached completion | `runtime/src/drivers.rs` mechanism tests, including all five prior driver tests |
| Zero capacity/budget, scoped destruction, recursive polling, crash with outstanding permit | New runtime driver edge tests |
| Non-Send queue, guard, permit, scoped future | Four runtime compile-fail examples |
| Exact Racer ceiling, error conversion, completion-before-drop accounting | `src/read.rs::drivers::tests` |
| Cancellation fences and safe reactor construction | Existing reactor and worker tests/compile-fail examples, unchanged |
| Cross-cache zeroization and accounting transfer | `src/runtime/admission/tests.rs:32-62`, unchanged |
| Reserved outbound/control progress and resource floors | Existing admission and ingress tests, unchanged |
| Held offer delivered after close releases socket and both charges | New `runtime::ingress::tests::offer_delivered_after_target_close_releases_socket_and_charges` |
| Diagnostic meanings, independent ring lifetimes and exact formatting | Existing `telemetry::failures::tests` plus expanded page fingerprint/cached-CRC test |
| Real cross-component ownership/compatibility | Top-level production_dataplane, process_restart, payload_zeroization, identity_integration and client_origin_conformance suites |

The existing runtime `simulation` feature explicitly exports driver crash support;
the existing application dev-dependency enables it. Production does not rely on a
dependency's `cfg(test)`. Private mechanism tests move with their implementation;
application policy and cross-component tests remain upstairs.

## Build input audit

No new member, dependency or feature is needed, so workspace manifests and lockfiles
do not change. `images/racer-dataplane/Containerfile:25-26` already copies the
runtime manifest and entire source directory. Both CI cache key/restore-key pairs
in `.github/workflows/ci.yaml:172-174,256-258` already include the runtime manifest;
the full keys include the commit SHA. Changing these inputs for an existing-module
addition would be cosmetic. Container builds and hardware/privileged tests are not
implied by Rust compile checks.

No performance claims are made. Further page/store/peer/read/client extraction is
outside this round; future choices must justify mechanisms, not move business
policy based on directory size or require an invented second consumer.
