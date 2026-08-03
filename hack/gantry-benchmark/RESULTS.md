# Gantry Benchmark

Gantry delivers a 99% reduction in ACR traffic and improves pod startup
latency across a 2000-node AKS cluster delivering a 40 GiB image to
every node.

We performed tests to check consistency. For each sample, we generated a new 
random 40 GiB image with 40 layers and
pushed equivalent, digest-pinned variants to separate private Azure Container
Registries. The baseline phase ran a Kubernetes Job with exactly one pod on
each node, pulling the image directly from the baseline registry. The Gantry
phase repeated the same one-pod-per-node workload, but containerd requested the
image from the Gantry agent running on each node; a small set of designated
pullers fetched layers from the Gantry registry and distributed them to peers.
We removed prior benchmark images before repeat samples, measured registry
traffic at each Azure Private Endpoint, measured pod startup from AKS audit
logs, and measured peer traffic from Gantry metrics.


### ACR traffic

| Cluster / sample | Image size | Baseline ACR | Gantry ACR | Byte reduction | Successful pulls B/G | Pull reduction |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| 1000 nodes - sample 1 | 40 GiB | 47.296 TB | 174.773 GB | 99.630% | 1002 / 4 | 99.601% |
| 1000 nodes - sample 2 | 40 GiB | 47.566 TB | 174.781 GB | 99.633% | 1008 / 5 | 99.504% |
| **1000 nodes - cross-region** | **40 GiB** | **53.369 TB** | **219.262 GB** | **99.589%** | **1254 / 5** | **99.601%** |
| 2000 nodes - sample 1 | 40 GiB | 95.285 TB | 243.589 GB | 99.744% | 2060 / 3 | 99.854% |
| 2000 nodes - sample 2 | 40 GiB | 96.045 TB | 242.070 GB | 99.748% | 2035 / 5 | 99.754% |
| 2000 nodes - sample 3 | 40 GiB | 97.110 TB | 247.528 GB | 99.745% | 2057 / 3 | 99.854% |


### Pod Startup latency

| Cluster / sample | Image size | Baseline P50 | Gantry P50 | Baseline P95 | Gantry P95 | Baseline P100 | Gantry P100 | P95 change |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 1000 nodes - sample 1 | 40 GiB | 682.725s | 1099.629s | 821.209s | 1171.863s | 1422.461s | 1885.385s | 42.700% slower |
| 1000 nodes - sample 2 | 40 GiB | 894.597s | 1087.179s | 1065.724s | 1169.448s | 1652.704s | 1885.011s | 9.733% slower |
| **1000 nodes - cross-region** | **40 GiB** | **3561.000s** | **1064.557s** | **3953.000s** | **1146.557s** | **5399.000s** | **1815.557s** | **70.995% faster** |
| 2000 nodes - sample 1 | 40 GiB | 1180.178s | 1088.163s | 1407.710s | 1172.734s | 2103.281s | 1883.781s | 16.692% faster |
| 2000 nodes - sample 2 | 40 GiB | 939.834s | 1093.091s | 1141.022s | 1177.821s | 1724.219s | 1856.380s | 3.225% slower |
| 2000 nodes - sample 3 | 40 GiB | 1241.331s | 1096.589s | 1472.041s | 1184.248s | 2131.053s | 1821.000s | 19.551% faster |

Positive improvement means Gantry started pods faster. The configured gate was
a maximum Gantry-to-baseline P95 ratio of 3.0, so all five audit-complete runs
and the cross-region performance sample passed even when an unusually fast
baseline made Gantry slower. The cross-region row uses retained Kubernetes pod
status timestamps; the other rows use AKS audit timestamps.

#### Latency excluding image-pull backoff

AKS audit logs retained the pod status patches containing `ErrImagePull` and
`ImagePullBackOff`. We matched those exact pod names to the per-pod audit
latencies in each result, excluded any pod that reported either reason, and
recomputed the same nearest-rank P50, P95, and P100 used by the benchmark.

