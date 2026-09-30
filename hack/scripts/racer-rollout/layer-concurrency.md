# Loadgen-only layer-concurrency 4 to 1 trial

This is an active `unbounded-system/racer-loadgen` DaemonSet argument trial,
not a runtime build, deployment default change, or Gantry/Racer tuning. The
preparation-only `layer_concurrency.py` has no apply operation. Run from the
assigned worktree. Keep snapshots, plans, review, and hashes in ignored `tmp/`.

## Semantics and fixed controls

The flag defaults to 4 (`cmd/racer-loadgen/main.go:57-63`). The puller fetches
manifest/config first, then starts `min(layer-concurrency, layers)` layer
workers (`cmd/racer-loadgen/pull.go:206-219`). Tests assert both reaching and
not exceeding image concurrency times layer concurrency
(`cmd/racer-loadgen/pull_test.go:413-429`). With eight layers and effective c6,
the layer-request ceiling per loadgen falls from 24 to 6. The idle connection
pool allowance also follows this flag (`pull.go:68-99`); this is an inherent
effect of the single argument, not a second configuration change. Only complete
verified images earn verified-byte credit (`pull.go:185-199`).

This follows `~/design.md:23-39,53,63-68` intent for bounded concurrency and
integrity without claiming new zero-copy behavior. This trial does not change
the implementation or resolve the known splice/copy distinction recorded in
`page-window.md:38-43`.

Preserve image 3235e717, catalog 512, verify=true, all other args, control mount,
resources, selectors, and the existing RollingUpdate maxUnavailable=100%.
No ConfigMap, Gantry, Racer, override, or load-control writes. The parent owns
effective c6 and must serialize configuration changes. A template patch triggers
loadgen restarts and catalog regeneration: existing 100% unavailability is NOT
a canary or uninterrupted-load guarantee. Parent owns pause/drain, recovery,
warm-up, readiness, and the observation interval; do not mix those samples with
the trial. Do not infer pod convergence from a template dry-run.

Parent-provided baseline (not remeasured here): windows 2, relay false, c6,
5.4 Tbps, 2.25% errors, original six bad nodes now zero, Racer2 memory peak
1.38 GiB under 2 GiB. Parent must track verified goodput, error rate, bad nodes,
fleet memory/restarts and queue pressure, with a memory abort threshold below
2 GiB. No throughput or memory improvement is established by this preparation.

## Prepare, review, parent-only apply

Each phase deadline is <=300s, heartbeat 60s. Helper commands finish within 55s
(TERM at 45s, KILL after 10s), checkpoint before/after every command, and there
are only two commands. No opaque long-running rollout watcher is started.

```sh
timeout --signal=TERM --kill-after=10s 300s python3 -B -m unittest discover -s hack/scripts/racer-rollout -p test_layer_concurrency.py
timeout --signal=TERM --kill-after=10s 180s python3 -B hack/scripts/racer-rollout/layer_concurrency.py --context joolshev-scale-test --state tmp/layer-trial
```

Review `before.json`, `after.expected.json`, `after.server-dry-run.json`,
`patch.json`, `rollback.json`, and `plan.json`. Save review and a checksum file
with the **reviewed** patch hash from `plan.json`, for example
`<hash>  patch.json` in `reviewed.sha256`. Check from the state directory, then
only the parent executes this mutation after its load/readiness gates:

```sh
timeout --signal=TERM --kill-after=10s 30s sha256sum --check reviewed.sha256
timeout --signal=TERM --kill-after=10s 60s kubectl --context joolshev-scale-test --request-timeout=45s -n unbounded-system patch daemonset racer-loadgen --type=json --patch-file patch.json -o json
```

Capture the apply response and a fresh after snapshot in the state directory.
The patch atomically tests UID, resourceVersion, full old spec, and old args;
its sole mutation replaces one argument element. Both separate and equals
flag forms are supported; missing, duplicate, or unexpected old values fail.
Server dry-run must return exactly the expected spec. Status-only RV changes
can invalidate the plan: regenerate into a fresh state directory and review
again, never edit a reviewed patch's RV. No blind retry after ambiguous apply;
inspect live UID/spec and parent rollout state first.

## Rollback (parent only, after inspecting live state)

`rollback.json` records the exact restore spec, but intentionally contains no
executable patch with a guessed future resourceVersion. Prepare rollback only
after the forward patch is live. The original UID and exact trial spec must
still match; drift requires review, not overwrite. A fresh GET supplies rollback
RV. Reverse changes only that same args element, restoring the entire old spec.

```sh
timeout --signal=TERM --kill-after=10s 180s python3 -B hack/scripts/racer-rollout/layer_concurrency.py --context joolshev-scale-test --state tmp/layer-rollback --rollback-of tmp/layer-trial/before.json
```

Review and hash the new rollback patch, then use the same parent-only apply
commands from its state directory. Parent gates the second loadgen rollout and
recovery. There is no automatic rollback or load adjustment.
