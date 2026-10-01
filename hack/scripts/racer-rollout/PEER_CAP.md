# Racer peer cap: one reversible operational item

`peer_cap.py` only reads live state and runs a **server dry-run**. It never applies,
drains, scales, restarts, or resumes load. Requires the existing rollout tooling's
Python 3, PyYAML, kubectl, and GNU timeout. Run from the assigned worktree, with the
intended kubectl context already selected. Do not use `rollout.py configure` for
this item: that function changes images and other configuration.

## Rationale and risk

The source default is 2 (`cmd/racer-dataplane/src/config.rs:231`). The cap is
retained per worker, not divided among workers (assertions in
`cmd/racer-dataplane/src/app_native.rs:503-510`), and constructs the worker's HTTP
pool (`cmd/racer-dataplane/src/app.rs:655-668`). Peer transfer reaches immediate
checkout (`cmd/racer-dataplane/src/peer/transfer.rs:287-293`); an active endpoint at
its cap returns `Overloaded` without a wait (`cmd/racer-dataplane/src/http/pool.rs:494-499`).
The test at `http/pool.rs:762-773` specifically asserts immediate peer overload.
RDMA sessions also use this cap (`app.rs:738-746`).

The fresh preparation snapshot found both named DaemonSets inheriting 16 from
`racer-dataplane-config`, with no explicit env value. The change adds explicit 4
to the dataplane container in each named override in existing `racer-v2.yaml`.
It does not change the inherited ConfigMap. All prior YAML bytes, aliases,
images, resource quotas, and other ConfigMap entries remain untouched.

Intended outcome: reduce connection fanout and associated buffered work. This is
an experiment, not a quota expansion or a promised throughput improvement. It
may increase immediate PeerCheckout overload, alternate-route retries, and
latency, or reduce useful parallelism. It does not reduce distinct peer count
directly or guarantee relief of the separate Relay quota. This aligns with the
connection-minimization intent in `/home/azureuser/design.md:29-33`; that design
does not prescribe a cap. The reported 1500-client C6/layer1/window2/relayfalse
run, roughly 800 peers on bad nodes, Relay near 200/255, high churn, and zero
images motivate the experiment but were not remeasured by this preparation.

## Plan, review, and parent-owned execution

Each invocation must use a **new ignored directory under `tmp/`**. The internal
phase deadline is 240 seconds, each kubectl command is bounded to 45 seconds,
and before/after command checkpoints provide a heartbeat within 60 seconds.

```sh
timeout --signal=TERM --kill-after=10s 240s python3 -B hack/scripts/racer-rollout/peer_cap.py --state tmp/peer-cap-forward-UNIQUE
timeout --signal=TERM --kill-after=10s 60s python3 -B -m unittest discover -s hack/scripts/racer-rollout -p 'test_peer_cap.py' -v
```

Review `change.diff`: exactly two additions of `name:
RACER_CONNECTIONS_PER_NEIGHBOR` / `value: "4"`, one per named dataplane override.
Review `before.json`, `config.json`, `dataplanes.json`, `after-data.json`,
`patch.json`, `dry-run.json`, and `checkpoint.log`. Live artifacts are ignored;
do not commit them. The JSON patch atomically tests UID, resourceVersion, and
the **entire current ConfigMap data**, then replaces only `/data/racer-v2.yaml`.
The source ConfigMap and both DaemonSet specs are rechecked before dry-run, but
cross-object observations cannot be atomic with the ConfigMap patch.

**Parent only, after its drain and recovery gates:** generate a fresh plan,
review it, and apply that exact `patch.json` using a bounded `kubectl patch cm
unbounded-component-overrides --type=json --patch-file ...` in
`unbounded-system`. Never remove the JSON tests to force a stale patch through.
Do not treat server acceptance of a ConfigMap string as proof the operator
rendered it: the parent must check override status/events and both resulting
DaemonSet templates, then complete a full dataplane rollout before measurement.
Verify every selected pod has the expected revision, image, readiness, and
effective cap 4; templates alone are not proof of running process configuration.
Keep all other load and configuration variables fixed. Resume only under the
parent's existing health/recovery gates; compare completion, throughput,
PeerCheckout errors, retries/churn, peers, buffers, and Relay occupancy. No
load control is included here.

## Rollback: fresh UID/RV and full dataplane rollout

Do not reuse a forward resourceVersion, use `kubectl rollout undo`, or replace
the whole ConfigMap from a stale backup. After the parent's drain, generate:

```sh
timeout --signal=TERM --kill-after=10s 240s python3 -B hack/scripts/racer-rollout/peer_cap.py --state tmp/peer-cap-rollback-UNIQUE --rollback-from tmp/peer-cap-forward-UNIQUE
```

Rollback fetches fresh UID/resourceVersion and full current data, requires the
same ConfigMap identity and exact forward data, and restores the original
`racer-v2.yaml` bytes. It accepts DaemonSet templates still at inherited 16 or
already at explicit 4, so a partially reconciled forward rollout can be rolled
back. Any ConfigMap data drift or inherited cap change fails closed for review.
The parent reviews the inverse diff, applies the newly guarded rollback patch,
then verifies **both full dataplane rollouts** back to inherited 16 and unchanged
images before resuming. Restoring the ConfigMap alone does not restore running
processes. If unrelated edits have occurred, prepare a separately reviewed
minimal inverse patch with fresh guards; never overwrite those edits.
