// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { GatewayPoolStatus, NodeStatus, WireguardPeer } from '../../../types';

const cniProblemNodeErrorTypes = new Set([
  'mtuMismatch',
  'configIPv4ForwardingDisabled',
  'configIPv6ForwardingDisabled',
  'configIptablesForwardDrop',
  'configIp6tablesForwardDrop',
  'configRPFilterStrict'
]);

function formatTime(dateStr?: string) {
  if (!dateStr) return 'Never';
  const date = new Date(dateStr);
  if (Number.isNaN(date.getTime()) || date.getTime() === 0) return 'Never';
  const diff = Date.now() - date.getTime();
  if (diff < 60000) return `${Math.floor(diff / 1000)}s ago`;
  if (diff < 3600000) return `${Math.floor(diff / 60000)}m ago`;
  if (diff < 86400000) return `${Math.floor(diff / 3600000)}h ago`;
  return `${Math.floor(diff / 86400000)}d ago`;
}

function formatBytes(value?: number) {
  const bytes = Number(value || 0);
  if (!Number.isFinite(bytes) || bytes <= 0) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let index = 0;
  let size = bytes;
  while (size >= 1024 && index < units.length - 1) {
    size /= 1024;
    index += 1;
  }
  const precision = size >= 100 || index === 0 ? 0 : 1;
  return `${size.toFixed(precision)} ${units[index]}`;
}

function formatDateAndAge(dateStr?: string) {
  if (!dateStr) return { absolute: 'Never', age: 'Never' };
  const date = new Date(dateStr);
  if (Number.isNaN(date.getTime()) || date.getTime() === 0) {
    return { absolute: 'Never', age: 'Never' };
  }
  return {
    absolute: date.toLocaleString(),
    age: formatTime(dateStr)
  };
}

function getPeerHealthCheckState(peer: WireguardPeer): { enabled: boolean; status: 'up' | 'down' | 'mixed' | '-' } {
  const rawStatus = (peer.healthCheck?.status || '').trim().toLowerCase();
  const enabled = peer.healthCheck?.enabled === true || (peer.healthCheck?.enabled !== false && rawStatus.length > 0);
  if (!enabled) {
    return { enabled: false, status: '-' };
  }
  if (rawStatus === 'up') {
    return { enabled: true, status: 'up' };
  }
  if (rawStatus === 'mixed') {
    return { enabled: true, status: 'mixed' };
  }
  return { enabled: true, status: 'down' };
}

function isPeerOnline(peer: WireguardPeer): boolean {
  const hcState = getPeerHealthCheckState(peer);
  if (hcState.enabled) return hcState.status === 'up';
  // Without health check, fall back to a recent handshake (within 3 minutes)
  if (peer.tunnel?.lastHandshake) {
    const hs = new Date(peer.tunnel.lastHandshake);
    return !Number.isNaN(hs.getTime()) && hs.getTime() > 0 && (Date.now() - hs.getTime()) < 180000;
  }
  return false;
}

function getPeerCounts(peers: WireguardPeer[], filter?: (peer: WireguardPeer) => boolean) {
  const filtered = filter ? peers.filter(filter) : peers;
  const total = filtered.length;
  const online = filtered.filter((peer) => isPeerOnline(peer)).length;
  return { online, total };
}

function getRouteMismatchCount(node: NodeStatus): number {
  const allRoutes = node.routingTable?.routes || [];

  let mismatch = 0;
  for (const route of allRoutes) {
    for (const hop of route.nextHops || []) {
      const hopExpected = hop.expected === true;
      const hopPresent = hop.present === true;
      if (hopExpected !== hopPresent) {
        mismatch += 1;
      }
    }
  }

  return mismatch;
}

function getCountColor(online: number, total: number) {
  if (total === 0) return 'var(--text-muted)';
  if (online === 0) return 'var(--status-danger)';
  if (online < total) return 'var(--status-warning)';
  return 'var(--status-success)';
}

function getCniStatus(node: NodeStatus, pullEnabled?: boolean) {
  if (getRouteMismatchCount(node) > 0) {
    return { label: 'Route mismatch', tone: 'warning' as const };
  }

  const pullMode = Boolean(pullEnabled);
  if (node.statusSource === 'apiserver-push' || node.statusSource === 'apiserver-ws') {
    return { label: 'Fallback', tone: 'warning' as const };
  }
  if (node.statusSource === 'push' || node.statusSource === 'ws') {
    return { label: 'Healthy', tone: 'success' as const };
  }
  if (pullMode) {
    if (node.fetchError) {
      return { label: 'Pull failed', tone: 'danger' as const };
    }
    if (node.statusSource === 'stale-cache') {
      return { label: 'Pull failed', tone: 'danger' as const };
    }
    if (node.statusSource === 'error') {
      return { label: 'Pull failed', tone: 'danger' as const };
    }
    if (!node.lastPushTime) {
      return { label: 'Pull failed', tone: 'danger' as const };
    }
    if (node.statusSource === 'pull') {
      return { label: 'Pulling', tone: 'warning' as const };
    }
    return { label: 'Pull failed', tone: 'danger' as const };
  }
  if (node.statusSource === 'stale-cache') {
    return { label: 'Stale', tone: 'warning' as const };
  }
  if (node.statusSource === 'error') {
    return { label: 'No data', tone: 'danger' as const };
  }
  return { label: 'Stale', tone: 'warning' as const };
}

