"""Simulator that quantitatively backs the claims in p2p-protocol.md.

1. Routing through topology-aware fingers ("prefer the nearest peer at every
   hop", aka PNS) really does keep traffic local. Compared to a baseline that
   picks fingers uniformly at random, PNS produces fewer cross-rack and
   cross-row bytes per pull, and a lower oversubscription-weighted
   "congestion factor" (effective bytes carried per system hop). The
   improvements show up as statistically significant per-seed gains across
   independent runs, not just a single lucky seed.

2. The three small fixes called out in the design doc actually fix what they
   claim to fix:

   - eps-jitter on the level-1 hop causes every level-1 finger slot to warm
     up, which is reflected in the realized "copies per stripe" approaching
     the theoretical V*b + b^2 ceiling for hot keys.
   - nearest-tier sampling at h_2 (and h_1) breaks the deterministic
     "everyone in this rack picks the same peer" hotspot. The serve-skew
     metrics (serve_p99 / serve_mean and serve_max / serve_mean) drop
     substantially versus deterministic argmin PNS.
   - virtualizing the level-2 finger (V*b candidates instead of b) gives
     tier-sampling something to actually spread across in racks where there
     would otherwise be only one same-tier h_2 candidate, doubling
     level-2-layer cache fanout for free.

3. The cache-fanout story holds at scale: the protocol grows roughly
   S * (V*b + b^2) serving slots for a hot file with no central decision,
   and the owner becomes idle for hot reads (most pulls are 1 hop, almost
   always intra-rack).

The script is paired by seed: for each seed it runs the full simulation
twice, once with PNS routing on and once with uniform-random routing, using
byte-identical placement, demand, and jitter coins. Only the routing rule
differs, so the reported "PNS benefit" columns are an apples-to-apples
ratio. Bands are mean [p5, p95] across independent seeds; the [*] marker
flags benefit intervals that cross zero and are therefore not statistically
significant for the given seed count.

Defaults run three cluster sizes (1k / 4k / 16k nodes) with 30 seeds. For a
quick sanity check, pass --seeds 3 --sizes 1024; the full sweep at 16k nodes
takes a while.
"""

from __future__ import annotations

import argparse
import math
import statistics
from collections import OrderedDict
from dataclasses import dataclass
from pathlib import Path

import numpy as np


# ---------- topology ---------------------------------------------------------

TIER_SAME = 0
TIER_RACK = 1
TIER_ROW = 2
TIER_CROSS_ROW = 3


@dataclass(frozen=True)
class Topology:
    """3-tier tree (rows -> racks -> nodes) with contiguous integer IDs."""

    nodes_per_rack: int
    racks_per_row: int
    n_rows: int

    @property
    def nodes_per_row(self) -> int:
        return self.nodes_per_rack * self.racks_per_row

    @property
    def n_nodes(self) -> int:
        return self.nodes_per_row * self.n_rows

    def tier_between(self, a: np.ndarray, b: np.ndarray) -> np.ndarray:
        a = np.asarray(a)
        b = np.asarray(b)
        same = a == b
        same_rack = a // self.nodes_per_rack == b // self.nodes_per_rack
        same_row = a // self.nodes_per_row == b // self.nodes_per_row
        return np.where(
            same,
            TIER_SAME,
            np.where(same_rack, TIER_RACK, np.where(same_row, TIER_ROW, TIER_CROSS_ROW)),
        )

    def tier_pair(self, a: int, b: int) -> int:
        if a == b:
            return TIER_SAME
        if a // self.nodes_per_rack == b // self.nodes_per_rack:
            return TIER_RACK
        if a // self.nodes_per_row == b // self.nodes_per_row:
            return TIER_ROW
        return TIER_CROSS_ROW


def build_topology(target_n: int, nodes_per_rack: int, racks_per_row: int) -> Topology:
    nodes_per_row = nodes_per_rack * racks_per_row
    n_rows = max(1, math.ceil(target_n / nodes_per_row))
    return Topology(
        nodes_per_rack=nodes_per_rack,
        racks_per_row=racks_per_row,
        n_rows=n_rows,
    )


@dataclass(frozen=True)
class FabricRatios:
    """Oversubscription ratios applied as per-tier edge weights."""

    oversub_rack: float = 4.0  # ToR -> spine
    oversub_row: float = 2.0   # spine -> super-spine


def edge_weight(tier: int, fabric: FabricRatios) -> float:
    """Cost amplifier for a hop crossing the given tier."""
    if tier <= TIER_RACK:
        return 1.0
    if tier == TIER_ROW:
        return fabric.oversub_rack
    return fabric.oversub_rack * fabric.oversub_row


# ---------- LRU proxy cache --------------------------------------------------


