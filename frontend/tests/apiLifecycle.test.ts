// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { pollNodeDetails } from '../src/api.ts';

test('HTTP 410 preserves the terminal expired detail result', async (t) => {
  t.mock.method(globalThis, 'fetch', async () => new Response(JSON.stringify({
    state: 'expired', nodeName: 'node', requestId: 'request', error: 'details expired',
  }), { status: 410 }));

  const result = await pollNodeDetails('node', 'request', new AbortController().signal);
  assert.equal(result.state, 'expired');
  assert.equal(result.error, 'details expired');
});
