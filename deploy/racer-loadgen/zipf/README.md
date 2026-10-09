# Racer-only Zipf benchmark

Direct UDS, 512 x 2 GiB generic blobs (1 TiB shared), Zipf 0.5,
seed `zipf-balanced-v1`, concurrency 16, no client
hash verification. Loadgen and dataplane select `agentpool=ddsv6`. The overlay
pins all three images to source tag `f9a088a22a29cf8549e1945af833f508a14199d1`.
Confirm builds and image digests before deployment. SHA tags are not digest pins.

This is an owned-only disk-retention comparison, not the default retention policy.
`RACER_ADMISSION_MODE=disabled` disables second-sight disk admission only; resource
admission and integrity checks remain active. The seed stays `zipf-balanced-v1`;
larger blobs change content digests. Do not erase existing disk data.

Node budgets are plaintext 32 GiB, ciphertext 64 GiB, dirty 2 GiB, registered 2 GiB,
and request contexts 1 GiB. Counts are flights 2048, queue entries 16384, client
connections 16384, pipes 1024, and relay transfers 1024. Per-neighbor connections
are 8, peer inflight maximum 1024, peer per-neighbor maximum 128, and active path
searches 64. Optional path-cache settings remain unchanged. Worker sizing stays automatic;
the target nodes select 21 I/O and 11 crypto workers. No Kubernetes limits are set.
These budgets provide about 1.52 GiB plaintext and 3.05 GiB ciphertext per I/O
worker. Phase 3 retains phase 2's payload budgets and raises the capacity package
listed above together. Node quotas are divided by I/O workers, then outbound
connections get one quarter: 195 slots per worker at 21 workers
(`cmd/racer-dataplane/src/app.rs:2399-2414`, `admission.rs:134-142`). Total and
ingress connection capacity also rise. Keep all other budgets, automatic threads,
no CPU or memory limits, and three approved NVMe devices per node unchanged.
Require fresh MemAvailable above 128 GiB on every node before changing budgets.
Patch only the authorized capacity env values with JSON tests for resourceVersion,
spec, and old env. Env names and defaults are in
`cmd/racer-dataplane/src/config.rs:195-240`.
Keep the existing 100% rollout and zero grace. Leave loadgens running with their
current template, including the user's live `--blob-concurrency=4`, to avoid
hashing the catalog again. Do not apply the loadgen overlay for this experiment.

Change live request concurrency through `racer-loadgen-control`, not the Pod
template. Each generic operation contains one blob, so `--blob-concurrency=4`
does not increase parallel reads for this workload. Changing the Pod template
restarts catalog hashing. Leave loadgen running when the experiment ends.

October 9 results across all 100 loadgens (C means request concurrency):

| Configuration | Concurrency | Gbit/s | Failed pulls |
| --- | --- | --- | --- |
| Connections only: 4096, payload 8/16 GiB | C16 | 945.0 | 83.66% |
| Payload 32/64 GiB, connections 4096 | C16 | 1,116.6 | 78.05% |
| Full capacity package above, final window | C16 | 1,668.3 | 0.0135% |
| Full capacity package | C32 | 1,674.3 | 0.0135% |
| Full capacity package, sustained 147-second check | C64 | 1,727.8 | 1.183% |

C16 is the final setting. C32 missed the required 5% throughput gain; sustained
C64 exceeded the 1% failure limit. Final C16 had 7,405 successes and one timeout,
about 11.7% more throughput than the original 1,494 Gbit/s C8 reference. These
tests do not isolate which limit helped or prove long-term reliability.

The aggressive dataplane rollout caused NVMe device-busy errors and file-slab
fallback on 93 nodes. Check raw-device selection after each rollout; recovery
verified all 300 approved devices across 100 nodes, with 21/11 workers and no
resource limits. Loadgens did not restart.

`control.yaml` and the source startup fallback select 16. The live startup
fallback remains 8; its mounted control file selects 16. Live
`--blob-concurrency=4` and source `--blob-concurrency=1` remain unchanged.

Each loadgen hashes the full catalog before origin readiness. Health and metrics
start first; readiness remains false during hashing. Startup is bounded by
`--startup-timeout=60m` and a readiness startup probe with 390 attempts at 10-second
intervals (65 minutes). The probe leaves room for the application deadline.
The prior 256 GiB catalog took about six minutes to hash; 1 TiB needs more headroom
than the prior 25-minute deadline. This is not a workload duration; configured load continues
after startup. Observe startup in bounded five-minute phases, not a full-length
rollout wait. Record startup logs and restarts; readiness is false during hashing.
The flatter exponent changes popularity only, not content. Changing it requires
a loadgen restart and catalog hashing; existing dataplane cache data is retained.

