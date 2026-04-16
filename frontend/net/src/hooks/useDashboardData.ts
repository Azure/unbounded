// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { useMemo } from 'react';
import { ClusterStatus, ClusterSummary, GatewayPoolStatus, NodeStatus, NodeSummary } from '../types';
import { getGatewayPoolNodeNames, isPeerOnline } from '../components/nodes/shared/index';

type DashboardDataParams = {
  summary: ClusterSummary | null;
  status: ClusterStatus | null;
  nodes: NodeStatus[];
  nodeSummaries: NodeSummary[];
  gatewayPools: GatewayPoolStatus[];
  gatewayPoolHiddenNames: Set<string>;
  hiddenSites: Set<string>;
  selectedNodeTypesFilter: Set<string>;
  networkTab: 'siteTopology' | 'matrix';
  maximizedPanel: 'nodes' | 'siteTopology' | 'matrix' | null;
  pullEnabledOptimistic: boolean | null;
  selectedNodeName: string | null;
  nodeDetail: (name: string) => NodeStatus | undefined;
};

function useDashboardData({
  summary,
  status,
  nodes,
  nodeSummaries,
  gatewayPools,
  gatewayPoolHiddenNames,
  hiddenSites,
  selectedNodeTypesFilter,
  networkTab,
  maximizedPanel,
  pullEnabledOptimistic,
  selectedNodeName,
  nodeDetail
}: DashboardDataParams) {
  const gatewayNodeNames = useMemo(() => {
    const names = new Set<string>();
    for (const pool of gatewayPools) {
      for (const gateway of getGatewayPoolNodeNames(pool)) {
        names.add(gateway);
      }
    }
    return names;
  }, [gatewayPools]);

  const gatewayByNode = useMemo(() => {
    const map = new Map<string, string>();
    for (const pool of gatewayPools) {
      if (!pool.name) continue;
      for (const gateway of getGatewayPoolNodeNames(pool)) {
        map.set(gateway, pool.name);
      }
    }
    return map;
  }, [gatewayPools]);

  const nodeK8sStatusMap = useMemo(() => {
    const map = new Map<string, string>();
    for (const ns of nodeSummaries) {
      if (ns.name && ns.k8sReady) {
        map.set(ns.name, ns.k8sReady);
      }
    }
    for (const n of nodes) {
      const name = n.nodeInfo?.name;
      const ready = n.nodeInfo?.k8sReady;
      if (name && ready && !map.has(name)) {
        map.set(name, ready);
      }
    }
    return map;
  }, [nodeSummaries, nodes]);

  // Site counts from full nodes (backward compat) or from summary node summaries
  const siteCounts = useMemo(() => {
    const counts = new Map<string, { online: number; total: number }>();
    if (nodes.length > 0) {
      for (const node of nodes) {
        const siteName = node.nodeInfo?.siteName;
        if (!siteName) continue;
        const nodeName = node.nodeInfo?.name;
        const isGatewayNode = node.nodeInfo?.isGateway || (nodeName ? gatewayNodeNames.has(nodeName) : false);
        if (isGatewayNode) continue;
        const current = counts.get(siteName) || { online: 0, total: 0 };
        current.total += 1;
        if (node.nodeInfo?.wireGuard?.interface) {
          current.online += 1;
        }
        counts.set(siteName, current);
      }
    } else {
      for (const ns of nodeSummaries) {
        const siteName = ns.siteName;
        if (!siteName) continue;
        const nodeName = ns.name || '';
        const isGatewayNode = ns.isGateway || gatewayNodeNames.has(nodeName);
        if (isGatewayNode) continue;
        const current = counts.get(siteName) || { online: 0, total: 0 };
        current.total += 1;
        if (ns.cniTone !== 'danger' && ns.cniStatus !== 'Unknown') {
          current.online += 1;
        }
        counts.set(siteName, current);
      }
    }
    return counts;
  }, [nodes, nodeSummaries, gatewayNodeNames]);

  const poolCounts = useMemo(() => {
    const counts = new Map<string, { online: number; total: number }>();
    if (nodes.length > 0) {
      const nodeByName = new Map<string, NodeStatus>();
      for (const node of nodes) {
        const name = node.nodeInfo?.name;
        if (!name) continue;
        nodeByName.set(name, node);
      }
      for (const pool of gatewayPools) {
        const poolName = pool.name || '';
        if (!poolName) continue;
        const gwNames = getGatewayPoolNodeNames(pool);
        let online = 0;
        for (const gwName of gwNames) {
          const node = nodeByName.get(gwName);
          if (!node) continue;
          if (node.nodeInfo?.wireGuard?.interface) {
            online += 1;
          }
        }
        const expectedTotal = Math.max(
          typeof pool.nodeCount === 'number' ? pool.nodeCount : 0,
          gwNames.length,
        );
        counts.set(poolName, { online, total: expectedTotal });
      }
    } else {
      const nsByName = new Map<string, NodeSummary>();
      for (const ns of nodeSummaries) {
        if (ns.name) nsByName.set(ns.name, ns);
      }
      for (const pool of gatewayPools) {
        const poolName = pool.name || '';
        if (!poolName) continue;
        const gwNames = getGatewayPoolNodeNames(pool);
        let online = 0;
        for (const gwName of gwNames) {
          const ns = nsByName.get(gwName);
          if (!ns) continue;
          if (ns.cniTone !== 'danger' && ns.cniStatus !== 'Unknown') {
            online += 1;
          }
        }
        const expectedTotal = Math.max(
          typeof pool.nodeCount === 'number' ? pool.nodeCount : 0,
          gwNames.length,
        );
        counts.set(poolName, { online, total: expectedTotal });
      }
    }
    return counts;
  }, [gatewayPools, nodes, nodeSummaries]);

  const edgeHealthCheckCounts = useMemo(() => {
    const nodeToEntity = new Map<string, string>();
    const nodeNameSet = new Set<string>();

    // Build node-to-entity mapping from full nodes or summaries
    const nodeSource = nodes.length > 0
      ? nodes.map((n) => ({ name: n.nodeInfo?.name, siteName: n.nodeInfo?.siteName }))
      : nodeSummaries.map((ns) => ({ name: ns.name, siteName: ns.siteName }));
    for (const node of nodeSource) {
      const name = node.name;
      if (!name) continue;
      nodeNameSet.add(name);
      const poolName = gatewayByNode.get(name);
      if (poolName) {
        nodeToEntity.set(name, `pool:${poolName}`);
      } else if (node.siteName) {
        nodeToEntity.set(name, `site:${node.siteName}`);
      }
    }

    const counts = new Map<string, { up: number; total: number }>();

    if (nodes.length > 0) {
      // Full nodes available: use peer-level health check data
      for (const node of nodes) {
        const name = node.nodeInfo?.name;
        if (!name) continue;
        const srcEntity = nodeToEntity.get(name);
        if (!srcEntity) continue;
        for (const peer of node.peers || []) {
          if (!peer.healthCheck?.enabled && !peer.healthCheck) continue;
          const peerSite = peer.siteName;
          if (!peerSite) continue;
          let dstEntity: string | undefined;
          if (peer.name) {
            if (!nodeNameSet.has(peer.name)) continue;
            dstEntity = nodeToEntity.get(peer.name);
          }
          if (!dstEntity) dstEntity = `site:${peerSite}`;
          if (srcEntity === dstEntity) continue;
          const edgeKey = srcEntity < dstEntity
            ? `${srcEntity}|${dstEntity}`
            : `${dstEntity}|${srcEntity}`;
          const current = counts.get(edgeKey) || { up: 0, total: 0 };
          current.total++;
          const rawStatus = (peer.healthCheck?.status || '').trim().toLowerCase();
          if (rawStatus === 'up') current.up++;
          counts.set(edgeKey, current);
        }
      }
    } else {
      // Summary mode: derive counts from connectivity matrix
      const matrix = summary?.connectivityMatrix;
      if (matrix) {
        for (const [, siteMatrix] of Object.entries(matrix)) {
          const results = siteMatrix?.results || {};
          for (const [src, row] of Object.entries(results)) {
            const srcEntity = nodeToEntity.get(src);
            if (!srcEntity) continue;
            for (const [dst, cellStatus] of Object.entries(row || {})) {
              if (src >= dst) continue; // count each pair once
              const dstEntity = nodeToEntity.get(dst);
              if (!dstEntity || srcEntity === dstEntity) continue;
              const edgeKey = srcEntity < dstEntity
                ? `${srcEntity}|${dstEntity}`
                : `${dstEntity}|${srcEntity}`;
              const current = counts.get(edgeKey) || { up: 0, total: 0 };
              current.total++;
              const status = (typeof cellStatus === 'string' ? cellStatus : '').trim().toLowerCase();
              if (status === 'up') current.up++;
              counts.set(edgeKey, current);
            }
          }
        }
      }
    }
    return counts;
  }, [nodes, nodeSummaries, gatewayByNode, summary]);

  const poolToSite = useMemo(() => {
    const map = new Map<string, string>();
    for (const pool of gatewayPools) {
      if (pool.name && pool.siteName) {
        map.set(pool.name, pool.siteName);
      }
    }
    return map;
  }, [gatewayPools]);

  // Visible full nodes (for NetworkCard and other components needing full NodeStatus)
  const visibleNodes = useMemo(() => {
    return nodes.filter((node) => {
      const nodeName = node.nodeInfo?.name || '';
      const isGatewayNode = node.nodeInfo?.isGateway || gatewayByNode.has(nodeName);
      const nodeType = isGatewayNode ? 'Gateway' : 'Worker';
      if (!selectedNodeTypesFilter.has(nodeType)) {
        return false;
      }
      const poolName = gatewayByNode.get(nodeName);
      if (poolName) {
        return !gatewayPoolHiddenNames.has(poolName);
      }
      const siteName = node.nodeInfo?.siteName;
      if (!siteName) return true;
      return !hiddenSites.has(siteName);
    });
  }, [nodes, gatewayByNode, selectedNodeTypesFilter, gatewayPoolHiddenNames, hiddenSites]);

  // Visible node summaries (for NodesTable)
  const visibleNodeSummaries = useMemo(() => {
    return nodeSummaries.filter((ns) => {
      const nodeName = ns.name || '';
      const isGatewayNode = ns.isGateway || gatewayByNode.has(nodeName);
      const nodeType = isGatewayNode ? 'Gateway' : 'Worker';
      if (!selectedNodeTypesFilter.has(nodeType)) {
        return false;
      }
      const poolName = gatewayByNode.get(nodeName);
      if (poolName) {
        return !gatewayPoolHiddenNames.has(poolName);
      }
      const siteName = ns.siteName;
      if (!siteName) return true;
      return !hiddenSites.has(siteName);
    });
  }, [nodeSummaries, gatewayByNode, selectedNodeTypesFilter, gatewayPoolHiddenNames, hiddenSites]);

  // Node healthy/total counts: prefer summary data
  const nodeHealthyCount = useMemo(() => {
    if (nodeSummaries.length > 0) {
      return nodeSummaries.filter((ns) => ns.cniTone === 'success').length;
    }
    return nodes.filter((node) => node.nodeInfo?.wireGuard?.interface).length;
  }, [nodes, nodeSummaries]);

  const nodeTotalCount = nodeSummaries.length || nodes.length || summary?.nodeCount || status?.nodeCount || 0;

  // Peer health: prefer summary aggregation
  const peerHealth = useMemo(() => {
    if (nodeSummaries.length > 0) {
      let healthy = 0;
      let total = 0;
      for (const ns of nodeSummaries) {
        total += ns.peerCount || 0;
        healthy += ns.healthyPeers || 0;
      }
      return { healthy, total };
    }
    let healthy = 0;
    let total = 0;
    for (const node of nodes) {
      for (const peer of node.peers || []) {
        total += 1;
        if (isPeerOnline(peer)) {
          healthy += 1;
        }
      }
    }
    return { healthy, total };
  }, [nodes, nodeSummaries]);

  const activeNetworkTab = maximizedPanel === 'siteTopology' || maximizedPanel === 'matrix'
    ? maximizedPanel
    : networkTab;

  const effectivePullEnabled = pullEnabledOptimistic ?? Boolean(summary?.pullEnabled ?? status?.pullEnabled);

  // Active selected node detail from the cache
  const activeSelectedNode = useMemo<NodeStatus | null>(() => {
    if (!selectedNodeName) return null;
    const detail = nodeDetail(selectedNodeName);
    if (detail) return detail;
    // Backward compat: check full nodes array
    return nodes.find((node) => node.nodeInfo?.name === selectedNodeName) || null;
  }, [selectedNodeName, nodeDetail, nodes]);

  return {
    activeNetworkTab,
    activeSelectedNode,
    edgeHealthCheckCounts,
    effectivePullEnabled,
    gatewayByNode,
    nodeK8sStatusMap,
    nodeHealthyCount,
    nodeTotalCount,
    peerHealth,
    poolCounts,
    poolToSite,
    siteCounts,
    visibleNodes,
    visibleNodeSummaries
  };
}

export default useDashboardData;
