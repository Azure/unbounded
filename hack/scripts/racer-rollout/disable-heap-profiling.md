# Explicitly disable Racer heap profiling

This is a separate opt-in operation, not an image upgrade phase. A production
binary built without `heap-profiling` rejects a set `RACER_HEAP_PROFILE_ADDR`, even
an empty value. Do not replay completed `upgrade.py` phases or rebuild images to
remove an old profiling override.

Requirements: Linux `/dev/stdin`, Python 3.9+, PyYAML, GNU timeout, kubectl. Run in the assigned
worktree. Keep load paused at concurrency zero; the parent owns recovery and
all cluster writes. Read-only preparation:

```sh
timeout --signal=TERM --kill-after=10s 300s python3 -B hack/scripts/racer-rollout/disable_heap_profiling.py plan --context joolshev-scale-test --state tmp/disable-heap-profiling
```

The tool GETs only `unbounded-system/unbounded-component-overrides`, snapshots
it, and generates private, exclusive-create `snapshot.json`, `plan.json`,
`patch.json`, and `plan.diff`. Do not commit these live artifacts. Review the
diff and plan hash before the parent runs:

```sh
timeout --signal=TERM --kill-after=10s 300s python3 -B hack/scripts/racer-rollout/disable_heap_profiling.py apply --context joolshev-scale-test --state tmp/disable-heap-profiling --approved-plan REVIEWED_HASH
```

Only the `racer-v2.yaml` key is replaced. Both named Racer DaemonSet overrides
must exist with exactly one `dataplane` container. The tool removes
`RACER_HEAP_PROFILE_ADDR` and only profiling options (`prof`, `prof_*`,
`lg_prof_*`) from literal `MALLOC_CONF`/`_RJEM_MALLOC_CONF` values. An allocator
env entry is removed only if that leaves no options; unrelated allocator options
are retained. Unknown/malformed allocator encodings and `valueFrom` fail closed.
`envFrom` requires separate review and is rejected. Images, routing, resources,
guards, scraping, TMPDIR, volumes, init containers, rollout policy, and other
ConfigMap keys remain unchanged.

Source-span edits preserve unrelated YAML bytes, anchors, and aliases. Shared
profiling entries across the two targets are supported. Any alias edit affecting
an unrelated consumer fails semantic validation; dangling aliases, duplicate
keys, overlapping edits, flow-style env sequences, and removal leaving an empty
env sequence fail closed rather than rewriting broad YAML structures.

Apply reproduces the transformation, validates the review hash and patch artifact,
GETs current state, and rejects drift. The JSON patch tests UID, resourceVersion,
and the entire data map atomically before replacing the single key. Server dry-run
must preserve all unrelated configuration before the actual patch is sent. A
conflict is not retried: re-snapshot and review. An already-applied result is
read-only. Unexpected post-apply admission changes are reported, not rolled back.

Each phase is bounded internally to 285 seconds. Subcommands use timeout with
SIGTERM and 10-second cleanup grace, and checkpoint.log records before/after
commands with last success. Inspect that checkpoint and live state after any
interruption before resuming.

Updating the ConfigMap can immediately reconcile and roll both DaemonSets under
their existing strategy. Before applying, the parent must confirm the production
images, paused load, expected target identity, and unchanged live configuration.
After applying, inspect both reconciled templates and replacement pods for absent
profiling settings, expected images, startup/readiness, and health before further
rollout or load gates. This tool does not delete pods, restart workloads, change
rollout limits, claim recovery, or resume load.

Tests:

```sh
timeout --signal=TERM --kill-after=10s 300s python3 -B -m unittest discover -s hack/scripts/racer-rollout -p 'test_*.py' -v
```
