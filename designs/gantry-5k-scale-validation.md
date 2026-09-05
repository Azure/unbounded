# Gantry 5K-Node Startup Validation

**Status:** Validation complete
**Date:** 2026-08-25
**Scope:** Gantry startup and DaemonSet stability on one 5,003-node AKS cluster

---

## 1. Executive summary

Gantry v0.2.4 was deployed as one pod per node on an AKS cluster containing
5,000 Flex Nodes and three system nodes. The initial rollout was unstable:
4,888 Gantry pods restarted and only 47 were Ready. Every observed failed
container exited with code 2 after its Kubernetes membership informer failed to
synchronize within the default 30-second deadline.

Two startup settings were required to stabilize the rollout:

```yaml
env:
  - name: GANTRY_MEMBERS_SYNC_TIMEOUT
    value: "30m"

startupProbe:
  httpGet:
    path: /livez
    port: metrics
  periodSeconds: 10
  timeoutSeconds: 1
  failureThreshold: 190
  successThreshold: 1
```

The final observed state was:

| Measurement | Value |
| --- | ---: |
| Desired Gantry pods | 5,003 |
| Current Gantry pods | 5,003 |
| Up-to-date Gantry pods | 5,003 |
| Ready / available Gantry pods | 5,002 |
| Pending Gantry pods | 1 |
| Pods with restarts on the final revision | 0 |
| Total restarts on the final revision | 0 |

This validates stable Gantry startup at 5K nodes with the adjusted startup
budgets. It does not validate image-distribution performance because the test
configuration still used the placeholder `registry.example.com` upstream.

---

## 2. Test environment

| Item | Value |
| --- | --- |
| Kubernetes version | 1.36.2 |
| Cluster nodes | 5,003: 5,000 Flex Nodes and 3 system nodes |
| Flex VM size | `Standard_D4_v4` |
| Gantry version | v0.2.4 |
| Gantry image | `ghcr.io/azure/gantry:v0.2.4` |
| Gantry topology | One DaemonSet pod per node |
| Gantry namespace | `unbounded-system` |
| Rollout strategy | `RollingUpdate`, `maxUnavailable: 10%`, `maxSurge: 0` |

Gantry was enabled by setting
`Site/flex-site.spec.components.gantry.enabled=true`. The operator then created
the cluster-wide Gantry DaemonSet.

---

## 3. Baseline behavior

The first rollout used the shipped defaults:

- membership informer synchronization deadline: 30 seconds;
- readiness probe: `/readyz`, beginning after 5 seconds;
- liveness probe: `/livez`, beginning after 30 seconds, every 30 seconds, with
  failure threshold 3.

The baseline observation was:

| Measurement | Value |
| --- | ---: |
| Total pods | 5,003 |
| Running pods | 5,002 |
| Ready pods | 47 |
| Pods with restarts | 4,888 |
| Total restarts at observation time | 8,579 |
| Failed-container exit code | 2 for all 4,888 observed failures |

Representative previous-container output ended with:

```text
members: members initial sync (timeout=30s):
members: wait for sync: context deadline exceeded
```

The container successfully initialized libp2p, connected to containerd, and
started the peer transfer endpoint before failing membership synchronization.

---

## 4. Root cause

Every Gantry pod creates:

1. a namespace-scoped, label-filtered Gantry Pod informer; and
2. a cluster-scoped, unfiltered Node informer.

The implementation is in `internal/gantry/members/members.go:139-163`. Startup
waits for both informer caches under a bounded context in
`cmd/gantry/main.go:1171-1191`. The default deadline is 30 seconds in
`cmd/gantry/main.go:1201-1212`.

A direct `kubectl get nodes -o name` measurement from a Flex VM took:

| Measurement | Value |
| --- | ---: |
| Wall time | 26.66 seconds |
| Client maximum RSS | 476,924 KiB |

These are direct measurements of one client-side list of 5,003 Nodes. They do
not establish server-side CPU or memory consumption.

The initial rollout started thousands of independent informer clients at once.
The single-list measurement left approximately 3.34 seconds of margin under the
default deadline:

