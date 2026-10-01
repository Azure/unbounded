# Coordinated Racer routing 3 -> 5

One operational item, no build or image change. `routing_v5.py` only reads and
server-dry-runs. It never applies, pauses load, drains, or restarts anything.
Requires the existing rollout dependencies: Python 3, PyYAML, kubectl, GNU timeout.
Every kubectl invocation pins context `joolshev-scale-test` and namespace
`unbounded-system`, without changing kubeconfig. Parent owns all cluster/load actions.

## Compatibility, motivation, and risk

Production source reviewed at **3645eaefd67342f9dd193f68b0a9cc52e220139f**
(line references describe that commit; symbol references below were refreshed
after the readability refactor, relative to `cmd/racer-dataplane/src`):

- `cmd/racer-dataplane/src/config.rs:173-178` accepts explicit 3 and 5, defaulting
  to 5. Default support does not override the currently explicit 3.
- `cmd/racer-dataplane/src/app.rs:746-759` supplies that same configuration to
  `Paths` and `PeerNetwork`. `peer.rs:60-69` validates neighbors using the selected
  graph. One dataplane setting controls both; there is no second routing knob.
- `cmd/racer-dataplane/src/topology.rs:12-39` defines V5 radix32 versus V3 radix18,
  with a 64-neighbor capacity/first-hop mask. `Graph::neighbors` and
  `neighbor_positions_for` in `topology/routing.rs` construct the versioned edges.
  V3 and V5 are different graphs, not interchangeable peers.
- `NORMAL_LINKS` and `FAILURE_LINKS` in `topology/routing.rs` keep budgets at 4/8.
  `v5_default_reduces_hops_on_balanced_deterministic_pairs` in
  `topology/routing/scenarios.rs` tests 1500 balanced source/destination pairs against
  independent shortest paths, sync/async agreement and hop-by-hop forwarding,
  asserting over 10% fewer total links. In the same file,
  `default_hundred_thousand_member_routes_at_every_distance_are_bounded` checks V5 at
  100,000 members with both budgets: <=64 candidates, bounded search quanta,
  expansions/edges/visited entries, cache capacity and released search admissions.
  These assertions were read, not rerun or represented as new Rust test results.

Controller coordination is an operational responsibility, not a controller
algorithm setting. `internal/racer/topology.go:126-174` reconciles owned endpoint
membership and publishes it. `internal/racer/wire/types.go:53-75` carries members,
shares, endpoints, rails, caches and versions, **not routing algorithms/edges**.
The controller test at `internal/racer/topology_controller_test.go:105-137`
covers both DaemonSet names and publication contents. The manifest explicitly
requires coordinated restart/drain (`deploy/racer/dataplane-config.yaml.tmpl:9-11`).
Thus changing both dataplane overrides is sufficient configuration; changing the
controller, membership versions, or the inherited ConfigMap is unnecessary.
This agrees with `/home/azureuser/design.md:29-33,59`: topology lives in the
dataplane with bounded hops. A one-pod canary or measurement on mixed graphs is
invalid. The controller does not negotiate a safe mixed-version topology.

Reported motivation (not measurements from this preparation): 1500 clients,
C16, layer concurrency 1, both windows 2, cap16, opaque relay false; V3 mean links
about 2.55 versus V5 2.045, with current VF TX / verified bytes about 2.647.
Lower route amplification is a hypothesis, not a throughput guarantee. Reported
degree rises from about 36 to 63 (general code bound is 64), risking more peer
connections, socket buffers and memory even at unchanged cap16. Do not combine
this trial with the separate peer-cap reduction. Watch per-node/worker peers,
open connections, RSS, cgroup working set, socket/slab memory, OOM/restarts,
Relay/ciphertext occupancy, overload, retries, timeouts, and verified throughput.

## Prepare and review

From the assigned worktree, use a **new ignored `tmp/` directory** each time:

```sh
timeout --signal=TERM --kill-after=10s 60s python3 -B -m unittest discover -s hack/scripts/racer-rollout -p 'test_routing_v5.py' -v
timeout --signal=TERM --kill-after=10s 280s python3 -B hack/scripts/racer-rollout/routing_v5.py --state tmp/routing-v5-forward-UNIQUE
```

