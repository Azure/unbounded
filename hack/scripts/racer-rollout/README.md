# Plain Racer rollout on the scale-test cluster

Parent executes these phases from the repository root. Python 3, PyYAML, kubectl,
GNU timeout and the existing cluster credentials are required. No images are built.
Keep load at its existing C0 setting until the parent chooses to resume measurement.
The script never changes load settings, replica counts (except the operator),
host/pod-network placement, tuning, durable identity, slabs, or unrelated overrides.
All three existing loadgen workloads are pinned, including the separate namespace.

Use the same state directory throughout. It holds timestamped command checkpoints
and pre-change workload/ConfigMap backups (not Secrets). Every phase has a 285s
internal deadline; kubectl calls report before/after checkpoints and are <=50s.
On failure, inspect the reported command and live state. Resume only the unfinished
phase; do not replay completed deletes or restart an unchanged timeout loop.

Run each command separately and inspect its result:

```sh
timeout --signal=TERM --kill-after=10s 300s python3 -B hack/scripts/racer-rollout/rollout.py stop
timeout --signal=TERM --kill-after=10s 300s python3 -B hack/scripts/racer-rollout/rollout.py writers
timeout --signal=TERM --kill-after=10s 300s python3 -B hack/scripts/racer-rollout/rollout.py cleanup-start
timeout --signal=TERM --kill-after=10s 300s python3 -B hack/scripts/racer-rollout/rollout.py cleanup-wait
timeout --signal=TERM --kill-after=10s 300s python3 -B hack/scripts/racer-rollout/rollout.py configure
timeout --signal=TERM --kill-after=10s 300s python3 -B hack/scripts/racer-rollout/rollout.py rollout
timeout --signal=TERM --kill-after=10s 300s python3 -B hack/scripts/racer-rollout/rollout.py start
```

`cleanup-wait` is ordinary DaemonSet rollout status: every Linux node should have a
successful cleanup init container and a ready unprivileged idle container. Expected
fleet size is 1500; inspect actual desired/current/ready counts and init failures:

```sh
timeout --signal=TERM --kill-after=10s 45s kubectl -n unbounded-system get ds racer-firewall-cleanup
timeout --signal=TERM --kill-after=10s 45s kubectl -n unbounded-system get pods -l app=racer-firewall-cleanup -o wide
```

If scheduling/image pulls outlast the observation window, inspect those results
before another bounded observation. Do not continue past failed cleanup. The cleanup
manifest is also available without any cluster write through the `manifest` phase.
Its payload uses host binaries, 15s subprocess bounds and an overall 240s deadline.
It deletes only the two exact tagged INPUT rule classes, individually tagged rules
inside RACER_STAGE47, that empty chain, and the named R47 ipsets. It never flushes
tables or touches kube rules. Unexpected references/untagged chain rules stop cleanup.
The old writer Deployment and DaemonSet are foreground-deleted first.

Observe each target separately, using the same bounded command (replace the target):

```sh
timeout --signal=TERM --kill-after=10s 300s python3 -B hack/scripts/racer-rollout/rollout.py wait --target deployment/unbounded-operator
```

Other targets: `deployment/racer-controller`, `daemonset/racer-dataplane`,
`daemonset/racer-dataplane-podnet`, `daemonset/gantry`, `daemonset/racer-loadgen`,
`deployment/racer-loadgen-client`, and `daemonset/racer-loadgen --namespace racer-loadgen`.
These use normal RollingUpdate with 10% maxUnavailable and zero DaemonSet surge,
not manual pod deletion. After successful cleanup and rollout, remove the idle DS:

```sh
timeout --signal=TERM --kill-after=10s 300s python3 -B hack/scripts/racer-rollout/rollout.py cleanup-delete
```

## Configuration dependencies inspected

* `internal/operator/components/racer/racer.go:174-202` owns installation wiring,
  including RACER_DATAPLANE_IMAGE; `config.go:18-47` preserves administrator config.
  The operator can rewrite that informational default image value after restart;
  the authoritative named workload overrides pin the actual images by digest.
* `internal/operator/components/racer/migration.go:25-190` uses placement and live
  occupants, not the stage47 source ConfigMap. Live racer-config has host networking
  plus eleven pod-network exceptions. Neither is changed. No migration-guard source
  key was present in the inspected live config or this published commit's code.
  The sanitizer nevertheless removes Racer/Unbounded-Racer GUARD config/env keys,
  including migration source settings if introduced before execution.
* Live `racer-v2.yaml` contained the launcher, copy init and five retired volumes.
  Both overrides and live templates are sanitized. ResourceVersion-checked PUT
  removes exact fields even when SSA would retain another manager's fields.
  Unrelated ConfigMap keys and non-Racer/Gantry override documents are preserved.
* Historical `designs/racer-dynamic-guard.md` describes the retired deployment.
  The user's explicit scope change supersedes that workflow. This script adds no
  continuing firewall controller, startup dependency, or evidence protocol.

Focused local tests (no cluster writes):

```sh
timeout --signal=TERM --kill-after=10s 300s python3 -B -m unittest discover -s hack/scripts/racer-rollout -p 'test_*.py' -v
```