function getNodeCniProblemErrors(node: NodeStatus) {
  const errors = (node.nodeErrors || [])
    .filter((entry) => cniProblemNodeErrorTypes.has((entry.type || '').trim()))
    .map((entry) => ({
      type: (entry.type || '').trim(),
      message: (entry.message || '').trim()
    }))
    .filter((entry) => entry.type.length > 0);
  return errors;
}

function hasNodeCniProblems(node: NodeStatus) {
  return getNodeCniProblemErrors(node).length > 0;
}

function getNodeCniProblemsTooltip(node: NodeStatus) {
  const messages = getNodeCniProblemMessages(node);
  if (messages.length === 0) {
    return undefined;
  }
  return `Node reported CNI configuration problems:\n${messages.map((entry) => `- ${entry}`).join('\n')}`;
}

function getNodeCniProblemMessages(node: NodeStatus) {
  return getNodeCniProblemErrors(node).map((entry) => entry.message || entry.type);
}

function getCniStatusTooltip(node: NodeStatus, pullEnabled?: boolean) {
  if (node.statusSource === 'apiserver-push' || node.statusSource === 'apiserver-ws') {
    const summary = 'Direct communication between the agent and the controller failed; the API server fallback path is being used.';
    const errors = (node.nodeErrors || [])
      .map((entry) => (entry.message || '').trim())
      .filter((entry) => entry.length > 0);
    if (errors.length === 0) {
      return summary;
    }
    return `${summary}\nDirect push errors:\n${errors.map((entry) => `- ${entry}`).join('\n')}`;
  }

  if (node.fetchError) {
    return node.fetchError;
  }

  const status = getCniStatus(node, pullEnabled);
  if (status.label === 'Route mismatch') {
    return 'Route validation detected mismatched expected/present tunnel routes.';
  }

  return undefined;
}

function getCniFilterCategory(node: NodeStatus, pullEnabled?: boolean) {
  if (getRouteMismatchCount(node) > 0) {
    return 'Route mismatch';
  }
  if (node.statusSource === 'ws') {
    return 'Live';
  }
  if (node.statusSource === 'push') {
    return 'Health via push';
  }
  if (node.statusSource === 'apiserver-push' || node.statusSource === 'apiserver-ws') {
    return 'Fallback';
  }
  return getCniStatus(node, pullEnabled).label;
}

function getNodeStatus(node: NodeStatus) {
  if (getRouteMismatchCount(node) > 0) return 'warning';
  if (node.statusSource === 'apiserver-push' || node.statusSource === 'apiserver-ws') return 'warning';
  if (node.nodeInfo?.wireGuard?.interface) return 'success';
  if (node.statusSource === 'error') return 'warning';
  return 'danger';
}

function getNodeTypeFromProviderId(providerId?: string): { label: 'VM' | 'VMSS' | 'Unknown'; title: string } {
  const value = (providerId || '').trim();
  if (!value) {
    return { label: 'Unknown', title: 'No provider ID is present' };
  }
  if (/virtualmachinescalesets\//i.test(value)) {
    return { label: 'VMSS', title: value };
  }
  if (/virtualmachines\//i.test(value)) {
    return { label: 'VM', title: value };
  }
  return { label: 'Unknown', title: value };
}

function getGatewayPoolNodeNames(pool: GatewayPoolStatus): string[] {
  const nodeNames = (pool.nodes || [])
    .map((node) => node.name)
    .filter((name): name is string => Boolean(name));
  if (nodeNames.length > 0) {
    return nodeNames;
  }
  return (pool.gateways || []).filter((name): name is string => Boolean(name));
}

function getGatewayPoolBadgeTone(total: number, online: number): 'success' | 'warning' | 'danger' | '' {
  if (total === 0) return '';
  if (online === 0) return 'danger';
  if (online < total) return 'warning';
  return 'success';
}

export {
  formatBytes,
  formatDateAndAge,
  formatTime,
  getCniFilterCategory,
  getCniStatus,
  getCniStatusTooltip,
  getCountColor,
  getGatewayPoolBadgeTone,
  getGatewayPoolNodeNames,
  getNodeCniProblemMessages,
  getNodeCniProblemsTooltip,
  getNodeStatus,
  getNodeTypeFromProviderId,
  hasNodeCniProblems,
  getPeerHealthCheckState,
  getPeerCounts,
  getRouteMismatchCount,
  isPeerOnline
};
