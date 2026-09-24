// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

import assert from 'node:assert/strict';
import { test } from 'node:test';
import { nodeForDetailView } from '../src/state/nodeMetadata.ts';

test('overview view preserves node information without inventing diagnostic collections', () => {
  const summary = {
    name: 'worker', siteName: 'site', k8sReady: 'Ready', isGateway: false,
    lastPushTime: '2026-09-17T01:00:00Z', statusSource: 'ws',
    nodeInfo: { internalIPs: ['192.0.2.1'], kernel: 'summary-kernel' },
  };
  const view = nodeForDetailView('worker', summary);
  assert.equal(view.nodeInfo.name, 'worker');
  assert.equal(view.nodeInfo.siteName, 'site');
  assert.deepEqual(view.nodeInfo.internalIPs, ['192.0.2.1']);
  assert.equal(view.lastPushTime, summary.lastPushTime);
  for (const key of ['peers', 'routingTable', 'bpfEntries']) {
    assert.equal(key in view, false, key);
  }
  assert.equal(nodeForDetailView('unknown').nodeInfo.name, 'unknown');
});

test('current summary overrides old snapshot metadata without mutating or copying diagnostic arrays', () => {
  const details = {
    nodeInfo: { name: 'worker', isGateway: true, kernel: 'old', kubelet: 'version' },
    lastPushTime: '2026-09-17T01:00:00Z', fetchError: 'old failure',
    peers: [{ name: 'peer' }], routingTable: { routes: [] }, bpfEntries: [],
  };
  const summary = {
    name: 'worker', isGateway: false, lastPushTime: '2026-09-17T01:01:00Z',
    nodeInfo: { kernel: 'new' }, statusSource: 'ws',
  };
  const view = nodeForDetailView('worker', summary, details);
  assert.equal(view.nodeInfo.kernel, 'new');
  assert.equal(view.nodeInfo.kubelet, 'version');
  assert.equal(view.nodeInfo.isGateway, false);
  assert.equal(view.lastPushTime, summary.lastPushTime);
  assert.equal(view.fetchError, undefined);
  assert.equal(view.peers, details.peers);
  assert.equal(view.routingTable, details.routingTable);
  assert.equal(view.bpfEntries, details.bpfEntries);
  assert.equal(details.nodeInfo.kernel, 'old');
  assert.equal(nodeForDetailView('worker', undefined, details).fetchError, 'old failure');
});