class LRU:
    """Tiny LRU on integer keys. Pinned keys are tracked separately; this only
    holds the proxy-cache portion."""

    __slots__ = ("cap", "od")

    def __init__(self, cap: int):
        self.cap = cap
        self.od: OrderedDict[int, None] = OrderedDict()

    def __contains__(self, k: int) -> bool:
        return k in self.od

    def hit(self, k: int) -> None:
        if k in self.od:
            self.od.move_to_end(k)

    def insert(self, k: int) -> None:
        if k in self.od:
            self.od.move_to_end(k)
            return
        if self.cap <= 0:
            return
        self.od[k] = None
        if len(self.od) > self.cap:
            self.od.popitem(last=False)

    def keys(self):
        return self.od.keys()

    def __len__(self) -> int:
        return len(self.od)


# ---------- per-key placement ------------------------------------------------


@dataclass
class Placement:
    owner: int
    replicas: tuple[int, ...]
    pinned: frozenset[int]
    level1_per_h2: dict[int, np.ndarray]

    # Per-client routing decisions, precomputed both ways so the same Placement
    # can serve PNS-on and PNS-off runs without re-randomising.
    h2_pns: np.ndarray
    h2_rand: np.ndarray
    h1_det_pns: np.ndarray   # used when PNS is on and the jitter coin is 0
    h1_det_rand: np.ndarray  # used when PNS is off and the jitter coin is 0


def _pick_min_tier(
    rng: np.random.Generator,
    tier_mat: np.ndarray,
    candidates: np.ndarray,
    *,
    tier_sample: bool,
) -> np.ndarray:
    """Per-row pick from `candidates` using the tier matrix.

    tier_sample=False reproduces argmin (deterministic nearest, ties broken
    by first-occurrence). tier_sample=True samples uniformly among the rows'
    minimum-tier candidates: any candidate sharing the row's smallest tier
    has equal probability of being selected. This is fix (1): nearest-tier
    sampling. With ties broken stochastically by `rng`, both PNS-on and
    PNS-off runs see deterministic but different picks because rng is part
    of the placement RNG; placement is identical across the PNS A/B because
    rng was seeded per key.
    """
    if not tier_sample:
        return candidates[tier_mat.argmin(axis=1)]
    min_tier = tier_mat.min(axis=1, keepdims=True)
    is_min = tier_mat == min_tier
    # Random tie-break: weight = uniform * is_min, argmax picks one of the mins.
    weights = is_min.astype(np.float64) * rng.random(tier_mat.shape)
    return candidates[weights.argmax(axis=1)]


def sample_placement(
    *,
    rng: np.random.Generator,
    topology: Topology,
    n_replicas: int,
    b: int,
    replica_placement: str,
    h2_virtual: int = 1,
    tier_sample_h2: bool = True,
    tier_sample_h1: bool = True,
) -> Placement:
    """Sample per-key state.

    `h2_virtual` (fix (3)): the level-2 finger set has `h2_virtual * b`
    members instead of `b`. Higher V gives more candidates per topological
    cell, which makes PNS argmin (or tier-sampling) naturally spread serve-
    load across more nodes per cell. Routing depth is unchanged. The L_1
    sets stay at size `b` per `h_2`.

    `tier_sample_h2`/`tier_sample_h1` (fix (1)): when set, the PNS pick at
    that layer samples uniformly among the minimum-tier candidates rather
    than locking onto one nearest node deterministically. This breaks the
    Voronoi-cell hotspot without sacrificing locality (the chosen node is
    still at the smallest reachable tier).
    """
    N = topology.n_nodes
    owner = int(rng.integers(0, N))

    pool = np.arange(N, dtype=np.int64)
    pool = pool[pool != owner]
    rng.shuffle(pool)

    b_l2 = b * max(1, h2_virtual)
    level2 = pool[:b_l2]
    level1_per_h2: dict[int, np.ndarray] = {}
    cursor = b_l2
    for h2 in level2.tolist():
        level1_per_h2[h2] = pool[cursor:cursor + b]
        cursor += b

    # Replica candidates are nodes outside owner / L_2 / all L_1.
    mask = np.ones(N, dtype=bool)
    mask[owner] = False
    mask[level2] = False
    for arr in level1_per_h2.values():
        mask[arr] = False
    candidates = np.where(mask)[0]

    n_extra = max(0, n_replicas - 1)
    n_extra = min(n_extra, len(candidates))
    if n_extra == 0:
        replicas: tuple[int, ...] = ()
    elif replica_placement == "pns":
        tiers = topology.tier_between(np.full_like(candidates, owner), candidates)
        # Stable argsort by tier; pool is already shuffled so ties break randomly.
        order = np.argsort(tiers, kind="stable")
        replicas = tuple(int(x) for x in candidates[order[:n_extra]])
    elif replica_placement == "uniform":
        rng.shuffle(candidates)
        replicas = tuple(int(x) for x in candidates[:n_extra])
    else:
        raise ValueError(f"unknown replica_placement: {replica_placement}")

    pinned = frozenset({owner, *replicas})

    clients = np.arange(N, dtype=np.int64)
    tier_mat = topology.tier_between(clients[:, None], level2[None, :])
    h2_pns = _pick_min_tier(rng, tier_mat, level2, tier_sample=tier_sample_h2)
    h2_rand = level2[rng.integers(0, b_l2, size=N)]

    h1_det_pns = np.zeros(N, dtype=np.int64)
    h1_det_rand = np.zeros(N, dtype=np.int64)
    for h2 in level2.tolist():
        l1 = level1_per_h2[h2]
        m_pns = h2_pns == h2
        if m_pns.any():
            sub = clients[m_pns]
            tm = topology.tier_between(sub[:, None], l1[None, :])
            h1_det_pns[m_pns] = _pick_min_tier(rng, tm, l1, tier_sample=tier_sample_h1)
        m_rand = h2_rand == h2
        if m_rand.any():
            h1_det_rand[m_rand] = l1[rng.integers(0, len(l1), size=int(m_rand.sum()))]

    return Placement(
        owner=owner,
        replicas=replicas,
        pinned=pinned,
        level1_per_h2=level1_per_h2,
        h2_pns=h2_pns,
        h2_rand=h2_rand,
        h1_det_pns=h1_det_pns,
        h1_det_rand=h1_det_rand,
    )


