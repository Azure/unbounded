// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { connectWebSocket, fetchClusterStatus, mergeDelta, StatusEvent } from '../api';
import { ClusterStatus, ClusterStatusDelta, ClusterSummary, ClusterSummaryDelta, NodeStatus, NodeSummary } from '../types';

const refreshIntervalMs = 10000;

type DiagEntry = {
  lastLogAt: number;
  suppressed: number;
};

const uiDiagEntries = new Map<string, DiagEntry>();

function resolveUiDiagEnabled() {
  if (typeof window === 'undefined') return false;
  try {
    const params = new URLSearchParams(window.location.search);
    const query = params.get('uiDiag');
    if (query === '1' || query === 'true') return true;
    if (query === '0' || query === 'false') return false;

    const stored = window.localStorage.getItem('uiDiag');
    if (!stored) return false;
    return stored === '1' || stored.toLowerCase() === 'true';
  } catch {
    return false;
  }
}

const UI_DIAG_ENABLED = resolveUiDiagEnabled();

function uiDiag(
  key: string,
  message: string,
  data?: Record<string, unknown>,
  options?: { minIntervalMs?: number; level?: 'log' | 'warn' }
) {
  if (!UI_DIAG_ENABLED) return;
  if (typeof window === 'undefined') return;
  const minIntervalMs = options?.minIntervalMs ?? 300;
  const level = options?.level ?? 'log';
  const now = performance.now();
  const entry = uiDiagEntries.get(key) || { lastLogAt: 0, suppressed: 0 };

  if (now - entry.lastLogAt < minIntervalMs) {
    entry.suppressed += 1;
    uiDiagEntries.set(key, entry);
    return;
  }

  const payload: Record<string, unknown> = {
    ...(data || {}),
    suppressed: entry.suppressed,
    at: new Date().toISOString()
  };

  if (level === 'warn') {
    console.warn(`[UI-DIAG] ${message}`, payload);
  } else {
    console.log(`[UI-DIAG] ${message}`, payload);
  }

  entry.lastLogAt = now;
  entry.suppressed = 0;
  uiDiagEntries.set(key, entry);
}

// buildNodeSummaryFromNodeStatus derives a NodeSummary from a full NodeStatus,
// mirroring the Go buildClusterSummary / deriveCniStatusAndTone logic.
function buildNodeSummaryFromNodeStatus(node: NodeStatus): NodeSummary {
  const peers = node.peers || [];
  let healthyPeers = 0;
  const now = Date.now();
  for (const peer of peers) {
    if (peer.healthCheck?.enabled) {
      const status = (peer.healthCheck.status || '').toLowerCase();
      if (status === 'up') {
        healthyPeers++;
      }
    } else if (peer.tunnel?.lastHandshake) {
      const handshakeTime = new Date(peer.tunnel.lastHandshake).getTime();
      if (now - handshakeTime < 3 * 60 * 1000) {
        healthyPeers++;
      }
    }
  }

  let routeMismatch = false;
  for (const route of node.routingTable?.routes || []) {
    for (const hop of route.nextHops || []) {
      if ((hop.expected === true) !== (hop.present === true)) {
        routeMismatch = true;
        break;
      }
    }
    if (routeMismatch) break;
  }

  let cniStatus = 'Unknown';
  let cniTone = 'warning';
  if (node.fetchError) {
    cniStatus = 'Fetch error';
    cniTone = 'danger';
  } else if ((node.nodeErrors || []).length > 0) {
    cniStatus = 'Errors';
    cniTone = 'danger';
  } else if (routeMismatch) {
    cniStatus = 'Route mismatch';
    cniTone = 'warning';
  } else {
    const src = node.statusSource || '';
    if (src === 'stale' || src === 'error') {
      cniStatus = 'Stale';
      cniTone = 'warning';
    } else if (src === '') {
      cniStatus = 'Unknown';
      cniTone = 'warning';
    } else {
      cniStatus = 'Healthy';
      cniTone = 'success';
    }
  }

  return {
    name: node.nodeInfo?.name,
    siteName: node.nodeInfo?.siteName,
    isGateway: node.nodeInfo?.isGateway,
    k8sReady: node.nodeInfo?.k8sReady,
    statusSource: node.statusSource,
    cniStatus,
    cniTone,
    errorCount: (node.nodeErrors || []).length,
    peerCount: peers.length,
    healthyPeers,
    routeCount: (node.routingTable?.routes || []).length,
    routeMismatch,
    fetchError: node.fetchError,
  };
}

