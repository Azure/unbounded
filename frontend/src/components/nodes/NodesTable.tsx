// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import * as React from 'react';
import { useEffect, useMemo, useRef, useState } from 'react';
import {
  ColumnDef,
  flexRender,
  getCoreRowModel,
  getPaginationRowModel,
  getSortedRowModel,
  SortingState,
  useReactTable
} from '@tanstack/react-table';
import { GatewayPoolStatus, NodeSummary, SiteStatus } from '../../types';
import {
  CloseXIcon,
  MagnifyPlusIcon,
  TableFilterButton,
  getCountColor,
  getGatewayPoolBadgeTone,
  uiDiag,
  useDismissOnOutside
} from './shared/index';

const allNodeTypeOptions = ['Gateway', 'Worker'];
const allK8sStatusOptions = ['Ready', 'NotReady', 'Missing'];
const allCniStatusOptions = ['Healthy', 'No data', 'Errors', 'Fetch error', 'Health check failing', 'Route mismatch', 'Stale', 'Unknown'];

function NodesTable({
  nodes,
  sites,
  gatewayPools,
  hiddenSites,
  hiddenGatewayPools,
  siteCounts,
  poolCounts,
  onToggleSite,
  onShowAllSites,
  onToggleGatewayPool,
  onShowAllGatewayPools,
  gatewayByNode,
  selectedNodeTypes,
  onSelectedNodeTypesChange,
  pullEnabled,
  onSelect,
  onToggleMaximize,
  isMaximized
}: {
  nodes: NodeSummary[];
  sites: SiteStatus[];
  gatewayPools: GatewayPoolStatus[];
  hiddenSites: Set<string>;
  hiddenGatewayPools: Set<string>;
  siteCounts: Map<string, { online: number; total: number }>;
  poolCounts: Map<string, { online: number; total: number }>;
  onToggleSite: (name: string) => void;
  onShowAllSites: () => void;
  onToggleGatewayPool: (name: string) => void;
  onShowAllGatewayPools: () => void;
  gatewayByNode: Map<string, string>;
  selectedNodeTypes: Set<string>;
  onSelectedNodeTypesChange: React.Dispatch<React.SetStateAction<Set<string>>>;
  pullEnabled?: boolean;
  onSelect: (nodeName: string) => void;
  onToggleMaximize: () => void;
  isMaximized: boolean;
}) {
  const [nodeNameFilter, setNodeNameFilter] = useState('');
  const [sorting, setSorting] = useState<SortingState>([
    { id: 'name', desc: false }
  ]);
  const [pagination, setPagination] = useState({ pageIndex: 0, pageSize: 25 });
  const [selectedK8sStatuses, setSelectedK8sStatuses] = useState<Set<string>>(() => new Set(allK8sStatusOptions));
  const [selectedCniStatuses, setSelectedCniStatuses] = useState<Set<string>>(
    () => new Set(allCniStatusOptions)
  );
  const [filterPopoverOpen, setFilterPopoverOpen] = useState(false);
  const [pageSizeMode, setPageSizeMode] = useState<'auto' | number>('auto');
  const [autoPageSize, setAutoPageSize] = useState(25);
  const identicalPaginationUpdateRef = useRef(0);
  const tableWrapperRef = useRef<HTMLDivElement | null>(null);
  const filterButtonRef = useRef<HTMLButtonElement | null>(null);
  const filterPopoverRef = useRef<HTMLDivElement | null>(null);
  const renderRef = useRef({ count: 0, windowStart: performance.now() });

  useDismissOnOutside(filterPopoverOpen, [filterButtonRef, filterPopoverRef], () => setFilterPopoverOpen(false));

  const getNodeTypeLabel = (row: NodeSummary) => {
    const nodeName = row.name || '';
    const isGatewayNode = row.isGateway || gatewayByNode.has(nodeName);
    return isGatewayNode ? 'Gateway' : 'Worker';
  };

  const getSiteBadgeLabel = (row: NodeSummary) => {
    return row.siteName || '-';
  };

  const getPoolBadgeLabel = (row: NodeSummary) => {
    const nodeName = row.name || '';
    const isGatewayNode = row.isGateway || gatewayByNode.has(nodeName);
    if (!isGatewayNode) {
      return '';
    }
    const gatewayPoolName = gatewayByNode.get(nodeName);
    if (gatewayPoolName) {
      return gatewayPoolName;
    }
    return '-';
  };

  const allSelectedNodeTypes = selectedNodeTypes.size === allNodeTypeOptions.length;
  const allSelectedK8sStatuses = selectedK8sStatuses.size === allK8sStatusOptions.length;
  const allSelectedCniStatuses = selectedCniStatuses.size === allCniStatusOptions.length;
  const allSitesVisible = hiddenSites.size === 0;
  const allGatewayPoolsVisible = hiddenGatewayPools.size === 0;

  const toggleAllSelection = (
    allSelected: boolean,
    options: string[],
    setter: React.Dispatch<React.SetStateAction<Set<string>>>
  ) => {
    setter(allSelected ? new Set() : new Set(options));
  };

  const resetNodeFilters = () => {
    setNodeNameFilter('');
    onSelectedNodeTypesChange(new Set(allNodeTypeOptions));
    setSelectedK8sStatuses(new Set(allK8sStatusOptions));
    setSelectedCniStatuses(new Set(allCniStatusOptions));
    onShowAllSites();
    onShowAllGatewayPools();
  };

  const toggleSimpleSelection = (
    option: string,
    setter: React.Dispatch<React.SetStateAction<Set<string>>>
  ) => {
    setter((prev) => {
      const next = new Set(prev);
      if (next.has(option)) {
        next.delete(option);
      } else {
        next.add(option);
      }
      return next;
    });
  };

  const toggleNodeTypeSelection = (option: string) => {
    onSelectedNodeTypesChange((prev) => {
      const next = new Set(prev);
      if (next.has(option)) {
        next.delete(option);
      } else {
        next.add(option);
      }
      return next;
    });
  };

  const hasTableFiltersApplied = nodeNameFilter.trim().length > 0
    || !allSelectedNodeTypes
    || !allSelectedK8sStatuses
    || !allSelectedCniStatuses
    || !allSitesVisible
    || !allGatewayPoolsVisible;

  const filteredNodes = useMemo(() => {
    const nameNeedle = nodeNameFilter.trim().toLowerCase();
    const warnedStatuses = new Set<string>();
    return nodes.filter((row) => {
      const nodeName = row.name || '';
      const nodeType = getNodeTypeLabel(row);
      const k8sStatus = row.k8sReady || 'NotReady';
      const cniStatus = row.cniStatus || 'Unknown';

      if (nameNeedle && !nodeName.toLowerCase().includes(nameNeedle)) return false;
      if (!selectedNodeTypes.has(nodeType)) return false;
      if (!selectedK8sStatuses.has(k8sStatus)) return false;
      // Always show nodes whose cniStatus doesn't match any known filter option
      if (!allCniStatusOptions.includes(cniStatus)) {
        if (!warnedStatuses.has(cniStatus)) {
          warnedStatuses.add(cniStatus);
          console.warn(`NodesTable: unknown cniStatus "${cniStatus}" not in filter options, showing node anyway`);
        }
        return true;
      }
      if (!selectedCniStatuses.has(cniStatus)) return false;

      return true;
    });
  }, [nodeNameFilter, nodes, selectedCniStatuses, selectedK8sStatuses, selectedNodeTypes]);

  const siteManagedCniMap = useMemo(() => {
    const managedBySite = new Map<string, boolean>();
    for (const site of sites) {
      if (!site.name) {
        continue;
      }
      if (typeof site.manageCniPlugin === 'boolean') {
        managedBySite.set(site.name, site.manageCniPlugin);
      }
    }
    return managedBySite;
  }, [sites]);

  renderRef.current.count += 1;
  const renderNow = performance.now();
  if (renderNow - renderRef.current.windowStart > 2000) {
    renderRef.current.windowStart = renderNow;
    renderRef.current.count = 1;
  }
  if (renderRef.current.count % 25 === 0) {
    uiDiag(
      'nodes-render',
      'nodes table rendering frequently',
      {
        renderCountIn2sWindow: renderRef.current.count,
        nodes: nodes.length,
        pageSizeMode,
        pageIndex: pagination.pageIndex,
        pageSize: pagination.pageSize
      },
      { minIntervalMs: 200, level: 'warn' }
    );
  }
  const columns = useMemo<ColumnDef<NodeSummary>[]>(
    () => [
      {
        id: 'name',
        header: 'Name',
        accessorFn: (row) => row.name || 'Unknown',
        cell: ({ row }) => row.original.name || 'Unknown'
      },
      {
        id: 'type',
        header: 'Type',
        accessorFn: (row) => {
          const nodeName = row.name || '';
          return (row.isGateway || gatewayByNode.has(nodeName)) ? 'Gateway' : 'Worker';
        },
        cell: ({ row }) => {
          const nodeName = row.original.name || '';
          const isGateway = row.original.isGateway || gatewayByNode.has(nodeName);
          return (
            <div className="cell-center">
              <span className={`badge ${isGateway ? 'info' : 'success'}`}>
                {isGateway ? 'Gateway' : 'Worker'}
              </span>
            </div>
          );
        }
      },
      {
        id: 'site',
        header: 'Site',
        accessorFn: (row) => getSiteBadgeLabel(row),
        cell: ({ row }) => {
          const site = getSiteBadgeLabel(row.original);
          return (
            <div className="cell-center">
              <span className="badge site-pool-badge">{site}</span>
            </div>
          );
        }
      },
      {
        id: 'pool',
        header: 'Pool',
        accessorFn: (row) => getPoolBadgeLabel(row),
        cell: ({ row }) => {
          const pool = getPoolBadgeLabel(row.original);
          if (!pool) {
            return <span></span>;
          }
          return (
            <div className="cell-center">
              <span className="badge site-pool-badge">{pool}</span>
            </div>
          );
        }
      },
      {
        id: 'peers',
        header: 'Peers',
        accessorFn: (row) => row.healthyPeers || 0,
        cell: ({ row }) => {
          const online = row.original.healthyPeers || 0;
          const total = row.original.peerCount || 0;
          return (
            <span style={{ color: getCountColor(online, total) }}>{online}/{total}</span>
          );
        }
      },
      {
        id: 'k8sStatus',
        header: 'K8s Status',
        accessorFn: (row) => row.k8sReady || 'NotReady',
        cell: ({ row }) => {
          const status = row.original.k8sReady || 'NotReady';
          const tone = status === 'Ready' ? 'success' : 'danger';
          return (
            <div className="cell-center">
              <span className={`badge ${tone}`}>
                {status}
              </span>
            </div>
          );
        }
      },
      {
        id: 'cniStatus',
        header: 'UN Status',
        accessorFn: (row) => row.cniStatus || 'Unknown',
        cell: ({ row }) => {
          const label = row.original.cniStatus || 'Unknown';
          const tone = row.original.cniTone || 'warning';
          const fetchErr = row.original.fetchError;
          const title = fetchErr ? `Fetch error: ${fetchErr}` : undefined;
          return (
            <div className="cell-center">
              <span className={`badge ${tone}`} title={title}>
                {label}
              </span>
            </div>
          );
        }
      },
      {
        id: 'cniManaged',
        header: 'CNI',
        accessorFn: (row) => {
          if ((row.errorCount || 0) > 0) {
            return 2;
          }
          const siteName = row.siteName || '';
          if (!siteName) {
            return 0;
          }
          return siteManagedCniMap.get(siteName) ? 1 : 0;
        },
        cell: ({ row }) => {
          if ((row.original.errorCount || 0) > 0) {
            return (
              <div className="cell-center">
                <span
                  className="route-presence route-presence-warning"
                  title={`${row.original.errorCount} error(s)`}
                >
                  !
                </span>
              </div>
            );
          }
          const siteName = row.original.siteName || '';
          if (!siteName) {
            return <div className="cell-center">-</div>;
          }
          const managed = siteManagedCniMap.get(siteName);
          if (managed === true) {
            return (
              <div className="cell-center">
                <span className="route-presence route-presence-success">&#10003;</span>
              </div>
            );
          }
          return <div className="cell-center">-</div>;
        }
      },
      {
        id: 'lastUpdate',
        header: 'Last Update',
        accessorFn: (row) => {
          const src = row.statusSource || '';
          return (src === 'ws' || src === 'apiserver-ws') ? 'Live' : src || '-';
        },
        cell: ({ row }) => {
          const src = row.original.statusSource || '';
          if (src === 'ws' || src === 'apiserver-ws') {
            const viaAPIServer = src === 'apiserver-ws';
            return (
              <span
                className={`live-indicator${viaAPIServer ? ' warning' : ''}`}
                title={viaAPIServer ? 'Streaming via API server fallback WebSocket' : 'Streaming via WebSocket'}
              >
                <span className="live-indicator-dot" aria-hidden="true"></span>
                Live
              </span>
            );
          }
          return <span>{src || '-'}</span>;
        }
      }
    ],
    [gatewayByNode, siteManagedCniMap]
  );

  const table = useReactTable({
    data: filteredNodes,
    columns,
    autoResetPageIndex: false,
    state: {
      sorting,
      pagination
    },
    onSortingChange: setSorting,
    onPaginationChange: (updater) => {
      setPagination((prev) => {
        const next = typeof updater === 'function' ? updater(prev) : updater;
        const unchanged = prev.pageIndex === next.pageIndex && prev.pageSize === next.pageSize;
        if (unchanged) {
          identicalPaginationUpdateRef.current += 1;
          if (identicalPaginationUpdateRef.current % 25 === 0) {
            uiDiag(
              'nodes-pagination-identical-loop',
              'repeated identical pagination updates observed',
              {
                count: identicalPaginationUpdateRef.current,
                pageIndex: prev.pageIndex,
                pageSize: prev.pageSize,
                pageSizeMode
              },
              { minIntervalMs: 200, level: 'warn' }
            );
          }
          return prev;
        }

        identicalPaginationUpdateRef.current = 0;
        uiDiag(
          'nodes-pagination-change',
          'table requested pagination change',
          {
            prevPageIndex: prev.pageIndex,
            prevPageSize: prev.pageSize,
            nextPageIndex: next.pageIndex,
            nextPageSize: next.pageSize,
            pageSizeMode
          },
          { minIntervalMs: 200 }
        );
        return next;
      });
    },
    getCoreRowModel: getCoreRowModel(),
    getSortedRowModel: getSortedRowModel(),
    getPaginationRowModel: getPaginationRowModel()
  });

  const filteredCount = filteredNodes.length;
  const pageCount = table.getPageCount();
  const pageIndex = table.getState().pagination.pageIndex;
  const pageSize = table.getState().pagination.pageSize;
  const displayPageCount = pageCount || 1;

  useEffect(() => {
    uiDiag(
      'nodes-pagination-state',
      'nodes pagination state snapshot',
      {
        filteredCount,
        pageCount,
        pageIndex,
        pageSize,
        pageSizeMode
      },
      { minIntervalMs: 400 }
    );
  }, [filteredCount, pageCount, pageIndex, pageSize, pageSizeMode]);

  useEffect(() => {
    setPagination((prev) => (prev.pageIndex === 0 ? prev : { ...prev, pageIndex: 0 }));
  }, [nodeNameFilter, selectedNodeTypes, selectedK8sStatuses, selectedCniStatuses, hiddenSites, hiddenGatewayPools]);

  useEffect(() => {
    setPagination((prev) => {
      if (pageCount <= 0 || prev.pageIndex < pageCount) {
        return prev;
      }
      uiDiag(
        'nodes-page-clamp',
        'clamping page index to page count',
        {
          prevPageIndex: prev.pageIndex,
          newPageIndex: pageCount - 1,
          pageCount
        },
        { minIntervalMs: 200 }
      );
      return { ...prev, pageIndex: pageCount - 1 };
    });
  }, [pageCount]);

  useEffect(() => {
    if (pageSizeMode !== 'auto') {
      setPagination((prev) =>
        prev.pageSize === pageSizeMode ? prev : { ...prev, pageSize: pageSizeMode }
      );
      uiDiag(
        'nodes-page-size-mode',
        'manual page size mode active',
        { pageSizeMode },
        { minIntervalMs: 250 }
      );
      return;
    }
    const wrapper = tableWrapperRef.current;
    if (!wrapper) return;

    let rafId = 0;
    let pending = false;

    const updateAutoPageSize = () => {
      if (pending) return;
      pending = true;
      rafId = window.requestAnimationFrame(() => {
        pending = false;

        const header = wrapper.querySelector('thead');
        const headerHeight = header instanceof HTMLElement ? header.getBoundingClientRect().height : 0;
        const rowHeights = Array.from(wrapper.querySelectorAll('tbody tr'))
          .slice(0, 5)
          .map((row) => (row instanceof HTMLElement ? row.getBoundingClientRect().height : 0))
          .filter((height) => height > 0);
        const measuredRowHeight = rowHeights.length > 0
          ? Math.max(24, rowHeights.reduce((sum, h) => sum + h, 0) / rowHeights.length)
          : 42;

        // Use viewport space rather than the wrapper's current rendered height.
        // This keeps auto page size stable even when table contents shrink (or become empty).
        const wrapperTop = wrapper.getBoundingClientRect().top;
        const bottomPadding = 24; // aligns with app bottom spacing
        const available = Math.max(0, window.innerHeight - wrapperTop - bottomPadding - headerHeight);

        const nextSize = Math.max(10, Math.floor(available / measuredRowHeight));
        setAutoPageSize(nextSize);
        uiDiag(
          'nodes-auto-page-size',
          'auto page size evaluated',
          {
            headerHeight,
            measuredRowHeight,
            available,
            nextSize
          },
          { minIntervalMs: 400 }
        );
        setPagination((prev) =>
          prev.pageSize === nextSize ? prev : { ...prev, pageSize: nextSize }
        );
      });
    };

    updateAutoPageSize();
    const resizeObserver = new ResizeObserver(updateAutoPageSize);
    resizeObserver.observe(wrapper);

    window.addEventListener('resize', updateAutoPageSize);
    return () => {
      if (rafId) {
        window.cancelAnimationFrame(rafId);
      }
      resizeObserver.disconnect();
      window.removeEventListener('resize', updateAutoPageSize);
    };
  }, [pageSizeMode, filteredCount]);

  return (
    <div className="card nodes-card">
      <div className="card-header">
        <div className="section-title">Nodes</div>
        <div className="table-header-actions">
          <div className="pagination">
            <span className="pagination-info">
              {filteredCount === 0 ? '0' : pageIndex * pageSize + 1}
              -{Math.min(filteredCount, (pageIndex + 1) * pageSize)} of {filteredCount}
            </span>
            <div className="filter-menu-anchor">
              <TableFilterButton
                ref={filterButtonRef}
                active={hasTableFiltersApplied}
                open={filterPopoverOpen}
                onToggle={() => setFilterPopoverOpen((open) => !open)}
                title="Node table filters"
              />
              {filterPopoverOpen && (
                <div className="filter-popover" ref={filterPopoverRef}>
                  <div className="filter-popover-first-row">
                    <div className="filter-section filter-first-row-section">
                      <div className="filter-section-title">Node Name</div>
                      <input
                        className="input filter-search-input"
                        placeholder="Search nodes"
                        value={nodeNameFilter}
                        onChange={(event) => setNodeNameFilter(event.target.value)}
                      />
                    </div>
                    <button
                      type="button"
                      className="button filter-reset-button filter-reset-docked"
                      onClick={resetNodeFilters}
                    >
                      Reset
                    </button>
                  </div>

                  <div className="filter-section">
                    <div className="filter-section-title">Node Type</div>
                    <div className="filter-badge-row">
                      <button
                        type="button"
                        className="filter-badge-button"
                        onClick={() => toggleAllSelection(allSelectedNodeTypes, allNodeTypeOptions, onSelectedNodeTypesChange)}
                        style={{ opacity: allSelectedNodeTypes ? 1 : 0.5 }}
                      >
                        <span className="badge all">All</span>
                      </button>
                      {allNodeTypeOptions.map((option) => (
                        <button
                          key={option}
                          type="button"
                          className="filter-badge-button"
                          onClick={() => toggleNodeTypeSelection(option)}
                          style={{ opacity: selectedNodeTypes.has(option) ? 1 : 0.5 }}
                        >
                          <span className={`badge ${option === 'Gateway' ? 'info' : 'success'}`}>{option}</span>
                        </button>
                      ))}
                    </div>
                  </div>

                  <div className="filter-section">
                    <div className="filter-section-title">Site / Pool</div>
                    <div className="filter-badge-row">
                      <button
                        type="button"
                        className="filter-badge-button"
                        onClick={() => {
                          if (allSitesVisible && allGatewayPoolsVisible) {
                            for (const site of sites) {
                              const siteName = site.name || '';
                              if (siteName && !hiddenSites.has(siteName)) {
                                onToggleSite(siteName);
                              }
                            }
                            for (const pool of gatewayPools) {
                              const poolName = pool.name || '';
                              if (poolName && !hiddenGatewayPools.has(poolName)) {
                                onToggleGatewayPool(poolName);
                              }
                            }
                          } else {
                            onShowAllSites();
                            onShowAllGatewayPools();
                          }
                        }}
                        style={{ opacity: allSitesVisible && allGatewayPoolsVisible ? 1 : 0.5 }}
                      >
                        <span className="badge all">All</span>
                      </button>
                      {sites.map((site) => {
                        const counts = siteCounts.get(site.name || '') || { online: 0, total: 0 };
                        const online = counts.online;
                        const total = counts.total;
                        const statusTone = online === 0 && total > 0 ? 'danger' : online < total ? 'warning' : 'success';
                        const visible = !hiddenSites.has(site.name || '');
                        return (
                          <button
                            key={`site-${site.name}`}
                            type="button"
                            className="filter-badge-button"
                            onClick={() => onToggleSite(site.name || '')}
                            style={{ opacity: visible ? 1 : 0.5 }}
                          >
                            <span className={`badge ${statusTone}`}>{site.name}</span>
                          </button>
                        );
                      })}
                      {gatewayPools.map((pool) => {
                        const poolName = pool.name || '';
                        const counts = poolCounts.get(poolName) || { online: 0, total: 0 };
                        const total = counts.total;
                        const online = counts.online;
                        const tone = getGatewayPoolBadgeTone(total, online);
                        const visible = !hiddenGatewayPools.has(poolName);
                        return (
                          <button
                            key={`pool-${poolName}`}
                            type="button"
                            className="filter-badge-button"
                            onClick={() => onToggleGatewayPool(poolName)}
                            style={{ opacity: visible ? 1 : 0.5 }}
                          >
                            <span className={`badge${tone ? ` ${tone}` : ''}`}>{poolName}</span>
                          </button>
                        );
                      })}
                    </div>
                  </div>

                  <div className="filter-section">
                    <div className="filter-section-title">K8s Status</div>
                    <div className="filter-badge-row">
                      <button
                        type="button"
                        className="filter-badge-button"
                        onClick={() => toggleAllSelection(allSelectedK8sStatuses, allK8sStatusOptions, setSelectedK8sStatuses)}
                        style={{ opacity: allSelectedK8sStatuses ? 1 : 0.5 }}
                      >
                        <span className="badge all">All</span>
                      </button>
                      {allK8sStatusOptions.map((option) => (
                        <button
                          key={option}
                          type="button"
                          className="filter-badge-button"
                          onClick={() => toggleSimpleSelection(option, setSelectedK8sStatuses)}
                          style={{ opacity: selectedK8sStatuses.has(option) ? 1 : 0.5 }}
                        >
                          <span className={`badge ${option === 'Ready' ? 'success' : 'danger'}`}>{option}</span>
                        </button>
                      ))}
                    </div>
                  </div>

                  <div className="filter-section">
                    <div className="filter-section-title">UN Status</div>
                    <div className="filter-badge-row">
                      <button
                        type="button"
                        className="filter-badge-button"
                        onClick={() => toggleAllSelection(allSelectedCniStatuses, allCniStatusOptions, setSelectedCniStatuses)}
                        style={{ opacity: allSelectedCniStatuses ? 1 : 0.5 }}
                      >
                        <span className="badge all">All</span>
                      </button>
                      {allCniStatusOptions.map((option) => {
                        const tone = option === 'Healthy'
                          ? 'success'
                          : option === 'Route mismatch' || option === 'Stale' || option === 'Unknown'
                            ? 'warning'
                            : option === 'Errors' || option === 'Fetch error' || option === 'Health check failing'
                              ? 'danger'
                              : '';
                        return (
                          <button
                            key={option}
                            type="button"
                            className="filter-badge-button"
                            onClick={() => toggleSimpleSelection(option, setSelectedCniStatuses)}
                            style={{ opacity: selectedCniStatuses.has(option) ? 1 : 0.5 }}
                          >
                            <span className={`badge${tone ? ` ${tone}` : ''}`}>{option}</span>
                          </button>
                        );
                      })}
                    </div>
                  </div>
                </div>
              )}
            </div>
            <select
              className="select"
              value={pageSizeMode === 'auto' ? 'auto' : String(pageSizeMode)}
              onChange={(event) => {
                const value = event.target.value;
                if (value === 'auto') {
                  uiDiag('nodes-page-size-select', 'page size mode changed', { value: 'auto' }, { minIntervalMs: 1 });
                  setPageSizeMode('auto');
                } else {
                  const size = Number(value);
                  uiDiag('nodes-page-size-select', 'page size mode changed', { value: size }, { minIntervalMs: 1 });
                  setPageSizeMode(size);
                  table.setPageSize(size);
                }
              }}
            >
              <option value="auto">Auto ({autoPageSize} / page)</option>
              {[10, 25, 50, 100, 250].map((size) => (
                <option key={size} value={size}>{size} / page</option>
              ))}
              <option value="99999">All</option>
            </select>
            <button
              className="button"
              onClick={() => table.previousPage()}
              disabled={!table.getCanPreviousPage()}
            >
              Prev
            </button>
            <button
              className="button"
              onClick={() => {
                const input = window.prompt(
                  `Go to page (1-${displayPageCount})`,
                  String(pageIndex + 1)
                );
                if (input == null) return;
                const nextPage = Number.parseInt(input.trim(), 10);
                if (!Number.isFinite(nextPage)) return;
                const clampedPage = Math.min(Math.max(nextPage, 1), displayPageCount);
                table.setPageIndex(clampedPage - 1);
              }}
              disabled={filteredCount === 0 || displayPageCount <= 1}
              title="Go to page"
            >
              Page {pageIndex + 1} / {displayPageCount}
            </button>
            <button
              className="button"
              onClick={() => table.nextPage()}
              disabled={!table.getCanNextPage()}
            >
              Next
            </button>
          </div>
          <button
            className="button zoom-action-button"
            onClick={onToggleMaximize}
            aria-label={isMaximized ? 'Restore' : 'Maximize'}
            title={isMaximized ? 'Restore' : 'Maximize'}
          >
            {isMaximized ? <CloseXIcon /> : <MagnifyPlusIcon />}
          </button>
        </div>
      </div>
      <div className="nodes-table-wrapper" ref={tableWrapperRef}>
        <table className="table sticky-table-header">
          <thead>
            {table.getHeaderGroups().map((headerGroup) => (
              <tr key={headerGroup.id}>
                {headerGroup.headers.map((header) => (
                  <th
                    key={header.id}
                    onClick={header.column.getToggleSortingHandler()}
                    style={{
                      cursor: header.column.getCanSort() ? 'pointer' : 'default',
                      position: header.column.getCanSort() ? 'relative' : undefined
                    }}
                  >
                    {flexRender(header.column.columnDef.header, header.getContext())}
                    {header.column.getCanSort() && (
                      <span
                        style={{
                          position: 'absolute',
                          right: 6,
                          top: '50%',
                          transform: 'translateY(-50%)',
                          width: 10,
                          textAlign: 'center',
                          pointerEvents: 'none'
                        }}
                      >
                        {header.column.getIsSorted() === 'asc'
                          ? '↑'
                          : header.column.getIsSorted() === 'desc'
                            ? '↓'
                            : ''}
                      </span>
                    )}
                  </th>
                ))}
              </tr>
            ))}
          </thead>
          <tbody>
            {table.getRowModel().rows.map((row) => (
              <tr key={row.id} onClick={() => onSelect(row.original.name || '')} style={{ cursor: 'pointer' }}>
                {row.getVisibleCells().map((cell) => (
                  <td key={cell.id}>{flexRender(cell.column.columnDef.cell, cell.getContext())}</td>
                ))}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}


export default NodesTable;
