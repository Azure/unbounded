// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { useEffect, useMemo, useRef, useState } from 'react';
import { ClusterStatus } from '../../types';
import { CloseXIcon } from '../nodes/shared/index';

function StatusJsonModal({
  open,
  onClose
}: {
  open: boolean;
  onClose: () => void;
}) {
  const [snapshotStatus, setSnapshotStatus] = useState<ClusterStatus | null>(null);
  const [fetchError, setFetchError] = useState<string | null>(null);
  const [fetching, setFetching] = useState(false);
  const [collapseAllVersion, setCollapseAllVersion] = useState(0);
  const wasOpenRef = useRef(false);

  useEffect(() => {
    if (!open) return;
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') {
        onClose();
      }
    };
    window.addEventListener('keydown', onKeyDown);
    return () => window.removeEventListener('keydown', onKeyDown);
  }, [open, onClose]);

  useEffect(() => {
    if (open && !wasOpenRef.current) {
      // Fetch fresh data from HTTP on each open
      wasOpenRef.current = true;
      setFetching(true);
      setFetchError(null);
      fetch('/status/json')
        .then((res) => {
          if (!res.ok) throw new Error(`HTTP ${res.status}`);
          return res.json();
        })
        .then((data) => {
          setSnapshotStatus(data as ClusterStatus);
          setCollapseAllVersion((version) => version + 1);
        })
        .catch((err) => {
          setFetchError((err as Error).message);
        })
        .finally(() => {
          setFetching(false);
        });
      return;
    }
    if (!open) {
      wasOpenRef.current = false;
    }
  }, [open]);

  const statusJsonValue = useMemo<JsonValue>(() => {
    if (!snapshotStatus) {
      return { message: 'No cluster status available' };
    }
    const compareNames = (aName?: string, bName?: string) => {
      const a = (aName || '').trim();
      const b = (bName || '').trim();
      if (!a && !b) return 0;
      if (!a) return 1;
      if (!b) return -1;
      return a.localeCompare(b);
    };

    const sortedStatus: ClusterStatus = {
      ...snapshotStatus,
      sites: [...(snapshotStatus.sites || [])].sort((a, b) => compareNames(a.name, b.name)),
      nodes: [...(snapshotStatus.nodes || [])].sort((a, b) => compareNames(a.nodeInfo?.name, b.nodeInfo?.name)),
      gatewayPools: [...(snapshotStatus.gatewayPools || [])].sort((a, b) => compareNames(a.name, b.name))
    };

    return sortedStatus as unknown as JsonValue;
  }, [snapshotStatus]);

  if (!open) return null;

  return (
    <div className="modal-backdrop" onClick={onClose}>
      <div className="modal modal-status-json" onClick={(event) => event.stopPropagation()}>
        <div className="modal-header">
          <div className="modal-title">Cluster Status JSON</div>
          <div className="modal-header-actions">
            <button
              className="button"
              type="button"
              onClick={() => setCollapseAllVersion((version) => version + 1)}
              title="Collapse all nested sections"
            >
              Collapse all children
            </button>
            <button className="button zoom-action-button" onClick={onClose} aria-label="Close" title="Close">
              <CloseXIcon />
            </button>
          </div>
        </div>
        <div className="status-json-viewer" role="region" aria-label="Cluster status JSON">
          {fetching && <div style={{ padding: '2rem', textAlign: 'center' }}>Loading...</div>}
          {fetchError && <div style={{ padding: '2rem', textAlign: 'center', color: 'var(--status-danger)' }}>Error: {fetchError}</div>}
          {!fetching && !fetchError && (
          <div className="json-tree">
            <JsonTreeNode
              value={statusJsonValue}
              depth={0}
              defaultExpandedDepth={2}
              path="root"
              collapseAllVersion={collapseAllVersion}
            />
          </div>
          )}
        </div>
      </div>
    </div>
  );
}

type JsonValue = string | number | boolean | null | JsonValue[] | { [key: string]: JsonValue };

