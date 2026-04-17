// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import * as React from 'react';
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import {
  getTopologyNodeGlyph,
  renderTopologyNodeGlyph,
  useTopologyNodeIconTextures
} from '../common/topologyIcons';
import { GatewayPoolStatus, NodeStatus, PeeringStatus, SiteStatus } from '../../types';
import { getNodeStatus, ReagraphModule } from '../nodes/shared/index';

function buildGraph(
  allSiteNames: string[],
  allPoolNames: string[],
  peerings: PeeringStatus[],
  hiddenSites: Set<string>,
  hiddenGatewayPools: Set<string>,
  existingGatewayPools: Set<string>,
  palette: { site: string; siteEmpty: string; siteWarn: string; siteDanger: string; pool: string; poolEmpty: string; poolWarn: string; poolDanger: string; edge: string; edgeDim: string; edgeUp: string; edgeWarn: string; edgeDanger: string },
  siteCounts?: Map<string, { online: number; total: number; warning?: number; danger?: number }>,
  poolCounts?: Map<string, { online: number; total: number; warning?: number; danger?: number }>,
  edgeHealthCheckCounts?: Map<string, { up: number; total: number }>,
  poolToSite?: Map<string, string>
) {
  // Dim a hex color by reducing its opacity (blend toward background)
  const dimColor = (hex: string) => {
    const r = parseInt(hex.slice(1, 3), 16);
    const g = parseInt(hex.slice(3, 5), 16);
    const b = parseInt(hex.slice(5, 7), 16);
    const mix = (c: number) => Math.round(c * 0.3 + 30 * 0.7);
    return `#${mix(r).toString(16).padStart(2, '0')}${mix(g).toString(16).padStart(2, '0')}${mix(b).toString(16).padStart(2, '0')}`;
  };
  const nodes: { id: string; label: string; fill: string; activeFill: string; data: { level: number; group: string; peerings: string[] } }[] = [];
  const edges: { id: string; source: string; target: string; fill?: string; size?: number; data?: { peerings: string[]; hcUp: number; hcTotal: number } }[] = [];
  const nodeSet = new Set<string>();
  const edgeSet = new Set<string>();
  const edgePeerings = new Map<string, Set<string>>();
  const sitePeerings = new Map<string, Set<string>>();
  const poolPeerings = new Map<string, Set<string>>();

  for (const peering of peerings) {
    const peeringName = peering.name || 'peering';
    for (const site of peering.sites || []) {
      if (!sitePeerings.has(site)) {
        sitePeerings.set(site, new Set());
      }
      sitePeerings.get(site)?.add(peeringName);
    }
    for (const pool of peering.gatewayPools || []) {
      if (!existingGatewayPools.has(pool)) {
        continue;
      }
      if (!poolPeerings.has(pool)) {
        poolPeerings.set(pool, new Set());
      }
      poolPeerings.get(pool)?.add(peeringName);
    }
  }

  const addNode = (id: string, label: string, group: string, hidden: boolean) => {
    if (!nodeSet.has(id)) {
      nodeSet.add(id);
      let fill = group === 'pool' ? palette.pool : palette.site;
      if (group === 'site' && siteCounts) {
        const counts = siteCounts.get(label);
        if (counts) {
          if (counts.total === 0) {
            fill = palette.siteEmpty;
          } else if ((counts.danger || 0) > 0) {
            fill = palette.siteDanger;
          } else if ((counts.warning || 0) > 0) {
            fill = palette.siteWarn;
          }
        }
      }
      if (group === 'pool' && poolCounts) {
        const counts = poolCounts.get(label);
        if (counts) {
          // No matching node entries for this pool in current cluster status:
          // render gray (empty) instead of green.
          if (counts.total === 0) {
            fill = palette.poolEmpty;
          } else if ((counts.danger || 0) > 0) {
            fill = palette.poolDanger;
          } else if ((counts.warning || 0) > 0) {
            fill = palette.poolWarn;
          } else if (counts.online === 0 && counts.total > 0) {
            fill = palette.poolDanger;
          } else if (counts.online < counts.total) {
            fill = palette.poolWarn;
          }
        }
      }
      if (hidden) {
        fill = dimColor(fill);
      }
      const peeringsForNode = group === 'pool'
        ? Array.from(poolPeerings.get(label) || [])
        : Array.from(sitePeerings.get(label) || []);
      nodes.push({ id, label, fill, activeFill: fill, data: { level: group === 'pool' ? 0 : 1, group, peerings: peeringsForNode } });
    }
  };

  const addEdge = (a: string, b: string) => {
    if (a === b) return;
    const key = a < b ? `${a}|${b}` : `${b}|${a}`;
    if (edgeSet.has(key)) return;
    edgeSet.add(key);
    const isHidden = (nodeId: string) => {
      if (nodeId.startsWith('site:')) return hiddenSites.has(nodeId.slice(5));
      if (nodeId.startsWith('pool:')) return hiddenGatewayPools.has(nodeId.slice(5));
      return false;
    };
    let edgeFill = palette.edge;
    if (isHidden(a) || isHidden(b)) {
      edgeFill = palette.edgeDim;
    } else if (edgeHealthCheckCounts) {
      const counts = edgeHealthCheckCounts.get(key);
      // No matching health check entries for this edge (missing map entry or total=0):
      // keep default gray edge color. Only color healthy/warn/danger with real data.
      if (!counts || counts.total === 0) {
        edgeFill = palette.edge;
      } else {
        if (counts.up === counts.total) {
          edgeFill = palette.edgeUp;
        } else if (counts.total - counts.up > counts.total / 2) {
          edgeFill = palette.edgeDanger;
        } else {
          edgeFill = palette.edgeWarn;
        }
      }
    }
    edges.push({ id: key, source: a, target: b, fill: edgeFill, size: 3.5 });
  };

  // Seed all known sites and gateway pools so isolated entities still render without edges.
  for (const site of allSiteNames) {
    addNode(`site:${site}`, site, 'site', hiddenSites.has(site));
  }
  for (const pool of allPoolNames) {
    addNode(`pool:${pool}`, pool, 'pool', hiddenGatewayPools.has(pool));
  }

  for (const peering of peerings) {
    const sites = peering.sites || [];
    const pools = (peering.gatewayPools || []).filter((pool) => existingGatewayPools.has(pool));
    const pName = peering.name || 'peering';
    const isPoolPeering = pName.startsWith('poolpeering/');

    for (const site of sites) {
      addNode(`site:${site}`, site, 'site', hiddenSites.has(site));
    }
    for (const pool of pools) {
      addNode(`pool:${pool}`, pool, 'pool', hiddenGatewayPools.has(pool));
    }

    // Track which peerings each edge belongs to
    const trackEdgePeering = (a: string, b: string) => {
      const key = a < b ? `${a}|${b}` : `${b}|${a}`;
      if (!edgePeerings.has(key)) edgePeerings.set(key, new Set());
      edgePeerings.get(key)?.add(pName);
    };

    if (pools.length > 0) {
      for (const site of sites) {
        for (const pool of pools) {
          trackEdgePeering(`site:${site}`, `pool:${pool}`);
          addEdge(`site:${site}`, `pool:${pool}`);
        }
      }
      if (isPoolPeering && pools.length > 1) {
        for (let i = 0; i < pools.length; i++) {
          for (let j = i + 1; j < pools.length; j++) {
            trackEdgePeering(`pool:${pools[i]}`, `pool:${pools[j]}`);
            addEdge(`pool:${pools[i]}`, `pool:${pools[j]}`);
          }
        }
      }
    } else if (sites.length > 1) {
      for (let i = 0; i < sites.length; i++) {
        for (let j = i + 1; j < sites.length; j++) {
          trackEdgePeering(`site:${sites[i]}`, `site:${sites[j]}`);
          addEdge(`site:${sites[i]}`, `site:${sites[j]}`);
        }
      }
    }
  }

  // Attach peering names and health check counts to edge data
  for (const edge of edges) {
    const pNames = edgePeerings.get(edge.id);
    const hc = edgeHealthCheckCounts?.get(edge.id);
    edge.data = {
      peerings: pNames ? Array.from(pNames) : [],
      hcUp: hc?.up ?? 0,
      hcTotal: hc?.total ?? 0
    };
  }

  // Sort nodes lexicographically by label so layout is deterministic
  nodes.sort((a, b) => a.label.localeCompare(b.label));

  return { nodes, edges };
}

