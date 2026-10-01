# Exact catalog weight plan and C0 runbook

## Scope and evidence

This is one offline tuning item, not an automatic cluster executor. Keep all 1,500
clients, the eleven C1 caps, sparse V5, origins, and integrity verification. The
parent owns pause, drain, opaque rollback, recovery, and resume. Do not combine
the opaque rollback with a shares trial or resume during mixed-scale publications.

`hack/scripts/racer-weight-plan.py` has three separate commands:

```
timeout --signal=TERM --kill-after=10s 60s python3 -B hack/scripts/racer-weight-plan.py capture --baseline BASELINE.json --output inputs.json
timeout --signal=TERM --kill-after=10s 180s python3 -B hack/scripts/racer-weight-plan.py plan --input inputs.json --output plan.json
timeout --signal=TERM --kill-after=10s 20s python3 -B hack/scripts/racer-weight-plan.py attest --plan plan.json --publication final-publication.json --output attestation.json
```

Output parents must already exist; outputs must not exist. Capture uses GET only,
selects an existing ready origin, fetches 512 small manifests, and retains only
sanitized metadata. No payload, token, certificate, or Secret is retained. Plan
and attest are offline. The input is pinned to the October 1 11:29:07.871078Z
baseline, not a generic optimizer silently accepting changed experiments.

Exact member and catalog hashes are checked before planning. Final map SHA-256:
`65ac1f3576631495585aebb42374fef3bedeea6ba82179f31ed0b57b9dc9ba47`.
Map encoding is compact JSON of UID-sorted `(UID, integer shares)` pairs. The
artifact binds every name to a UID, capture-time resourceVersion, original absent
annotation or explicit `1`, candidate value, rollback operation, and modeled
baseline/fixed/equal-healthy TX/RX. Refresh resourceVersions before execution.

## Code contract and deterministic search

Positive uint32 shares are accepted (`internal/racer/topology.go:372-391`);
maximum/invalid values are asserted in `membership_test.go:47-86`. Integer
placement compares cost/share with UID-order ties (`cmd/racer-dataplane/src/topology/placement.rs:72-120`).
V5 weights eligible equal-cost next hops (`topology/routing.rs:370-438`). Uniform
400/100 scaling preserves placement and expected routing probabilities, not
necessarily individual hashed next hops. Shares enter placement identity; more
than 64 changes lose incremental hints (`topology/membership.rs:78-81,114-146`).

The model assumes complete-catalog shuffle, primary hits, observed fixed demand,
and expected healthy shortest-path routing. It excludes retries, coalescing,
warming, CPU/disk bottlenecks and transition costs. Baseline TX fit is r=0.998684.
Current metadata identities and catalog match the baseline constraints; they are
not an archived historical controller publication.

1. Baseline shares 400 healthy / 100 impaired. Candidate 0 uses 400 / 70.
2. Evaluate fixed demand and equal healthy demand at the same total, leaving
   impaired demand fixed. For node i, L is its maximum TX/RX across both scenarios.
3. Healthy update: `w * sqrt(mean_healthy(L) / L)`. Normalize with 60 bisection
   steps on scale [0,10], clip to [200,800], total 595600. Floor then allocate
   remainder by descending fractional part, ascending UID-position tie.
4. Impaired ratio R is maximum scenario/direction load divided by that node's
   original fixed-demand baseline directional load. If R > .98, replace weight
   with `max(1, min(w-1, floor(.95*w/R)))`; otherwise retain it.
5. Evaluate candidates 0 through 5. Accept only candidates with all 22 impaired
   limits satisfied in both scenarios. Minimize the worst fleet TX/RX; ties use
   moved primary bytes then iteration. This is not a global optimality proof.

Hash each occupied slot against all members once. Exact pruning keeps every
impaired candidate and healthy integer costs <=4 times minimum healthy cost:
outside that bound no healthy [200,800] weight can beat the minimum-cost node.
No approximation to cost or comparison is used for final ownership.

Candidate 5: fixed TX/RX peaks 10.900039/10.693534 Gbps; equal-healthy peaks
11.006339/9.942160. Ownership changes 21,645,305,856 bytes (7.887099% of catalog),
not a physical migration estimate. Healthy shares 228-800; impaired 61-70.
Expected links 2.045588 fixed, 2.045456 equal-healthy. Worst impaired ratio .979898.

NIC-only conditional multiplier is 1.135709. Enforcing absolute impaired baseline
ceilings at higher demand limits proportional scaling to 1.020514, or healthy-only
scaling to 1.026169. These are model sensitivity bounds, NOT a prohibition on later
measured gain: the constraints were chosen at baseline demand, and actual progress
is coupled to contention. C1 limits concurrency, not achieved goodput. Reassess
impaired resource/error behavior from observations during any authorized trial.

## Parent execution and rollback checklist (not executed by this item)

1. Finish opaque rollback independently. Verify all clients C0 and existing full
   drain gates. Preserve exact cap map, verification, pod identities and images.
2. Reproduce and independently review the exact plan. Verify member/catalog/cache
   identity, node UIDs, original annotations, current admitted membership, and
   rollback values. Keep durable progress for each subsequent write.
3. Under C0, apply final candidate annotations directly in bounded, concurrency-
   limited batches with fresh UID/resourceVersion/current-value guards. Do not
   perform a separate 400/100 scaling rollout. All 1,500 annotations change from
   absent/1 to the candidate map, even nodes remaining at normalized 400.
