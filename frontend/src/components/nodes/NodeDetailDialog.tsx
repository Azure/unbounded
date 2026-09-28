// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useMemo, useState } from 'react';
import type { ComponentProps } from 'react';
import type { DetailView } from '../../state/nodeDetails';
import { nodeForDetailView } from '../../state/nodeMetadata';
import NodeDetailModal from './NodeDetailModal';
import { formatDateAndAge } from './shared/index';

type Props = Omit<ComponentProps<typeof NodeDetailModal>, 'node' | 'detailControls' | 'detailsLoaded'> & {
  detail: DetailView;
  onLoad: (forceRefresh?: boolean) => void;
};

export default function NodeDetailDialog({ detail, onLoad, ...props }: Props) {
  const [jsonOpen, setJsonOpen] = useState(false);
  const [, setClock] = useState(0);
  const expiresAt = detail.snapshot?.expiresAt;
  useEffect(() => {
    if (!props.nodeName) return;
    const timer = window.setInterval(() => setClock((clock) => clock + 1), 1000);
    return () => window.clearInterval(timer);
  }, [props.nodeName]);
  useEffect(() => setJsonOpen(false), [props.nodeName, expiresAt]);
  const snapshot = detail.snapshot && Date.parse(detail.snapshot.expiresAt) > Date.now() ? detail.snapshot : undefined;
  const node = useMemo(
    () => props.nodeName ? nodeForDetailView(props.nodeName, props.summary, snapshot?.status) : null,
    [props.nodeName, props.summary, snapshot?.status],
  );
  if (!props.nodeName) return null;
  const busy = detail.state === 'loading';
  const collected = snapshot ? formatDateAndAge(snapshot.collectedAt) : undefined;
  const state = !snapshot && detail.state === 'loaded' ? 'expired' : detail.state;
  const message = state === 'not-loaded' ? 'Detailed data is not loaded. Choose Load data to inspect peers, routes, BPF entries, and full node JSON.'
    : state === 'loading' ? 'Loading node details...'
    : state === 'expired' ? 'Detailed data expired and was removed. Choose Load data to request it again.'
    : state === 'error' ? 'The detail request failed. Choose Load data to retry or Refresh to force a fresh collection.'
    : 'Detailed data loaded.';
  const controls = (
    <div className="card node-modal-card" aria-live="polite">
      <div>{message}</div>
      {detail.error && <div role="alert">Request error: {detail.error}</div>}
      {snapshot && (
        <div>
          {detail.error || busy ? 'Showing previous still-valid snapshot. ' : ''}
          Collected {collected?.age} ({collected?.absolute}).
          {' '}Received {snapshot.receivedAt}. Expires {snapshot.expiresAt}.
        </div>
      )}
      {busy && detail.deadline && <div>Request deadline: {detail.deadline}</div>}
      <div className="modal-header-actions">
        <button className="button" disabled={busy} onClick={() => onLoad(false)}>Load data</button>
        <button className="button" disabled={busy} onClick={() => onLoad(true)}>Refresh</button>
        {snapshot && <button className="button" onClick={() => setJsonOpen((open) => !open)}>
          {jsonOpen ? 'Hide' : 'Show'} full node JSON
        </button>}
      </div>
      {snapshot && jsonOpen && <pre aria-label="Full node JSON">{JSON.stringify(snapshot.status, null, 2)}</pre>}
    </div>
  );
  // Unmount the heavy tables when data expires so their memoized rows, maps,
  // column callbacks and serialized JSON cannot keep the snapshot alive.
  return (
    <NodeDetailModal
      {...props}
      key={snapshot ? 'details' : 'overview'}
      node={node}
      detailsLoaded={Boolean(snapshot)}
      detailControls={controls}
    />
  );
}
