// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

import type { ClusterStatus, ClusterStatusDelta, ClusterSummary, ClusterSummaryDelta, NodeStatus, NodeDetailResult } from './types';

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

async function fetchNodeDetails(path: string, options: RequestInit): Promise<NodeDetailResult> {
  const response = await fetch(buildControllerUrl(path), { credentials: 'same-origin', ...options });
  const text = await response.text();
  let result: NodeDetailResult;
  try {
    result = JSON.parse(text);
  } catch {
    throw new Error(`Detail request failed (${response.status} ${response.statusText})${text ? `: ${text}` : ''}`);
  }
  if (!response.ok) {
    // Lifecycle failures intentionally use 410/404/503; retain their state so
    // expiry isn't presented as a generic network failure.
    if (!result?.nodeName || !['expired', 'unavailable', 'retryable'].includes(result.state)) {
      throw new Error(result?.error || `Detail request failed (${response.status} ${response.statusText})`);
    }
  }
  if (!result || typeof result.state !== 'string') {
    throw new Error('Invalid detail response from controller');
  }
  return result;
}

export function requestNodeDetails(name: string, forceRefresh: boolean, signal: AbortSignal) {
  return fetchNodeDetails(`/status/node/${encodeURIComponent(name)}/details`, {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ forceRefresh }), signal,
  });
}

export function pollNodeDetails(name: string, requestId: string, signal: AbortSignal) {
  return fetchNodeDetails(`/status/node/${encodeURIComponent(name)}/details?requestId=${encodeURIComponent(requestId)}`, {
    signal, cache: 'no-store',
  });
}

export async function fetchClusterStatus(signal?: AbortSignal): Promise<ClusterSummary | ClusterStatus> {
  const url = buildControllerUrl('/status/json');
  try {
    const res = await fetch(url, { signal, cache: 'no-store', credentials: 'same-origin' });
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
