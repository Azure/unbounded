# Live Racer copy-only disk admission fix

## Scope and result

Parent: `8938e0224ffd627fa0c21f6be045be3ae1076d65`.
Worktree: `tmp/racer-live-continuation`, branch `fix/racer-live-continuation`.

Fixed a reproduced owner-side failure: `Fill::copy_only` read disk without the
fill owner's idle-memory reclamation callback. An otherwise retrievable disk page
could return `Overloaded` solely because idle cached ciphertext occupied staging
headroom. This affected explicit copy-only probes and ordinary peer bootstrap,
which first tries `MetadataService::bootstrap_copy`.

This is a runtime fix, but not a claim that all fleet continuation failures are
resolved. Live captures also contain relay-slot exhaustion and other owner
overloads. The fixed image was not built, pushed, or deployed. Fleet recovery and
the healthy concurrency sweep remain unverified.

## Implementation evidence

- Parent `cmd/racer-dataplane/src/read/fill.rs:609-618` called `disk.read` directly
  and propagated admission errors. In contrast, ordinary acquisition supplied
  `reserve_with_reclamation` at parent `fill.rs:659-675`.
- `cmd/racer-dataplane/src/store/reader.rs:109-121` admits aligned disk staging
  plus decoded ciphertext together. On this filesystem, a full page requests
  33,554,960 bytes. `store/slab.rs:78-99` can free its own idle staging buffer,
  but cannot reclaim the fill's memory cache.
- `cmd/racer-dataplane/src/read/metadata.rs:763-783` reaches copy-only disk reads
  from peer bootstrap before fresh acquisition, even for an Acquire operation.
- The fix at `cmd/racer-dataplane/src/read/fill.rs:609-632` uses the existing
  `read_with_token_reclaim` and cache-scoped bounded `reserve_with_reclamation`.
  `fill.rs:133-173` retains the two-pass fair-share/global policy; it does not
  raise budgets, retry network requests, or revoke live leases.
- `cmd/racer-dataplane/src/read/INTEGRATION.md:96-109` describes local disk staging
  using this policy. The parent copy-only path was an implementation gap relative
  to that general statement; this fix applies the established policy to that path.

## Live collection

