"""Generate the cache-distribution ring diagram for architecture.md.

Run: python3 gen_cache_ring.py
Output: cache-ring.png in the same directory.
"""

import math
import os

import matplotlib.pyplot as plt
from matplotlib.patches import Circle, FancyArrowPatch, FancyBboxPatch


NODES = [
    ("Node A", ["\u2605 D1", "D6"]),
    ("Node B", ["D1", "D2"]),
    ("Node C", ["\u2605 D2", "D5"]),
    ("Node D", ["\u2605 D3"]),
    ("Node E", ["D2", "\u2605 D4"]),
    ("Node F", ["D1", "D3"]),
]

# Which digest each puller (★) was originally pulled from origin for.
# Maps node-index -> digest label.
PULLERS = {
    0: "D1",  # Node A
    2: "D2",  # Node C
    3: "D3",  # Node D
    4: "D4",  # Node E
}

RING_RADIUS = 3.2
NODE_RADIUS = 0.85
ORIGIN_XY = (0.0, -5.6)


def node_position(i: int, n: int):
    # Place node 0 at the top, go clockwise.
    angle = math.pi / 2 - i * (2 * math.pi / n)
    return RING_RADIUS * math.cos(angle), RING_RADIUS * math.sin(angle)


def draw():
    fig, ax = plt.subplots(figsize=(9, 10), dpi=160)
    ax.set_aspect("equal")
    ax.axis("off")

    # Cluster boundary — faint dashed circle behind the nodes.
    cluster = Circle(
        (0, 0),
        RING_RADIUS + NODE_RADIUS + 0.45,
        fill=False,
        linestyle=(0, (4, 3)),
        edgecolor="#9aa3ad",
        linewidth=1.2,
        zorder=0,
    )
    ax.add_patch(cluster)
    ax.text(
        0,
        RING_RADIUS + NODE_RADIUS + 0.9,
        "Kubernetes cluster  \u00b7  libp2p P2P fabric",
        ha="center",
        va="bottom",
        fontsize=11,
        color="#4a5560",
        style="italic",
    )

    # Ring edges (connect consecutive nodes around the circle).
    n = len(NODES)
    positions = [node_position(i, n) for i in range(n)]
    for i in range(n):
        x1, y1 = positions[i]
        x2, y2 = positions[(i + 1) % n]
        ax.plot(
            [x1, x2],
            [y1, y2],
            color="#c5ccd6",
            linewidth=1.4,
            zorder=1,
        )

    # Nodes.
    for i, (name, digests) in enumerate(NODES):
        x, y = positions[i]
        circ = Circle(
            (x, y),
            NODE_RADIUS,
            facecolor="#eef3fb",
            edgecolor="#3b6aa8",
            linewidth=1.6,
            zorder=2,
        )
        ax.add_patch(circ)
        ax.text(
            x,
            y + 0.22,
            name,
            ha="center",
            va="center",
            fontsize=10,
            fontweight="bold",
            color="#1f2a37",
            zorder=3,
        )
        ax.text(
            x,
            y - 0.25,
            "\n".join(digests),
            ha="center",
            va="center",
            fontsize=9,
            color="#1f2a37",
            zorder=3,
        )

    # Origin registry.
    ox, oy = ORIGIN_XY
    origin_box = FancyBboxPatch(
        (ox - 1.4, oy - 0.45),
        2.8,
        0.9,
        boxstyle="round,pad=0.02,rounding_size=0.18",
        facecolor="#fff4e0",
        edgecolor="#b8862a",
        linewidth=1.6,
        zorder=2,
    )
    ax.add_patch(origin_box)
    ax.text(
        ox,
        oy,
        "Origin registry",
        ha="center",
        va="center",
        fontsize=11,
        fontweight="bold",
        color="#5a3f00",
        zorder=3,
    )

    # Origin -> puller arrows (each digest pulled from origin exactly once).
    # Curve the arrows so they fan around the ring instead of crossing through
    # nodes on the far side. The sign of `rad` decides which way to curve;
    # we pick it from the angular position of the target so each arrow
    # bows outward away from the cluster center.
    for i, digest in PULLERS.items():
        nx, ny = positions[i]
        dx = nx - ox
        dy = ny - oy
        dist = math.hypot(dx, dy)
        # Aim at the edge of the node circle, not its center.
        tx = nx - (dx / dist) * NODE_RADIUS
        ty = ny - (dy / dist) * NODE_RADIUS
        sx = ox + (dx / dist) * 0.5  # start just above the origin box

        # Curvature: positive bends left-of-direction, negative right.
        # Choose the bow so the arrow goes around the cluster rather than
        # through nodes on the far side of the ring from origin.
        if nx > 0.05:
            rad = -0.30
        elif nx < -0.05:
            rad = 0.30
        elif ny > 0:
            # On-axis far-side target (e.g., Node A directly above origin):
            # curve through the left-side gap between Node F and Node E to
            # avoid passing through Node D, which sits between them.
            rad = 0.40
        else:
            # On-axis near-side target (e.g., Node D directly above origin
            # but close to it): a straight arrow is clear of every other node.
            rad = 0.0

        arrow = FancyArrowPatch(
            (sx, oy + 0.45),
            (tx, ty),
            arrowstyle="-|>",
            mutation_scale=14,
            linewidth=1.4,
            color="#b8862a",
            connectionstyle=f"arc3,rad={rad}",
            zorder=1,
        )
        ax.add_patch(arrow)

        # Label near the arrow's midpoint, nudged outward along the curve.
        mx = (sx + tx) / 2
        my = (oy + 0.45 + ty) / 2
        # Perpendicular offset, sign matched to the curvature direction.
        ndx = tx - sx
        ndy = ty - (oy + 0.45)
        nlen = math.hypot(ndx, ndy)
        perp_x = -ndy / nlen
        perp_y = ndx / nlen
        offset = 0.55 * (-rad if rad != 0 else 0.0)
        if rad == 0.0:
            # Vertical arrow: push the label to the side.
            label_x = mx + 0.30
            label_y = my
            rotation = 90
        else:
            label_x = mx + perp_x * offset
            label_y = my + perp_y * offset
            rotation = math.degrees(math.atan2(ndy, ndx))
            if rotation > 90:
                rotation -= 180
            elif rotation < -90:
                rotation += 180
        ax.text(
            label_x,
            label_y,
            f"{digest} once",
            ha="center",
            va="center",
            fontsize=8.5,
            color="#7a5a10",
            rotation=rotation,
            rotation_mode="anchor",
            zorder=4,
        )

    # Legend.
    legend_y = -7.3
    ax.text(
        -4.6,
        legend_y,
        "\u2605  HRW rank-0 (designated puller) for that digest",
        fontsize=9,
        color="#1f2a37",
        ha="left",
        va="center",
    )
    ax.text(
        -4.6,
        legend_y - 0.40,
        "Ring edges = libp2p mesh (every node is a potential peer of every other)",
        fontsize=9,
        color="#4a5560",
        ha="left",
        va="center",
    )
    ax.text(
        -4.6,
        legend_y - 0.80,
        "Origin contacted at most once per unique digest (F1)",
        fontsize=9,
        color="#4a5560",
        ha="left",
        va="center",
    )

    ax.set_xlim(-5.2, 5.2)
    ax.set_ylim(-8.4, 5.0)
    fig.tight_layout()

    out = os.path.join(os.path.dirname(os.path.abspath(__file__)), "cache-ring.png")
    fig.savefig(out, dpi=160, bbox_inches="tight", facecolor="white")
    print(f"wrote {out}")


if __name__ == "__main__":
    draw()
