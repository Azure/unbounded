// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import React, { Suspense } from 'react';
import { CloseXIcon, MagnifyPlusIcon } from '../nodes/shared/index';
import ConnectivityHeatmap from '../network/ConnectivityHeatmap';
const Topology = React.lazy(() => import('../network/Topology'));
import { GatewayPoolStatus, NodeStatus, PeeringStatus, SiteMatrix, SiteStatus } from '../../types';

type NetworkTab = 'siteTopology' | 'matrix';

type NetworkCardProps = {
  activeNetworkTab: NetworkTab;
  edgeHealthCheckCounts: Map<string, { up: number; total: number }>;
  gatewayByNode: Map<string, string>;
  gatewayPools: GatewayPoolStatus[];
  hiddenGatewayPools: Set<string>;
  hiddenSites: Set<string>;
  isMaximized: boolean;
  nodeStatuses: NodeStatus[];
  peerings: PeeringStatus[];
  poolCounts: Map<string, { online: number; total: number }>;
  poolToSite: Map<string, string>;
  siteCounts: Map<string, { online: number; total: number }>;
  sites: SiteStatus[];
  statusMatrix?: Record<string, SiteMatrix>;
  theme: 'dark' | 'light';
  onSelectTab: (tab: NetworkTab) => void;
  onToggleMaximize: () => void;
};

function NetworkCard({
  activeNetworkTab,
  edgeHealthCheckCounts,
  gatewayByNode,
  gatewayPools,
  hiddenGatewayPools,
  hiddenSites,
  isMaximized,
  nodeStatuses,
  peerings,
  poolCounts,
  poolToSite,
  siteCounts,
  sites,
  statusMatrix,
  theme,
  onSelectTab,
  onToggleMaximize
}: NetworkCardProps) {
  return (
    <div className={`card network-card ${isMaximized ? 'card-maximized' : ''}`}>
      <div className="tabs-row detail-tabs-row">
        <div className="tabs detail-tabs">
          <button
            className={`tab-button ${activeNetworkTab === 'siteTopology' ? 'active' : ''}`}
            onClick={() => onSelectTab('siteTopology')}
          >
            Site Topology
          </button>
          <button
            className={`tab-button ${activeNetworkTab === 'matrix' ? 'active' : ''}`}
            onClick={() => onSelectTab('matrix')}
          >
            Connectivity Matrix
          </button>
        </div>
        <div className="detail-tabs-controls">
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
      {activeNetworkTab === 'siteTopology' && (
        <div className="network-panel active">
          <Suspense fallback={<div style={{ padding: '2rem', textAlign: 'center' }}>Loading topology...</div>}>
            <Topology
              mode="sitesAndPools"
              isMaximized={isMaximized}
              sites={sites}
              peerings={peerings}
              nodes={nodeStatuses}
              gatewayPools={gatewayPools}
              gatewayByNode={gatewayByNode}
              hiddenSites={hiddenSites}
              hiddenGatewayPools={hiddenGatewayPools}
              theme={theme}
              siteCounts={siteCounts}
              poolCounts={poolCounts}
              edgeHealthCheckCounts={edgeHealthCheckCounts}
              poolToSite={poolToSite}
            />
          </Suspense>
        </div>
      )}
      {activeNetworkTab === 'matrix' && (
        <div className="network-panel active">
          <ConnectivityHeatmap
            matrix={statusMatrix}
            hiddenSites={hiddenSites}
            hiddenGatewayPools={hiddenGatewayPools}
            sites={sites}
            gatewayPools={gatewayPools}
            siteCounts={siteCounts}
            nodeStatuses={nodeStatuses}
          />
        </div>
      )}
    </div>
  );
}

export default NetworkCard;
