# Acquisition routing after first-hop rejection

Base: `1da9a10116d006deeece9c01f21863ff0d59f4bf`.
Worktree/branch: `tmp/racer-fix-post-receipt-stream`, `racer-fix-post-receipt-stream`.
Affected image: **ghcr.io/azure/racer-dataplane**; default build inputs and command
are `images/racer-dataplane/Containerfile:18-31`.

## Proven defect and correction

At the base, original requesters select the canonical shortest route on every fresh
attempt (`peer/requester.rs:114-130` at the base). The relay already avoids its own
congested neighbors (`peer/relay.rs:77-105`), but that feedback does not reach the
original requester's route choice. Receipts recover unused delegation, yet repeat
sends to the same rejecting first hop still spend attempts and actual forward links.
Advancing candidate rank on those receipts also spends the limited allowance on
lower-ranked candidates and their predecessor probes before reaching the first one.

Source paths below are relative to `cmd/racer-dataplane/src/`:

- `security/forwarding.rs:176`: only a fully verified receipt whose signer is the
  original first receiver exposes a first-hop rejection. Downstream receipts do not
  imply adjacent congestion. Wire format, binding, signatures, replay checks and
  receipt refunds are unchanged.
- `read/candidates.rs:208-265`: retain bounded per-acquisition first-edge hints,
  keep an unreached candidate's rank, back off, then spend another original attempt.
  No new attempts, links, deadlines, origin authority, or admission capacity.
- `peer/requester.rs:122-170`: choose the shortest eligible route before signing;
  use its transport plan rather than recomputing the canonical route after signing.
  If all eligible first hops are excluded, a bounded canonical fallback
  remains available. A new acquisition starts with no inherited hints.
- `designs/racer-peer-security.md:129` documents this local routing rule. It extends
  the prior receipt design without changing its security/resource ceilings.

## Fresh live evidence and inference boundaries

Ignored capture artifacts live in this worktree's `tmp/`. Commands used explicit
`joolshev-scale-test`, 20-second kubectl request timeouts, 28-second local subprocess
timeouts, and remote `timeout -k 1s 26s`. Capture stores selected signed header
fields and bindings, never credentials or bodies. Bounded raw-socket filters and
read-only uprobes ran through existing net-node containers.

`tmp/tagged-unbounded-net-node-t7ssg-1790531060.json` records a foreground GET of
layer `8952e5cc686eb8a0d53fe830df7804a1ed34e72073da0e867d3072c4f042ed8b` through
Gantry `10.244.165.74`, with live dataplane `1da9a101` at `10.244.165.102`:

- HTTP 200, **16,777,216 / 59,679,232 bytes**, EOF at Unix time
  `1790531047.9292326` (2026-09-27 17:44:07.929 UTC).
- Request **H6csTSXmBoK2q9ZFJOsauA==**, PAGE1, is joined by exact signed-head bindings
  in `tmp/tagged-unbounded-net-node-t7ssg-1790531060.json-terminal.json:3-220`.
  At 17:44:07.146, its second candidate receives a first-hop `not-forwarded` from
  `15982640-f59c-4652-af93-341579cc8e0a` (`10.243.93.89`). At 07.910 it sends to
  the same candidate over the same first hop again. At 07.927919 it receives signed
  `unavailable`; at **07.928587** the local terminal probe reads **one attempt,
  zero links**, immediately followed by foreground EOF.
- The terminal cause is instrumented, not inferred from a counter. The executable's
  executable LOAD mapping has virtual/file offset difference 0x1000
  (`tmp/inspect-unbounded-net-node-rsq6l-1790530580.json`). Disassembly at
  `tmp/disassemble-unbounded-net-node-t7ssg-1790530660.json:1707-1759` shows the
  zero-route-links branch at virtual `0x2b1808`; the probe uses file offset
  `0x2b0808`. Request scope and budget pointers are read from that closure and
  request bytes join the captured route ID. An independent zero-attempt probe
  at file offset `0x2b08cc` records other tagged terminal failures.
- Packet capture omits some fragmented/uncaptured headers. It does not prove the
  internal cause of the final downstream `unavailable` or health of an alternate
  live route. The causal attribution to avoidable first-hop repetition comes from
  the controlled production regression below, not an assertion that this fix alone
  guarantees every live EOF is resolved.

Cleanup artifacts `tmp/cleanup-unbounded-net-node-{t7ssg,rsq6l}-*.json` show no
bpftrace processes and empty uprobe registrations. No rollout, push, cluster
configuration change, host sysctl change, or durable-state deletion occurred.

