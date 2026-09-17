// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

import assert from 'node:assert/strict';
import { test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { createServer } from 'vite';

// Browser tooling is optional; see README.md for the explicit browser test command.
const { chromium } = await import(process.env.PLAYWRIGHT_MODULE || 'playwright');

test('native browser Load data, polling, refresh, expiry and cleanup lifecycle', async (t) => {
  const requests = [];
  let mode = 'complete';
  let requestId = 0;
  const server = await createServer({
    configFile: false,
    root: fileURLToPath(new URL('..', import.meta.url)),
    server: { host: '127.0.0.1', port: 0 },
    plugins: [{
      name: 'details-http-fixture',
      configureServer(server) {
        server.middlewares.use(async (req, res, next) => {
          const url = new URL(req.url, 'http://localhost');
          if (url.pathname !== '/status/node/node-a/details') return next();
          let body = '';
          for await (const chunk of req) body += chunk;
          requests.push({
            method: req.method, requestId: url.searchParams.get('requestId'),
            body: body ? JSON.parse(body) : undefined, cookie: req.headers.cookie,
          });
          res.setHeader('Content-Type', 'application/json');
          res.setHeader('Cache-Control', 'no-store');
          const result = { nodeName: 'node-a', requestId: `request-${requestId}` };
          if (mode === 'error') {
            res.statusCode = 503;
            res.end(JSON.stringify({ ...result, state: 'retryable', error: 'fixture unavailable' }));
          } else if (req.method === 'POST' || mode === 'pending') {
            if (req.method === 'POST') result.requestId = `request-${++requestId}`;
            res.statusCode = 202;
            res.end(JSON.stringify({
              ...result, state: 'pending',
              deadline: new Date(Date.now() + (mode === 'deadline' ? -1 : 30000)).toISOString(),
            }));
          } else {
            const now = new Date().toISOString();
            res.end(JSON.stringify({
              ...result, state: 'complete',
              details: {
                ...result, collectedAt: now, receivedAt: now,
                expiresAt: new Date(Date.now() + (mode === 'expire' ? 2000 : 60000)).toISOString(),
                status: {
                  nodeInfo: { name: 'node-a', kernel: 'old-detail-kernel' },
                  peers: [{ name: 'loaded-peer', healthCheck: { enabled: true, status: 'up' } }],
                  routingTable: { routes: [{ destination: '192.0.2.0/24' }] },
                  bpfEntries: [{ cidr: '198.51.100.0/24' }],
                },
              },
            }));
          }
        });
      },
    }],
  });
  t.after(() => server.close());
  await server.listen();
  const origin = `http://127.0.0.1:${server.httpServer.address().port}`;
  const browser = await chromium.launch();
  t.after(() => browser.close());
  const context = await browser.newContext();
  await context.addCookies([{ name: 'viewer', value: 'test-session', url: origin }]);
  const page = await context.newPage();
  const errors = [];
  page.on('pageerror', (error) => errors.push(error.message));
  page.setDefaultTimeout(10000);
  await page.goto(`${origin}/tests/fixtures/nodeDetails.html`);
  const open = () => page.getByRole('button', { name: 'Open node', exact: true }).click();
  const load = () => page.getByRole('button', { name: 'Load data', exact: true }).click();
  const refresh = () => page.getByRole('button', { name: 'Refresh', exact: true }).click();
  const loaded = () => page.getByText('Detailed data loaded.', { exact: true }).waitFor();
  const deadline = () => page.getByText('Request deadline:').waitFor();
  const fullJSON = page.getByLabel('Full node JSON');

  await open();
  const info = page.locator('.node-detail-left');
  const tabs = page.locator('.detail-tabs');
  const freshness = info.locator('.node-info-card').filter({ hasText: 'Status Push Updated' });
  await info.getByText('Node Info', { exact: true }).waitFor();
  assert.match(await info.innerText(), /192\.0\.2\.10/);
  assert.match(await info.innerText(), /summary-build/);
  assert.match(await freshness.innerText(), /\ds ago/);
  assert.equal(await page.getByText('Healthy', { exact: true }).count(), 1);
  assert.equal(await tabs.getByRole('button').count(), 3);
  for (const tab of ['Routes', 'BPF', 'Peerings']) {
    await tabs.getByRole('button', { name: tab, exact: true }).click();
    assert.equal(await page.locator('.node-detail-right table').count(), 0);
    assert.equal(await page.getByText('WG Validation N/A', { exact: true }).count(), 0);
  }
  assert.equal(requests.length, 0, 'selection does not collect details');
  await load();
  await loaded();
  await page.getByText('loaded-peer', { exact: true }).waitFor();
  await page.getByRole('button', { name: 'Update summary' }).click();
  await info.getByText('updated-summary-kernel', { exact: true }).waitFor();
  assert.equal(await page.getByText('Route mismatch', { exact: true }).count(), 1);
  assert.equal(await info.getByText('old-detail-kernel', { exact: true }).count(), 0);
  assert.deepEqual(requests.map(({ method }) => method), ['POST', 'GET']);
  assert.deepEqual(requests[0].body, { forceRefresh: false });
  assert.equal(requests[1].requestId, 'request-1');
  assert.ok(requests.every(({ cookie }) => cookie === 'viewer=test-session'));
  await page.getByRole('button', { name: 'Show full node JSON' }).click();
  assert.equal(JSON.parse(await fullJSON.innerText()).nodeInfo.name, 'node-a');
  await load();
  assert.equal(requests.length, 2, 'Load data reuses the still-valid snapshot');

  mode = 'error';
  await refresh();
  await page.getByRole('alert').waitFor();
  assert.deepEqual(requests.at(-1).body, { forceRefresh: true });
  assert.match(await page.getByRole('alert').innerText(), /fixture unavailable/);
  assert.equal(JSON.parse(await fullJSON.innerText()).nodeInfo.name, 'node-a');
  await page.getByText('Showing previous still-valid snapshot.', { exact: false }).waitFor();

  mode = 'expire';
  await refresh();
  await loaded();
  await page.getByRole('button', { name: 'Show full node JSON' }).click();
  await fullJSON.waitFor();
  await page.getByText('Detailed data expired and was removed.', { exact: false }).waitFor();
  assert.equal(await fullJSON.count(), 0, 'expiry unmounts the retained full payload');
  assert.equal(await page.locator('.node-detail-right table').count(), 0);
  assert.equal(await page.getByText('loaded-peer', { exact: true }).count(), 0);
  await info.getByText('updated-summary-kernel', { exact: true }).waitFor();
  assert.equal(await tabs.getByRole('button').count(), 3, 'expiry preserves unloaded tabs');
  const afterExpiry = requests.length;
  await page.waitForTimeout(1100);
  assert.equal(requests.length, afterExpiry, 'expiry never collects replacements');

  mode = 'deadline';
  await load();
  await page.getByRole('alert').waitFor();
  await info.getByText('Node Info', { exact: true }).waitFor();
  assert.match(await page.getByRole('alert').innerText(), /deadline expired/);
  assert.equal(requests.at(-1).method, 'POST', 'expired requests must not start polling');

  mode = 'pending';
  await load();
  await deadline();
  await info.getByText('Node Info', { exact: true }).waitFor();
  await page.getByRole('button', { name: 'Close', exact: true }).click();
  const afterCancel = requests.length;
  await open();
  await page.getByText('Detailed data is not loaded.', { exact: false }).waitFor();
  await page.waitForTimeout(1100);
  assert.equal(requests.length, afterCancel, 'close cancels polling; reopening never loads');
  await load();
  await deadline();
  await page.getByRole('button', { name: 'Unmount', exact: true }).click();
  const afterUnmount = requests.length;
  await page.waitForTimeout(1100);
  assert.equal(requests.length, afterUnmount, 'unmount cancels pending timers');
  assert.deepEqual(errors, [], 'native timer invocation and StrictMode cleanup must not throw');
});
