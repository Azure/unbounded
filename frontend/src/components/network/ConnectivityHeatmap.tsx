// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { useEffect, useMemo, useRef, useState } from 'react';
import { GatewayPoolStatus, NodeStatus, SiteMatrix, SiteStatus } from '../../types';
import { getCniStatus } from '../nodes/shared/index';

function getGatewayPoolNodeNames(pool: GatewayPoolStatus): string[] {
  const nodeNames = (pool.nodes || [])
    .map((node) => node.name)
    .filter((name): name is string => Boolean(name));
  if (nodeNames.length > 0) {
    return nodeNames;
  }
  return (pool.gateways || []).filter((name): name is string => Boolean(name));
}

function ConnectivityHeatmap({
  matrix,
  hiddenSites,
  hiddenGatewayPools,
  sites,
  gatewayPools,
  siteCounts,
  nodeStatuses
}: {
  matrix?: Record<string, SiteMatrix>;
  hiddenSites: Set<string>;
  hiddenGatewayPools: Set<string>;
  sites: SiteStatus[];
  gatewayPools: GatewayPoolStatus[];
  siteCounts: Map<string, { online: number; total: number }>;
  nodeStatuses: NodeStatus[];
}) {
  const [activeScope, setActiveScope] = useState<string>('all');
  const matrixRef = useRef<HTMLDivElement | null>(null);
  const [matrixSize, setMatrixSize] = useState({ width: 420, height: 420 });
  const [tooltip, setTooltip] = useState<{
    x: number;
    y: number;
    src: string;
    dst: string;
    value: number;
  } | null>(null);
  const siteNames = useMemo(() => {
    return Object.keys(matrix || {})
      .filter((name) => !name.startsWith('pool:'))
      .filter((name) => !hiddenSites.has(name))
      .sort();
  }, [matrix, hiddenSites]);
  const gatewayPoolNames = useMemo(() => {
    return (gatewayPools || [])
      .map((pool) => pool.name || '')
      .filter((name) => Boolean(name) && !hiddenGatewayPools.has(name))
      .sort();
  }, [gatewayPools, hiddenGatewayPools]);
  const selectorOptions = useMemo(
    () => [
      { key: 'all', label: 'All', kind: 'all' as const },
      ...siteNames.map((site) => ({ key: `site:${site}`, label: site, kind: 'site' as const })),
      ...gatewayPoolNames.map((pool) => ({ key: `pool:${pool}`, label: pool, kind: 'pool' as const }))
    ],
    [siteNames, gatewayPoolNames]
  );
  const siteLookup = useMemo(() => {
    const map = new Map<string, SiteStatus>();
    for (const site of sites) {
      if (site.name) {
        map.set(site.name, site);
      }
    }
    return map;
  }, [sites]);
  const nodeStatusByName = useMemo(() => {
    const map = new Map<string, NodeStatus>();
    for (const node of nodeStatuses) {
      const name = node.nodeInfo?.name;
      if (name) {
        map.set(name, node);
      }
    }
    return map;
  }, [nodeStatuses]);
  const visibleNodeNames = useMemo(() => {
    return nodeStatuses
      .filter((node) => {
        const nodeName = node.nodeInfo?.name || '';
        if (!nodeName) return false;
        const siteName = node.nodeInfo?.siteName;
        if (siteName && hiddenSites.has(siteName)) {
          return false;
        }
        const poolName = (gatewayPools || []).find((pool) => {
          const poolNameValue = pool.name || '';
          if (!poolNameValue) return false;
          return getGatewayPoolNodeNames(pool).includes(nodeName);
        })?.name;
        if (poolName && hiddenGatewayPools.has(poolName)) {
          return false;
        }
        return true;
      })
      .map((node) => node.nodeInfo?.name || '')
      .filter((name) => Boolean(name));
  }, [nodeStatuses, hiddenSites, hiddenGatewayPools, gatewayPools]);

  const adjacency = useMemo(() => {
    const map = new Map<string, Set<string>>();
    const allVisible = new Set(visibleNodeNames);
    for (const node of nodeStatuses) {
      const src = node.nodeInfo?.name;
      if (!src || !allVisible.has(src)) continue;
      if (!map.has(src)) map.set(src, new Set<string>());
      for (const peer of node.peers || []) {
        const dst = peer.name;
        if (!dst || !allVisible.has(dst) || dst === src) continue;
        map.get(src)?.add(dst);
        if (!map.has(dst)) map.set(dst, new Set<string>());
        map.get(dst)?.add(src);
      }
    }
    return map;
  }, [nodeStatuses, visibleNodeNames]);

  const healthCheckStatusByPair = useMemo(() => {
    const map = new Map<string, string>();
    for (const siteMatrix of Object.values(matrix || {})) {
      const results = siteMatrix?.results || {};
      for (const [src, row] of Object.entries(results)) {
        for (const [dst, cell] of Object.entries(row || {})) {
          const key = src < dst ? `${src}|${dst}` : `${dst}|${src}`;
          if (!map.has(key) && cell) {
            map.set(key, typeof cell === 'string' ? cell : (cell as { healthCheckStatus?: string })?.healthCheckStatus || '');
          }
        }
      }
    }
    // Also extract health check status from per-node peer data (covers cross-site links)
    for (const node of nodeStatuses) {
      const src = node.nodeInfo?.name;
      if (!src) continue;
      for (const peer of node.peers || []) {
        const dst = peer.name;
        if (!dst || dst === src) continue;
        const key = src < dst ? `${src}|${dst}` : `${dst}|${src}`;
        if (map.has(key)) continue; // matrix data takes priority
        const status = peer.healthCheck?.status;
        if (status) {
          map.set(key, status);
        }
      }
    }
    return map;
  }, [matrix, nodeStatuses]);

  const selectedNodeNames = useMemo(() => {
    const visibleSet = new Set(visibleNodeNames);
    if (activeScope === 'all') {
      return [...visibleNodeNames].sort();
    }

    if (activeScope.startsWith('site:')) {
      const siteName = activeScope.slice(5);
      const fromMatrix = (matrix?.[siteName]?.nodes || [])
        .map((name) => String(name))
        .filter((name) => visibleSet.has(name));
      if (fromMatrix.length > 0) {
        return Array.from(new Set(fromMatrix)).sort();
      }
      return nodeStatuses
        .map((node) => node.nodeInfo?.name || '')
        .filter((name) => {
          if (!name || !visibleSet.has(name)) return false;
          const site = nodeStatusByName.get(name)?.nodeInfo?.siteName;
          return site === siteName;
        })
        .sort();
    }

    if (activeScope.startsWith('pool:')) {
      const poolName = activeScope.slice(5);
      const poolMatrixNodes = (matrix?.[`pool:${poolName}`]?.nodes || [])
        .map((name) => String(name))
        .filter((name) => visibleSet.has(name));
      if (poolMatrixNodes.length > 0) {
        return Array.from(new Set(poolMatrixNodes)).sort();
      }
      const pool = gatewayPools.find((item) => item.name === poolName);
      if (!pool) return [];
      const members = new Set<string>();
      for (const nodeName of getGatewayPoolNodeNames(pool)) {
        if (!visibleSet.has(nodeName)) continue;
        members.add(nodeName);
        for (const peerName of adjacency.get(nodeName) || []) {
          if (visibleSet.has(peerName)) {
            members.add(peerName);
          }
        }
      }
      return Array.from(members).sort();
    }

    return [];
  }, [activeScope, visibleNodeNames, matrix, nodeStatuses, nodeStatusByName, gatewayPools, adjacency]);

  useEffect(() => {
    if (!selectorOptions.some((option) => option.key === activeScope)) {
      setActiveScope('all');
    }
  }, [activeScope, selectorOptions]);

  useEffect(() => {
    if (!matrixRef.current) return;
    const updateSize = () => {
      const rect = matrixRef.current?.getBoundingClientRect();
      if (!rect) return;
      const width = Math.max(0, Math.floor(rect.width));
      const height = Math.max(0, Math.floor(rect.height));
      if (width > 0 && height > 0) {
        setMatrixSize({ width, height });
      }
    };
    updateSize();
    const observer = new ResizeObserver(updateSize);
    observer.observe(matrixRef.current);
    return () => observer.disconnect();
  }, []);

  if ((!matrix || siteNames.length === 0) && selectedNodeNames.length === 0) {
    return <div>No connectivity data available.</div>;
  }

  const matrixNodes = selectedNodeNames;
  if (matrixNodes.length === 0) {
    return <div>No connectivity data available.</div>;
  }
  const cells: Array<{ row: number; col: number; value: number; src: string; dst: string }> = [];
  const hasExpectedLink = (src: string, dst: string) => {
    if (src === dst) return true;
    return adjacency.get(src)?.has(dst) || adjacency.get(dst)?.has(src) || false;
  };

  const selfCellValueFromCniStatus = (node?: NodeStatus) => {
    if (!node) {
      return 0;
    }
    if (node.statusSource === 'apiserver-push' || node.statusSource === 'apiserver-ws') {
      return 2;
    }
    const cni = getCniStatus(node);
    if (cni.tone === 'success') return 1;
    if (cni.tone === 'warning') return 2;
    return 0;
  };

  for (let i = 0; i < matrixNodes.length; i++) {
    const src = matrixNodes[i];
    for (let j = 0; j < matrixNodes.length; j++) {
      const dst = matrixNodes[j];
      const pairKey = src < dst ? `${src}|${dst}` : `${dst}|${src}`;
      const hcStatus = (healthCheckStatusByPair.get(pairKey) || '').trim().toLowerCase();
      let value = hcStatus === 'up' ? 1 : hcStatus === 'mixed' ? 2 : hcStatus ? 0 : -1;
      if (src === dst) {
        const node = nodeStatusByName.get(src);
        value = selfCellValueFromCniStatus(node);
      } else if (!hasExpectedLink(src, dst)) {
        value = -2;
      }
      cells.push({ row: i, col: j, value, src, dst });
    }
  }

  const { width, height } = matrixSize;

  const getColor = (value: number) => {
    if (value === 1) return '#4ade80';
    if (value === 2) return '#facc15';
    if (value === 0) return '#f87171';
    if (value === -2) return 'transparent';
    return '#6b7280';
  };

  const renderMatrix = (width: number, height: number) => {
    if (width <= 0 || height <= 0) {
      return <div className="matrix-container" ref={matrixRef}></div>;
    }
    const showAxisLabels = false;
    const margin = { top: 12, left: 12, right: 12, bottom: 12 };
    const innerWidth = width - margin.left - margin.right;
    const innerHeight = height - margin.top - margin.bottom;
    const minInner = Math.min(innerWidth, innerHeight);
    const gap = Math.max(1, Math.round(minInner * 0.005));
    const cellSize = Math.floor((minInner - gap * (matrixNodes.length - 1)) / matrixNodes.length);
    const gridWidth = cellSize * matrixNodes.length + gap * (matrixNodes.length - 1);
    const gridHeight = cellSize * matrixNodes.length + gap * (matrixNodes.length - 1);
    const offsetX = Math.max(0, Math.floor((innerWidth - gridWidth) / 2));
    const offsetY = Math.max(0, Math.floor((innerHeight - gridHeight) / 2));
    const scale = 1;

    return (
      <div className="matrix-container" ref={matrixRef}>
        <div className="matrix-wrapper">
          <svg width="100%" height="100%" viewBox={`0 0 ${width} ${height}`} preserveAspectRatio="xMidYMid meet">
          <g
            transform={`translate(${margin.left + offsetX}, ${margin.top + offsetY}) scale(${scale})`}
          >
            {cells.map((cell) => {
              const x = cell.col * (cellSize + gap);
              const y = cell.row * (cellSize + gap);
              const w = cellSize;
              const h = cellSize;
              return (
                <rect
                  key={`${cell.src}-${cell.dst}`}
                  x={x}
                  y={y}
                  width={w}
                  height={h}
                  fill={getColor(cell.value)}
                  onMouseMove={(event) => {
                    setTooltip({
                      x: event.clientX + 12,
                      y: event.clientY + 12,
                      src: cell.src,
                      dst: cell.dst,
                      value: cell.value
                    });
                  }}
                  onMouseLeave={() => setTooltip(null)}
                />
              );
            })}
            {showAxisLabels &&
              matrixNodes.map((label, index) => (
                <text
                  key={`x-${label}`}
                  x={index * (cellSize + gap) + cellSize / 2}
                  y={-6}
                  textAnchor="middle"
                  fontSize={10}
                  fill="var(--text-muted)"
                >
                  {label}
                </text>
              ))}
            {showAxisLabels &&
              matrixNodes.map((label, index) => (
                <text
                  key={`y-${label}`}
                  x={-6}
                  y={index * (cellSize + gap) + cellSize / 2 + 3}
                  textAnchor="end"
                  fontSize={10}
                  fill="var(--text-muted)"
                >
                  {label}
                </text>
              ))}
          </g>
          </svg>
        </div>
        {tooltip && (
          <div className="heatmap-tooltip" style={{ left: tooltip.x, top: tooltip.y }}>
            <div>{tooltip.src} {'->'} {tooltip.dst}</div>
            <div style={{ fontSize: '0.75rem', color: 'var(--text-muted)' }}>
              {tooltip.src === tooltip.dst
                ? (tooltip.value === 1
                    ? 'CNI Healthy'
                    : tooltip.value === 2
                      ? `CNI Warning: ${getCniStatus(nodeStatusByName.get(tooltip.src))?.label || 'Warning'}`
                      : 'CNI No Data')
                : tooltip.value === 1
                  ? 'HC Up'
                  : tooltip.value === 2
                    ? 'HC Mixed'
                  : tooltip.value === 0
                    ? 'HC Down'
                    : tooltip.value === -2
                      ? 'No Link Expected'
                      : 'No Data'}
            </div>
          </div>
        )}
      </div>
    );
  };

  return (
    <div className="matrix-panel">
      <div className="site-row">
        {selectorOptions.map((option) => {
          const isActive = option.key === activeScope;
          const isSite = option.kind === 'site';
          const isPool = option.kind === 'pool';
          const siteInfo = isSite ? siteLookup.get(option.label) : undefined;
          const counts = isSite ? siteCounts.get(option.label) : undefined;
          const online = counts?.online ?? siteInfo?.onlineCount ?? 0;
          const total = counts?.total ?? siteInfo?.nodeCount ?? 0;
          const status = isSite
            ? (online === 0 && total > 0 ? 'danger' : online < total ? 'warning' : 'success')
            : isPool
              ? 'info'
              : 'all';
          return (
            <button
              key={option.key}
              className={`site-pill ${isActive ? 'active' : 'inactive'}`}
              onClick={() => setActiveScope(option.key)}
              style={{ cursor: 'pointer', border: 'none' }}
            >
              <span className={`badge ${status}`}>{option.label}</span>
            </button>
          );
        })}
      </div>
      {renderMatrix(width, height)}
    </div>
  );
}


export default ConnectivityHeatmap;
