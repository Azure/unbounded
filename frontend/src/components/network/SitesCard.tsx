// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { useMemo, useState } from 'react';
import { NodeStatus, NodeSummary, SiteStatus } from '../../types';

type SummaryRow = {
  id: string;
  name: string;
  workers: number;
  gateways: number;
  nodesOnline: number;
  nodesTotal: number;
  state: 'Healthy' | 'Unhealthy' | 'No Data';
};

type SortKey = 'name' | 'workers' | 'gateways' | 'state';

function SitesCard({
  sites,
  siteCounts,
  nodeSummaries,
  nodes
}: {
  sites: SiteStatus[];
  siteCounts: Map<string, { online: number; total: number }>;
  nodeSummaries: NodeSummary[];
  nodes: NodeStatus[];
}) {
  const [sort, setSort] = useState<{ key: SortKey; direction: 'asc' | 'desc' }>({ key: 'name', direction: 'asc' });

  const rows = useMemo<SummaryRow[]>(() => {
    return sites.map((site) => {
      const name = site.name || '-';
      const counts = siteCounts.get(name) || { online: 0, total: 0 };

      let workers = 0;
      let gateways = 0;
      if (nodeSummaries.length > 0) {
        for (const ns of nodeSummaries) {
          if ((ns.siteName || '-') !== name) continue;
          if (ns.isGateway) {
            gateways++;
          } else {
            workers++;
          }
        }
      } else {
        for (const node of nodes) {
          if ((node.nodeInfo?.siteName || '-') !== name) continue;
          if (node.nodeInfo?.isGateway) {
            gateways++;
          } else {
            workers++;
          }
        }
      }

      return {
        id: `site-${name}`,
        name,
        workers,
        gateways,
        nodesOnline: counts.online,
        nodesTotal: counts.total,
        state: counts.total === 0 ? 'No Data' : counts.online === counts.total ? 'Healthy' : 'Unhealthy'
      };
    });
  }, [nodeSummaries, nodes, siteCounts, sites]);

  const sortedRows = useMemo(() => {
    const direction = sort.direction === 'asc' ? 1 : -1;
    const getSortValue = (row: SummaryRow) => {
      switch (sort.key) {
        case 'name':
          return row.name.toLowerCase();
        case 'workers':
          return row.workers.toString().padStart(6, '0');
        case 'gateways':
          return row.gateways.toString().padStart(6, '0');
        case 'state':
          return row.state.toLowerCase();
        default:
          return '';
      }
    };

    return [...rows].sort((left, right) => {
      const leftValue = getSortValue(left);
      const rightValue = getSortValue(right);
      const comparison = String(leftValue).localeCompare(String(rightValue), undefined, { numeric: true, sensitivity: 'base' });
      return comparison * direction;
    });
  }, [rows, sort]);

  const toggleSort = (key: SortKey) => {
    setSort((prev) => {
      if (prev.key !== key) {
        return { key, direction: 'asc' };
      }
      return { key, direction: prev.direction === 'asc' ? 'desc' : 'asc' };
    });
  };

  const sortArrow = (key: SortKey) => {
    if (sort.key !== key) return '';
    return sort.direction === 'asc' ? '↑' : '↓';
  };

  return (
    <div className="card sites-card">
      <div className="section-title">Sites</div>
      <div className="site-pool-summary-table-wrapper">
        <table className="table site-pool-summary-table">
          <thead>
            <tr>
              <th>
                <button type="button" className="site-pool-sort-button" onClick={() => toggleSort('name')}>
                  Name {sortArrow('name')}
                </button>
              </th>
              <th>
                <button type="button" className="site-pool-sort-button" onClick={() => toggleSort('workers')}>
                  Workers {sortArrow('workers')}
                </button>
              </th>
              <th>
                <button type="button" className="site-pool-sort-button" onClick={() => toggleSort('gateways')}>
                  Gateways {sortArrow('gateways')}
                </button>
              </th>
              <th>
                <button type="button" className="site-pool-sort-button" onClick={() => toggleSort('state')}>
                  State {sortArrow('state')}
                </button>
              </th>
            </tr>
          </thead>
          <tbody>
            {sortedRows.map((row) => (
              <tr key={row.id}>
                <td>{row.name}</td>
                <td>{row.workers}</td>
                <td>{row.gateways}</td>
                <td>
                  <span className={`badge ${row.state === 'Healthy' ? 'success' : row.state === 'No Data' ? 'secondary' : 'danger'}`}>{row.state}</span>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}


export default SitesCard;