def epsilon_for(N: int, b: int, k: float = 2.0) -> float:
    if b <= 1:
        return 0.0
    return min(1.0, k * b * b * math.log(b) / max(1, N))


# ---------- demand -----------------------------------------------------------


def zipf_weights(n_files: int, alpha: float) -> np.ndarray:
    ranks = np.arange(1, n_files + 1, dtype=np.float64)
    if alpha == 0.0:
        w = np.ones_like(ranks)
    else:
        w = 1.0 / np.power(ranks, alpha)
    return w / w.sum()


def generate_demand(
    rng: np.random.Generator,
    *,
    N: int,
    n_files: int,
    requests_per_node: float,
    zipf_alpha: float,
) -> tuple[np.ndarray, np.ndarray]:
    R = max(1, int(round(requests_per_node * N)))
    clients = rng.integers(0, N, size=R, dtype=np.int64)
    weights = zipf_weights(n_files, zipf_alpha)
    files = rng.choice(n_files, size=R, p=weights).astype(np.int64)
    return clients, files


# ---------- per-seed simulation ---------------------------------------------


@dataclass
class SeedResult:
    n_nodes: int
    b: int
    eps: float
    n_keys: int
    total_pulls: int

    client_hops_mean: float
    client_hops_max: int
    system_hops_mean: float
    system_hops_max: int
    eff_bytes_mean: float           # effective bytes / pull (oversub-weighted)
    congestion_factor: float        # eff_bytes / system_hops (system mean)
    cross_rack_share: float         # of system bytes
    cross_row_share: float          # of system bytes

    serve_mean: float
    serve_p99: float
    serve_max: int
    serve_skew_p99: float           # serve_p99 / serve_mean
    serve_skew_max: float           # serve_max / serve_mean

    copies_per_stripe_mean: float
    copies_hot_file_mean: float
    hot_file_share: float