## Controlled old-fail/new-pass production regression

`peer/production_stream_tests.rs:78` adds
`sdk_sustained_full_images_route_around_rejecting_first_hops`. It runs five real
production peer graphs with Fill/election, AEAD, signed TCP transit, sliding range
delivery, and the actual Go 1.26.6 SDK. The fixture excludes direct source-to-target
edges so this small graph requires transit, while production Requester performs
route selection and exchange. It does not model 1500 simultaneously active peers.

Two canonical first hops are held at their unchanged **8/8 Relay** ceiling for
2500 ms in each of six four-layer batches. Assertions require successful page
delivery via real non-rejecting forwarders after two different first-hop receipts
**before** releasing those charges. Three complete eight-layer rounds run warm and
cold, checking exact sizes and SHA-256. Layer sizes total **542,950,400 bytes**;
the live image's **542,952,560 bytes** includes different manifest/config content.
Existing final zero page/dirty/Relay/Connection/Waiter/Flight accounting is preserved.

- `tmp/sdk-old-routing.log`: disable only forwarding the new exclusion hints in the
  fixture, retaining receipts, retries, deadlines and pressure. All four initial
  layers fail at exactly 16 MiB; the progress-under-pressure assertion fails. This
  isolates canonical-route repetition, not an exact-base checkout comparison.
- `tmp/sdk-new-final.log`, `tmp/sdk-final.log`, `tmp/sdk-final-path.log`: full
  warm/cold three-round passes, including the final signed-forwarder assertions.
  A preliminary attempt that changed routes but still advanced candidate rank failed
  (`tmp/sdk-new.log`); retaining the unreached rank completes the causal correction.
- `read/candidates.rs:563` extends the existing owner regression without removing
  assertions: first-hop hints persist and deduplicate, candidate rank stays fixed,
  and fresh acquisitions start empty. It retains exact eight-attempt/link exhaustion,
  ordinary overload/lost-response non-refund, corrupt signature, cancellation,
  deadline, abandonment, and released output accounting checks.
- `security/forwarding.rs:1383-1475` checks that relay/downstream receipts cannot
  exclude the original first hop, only adjacent authenticated non-submission can,
  and ordinary overload/unavailable/miss cannot. Existing binding/replay/expiry
  assertions remain intact.

## Verification

Every test/build invocation has a process-group hard bound below 300 seconds.
Full checks and limitations are recorded alongside command/exit/elapsed JSON files
under `tmp/`:

- `full.log`: **585 library, 2 binary, 18 conformance, 11 production, 31 doctests**
  pass, all features, release, serial.
- `sdk-full-compat.log`: five existing full-image SDK scenarios pass warm/cold.
- `sdk-sliding.log`: both existing sliding SDK scenarios pass warm/cold.
- `sdk-existing.log`: all three earlier sustained SDK scenarios pass warm/cold;
  the grouped command reaches its 285-second hard bound during the fourth/new
  scenario. `sdk-final.log` completes that new scenario separately.
- `intersecting.log`: nine real 1500-member-route/intersecting tests pass, including
  their production Fill and congestion regressions. Membership size is not fleet
  concurrency.
- `crossing.log`: both crossing production Fill regressions pass, including corrupt
  streamed ciphertext rejection.
- `fill-eight-layer.log`: the older ignored `full_image_real_fill_reclaims_relay_cache`
  fails its setup assumption that ciphertext usage equals the entire quota, before
  serving the image (16,777,232 versus 33,554,464). An independently compiled exact
  base source archive reproduces the identical assertion at its original line 761
  (`fill-eight-layer-exact-base.log`). The existing test is preserved; this is a
  pre-existing fixture incompatibility, not a passing check or this fix's regression.
- `clippy-final.log`: all-target/all-feature release Clippy completes with existing
  warnings. `build.log`: image-default locked release binary build passes.
- `go.log`: Go 1.26.6 SDK, Gantry adapter and mirror tests pass.
- `fmt.log`: scoped `make fmt GO_PACKAGE_DIRS=pkg/racersdk
  GO_PACKAGE_PATTERNS=./pkg/racersdk/...` passes using Go 1.26.6-compatible tooling.
  `rustfmt-final.log`: Cargo formatting passes.

All-1500-node health remains **unproven**. The parent owns image publication, rollout,
settling, and full-image acceptance on every node under the original loadgen criteria.