function buildNodeGraph(
  nodes: NodeStatus[],
  gatewayByNode: Map<string, string>,
  palette: {
    nodeHealthy: string;
    nodeWarn: string;
    nodeDanger: string;
    edge: string;
    edgeUp: string;
    edgeWarn: string;
    edgeDanger: string;
  },
  options?: {
    edgeSize?: number;
  }
) {
  const graphNodes: {
    id: string;
    label: string;
    fill: string;
    activeFill: string;
    data: { level: number; group: string; peerings: string[]; lines: string[] };
  }[] = [];
  const graphEdges: {
    id: string;
    source: string;
    target: string;
    fill?: string;
    size?: number;
    data?: {
      peerings: string[];
      hcUp: number;
      hcTotal: number;
      sourceLabel: string;
      targetLabel: string;
    };
  }[] = [];

  const nodeByName = new Map<string, NodeStatus>();
  for (const node of nodes) {
    const nodeName = node.nodeInfo?.name;
    if (!nodeName) continue;
    nodeByName.set(nodeName, node);
  }

  for (const [nodeName, node] of nodeByName.entries()) {
    const isGateway = node.nodeInfo?.isGateway || gatewayByNode.has(nodeName);
    const status = getNodeStatus(node);
    let fill = palette.nodeHealthy;
    if (status === 'warning') {
      fill = palette.nodeWarn;
    } else if (status === 'danger') {
      fill = palette.nodeDanger;
    }

    const siteName = node.nodeInfo?.siteName || '-';
    const poolName = gatewayByNode.get(nodeName) || '-';
    const lines = isGateway
      ? [`Gateway Pool: ${poolName}`, `Site: ${siteName}`]
      : [`Site: ${siteName}`];

    graphNodes.push({
      id: `node:${nodeName}`,
      label: nodeName,
      fill,
      activeFill: fill,
      data: {
        level: isGateway ? 0 : 1,
        group: isGateway ? 'gateway-node' : 'worker-node',
        peerings: [],
        lines
      }
    });
  }

  const edgeCounts = new Map<string, { up: number; total: number }>();
  for (const [nodeName, node] of nodeByName.entries()) {
    for (const peer of node.peers || []) {
      const peerName = peer.name;
      if (!peerName || !nodeByName.has(peerName) || peerName === nodeName) {
        continue;
      }
      const srcId = `node:${nodeName}`;
      const dstId = `node:${peerName}`;
      const key = srcId < dstId ? `${srcId}|${dstId}` : `${dstId}|${srcId}`;
      const current = edgeCounts.get(key) || { up: 0, total: 0 };
      if (peer.healthCheck?.enabled || peer.healthCheck) {
        current.total += 1;
        const rawStatus = (peer.healthCheck?.status || '').trim().toLowerCase();
        if (rawStatus === 'up') {
          current.up += 1;
        }
      }
      edgeCounts.set(key, current);
    }
  }

  for (const [edgeId, counts] of edgeCounts.entries()) {
    const [source, target] = edgeId.split('|');
    let edgeFill = palette.edge;
    if (counts.total > 0) {
      if (counts.up === counts.total) {
        edgeFill = palette.edgeUp;
      } else if (counts.total - counts.up > counts.total / 2) {
        edgeFill = palette.edgeDanger;
      } else {
        edgeFill = palette.edgeWarn;
      }
    }

    graphEdges.push({
      id: edgeId,
      source,
      target,
      fill: edgeFill,
      size: options?.edgeSize ?? 2,
      data: {
        peerings: [],
        hcUp: counts.up,
        hcTotal: counts.total,
        sourceLabel: source.startsWith('node:') ? source.slice(5) : source,
        targetLabel: target.startsWith('node:') ? target.slice(5) : target
      }
    });
  }

  graphNodes.sort((a, b) => a.label.localeCompare(b.label));
  graphEdges.sort((a, b) => a.id.localeCompare(b.id));

  return { nodes: graphNodes, edges: graphEdges };
}

