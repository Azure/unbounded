// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

import type { ClusterStatus, ClusterStatusDelta, ClusterSummary, ClusterSummaryDelta, NodeStatus, NodeSummary } from '../types';

export function isSummaryOnline(node: NodeSummary): boolean {
  // Older summary servers lacked interface metadata; retain their established
  // fallback while full-response projection preserves actual interface counts.
  return node.wireGuardOnline ?? (node.cniTone !== 'danger' && node.cniStatus !== 'Unknown');
}

function cniState(source?: string, fetchError?: string, errorCount = 0, routeMismatch = false) {
  const [cniStatus, cniTone] = source === 'no-data' ? ['No data', 'warning']
    : fetchError ? ['Fetch error', 'danger']
    : errorCount ? ['Errors', 'danger']
    : routeMismatch ? ['Route mismatch', 'warning']
    : source === 'stale' || source === 'error' ? ['Stale', 'warning']
    : !source ? ['Unknown', 'warning'] : ['Healthy', 'success'];
  return { cniStatus, cniTone };
}

export function summarizeNode(node: NodeStatus, now = Date.now()): NodeSummary {
  const peers = node.peers || [];
  const healthyPeers = peers.filter((peer) => peer.healthCheck?.enabled
    ? peer.healthCheck.status?.toLowerCase() === 'up'
    : Boolean(peer.tunnel?.lastHandshake && now - Date.parse(peer.tunnel.lastHandshake) < 180000)).length;
  const routeMismatch = (node.routingTable?.routes || []).some((route) =>
    (route.nextHops || []).some((hop) => (hop.expected === true) !== (hop.present === true)));
  const errorCount = node.nodeErrors?.length || 0;
  const source = node.statusSource;
  return {
    name: node.nodeInfo?.name,
    siteName: node.nodeInfo?.siteName,
    isGateway: node.nodeInfo?.isGateway,
    k8sReady: node.nodeInfo?.k8sReady,
    statusSource: source,
    ...cniState(source, node.fetchError, errorCount, routeMismatch), errorCount,
    firstError: node.nodeErrors?.[0]?.message,
    peerCount: peers.length,
    healthyPeers,
    routeCount: node.routingTable?.routes?.length || 0,
    routeMismatch,
    fetchError: node.fetchError,
    wireGuardOnline: Boolean(node.nodeInfo?.wireGuard?.interface),
  };
}

// Whitelist wire fields, including for already-summary input. Never spread full
// cluster/node objects into persistent state or the global JSON export.
function summaryNode(node: NodeSummary): NodeSummary {
  return {
    name: node.name, siteName: node.siteName, isGateway: node.isGateway,
    k8sReady: node.k8sReady, statusSource: node.statusSource,
    cniStatus: node.cniStatus, cniTone: node.cniTone,
    errorCount: node.errorCount, firstError: node.firstError,
    peerCount: node.peerCount, healthyPeers: node.healthyPeers,
    routeCount: node.routeCount, routeMismatch: node.routeMismatch,
    fetchError: node.fetchError, wireGuardOnline: node.wireGuardOnline,
  };
}

export function toClusterSummary(input: ClusterSummary | ClusterStatus, now = Date.now()): ClusterSummary {
  const summary = input as ClusterSummary;
  return {
    seq: summary.seq, timestamp: input.timestamp,
    nodeCount: input.nodeCount, siteCount: input.siteCount,
    azureTenantId: input.azureTenantId, leaderInfo: input.leaderInfo,
    buildInfo: input.buildInfo, sites: input.sites,
    gatewayPools: input.gatewayPools, peerings: input.peerings,
    errors: input.errors, warnings: input.warnings, problems: input.problems,
    pullEnabled: input.pullEnabled,
    nodeSummaries: summary.nodeSummaries != null
      ? summary.nodeSummaries.map(summaryNode)
      : ((input as ClusterStatus).nodes || []).map((node) => summarizeNode(node, now)),
  };
}

export function mergeSummary(
  current: ClusterSummary | null, delta: ClusterSummaryDelta
): ClusterSummary | null {
  if (!current) return null;
  if (current.seq != null && delta.seq != null && delta.seq <= current.seq) return current;
  const metadata = toClusterSummary(delta);
  const merged = { ...current };
  for (const key of Object.keys(metadata) as (keyof ClusterSummary)[]) {
    if (key !== 'nodeSummaries' && metadata[key] !== undefined) {
      Object.assign(merged, { [key]: metadata[key] });
    }
  }
  if (delta.nodeSummaries || delta.removedNodes) {
    const nodes = new Map((current.nodeSummaries || []).map((node) => [node.name, node]));
    for (const name of delta.removedNodes || []) nodes.delete(name);
    for (const node of delta.nodeSummaries || []) nodes.set(node.name, summaryNode(node));
    merged.nodeSummaries = [...nodes.values()].sort((a, b) => (a.name || '').localeCompare(b.name || ''));
  }
  return merged;
}

export function mergeLegacySummary(current: ClusterSummary | null, delta: ClusterStatusDelta): ClusterSummary | null {
  if (delta.nodes) return toClusterSummary({ ...current, ...delta, nodeSummaries: undefined } as ClusterStatus);
  // Legacy updatedNodes contains changed top-level fields. Preserve summary
  // counts when a patch omits the corresponding full array.
  const previous = new Map((current?.nodeSummaries || []).map((node) => [node.name, node]));
  const nodeSummaries = (delta.updatedNodes || []).map((node) => {
    const next = summarizeNode(node);
    const old = previous.get(next.name);
    if (!old) return next;
    const merged = {
      ...old, ...next,
      siteName: node.nodeInfo ? next.siteName : old.siteName,
      isGateway: node.nodeInfo ? next.isGateway : old.isGateway,
      k8sReady: node.nodeInfo ? next.k8sReady : old.k8sReady,
      wireGuardOnline: node.nodeInfo ? next.wireGuardOnline : old.wireGuardOnline,
      peerCount: node.peers ? next.peerCount : old.peerCount,
      healthyPeers: node.peers ? next.healthyPeers : old.healthyPeers,
      routeCount: node.routingTable ? next.routeCount : old.routeCount,
      routeMismatch: node.routingTable ? next.routeMismatch : old.routeMismatch,
      errorCount: node.nodeErrors ? next.errorCount : old.errorCount,
      firstError: node.nodeErrors ? next.firstError : old.firstError,
      statusSource: node.statusSource ?? old.statusSource,
      fetchError: node.fetchError ?? old.fetchError,
    };
    return { ...merged, ...cniState(merged.statusSource, merged.fetchError, merged.errorCount, merged.routeMismatch) };
  });
  return mergeSummary(current, { ...toClusterSummary(delta), nodeSummaries, removedNodes: delta.removedNodes });
}