// buildSummaryFromFullStatus converts a ClusterStatus to a ClusterSummary.
// Used for backward compatibility when the server sends full status.
function buildSummaryFromFullStatus(cs: ClusterStatus): ClusterSummary {
  const nodes = cs.nodes || [];
  return {
    timestamp: cs.timestamp,
    nodeCount: cs.nodeCount,
    siteCount: cs.siteCount,
    azureTenantId: cs.azureTenantId,
    leaderInfo: cs.leaderInfo,
    buildInfo: cs.buildInfo,
    sites: cs.sites,
    gatewayPools: cs.gatewayPools,
    peerings: cs.peerings,
    errors: cs.errors,
    warnings: cs.warnings,
    problems: cs.problems,
    pullEnabled: cs.pullEnabled,
    nodeSummaries: nodes.map(buildNodeSummaryFromNodeStatus),
    connectivityMatrix: cs.connectivityMatrix,
  };
}

function useClusterStatus() {
  // Summary from WS cluster_summary messages (new protocol)
  const [summaryFromWs, setSummaryFromWs] = useState<ClusterSummary | null>(null);
  // Full status from HTTP fetch or old-format WS messages (backward compat)
  const [status, setStatus] = useState<ClusterStatus | null>(null);
  // On-demand node detail cache
  const [nodeDetailCache, setNodeDetailCache] = useState<Map<string, NodeStatus>>(() => new Map());
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [wsConnected, setWsConnected] = useState(false);
  const wsRef = useRef<WebSocket | null>(null);
  const nodeSubscriptionsRef = useRef<Set<string>>(new Set());
  // Ref for delta merge so we always have the latest status
  const statusRef = useRef<ClusterStatus | null>(null);

  // Effective summary: prefer WS summary, fall back to derived from full status
  const summary = useMemo<ClusterSummary | null>(() => {
    if (summaryFromWs) return summaryFromWs;
    if (status) return buildSummaryFromFullStatus(status);
    return null;
  }, [summaryFromWs, status]);

  useEffect(() => {
    let polling: number | undefined;
    let reconnectTimer: number | undefined;

    const updateNodeDetailCacheFromStatus = (cs: ClusterStatus) => {
      const nodes = cs.nodes || [];
      if (nodes.length === 0) return;
      setNodeDetailCache((prev) => {
        const next = new Map(prev);
        for (const node of nodes) {
          const name = node.nodeInfo?.name;
          if (name) next.set(name, node);
        }
        return next;
      });
    };

    const refresh = async (background?: boolean) => {
      if (!background) {
        setLoading(true);
      }
      try {
        const data = await fetchClusterStatus();
        statusRef.current = data;
        setStatus(data);
        updateNodeDetailCacheFromStatus(data);
        setError(null);
      } catch (err) {
        setError((err as Error).message);
      } finally {
        setLoading(false);
      }
    };

    const schedulePoll = () => {
      if (polling) return;
      polling = window.setInterval(() => {
        refresh(true);
      }, refreshIntervalMs);
    };

    const stopPoll = () => {
      if (polling) {
        window.clearInterval(polling);
        polling = undefined;
      }
    };

    const handleMessage = (event: StatusEvent) => {
      uiDiag('ws-event', 'websocket event', { type: event.type }, { minIntervalMs: 250 });

      if (event.type === 'cluster_summary') {
        const cs = event.data as ClusterSummary;
        uiDiag('ws-cluster-summary', 'cluster summary received', {
          nodeCount: cs.nodeSummaries?.length ?? 0
        }, { minIntervalMs: 500 });
        setSummaryFromWs((prev) => {
          // Only accept if seq is at least as recent as what we have
          if (prev && typeof prev.seq === 'number' && typeof cs.seq === 'number' && cs.seq < prev.seq) {
            return prev;
          }
          return cs;
        });
        setError(null);
        setLoading(false);
      } else if (event.type === 'cluster_summary_delta') {
        const delta = event.data as ClusterSummaryDelta;
        setSummaryFromWs((prev) => {
          if (!prev) return prev;
          // Skip stale deltas
          if (typeof prev.seq === 'number' && typeof delta.seq === 'number' && delta.seq <= prev.seq) {
            return prev;
          }
          const merged = { ...prev };
          if (delta.seq != null) merged.seq = delta.seq;
          if (delta.timestamp != null) merged.timestamp = delta.timestamp;
          if (delta.nodeCount != null) merged.nodeCount = delta.nodeCount;
          if (delta.siteCount != null) merged.siteCount = delta.siteCount;
          if (delta.azureTenantId != null) merged.azureTenantId = delta.azureTenantId;
          if (delta.leaderInfo !== undefined) merged.leaderInfo = delta.leaderInfo;
          if (delta.buildInfo !== undefined) merged.buildInfo = delta.buildInfo;
          if (delta.sites) merged.sites = delta.sites;
          if (delta.gatewayPools) merged.gatewayPools = delta.gatewayPools;
          if (delta.peerings) merged.peerings = delta.peerings;
          if (delta.errors) merged.errors = delta.errors;
          if (delta.warnings) merged.warnings = delta.warnings;
          if (delta.problems) merged.problems = delta.problems;
          if (delta.pullEnabled != null) merged.pullEnabled = delta.pullEnabled;
          if (delta.connectivityMatrix !== undefined) merged.connectivityMatrix = delta.connectivityMatrix;
          if (delta.nodeSummaries || delta.removedNodes) {
            const byName = new Map<string, NodeSummary>();
            for (const ns of prev.nodeSummaries || []) {
              if (ns.name) byName.set(ns.name, ns);
            }
            if (delta.removedNodes) {
              for (const name of delta.removedNodes) byName.delete(name);
            }
            if (delta.nodeSummaries) {
              for (const ns of delta.nodeSummaries) {
                if (ns.name) byName.set(ns.name, ns);
              }
            }
            merged.nodeSummaries = Array.from(byName.values())
              .sort((a, b) => (a.name ?? '').localeCompare(b.name ?? ''));
          }
          return merged;
        });
      } else if (event.type === 'cluster_status') {
        // Backward compat: old server sends full status
        const fullStatus = event.data as ClusterStatus;
        const nodeCount = fullStatus?.nodes?.length ?? 0;
        uiDiag('ws-cluster-status', 'full cluster status received', { nodeCount }, { minIntervalMs: 500 });
        statusRef.current = fullStatus;
        setStatus(fullStatus);
        updateNodeDetailCacheFromStatus(fullStatus);
        setError(null);
        setLoading(false);
      } else if (event.type === 'cluster_status_delta') {
        // Backward compat: merge delta into cached full status
        const start = performance.now();
        const merged = mergeDelta(statusRef.current, event.data as ClusterStatusDelta);
        const durationMs = Math.round((performance.now() - start) * 100) / 100;
        uiDiag(
          'ws-delta-merge',
          'delta merged',
          {
            durationMs,
            prevNodes: statusRef.current?.nodes?.length ?? 0,
            nextNodes: merged?.nodes?.length ?? 0
          },
          { minIntervalMs: 300, level: durationMs >= 30 ? 'warn' : 'log' }
        );
        statusRef.current = merged;
        setStatus(merged);
        updateNodeDetailCacheFromStatus(merged);
        setError(null);
      } else if (event.type === 'node_detail_response' || event.type === 'node_detail_update') {
        const nodeName = (event as StatusEvent).nodeName;
        const nodeData = event.data as NodeStatus;
        if (nodeName && nodeData) {
          uiDiag('ws-node-detail', 'node detail received', { nodeName, type: event.type }, { minIntervalMs: 200 });
          setNodeDetailCache((prev) => {
            const next = new Map(prev);
            next.set(nodeName, nodeData);
            return next;
          });
        }
      }
    };

    const connect = () => {
      let keepaliveTimer: number | undefined;
      let lastMessageTime = Date.now();

      const ws = connectWebSocket(
        (event) => {
          lastMessageTime = Date.now();
          handleMessage(event);
        },
        () => {
          setWsConnected(true);
          setError(null);
          stopPoll();
          lastMessageTime = Date.now();
          // Subscribe to cluster summary on connect
          if (ws && ws.readyState === WebSocket.OPEN) {
            ws.send(JSON.stringify({ type: 'cluster_summary_subscribe' }));
            // Re-subscribe to any active node detail subscriptions
            for (const name of nodeSubscriptionsRef.current) {
              ws.send(JSON.stringify({ type: 'node_detail_subscribe', nodeName: name }));
            }
          }
          // Start keepalive: send ping every 30s, close if no message received in 60s
          keepaliveTimer = window.setInterval(() => {
            if (Date.now() - lastMessageTime > 60000) {
              // No message in 60s -- connection is hung, force reconnect
              if (ws) {
                try { ws.close(); } catch { /* ignore */ }
              }
              return;
            }
            if (ws && ws.readyState === WebSocket.OPEN) {
              try { ws.send(JSON.stringify({ type: 'ping' })); } catch { /* ignore */ }
            }
          }, 10000);
        },
        () => {
          if (keepaliveTimer) {
            window.clearInterval(keepaliveTimer);
            keepaliveTimer = undefined;
          }
          wsRef.current = null;
          setWsConnected(false);
          schedulePoll();
          if (!reconnectTimer) {
            reconnectTimer = window.setTimeout(() => {
              reconnectTimer = undefined;
              connect();
            }, 2000);
          }
        }
      );
      if (ws) {
        wsRef.current = ws;
      } else {
        schedulePoll();
      }
    };

    // Connect WS -- the server sends cluster_summary immediately on connect.
    // No initial HTTP fetch needed; polling only starts if WS fails.
    connect();

    return () => {
      stopPoll();
      if (reconnectTimer) {
        window.clearTimeout(reconnectTimer);
      }
      wsRef.current?.close();
    };
  }, []);

  const sendWsMessage = useCallback((msg: Record<string, unknown>) => {
    if (wsRef.current && wsRef.current.readyState === WebSocket.OPEN) {
      wsRef.current.send(JSON.stringify(msg));
    }
  }, []);

  const nodeDetail = useCallback((name: string): NodeStatus | undefined => {
    return nodeDetailCache.get(name);
  }, [nodeDetailCache]);

  const requestNodeDetail = useCallback((name: string) => {
    sendWsMessage({ type: 'node_detail_request', nodeName: name });
  }, [sendWsMessage]);

  const subscribeNodeDetail = useCallback((name: string) => {
    nodeSubscriptionsRef.current.add(name);
    sendWsMessage({ type: 'node_detail_subscribe', nodeName: name });
  }, [sendWsMessage]);

  const unsubscribeNodeDetail = useCallback((name: string) => {
    nodeSubscriptionsRef.current.delete(name);
    sendWsMessage({ type: 'node_detail_unsubscribe', nodeName: name });
  }, [sendWsMessage]);

  return {
    summary,
    status,
    loading,
    error,
    wsConnected,
    sendWsMessage,
    nodeDetail,
    requestNodeDetail,
    subscribeNodeDetail,
    unsubscribeNodeDetail,
  };
}

export default useClusterStatus;
