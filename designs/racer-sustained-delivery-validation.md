# Sustained delivery hotspot correction

## Scope

Base: `d22bef3ebc3518e8ada7db307491afb20cf8538e`.
Worktree and branch: `tmp/racer-fix-sustained-delivery`,
`racer-fix-sustained-delivery`.
Affected image: `ghcr.io/azure/racer-dataplane`; its source and release build
inputs are `images/racer-dataplane/Containerfile:18-31`.

This is a locally verified correction to a tagged live failure mechanism.
All-1500-node acceptance remains unverified until the parent deploys the image
and repeats the settled full-image benchmark. No image was pushed or deployed.

## Live evidence and causal chain

Evidence directory: `/home/azureuser/code/unbounded/tmp/racer-fix-sustained-delivery/tmp`.

- `inspect.json` verifies the live ELF executable segment has file offset
  `0x123f40`, virtual address `0x124f40`. The probe uses file offset `0x2d4a7b`
  for instruction address `0x2d5a7b`, not the virtual address directly.
- `disassemble.json` identifies that instruction as the comparison of active
  connections with the per-endpoint ceiling in `HttpPool::checkout_inner`.
- `trace-unbounded-net-node-vvhk5.json` records a bounded capture beginning
  2026-09-27 14:58:00 UTC on `racer-dataplane-45hzd`, node
  `aks-ddv5-17198779-vmss0000ab`, live image `d22bef3e`.
  It records 137 distinct request IDs rejected at active/limit `2/2` on
  `10.242.59.86:8082`. Of those, 100 match captured signed `overloaded`
  responses using request IDs plus canonical signed-head request bindings.
  `tagged-cause.json` retains the matches. Capture is partial, so counts are
  observations rather than a complete fleet census.
- Example request `NAVOZgT4NG5VnU3uj1kJ/Q==`, attempt
  `hkEkfwm4Fuy1EQKr1jbPVw==`, is layer
  `2115d07a4bfcddb9d1c4322c22fda03d1f24032560ae938b8c8d224eef408c1d`,
  page 1. The slot rejection and signed overload correlate. Captured admitted
  traffic identifies this hot edge as the final edge to candidate
  `4d68f064-4d3e-4a5a-92f5-9b221a52458c`.
- The ingress capture `trace.json` includes request
  `jzAQzYT1VNNmDtJE1I9y3w==` for that page: three candidate requests receive
  overload at 14:57:08.0668, 08.2988, and 08.5379 UTC. Remote acquisition
  allowances are 3, 2, and 0. The terminal candidate sequence is exhausted
  even though the failures describe temporary transit admission.
- A foreground layer GET happened to succeed in these captures. It is not
  evidence of full-image or fleet acceptance. The supplied settled report
  `../racer-v2-cluster-validation/tmp/racer-d22bef-c1-stable-retry2-diagnostic-report.md`
  remains the full-image failure baseline.

The code explains the amplification: candidate attempts debit and partition
credits before sending (`cmd/racer-dataplane/src/read/candidates.rs:360-397`),
and the owner only backs off between its three candidates
(`cmd/racer-dataplane/src/read/candidates.rs:208-275`). Transit previously chose
the canonical shortest route without considering its own active endpoint slots,
then nonwaiting checkout rejected it (`cmd/racer-dataplane/src/peer/stream.rs:215-227`).

**Proven:** live same-page slot rejection produces signed overload, and the
production-path regression fails without local admission-aware routing and
passes with it. **Inferred:** this mechanism contributes materially to fleet
EOF/503 rates. The fraction of fleet errors attributable to it, and post-fix
all-node goodput, require deployment evidence.

## Correction and preserved contracts

`cmd/racer-dataplane/src/peer/relay.rs:66-103` snapshots saturated incident
endpoint slots and selects the shortest available route before signing or
sending. `cmd/racer-dataplane/src/topology/paths.rs:166-224` folds those local
edges into the existing bounded route-cache key and search. Saturated nodes
remain reachable through other incident edges. Empty available subgraphs still
return overload to the acquisition owner's existing backoff.