Use **2147483648 bytes per successful pull** for logical throughput. Do not reuse
the old 64 MiB dashboard multiplier. Received bytes include failed partial reads;
logical delivery is not network traffic or client-verified content. Measure fresh
counter increments after warmup. Memory, disk, and peer counters count acquisition
events across providers and requesters, not exclusive client-read outcomes.

Only for a separately authorized catalog rollout, patch the loadgen as follows.
Do not run this on the active C16 test; it restarts catalog hashing:

```sh
timeout --signal=TERM --kill-after=10s 60s kubectl --context=joolshev-nvme-test -n unbounded-system patch daemonset racer-loadgen --type=strategic --patch-file=deploy/racer-loadgen/zipf/daemonset-patch.yaml
```

First check the live context, control ConfigMap, image, and dataplane Pod UIDs.
Verify those UIDs are unchanged afterward. This keeps the existing image, buffers,
dataplane, volume, and control ConfigMap; do not apply the full overlay for this
change. The 100% rollout restarts all loadgens and interrupts load during hashing.

## Standalone bootstrap for operator v0.8.0

Read-only checks on 2026-10-09 found operator `v0.8.0` and neither Racer CRD
on `joolshev-nvme-test`. The existing overrides ConfigMap has net entries.
Do not apply these overrides to that operator or upgrade it just for this test.
The published [v0.8.0 registry source](https://github.com/Azure/unbounded/blob/v0.8.0/internal/operator/reconciler.go#L94-L108)
registers net, machina, gantry, token-refresher, metalman, and storage, not Racer.
This was checked through the GitHub contents API, not Git history. The live image
tag matches that release; its exact binary provenance was not checked.

Current source supports Racer workload overrides, but only for workloads it
already generates (`internal/operator/override/resolve.go:50-76`). The controller
does not create dataplanes (`internal/racer/workload/workload.go:4-5`). A standalone
controller alone is therefore not a deployment solution. A separate standalone
installation needs current CRDs, active admission policies before RBAC, serving
TLS and bootstrap trust, a new installation identity, and a generated dataplane
workload. It must not compete with an operator for these resources. That complete
standalone bootstrap is provided by `hack/cmd/racer-bootstrap`. It calls only the
existing Racer component planner and executor, including the staged identity and
TLS protocols and authoritative dataplane builder. It never runs the full Site
reconciler. Leave the shared overrides ConfigMap untouched: the tool reads this
directory's local override document instead. Never reset an existing marker.

For the verified Racer-free v0.8.0 operator, run these bounded phases:

```sh
timeout --signal=TERM --kill-after=10s 60s kubectl --context=joolshev-nvme-test apply -f deploy/racer/crd/
timeout --signal=TERM --kill-after=10s 300s go build -o bin/racer-bootstrap ./hack/cmd/racer-bootstrap
timeout --signal=TERM --kill-after=10s 250s bin/racer-bootstrap --context=joolshev-nvme-test --apply
```

The bootstrap command installs policies and bindings and waits for their type checks
before granting RBAC. It creates the benchmark volume, installation identity,
serving TLS, configuration, and controller. It creates the dataplane only after
the controller commits valid installation state. Restart the command after an
interrupted phase; it recovers through existing staged state, not a new UUID.
It refuses another operator version and never updates an existing dataplane.
The parent owns device annotations, dataplane restarts, and load enablement.
For an existing standalone dataplane, apply its local override separately; the
bootstrap command intentionally leaves it unchanged. With the rollout authorized:

```sh
timeout --signal=TERM --kill-after=10s 60s bash -o pipefail -c 'python3 -c '\''import json,yaml; d=yaml.safe_load(open("deploy/racer-loadgen/zipf/racer-overrides.yaml")); print(json.dumps(next(e["patch"] for e in d["overrides"] if e["kind"] == "DaemonSet" and e["name"] == "racer-dataplane")))'\'' | kubectl --context=joolshev-nvme-test -n unbounded-system patch daemonset racer-dataplane --type=strategic --patch-file=/dev/stdin'
```

This is a bounded bootstrap tool, not a continuous TLS-maintenance service. Rerun
it for Racer TLS upkeep well before the 14-day serving leaf expires; it may roll
controllers. Do not introduce a second manager without reviewing ownership.

Standalone bootstrap is required on v0.8.0; applying the loadgen overlay or the
shared overrides alone cannot install Racer. After bootstrap and the NVMe gate
below, deploy loadgen paused, leaving the shared overrides ConfigMap untouched:

```sh
timeout --signal=TERM --kill-after=10s 60s bash -o pipefail -c 'kubectl kustomize --load-restrictor LoadRestrictionsNone deploy/racer-loadgen/zipf-paused | kubectl --context=joolshev-nvme-test apply -f -'
```

Confirm every loadgen reports applied concurrency zero and no in-flight reads.
After checking dataplane raw-device selection and Pod resource limits, enable
load with the concurrency patch shown below, using `"8"`.

With a verified compatible Racer manager, use `racer-overrides.yaml` for the two
Racer workloads only. Current dataplane defaults have resource requests and no
limits (`internal/operator/components/racer/racer.go:276-282`). Overrides reject
null deletion, so this file deliberately does not claim `limits: {}` removes
existing limits. Check the final Pod spec and namespace LimitRanges; stop if CPU
or memory limits are injected. Do not alter unrelated overrides to fix this.

Both benchmark DaemonSets request 100% unavailability and zero termination grace.
This can interrupt all reads and lose recent checkpoints. Normally pause and drain
load before changing dataplanes; zero grace is not a safe drain mechanism. For this
authorized disposable-cache experiment, the parent permits rollouts with C8 active.
Exclude rollout and catalog startup from cache-balance measurements.

## NVMe gate

No disk selector is shipped. There is no safe generic "all NVMe" selector.
Inventory each node's by-id targets and approve only disposable local devices,
excluding OS disks, partitions, mounted devices, holders, and protected data.
Use an anchored explicit ID allowlist per node. First and last 1 MiB must already
be zero. Never clear them automatically. Startup may silently use file slabs or
only a subset of the devices; verify raw-device logs and selected identities on
every node before enabling load. See `docs/content/reference/racer.md:111-185`.

## Render and validate

Run at repository root. The helper prints a merge patch; it does not call kubectl.
Its resourceVersion guards against a concurrent edit. It adds only the dedicated
`racer-zipf.yaml` data key, preserving the existing net entries. A conflicting
existing key is rejected. Save output only in project-local scratch space.

```sh
timeout --signal=TERM --kill-after=10s 300s python3 -B deploy/racer-loadgen/zipf/render_test.py
timeout --signal=TERM --kill-after=10s 60s kubectl kustomize --load-restrictor LoadRestrictionsNone deploy/racer-loadgen/zipf
```

The following override-ConfigMap path is ONLY for a compatible full operator,
not the v0.8.0 standalone path above. After all gates pass:

```sh
timeout --signal=TERM --kill-after=10s 60s bash -o pipefail -c 'kubectl --context=joolshev-nvme-test -n unbounded-system get cm unbounded-component-overrides -o json | python3 -B deploy/racer-loadgen/zipf/override_patch.py > tmp/racer-zipf-overrides.patch.json'
# Review the patch before this mutation. It must contain only racer-zipf.yaml.
timeout --signal=TERM --kill-after=10s 60s kubectl --context=joolshev-nvme-test -n unbounded-system patch cm unbounded-component-overrides --type=merge --patch-file=tmp/racer-zipf-overrides.patch.json
timeout --signal=TERM --kill-after=10s 60s kubectl --context=joolshev-nvme-test apply -f deploy/racer-loadgen/direct/volume.yaml
# Wait for manager reconciliation, then check both Racer workloads and raw mode.
timeout --signal=TERM --kill-after=10s 60s bash -o pipefail -c 'kubectl kustomize --load-restrictor LoadRestrictionsNone deploy/racer-loadgen/zipf | kubectl --context=joolshev-nvme-test apply -f -'
```

The last command starts at C16. For a paused first deployment, use
`deploy/racer-loadgen/zipf-paused` instead of `deploy/racer-loadgen/zipf` in the
render/apply command. This changes the control ConfigMap to `"0"`.
For an existing deployment, the following pauses reads without removing origins:

```sh
timeout --signal=TERM --kill-after=10s 60s kubectl --context=joolshev-nvme-test -n unbounded-system patch cm racer-loadgen-control --type=merge -p '{"data":{"concurrency":"0"}}'
```

Wait for projected ConfigMap updates, applied concurrency zero, and in-flight zero
on every loadgen. For a new deployment, start cautiously at `"8"` and validate
before raising concurrency. Reapplying the overlay selects C16 and may restart
loadgens; do not use it to control an active experiment or retain a pause. A readiness
probe proves origin readiness, not successful cache reads. Inspect acquisition
errors and received bytes; verify=false does not credit verified-byte metrics.
