// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

import { useMemo } from 'react';
import type { ClusterSummary, GatewayPoolStatus, NodeSummary } from '../types';
import { getGatewayPoolNodeNames } from '../components/nodes/shared/index';
import { isSummaryOnline } from '../state/clusterSummary';

type DashboardDataParams = {
  summary: ClusterSummary | null;
  nodeSummaries: NodeSummary[];
  gatewayPools: GatewayPoolStatus[];
  gatewayPoolHiddenNames: Set<string>;
  hiddenSites: Set<string>;
  selectedNodeTypesFilter: Set<string>;
  pullEnabledOptimistic: boolean | null;
};

function useDashboardData({
  summary, nodeSummaries, gatewayPools, gatewayPoolHiddenNames,
  hiddenSites, selectedNodeTypesFilter, pullEnabledOptimistic,
}: DashboardDataParams) {
  const gatewayByNode = useMemo(() => {
    const map = new Map<string, string>();
    for (const pool of gatewayPools) {
      if (!pool.name) continue;
      for (const gateway of getGatewayPoolNodeNames(pool)) map.set(gateway, pool.name);
    }
    return map;
  }, [gatewayPools]);

  const nodeK8sStatusMap = useMemo(() => {
    const map = new Map<string, string>();
    for (const node of nodeSummaries) {
      if (node.name && node.k8sReady) map.set(node.name, node.k8sReady);
    }
    return map;
  }, [nodeSummaries]);

  const siteCounts = useMemo(() => {
    const counts = new Map<string, { online: number; total: number }>();
    for (const node of nodeSummaries) {
      if (!node.siteName || node.isGateway || gatewayByNode.has(node.name || '')) continue;
      const current = counts.get(node.siteName) || { online: 0, total: 0 };
      current.total++;
      if (isSummaryOnline(node)) current.online++;
      counts.set(node.siteName, current);
    }
    return counts;
  }, [nodeSummaries, gatewayByNode]);

  const poolCounts = useMemo(() => {
    const counts = new Map<string, { online: number; total: number }>();
    const byName = new Map(nodeSummaries.map((node) => [node.name, node]));
    for (const pool of gatewayPools) {
      if (!pool.name) continue;
      const names = getGatewayPoolNodeNames(pool);
      const online = names.filter((name) => {
        const node = byName.get(name);
        return node && isSummaryOnline(node);
      }).length;
      counts.set(pool.name, { online, total: Math.max(pool.nodeCount || 0, names.length) });
    }
    return counts;
  }, [gatewayPools, nodeSummaries]);

  const visibleNodeSummaries = useMemo(() => nodeSummaries.filter((node) => {
    const name = node.name || '';
    const isGateway = node.isGateway || gatewayByNode.has(name);
    if (!selectedNodeTypesFilter.has(isGateway ? 'Gateway' : 'Worker')) return false;
    const pool = gatewayByNode.get(name);
    if (pool) return !gatewayPoolHiddenNames.has(pool);
    return !node.siteName || !hiddenSites.has(node.siteName);
  }), [nodeSummaries, gatewayByNode, selectedNodeTypesFilter, gatewayPoolHiddenNames, hiddenSites]);

  const nodeHealthyCount = useMemo(() =>
    nodeSummaries.filter((node) => node.cniTone === 'success').length, [nodeSummaries]);
  const peerHealth = useMemo(() => nodeSummaries.reduce((counts, node) => ({
    healthy: counts.healthy + (node.healthyPeers || 0),
    total: counts.total + (node.peerCount || 0),
  }), { healthy: 0, total: 0 }), [nodeSummaries]);

  return {
    effectivePullEnabled: pullEnabledOptimistic ?? Boolean(summary?.pullEnabled),
    gatewayByNode, nodeK8sStatusMap, nodeHealthyCount,
    nodeTotalCount: nodeSummaries.length || summary?.nodeCount || 0,
    peerHealth, poolCounts, siteCounts, visibleNodeSummaries,
  };
}

export default useDashboardData;