Context `joolshev-scale-test`, namespace `unbounded-system` throughout.
Image workflow [36394780932](https://github.com/Azure/unbounded/actions/runs/36394780932)
matched the exact parent and completed successfully during bounded polling.
`python3 tmp/racer-pin-image.py 8938e0224ffd627fa0c21f6be045be3ae1076d65`
changed only the dataplane image override. Generation 38 was observed with all
1,500 updated, ready, and available at 08:09:42 UTC.

Deployed tag: `ghcr.io/azure/racer-dataplane:8938e0224ffd627fa0c21f6be045be3ae1076d65`.
All 1,500 sampled containers reported image ID
`sha256:84fd7d4c601cc6c78133b44eeacf7054a4148e7d704dd4f6fbcf8027617ae3ca`.

Parent-workspace ignored artifacts:

- `tmp/racer-live-20260928T080946Z-mirror.log`: six fleet snapshots concurrent
  with three ordinary verified probes of images 0, 17, and 255, four layer workers.
  Includes HTTP 200 truncations at 16 MiB page boundaries and pre-body 503s.
- `tmp/racer-live-20260928T081244Z-direct.log`: eight fleet snapshots while
  repeatedly issuing four distinct direct-UDS bootstrap/pinned continuations.
  The collector initially interleaved some print calls; JSON prefixes remain
  decodable. A print lock was added for the next collection.
- `tmp/racer-live-20260928T083006Z-mirror.log`: repeated fleet snapshots and
  verified four-layer probes. Includes a 77,553,152-byte response truncated at
  67,108,864 bytes. The same layer also completed and verified in earlier probes.
- Each log has a `.pods.json` sidecar with pod UID, container process identity,
  start state, restart count, and image ID. Rings were repeatedly fetched without
  clearing them. Event totals are diagnostic events, not request counts.

A correlated example in the direct log:

| Pod / log line | Evidence |
| --- | --- |
| `racer-dataplane-h2kx2`, 5231 | Worker 0 sequences 18424-18431: Ciphertext admission requests 33,554,960 bytes, limit 536,870,912; used drops from 535,047,030 to 518,269,798, then remains unchanged across retries. Sequence 18432: `PeerLocal/Overloaded`, request `8ef583a78dac9cf935a2f3eb94d5e6d1`, attempt `93353b3a5b748a75ef061f192e353b08`. |
| `racer-dataplane-x7rsk`, 5973 | Sequence 15408 receives `CandidateResponse/Overloaded` for the same request and attempt. |
| `racer-dataplane-jhqpp`, 5311 | Same request subsequently encounters `PeerRelay/Overloaded`; adjacent admission records show Relay used 8, limit 8. |
| `racer-dataplane-x7rsk`, 5973 | Sequences 15419 and 15444-15445: candidates exhausted, page 3 unavailable, `NextSlice/Unavailable`, sent 33,554,432 of 42,131,456 bytes. |

This proves typed owner/relay pressure reaches an incomplete continuation across
workers and nodes. It does not prove that every owner overload is reclaimable or
that the captured owner attempt was specifically CopyOnly; the diagnostic schema
does not include operation mode. The deterministic regression establishes the
copy-only bug with the exact disk-admission request, separately from that inference.

## Reproducer and checks

Two regressions at `cmd/racer-dataplane/src/read/fill_tests.rs:1406-1598` exercise
direct copy-only and the peer-bootstrap entry point. They encrypt and persist a
real full page with io_uring/direct slab I/O, confirm the disk mapping, evict its
memory lookup, and retain 30 other full ciphertext pages under a 512 MiB worker
budget. They assert:

1. All-live pressure fails with `Overloaded`, without origin work or revocation.
2. Releasing all but one live copy permits the disk read through idle reclamation.
3. Returned ciphertext and envelope exactly match the original; the pinned copy
   remains intact. Bootstrap spends no fresh acquisition credits.
4. The diagnostics include the live 33,554,960-byte request; teardown returns
   ciphertext accounting and in-flight I/O to zero.

The original regression failed on parent behavior at the idle-reclamation success
assertion with `Overloaded`, then passed after the fix. Initial fixture iterations
needed an opened/configured writer and canonical millisecond expiry; these setup
failures were corrected before accepting the regression.

Checks used external `timeout --signal=TERM --kill-after=10s` bounds no greater
than 300 seconds; Go tests also used `-timeout=5m`:

- Rust library: 749 passed, 7 explicit ignores, 41 filtered (contention filters).
  The intended DST exclusion used a nonmatching module name, so the six existing
  DST tests actually ran and passed, including concurrent relayed layer pressure,
  healthy relayed page reads, and generated native/non-native churn.
- Production integration: 15 passed, 2 opt-in ignores.
- Client/origin conformance: 19 passed, 1 opt-in SDK ignore.
- All-target/all-feature `cargo check`, 32 doctests, cargo formatting, whitespace:
  passed.
- Clippy: completed with the existing 96 library / 122 library-test warnings;
  no findings in the added regression. Strict warning-free Clippy is not claimed.
- Scoped `make fmt` and `make lint` for `pkg/racersdk` with Go 1.26.6, repository
  gofumpt/golangci-lint/actionlint: passed. Three unrelated formatter-only blank
  lines were reverted after checks.
- Gantry mirror and Racer adapter Go tests passed. SDK initially failed its
  100 ms queue cancellation race at `pkg/racersdk/read_options_test.go:118-168`
  during concurrent checks; the isolated test passed five repetitions, and the
  full SDK suite passed afterward. No Go behavior was changed.

## Measurement conditions

No runtime ConfigMap or loadgen settings were changed. Plaintext/ciphertext each
1 GiB per node, client connections 2048, connections per neighbor 16, maximum
threads 8, CPU/memory requests 2 CPU/2 GiB. Observed worker byte limits were
512 MiB. Catalog 256, eight layers, 64 MiB nominal layer size, jitter 0.2,
seed benchmark-v1, image concurrency 1, layer concurrency 4, SHA-256 verification
enabled, pull timeout 2 minutes. Gantry remained `b8d1a44a...`; loadgen remained
`20c043848...`. Existing identities, keys, and slab data were preserved.

Five-minute Prometheus windows, 60-second scrapes, all four relevant jobs with
1,500 continuously-up series and at least five samples:

| Window end UTC | Verified GB/s | Failed image attempts | NIC TX / RX Tbit/s |
| --- | ---: | ---: | ---: |
| 08:14:43.393 | 75.560 | 82.133% | 4.045 / 3.746 |
| 08:34:40.548 | 97.191 | 71.727% | 4.752 / 4.400 |

These are observations of the parent diagnostic image, with diagnostic collection
and probes during the windows, not an A/B test of the fix. Verified bytes count
only complete successful image pulls; NIC bytes include forwarding. Warmed-state
drift is material, so the later rate is not attributed to this source change.

The rollout had 526 pods with previous exit-code-1 startup failures; three sampled
previous logs reported `DeadlineExceeded`. The latest recorded termination was
08:08:39 UTC, before both measurement windows. All fleets were still updated, ready,
and available at 1,500 at the final status check. Diagnostic pod
`racer-operational-diag` was deleted and absence verified. No fix image workflow
was dispatched and no commit was pushed.
