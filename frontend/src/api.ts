// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { ClusterStatus, ClusterStatusDelta, ClusterSummary, ClusterSummaryDelta, NodeStatus } from './types';

export type StatusEvent = {
  type: 'cluster_status' | 'cluster_status_delta' | 'cluster_summary' | 'cluster_summary_delta' | 'node_detail_response' | 'node_detail_update';
  data: ClusterStatusDelta | ClusterStatus | ClusterSummary | ClusterSummaryDelta | NodeStatus;
  nodeName?: string;
};

function buildControllerUrl(path: string): string {
  // Always use relative URLs -- the frontend is served by the controller,
  // so the browser already knows the correct origin.
  return path;
}

export async function fetchClusterStatus(): Promise<ClusterStatus> {
  const url = buildControllerUrl('/status/json');
  try {
    const res = await fetch(url);
    if (!res.ok) {
      let details = '';
      try {
        details = await res.text();
      } catch {
        details = '';
      }
      const suffix = details ? `: ${details}` : '';
      throw new Error(`Status request failed (${res.status} ${res.statusText})${suffix}`);
    }
    return res.json();
  } catch (err) {
    if (err instanceof TypeError) {
      throw new Error(
        `Failed to fetch cluster status from ${url}.`
      );
    }
    throw err;
}

}


export function mergeDelta(current: ClusterStatus | null, delta: ClusterStatusDelta): ClusterStatus {
  if (!current) {
    return delta as ClusterStatus;
  }
  const merged: ClusterStatus = { ...current };
  merged.timestamp = delta.timestamp ?? merged.timestamp;
  merged.nodeCount = delta.nodeCount ?? merged.nodeCount;
  merged.siteCount = delta.siteCount ?? merged.siteCount;
  merged.azureTenantId = delta.azureTenantId ?? merged.azureTenantId;
  merged.buildInfo = delta.buildInfo ?? merged.buildInfo;
  merged.leaderInfo = delta.leaderInfo ?? merged.leaderInfo;
  merged.errors = delta.errors ?? merged.errors;
  merged.warnings = delta.warnings ?? merged.warnings;
  merged.problems = delta.problems ?? merged.problems;
  merged.sites = delta.sites ?? merged.sites;
  merged.gatewayPools = delta.gatewayPools ?? merged.gatewayPools;
  merged.peerings = delta.peerings ?? merged.peerings;
  merged.pullEnabled = delta.pullEnabled ?? merged.pullEnabled;

  if (delta.nodes) {
    // Allow full-node snapshots to replace local state directly.
    merged.nodes = delta.nodes;
  } else {
    const nodeMap: Record<string, NodeStatus> = {};
    for (const node of current.nodes || []) {
      const name = node.nodeInfo?.name;
      if (name) {
        nodeMap[name] = node;
      }
    }

    for (const name of delta.removedNodes || []) {
      delete nodeMap[name];
    }

    for (const updated of delta.updatedNodes || []) {
      const name = updated.nodeInfo?.name;
      if (!name) continue;
      if (nodeMap[name]) {
        nodeMap[name] = { ...nodeMap[name], ...updated };
      } else {
        nodeMap[name] = updated;
      }
    }

    merged.nodes = Object.values(nodeMap);
  }

  if (delta.connectivityMatrix !== undefined && delta.connectivityMatrix !== null) {
    merged.connectivityMatrix = delta.connectivityMatrix || undefined;
  }

  return merged;
}

export function connectWebSocket(
  onMessage: (event: StatusEvent) => void,
  onOpen: () => void,
  onClose: () => void
): WebSocket | null {
  // Always use relative URLs -- the frontend is served by the controller.
  const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
  const wsUrl = protocol + '//' + window.location.host + '/status/ws';
  try {
    const ws = new WebSocket(wsUrl);
    ws.onmessage = (event) => {
      try {
        const msg = JSON.parse(event.data);
        const type = msg?.type;
        if (type === 'cluster_status' || type === 'cluster_status_delta' ||
            type === 'cluster_summary' || type === 'cluster_summary_delta' || type === 'node_detail_response' || type === 'node_detail_update') {
          onMessage({ type, data: msg.data ?? msg, nodeName: msg.nodeName } as StatusEvent);
        }
      } catch {
        return;
      }
    };
    ws.onopen = onOpen;
    ws.onclose = onClose;
    ws.onerror = () => {
      try {
        ws.close();
      } catch {
        return;
      }
    };
    return ws;
  } catch {
    return null;
  }
}
