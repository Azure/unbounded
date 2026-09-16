// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

import { useCallback, useEffect, useRef, useState } from 'react';
import { connectWebSocket, fetchClusterStatus } from '../api';
import type { StatusEvent } from '../api';
import type { ClusterStatus, ClusterStatusDelta, ClusterSummary, ClusterSummaryDelta } from '../types';
import { mergeLegacySummary, mergeSummary, toClusterSummary } from '../state/clusterSummary';

function useClusterStatus() {
  const [summary, setSummary] = useState<ClusterSummary | null>(null);
  const summaryRef = useRef<ClusterSummary | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [wsConnected, setWsConnected] = useState(false);
  const wsRef = useRef<WebSocket | null>(null);

  useEffect(() => {
    let disposed = false;
    let polling: number | undefined;
    let reconnectTimer: number | undefined;
    let keepalive: number | undefined;
    let revision = 0;
    let fetching = false;
    const abort = new AbortController();
    const update = (next: ClusterSummary | null) => {
      if (disposed || !next) return;
      // Projection occurs before React's update queue: no queued callback holds
      // a legacy full response, even briefly across subsequent renders.
      summaryRef.current = next;
      setSummary(next);
      setError(null);
      setLoading(false);
      revision++;
    };
    const refresh = async () => {
      if (fetching || disposed) return;
      fetching = true;
      const startedAtRevision = revision;
      try {
        const data = await fetchClusterStatus(abort.signal);
        if (!disposed && startedAtRevision === revision) update(toClusterSummary(data));
      } catch (err) {
        if (!disposed) { setError((err as Error).message); setLoading(false); }
      } finally { fetching = false; }
    };
    const stopPoll = () => {
      if (polling !== undefined) window.clearInterval(polling);
      polling = undefined;
    };
    const schedulePoll = () => {
      if (disposed || polling !== undefined) return;
      void refresh();
      polling = window.setInterval(() => void refresh(), 10000);
    };
    const handleMessage = (event: StatusEvent) => {
      if (disposed) return;
      if (event.type === 'cluster_summary' || event.type === 'cluster_status') {
        update(toClusterSummary(event.data as ClusterSummary | ClusterStatus));
      } else if (event.type === 'cluster_summary_delta') {
        if (!summaryRef.current) { void refresh(); return; }
        update(mergeSummary(summaryRef.current, event.data as ClusterSummaryDelta));
      } else if (event.type === 'cluster_status_delta') {
        if (!summaryRef.current) { void refresh(); return; }
        update(mergeLegacySummary(summaryRef.current, event.data as ClusterStatusDelta));
      }
      // Unsolicited legacy node details are deliberately ignored.
    };
    const connect = () => {
      if (disposed) return;
      let lastMessageTime = Date.now();
      const ws = connectWebSocket(
        (event) => { lastMessageTime = Date.now(); handleMessage(event); },
        () => {
          if (disposed) { ws?.close(); return; }
          setWsConnected(true);
          setError(null);
          stopPoll();
          lastMessageTime = Date.now();
          ws?.send(JSON.stringify({ type: 'cluster_summary_subscribe' }));
          keepalive = window.setInterval(() => {
            if (Date.now() - lastMessageTime > 60000) { ws?.close(); return; }
            if (ws?.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: 'ping' }));
          }, 10000);
        },
        () => {
          if (keepalive !== undefined) window.clearInterval(keepalive);
          keepalive = undefined;
          wsRef.current = null;
          if (disposed) return;
          setWsConnected(false);
          schedulePoll();
          reconnectTimer = window.setTimeout(() => { reconnectTimer = undefined; connect(); }, 2000);
        }
      );
      wsRef.current = ws;
      if (!ws) {
        schedulePoll();
        reconnectTimer = window.setTimeout(() => { reconnectTimer = undefined; connect(); }, 2000);
      }
    };
    connect();
    return () => {
      disposed = true;
      abort.abort();
      stopPoll();
      if (reconnectTimer !== undefined) window.clearTimeout(reconnectTimer);
      if (keepalive !== undefined) window.clearInterval(keepalive);
      const ws = wsRef.current;
      if (ws) {
        ws.onopen = ws.onclose = ws.onmessage = ws.onerror = null;
        ws.close();
      }
      wsRef.current = null;
    };
  }, []);

  const sendWsMessage = useCallback((message: Record<string, unknown>) => {
    if (wsRef.current?.readyState === WebSocket.OPEN) wsRef.current.send(JSON.stringify(message));
  }, []);

  return { summary, loading, error, wsConnected, sendWsMessage };
}

export default useClusterStatus;