def run_seed(
    *,
    seed: int,
    topology: Topology,
    fabric: FabricRatios,
    pns_enabled: bool,
    n_files: int,
    stripes_per_file: int,
    n_replicas: int,
    cache_capacity_keys: int,
    requests_per_node: float,
    zipf_alpha: float,
    eps_k: float,
    replica_placement: str,
    h2_virtual: int = 1,
    tier_sample_h2: bool = True,
    tier_sample_h1: bool = True,
) -> SeedResult:
    """One seed of the multi-stripe simulation.

    All randomness derives from `seed` via SeedSequence splits. PNS-on and
    PNS-off runs of the same seed see byte-identical placements, demand and
    jitter coins; only the routing rule differs.
    """
    ss = np.random.SeedSequence(seed)
    s_place, s_demand, s_jitter, s_stripeord = ss.spawn(4)
    rng_place_master = np.random.default_rng(s_place)
    rng_demand = np.random.default_rng(s_demand)
    rng_jitter = np.random.default_rng(s_jitter)
    rng_stripeord = np.random.default_rng(s_stripeord)

    N = topology.n_nodes
    b = max(2, math.ceil(N ** (1.0 / 3.0)))
    eps = epsilon_for(N, b, k=eps_k)

    n_keys = n_files * stripes_per_file

    # Per-key placement; each gets an independent RNG so the placement is
    # invariant across PNS toggle.
    placements: list[Placement] = []
    sub_seeds = rng_place_master.integers(0, 2**63 - 1, size=n_keys, dtype=np.int64)
    for ks in sub_seeds.tolist():
        placements.append(
            sample_placement(
                rng=np.random.default_rng(np.uint64(ks)),
                topology=topology,
                n_replicas=n_replicas,
                b=b,
                replica_placement=replica_placement,
                h2_virtual=h2_virtual,
                tier_sample_h2=tier_sample_h2,
                tier_sample_h1=tier_sample_h1,
            )
        )

    request_clients, request_files = generate_demand(
        rng_demand,
        N=N,
        n_files=n_files,
        requests_per_node=requests_per_node,
        zipf_alpha=zipf_alpha,
    )
    R = len(request_clients)
    total_pulls = R * stripes_per_file

    jitter_coins = rng_jitter.random(total_pulls) < eps
    jitter_pick = rng_jitter.integers(0, b, size=total_pulls)

    proxy: list[LRU] = [LRU(cache_capacity_keys) for _ in range(N)]

    client_hops = np.zeros(total_pulls, dtype=np.int8)
    system_hops = np.zeros(total_pulls, dtype=np.int8)
    cross_rack = np.zeros(total_pulls, dtype=np.int8)
    cross_row = np.zeros(total_pulls, dtype=np.int8)
    eff_bytes = np.zeros(total_pulls, dtype=np.float64)
    serve_count = np.zeros(N, dtype=np.int64)

    tier_pair = topology.tier_pair
    w_rack = fabric.oversub_rack
    w_row = fabric.oversub_rack * fabric.oversub_row

    def tier_w(t: int) -> float:
        if t <= TIER_RACK:
            return 1.0
        if t == TIER_ROW:
            return w_rack
        return w_row

    pull_idx = 0
    for r in range(R):
        c = int(request_clients[r])
        f = int(request_files[r])
        # Pull all S stripes of file f in random order.
        stripe_order = np.arange(stripes_per_file)
        rng_stripeord.shuffle(stripe_order)
        for s in stripe_order.tolist():
            key = f * stripes_per_file + s
            pl = placements[key]
            pin = pl.pinned

            # Client-side hit (pinned membership or in c's proxy cache).
            if c in pin:
                pull_idx += 1
                continue
            if key in proxy[c]:
                proxy[c].hit(key)
                pull_idx += 1
                continue

            if pns_enabled:
                h2 = int(pl.h2_pns[c])
            else:
                h2 = int(pl.h2_rand[c])

            if jitter_coins[pull_idx]:
                l1 = pl.level1_per_h2[h2]
                h1 = int(l1[jitter_pick[pull_idx] % len(l1)])
            elif pns_enabled:
                h1 = int(pl.h1_det_pns[c])
            else:
                h1 = int(pl.h1_det_rand[c])

            owner = pl.owner

            ch = sh = 0
            cr_rack = cr_row = 0
            ef = 0.0
            served_by = -1

            if h2 == c:
                # h_2 collapse: path is c -> h1 -> owner.
                if h1 == c:
                    served_by = owner
                    t1 = tier_pair(owner, c)
                    ch = sh = 1
                    cr_rack = 1 if t1 >= TIER_ROW else 0
                    cr_row = 1 if t1 == TIER_CROSS_ROW else 0
                    ef = tier_w(t1)
                elif (h1 in pin) or (key in proxy[h1]):
                    served_by = h1
                    if key in proxy[h1]:
                        proxy[h1].hit(key)
                    t1 = tier_pair(h1, c)
                    ch = sh = 1
                    cr_rack = 1 if t1 >= TIER_ROW else 0
                    cr_row = 1 if t1 == TIER_CROSS_ROW else 0
                    ef = tier_w(t1)
                else:
                    served_by = owner
                    t1 = tier_pair(owner, h1)
                    t2 = tier_pair(h1, c)
                    ch = sh = 2
                    cr_rack = (1 if t1 >= TIER_ROW else 0) + (1 if t2 >= TIER_ROW else 0)
                    cr_row = (1 if t1 == TIER_CROSS_ROW else 0) + (1 if t2 == TIER_CROSS_ROW else 0)
                    ef = tier_w(t1) + tier_w(t2)
                    proxy[h1].insert(key)
            elif (h2 in pin) or (key in proxy[h2]):
                served_by = h2
                if key in proxy[h2]:
                    proxy[h2].hit(key)
                t1 = tier_pair(h2, c)
                ch = sh = 1
                cr_rack = 1 if t1 >= TIER_ROW else 0
                cr_row = 1 if t1 == TIER_CROSS_ROW else 0
                ef = tier_w(t1)
                # Eager push h_2 -> h_1 if h_1 is not warm. Counted as system
                # bytes only; client_hops stays at 1.
                if h1 != c and h1 != h2 and not (h1 in pin or key in proxy[h1]):
                    tp = tier_pair(h2, h1)
                    sh += 1
                    cr_rack += 1 if tp >= TIER_ROW else 0
                    cr_row += 1 if tp == TIER_CROSS_ROW else 0
                    ef += tier_w(tp)
                    proxy[h1].insert(key)
            elif (h1 in pin) or (key in proxy[h1]):
                served_by = h1
                if key in proxy[h1]:
                    proxy[h1].hit(key)
                t1 = tier_pair(h1, h2)
                t2 = tier_pair(h2, c)
                ch = sh = 2
                cr_rack = (1 if t1 >= TIER_ROW else 0) + (1 if t2 >= TIER_ROW else 0)
                cr_row = (1 if t1 == TIER_CROSS_ROW else 0) + (1 if t2 == TIER_CROSS_ROW else 0)
                ef = tier_w(t1) + tier_w(t2)
                proxy[h2].insert(key)
            else:
                served_by = owner
                t1 = tier_pair(owner, h1)
                t2 = tier_pair(h1, h2)
                t3 = tier_pair(h2, c)
                ch = sh = 3
                cr_rack = (
                    (1 if t1 >= TIER_ROW else 0)
                    + (1 if t2 >= TIER_ROW else 0)
                    + (1 if t3 >= TIER_ROW else 0)
                )
                cr_row = (
                    (1 if t1 == TIER_CROSS_ROW else 0)
                    + (1 if t2 == TIER_CROSS_ROW else 0)
                    + (1 if t3 == TIER_CROSS_ROW else 0)
                )
                ef = tier_w(t1) + tier_w(t2) + tier_w(t3)
                proxy[h1].insert(key)
                proxy[h2].insert(key)

            client_hops[pull_idx] = ch
            system_hops[pull_idx] = sh
            cross_rack[pull_idx] = cr_rack
            cross_row[pull_idx] = cr_row
            eff_bytes[pull_idx] = ef
            if served_by >= 0:
                serve_count[served_by] += 1
            pull_idx += 1

    # End-of-run cache state per key (for hot-file copies and copies-per-stripe).
    copies_per_key = np.zeros(n_keys, dtype=np.int32)
    for k in range(n_keys):
        copies_per_key[k] = len(placements[k].pinned)
    for node in range(N):
        for k in proxy[node].keys():
            copies_per_key[k] += 1

    # Hot file: file index 0 (highest-rank under Zipf; arbitrary under uniform).
    hot_keys = list(range(0, stripes_per_file))
    copies_hot = float(np.mean(copies_per_key[hot_keys]))

    weights = zipf_weights(n_files, zipf_alpha)
    expected_hot_share = float(weights[0])
    readers = max(1.0, R * expected_hot_share)
    hot_file_share = min(1.0, stripes_per_file * copies_hot / readers)

    # Hops aggregates: only count pulls that actually moved bytes (skip same-node
    # hits that produce zero hops; otherwise the mean is dominated by trivial 0s).
    served_mask = system_hops > 0
    if served_mask.any():
        client_hops_mean = float(client_hops[served_mask].mean())
        system_hops_mean = float(system_hops[served_mask].mean())
        client_hops_max = int(client_hops.max())
        system_hops_max = int(system_hops.max())
        eff_bytes_mean = float(eff_bytes[served_mask].mean())
        sys_total = float(system_hops.sum())
        cross_rack_share = float(cross_rack.sum()) / sys_total
        cross_row_share = float(cross_row.sum()) / sys_total
        cong = float(eff_bytes.sum()) / sys_total
    else:
        client_hops_mean = system_hops_mean = eff_bytes_mean = 0.0
        client_hops_max = system_hops_max = 0
        cross_rack_share = cross_row_share = 0.0
        cong = 1.0

    serve_mean = float(serve_count.mean()) if N else 0.0
    serve_p99 = float(np.percentile(serve_count, 99))
    serve_max = int(serve_count.max())
    serve_skew_p99 = serve_p99 / serve_mean if serve_mean > 0 else 0.0
    serve_skew_max = serve_max / serve_mean if serve_mean > 0 else 0.0

    return SeedResult(
        n_nodes=N,
        b=b,
        eps=eps,
        n_keys=n_keys,
        total_pulls=int(served_mask.sum()),
        client_hops_mean=client_hops_mean,
        client_hops_max=client_hops_max,
        system_hops_mean=system_hops_mean,
        system_hops_max=system_hops_max,
        eff_bytes_mean=eff_bytes_mean,
        congestion_factor=cong,
        cross_rack_share=cross_rack_share,
        cross_row_share=cross_row_share,
        serve_mean=serve_mean,
        serve_p99=serve_p99,
        serve_max=serve_max,
        serve_skew_p99=serve_skew_p99,
        serve_skew_max=serve_skew_max,
        copies_per_stripe_mean=float(copies_per_key.mean()),
        copies_hot_file_mean=copies_hot,
        hot_file_share=hot_file_share,
    )