4. Expect non-atomic mixed-scale intermediate publications. They are not a load
   trial. Keep C0, allow controller progress, and do not infer convergence from a
   successful patch. On interruption inspect actual values before resuming only
   unapplied entries. On failure restore exact absent/1 values under C0, using
   fresh guards that first verify the candidate value; never blindly overwrite.
5. Check all final Node annotations and controller last-admitted values against
   the map. Obtain an authenticated `/v1/snapshot` through an existing approved
   control-client channel. Do not export credentials or relax authentication. Run
   offline `attest`; retain its sequence and membership version plus publication.
   The version ConfigMap alone is not the member map. Check controller replicas
   serve the same final publication after replication. Prepare/commit/install are
   distinct (`internal/racer/topology.go:148-175`).
   Alternatively, for committed membership application only, use the bounded
   Kubernetes-metadata helper below. It does not claim controller replica serving
   health or attest cache-catalog contents; preserve those separate gates.
6. On dataplanes supporting `GET /debug/membership`, collect a direct response
   from every expected process, binding each response to its Node UID, Pod UID,
   container identity and restart count. Older images return 404 and cannot attest.
   Require `fully_applied=1`, `pending_sequence=0`, and matching accepted sequence,
   membership version and canonical membership hash from step 5. The hash is the
   controller's full canonical membership hash, NOT the planner's UID/shares hash.
   `matching_workers` counts actual worker-installed publication sequences equal
   to the accepted sequence, observed within two seconds; it must equal the
   nonzero `expected_workers`. Missing, stale, stopped or lagging workers fail
   closed. Pending counters describe downloaded/prepared state, never acceptance.
   The diagnostic reads existing state without advancing control or doing I/O.
   This is a bounded observation, not a lease or proof that the controller has
   not advanced: hold the final controller map stable, bracket fleet collection
   with controller checks, and invalidate evidence after a restart or map change.
   Ready and poll cursors alone remain insufficient. The old-membership 30-second
   grace is NOT a fleet convergence deadline. No keys or credentials are exposed;
   no membership identities are added as Prometheus labels.
7. Parent alone resumes a guarded warm-up, retains all verification, and measures
   a full steady window only after placement/cache effects stabilize. Compare
   verified goodput, errors, all-node progress, TX/RX, impaired-node behavior and
   readiness against the appropriate post-rollback baseline. Roll back under C0
   on the parent-defined failure gates. Do not deploy intermediate optimizer steps.

Focused tests (artifacts optional, no cluster access):

```
timeout --signal=TERM --kill-after=10s 30s env RACER_WEIGHT_ARTIFACTS=/path/to/artifacts python3 -B hack/scripts/racer-weight-plan_test.py
timeout --signal=TERM --kill-after=10s 30s python3 -B hack/scripts/racer-membership-attest_test.py
```

### Read-only Kubernetes membership attestation alternative

After the parent finishes rollout and drain, with the controller map held stable:

```
timeout --signal=TERM --kill-after=10s 280s python3 -B hack/scripts/racer-membership-attest.py --context joolshev-scale-test --expected-image ghcr.io/azure/racer-dataplane:EXACT_COMMIT --expected-count 1500 --output NEW_ATTESTATION.json
```

Add `--candidate-plan plan.json` after a shares trial to require its exact Node
UID/name/annotation/shares map and verify its map hash. Without it, the helper
attests the current committed map, not the desired optimizer candidate. Output is
optional (stdout otherwise), exclusive-create, with an already existing parent.
Do not interpret output from a nonzero exit as success.

The helper GETs only the installation/version ConfigMaps, Nodes, relevant Pods and
DaemonSets through the explicit authenticated Kubernetes context. It reconstructs
the full canonical membership document from last-admitted records and requires its
SHA-256 to equal the durable membership hash. Canonical field order, omitted site
and NUMA fields, Unicode escaping and no trailing newline match the wire codec.
Installation UID binding and positive monotonic-counter shape are checked.
Annotations alone or version counters alone are never sufficient.

It verifies exactly one ready, running, expected-image dataplane per admitted Node,
current DaemonSet ownership, and admitted peer endpoint matching the Pod IP/port.
One existing net-node host session performs 32 concurrent read-only curls against
the identified diagnostic endpoints. Each curl has a two-second request ceiling,
four-second external TERM bound, and 1 KiB response limit. The remote process has
its own timeout; the operation has a 265-second alarm plus cleanup headroom within
the required external 280-second bound. Progress is reported every 30 seconds
while a command runs. No retries, pod creation, private identity reads or Secrets.

Both inventory brackets must match, including durable data and installation/CM
UIDs, Node mapping, Pod/container/image/restart identities, endpoints, and selected
net-node access identity. ResourceVersion alone may change on unchanged controller
CAS confirmations and is not compared. Missing, pending, stale-worker, hash/version
mismatch, HTTP error, rollout or restart fails closed. A fresh attempt requires
new complete brackets, not reuse of previous successful rows. Each process was
observed within the collection interval; the report is not simultaneous fleet
state, a future lease, or authorization to resume load.

Captured operational JSON remains outside git. Before later execution the parent
must persist candidate, rollback, input hashes, and progress in its approved
operational location; this runbook is not permission to mutate the cluster.
