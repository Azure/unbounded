// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

type NodeDetailTabsHeaderProps = {
  detailTab: 'peerings' | 'routes' | 'bpf';
  routesValidationSummary: { total: number; mismatch: number };
  bpfEntryCount: number;
  paginationControls: React.ReactNode;
  onDetailTabChange: (tab: 'peerings' | 'routes' | 'bpf') => void;
};

function NodeDetailTabsHeader({
  detailTab,
  routesValidationSummary,
  bpfEntryCount,
  paginationControls,
  onDetailTabChange
}: NodeDetailTabsHeaderProps) {
  return (
    <div className="tabs-row detail-tabs-row">
      <div className="tabs detail-tabs">
        <button
          className={`tab-button ${detailTab === 'peerings' ? 'active' : ''}`}
          onClick={() => onDetailTabChange('peerings')}
        >
          Peerings
        </button>
        <button
          className={`tab-button ${detailTab === 'routes' ? 'active' : ''}`}
          onClick={() => onDetailTabChange('routes')}
        >
          Routes
          {routesValidationSummary.mismatch > 0 && (
            <span
              className="tab-warning-icon route-presence route-presence-warning"
              title={`${routesValidationSummary.mismatch} mismatched route next hop(s)`}
            >
              ⚠
            </span>
          )}
        </button>
        {bpfEntryCount > 0 && (
          <button
            className={`tab-button ${detailTab === 'bpf' ? 'active' : ''}`}
            onClick={() => onDetailTabChange('bpf')}
          >
            BPF
          </button>
        )}
      </div>
      <div className="detail-tabs-controls">
        {detailTab !== 'peerings' && detailTab !== 'bpf' && (
          <>
            {(() => {
              const summary = routesValidationSummary;
              if (summary.total === 0) {
                return <span className="badge">WG Validation N/A</span>;
              }
              if (summary.mismatch === 0) {
                return null;
              }
              return (
                <span className="badge warning">
                  {summary.mismatch} mismatch
                </span>
              );
            })()}
          </>
        )}
        {paginationControls}
      </div>
    </div>
  );
}

export default NodeDetailTabsHeader;
