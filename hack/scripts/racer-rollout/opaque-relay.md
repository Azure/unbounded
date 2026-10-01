# Reviewed opaque relay experiment

This deliberately narrow tool changes only `RACER_OPAQUE_RELAY` in the two Racer
DaemonSet dataplane overrides in `unbounded-component-overrides/data/racer-v2.yaml`.
It leaves images, routing, windows, resource limits, aliases and all other YAML
bytes unchanged. Missing relay entries become explicit `"true"`; existing literal
`"false"` entries are replaced. Other values, duplicate fields, ambiguous layouts,
or edits affecting unrelated alias consumers fail closed. This is not a generic
environment editor.

Production reachability (paths relative to `cmd/racer-dataplane`):
`Config::from_lookup` in `src/config.rs` parses the setting,
`WorkerApplication::assemble` in `src/app.rs` installs it, and
`PeerServer::serve_connection` in `src/peer/server.rs` selects ciphertext streaming
only for intermediate HTTP hops without an admitted native transfer.
The bounded pipe path is `HttpIo::relay_body` in `src/http/connection.rs`.
In `src/peer/tests/opaque.rs`,
`signed_full_page_bootstrap_and_page_stream_without_transit_allocation` checks
payload equality, decryption, bounded transit admission, keepalive and fallback;
`signed_materialized_full_page_bootstrap_and_page_keepalive` covers the disabled
streaming path, and `signed_success_truncation_closes_relay_and_pooled_destination`
covers truncation. This retains the
user design's encrypted transit and integrity requirements (`~/design.md:15-19,39`)
and does not alter its bounded routing (`:29-33`). No throughput gain is promised.
It does not fix the separate current Gantry subscription-copy versus design-splice
mismatch (`pkg/racersdk/http_transfer.go:8-14`, `~/design.md:48-53`).

## Plan and dry-run (no persisted cluster mutation)

Use a fresh state directory inside this worktree. Snapshot/plan creation issues
GETs only; artifacts are exclusively created and never overwritten. Keep them
ignored, private, and out of commits. The inherited ConfigMap's relay value must
be `false`; its complete snapshot is also retained and checked for drift.

```sh
timeout --signal=TERM --kill-after=10s 300s python3 -B hack/scripts/racer-rollout/opaque_relay.py plan --context joolshev-scale-test --state tmp/relay-enable
```

Review `snapshot.json`, `plan.json` (including `original_values`, null meaning
absent/inherited), `patch.json` and `plan.diff`. Copy the printed plan hash:

```sh
timeout --signal=TERM --kill-after=10s 300s python3 -B hack/scripts/racer-rollout/opaque_relay.py verify --context joolshev-scale-test --state tmp/relay-enable --approved-plan HASH
```

`verify` sends `--dry-run=server` only. A successful dry-run is not approval to
apply, proof of fleet convergence, or evidence of throughput improvement.

## Parent-only apply and rollback

The parent owns the FQ canary, load reduction, pause/drain, rollout readiness,
configuration attestation and resumption. Do not overlap independent experiments.
After those gates and explicit review, the parent can use the same command as
`verify` with phase `apply`. Apply performs its own fresh GET/drift checks and
server dry-run before the write. It never pauses, resumes, deletes pods, or
restarts workloads directly. The operator owns reconciliation.

The JSON patch atomically tests operator ConfigMap UID, resourceVersion and
**entire data**, then replaces only `racer-v2.yaml`. The exact reviewed patch is
sent from memory via stdin. Admission changes outside the intended configuration
are rejected at dry-run. Conflicts fail without retry. An already-applied exact
configuration skips mutation. The inherited ConfigMap check is a separate GET,
not an atomic cross-resource transaction: parent must prevent concurrent tuning
and attest the effective configuration after reconciliation.

For rollback, use the original enable plan and a **new** state directory:

```sh
timeout --signal=TERM --kill-after=10s 300s python3 -B hack/scripts/racer-rollout/opaque_relay.py rollback-plan --context joolshev-scale-test --state tmp/relay-rollback --source-plan tmp/relay-enable/plan.json
```

Rollback planning accepts only the same ConfigMap identity and exact enabled
configuration (server bookkeeping may change), plus unchanged inherited defaults.
It restores the original YAML text byte-for-byte, including removing inserted
env entries, and guards with the **current** UID/resourceVersion/entire data.
Review the new hash and diff, then `verify` and parent-only `apply` using the
rollback state/hash. Never apply an old inverse patch with guessed resourceVersion.
Unrelated changes cause rejection, not overwrite; review a fresh plan or recover
manually under parent control. Restoring this ConfigMap is not itself proof the
fleet has rolled back.

Every phase has a 285-second internal deadline, bounded subprocesses with cleanup,
and a timestamped checkpoint. No Go files are changed; Python tests run with:

```sh
timeout --signal=TERM --kill-after=10s 300s python3 -B -m unittest discover -s hack/scripts/racer-rollout -p 'test_*.py'
```
