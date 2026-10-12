Gantry reduces repeated image downloads from an origin registry by fetching
content on a bounded set of nodes and distributing the verified bytes between
cluster peers.

In a 1,000-node AKS benchmark, every node started one pod using the same cold
40 GiB, 40-layer image. Comparing the mean of two direct-origin reference runs
with the mean of three Gantry chair-design runs:

- ACR traffic decreased by **99.2%**, from 47.431 TB to 377.833 GB.
- Pod-start P50 decreased by **19.3%**, from 788.661 seconds to 636.100 seconds.
- Pod-start P95 decreased by **24.5%**, from 943.467 seconds to 712.667 seconds.
- Pod-start P100 decreased by **47.7%**, from 1,537.583 seconds to 803.933 seconds.
- All 1,000 pods started successfully in every Gantry chair-design run.

The direct-origin and chair-design measurements are separate run sets from the
same benchmark family. They use the same cluster scale, rollout shape, image
size, and latency definition, but they are not a paired A/B run collected in
one benchmark execution. Treat the comparison as representative measured
performance, not as a guarantee for every registry, image, or cluster.

## Results

Lower latency is better.

| Metric | Direct origin, two-run mean | Gantry chair design, three-run mean | Change |
| --- | ---: | ---: | ---: |
| ACR Private Endpoint traffic | 47.431 TB | 377.833 GB | 99.203% lower |
| Pod-start P50 | 788.661s | 636.100s | 19.344% faster |
| Pod-start P95 | 943.467s | 712.667s | 24.463% faster |
| Pod-start P100 | 1,537.583s | 803.933s | 47.714% faster |

The latency values cover the complete pod-start path: image download,
containerd verification and unpacking, and container creation. They are not
network-transfer-only measurements.

### Source registry and peer traffic

The three Gantry runs produced the following mean traffic:

| Measurement | Mean | Range |
| --- | ---: | ---: |
| ACR Private Endpoint traffic | 377.833 GB | 375.0-382.2 GB |
| Gantry-accounted origin traffic | 346.467 GB | 343.6-352.2 GB |
| Peer traffic served inside the cluster | 42.633 TB | 42.6-42.7 TB |

Compared with the 47.431 TB direct-origin reference mean,
Gantry-accounted origin traffic decreased by 99.270%.

The peer traffic is intentional. Gantry moves the image payload between nodes
instead of repeatedly transferring it from the origin registry. The result is
a reduction in origin-registry and Private Endpoint traffic, not a reduction
in the total bytes required to place and unpack the image on every node.

ACR wire traffic was approximately 9% higher than the payload bytes accounted
for by Gantry metrics. The comparison therefore uses ACR Private Endpoint
traffic as the customer-visible source-registry measurement.

### Run-to-run consistency

#### Direct-origin reference runs

| Run | ACR traffic | P50 | P95 | P100 |
| --- | ---: | ---: | ---: | ---: |
| 1,000 nodes, sample 1 | 47.296 TB | 682.725s | 821.209s | 1,422.461s |
| 1,000 nodes, sample 2 | 47.566 TB | 894.597s | 1,065.724s | 1,652.704s |

#### Gantry chair-design runs

| Run | Completed pods | ACR traffic | Gantry origin traffic | Peer traffic | P50 | P95 | P100 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| `chair-design-112507` | 1,000/1,000 | 382.2 GB | 352.2 GB | 42.7 TB | 633.0s | 714.8s | 851.9s |
| `chair-design-040127` | 1,000/1,000 | 376.3 GB | 343.6 GB | 42.6 TB | 635.0s | 711.2s | 796.2s |
| `chair-design-030517` | 1,000/1,000 | 375.0 GB | 343.6 GB | 42.6 TB | 640.3s | 712.0s | 763.7s |

Across the three Gantry runs, P95 stayed within a 3.6-second band while every
run completed all 1,000 pods.

## Benchmark method

| Item | Configuration or source |
| --- | --- |
| Cluster | 1,000-node AKS cluster |
| Workload | One pod per node starting concurrently |
| Image | Cold 40 GiB image with 40 layers |
| Direct-origin path | Each node pulls from a private Azure Container Registry |
| Gantry path | A bounded set of nodes pulls from ACR and serves content to peers |
| ACR traffic | `Microsoft.Network/privateEndpoints/PEBytesIn` over an isolated phase window |
| Pod-start latency | AKS audit timestamp from pod creation to first container-started status |
| Peer traffic | Per-pod `gantry_peer_serve_bytes_total{kind}` counter deltas |
| Gantry origin traffic | Gantry origin-byte counter deltas |

Before each cold phase, the benchmark removed prior workload images and
verified that matching containerd image records were absent. Collection waited
for one completed pod on every target node, zero Gantry in-flight work, and
stable counters. Missing or physically implausible telemetry invalidated a run
instead of being treated as zero.

## How to interpret the results

- The benchmark intentionally stresses origin distribution with a very large
  image and a simultaneous fleet-wide rollout.
- Smaller images, staggered deployments, warm node caches, different VM
  sizes, and different registry locations can produce different results.
- Image unpacking is a material part of pod startup. Faster delivery does not
  remove the per-node CPU and disk work required to unpack 40 layers.
- ACR traffic and Gantry traffic come from different measurement systems.
  ACR reports Private Endpoint wire bytes, while Gantry reports payload bytes.
- The measured reduction applies to origin traffic. Peer distribution still
  carries approximately one image payload to every node.

## Source data

The full engineering datasets, additional scales, cross-region runs, and
latency decomposition remain available in the repository:

- [Baseline and paired benchmark results](https://github.com/Azure/unbounded/blob/main/hack/gantry-benchmark/RESULTS.md)
- [Gantry chair-design and fleet results](https://github.com/Azure/unbounded/blob/main/hack/gantry-benchmark/GANTRY-ONLY-RESULTS.md)
- [Pod startup latency analysis](https://github.com/Azure/unbounded/blob/main/hack/gantry-benchmark/PULL-LATENCY-ANALYSIS.md)
