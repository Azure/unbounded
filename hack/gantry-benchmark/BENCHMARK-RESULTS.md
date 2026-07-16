# Gantry P2P Cold-Pull Benchmark Results

Reference results for the Gantry capped-cascade P2P distribution, captured so
they can be reproduced and compared on a different cluster. Two workload shapes
are recorded: a **1 GiB single-layer** image (the pathological M=1 worst case,
no cross-layer pipelining) and an **8 GiB / 8-layer** image (representative of
real multi-layer images).

> Methodology note: the benchmark measures origin egress with a counting proxy
> (`acr-origin-proxy`) sitting in front of ACR, and installs a **strict**
> containerd `hosts.toml` for the Gantry-cold phase (mirror-only, no `server=`
> origin fall-through). This attributes origin bytes to Gantry's own pipeline
> only; containerd cannot bypass a slow/erroring Gantry to origin. Baseline
> pulls the same fresh image directly from the proxy. Every run builds a fresh,
> unique-digest image so the pull is genuinely cold.

## Environment (capture per cluster when comparing)

| Field | This capture |
| --- | --- |
| Cluster / context | `aks-gantry-d8-300-0715040655-f8459c` (AKS) |
| Worker nodes | 300 |
| Node SKU | D8-class (inferred from cluster name; confirm per cluster) |
| Observed per-stream P2P throughput | ~140-500 Mbps (derived from peer-fetch hit/stall durations) |
| Node image-fs | ~122 GiB, ~50 GiB free at capture time; DiskPressure=False |
| Registry | ACR `gantryd80715040655f8459c.azurecr.io` (behind counting proxy) |
| Gantry agent image | `gantry:fanout-20260715202829` (all cascade code) |
| Gantry code commits | `aabeca18` (60s timeout + strict hosts.toml), `1980c50e` (provider shuffle), `38e0ad7d` (prefetch replicas, inert on this path) |

## Gantry config (the winning capped-cascade knobs)

These are the runtime `gantry-config` values used for the passing runs. They are
the load-bearing settings to replicate on another cluster:

| Knob | Value | Purpose |
| --- | --- | --- |
| `peer_fetch_timeout` | `60s` | Bail off a slow/lockstep seed and re-select (short, not 1h). |
| `transfer_max_concurrent_serves` | `100` | Serve cap: shed excess blob GETs with instant `429` (the "leaders make leaders" cascade). |
| `peer_rediscover_budget` | `5m` | Re-discovery loop: keep re-running FindProviders + retry until served, within budget. |
| `peer_rediscover_backoff` | `1s` | Pause between re-discovery rounds. |
| provider shuffle | (always on, code) | Each requester tries providers in random order to spread across leaders + finishers. |
| strict `hosts.toml` (Gantry-cold) | mirror-only, no `server=` | Shed/exhausted requests retry Gantry, never fall to origin. |
| `prefetch_puller_replicas` | `8` | Inert in this scenario (prefetch does not fire on cold live-stream pulls); layer is seeded by reactive cold-start. |

Regression gates: `BENCHMARK_MINIMUM_BYTE_REDUCTION=0.90`, `BENCHMARK_MAXIMUM_LATENCY_RATIO=1.0`.

## Summary

| Run | Image | Layers | Nodes | Origin reduction | Gantry P50 | Gantry P95 | Baseline P95 | Result |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 1 GiB | 1 GiB | 1 | 300 | **99.67%** | 50.3 s | **98.3 s** | 273.0 s | **PASS** |
| 8 GiB | 8 GiB | 8 | 300 | **97.58%** | 206.3 s | **227.3 s** | 2442.0 s | **PASS** |

---

## Run 1 - 1 GiB single layer (M=1 worst case)

- Run ID: `run-20260715-212148-bc0c627e`
- Image: 1 GiB, 1 payload layer + alpine base
- Result: **PASS** (both gates green)

### Latency / origin (comparison.md)

| Metric | Baseline | Gantry cold | Reduction |
| --- | ---: | ---: | ---: |
| ACR upstream bytes | 322,151,019,900 (~300 GiB) | 1,073,836,734 (~1.0 GiB) | **99.67%** |
| Pod start P50 | 161.04 s | 50.35 s | 68.74% |
| Pod start P95 | 273.04 s | 98.35 s | 63.98% |
| Proxy requests | 1200 | 302 | 74.83% |

### Origin attribution (proxy by_client_class / by_path_class)

| | requests | bytes |
| --- | ---: | ---: |
| `containerd` (direct-to-origin bypass) | **0** | 0 |
| `gantry` (agent origin client) | 302 | 1,073,836,734 |
| by_path `blob` (layer) | **2** | ~1.0 GiB |
| by_path `manifest_by_digest` | 300 | 1004 B |

Zero containerd bypass; the 1 GiB layer was pulled from origin **2 times** (2 initial seeds via reactive cold-start), everything else P2P.

### P2P cascade counters (Prometheus, cluster sums)

