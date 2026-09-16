// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { isSummaryOnline, mergeLegacySummary, mergeSummary, summarizeNode, toClusterSummary } from '../src/state/clusterSummary.ts';

test('full compatibility projection preserves counts and drops detail arrays', () => {
  const node = {
    nodeInfo: { name: 'worker', siteName: 'site', wireGuard: { interface: 'wg0' } },
    statusSource: 'push',
    peers: [
      { healthCheck: { enabled: true, status: 'up' } },
      { healthCheck: { enabled: true, status: 'down' } },
      { tunnel: { lastHandshake: new Date(99000).toISOString() } },
      { tunnel: { lastHandshake: new Date(0).toISOString() } },
    ],
    routingTable: { routes: [{ nextHops: [{ expected: true, present: false }] }] },
    bpfEntries: [{ cidr: 'hidden' }],
  };
  const projected = toClusterSummary({
    nodes: [node], nodeCount: 1, siteCount: 1,
    sites: [{ name: 'site' }], gatewayPools: [{ name: 'pool' }], peerings: [{ name: 'peering' }],
  }, 200000);
  assert.equal(projected.nodeSummaries[0].healthyPeers, 2);
  assert.equal(projected.nodeSummaries[0].peerCount, 4);
  assert.equal(projected.nodeSummaries[0].routeCount, 1);
  assert.equal(projected.nodeSummaries[0].cniStatus, 'Route mismatch');
  assert.equal(projected.nodeSummaries[0].wireGuardOnline, true);
  assert.deepEqual(projected.peerings, [{ name: 'peering' }]);
  assert.deepEqual(projected.sites, [{ name: 'site' }]);
  assert.deepEqual(projected.gatewayPools, [{ name: 'pool' }]);
  for (const key of ['"nodes"', '"peers"', '"routingTable"', '"nextHops"', '"bpfEntries"']) {
    assert.equal(JSON.stringify(projected).includes(key), false, key);
  }
});

test('summary input wins over legacy full fields and is itself whitelisted', () => {
  const summary = toClusterSummary({
    nodeSummaries: [{ name: 'node', peerCount: 19, peers: [{ name: 'hidden' }] }],
    nodes: [{ nodeInfo: { name: 'hidden' } }],
  } as never);
  assert.equal(summary.nodeSummaries[0].peerCount, 19);
  assert.equal(JSON.stringify(summary).includes('hidden'), false);
});

test('CNI priority preserves no-data, errors, unknown and health', () => {
  assert.equal(summarizeNode({ statusSource: 'no-data', fetchError: 'error' }).cniStatus, 'No data');
  assert.equal(summarizeNode({ fetchError: 'error' }).cniStatus, 'Fetch error');
  const errors = summarizeNode({ nodeErrors: [{ message: 'bootstrap blocked' }] });
  assert.equal(errors.firstError, 'bootstrap blocked');
  assert.equal(errors.cniTone, 'danger');
  assert.equal(summarizeNode({}).cniStatus, 'Unknown');
  assert.equal(summarizeNode({ statusSource: 'stale' }).cniStatus, 'Stale');
  assert.equal(summarizeNode({ statusSource: 'push' }).cniTone, 'success');
});

test('resource online counts retain interface semantics independently of CNI health', () => {
  assert.equal(isSummaryOnline(summarizeNode({
    nodeInfo: { wireGuard: { interface: 'wg0' } }, nodeErrors: [{ message: 'broken route' }],
  })), true);
  assert.equal(isSummaryOnline(summarizeNode({ statusSource: 'push' })), false);
  assert.equal(isSummaryOnline({ cniStatus: 'Unknown', cniTone: 'warning' }), false);
  assert.equal(isSummaryOnline({ cniStatus: 'Healthy', cniTone: 'success' }), true);
});

test('summary deltas reject stale sequence and remove nodes without losing resources', () => {
  const initial = toClusterSummary({ seq: 4, sites: [{ name: 'site' }], nodeSummaries: [{ name: 'old' }] });
  assert.equal(mergeSummary(initial, { seq: 4, removedNodes: ['old'] }), initial);
  const next = mergeSummary(initial, { seq: 5, removedNodes: ['old'], nodeSummaries: [{ name: 'new' }] });
  assert.deepEqual(next.nodeSummaries, toClusterSummary({ nodeSummaries: [{ name: 'new' }] }).nodeSummaries);
  assert.deepEqual(next.sites, initial.sites);
});

test('legacy partial updates keep counts and CNI facts without retaining full base', () => {
  const initial = toClusterSummary({ nodes: [{
    nodeInfo: { name: 'node', siteName: 'site' }, statusSource: 'push',
    peers: [{ healthCheck: { enabled: true, status: 'up' } }],
    routingTable: { routes: [{ nextHops: [{ expected: true }] }] },
  }] });
  const next = mergeLegacySummary(initial, { updatedNodes: [{ nodeInfo: { name: 'node' }, lastPushTime: 'changed' }] });
  assert.equal(next.nodeSummaries[0].peerCount, 1);
  assert.equal(next.nodeSummaries[0].routeMismatch, true);
  assert.equal(next.nodeSummaries[0].cniStatus, 'Route mismatch');
  assert.equal(JSON.stringify(next).includes('nextHops'), false);
  const empty = mergeLegacySummary(next, { nodes: [] });
  assert.deepEqual(empty.nodeSummaries, []);
});