$$
30.00\text{ s} - 26.66\text{ s} = 3.34\text{ s}
$$

The evidence establishes that the failed pods exceeded the 30-second deadline
during the concurrent rollout. It does not isolate how much of the added delay
came from the API server, network, serialization, client scheduling, or local
resource contention.

### Resync semantics correction

The 30-second informer resync period is not a 30-second API relist. Client-go
resync delivers cached objects to registered handlers without interacting with
authoritative storage. Gantry's membership manager registers no event handlers;
it reads the informer stores directly. The observed failure was the initial
LIST/WATCH cache synchronization, not periodic relisting.

---

## 5. Mitigation sequence

### 5.1 Increase member sync timeout to 10 minutes

The first mitigation set:

```yaml
env:
  - name: GANTRY_MEMBERS_SYNC_TIMEOUT
    value: "10m"
```

This removed the 30-second process deadline, but pods still restarted. The
existing liveness probe killed containers after roughly 90 seconds while they
were still synchronizing. Their logs ended with:

```text
members initial sync (timeout=10m0s): members: wait for sync: context canceled
```

The `context canceled` result distinguished kubelet termination from the
process's own 10-minute deadline.

### 5.2 Add startup probe protection

A startup probe was added so liveness would not run during the long initial
sync:

```yaml
startupProbe:
  httpGet:
    path: /livez
    port: metrics
  periodSeconds: 10
  timeoutSeconds: 1
  failureThreshold: 70
  successThreshold: 1
```

This allowed up to 700 seconds for startup. Pods on this revision remained alive
with zero premature restarts, and Ready pods began increasing.

### 5.3 Increase both budgets to 30 minutes

At approximately 10 minutes, 19 pods began reaching the process's own
10-minute synchronization deadline. The final settings were therefore changed
to:

```yaml
env:
  - name: GANTRY_MEMBERS_SYNC_TIMEOUT
    value: "30m"

startupProbe:
  httpGet:
    path: /livez
    port: metrics
  periodSeconds: 10
  timeoutSeconds: 1
  failureThreshold: 190
  successThreshold: 1
```

The startup-probe budget is derived as:

$$
190 \times 10\text{ s} = 1{,}900\text{ s} = 31\text{ min }40\text{ s}
$$

This leaves 100 seconds beyond the process's 30-minute member-sync deadline for
the process to report a synchronization failure before kubelet intervenes.

---

## 6. Rollout behavior

The rollout appeared to stop at intermediate `UP-TO-DATE` values such as 3,949
and 801. These were not fixed node-count limits.

The DaemonSet uses `maxUnavailable: 10%`. At 5,003 desired pods, the nominal
unavailability budget is approximately 500 pods. When many new pods were alive
but not Ready, the DaemonSet controller retained old Ready pods instead of
replacing them. As new pods completed membership synchronization and became
Ready, the budget was released and replacement continued.

One observed revision split was:

| Revision | Pods | Ready | Not Ready |
| --- | ---: | ---: | ---: |
| Old revision A | 2,785 | 2,785 | 0 |
| Old revision B | 1,054 | 1,054 | 0 |
| New revision | 1,164 | 242 | 922 |

The new revision had zero restarts at that observation. Readiness continued to
increase, after which `UP-TO-DATE` advanced again.

---

## 7. Successful startup evidence

A sampled pod on the protected revision logged:

```text
members informer ready node_name=flex-scale-4341 peers=3840
mirror endpoint listening addr=0.0.0.0:5000
ops endpoint listening addr=0.0.0.0:9095
members: bootstrap converged; ceasing periodic dials
routing_table=8 target=5
```

For that sample, the interval from `gantry starting` to
`members informer ready` was approximately 33 seconds. Other pods took longer,
which is why the rollout required a substantially larger tail budget than the
sample.

The final DaemonSet had all 5,003 pods on the latest revision, 5,002 Ready, one
Pending, and zero restarts on the final revision.

---

## 8. Side effects and limitations

### 8.1 Slower failure detection

Real membership failures can now take up to 30 minutes to surface. The startup
probe suppresses normal liveness processing for up to 31 minutes 40 seconds.