The phase budget is 280 seconds; internal deadline 270 seconds. Each kubectl
command gets TERM after 45 seconds, KILL after another 10; before/after exact
command checkpoints in `checkpoint.log` provide heartbeats within 60 seconds.
A timeout/failure means inspect, not unbounded replay. No `ready.json` means no
approved artifact, even if an intermediate `patch.json` exists.

Review `change.diff`: **only two scalar digits '3' -> '5'**, one per named
DaemonSet's dataplane env. Quotes, whitespace, comments, images, all aliases,
other env values and every other ConfigMap data byte remain unchanged. Missing,
duplicate, indirect, merged, shared or already-changed routing values fail closed.
The tool verifies both live templates' production image, explicit algorithm,
envFrom and effective cap16/window2/relayfalse, including the override values.
It does not certify load-generator state or all 1500 running pods.

Artifacts: `before.json`, `config.json`, `dataplanes.json`, `after-data.json`,
`patch.json`, `change.diff`, `dry-run.json`, `SHA256SUMS`, `ready.json`, checkpoint.
Hashes are SHA-256 of exact saved file bytes; reproduce with bounded
`sha256sum -c SHA256SUMS` using the artifact directory as working directory.
The patch JSON is deterministically serialized, including its guards. A fresh
resourceVersion intentionally changes its hash. Keep all artifacts ignored.

The patch atomically tests ConfigMap UID, resourceVersion and **full data**,
then replaces only `/data/racer-v2.yaml`. A stale guard causes no mutation;
never remove tests or force it through. Other resource specs/data are re-read
before server dry-run, but cross-object checks are observations, not an atomic
transaction. Server dry-run proves API acceptance and exact returned data,
not operator rendering, process configuration, or load safety.

## Parent-only transition gate

1. Record matched baseline window and memory/connection headroom. Pause **all**
   client/origin workload sources and drain in-flight work. Parent retains
   recovery responsibility while paused. Do not change C16/layer1/windows2,
   cap16/relayfalse, images, quotas, shares, or membership during this trial.
2. Generate a fresh plan after draining, inspect its diff/hash/ready marker and
   recheck live cross-resource baseline. Parent may then apply that exact patch:

   ```sh
   timeout --signal=TERM --kill-after=10s 45s kubectl --context=joolshev-scale-test --request-timeout=30s -n unbounded-system patch cm unbounded-component-overrides --type=json --patch-file tmp/routing-v5-forward-UNIQUE/patch.json
   ```

3. Check operator override status/events and both rendered DaemonSet templates.
   Keep load paused throughout reconciliation and the **entire 1500-pod fleet**
   rollout. Use bounded rollout checks in parent-owned phases. Verify all owned
   pods' revisions, readiness, unchanged image/imageID, explicit algorithm 5 and
   effective cap/window/relay baseline. No old V3 pods may remain; templates
   alone are not running-process evidence. Verify common membership convergence.
4. Resume only after parent health/recovery gates pass. Restore C16 with the
   same layer1 and both windows2; compare matched steady-state windows, not
   rollout/cache-warmup intervals. Measure VF TX / verified bytes alongside
   verified bytes/images, failure rates, latency and memory. Stop/drain and
   roll back on correctness failures, sustained regressions or unsafe memory.
   Parent chooses operational thresholds from its baseline, not a fabricated
   universal memory limit.

## Fresh exact rollback, including partial forward rollout

Parent pauses/drains before rollback too. Do not use rollout undo, stale PUT,
or an inverse with the forward resourceVersion:

```sh
timeout --signal=TERM --kill-after=10s 280s python3 -B hack/scripts/racer-rollout/routing_v5.py --state tmp/routing-v5-rollback-UNIQUE --rollback-from tmp/routing-v5-forward-UNIQUE
```

Rollback requires the same ConfigMap UID and **exact** forward-transformed full
data, then restores the original bytes with fresh resourceVersion/full-data
tests. It accepts live templates at 3 or 5 to permit recovery of a partial
rollout, but never permits serving traffic on that mixed fleet. Any unrelated
data drift or baseline/image change fails closed for separate review, with no
apply. Review inverse diff, hash and dry-run; parent applies the new rollback
patch with the same bounded patch command and rollback path. Complete both
DaemonSet rollouts and verify all 1500 pods back at 3, unchanged images and
baseline settings before resuming matched C16 load. Restoring only the ConfigMap
does not restore running processes. No cluster mutation was performed here.
