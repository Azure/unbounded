// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { NodeDetails } from '../src/state/nodeDetails.ts';

const tick = async () => { for (let i = 0; i < 5; i++) await Promise.resolve(); };
const time = (value: number) => new Date(value).toISOString();
function fixture() {
  let now = 1000;
  let id = 0;
  const timers = new Map<number, { at: number; run: () => void }>();
  const calls: { name: string; force?: boolean; signal: AbortSignal; resolve: (value: any) => void; reject: (error: Error) => void }[] = [];
  const send = (name: string, signal: AbortSignal, force?: boolean) =>
    new Promise<any>((resolve, reject) => calls.push({ name, signal, force, resolve, reject }));
  const store = new NodeDetails({
    request: (name, force, signal) => send(name, signal, force),
    poll: (name, _id, signal) => send(name, signal),
  }, () => {}, {
    now: () => now,
    setTimeout: (run, delay) => { timers.set(++id, { at: now + delay, run }); return id as any; },
    clearTimeout: (id) => { timers.delete(id as any); },
  });
  const advance = (value: number, runTimers = true) => {
    now = value;
    if (runTimers) for (const [id, timer] of [...timers]) {
      if (timer.at <= now) { timers.delete(id); timer.run(); }
    }
  };
  const complete = (requestId = 'a', expiresAt = 5000, nodeName = 'node') => ({
    state: 'complete', nodeName, requestId,
    details: {
      nodeName, requestId, collectedAt: time(1000), receivedAt: time(1000), expiresAt: time(expiresAt),
      status: { nodeInfo: { name: nodeName }, peers: [{ name: 'peer' }], bpfEntries: [{ cidr: 'heavy' }] },
    },
  });
  return { store, calls, timers, advance, complete };
}

test('reads/open/summary/reconnect do not load; explicit load reuses cache and refresh forces', async () => {
  const f = fixture();
  for (let i = 0; i < 5; i++) assert.equal(f.store.read('node').state, 'not-loaded');
  assert.equal(f.calls.length, 0);
  f.store.load('node');
  assert.equal(f.calls[0].force, false);
  f.calls[0].resolve(f.complete());
  await tick();
  f.store.load('node');
  assert.equal(f.calls.length, 1);
  f.store.load('node', true);
  assert.equal(f.calls[1].force, true);
  f.calls[1].reject(new Error('leadership unavailable'));
  await tick();
  assert.equal(f.store.read('node').state, 'error');
  assert.equal(f.store.read('node').error, 'leadership unavailable');
  assert.equal(f.store.read('node').snapshot.requestId, 'a');
  f.advance(5000);
  assert.equal(f.store.read('node').snapshot, undefined);
  assert.equal(f.calls.length, 2);
});

test('expiry actively releases data and read-time expiry cannot extend TTL', async () => {
  for (const runTimers of [true, false]) {
    const f = fixture();
    f.store.load('node');
    f.calls[0].resolve(f.complete());
    await tick();
    f.advance(4999);
    assert.equal(f.store.read('node').state, 'loaded');
    f.advance(5000, runTimers);
    assert.deepEqual(f.store.read('node'), { state: 'expired', error: undefined, deadline: undefined });
    assert.equal(f.calls.length, 1);
  }
});

test('selection cancellation and superseded refresh reject stale async payloads', async () => {
  const f = fixture();
  f.store.load('node');
  f.store.cancel('node');
  assert.equal(f.calls[0].signal.aborted, true);
  f.calls[0].resolve(f.complete());
  await tick();
  assert.equal(f.store.read('node').snapshot, undefined);
  f.store.load('node');
  f.store.load('node', true);
  f.calls[2].resolve(f.complete('new', 9000));
  f.calls[1].resolve(f.complete('old'));
  await tick();
  assert.equal(f.store.read('node').snapshot.requestId, 'new');
  f.advance(5000);
  assert.equal(f.store.read('node').snapshot.requestId, 'new');
});

test('pending polling honors original deadline, aborts in-flight GET, rejects late replies', async () => {
  const f = fixture();
  f.store.load('node');
  f.calls[0].resolve({ state: 'pending', nodeName: 'node', requestId: 'a', deadline: time(4000) });
  await tick();
  f.advance(2000);
  assert.equal(f.calls.length, 2);
  f.calls[1].resolve({ state: 'pending', nodeName: 'node', requestId: 'a', deadline: time(9000) });
  await tick();
  assert.equal(f.store.read('node').deadline, time(4000));
  f.advance(3000);
  f.advance(4000);
  assert.equal(f.calls[2].signal.aborted, true);
  assert.equal(f.store.read('node').state, 'expired');
  f.calls[2].resolve(f.complete());
  await tick();
  assert.equal(f.store.read('node').snapshot, undefined);
});

test('invalid and expired responses surface errors, not empty success', async () => {
  for (const result of [
    { state: 'unavailable', nodeName: 'node', error: 'offline' },
    { state: 'pending', nodeName: 'node', requestId: 'a' },
    { state: 'complete', nodeName: 'other' },
  ]) {
    const f = fixture();
    f.store.load('node');
    f.calls[0].resolve(result);
    await tick();
    assert.equal(f.store.read('node').state, 'error');
    assert.equal(f.store.read('node').snapshot, undefined);
  }
  const f = fixture();
  f.store.load('node');
  f.calls[0].resolve(f.complete('a', 1000));
  await tick();
  assert.equal(f.store.read('node').state, 'expired');
});

test('dispose cancels pending work, clears timers/cache, and ignores late results', async () => {
  const f = fixture();
  f.store.load('node');
  f.store.dispose();
  assert.equal(f.calls[0].signal.aborted, true);
  assert.equal(f.timers.size, 0);
  f.calls[0].resolve(f.complete());
  await tick();
  assert.equal(f.store.read('node').snapshot, undefined);
});