### 8.2 Longer concurrent startup load

The changes prevent restart storms but do not reduce initial LIST/WATCH load.
Slow clients remain alive and retain their in-progress work instead of failing
and immediately starting another list.

### 8.3 Resource reservations

The shipped Gantry container requests 100 millicores and 1 GiB per pod in
`deploy/gantry/daemonset.yaml.tmpl:224-230`. At 5,003 pods, the derived scheduler
reservation is:

$$
5{,}003 \times 0.1\text{ core} = 500.3\text{ cores}
$$

$$
5{,}003 \times 1\text{ GiB} = 5{,}003\text{ GiB}
$$

These are scheduler requests, not measured usage.

### 8.4 Configuration persistence

Unbounded operator v0.2.4 did not consume the workload-override mechanism in the
current repository. The effective environment variable and startup probe were
therefore applied directly to the DaemonSet. An operator reconciliation or
upgrade may overwrite those fields. The intended settings were also recorded in
the `unbounded-component-overrides` ConfigMap for forward compatibility, but
that ConfigMap was not the effective source for v0.2.4.

### 8.5 Functional scope

This exercise validated:

- DaemonSet scheduling at 5K scale;
- stable Gantry process startup;
- Kubernetes membership synchronization;
- mirror and metrics endpoint startup; and
- initial libp2p/DHT bootstrap on sampled pods.

It did not validate:

- origin-pull reduction;
- peer transfer throughput;
- image rollout convergence;
- registry authentication; or
- steady-state CPU and memory usage.

The deployed configuration still referenced `registry.example.com`, so no
customer registry traffic was tested.

---

## 9. Implications for a 30K-node cluster

The 5K result does not establish 30K readiness. Increasing timeouts tolerates the
current startup architecture; it does not reduce its work.

Before a 30K qualification, the following require implementation or measurement:

1. **Remove the per-pod full Node informer.** Zone information is the reason the
   Node informer exists. Publishing the node's zone through the Gantry Pod or
   another bounded membership path would remove one full cluster-wide object set
   from every agent.
2. **Bound peer membership state.** Every Gantry pod currently maintains the
   Gantry Pod informer view. A 30K design should evaluate sharded or DHT-native
   membership so each agent does not need every peer as a Kubernetes object.
3. **Stage Gantry rollout.** Avoid starting all agents simultaneously. Validate
   a bounded rollout rate or staged node cohorts and measure API request volume,
   cache-sync latency, and rollout completion time.
4. **Make startup budgets first-class configuration.** The operator should carry
   member-sync timeout and startup-probe settings into the rendered DaemonSet.
   Static scale tiers may be used initially, but should be based on measured
   cache-sync latency rather than node count alone.
5. **Add startup telemetry.** Record initial Pod-list duration, Node-list
   duration, object counts, payload sizes, cache-sync duration, process RSS, and
   API throttling. Current logs expose only the combined timeout outcome.
6. **Run an actual image-distribution workload.** Configure the target registry,
   deploy a controlled image to every node, and measure origin bytes, peer bytes,
   convergence, failures, and steady-state resource use.

The existing design already calls for re-validating the per-node resource
budget at 10K scale (`designs/gantry-detailed-design.md:752-753`). The 5K
startup findings make that validation a prerequisite for 30K rather than a
follow-up optimization.

---

## 10. Reproduction checks

Use the following checks during future runs:

```bash
kubectl -n unbounded-system get daemonset gantry

kubectl -n unbounded-system get pods \
  -l app.kubernetes.io/name=gantry \
  -o json

kubectl -n unbounded-system logs <pod> -c gantry --previous

kubectl -n unbounded-system get controllerrevision \
  -l app.kubernetes.io/name=gantry
```

The minimum acceptance criteria are:

- desired, current, updated, Ready, and available counts match the target node
  count, excluding any separately explained unschedulable node;
- no pods on the final revision have restarted;
- sampled logs contain `members informer ready`;
- sampled pods start the mirror and ops endpoints; and
- sampled pods report a non-empty DHT routing table.