| Counter | Value | Meaning |
| --- | ---: | --- |
| `p2p_origin_pull_total{kind="layer"}` | 2 | Layer origin seeds |
| `p2p_origin_pull_total{kind="manifest"}` | 1 | Manifest origin seed |
| `p2p_peer_fetch_total{outcome="hit"}` | 699 | Successful peer fetches |
| `p2p_peer_fetch_total{outcome="busy"}` | **3190** | Instant `429` shed -> re-select (serve cap working) |
| `p2p_peer_fetch_total{outcome="stall"}` | **100** | 60s stalls (down 8x from 824 with cap off) |
| `p2p_peer_fetch_total{outcome="notfound"}` | 402 | Provider raced/not-yet-committed |
| `p2p_peer_serve_total` (sum) | 799 | Total peer serves |
| nodes serving >=1 peer (fan-out) | **218 / 300** | Cascade fan-out width |

### Completion spread (pod finishedAt by minute)

```
21:33  181
21:34  118
21:35    1
```

Most pods completed within ~2 minutes.

### Contrast: same image, cascade knobs OFF

For reference, the identical 1 GiB image with `transfer_max_concurrent_serves=0`
and `peer_rediscover_budget=0` (cap/re-discovery disabled): Gantry P95 **227 s**,
`stall=824`, `busy=0`. Flipping the two knobs on cut P95 227 s -> 98 s and stalls
824 -> 100.

---

## Run 2 - 8 GiB / 8 layers (representative multi-layer)

- Run ID: `run-20260715-214517-1b15c422`
- Image: 8 GiB total, 8 x ~1 GiB payload layers + alpine base (`BENCHMARK_IMAGE_LAYERS=8`)
- Gantry phase window: `22:44:25Z` -> `22:49:19Z` (4 m 54 s wall)
- Result: **PASS** (both gates green)

### Latency / origin (comparison.md)

| Metric | Baseline | Gantry cold | Reduction |
| --- | ---: | ---: | ---: |
| ACR upstream bytes | 2,577,204,748,626 (~2.34 TiB) | 62,282,433,647 (~58 GiB) | **97.58%** |
| Pod start P50 | 2405.02 s | 206.32 s | 91.42% |
| Pod start P95 | 2442.02 s | 227.32 s | 90.69% |
| Pod start P100 | 2461.02 s | 236.32 s | 90.40% |
| Proxy requests | 4482 | 367 | 91.81% |

### Origin attribution (proxy by_client_class / by_path_class)

| | requests | bytes |
| --- | ---: | ---: |
| `containerd` (direct-to-origin bypass) | **0** | 0 |
| `gantry` (agent origin client) | 367 | 62,282,433,647 |
| by_path `blob` (layers) | **66** | ~58 GiB |
| by_path `manifest_by_digest` | 301 | 4262 B |

Zero containerd bypass. The 8 layers were seeded from origin **60 times total** (~7.5 seeds/layer via reactive cold-start); everything else P2P.

### P2P cascade counters (Prometheus, run-scoped delta over the phase window)

| Counter | Value | Meaning |
| --- | ---: | --- |
| `p2p_origin_pull_total{kind="layer"}` | 60 | Layer origin seeds (~7.5 per layer x 8 layers) |
| `p2p_origin_pull_total{kind="config"}` | 6 | Config-blob origin seeds |
| `p2p_origin_pull_total{kind="manifest"}` | 2 | Manifest origin seeds |
| `p2p_peer_fetch_total{outcome="hit"}` | 2851 | Successful peer fetches |
| `p2p_peer_fetch_total{outcome="busy"}` | 2542 | Instant `429` shed -> re-select (serve cap working) |
| `p2p_peer_fetch_total{outcome="stall"}` | 864 | 60s stalls (more than 1 GiB run: 8x whole-blob layers) |
| `p2p_peer_fetch_total{outcome="notfound"}` | 573 | Provider raced/not-yet-committed |
| `p2p_peer_serve_total` (sum) | 3715 | Total peer serves |
| nodes serving >=1 peer (fan-out) | **252 / 300** | Cascade fan-out width |

### Completion spread (pod finishedAt by UTC minute)

```
22:47  204
22:48   96
```

All 300 pods completed inside a ~2-minute window; P50->P100 spread is only 30 s (206 s -> 236 s), a very tight tail.

### Multi-layer scaling vs the 1 GiB M=1 case

8x the bytes (8 GiB vs 1 GiB) completed in only ~2.3x the P95 wall time (227 s vs 98 s). Effective per-node throughput rose from ~10 MiB/s (~87 Mbps) at 1 GiB to ~36 MiB/s (~300 Mbps) at 8 GiB, because the 8 layers form 8 independent per-digest cascades that pipeline across each other and hide the whole-blob per-layer log(N) penalty. This confirms chunking is not required for many-layer images: multi-layer images scale better than the single-layer worst case.

---

## How to reproduce on another cluster

1. Set `hack/gantry-benchmark/env.local` for the target cluster (context, ACR,
   `BENCHMARK_IMAGE_SIZE_MIB`, `BENCHMARK_IMAGE_LAYERS`).
2. Deploy the Gantry agent with the cascade knobs above in `gantry-config`.
3. `make -C hack/gantry-benchmark enable && ... preflight && ... run`.
4. Compare against the tables above. Record the target cluster's node SKU and
   observed per-stream P2P throughput, since absolute latency scales with
   per-node network bandwidth (origin reduction should stay ~99% regardless).