# ---------- aggregation across seeds ----------------------------------------


def _band(values: list[float]) -> tuple[float, float, float]:
    arr = np.asarray(values, dtype=np.float64)
    return float(arr.mean()), float(np.percentile(arr, 5)), float(np.percentile(arr, 95))


def _paired_benefit(pns: list[float], base: list[float], floor: float) -> tuple[
    float, float, float, bool
]:
    """Per-seed (base - pns) / base. Returns mean, p5, p95, significant?
    Marks NOT significant when the [p5, p95] interval crosses zero, OR when
    too many baseline samples are below `floor` (denominator unstable).
    """
    pns_a = np.asarray(pns, dtype=np.float64)
    base_a = np.asarray(base, dtype=np.float64)
    valid = base_a >= floor
    if valid.sum() < max(3, len(base_a) // 2):
        return 0.0, 0.0, 0.0, False
    rel = (base_a[valid] - pns_a[valid]) / base_a[valid]
    mean = float(rel.mean())
    p5 = float(np.percentile(rel, 5))
    p95 = float(np.percentile(rel, 95))
    significant = (p5 > 0.0) or (p95 < 0.0)
    return mean, p5, p95, significant


# ---------- formatting -------------------------------------------------------


def fmt_int(x: float) -> str:
    return f"{int(round(x)):,}"


def fmt_band(mean: float, p5: float, p95: float, *, pct: bool = False, digits: int = 3) -> str:
    if pct:
        return f"{mean * 100:.{digits}f}% [{p5 * 100:.{digits}f}, {p95 * 100:.{digits}f}]"
    return f"{mean:.{digits}f} [{p5:.{digits}f}, {p95:.{digits}f}]"


def fmt_benefit(mean: float, p5: float, p95: float, sig: bool) -> str:
    flag = "" if sig else " [*]"
    return f"{mean * 100:+.1f}% [{p5 * 100:+.1f}, {p95 * 100:+.1f}]{flag}"


# ---------- driver -----------------------------------------------------------


def parse_args() -> argparse.Namespace:
    ap = argparse.ArgumentParser(
        description=__doc__,
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    ap.add_argument(
        "--sizes",
        type=str,
        default="1024,4096,16384",
        help="comma-separated target node counts (default: 1024,4096,16384). "
             "Larger sizes scale linearly in time; combine with --seeds.",
    )
    ap.add_argument("--nodes-per-rack", type=int, default=32)
    ap.add_argument("--racks-per-row", type=int, default=16)
    ap.add_argument("--replicas", type=int, default=8,
                    help="successor-list length r (incl owner)")
    ap.add_argument("--replica-placement", choices=("auto", "pns", "uniform"),
                    default="auto",
                    help="auto = pns when PNS routing is on, uniform when off")
    ap.add_argument("--oversub-rack", type=float, default=4.0)
    ap.add_argument("--oversub-row", type=float, default=2.0)
    ap.add_argument("--files", type=int, default=20,
                    help="number of files (keys = files * stripes)")
    ap.add_argument("--stripes", type=int, default=10)
    ap.add_argument("--zipf-alpha", type=float, default=1.0,
                    help="Zipf skew over files; 0 == uniform-over-files")
    ap.add_argument("--requests-per-node", type=float, default=1.0,
                    help="R / N. Each request fans out to all S stripes.")
    ap.add_argument("--cache-capacity-keys", type=int, default=0,
                    help="per-node proxy LRU capacity in stripes "
                         "(0 means auto: 2 * b^2 derived per N)")
    ap.add_argument("--eps-k", type=float, default=2.0)
    ap.add_argument("--h2-virtual", type=int, default=2,
                    help="virtual-node multiplier for the level-2 finger "
                         "(fix 3). |L_2| = h2_virtual * b. Default 2. "
                         "Higher V gives more h_2 candidates per cell, so "
                         "PNS argmin spreads serve-load across V nodes per "
                         "topological cell at the cost of V * b cache "
                         "footprint at the L_2 layer per key. Set 1 to "
                         "disable virtualization.")
    ap.add_argument("--no-h2-tier-sample", action="store_true",
                    help="disable nearest-tier random sampling for h_2 "
                         "(fix 1). With it enabled (default), the PNS h_2 "
                         "pick samples uniformly among candidates at the "
                         "minimum tier instead of locking onto one nearest "
                         "node deterministically.")
    ap.add_argument("--no-h1-tier-sample", action="store_true",
                    help="disable nearest-tier random sampling for h_1.")
    ap.add_argument("--seeds", type=int, default=30,
                    help="number of independent seeds for variance bands")
    ap.add_argument("--seed-base", type=int, default=42)
    ap.add_argument("--benefit-floor", type=float, default=0.01,
                    help="minimum baseline value to compute PNS-benefit ratio")
    ap.add_argument(
        "--out",
        type=str,
        default=str(Path(__file__).parent / "results_table.md"),
    )
    return ap.parse_args()


def cache_capacity(arg_value: int, b: int) -> int:
    if arg_value > 0:
        return arg_value
    # Auto: 2 * b^2 - enough room for the b^2 ceiling on a few hot keys plus
    # working set, but small enough that less-popular keys evict.
    return max(8, 2 * b * b)


def run_size(args: argparse.Namespace, target_n: int) -> dict:
    topology = build_topology(target_n, args.nodes_per_rack, args.racks_per_row)
    fabric = FabricRatios(args.oversub_rack, args.oversub_row)
    N = topology.n_nodes
    b = max(2, math.ceil(N ** (1.0 / 3.0)))
    cap = cache_capacity(args.cache_capacity_keys, b)
    eps = epsilon_for(N, b, k=args.eps_k)

    rep_pns = "pns" if args.replica_placement == "auto" else args.replica_placement
    rep_off = "uniform" if args.replica_placement == "auto" else args.replica_placement

    pns_results: list[SeedResult] = []
    off_results: list[SeedResult] = []

    for i in range(args.seeds):
        seed = args.seed_base + i
        pns_results.append(
            run_seed(
                seed=seed, topology=topology, fabric=fabric, pns_enabled=True,
                n_files=args.files, stripes_per_file=args.stripes,
                n_replicas=args.replicas, cache_capacity_keys=cap,
                requests_per_node=args.requests_per_node,
                zipf_alpha=args.zipf_alpha, eps_k=args.eps_k,
                replica_placement=rep_pns,
                h2_virtual=args.h2_virtual,
                tier_sample_h2=not args.no_h2_tier_sample,
                tier_sample_h1=not args.no_h1_tier_sample,
            )
        )
        off_results.append(
            run_seed(
                seed=seed, topology=topology, fabric=fabric, pns_enabled=False,
                n_files=args.files, stripes_per_file=args.stripes,
                n_replicas=args.replicas, cache_capacity_keys=cap,
                requests_per_node=args.requests_per_node,
                zipf_alpha=args.zipf_alpha, eps_k=args.eps_k,
                replica_placement=rep_off,
                h2_virtual=args.h2_virtual,
                # Tier-sample flags don't affect the uniform-random branch
                # but pass through for completeness.
                tier_sample_h2=not args.no_h2_tier_sample,
                tier_sample_h1=not args.no_h1_tier_sample,
            )
        )

    def col(results: list[SeedResult], attr: str) -> list[float]:
        return [getattr(r, attr) for r in results]

    return {
        "n": N,
        "b": b,
        "eps": eps,
        "cache_cap": cap,
        # PNS-on bands
        "client_hops": _band(col(pns_results, "client_hops_mean")),
        "system_hops": _band(col(pns_results, "system_hops_mean")),
        "cong": _band(col(pns_results, "congestion_factor")),
        "cross_rack": _band(col(pns_results, "cross_rack_share")),
        "cross_row": _band(col(pns_results, "cross_row_share")),
        "copies_hot": _band(col(pns_results, "copies_hot_file_mean")),
        "copies_per_stripe": _band(col(pns_results, "copies_per_stripe_mean")),
        "hot_share": _band(col(pns_results, "hot_file_share")),
        "serve_skew_p99": _band(col(pns_results, "serve_skew_p99")),
        "serve_skew_max": _band(col(pns_results, "serve_skew_max")),
        # PNS benefits (paired)
        "ben_cross_rack": _paired_benefit(
            col(pns_results, "cross_rack_share"),
            col(off_results, "cross_rack_share"),
            args.benefit_floor,
        ),
        "ben_cross_row": _paired_benefit(
            col(pns_results, "cross_row_share"),
            col(off_results, "cross_row_share"),
            args.benefit_floor,
        ),
        "ben_cong": _paired_benefit(
            col(pns_results, "congestion_factor"),
            col(off_results, "congestion_factor"),
            args.benefit_floor,
        ),
        "ben_serve_p99": _paired_benefit(
            col(pns_results, "serve_skew_p99"),
            col(off_results, "serve_skew_p99"),
            args.benefit_floor,
        ),
    }


def render_markdown(args: argparse.Namespace, rows: list[dict]) -> str:
    lines: list[str] = []
    lines.append("# Pull-through cache evaluator results\n")
    lines.append("")
    lines.append("## Configuration\n")
    lines.append(f"- topology: {args.nodes_per_rack} nodes/rack, "
                 f"{args.racks_per_row} racks/row")
    lines.append(f"- replicas (incl owner): {args.replicas} "
                 f"(placement = {args.replica_placement})")
    lines.append(f"- oversubscription: rack {args.oversub_rack}x, "
                 f"row {args.oversub_row}x")
    lines.append(f"- workload: {args.files} files * {args.stripes} stripes; "
                 f"Zipf alpha={args.zipf_alpha}; "
                 f"requests/node={args.requests_per_node}")
    lines.append(f"- jitter: eps_k={args.eps_k}")
    lines.append(
        f"- h_2 virtual multiplier V={args.h2_virtual} "
        f"(|L_2| = V * b)"
    )
    lines.append(
        f"- nearest-tier sampling: h_2 "
        f"{'on' if not args.no_h2_tier_sample else 'off'}, "
        f"h_1 {'on' if not args.no_h1_tier_sample else 'off'}"
    )
    lines.append(f"- seeds: {args.seeds} (paired PNS A/B)")
    lines.append("")
    lines.append("Bands are mean [p5, p95] across seeds. PNS benefit is the "
                 "per-seed paired ratio (baseline - pns) / baseline; values "
                 "marked [*] have a [p5, p95] interval that crosses zero "
                 "and are not statistically significant.\n")

    # Main table.
    lines.append("## PNS-on locality and load (mean [p5, p95])\n")
    headers = [
        "N", "b", "eps", "cache cap",
        "client hops", "system hops",
        "congestion factor", "cross-rack share", "cross-row share",
        "copies/stripe", "copies hot", "hot-file share",
        "serve skew p99", "serve skew max",
    ]
    lines.append("| " + " | ".join(headers) + " |")
    lines.append("| " + " | ".join(["---"] * len(headers)) + " |")
    for r in rows:
        cells = [
            fmt_int(r["n"]),
            fmt_int(r["b"]),
            f"{r['eps']:.3f}",
            fmt_int(r["cache_cap"]),
            fmt_band(*r["client_hops"]),
            fmt_band(*r["system_hops"]),
            fmt_band(*r["cong"]),
            fmt_band(*r["cross_rack"]),
            fmt_band(*r["cross_row"]),
            fmt_band(*r["copies_per_stripe"]),
            fmt_band(*r["copies_hot"]),
            fmt_band(*r["hot_share"], pct=True, digits=2),
            fmt_band(*r["serve_skew_p99"], digits=2),
            fmt_band(*r["serve_skew_max"], digits=2),
        ]
        lines.append("| " + " | ".join(cells) + " |")
    lines.append("")

    # Paired benefit table.
    lines.append("## PNS benefit, paired by seed (mean [p5, p95])\n")
    bheaders = ["N", "cross-rack", "cross-row", "congestion", "serve skew p99"]
    lines.append("| " + " | ".join(bheaders) + " |")
    lines.append("| " + " | ".join(["---"] * len(bheaders)) + " |")
    for r in rows:
        cells = [
            fmt_int(r["n"]),
            fmt_benefit(*r["ben_cross_rack"]),
            fmt_benefit(*r["ben_cross_row"]),
            fmt_benefit(*r["ben_cong"]),
            fmt_benefit(*r["ben_serve_p99"]),
        ]
        lines.append("| " + " | ".join(cells) + " |")
    lines.append("")

    lines.append(
        "Legend: positive PNS-benefit means PNS reduces the metric "
        "(less cross-boundary IO, less congestion, less serve-load skew). "
        "[*] marks a band crossing zero (not significant given the seed count).\n"
    )
    return "\n".join(lines)


def main() -> None:
    args = parse_args()
    sizes = [int(s) for s in args.sizes.split(",") if s.strip()]
    rows: list[dict] = []
    for target_n in sizes:
        rows.append(run_size(args, target_n))

    md = render_markdown(args, rows)
    print(md, end="")

    out_path = Path(args.out)
    out_path.parent.mkdir(parents=True, exist_ok=True)
    out_path.write_text(md)


if __name__ == "__main__":
    main()
