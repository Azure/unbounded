// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

import * as React from 'react';
import { Suspense, useCallback, useEffect, useMemo, useRef, useState } from 'react';
import NodeTable from './components/nodes/NodesTable';
import SitesCard from './components/network/SitesCard';
import StatusJsonModal from './components/status/StatusJsonModal';
import ErrorBoundary from './components/common/ErrorBoundary';
import {
  formatTime
} from './components/nodes/shared/index';
import useClusterStatus from './hooks/useClusterStatus';
import useDashboardData from './hooks/useDashboardData';
import useNodeDetails from './hooks/useNodeDetails';

const NodeDetailModal = React.lazy(() => import('./components/nodes/NodeDetailDialog'));

export default function App() {
  const {
    summary, loading, error, wsConnected, sendWsMessage
  } = useClusterStatus();
  const sites = summary?.sites || [];
  const gatewayPools = summary?.gatewayPools || [];
  const nodeSummaries = summary?.nodeSummaries || [];
  const [hiddenSites, setHiddenSites] = useState<Set<string>>(new Set());
  const [hiddenGatewayPools, setHiddenGatewayPools] = useState<Set<string>>(new Set());
  const [selectedNodeName, setSelectedNodeName] = useState<string | null>(null);
  const { detail, load: loadNodeDetail, cancel: cancelNodeDetail } = useNodeDetails(selectedNodeName, nodeSummaries);
  const [selectedNodeDetailTab, setSelectedNodeDetailTab] = useState<'peerings' | 'routes' | 'bpf'>('peerings');
  const [pullEnabledOptimistic, setPullEnabledOptimistic] = useState<boolean | null>(null);
  const [selectedNodeTypesFilter, setSelectedNodeTypesFilter] = useState<Set<string>>(new Set(['Gateway', 'Worker']));
  const [maximizedPanel, setMaximizedPanel] = useState<'nodes' | null>(null);
  const [infoOpen, setInfoOpen] = useState(false);
  const [statusJsonOpen, setStatusJsonOpen] = useState(false);
  const [errorsDismissed, setErrorsDismissed] = useState(false);
  const [warningsDismissed, setWarningsDismissed] = useState(false);
  const prevErrorsRef = useRef<string>('');
  const prevWarningsRef = useRef<string>('');
  const infoPanelRef = useRef<HTMLDivElement | null>(null);
  const [theme, setTheme] = useState<'dark' | 'light'>(() => {
    if (typeof window === 'undefined') return 'dark';
    const stored = window.localStorage.getItem('theme');
    return stored === 'light' ? 'light' : 'dark';
  });

  useEffect(() => {
    document.documentElement.dataset.theme = theme;
    window.localStorage.setItem('theme', theme);
  }, [theme]);

  useEffect(() => {
    if (!maximizedPanel) return;
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') {
        setMaximizedPanel(null);
      }
    };
    window.addEventListener('keydown', onKeyDown);
    return () => window.removeEventListener('keydown', onKeyDown);
  }, [maximizedPanel]);

  useEffect(() => {
    if (!infoOpen) return;
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') {
        setInfoOpen(false);
      }
    };
    window.addEventListener('keydown', onKeyDown);
    infoPanelRef.current?.focus();
    return () => window.removeEventListener('keydown', onKeyDown);
  }, [infoOpen]);

  useEffect(() => {
    if (!selectedNodeName) return;
    setInfoOpen(false);
  }, [selectedNodeName]);

  useEffect(() => {
    if (!statusJsonOpen) return;
    setInfoOpen(false);
  }, [statusJsonOpen]);

  useEffect(() => {
    if (pullEnabledOptimistic === null) return;
    const pullEnabled = summary?.pullEnabled;
    if (typeof pullEnabled === 'boolean' && pullEnabled === pullEnabledOptimistic) {
      setPullEnabledOptimistic(null);
    }
  }, [summary?.pullEnabled, pullEnabledOptimistic]);

  useEffect(() => {
    if (pullEnabledOptimistic === null) return;
    const timer = window.setTimeout(() => {
      setPullEnabledOptimistic(null);
    }, 4000);
    return () => window.clearTimeout(timer);
  }, [pullEnabledOptimistic]);

  // Combine fetch errors and controller-reported errors into one sorted list.
  const activeErrors = useMemo(() => {
    const items: string[] = [];
    if (error) items.push(`controller: ${error}`);
    const errors = summary?.errors || [];
    for (const msg of errors) {
      items.push(`controller: ${msg}`);
    }
    items.sort();
    return items;
  }, [error, summary?.errors]);
  const activeWarnings = useMemo(() => {
    const items: string[] = [];
    const warnings = summary?.warnings || [];
    for (const msg of warnings) {
      items.push(`controller: ${msg}`);
    }
    // Summary errors stay independent of explicitly loaded diagnostics.
    for (const ns of nodeSummaries) {
      const count = ns.errorCount || 0;
      if (count === 1 && ns.firstError) {
        items.push(`node ${ns.name || 'unknown'}: ${ns.firstError}`);
      } else if (count > 0) {
        items.push(`node ${ns.name || 'unknown'}: ${count} error(s)`);
      }
    }
    items.sort();
    return items;
  }, [summary?.warnings, nodeSummaries]);

  useEffect(() => {
    const key = activeErrors.join('\n');
    if (key !== prevErrorsRef.current) {
      prevErrorsRef.current = key;
      if (activeErrors.length > 0) {
        setErrorsDismissed(false);
      }
    }
  }, [activeErrors]);

  useEffect(() => {
    const key = activeWarnings.join('\n');
    if (key !== prevWarningsRef.current) {
      prevWarningsRef.current = key;
    }
  }, [activeWarnings]);

  const toggleSite = (name: string) => {
    setHiddenSites((prev) => {
      const next = new Set(prev);
      if (next.has(name)) {
        next.delete(name);
      } else {
        next.add(name);
      }
      return next;
    });
  };

  const showAllSites = () => {
    setHiddenSites(new Set());
  };

  const toggleGatewayPool = (name: string) => {
    setHiddenGatewayPools((prev) => {
      const next = new Set(prev);
      if (next.has(name)) {
        next.delete(name);
      } else {
        next.add(name);
      }
      return next;
    });
  };

  const showAllGatewayPools = () => {
    setHiddenGatewayPools(new Set());
  };

  const handleSelectNode = useCallback((nodeName: string) => {
    cancelNodeDetail();
    setSelectedNodeName(nodeName);
  }, [cancelNodeDetail]);

  const handleCloseModal = useCallback(() => {
    cancelNodeDetail();
    setSelectedNodeName(null);
  }, [cancelNodeDetail]);

  const {
    effectivePullEnabled,
    gatewayByNode,
    nodeK8sStatusMap,
    nodeHealthyCount,
    nodeTotalCount,
    peerHealth,
    poolCounts,
    siteCounts,
    visibleNodeSummaries
  } = useDashboardData({
    summary,
    nodeSummaries,
    gatewayPools,
    gatewayPoolHiddenNames: hiddenGatewayPools,
    hiddenSites,
    selectedNodeTypesFilter,
    pullEnabledOptimistic
  });

  // All known node names for the detail modal's peer navigation
  const allNodeNames = useMemo(() => {
    const names = nodeSummaries.map((ns) => ns.name || '').filter(Boolean);
    return names;
  }, [nodeSummaries]);

  useEffect(() => {
    if (!selectedNodeName) return;
    // Check if node still exists in the cluster
    const exists = nodeSummaries.some((ns) => ns.name === selectedNodeName);
    if (!exists) {
      handleCloseModal();
    }
  }, [nodeSummaries, selectedNodeName, handleCloseModal]);

  const wsState = wsConnected ? 'ok' : summary ? 'warn' : 'err';
  const wsLabel = wsConnected
    ? 'WebSocket connected'
    : summary
      ? 'Polling only'
      : 'No data';

  const renderNodesCard = (isMaximized: boolean) => {
    const content = (
      <NodeTable
        nodes={visibleNodeSummaries}
        sites={sites}
        gatewayPools={gatewayPools}
        hiddenSites={hiddenSites}
        hiddenGatewayPools={hiddenGatewayPools}
        siteCounts={siteCounts}
        poolCounts={poolCounts}
        onToggleSite={toggleSite}
        onShowAllSites={showAllSites}
        onToggleGatewayPool={toggleGatewayPool}
        onShowAllGatewayPools={showAllGatewayPools}
        gatewayByNode={gatewayByNode}
        selectedNodeTypes={selectedNodeTypesFilter}
        onSelectedNodeTypesChange={setSelectedNodeTypesFilter}
        pullEnabled={effectivePullEnabled}
        onSelect={handleSelectNode}
        onToggleMaximize={() => setMaximizedPanel(isMaximized ? null : 'nodes')}
        isMaximized={isMaximized}
      />
    );
    if (!isMaximized) return content;
    return <div className="card-maximized">{content}</div>;
  };

  const buildInfo = summary?.buildInfo;
  const leaderInfo = summary?.leaderInfo;
  const timestamp = summary?.timestamp;

  if (maximizedPanel) {
    return (
      <div className="app app-maximized">
        <div className="maximize-backdrop" onClick={() => setMaximizedPanel(null)}></div>
        {renderNodesCard(true)}
        <Suspense fallback={null}>
          <NodeDetailModal
            nodeName={selectedNodeName}
            summary={nodeSummaries.find((node) => node.name === selectedNodeName)}
            detail={detail}
            onLoad={loadNodeDetail}
            allNodeNames={allNodeNames}
            gatewayByNode={gatewayByNode}
            nodeK8sStatusMap={nodeK8sStatusMap}
            azureTenantId={summary?.azureTenantId}
            pullEnabled={effectivePullEnabled}
            theme={theme}
            detailTab={selectedNodeDetailTab}
            onDetailTabChange={setSelectedNodeDetailTab}
            onSelectNode={handleSelectNode}
            onClose={handleCloseModal}
          />
        </Suspense>
      </div>
    );
  }

  return (
    <div className="app">
      {infoOpen && (
        <div className="info-backdrop" onClick={() => setInfoOpen(false)}></div>
      )}
      <div className="header">
        <h1>Unbounded Net Cluster Status</h1>
        <div className="header-info">
          {activeErrors.length > 0 && (
            <button
              className="button button-error"
              onClick={() => setErrorsDismissed(!errorsDismissed)}
              title={errorsDismissed ? 'Show errors' : 'Hide errors'}
            >
              Errors ({activeErrors.length})
            </button>
          )}
          {activeWarnings.length > 0 && (
            <button
              className="button button-warning"
              onClick={() => setWarningsDismissed(!warningsDismissed)}
              title={warningsDismissed ? 'Show warnings' : 'Hide warnings'}
            >
              Warnings ({activeWarnings.length})
            </button>
          )}
          <button
            className="button"
            onClick={() => setStatusJsonOpen(true)}
            aria-label="Open cluster status JSON"
            title="Open cluster status JSON"
          >
            JSON
          </button>
          <button
            className={`pull-toggle ${effectivePullEnabled ? 'active' : ''}`}
            onClick={() => {
              const next = !effectivePullEnabled;
              setPullEnabledOptimistic(next);
              sendWsMessage({ type: 'set_pull_enabled', enabled: next });
            }}
            aria-label="Toggle pull mode"
            title={effectivePullEnabled ? 'Pull mode enabled' : 'Pull mode disabled'}
          >
            <svg viewBox="0 0 24 24" aria-hidden="true" width="18" height="18">
              <path d="M12 4v12m0 0l-4-4m4 4l4-4M5 20h14" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" fill="none" />
            </svg>
            <span className="pull-toggle-label">Pull</span>
          </button>
          <button
            className="theme-toggle"
            onClick={() => setTheme(theme === 'dark' ? 'light' : 'dark')}
            aria-label="Toggle theme"
            title="Toggle theme"
          >
            <span className={`theme-toggle-item ${theme === 'light' ? 'active' : ''}`}>
              <svg viewBox="0 0 24 24" aria-hidden="true">
                <circle cx="12" cy="12" r="4" fill="currentColor" />
                <path
                  d="M12 2v3M12 19v3M4.22 4.22l2.12 2.12M17.66 17.66l2.12 2.12M2 12h3M19 12h3M4.22 19.78l2.12-2.12M17.66 6.34l2.12-2.12"
                  stroke="currentColor"
                  strokeWidth="1.5"
                  strokeLinecap="round"
                  fill="none"
                />
              </svg>
            </span>
            <span className={`theme-toggle-item ${theme === 'dark' ? 'active' : ''}`}>
              <svg viewBox="0 0 24 24" aria-hidden="true">
                <path
                  d="M21 14.5A8.5 8.5 0 0 1 9.5 3a6.5 6.5 0 1 0 11.5 11.5Z"
                  fill="currentColor"
                />
              </svg>
            </span>
          </button>
          {(buildInfo?.version || leaderInfo?.podName || leaderInfo?.nodeName) && (
            <div className="info-container">
              <button
                className={`info-button ${wsState}`}
                onClick={() => setInfoOpen((open) => !open)}
                aria-label="Toggle build info"
                title="Toggle build info"
              >
                <span className="info-icon">i</span>
              </button>
              {infoOpen && (
                <div
                  className="about-panel info-popover"
                  onClick={(event) => event.stopPropagation()}
                  ref={infoPanelRef}
                  tabIndex={-1}
                >
                  <table className="info-table">
                    <tbody>
                      <tr>
                        <td>Status</td>
                        <td>{wsLabel}</td>
                      </tr>
                      {!wsConnected && timestamp && (
                        <tr>
                          <td>Last Updated</td>
                          <td>{formatTime(timestamp)}</td>
                        </tr>
                      )}
                      {buildInfo?.version && (
                        <tr>
                          <td>Version</td>
                          <td>{buildInfo.version}</td>
                        </tr>
                      )}
                      {buildInfo?.commit && (
                        <tr>
                          <td>Commit</td>
                          <td>{buildInfo.commit}</td>
                        </tr>
                      )}
                      {buildInfo?.buildTime && (
                        <tr>
                          <td>Build Time</td>
                          <td>{buildInfo.buildTime}</td>
                        </tr>
                      )}
                      {leaderInfo?.podName && (
                        <tr>
                          <td>Leader Pod</td>
                          <td>{leaderInfo.podName}</td>
                        </tr>
                      )}
                      {leaderInfo?.nodeName && (
                        <tr>
                          <td>Leader Node</td>
                          <td>{leaderInfo.nodeName}</td>
                        </tr>
                      )}
                    </tbody>
                  </table>
                </div>
              )}
            </div>
          )}
        </div>
      </div>

      <ErrorBoundary>
        {!errorsDismissed && activeErrors.length > 0 && (
          <div className="card error-panel">
            <div className="alert-panel-header">
              <div className="section-title">Errors</div>
            </div>
            <ul className="alert-list">
              {activeErrors.map((msg, i) => <li key={i}>{msg}</li>)}
            </ul>
          </div>
        )}
        {!warningsDismissed && activeWarnings.length > 0 && (
          <div className="card warning-panel">
            <div className="alert-panel-header">
              <div className="section-title">Warnings</div>
            </div>
            <ul className="alert-list">
              {activeWarnings.map((msg, i) => <li key={i}>{msg}</li>)}
            </ul>
          </div>
        )}

        <div className="grid-two">
          <div>
            <div className="card">
              <div className="section-title">Overview</div>
              <div className="overview-grid">
                <div className="card overview-metric-card">
                  <div className="section-title overview-metric-title">Sites</div>
                  <div className="overview-metric-value">{(summary?.siteCount ?? sites.length).toLocaleString()}</div>
                </div>
                <div className="card overview-metric-card">
                  <div className="section-title overview-metric-title">Gateway Pools</div>
                  <div className="overview-metric-value">{gatewayPools.length.toLocaleString()}</div>
                </div>
                <div className="card overview-metric-card">
                  <div className="section-title overview-metric-title">Nodes</div>
                  <div className="overview-metric-value">{nodeHealthyCount.toLocaleString()}<wbr />/{nodeTotalCount.toLocaleString()}</div>
                </div>
                <div className="card overview-metric-card">
                  <div className="section-title overview-metric-title">Peerings</div>
                  <div className="overview-metric-value">{peerHealth.healthy.toLocaleString()}<wbr />/{peerHealth.total.toLocaleString()}</div>
                </div>
              </div>
            </div>
            <SitesCard
              sites={sites}
              siteCounts={siteCounts}
              nodeSummaries={nodeSummaries}
            />
          </div>
          <div>
            {loading && <div className="card">Loading...</div>}
            {!loading && renderNodesCard(false)}
          </div>
        </div>
        <Suspense fallback={null}>
          <NodeDetailModal
            nodeName={selectedNodeName}
            summary={nodeSummaries.find((node) => node.name === selectedNodeName)}
            detail={detail}
            onLoad={loadNodeDetail}
            allNodeNames={allNodeNames}
            gatewayByNode={gatewayByNode}
            nodeK8sStatusMap={nodeK8sStatusMap}
            azureTenantId={summary?.azureTenantId}
            pullEnabled={effectivePullEnabled}
            theme={theme}
            detailTab={selectedNodeDetailTab}
            onDetailTabChange={setSelectedNodeDetailTab}
            onSelectNode={handleSelectNode}
            onClose={handleCloseModal}
          />
        </Suspense>
        <StatusJsonModal
          open={statusJsonOpen}
          onClose={() => setStatusJsonOpen(false)}
        />
      </ErrorBoundary>
    </div>
  );
}