| Cluster / sample | Excluded pods B/G | Filtered P50 B/G | Filtered P95 B/G | Filtered P100 B/G | Filtered P95 change |
| --- | ---: | ---: | ---: | ---: | ---: |
| 1000 nodes - sample 1 | 2 / 19 | 682.283s / 1098.776s | 820.422s / 1164.024s | 913.445s / 1334.578s | 41.881% slower |
| 1000 nodes - sample 2 | 8 / 16 | 893.362s / 1086.727s | 1062.555s / 1157.776s | 1427.002s / 1358.702s | 8.961% slower |
| 2000 nodes - sample 1 | 60 / 30 | 1172.347s / 1086.737s | 1374.781s / 1167.368s | 1745.801s / 1481.877s | 15.087% faster |
| 2000 nodes - sample 2 | 35 / 29 | 937.999s / 1092.350s | 1122.102s / 1171.529s | 1449.360s / 1495.695s | 4.405% slower |
| 2000 nodes - sample 3 | 58 / 36 | 1235.343s / 1095.862s | 1444.267s / 1173.629s | 1581.197s / 1505.150s | 18.739% faster |

Filtering changed P50 and P95 modestly but reduced P100 substantially. This
confirms that image-pull retries primarily drove the longest startup tails.
The unfiltered table remains the primary end-to-end result because
`ImagePullBackOff` is part of the workload experience seen by users.

### Gantry internals

| Cluster / sample | Image size | Peer bytes | Internal origin bytes | Origin pulls | Origin layer pulls | Peer hits | Fallbacks |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 1000 nodes - sample 1 | 40 GiB | 43.566 TB | 158.928 GB | 157 | 153 | 42,597 | 0 |
| 1000 nodes - sample 2 | 40 GiB | 43.438 TB | 160.002 GB | 159 | 154 | 42,472 | 0 |
| 2000 nodes - sample 1 | 40 GiB | 86.971 TB | 221.210 GB | 213 | 210 | 85,044 | 0 |
| 2000 nodes - sample 2 | 40 GiB | 86.926 TB | 223.358 GB | 214 | 209 | 85,001 | 0 |
| 2000 nodes - sample 3 | 40 GiB | 87.227 TB | 226.579 GB | 218 | 215 | 85,294 | 0 |

## Cross-region 1000-node sample

We ran a separate 1000-node experiment with AKS and the benchmark operator in
Canada Central and both private ACRs in UK South. This run used the same fresh
40 GiB, 40-layer payload in both phases. Both Jobs completed on all 1000 nodes
with no failed pods.

The ACR event and Private Endpoint measurements below are authoritative. AKS
did not export `AKSAuditAdmin` records for this run, so latency uses the
retained Kubernetes pod status timestamps instead of audit timestamps. This
sample is therefore not included in the same-region aggregates or the
audit-derived backoff analysis above.

| Metric | Baseline | Gantry cold | Reduction |
| --- | ---: | ---: | ---: |
| ACR Private Endpoint bytes | 53.369 TB | 219.262 GB | 99.589% |
| Successful ACR pull events | 1,254 | 5 | 99.601% |
| Pod start P50 | 3561.000s | 1064.557s | 70.105% |
| Pod start P95 | 3953.000s | 1146.557s | 70.995% |
| Pod start P100 | 5399.000s | 1815.557s | 66.372% |

| Gantry measurement | Value |
| --- | ---: |
| Peer bytes served | 43.619 TB |
| Internal origin bytes | 193.575 GB |
| Origin pulls | 190 |
| Origin layer pulls | 184 |
| Peer fetch hits | 42,651 |
| Direct-origin fallbacks | 0 |

The performance-only comparison passed every non-audit gate. Private Endpoint
bytes fell by 99.589%, P95 improved by 70.995%, the baseline recorded no
Gantry activity, and the Gantry phase recorded no direct-origin fallback.



## Aggregate results by scale

| Metric | 1000 nodes, 2 runs | 2000 nodes, 3 runs |
| --- | ---: | ---: |
| Average baseline ACR bytes | 47.431 TB | 96.147 TB |
| Average Gantry ACR bytes | 174.777 GB | 244.396 GB |
| Weighted ACR-byte reduction | 99.632% | 99.746% |
| Average successful pulls B/G | 1005.0 / 4.5 | 2050.7 / 3.7 |
| Weighted pull reduction | 99.552% | 99.821% |
| Average P50 B/G | 788.661s / 1093.404s | 1120.448s / 1092.614s |
| Average P95 B/G | 943.466s / 1170.656s | 1340.258s / 1178.267s |
| Average P95 change | 24.080% slower | 12.087% faster |
| Average peer bytes | 43.502 TB | 87.042 TB |
| Gantry P95 range | 1169.448-1171.863s | 1172.734-1184.248s |

