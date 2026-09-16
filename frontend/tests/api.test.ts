// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { fetchClusterStatus, pollNodeDetails, requestNodeDetails } from '../src/api.ts';

test('detail API uses viewer credentials, escaped names and request IDs, and explicit force intent', async (t) => {
  const calls: { url: string; init: RequestInit }[] = [];
  t.mock.method(globalThis, 'fetch', async (url, init) => {
    calls.push({ url: String(url), init });
    return new Response(JSON.stringify({ state: 'pending', nodeName: 'node', requestId: 'a' }), { status: 202 });
  });
  const abort = new AbortController();
  await requestNodeDetails('node / a', false, abort.signal);
  await requestNodeDetails('node / a', true, abort.signal);
  await pollNodeDetails('node / a', 'request & id', abort.signal);
  assert.equal(calls[0].url, '/status/node/node%20%2F%20a/details');
  assert.equal(calls[0].init.method, 'POST');
  assert.equal(calls[0].init.body, '{"forceRefresh":false}');
  assert.equal(calls[1].init.body, '{"forceRefresh":true}');
  assert.equal(calls[2].url, '/status/node/node%20%2F%20a/details?requestId=request%20%26%20id');
  for (const call of calls) {
    assert.equal(call.init.credentials, 'same-origin');
    assert.equal(call.init.signal, abort.signal);
  }
  assert.equal(calls[2].init.cache, 'no-store');
});

test('auth, leadership and malformed failures are surfaced', async (t) => {
  const responses = [
    new Response('viewer authorization required', { status: 403 }),
    new Response(JSON.stringify({ error: 'leadership changed' }), { status: 503 }),
    new Response('{}'),
  ];
  t.mock.method(globalThis, 'fetch', async () => responses.shift()!);
  const signal = new AbortController().signal;
  await assert.rejects(requestNodeDetails('node', false, signal), /authorization required/);
  await assert.rejects(requestNodeDetails('node', false, signal), /leadership changed/);
  await assert.rejects(requestNodeDetails('node', false, signal), /Invalid detail response/);
});

test('HTTP lifecycle failures preserve expired, unavailable and retryable states', async (t) => {
  const statuses = { expired: 410, unavailable: 404, retryable: 503 };
  for (const [state, status] of Object.entries(statuses)) {
    t.mock.method(globalThis, 'fetch', async () => new Response(JSON.stringify({
      state, nodeName: 'node', requestId: 'a', error: 'explicit failure',
    }), { status }));
    const result = await pollNodeDetails('node', 'a', new AbortController().signal);
    assert.equal(result.state, state);
    assert.equal(result.error, 'explicit failure');
  }
});

test('network rejection propagates without success-shaped data', async (t) => {
  t.mock.method(globalThis, 'fetch', async () => { throw new TypeError('network unavailable'); });
  await assert.rejects(requestNodeDetails('node', false, new AbortController().signal), /network unavailable/);
});

test('bulk polling accepts summary and legacy shapes and passes cancellation', async (t) => {
  const responses = [{ nodeSummaries: [{ name: 'node' }] }, { nodes: [{ nodeInfo: { name: 'node' } }] }];
  const signal = new AbortController().signal;
  t.mock.method(globalThis, 'fetch', async (url, init) => {
    assert.equal(url, '/status/json');
    assert.equal(init.signal, signal);
    assert.equal(init.cache, 'no-store');
    return new Response(JSON.stringify(responses.shift()));
  });
  assert.ok('nodeSummaries' in await fetchClusterStatus(signal));
  assert.ok('nodes' in await fetchClusterStatus(signal));
});