The endpoint check is only a hint; checkout remains authoritative. A capacity
race fails under the existing ceiling. No sent envelope is retried, no attempt
or link credit is refunded, no wire field changes, and no connection, memory,
relay, replay, search-work, or waiter ceiling increases. Every detour preserves
the signed deadline, remaining hops, visited exclusions, and reverse chain.
Transit still never waits for another exchange's active scarce resources.

The algorithm documentation's lexicographic shortest-path rule remains the tie
rule within the available local subgraph; its local-edge section now documents
temporary capacity exclusions alongside local failed-link exclusions.

## Regression and checks

All commands had explicit bounds, at most 300 seconds per test/build call.

- Old route selection with the new signed-stream regression: FAIL at round 0.
- Old route selection with the new production Fill regression: FAIL with
  `Unavailable`. A preliminary version incorrectly accepted pressure failures;
  that assertion was tightened before the final old-fail/new-pass comparison.
- Old route selection with the new SDK regression: FAIL, one layer stops at
  50,331,648/65,509,376 bytes and the SDK deadline expires. For this comparison,
  the local saturated-edge list was deliberately replaced with its empty slice,
  restoring base route selection; the final code restores the full list.
- Final sustained tests: **3 passed** (`tmp/final-sustained.log`). Seven active
  peers on a real 1500-member radix-18 graph, four concurrent signed streams,
  12 successive arrivals each, full 16 MiB pages, and the original two-slot hot
  edge held busy. The Fill variant exercises acquisition, decryption, cache
  eviction, and repeated cold local reads. Final accounting returns to zero.
- SDK sustained test: **warm and cold passed**. Four concurrent layer reads,
  three complete eight-layer rounds with staggered arrivals, all byte counts
  and SHA-256 digests verified. Layer sizes match the live benchmark and total
  542,950,400 bytes per round. Fixture manifest/config bytes differ from the
  live 542,952,560-byte complete image. `tmp/go126-sustained-sdk.log` confirms
  this also passes with `GOTOOLCHAIN=go1.26.6`.
- All eight intersecting-flow tests: **passed**, including reverse-capacity
  cycles, scarce-resource unwind, and sustained 200/s incoming keepalive churn.
- Existing SDK scenarios: **7 passed**, each warm/cold (`tmp/sdk-existing.log`).
- Crossing Fill and corrupt-ciphertext regressions: **2 passed** (`tmp/crossing.log`).
- SDK-to-Rust UDS conformance: **passed** (`tmp/sdk-conformance.log`).
- Full all-feature Rust tests, debug and release: **579 library, 2 binary,
  18 conformance, 11 production, 31 doctests passed**. Hardware/expensive tests
  remain explicitly ignored in the default suite; relevant ignored tests above
  were run separately. Logs: `tmp/full-debug.log`, `tmp/final-release-tests.log`.
- Go SDK and Gantry Racer adapter tests: **passed**, including Go 1.26.6
  (`tmp/go126-sdk.log`).
- All-target/all-feature Cargo check: **passed** (`tmp/final-check.log`).
- Release binary build: **passed**, both all features and image-default
  `--bin racer-dataplane --no-default-features` (`tmp/final-release-build.log`).
- `cargo fmt --check`, fixture `gofumpt`, `git diff --check`: **passed**.
- `make fmt`: **passed**, zero lint issues, using the prior worktree's compatible
  golangci-lint v2.13.1. Two unrelated blank-line autofixes were inspected and
  reverted. No Go module or toolchain configuration changed.

## Live cleanup

Every kubectl invocation selected `joolshev-scale-test`, used
`--request-timeout=20s`, and had a 30-second subprocess bound. Remote tracing
had its own shorter timeout. No durable data, sysctl, deployment, or AKS
configuration was changed. Both traced hosts report no bpftrace processes and
empty uprobe registrations in `tmp/final-cleanup-unbounded-net-node-{t7ssg,vvhk5}.json`.