function JsonTreeNode({
  name,
  value,
  depth,
  trailingComma,
  defaultExpandedDepth,
  path,
  collapseAllVersion
}: {
  name?: string;
  value: JsonValue;
  depth: number;
  trailingComma?: boolean;
  defaultExpandedDepth: number;
  path: string;
  collapseAllVersion: number;
}) {
  const collapsible = value !== null && typeof value === 'object';
  const isArray = Array.isArray(value);
  const entries = collapsible
    ? (isArray
      ? (value as JsonValue[]).map((entry, index) => [String(index), entry] as const)
      : Object.entries(value as Record<string, JsonValue>))
    : [];
  const [expanded, setExpanded] = useState(depth < defaultExpandedDepth);

  useEffect(() => {
    if (depth === 0) {
      setExpanded(true);
    } else {
      setExpanded(false);
    }
  }, [collapseAllVersion, depth]);

  const keyPrefix = name !== undefined ? (
    <>
      <span className="json-key">"{name}"</span>
      <span className="json-punctuation">: </span>
    </>
  ) : null;

  if (!collapsible) {
    const valueClass = value === null
      ? 'json-null'
      : typeof value === 'string'
        ? 'json-string'
        : typeof value === 'number'
          ? 'json-number'
          : 'json-boolean';
    const renderedValue = value === null
      ? 'null'
      : typeof value === 'string'
        ? `"${value}"`
        : String(value);

    return (
      <div className="json-line" style={{ paddingLeft: depth * 16 }}>
        {keyPrefix}
        <span className={valueClass}>{renderedValue}</span>
        {trailingComma ? <span className="json-punctuation">,</span> : null}
      </div>
    );
  }

  const opening = isArray ? '[' : '{';
  const closing = isArray ? ']' : '}';
  const routeEntryPreview = !isArray
    ? (() => {
      const objectValue = value as Record<string, JsonValue>;
      const destination = objectValue.destination;
      const nextHops = objectValue.nextHops;
      if (typeof destination !== 'string' || destination.trim().length === 0) {
        return null;
      }
      if (!Array.isArray(nextHops)) {
        return `${destination} -- 0 nextHops`;
      }
      return `${destination} -- ${nextHops.length} nextHop${nextHops.length === 1 ? '' : 's'}`;
    })()
    : null;
  const objectNamePreview = !isArray
    ? (() => {
      const objectValue = value as Record<string, JsonValue>;
      const candidate = objectValue.name;
      if (typeof candidate === 'string' && candidate.trim().length > 0) {
        return candidate;
      }
      const nodeInfo = objectValue.nodeInfo;
      if (nodeInfo && typeof nodeInfo === 'object' && !Array.isArray(nodeInfo)) {
        const nodeName = (nodeInfo as Record<string, JsonValue>).name;
        if (typeof nodeName === 'string' && nodeName.trim().length > 0) {
          return nodeName;
        }
      }
      return null;
    })()
    : null;
  const summary = isArray
    ? `${entries.length} item${entries.length === 1 ? '' : 's'}`
    : routeEntryPreview
      ? routeEntryPreview
    : objectNamePreview
      ? `${objectNamePreview} -- ${entries.length} key${entries.length === 1 ? '' : 's'}`
      : `${entries.length} key${entries.length === 1 ? '' : 's'}`;

  if (!expanded) {
    return (
      <div className="json-line" style={{ paddingLeft: depth * 16 }}>
        <button
          type="button"
          className="json-toggle"
          onClick={() => setExpanded(true)}
          aria-label="Expand JSON section"
          title="Expand"
        >
          ▶
        </button>
        {keyPrefix}
        <span className="json-bracket">{opening}</span>
        <span className="json-summary">{summary}</span>
        <span className="json-bracket">{closing}</span>
        {trailingComma ? <span className="json-punctuation">,</span> : null}
      </div>
    );
  }

  return (
    <>
      <div className="json-line" style={{ paddingLeft: depth * 16 }}>
        <button
          type="button"
          className="json-toggle"
          onClick={() => setExpanded(false)}
          aria-label="Collapse JSON section"
          title="Collapse"
        >
          ▼
        </button>
        {keyPrefix}
        <span className="json-bracket">{opening}</span>
      </div>
      {entries.map(([entryKey, entryValue], index) => (
        <JsonTreeNode
          key={`${path}.${entryKey}`}
          name={isArray ? undefined : entryKey}
          value={entryValue}
          depth={depth + 1}
          trailingComma={index < entries.length - 1}
          defaultExpandedDepth={defaultExpandedDepth}
          path={`${path}.${entryKey}`}
          collapseAllVersion={collapseAllVersion}
        />
      ))}
      <div className="json-line" style={{ paddingLeft: depth * 16 }}>
        <span className="json-indent-placeholder" aria-hidden="true" />
        <span className="json-bracket">{closing}</span>
        {trailingComma ? <span className="json-punctuation">,</span> : null}
      </div>
    </>
  );
}


export default StatusJsonModal;