Doubling the cluster produced the following ratios based on scale-level means:

- Baseline ACR bytes: 2.027x.
- Gantry ACR bytes: 1.398x.
- Peer bytes: 2.001x.
- Peer hits: 2.001x.
- Internal origin pulls: 1.361x.
- Baseline P95: 1.421x.
- Gantry P95: 1.007x.

This is the central scaling result. Peer distribution absorbed almost the
entire additional payload volume, while Gantry ACR traffic and P95 latency grew
far more slowly than node count.

## Methodology

### Final cluster and workload

| Item | Configuration |
| --- | --- |
| AKS node VM | `Standard_D8s_v3` |
| 1000-node shape | One 1000-node system pool |
| 2000-node shape | 1000-node system pool plus 1000-node `bench2` user pool |
| Node OS and runtime | Ubuntu 24.04.4 LTS, containerd 2.3.2-2 |
| Node OS disk | 512 GiB managed disk |
| Pod network | Azure CNI Overlay, `10.64.0.0/12` |
| Workload | 40 random 1 GiB layers, 40 GiB per node |
| Baseline registry | `vapagantrybaseline.azurecr.io` |
| Gantry registry | `vapagantryp2p.azurecr.io` |
| Registry access | Premium ACR through approved Private Endpoints, public access disabled |
| Puller selection | `ceil(nodes * 0.02)` |
| Peer serve cap | 10 concurrent serves per Gantry pod |
| Job timeout | 75 minutes at 1000 nodes; 120 minutes at 2000 nodes |

The operator was a managed-identity `Standard_D32ds_v5` VM with a 512 GiB
Premium SSD v2 build disk configured for 20,000 IOPS and 750 MB/s. Podman
storage, image build contexts, and payload files lived on that disk.

### Cache isolation and routing

Each final run generated a new random payload and payload SHA-256. Baseline and
Gantry images contained the same payload bytes but used phase-specific paths,
which produced distinct layer and image digests and prevented containerd from
reusing baseline blobs during the Gantry phase. Between repeated runs, a
readiness-gated DaemonSet removed only images from the two benchmark workload
repositories and verified zero matching image records remained on every node.

### Measurements

| Metric | Authoritative source |
| --- | --- |
| ACR image pulls | `ContainerRegistryRepositoryEvents`, exact repository and manifest digest, result 200 |
| ACR bytes | `Microsoft.Network/privateEndpoints/PEBytesIn`, isolated whole-minute phase window |
| Startup latency | `AKSAuditAdmin`, pod create to first started-status request |
| Peer bytes | Per-Gantry-pod `gantry_peer_serve_bytes_total{kind}` deltas |
| Internal origin activity | Gantry origin byte, pull, success, and fallback counter deltas |

The runner required one completed pod on every distinct target node. It waited
for Gantry in-flight work to reach zero and counters to stabilize before
collecting results. Missing or physically implausible Azure telemetry
invalidated a run rather than being treated as zero.

At 2000 nodes, broad kubelet, node-exporter, and kube-state-metrics
ServiceMonitors were excluded from Prometheus selection. The benchmark-owned
Gantry PodMonitor remained enabled. This reduced the Prometheus head from about
7.28 million series and 24 GiB resident memory to a small clean baseline before
the 2000-node runs.



## Conclusions

1. Gantry consistently reduced source ACR bytes by more than 99.63% at 1000
   nodes and more than 99.74% at 2000 nodes.
2. Gantry reduced successful ACR pull events to three to five per final run,
   versus roughly one event per baseline node plus retry overhead.
3. Peer bytes and peer hits scaled almost exactly with node count, while Gantry
   source bytes and origin pulls grew much more slowly.
4. Gantry P95 was stable across all final runs.
5. `ImagePullBackOff` retry and backoff cycles extended the longest pod-startup
   tails, especially P100, beyond normal image transfer and extraction time.
   Excluding exact audit-identified backoff pods changed P50 and P95 modestly
   but reduced P100 substantially. In 2000-node sample 3, excluding 58 baseline
   and 36 Gantry pods reduced P100 from 2131.053s to 1581.197s for baseline and
   from 1821.000s to 1505.150s for Gantry.
6. At 2000 nodes, Gantry improved average P95 by 12.087%; at 1000 nodes