function Topology({
  mode,
  isMaximized,
  sites,
  peerings,
  nodes,
  gatewayPools,
  gatewayByNode,
  hiddenSites,
  hiddenGatewayPools,
  theme,
  siteCounts,
  poolCounts,
  edgeHealthCheckCounts,
  poolToSite
}: {
  mode: 'sitesAndPools' | 'nodes';
  isMaximized: boolean;
  sites: SiteStatus[];
  peerings: PeeringStatus[];
  nodes: NodeStatus[];
  gatewayPools: GatewayPoolStatus[];
  gatewayByNode: Map<string, string>;
  hiddenSites: Set<string>;
  hiddenGatewayPools: Set<string>;
  theme: 'dark' | 'light';
  siteCounts: Map<string, { online: number; total: number }>;
  poolCounts: Map<string, { online: number; total: number }>;
  edgeHealthCheckCounts: Map<string, { up: number; total: number }>;
  poolToSite: Map<string, string>;
}) {
  const graphRef = useRef<any>(null);
  const topologyWrapperRef = useRef<HTMLDivElement | null>(null);
  const [reagraphModule, setReagraphModule] = useState<ReagraphModule | null>(null);

  useEffect(() => {
    let cancelled = false;
    import('reagraph')
      .then((module) => {
        if (cancelled) return;
        setReagraphModule({
          GraphCanvas: module.GraphCanvas as React.ComponentType<any>,
          darkTheme: module.darkTheme as Record<string, any>,
          lightTheme: module.lightTheme as Record<string, any>
        });
      })
      .catch((error) => {
        console.error('Failed to load topology renderer', error);
      });

    return () => {
      cancelled = true;
    };
  }, []);

  const palette = useMemo(
    () =>
      theme === 'light'
        ? {
          site: '#16a34a',
          siteEmpty: '#6b7280',
          siteWarn: '#facc15',
          siteDanger: '#dc2626',
          pool: '#15803d',
          poolEmpty: '#6b7280',
          poolWarn: '#facc15',
          poolDanger: '#dc2626',
          edge: '#94a3b8',
          edgeDim: '#cbd5e1',
          edgeUp: '#16a34a',
          edgeWarn: '#facc15',
          edgeDanger: '#dc2626',
          nodeHealthy: '#16a34a',
          nodeWarn: '#facc15',
          nodeDanger: '#dc2626',
          label: '#1f2937',
          background: '#ffffff'
        }
        : {
          site: '#1DE9AC',
          siteEmpty: '#6b7280',
          siteWarn: '#facc15',
          siteDanger: '#ef4444',
          pool: '#1DE9AC',
          poolEmpty: '#6b7280',
          poolWarn: '#facc15',
          poolDanger: '#ef4444',
          edge: '#4b5563',
          edgeDim: '#334155',
          edgeUp: '#1DE9AC',
          edgeWarn: '#facc15',
          edgeDanger: '#ef4444',
          nodeHealthy: '#1DE9AC',
          nodeWarn: '#facc15',
          nodeDanger: '#ef4444',
          label: '#e0e0e0',
          background: '#1a1a1a'
        },
    [theme]
  );
  const graphTheme = useMemo(() => {
    if (!reagraphModule) return null;
    const base = theme === 'light' ? reagraphModule.lightTheme : reagraphModule.darkTheme;
    return {
      ...base,
      canvas: {
        ...base.canvas,
        background: palette.background,
        fog: null
      },
      edge: {
        ...base.edge,
        fill: palette.edge,
        activeFill: palette.edge,
        opacity: 1,
        inactiveOpacity: 0.25
      },
      node: {
        ...base.node,
        activeFill: 'rgba(0,0,0,0)',
        inactiveOpacity: 1,
        hoverOpacity: 0,
        label: {
          ...base.node.label,
          color: palette.label,
          activeColor: palette.label,
          fontSize: 16,
          stroke: palette.background,
          strokeColor: palette.background
        }
      }
    };
  }, [palette, reagraphModule, theme]);

  const [hovered, setHovered] = useState<{
    x: number;
    y: number;
    label: string;
    group: string;
    lines: string[];
  } | null>(null);
  const [edgeHovered, setEdgeHovered] = useState<{
    x: number;
    y: number;
    title: string;
    detail: string;
  } | null>(null);
  const [hoveredNodeId, setHoveredNodeId] = useState<string | null>(null);
  const [showZoomHint, setShowZoomHint] = useState(false);
  const [zoomHintRect, setZoomHintRect] = useState<{ left: number; top: number; width: number; height: number } | null>(null);
  const zoomHintTimerRef = useRef<number | null>(null);
  const topologyIconTextures = useTopologyNodeIconTextures();
  const allSiteNames = useMemo(
    () => sites
      .map((site) => (site.name || '').trim())
      .filter((name): name is string => name.length > 0),
    [sites]
  );
  const allPoolNames = useMemo(
    () => gatewayPools
      .map((pool) => (pool.name || '').trim())
      .filter((name): name is string => name.length > 0),
    [gatewayPools]
  );
  const topologySiteCounts = useMemo(() => {
    const counts = new Map<string, { online: number; total: number; warning: number; danger: number }>();
    for (const siteName of allSiteNames) {
      counts.set(siteName, { online: 0, total: 0, warning: 0, danger: 0 });
    }

    if (nodes.length > 0) {
      for (const node of nodes) {
        const siteName = node.nodeInfo?.siteName;
        if (!siteName) continue;
        const nodeName = node.nodeInfo?.name;
        const isGatewayNode = node.nodeInfo?.isGateway || (nodeName ? gatewayByNode.has(nodeName) : false);
        if (isGatewayNode) continue;

        const current = counts.get(siteName) || { online: 0, total: 0, warning: 0, danger: 0 };
        current.total += 1;

        const status = getNodeStatus(node);
        if (status === 'success') {
          current.online += 1;
        } else if (status === 'warning') {
          current.warning += 1;
        } else {
          current.danger += 1;
        }

        counts.set(siteName, current);
      }
    } else {
      // Summary mode: use siteCounts prop (online/total only).
      for (const [siteName, sc] of siteCounts.entries()) {
        const offline = Math.max(0, sc.total - sc.online);
        counts.set(siteName, { online: sc.online, total: sc.total, warning: 0, danger: offline });
      }
    }

    return counts;
  }, [allSiteNames, gatewayByNode, nodes, siteCounts]);
  const topologyPoolCounts = useMemo(() => {
    const counts = new Map<string, { online: number; total: number; warning: number; danger: number }>();

    if (nodes.length > 0) {
      const nodeByName = new Map<string, NodeStatus>();
      for (const node of nodes) {
        const nodeName = node.nodeInfo?.name;
        if (!nodeName) continue;
        nodeByName.set(nodeName, node);
      }

      for (const poolName of allPoolNames) {
        const baseline = poolCounts.get(poolName) || { online: 0, total: 0 };
        const current = { online: 0, total: baseline.total, warning: 0, danger: 0 };

        for (const [nodeName, nodePoolName] of gatewayByNode.entries()) {
          if (nodePoolName !== poolName) continue;
          const node = nodeByName.get(nodeName);
          if (!node) continue;
          const status = getNodeStatus(node);
          if (status === 'success') {
            current.online += 1;
          } else if (status === 'warning') {
            current.warning += 1;
          } else {
            current.danger += 1;
          }
        }

        const accounted = current.online + current.warning + current.danger;
        if (current.total < accounted) {
          current.total = accounted;
        } else if (current.total > accounted) {
          current.danger += current.total - accounted;
        }

        counts.set(poolName, current);
      }
    } else {
      // Summary mode: use poolCounts prop (online/total only).
      for (const poolName of allPoolNames) {
        const pc = poolCounts.get(poolName) || { online: 0, total: 0 };
        const offline = Math.max(0, pc.total - pc.online);
        counts.set(poolName, { online: pc.online, total: pc.total, warning: 0, danger: offline });
      }
    }

    return counts;
  }, [allPoolNames, gatewayByNode, nodes, poolCounts]);

  const sitePoolGraph = useMemo(
    () => buildGraph(
      allSiteNames,
      allPoolNames,
      peerings,
      hiddenSites,
      hiddenGatewayPools,
      new Set(gatewayPools.map((pool) => pool.name).filter((name): name is string => Boolean(name))),
      palette,
      topologySiteCounts,
      topologyPoolCounts,
      edgeHealthCheckCounts,
      poolToSite
    ),
    [allPoolNames, allSiteNames, peerings, gatewayPools, hiddenSites, hiddenGatewayPools, palette, topologySiteCounts, topologyPoolCounts, edgeHealthCheckCounts, poolToSite]
  );
  const nodeGraph = useMemo(
    () => buildNodeGraph(nodes, gatewayByNode, palette, { edgeSize: 1.4 }),
    [nodes, gatewayByNode, palette]
  );
  const graph = mode === 'nodes' ? nodeGraph : sitePoolGraph;
  const dimNodeColor = useCallback((color: string) => {
    const hex = (color || '').trim();
    const match = /^#([0-9a-fA-F]{6})$/.exec(hex);
    if (!match) return palette.edgeDim;
    const value = match[1];
    const r = parseInt(value.slice(0, 2), 16);
    const g = parseInt(value.slice(2, 4), 16);
    const b = parseInt(value.slice(4, 6), 16);
    const mix = (channel: number) => Math.round(channel * 0.35 + 40 * 0.65);
    return `#${mix(r).toString(16).padStart(2, '0')}${mix(g).toString(16).padStart(2, '0')}${mix(b).toString(16).padStart(2, '0')}`;
  }, [palette.edgeDim]);

  const graphNodesForRender = useMemo(() => {
    const baseNodes = mode === 'nodes'
      ? graph.nodes.map((node) => ({ ...node, label: '' }))
      : graph.nodes;

    if (!hoveredNodeId) return baseNodes;
    const connectedNodeIds = new Set<string>([hoveredNodeId]);
    for (const edge of graph.edges) {
      if (edge.source === hoveredNodeId) {
        connectedNodeIds.add(edge.target);
      } else if (edge.target === hoveredNodeId) {
        connectedNodeIds.add(edge.source);
      }
    }

    return baseNodes.map((node) => {
      if (connectedNodeIds.has(node.id)) {
        return node;
      }
      const dimmed = dimNodeColor(node.fill || palette.site);
      return {
        ...node,
        fill: dimmed,
        activeFill: dimmed
      };
    });
  }, [graph.edges, graph.nodes, hoveredNodeId, dimNodeColor, mode, palette.site]);

  const graphEdgesForRender = useMemo(() => {
    if (!hoveredNodeId) return graph.edges;
    return graph.edges.map((edge) => {
      const connected = edge.source === hoveredNodeId || edge.target === hoveredNodeId;
      if (connected) return edge;
      return {
        ...edge,
        fill: palette.edgeDim
      };
    });
  }, [graph.edges, hoveredNodeId, palette.edgeDim]);

  const topologyConfig = useMemo(() => {
    const nodeCount = graph.nodes.length;

    const siteTopologyConfig = {
      minCameraDistance: 2,
      nodeSize: nodeCount <= 6 ? 68 : 50,
      layoutOverrides: {
        radius: 25,
        concentricSpacing: 25
      }
    };

    const nodeTopologyConfig = {
      ...siteTopologyConfig,
      minCameraDistance: 6,

      layoutOverrides: {
        ...siteTopologyConfig.layoutOverrides,
        radius: 25,
        concentricSpacing: 50
      }
    };

    return mode === 'nodes' ? nodeTopologyConfig : siteTopologyConfig;
  }, [graph.nodes.length, mode]);
  const topologyLayoutType = mode === 'nodes' ? 'concentric2d' : 'concentric2d';

  useEffect(() => {
    if (mode !== 'sitesAndPools') return;
    if (!graphRef.current) return;

    const fit = () => graphRef.current?.fitNodesInView();
    const first = requestAnimationFrame(() => {
      const second = requestAnimationFrame(fit);
      (fit as unknown as { _second?: number })._second = second;
    });

    return () => {
      cancelAnimationFrame(first);
      const second = (fit as unknown as { _second?: number })._second;
      if (typeof second === 'number') {
        cancelAnimationFrame(second);
      }
    };
  }, [mode, graph.nodes.length, graph.edges.length]);

  useEffect(() => () => {
    if (zoomHintTimerRef.current !== null) {
      window.clearTimeout(zoomHintTimerRef.current);
      zoomHintTimerRef.current = null;
    }
  }, []);

  const updateZoomHintRect = useCallback(() => {
    const rect = topologyWrapperRef.current?.getBoundingClientRect();
    if (!rect) {
      setZoomHintRect(null);
      return;
    }
    setZoomHintRect({ left: rect.left, top: rect.top, width: rect.width, height: rect.height });
  }, []);

  const showCtrlZoomHint = useCallback(() => {
    updateZoomHintRect();
    setShowZoomHint(true);
    if (zoomHintTimerRef.current !== null) {
      window.clearTimeout(zoomHintTimerRef.current);
    }
    zoomHintTimerRef.current = window.setTimeout(() => {
      setShowZoomHint(false);
      setZoomHintRect(null);
      zoomHintTimerRef.current = null;
    }, 1400);
  }, [updateZoomHintRect]);

  useEffect(() => {
    if (!showZoomHint) {
      return;
    }
    const update = () => updateZoomHintRect();
    window.addEventListener('resize', update, { passive: true });
    window.addEventListener('scroll', update, { passive: true, capture: true });
    document.addEventListener('scroll', update, { passive: true, capture: true });
    return () => {
      window.removeEventListener('resize', update);
      window.removeEventListener('scroll', update, true);
      document.removeEventListener('scroll', update, true);
    };
  }, [showZoomHint, updateZoomHintRect]);

  const handleWheelCapture = useCallback((event: React.WheelEvent<HTMLDivElement>) => {
    if (event.ctrlKey) {
      return;
    }
    event.stopPropagation();
    showCtrlZoomHint();
  }, [showCtrlZoomHint]);

  if (graph.nodes.length === 0) {
    return <div>No peering data available.</div>;
  }

  if (!reagraphModule || !graphTheme) {
    return <div>Loading topology renderer...</div>;
  }

  const GraphCanvas = reagraphModule.GraphCanvas;
  const zoomHintStyle = (() => {
    if (!zoomHintRect) {
      return { left: '50vw', top: '50vh' } as React.CSSProperties;
    }
    return { left: zoomHintRect.left + zoomHintRect.width / 2, top: zoomHintRect.top + zoomHintRect.height / 2 };
  })();
  const zoomHintOverlayStyle = (() => {
    if (!zoomHintRect) {
      return null;
    }
    return { left: zoomHintRect.left, top: zoomHintRect.top, width: zoomHintRect.width, height: zoomHintRect.height };
  })();

  return (
    <div className="topology-card">
      <div
        ref={topologyWrapperRef}
        className="topology-wrapper"
        onWheelCapture={handleWheelCapture}
        style={{
          height: isMaximized ? '100%' : 'auto',
          aspectRatio: isMaximized ? undefined : '1 / 1'
        }}
      >
        <button
          className="topology-reset"
          onClick={() => graphRef.current?.fitNodesInView()}
        >
          Reset
        </button>
        <GraphCanvas
          ref={graphRef}
          nodes={graphNodesForRender}
          edges={graphEdgesForRender}
          draggable={mode === 'sitesAndPools'}
          minDistance={topologyConfig.minCameraDistance}
          theme={graphTheme}
          animated={false}
          layoutType={topologyLayoutType as any}
          layoutOverrides={mode === 'nodes' ? (topologyConfig.layoutOverrides as any) : undefined}
          labelType={'all' as any}
          edgeArrowPosition={'none' as any}
          defaultNodeSize={topologyConfig.nodeSize}
          renderNode={({ node: n, size, opacity }) => {
            const color = n.fill || palette.site;
            const group = (n.data as { group?: string } | undefined)?.group;
            const glyph = getTopologyNodeGlyph(group);
            const material = (
              <meshBasicMaterial
                color={color}
                opacity={opacity}
                transparent={opacity < 1}
              />
            );
            const iconNode = renderTopologyNodeGlyph({
              glyph,
              size,
              color,
              textures: topologyIconTextures,
              workerOffsetScale: -0.16
            });
            if (iconNode) return iconNode;

            return (
              <mesh>
                <sphereGeometry args={[size, 32, 32]} />
                {material}
              </mesh>
            );
          }}
          onNodePointerOver={(node, event) => {
            const tooltipLabel = (node.label || '').trim()
              || (node.id.startsWith('node:')
                ? node.id.slice(5)
                : node.id.startsWith('site:')
                  ? node.id.slice(5)
                  : node.id.startsWith('pool:')
                    ? node.id.slice(5)
                    : node.id);
            setHoveredNodeId(node.id);
            setHovered({
              x: event.clientX + 12,
              y: event.clientY + 12,
              label: tooltipLabel,
              group: (node.data as { group?: string })?.group || 'site',
              lines: (() => {
                const data = node.data as { lines?: string[]; peerings?: string[] } | undefined;
                if (data?.lines && data.lines.length > 0) {
                  return data.lines;
                }
                const peerings = data?.peerings || [];
                return [`Peerings: ${peerings.length > 0 ? peerings.join(', ') : '-'}`];
              })()
            });
          }}
          onNodePointerOut={() => {
            setHovered(null);
            setHoveredNodeId(null);
          }}
          onEdgePointerOver={(edge, event) => {
            if (!event) return;
            setHovered(null);
            setHoveredNodeId(null);
            const data = edge.data as {
              peerings?: string[];
              hcUp?: number;
              hcTotal?: number;
              sourceLabel?: string;
              targetLabel?: string;
            } | undefined;
            const hcDetail = data?.hcTotal && data.hcTotal > 0
              ? `${data.hcUp ?? 0}/${data.hcTotal} links up`
              : 'No health check data';
            const title = mode === 'nodes'
              ? `${data?.sourceLabel || edge.source} <-> ${data?.targetLabel || edge.target}`
              : (data?.peerings?.join(', ') || 'Unknown peering');
            setEdgeHovered({
              x: event.clientX + 12,
              y: event.clientY + 12,
              title,
              detail: hcDetail
            });
          }}
          onEdgePointerOut={() => setEdgeHovered(null)}
          onCanvasPointerOut={() => {
            setHovered(null);
            setHoveredNodeId(null);
            setEdgeHovered(null);
          }}
          onCanvasClick={() => {
            setHovered(null);
            setHoveredNodeId(null);
            setEdgeHovered(null);
          }}
        />
        {hovered && createPortal(
          <div className="topology-tooltip" style={{ left: hovered.x, top: hovered.y }}>
            <div><strong>{hovered.label}</strong></div>
            <div>Type: {
              hovered.group === 'pool'
                ? 'Gateway Pool'
                : hovered.group === 'site'
                  ? 'Site'
                  : hovered.group === 'gateway-node'
                    ? 'Gateway Node'
                    : 'Node'
            }</div>
            {hovered.lines.map((line, index) => (
              <div key={`${hovered.label}-line-${index}`}>{line}</div>
            ))}
          </div>,
          document.body
        )}
        {edgeHovered && createPortal(
          <div className="topology-tooltip" style={{ left: edgeHovered.x, top: edgeHovered.y }}>
            <div><strong>{edgeHovered.title}</strong></div>
            <div>{edgeHovered.detail}</div>
          </div>,
          document.body
        )}
        {showZoomHint && zoomHintOverlayStyle && createPortal(
          <div className="topology-zoom-hint-overlay" style={zoomHintOverlayStyle} />,
          document.body
        )}
        {showZoomHint && createPortal(
          <div className="topology-zoom-hint" style={zoomHintStyle}>
            <span>Hold Ctrl and scroll to zoom graph</span>
          </div>,
          document.body
        )}
      </div>
    </div>
  );
}


export default Topology;
